package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/agent/task"
	"github.com/charmbracelet/crush/internal/agent/taskquestion"
	"github.com/charmbracelet/crush/internal/backend"
	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/proto"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/charmbracelet/crush/internal/question"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// taskHTTP issues a request against the harness HTTP surface and
// returns the status plus raw body.
func taskHTTP(t *testing.T, h *e2eHarness, method, path string, query url.Values, body any) (int, []byte) {
	t.Helper()
	if query == nil {
		query = url.Values{}
	}
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		require.NoError(t, err)
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(t.Context(), method,
		h.httpSrv.URL+path+"?"+query.Encode(), rdr)
	require.NoError(t, err)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := h.httpSrv.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, data
}

// requireSSEEvent drains decoded SSE envelopes until one matches the
// wanted predicate, failing on stream close or deadline.
func requireSSEEvent[T any](t *testing.T, evc <-chan any, want func(T) bool, what string) T {
	t.Helper()
	deadline := time.After(20 * time.Second)
	for {
		select {
		case ev, ok := <-evc:
			if !ok {
				t.Fatalf("SSE stream closed while waiting for %s", what)
			}
			if e, is := ev.(T); is && want(e) {
				return e
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// sseRecorder decouples a test from the delivery order of events that
// ride independent app bridges (task events vs. task-question
// batches). A plain drain loses every non-matching envelope forever,
// so a scan that follows an earlier one can only succeed if the
// bridges happened to deliver in scan order. The recorder parks
// unmatched events and rechecks them on every later scan.
type sseRecorder struct {
	src <-chan any
	buf []any
}

func requireRecordedSSEEvent[T any](t *testing.T, r *sseRecorder, want func(T) bool, what string) T {
	t.Helper()
	deadline := time.After(20 * time.Second)
	for {
		for i, ev := range r.buf {
			if e, is := ev.(T); is && want(e) {
				r.buf = append(r.buf[:i:i], r.buf[i+1:]...)
				return e
			}
		}
		select {
		case ev, ok := <-r.src:
			if !ok {
				t.Fatalf("SSE stream closed while waiting for %s", what)
			}
			r.buf = append(r.buf, ev)
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func requireWireCode(t *testing.T, body []byte, want string) {
	t.Helper()
	var e proto.Error
	require.NoError(t, json.Unmarshal(body, &e))
	require.Equal(t, want, e.Code, "error body: %s", string(body))
}

func ptrTrue() *bool { b := true; return &b }

// taskHarness builds the full real stack: a live server, a workspace
// created through the real backend path (durable SQLite task store),
// an owner session, and an attached, session-bound SSE stream.
type taskHarness struct {
	h     *e2eHarness
	wsID  string
	cid   string
	owner string

	events    <-chan any
	ws        *backend.Workspace
	closeSSE  context.CancelFunc
	streamCtx context.Context
}

func newTaskHarness(t *testing.T) *taskHarness {
	t.Helper()
	// Skip the Catwalk fetch: these tests exercise task transport,
	// not provider catalogs.
	t.Setenv("CRUSH_DISABLE_PROVIDER_AUTO_UPDATE", "1")
	h := newRealCreateHarness(t)
	cid := uuid.New().String()
	ws := h.postWorkspace(t, proto.Workspace{
		Path: t.TempDir(), DataDir: t.TempDir(), ClientID: cid,
	})

	w, err := h.backend.GetWorkspace(ws.ID)
	require.NoError(t, err)
	wsDataDir := w.Cfg.Config().Options.DataDirectory
	backend.SetWorkspaceShutdownFnForTest(w, func() {
		_ = db.Release(wsDataDir)
	})

	owner, err := h.backend.CreateSession(t.Context(), ws.ID, "owner")
	require.NoError(t, err)

	streamCtx, cancel := context.WithCancel(t.Context())
	evc, cancelSSE := h.subscribeSSE(t, streamCtx, ws.ID, cid)
	t.Cleanup(func() {
		// Tear the stream down and retire the client: retiring
		// releases the claim immediately (no detach grace), the last
		// claim releases the workspace, and the overridden shutdown
		// releases the pooled DB so Windows can remove the temp dir.
		cancel()
		cancelSSE()
		_ = h.backend.RetireClient(cid)
		require.Eventually(t, func() bool {
			_, err := h.backend.GetWorkspace(ws.ID)
			return errors.Is(err, backend.ErrWorkspaceNotFound)
		}, 15*time.Second, 25*time.Millisecond, "workspace DB must be released for tempdir cleanup")
	})

	require.Eventually(t, func() bool {
		return backend.WorkspaceLiveStreamCountForTest(w) >= 1
	}, 5*time.Second, 20*time.Millisecond, "client must attach before session binding")
	require.NoError(t, h.backend.SetCurrentSession(ws.ID, cid, owner.ID))

	return &taskHarness{
		h: h, wsID: ws.ID, cid: cid, owner: owner.ID,
		events: evc, ws: w, closeSSE: cancel, streamCtx: streamCtx,
	}
}

func (th *taskHarness) query(clientID string) url.Values {
	if clientID == "" {
		clientID = th.cid
	}
	return url.Values{"client_id": []string{clientID}}
}

// startTask admits one real task whose runner parks until released.
func (th *taskHarness) startTask(t *testing.T, toolCallID, prompt string, run task.Runner) *task.Task {
	t.Helper()
	return th.startTaskOwned(t, th.owner, toolCallID, prompt, run)
}

func (th *taskHarness) startTaskOwned(t *testing.T, owner, toolCallID, prompt string, run task.Runner) *task.Task {
	t.Helper()
	rec, err := th.ws.App.Tasks().Start(t.Context(), task.StartRequest{
		CallerSessionID: owner,
		ParentSessionID: owner,
		ChildSessionID:  uuid.NewString(),
		ChildTitle:      "child " + toolCallID,
		ParentMessageID: "pm-" + toolCallID,
		ToolCallID:      toolCallID,
		Profile:         "coder",
		Provider:        "testprov",
		Model:           "testmodel",
		Prompt:          prompt,
		Run:             run,
	})
	require.NoError(t, err)
	return rec
}

func parkUntilRelease(entered chan<- struct{}, release <-chan struct{}) task.Runner {
	return func(ctx context.Context, h *task.Handle) (task.Result, error) {
		entered <- struct{}{}
		select {
		case <-release:
			return task.Result{Text: "child output", Summary: "wrapped up"}, nil
		case <-ctx.Done():
			return task.Result{}, ctx.Err()
		}
	}
}

// TestTaskEvents_SSERoundTrip is the spec section 5 acceptance
// procedure, steps 1-4: real task lifecycle facts published by the
// App task broker arrive over SSE as task_event envelopes carrying
// every contract field, including the task_id + tool_call_id
// correlation pair; every event type round-trips.
func TestTaskEvents_SSERoundTrip(t *testing.T) {
	th := newTaskHarness(t)

	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	rec := th.startTask(t, "tc-1", "do the thing", parkUntilRelease(entered, release))
	<-entered

	// The created fact must name the admission tool call.
	created := requireSSEEvent(t, th.events, func(ev pubsub.Event[proto.AgentTaskEvent]) bool {
		return ev.Payload.Type == "created" && ev.Payload.TaskID == rec.ID
	}, "created task event")
	env := created.Payload
	require.Equal(t, rec.ID, env.TaskID)
	require.Equal(t, "tc-1", env.ToolCallID)
	require.Equal(t, th.owner, env.ParentSessionID)
	require.NotEmpty(t, env.ChildSessionID)
	require.Equal(t, "pm-tc-1", env.ParentMessageID)
	require.Equal(t, "coder", env.Profile)
	require.Equal(t, "testprov", env.ResolvedProvider)
	require.Equal(t, "testmodel", env.ResolvedModel)
	require.NotEmpty(t, env.At)
	_, err := time.Parse(time.RFC3339Nano, env.At)
	require.NoError(t, err, "At must be RFC3339Nano UTC")
	require.NotZero(t, env.RunGeneration)

	// Dispatch publishes started.
	requireSSEEvent(t, th.events, func(ev pubsub.Event[proto.AgentTaskEvent]) bool {
		return ev.Payload.Type == "started" && ev.Payload.TaskID == rec.ID &&
			ev.Payload.ToolCallID == "tc-1" && ev.Payload.Status == "running"
	}, "started task event")

	// The waiting/resumed pair drives the live handle directly to
	// prove the transitions publish.
	h, ok := th.ws.App.Tasks().Handle(rec.ID)
	require.True(t, ok, "task must be live while its runner is parked")
	require.NoError(t, h.WaitingForInput(t.Context()))
	requireSSEEvent(t, th.events, func(ev pubsub.Event[proto.AgentTaskEvent]) bool {
		return ev.Payload.Type == "waiting_for_input" && ev.Payload.TaskID == rec.ID &&
			ev.Payload.ToolCallID == "tc-1"
	}, "waiting_for_input task event")
	require.NoError(t, h.Resumed(t.Context()))
	requireSSEEvent(t, th.events, func(ev pubsub.Event[proto.AgentTaskEvent]) bool {
		return ev.Payload.Type == "resumed" && ev.Payload.TaskID == rec.ID &&
			ev.Payload.Status == "running"
	}, "resumed task event")

	// Terminal completed flows through the same envelope.
	close(release)
	comp := requireSSEEvent(t, th.events, func(ev pubsub.Event[proto.AgentTaskEvent]) bool {
		return ev.Payload.Type == "completed" && ev.Payload.TaskID == rec.ID
	}, "completed task event")
	require.Equal(t, "completed", comp.Payload.Status)
	require.Equal(t, "tc-1", comp.Payload.ToolCallID)

	// failed and cancelled come from real terminal transitions.
	entered2 := make(chan struct{}, 1)
	rec2 := th.startTask(t, "tc-2", "fail me", func(ctx context.Context, h *task.Handle) (task.Result, error) {
		entered2 <- struct{}{}
		return task.Result{}, errors.New("boom")
	})
	<-entered2
	requireSSEEvent(t, th.events, func(ev pubsub.Event[proto.AgentTaskEvent]) bool {
		return ev.Payload.Type == "failed" && ev.Payload.TaskID == rec2.ID &&
			ev.Payload.Status == "failed" && ev.Payload.ToolCallID == "tc-2"
	}, "failed task event")

	entered3 := make(chan struct{}, 1)
	release3 := make(chan struct{})
	rec3 := th.startTask(t, "tc-3", "cancel me", parkUntilRelease(entered3, release3))
	<-entered3
	require.NoError(t, th.ws.App.Tasks().Cancel(t.Context(), th.owner, rec3.ID))
	requireSSEEvent(t, th.events, func(ev pubsub.Event[proto.AgentTaskEvent]) bool {
		return ev.Payload.Type == "cancelled" && ev.Payload.TaskID == rec3.ID &&
			ev.Payload.Status == "cancelled" && ev.Payload.ToolCallID == "tc-3"
	}, "cancelled task event")

	// interrupted is recovery-owned; publish the fact through the
	// App task broker the same way recovery's durable events flow.
	th.ws.SendEvent(pubsub.Event[task.Event]{
		Type: pubsub.UpdatedEvent,
		Payload: task.Event{
			Type: task.EventInterrupted,
			Task: &task.Task{
				ID: "interrupted-1", ParentSessionID: th.owner, ToolCallID: "tc-4",
				ParentMessageID: "pm-4", Profile: "coder", Provider: "p", Model: "m",
				Status: task.StatusInterrupted, RunGeneration: 1, UpdatedAt: time.Now().UTC(),
			},
		},
	})
	requireSSEEvent(t, th.events, func(ev pubsub.Event[proto.AgentTaskEvent]) bool {
		return ev.Payload.Type == "interrupted" && ev.Payload.TaskID == "interrupted-1" &&
			ev.Payload.ToolCallID == "tc-4"
	}, "interrupted task event")
}

// TestTaskMessages_RouteContract proves the direct child-message
// route: 202 acceptances with FIFO sequence and derived child
// metadata, plus the exact error/code matrix for empty prompts and
// every client/ownership failure class.
func TestTaskMessages_RouteContract(t *testing.T) {
	th := newTaskHarness(t)

	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	rec := th.startTask(t, "tc-msg", "working", parkUntilRelease(entered, release))
	<-entered

	base := "/v1/workspaces/" + th.wsID + "/tasks/" + rec.ID + "/messages"

	// Owner append is accepted with FIFO metadata. Running tasks
	// claim no successor attempt: sequence one follows the
	// sequence-zero admission prompt.
	status, body := taskHTTP(t, th.h, http.MethodPost, base, th.query(""), proto.ChildMessageRequest{Prompt: "hello child"})
	require.Equal(t, http.StatusAccepted, status)
	var acc proto.ChildMessageAccepted
	require.NoError(t, json.Unmarshal(body, &acc))
	require.Equal(t, rec.ID, acc.TaskID)
	require.Equal(t, rec.ChildSessionID, acc.ChildSessionID)
	require.Equal(t, uint64(1), acc.Sequence)
	require.Empty(t, acc.AttemptTaskID, "running tasks never claim an attempt")
	require.Equal(t, "running", acc.Status)

	// A second append gets the next FIFO sequence.
	status, body = taskHTTP(t, th.h, http.MethodPost, base, th.query(""), proto.ChildMessageRequest{Prompt: "and again"})
	require.Equal(t, http.StatusAccepted, status)
	require.NoError(t, json.Unmarshal(body, &acc))
	require.Equal(t, uint64(2), acc.Sequence)

	// Attachments ride the acceptance path.
	status, _ = taskHTTP(t, th.h, http.MethodPost, base, th.query(""), proto.ChildMessageRequest{
		Prompt:      "with file",
		Attachments: []proto.Attachment{{FilePath: "/tmp/x.txt", FileName: "x.txt", MimeType: "text/plain", Content: []byte("hi")}},
	})
	require.Equal(t, http.StatusAccepted, status)

	// Empty prompt is 400/empty_prompt and stores nothing.
	status, body = taskHTTP(t, th.h, http.MethodPost, base, th.query(""), proto.ChildMessageRequest{Prompt: ""})
	require.Equal(t, http.StatusBadRequest, status)
	requireWireCode(t, body, "empty_prompt")

	// Error matrix ---------------------------------------------------
	// Missing client id: 401/client_required.
	status, body = taskHTTP(t, th.h, http.MethodPost, base, url.Values{}, proto.ChildMessageRequest{Prompt: "hi"})
	require.Equal(t, http.StatusUnauthorized, status)
	requireWireCode(t, body, "client_required")

	// Malformed client id: 401/client_required.
	status, body = taskHTTP(t, th.h, http.MethodPost, base, th.query("not-a-uuid"), proto.ChildMessageRequest{Prompt: "hi"})
	require.Equal(t, http.StatusUnauthorized, status)
	requireWireCode(t, body, "client_required")

	// Unattached client: 403/client_unattached.
	status, body = taskHTTP(t, th.h, http.MethodPost, base, th.query(uuid.NewString()), proto.ChildMessageRequest{Prompt: "hi"})
	require.Equal(t, http.StatusForbidden, status)
	requireWireCode(t, body, "client_unattached")

	// Retired client: 403/client_unattached.
	retired := uuid.NewString()
	require.NoError(t, th.h.backend.RetireClient(retired))
	status, body = taskHTTP(t, th.h, http.MethodPost, base, th.query(retired), proto.ChildMessageRequest{Prompt: "hi"})
	require.Equal(t, http.StatusForbidden, status)
	requireWireCode(t, body, "client_unattached")

	// Attached but sessionless client: 409/client_session_required.
	cid2 := uuid.NewString()
	ctx2, cancel2 := context.WithCancel(t.Context())
	defer cancel2()
	_, cancelSSE2 := th.h.subscribeSSE(t, ctx2, th.wsID, cid2)
	defer cancelSSE2()
	require.Eventually(t, func() bool {
		return backend.WorkspaceLiveStreamCountForTest(th.ws) >= 2
	}, 5*time.Second, 20*time.Millisecond)
	status, body = taskHTTP(t, th.h, http.MethodGet, "/v1/workspaces/"+th.wsID+"/tasks", th.query(cid2), nil)
	require.Equal(t, http.StatusConflict, status)
	requireWireCode(t, body, "client_session_required")

	// Foreign owner: 403/task_owner_forbidden.
	other, err := th.h.backend.CreateSession(t.Context(), th.wsID, "other")
	require.NoError(t, err)
	require.NoError(t, th.h.backend.SetCurrentSession(th.wsID, cid2, other.ID))
	status, body = taskHTTP(t, th.h, http.MethodGet, "/v1/workspaces/"+th.wsID+"/tasks/"+rec.ID, th.query(cid2), nil)
	require.Equal(t, http.StatusForbidden, status)
	requireWireCode(t, body, "task_owner_forbidden")

	// Unknown task: 404/task_not_found.
	status, body = taskHTTP(t, th.h, http.MethodGet, "/v1/workspaces/"+th.wsID+"/tasks/"+uuid.NewString(), th.query(""), nil)
	require.Equal(t, http.StatusNotFound, status)
	requireWireCode(t, body, "task_not_found")

	// Deleted owner: 410/task_owner_deleted.
	ghost, err := th.h.backend.CreateSession(t.Context(), th.wsID, "ghost")
	require.NoError(t, err)
	enteredG := make(chan struct{}, 1)
	releaseG := make(chan struct{})
	recG := th.startTaskOwned(t, ghost.ID, "tc-ghost", "ghost work", parkUntilRelease(enteredG, releaseG))
	<-enteredG
	require.NoError(t, th.h.backend.DeleteSession(t.Context(), th.wsID, ghost.ID))
	status, body = taskHTTP(t, th.h, http.MethodGet, "/v1/workspaces/"+th.wsID+"/tasks/"+recG.ID, th.query(cid2), nil)
	require.Equal(t, http.StatusGone, status)
	requireWireCode(t, body, "task_owner_deleted")
	close(releaseG)

	// Terminal cancel: 409/task_already_terminal.
	close(release)
	requireSSEEvent(t, th.events, func(ev pubsub.Event[proto.AgentTaskEvent]) bool {
		return ev.Payload.Type == "completed" && ev.Payload.TaskID == rec.ID
	}, "completion before terminal-cancel probe")
	status, body = taskHTTP(t, th.h, http.MethodPost, "/v1/workspaces/"+th.wsID+"/tasks/"+rec.ID+"/cancel", th.query(""), nil)
	require.Equal(t, http.StatusConflict, status)
	requireWireCode(t, body, "task_already_terminal")

	// The queued messages dispatch as successor attempts in FIFO
	// order once the first attempt terminalizes.
	require.Eventually(t, func() bool {
		st, b := taskHTTP(t, th.h, http.MethodGet, "/v1/workspaces/"+th.wsID+"/tasks", th.query(""), nil)
		if st != http.StatusOK {
			return false
		}
		var list proto.TaskListResponse
		return json.Unmarshal(b, &list) == nil && len(list.Tasks) >= 3
	}, 20*time.Second, 50*time.Millisecond, "queued messages must start successor attempts")
}

// TestTaskEvents_DroppedStreamResyncFromSQLite is spec procedure step
// 5: every live event is dropped with the stream, and reconnecting
// clients recover task state, bounded output, and the terminal outbox
// row from SQLite through the named GET routes.
func TestTaskEvents_DroppedStreamResyncFromSQLite(t *testing.T) {
	th := newTaskHarness(t)

	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	rec := th.startTask(t, "tc-drop", "survive the drop", parkUntilRelease(entered, release))
	<-entered

	// Kill the stream before the terminal event is published, so the
	// completion fact is lost to every live channel.
	th.closeSSE()
	for range th.events { //nolint:revive // drain until closed
	}

	close(release)
	require.Eventually(t, func() bool {
		final, err := th.ws.App.Tasks().Status(t.Context(), th.owner, rec.ID)
		return err == nil && final.Status == task.StatusCompleted
	}, 20*time.Second, 25*time.Millisecond, "the task must terminalize with nobody watching")

	// Reconnect and recover through the durable GET routes.
	ctx2, cancel2 := context.WithCancel(t.Context())
	defer cancel2()
	_, cancelSSE2 := th.h.subscribeSSE(t, ctx2, th.wsID, th.cid)
	defer cancelSSE2()
	require.Eventually(t, func() bool {
		return backend.WorkspaceLiveStreamCountForTest(th.ws) >= 1
	}, 5*time.Second, 20*time.Millisecond)
	require.NoError(t, th.h.backend.SetCurrentSession(th.wsID, th.cid, th.owner))

	status, body := taskHTTP(t, th.h, http.MethodGet, "/v1/workspaces/"+th.wsID+"/tasks", th.query(""), nil)
	require.Equal(t, http.StatusOK, status)
	var list proto.TaskListResponse
	require.NoError(t, json.Unmarshal(body, &list))
	require.Len(t, list.Tasks, 1)
	require.Equal(t, rec.ID, list.Tasks[0].ID)
	require.Equal(t, "completed", list.Tasks[0].Status)

	status, body = taskHTTP(t, th.h, http.MethodGet, "/v1/workspaces/"+th.wsID+"/tasks/"+rec.ID+"/output", th.query(""), nil)
	require.Equal(t, http.StatusOK, status)
	var out proto.TaskOutputResponse
	require.NoError(t, json.Unmarshal(body, &out))
	require.Equal(t, rec.ID, out.TaskID)
	require.Equal(t, "completed", out.Status)
	require.Equal(t, "child output", out.Result)
	require.False(t, out.Truncated)

	status, body = taskHTTP(t, th.h, http.MethodGet, "/v1/workspaces/"+th.wsID+"/tasks/resync", th.query(""), nil)
	require.Equal(t, http.StatusOK, status)
	var resync proto.TaskResyncResponse
	require.NoError(t, json.Unmarshal(body, &resync))
	require.Len(t, resync.Tasks, 1)
	var sawTerminal bool
	for _, e := range resync.Outbox {
		if e.TaskID == rec.ID && e.EventType == "completed" {
			sawTerminal = true
			var snap map[string]any
			require.NoError(t, json.Unmarshal(e.Payload, &snap))
			require.Equal(t, rec.ID, snap["id"])
		}
	}
	require.True(t, sawTerminal, "terminal outbox row must survive the dropped stream: %+v", resync.Outbox)
	require.NotNil(t, resync.Inbox)
	require.NotNil(t, resync.Questions)
}

// TestTaskList_ParentFilterAndHidden pins the subagent-switcher read:
// GET /tasks returns exactly the caller's direct child tasks, the
// parent_session_id filter is accepted only when it equals the
// server-derived caller and rejected 403 otherwise, and hidden
// system-owned (agentic_fetch) tasks never surface on the wire.
func TestTaskList_ParentFilterAndHidden(t *testing.T) {
	th := newTaskHarness(t)

	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	visible := th.startTask(t, "tc-vis", "public work", parkUntilRelease(entered, release))
	<-entered

	hidden, err := th.ws.App.Tasks().Start(t.Context(), task.StartRequest{
		CallerSessionID: th.owner,
		ParentSessionID: th.owner,
		ChildSessionID:  uuid.NewString(),
		ParentMessageID: "pm-hidden",
		ToolCallID:      "tc-hidden",
		Profile:         task.HiddenProfile,
		Prompt:          "hidden work",
		Run:             parkUntilRelease(entered, release),
	})
	require.NoError(t, err)
	<-entered

	// Both the unfiltered and the matching-parent read return the
	// visible task only: the hidden record never reaches the wire.
	for _, tc := range []struct{ name, parent string }{
		{"unfiltered", ""},
		{"matching parent", th.owner},
	} {
		q := th.query("")
		if tc.parent != "" {
			q.Set("parent_session_id", tc.parent)
		}
		status, body := taskHTTP(t, th.h, http.MethodGet, "/v1/workspaces/"+th.wsID+"/tasks", q, nil)
		require.Equal(t, http.StatusOK, status, tc.name)
		var list proto.TaskListResponse
		require.NoError(t, json.Unmarshal(body, &list), tc.name)
		require.Len(t, list.Tasks, 1, tc.name)
		require.Equal(t, visible.ID, list.Tasks[0].ID, tc.name)
		require.NotEqual(t, hidden.ID, list.Tasks[0].ID, tc.name)
	}

	// A parent filter naming another session is 403 before any read.
	other, err := th.h.backend.CreateSession(t.Context(), th.wsID, "other")
	require.NoError(t, err)
	q := th.query("")
	q.Set("parent_session_id", other.ID)
	status, body := taskHTTP(t, th.h, http.MethodGet, "/v1/workspaces/"+th.wsID+"/tasks", q, nil)
	require.Equal(t, http.StatusForbidden, status)
	requireWireCode(t, body, "task_owner_forbidden")

	// Drain both attempts before the harness releases the database.
	close(release)
	require.Eventually(t, func() bool {
		v, err := th.ws.App.Tasks().Status(t.Context(), th.owner, visible.ID)
		if err != nil {
			return false
		}
		h, err := th.ws.App.Tasks().Store().Get(t.Context(), hidden.ID)
		return err == nil && v.Status.Terminal() && h.Status.Terminal()
	}, 20*time.Second, 25*time.Millisecond, "both attempts must terminalize before teardown")
}

// TestTaskQuestions_AnswerOverSSE is spec procedure step 6: a real
// child question reaches the client as an extended task-question
// envelope, the owner answers through the named route, and the same
// task attempt resumes rather than running a second time.
func TestTaskQuestions_AnswerOverSSE(t *testing.T) {
	th := newTaskHarness(t)
	asked := make(chan string, 1)
	var entries int

	rec := th.startTask(t, "tc-q", "ask the owner", func(ctx context.Context, h *task.Handle) (task.Result, error) {
		entries++
		answers, err := th.ws.App.TaskQuestions().AskTask(ctx, taskquestion.TaskQuestionRequest{
			TaskID:         h.TaskID(),
			OwnerSessionID: th.owner,
			ChildSessionID: h.ChildSessionID(),
			RunGeneration:  h.RunGeneration(),
			Batch: question.Request{
				ToolCallID: "tc-q-inner",
				Questions: []question.Question{{
					Type:        question.TypeYesNo,
					Text:        "Proceed?",
					Description: "Continue?",
				}},
			},
		})
		if err != nil {
			return task.Result{}, err
		}
		asked <- answers[0].QuestionID
		return task.Result{Text: "answered " + answers[0].QuestionID, Summary: "resumed after answer"}, nil
	})

	// The request envelope extends the question shape with the full
	// task correlation. Both envelopes ride independent app bridges,
	// so their stream order is scheduling-dependent: record the
	// stream and re-check parked events on the follow-up scan.
	r := &sseRecorder{src: th.events}
	qev := requireRecordedSSEEvent(t, r, func(ev pubsub.Event[proto.TaskQuestion]) bool {
		return ev.Payload.TaskID == rec.ID
	}, "task question request envelope")
	require.NotEmpty(t, qev.Payload.QuestionID)
	require.Equal(t, th.owner, qev.Payload.OwnerSessionID)
	require.Equal(t, rec.ChildSessionID, qev.Payload.ChildSessionID)
	require.Equal(t, rec.RunGeneration, qev.Payload.RunGeneration)
	require.Equal(t, "pending", qev.Payload.Resolution)
	require.NotEmpty(t, qev.Payload.Batch.Questions)
	require.NotEmpty(t, qev.Payload.CreatedAt)
	require.Nil(t, qev.Payload.ResolvedAt)

	// The durable wait published before the batch became observable.
	requireRecordedSSEEvent(t, r, func(ev pubsub.Event[proto.AgentTaskEvent]) bool {
		return ev.Payload.Type == "waiting_for_input" && ev.Payload.TaskID == rec.ID &&
			ev.Payload.ToolCallID == "tc-q"
	}, "waiting_for_input task event")

	// The pending route lists it for the owner.
	status, body := taskHTTP(t, th.h, http.MethodGet, "/v1/workspaces/"+th.wsID+"/task-questions/pending", th.query(""), nil)
	require.Equal(t, http.StatusOK, status)
	var plist proto.TaskQuestionListResponse
	require.NoError(t, json.Unmarshal(body, &plist))
	require.Len(t, plist.Questions, 1)
	require.Equal(t, qev.Payload.QuestionID, plist.Questions[0].QuestionID)

	// Malformed and unknown answer bodies never alter state.
	status, body = taskHTTP(t, th.h, http.MethodPost, "/v1/workspaces/"+th.wsID+"/task-questions/answer",
		th.query(""), proto.TaskQuestionAnswerRequest{Responses: []proto.TaskQuestionAnswer{{}}})
	require.Equal(t, http.StatusBadRequest, status)
	requireWireCode(t, body, "invalid_task_question_answer")
	status, body = taskHTTP(t, th.h, http.MethodPost, "/v1/workspaces/"+th.wsID+"/task-questions/answer",
		th.query(""), proto.TaskQuestionAnswerRequest{
			QuestionID: "nope", Responses: []proto.TaskQuestionAnswer{{QuestionID: "x", Yes: ptrTrue()}},
		})
	require.Equal(t, http.StatusNotFound, status)
	requireWireCode(t, body, "task_question_not_found")

	// A foreign caller is forbidden without state change.
	cid2 := uuid.NewString()
	ctx2, cancel2 := context.WithCancel(t.Context())
	defer cancel2()
	_, cancelSSE2 := th.h.subscribeSSE(t, ctx2, th.wsID, cid2)
	defer cancelSSE2()
	require.Eventually(t, func() bool {
		return backend.WorkspaceLiveStreamCountForTest(th.ws) >= 2
	}, 5*time.Second, 20*time.Millisecond)
	other, err := th.h.backend.CreateSession(t.Context(), th.wsID, "other")
	require.NoError(t, err)
	require.NoError(t, th.h.backend.SetCurrentSession(th.wsID, cid2, other.ID))
	status, body = taskHTTP(t, th.h, http.MethodPost, "/v1/workspaces/"+th.wsID+"/task-questions/answer",
		th.query(cid2), proto.TaskQuestionAnswerRequest{
			QuestionID: qev.Payload.QuestionID,
			Responses:  []proto.TaskQuestionAnswer{{QuestionID: qev.Payload.Batch.Questions[0].ID, Yes: ptrTrue()}},
		})
	require.Equal(t, http.StatusForbidden, status)
	requireWireCode(t, body, "task_owner_forbidden")

	// The owner's answer resolves the question...
	status, body = taskHTTP(t, th.h, http.MethodPost, "/v1/workspaces/"+th.wsID+"/task-questions/answer",
		th.query(""), proto.TaskQuestionAnswerRequest{
			QuestionID: qev.Payload.QuestionID,
			Responses:  []proto.TaskQuestionAnswer{{QuestionID: qev.Payload.Batch.Questions[0].ID, Yes: ptrTrue()}},
		})
	require.Equal(t, http.StatusOK, status, string(body))
	var resp proto.TaskQuestionResolutionResponse
	require.NoError(t, json.Unmarshal(body, &resp))
	require.True(t, resp.Resolved)

	// ...the resolution notification reconciles by question id...
	requireRecordedSSEEvent(t, r, func(ev pubsub.Event[proto.TaskQuestionNotification]) bool {
		return ev.Payload.QuestionID == qev.Payload.QuestionID &&
			ev.Payload.TaskID == rec.ID &&
			ev.Payload.Resolution == "answered"
	}, "task question resolution notification")

	// ...and the same attempt resumes: a resumed fact, never a
	// second first-turn run.
	requireRecordedSSEEvent(t, r, func(ev pubsub.Event[proto.AgentTaskEvent]) bool {
		return ev.Payload.Type == "resumed" && ev.Payload.TaskID == rec.ID &&
			ev.Payload.ToolCallID == "tc-q"
	}, "resumed task event")
	select {
	case <-asked:
	case <-time.After(20 * time.Second):
		t.Fatal("AskTask never returned the owner answer")
	}
	comp := requireRecordedSSEEvent(t, r, func(ev pubsub.Event[proto.AgentTaskEvent]) bool {
		return ev.Payload.Type == "completed" && ev.Payload.TaskID == rec.ID
	}, "completed task event")
	require.Equal(t, "resumed after answer", comp.Payload.Summary)
	require.Equal(t, 1, entries, "the answered child ran exactly one first attempt")

	// Pending reads now report nothing, and re-resolving the
	// resolved question observes already-resolved.
	status, body = taskHTTP(t, th.h, http.MethodGet, "/v1/workspaces/"+th.wsID+"/task-questions/pending", th.query(""), nil)
	require.Equal(t, http.StatusOK, status)
	require.NoError(t, json.Unmarshal(body, &plist))
	require.Empty(t, plist.Questions)
	status, body = taskHTTP(t, th.h, http.MethodPost, "/v1/workspaces/"+th.wsID+"/task-questions/cancel",
		th.query(""), proto.TaskQuestionCancelRequest{QuestionID: qev.Payload.QuestionID})
	require.Equal(t, http.StatusConflict, status)
	requireWireCode(t, body, "task_question_already_resolved")
}
