package agent

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent/task"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gatedModel is a fantasy.LanguageModel that blocks inside Stream until
// release is closed or its context is cancelled, signalling entered on
// first invocation. It drives a child run that stays live long enough to
// test capacity, quota, and cancellation without a provider.
type gatedModel struct {
	text    string
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func newGatedModel(text string) *gatedModel {
	return &gatedModel{
		text:    text,
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (m *gatedModel) Provider() string { return "fake" }
func (m *gatedModel) Model() string    { return "fake-model" }

func (m *gatedModel) Generate(_ context.Context, _ fantasy.Call) (*fantasy.Response, error) {
	return nil, errors.New("not implemented")
}

func (m *gatedModel) Stream(ctx context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
	return func(yield func(fantasy.StreamPart) bool) {
		m.once.Do(func() { close(m.entered) })
		// Observe cancellation so a cancelled task's runner returns
		// promptly instead of waiting for a release that never comes.
		select {
		case <-m.release:
		case <-ctx.Done():
			return
		}
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextStart, ID: "1"}) {
			return
		}
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "1", Delta: m.text}) {
			return
		}
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextEnd, ID: "1"}) {
			return
		}
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
	}, nil
}

func (m *gatedModel) GenerateObject(_ context.Context, _ fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errors.New("not implemented")
}

func (m *gatedModel) StreamObject(_ context.Context, _ fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errors.New("not implemented")
}

// fakeChildAgent builds a real sessionAgent over a fake model so the
// delegation path exercises actual SessionAgent execution and message
// persistence without any provider traffic.
func fakeChildAgent(env fakeEnv, model fantasy.LanguageModel) SessionAgent {
	large := Model{
		Model:      model,
		CatwalkCfg: catwalk.Model{ID: "mock-model", ContextWindow: 8192, DefaultMaxTokens: 128},
		ModelCfg:   config.SelectedModel{Provider: "mock", Model: "mock-model"},
	}
	return NewSessionAgent(SessionAgentOptions{
		LargeModel:           large,
		SmallModel:           large,
		SystemPrompt:         "fake system prompt",
		IsSubAgent:           true,
		DisableAutoSummarize: true,
		IsYolo:               true,
		Sessions:             env.sessions,
		Messages:             env.messages,
	})
}

// taskToolEnv wires a coordinator to a real task.Manager over the shared
// test SQLite connection and a fake-model child agent factory. nextAgent
// supplies one child agent per delegation call.
func taskToolEnv(
	t *testing.T,
	limits task.Limits,
	nextAgent func(env fakeEnv) SessionAgent,
) (*coordinator, *task.Manager, fakeEnv, chan task.Event) {
	t.Helper()
	env := testEnv(t)
	require.NoError(t, os.WriteFile(filepath.Join(env.workingDir, "crush.json"), []byte(profileTestConfig), 0o644))
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)
	cfg.SetupAgents()

	coord := &coordinator{
		cfg:         cfg,
		sessions:    env.sessions,
		messages:    env.messages,
		permissions: env.permissions,
		history:     env.history,
		filetracker: *env.filetracker,
	}
	mgr := task.New(t.Context(), task.Config{
		WorkspaceID: env.workingDir,
		Limits:      limits,
		Store:       task.NewSQLiteStore(env.conn),
	})
	events := make(chan task.Event, 64)
	mgr.Subscribe(func(ev task.Event) { events <- ev })
	t.Cleanup(func() {
		_ = mgr.Shutdown(context.WithoutCancel(t.Context()))
	})

	coord.tasks = mgr
	coord.newChildAgent = func(_ context.Context, name, _ string) (SessionAgent, config.ResolvedProfile, error) {
		return nextAgent(env), config.ResolvedProfile{Name: name}, nil
	}
	return coord, mgr, env, events
}

func delegationCtx(t *testing.T, parentSessionID, messageID string) context.Context {
	t.Helper()
	ctx := context.WithValue(t.Context(), tools.SessionIDContextKey, parentSessionID)
	return context.WithValue(ctx, tools.MessageIDContextKey, messageID)
}

func runAgentTool(t *testing.T, coord *coordinator, ctx context.Context, callID, input string) fantasy.ToolResponse {
	t.Helper()
	tool, err := coord.agentTool(ctx)
	require.NoError(t, err)
	resp, err := tool.Run(ctx, fantasy.ToolCall{ID: callID, Name: AgentToolName, Input: input})
	require.NoError(t, err)
	return resp
}

func taskIDFromResponse(t *testing.T, content string) string {
	t.Helper()
	for _, line := range strings.Split(content, "\n") {
		if id, ok := strings.CutPrefix(line, "Task ID: "); ok {
			return strings.TrimSpace(id)
		}
	}
	t.Fatalf("no Task ID line in response:\n%s", content)
	return ""
}

func waitTerminalEvent(t *testing.T, events chan task.Event, id string) *task.Task {
	t.Helper()
	deadline := time.After(15 * time.Second)
	for {
		select {
		case ev := <-events:
			if ev.Task.ID == id && ev.Task.Status.Terminal() {
				return ev.Task
			}
		case <-deadline:
			t.Fatalf("timeout waiting for terminal event of task %s", id)
		}
	}
}

// waitTerminalEvents collects the terminal snapshot of every id from the
// shared event stream in one pass. Waiting per id with waitTerminalEvent
// in a multi-task test is order-dependent: the first call consumes and
// discards a sibling's terminal event when it wins the race, and the
// second call then times out. Multi-task tests must use this instead.
func waitTerminalEvents(t *testing.T, events chan task.Event, ids ...string) map[string]*task.Task {
	t.Helper()
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	got := make(map[string]*task.Task, len(ids))
	deadline := time.After(15 * time.Second)
	for len(got) < len(want) {
		select {
		case ev := <-events:
			if want[ev.Task.ID] && ev.Task.Status.Terminal() {
				got[ev.Task.ID] = ev.Task
			}
		case <-deadline:
			t.Fatalf("timeout waiting for terminal events of %v, got %v", ids, slices.Sorted(maps.Keys(got)))
		}
	}
	return got
}

// childSessionIDFromResponse extracts the Child session ID line of an
// acceptance response.
func childSessionIDFromResponse(t *testing.T, content string) string {
	t.Helper()
	for _, line := range strings.Split(content, "\n") {
		if id, ok := strings.CutPrefix(line, "Child session ID: "); ok {
			return strings.TrimSpace(id)
		}
	}
	t.Fatalf("no Child session ID line in response:\n%s", content)
	return ""
}

// acceptedOf decodes the typed AgentCallAccepted metadata carried by an
// acceptance response.
func acceptedOf(t *testing.T, resp fantasy.ToolResponse) AgentCallAccepted {
	t.Helper()
	require.False(t, resp.IsError, resp.Content)
	var acc AgentCallAccepted
	require.NoError(t, json.Unmarshal([]byte(resp.Metadata), &acc))
	return acc
}

// TestAgentTool_BlockedChildDoesNotBlockCallAgent proves the unified
// asynchronous contract end to end: a child that stays blocked inside
// the provider stream cannot block the tool call, which returns a typed
// acceptance while the task row is still live; after the child is
// released it terminalizes with its result and a persisted transcript,
// all without the caller waiting.
func TestAgentTool_BlockedChildDoesNotBlockCallAgent(t *testing.T) {
	gate := newGatedModel("child answer")
	coord, mgr, env, events := taskToolEnv(t, task.Limits{}, func(env fakeEnv) SessionAgent {
		return fakeChildAgent(env, gate)
	})
	parent, err := env.sessions.Create(t.Context(), "Parent")
	require.NoError(t, err)
	ctx := delegationCtx(t, parent.ID, "msg-accept")

	// The child blocks as soon as it reaches the model. The call must
	// still return an acceptance: if any synchronous path remained,
	// runAgentTool would hang until the gate is released.
	resp := runAgentTool(t, coord, ctx, "call-accept", `{"prompt":"do it"}`)
	acc := acceptedOf(t, resp)
	assert.Contains(t, resp.Content, "Agent task accepted")
	assert.NotEmpty(t, acc.TaskID)
	assert.NotEmpty(t, acc.ChildSessionID)
	assert.Equal(t, string(config.AgentCoder), acc.Profile)
	assert.Equal(t, "mock", acc.Provider)
	assert.Equal(t, "mock-model", acc.Model)

	<-gate.entered
	snap, err := mgr.Status(t.Context(), parent.ID, acc.TaskID)
	require.NoError(t, err)
	assert.True(t, snap.Status.Live(), "the task is still live while the child blocks")
	assert.NotEqual(t, task.StatusCompleted, acc.Status)

	close(gate.release)
	terminal := waitTerminalEvent(t, events, acc.TaskID)
	assert.Equal(t, task.StatusCompleted, terminal.Status)
	assert.Equal(t, "child answer", terminal.Result)

	stored, err := mgr.Status(t.Context(), parent.ID, acc.TaskID)
	require.NoError(t, err)
	assert.Equal(t, "child answer", stored.Result)

	child, err := env.sessions.Get(t.Context(), acc.ChildSessionID)
	require.NoError(t, err)
	assert.Equal(t, parent.ID, child.ParentSessionID)
	msgs, err := env.messages.List(t.Context(), child.ID)
	require.NoError(t, err)
	require.Len(t, msgs, 2, "child transcript keeps user + assistant messages")
}

// TestAgentTool_AcceptanceIsPersistedBeforeReturn proves the durable
// admission half of the contract: the acceptance names a task id whose
// SQLite row is already readable the instant the tool returns, with the
// prompt and bound child session recorded.
func TestAgentTool_AcceptanceIsPersistedBeforeReturn(t *testing.T) {
	gate := newGatedModel("bg answer")
	coord, mgr, env, events := taskToolEnv(t, task.Limits{}, func(env fakeEnv) SessionAgent {
		return fakeChildAgent(env, gate)
	})
	parent, err := env.sessions.Create(t.Context(), "Parent")
	require.NoError(t, err)
	ctx := delegationCtx(t, parent.ID, "msg-bg")

	resp := runAgentTool(t, coord, ctx, "call-bg", `{"prompt":"go"}`)
	require.False(t, resp.IsError, resp.Content)
	assert.Contains(t, resp.Content, "Task ID: ")
	assert.Contains(t, resp.Content, "Child session ID: ")
	assert.Contains(t, resp.Content, "Profile: coder")
	assert.Contains(t, resp.Content, "Model: mock/mock-model")

	taskID := taskIDFromResponse(t, resp.Content)
	snap, err := mgr.Status(t.Context(), parent.ID, taskID)
	require.NoError(t, err, "task row must exist immediately after acceptance return")
	assert.True(t, snap.Status.Live())
	assert.Equal(t, "go", snap.Prompt)
	require.NotEmpty(t, snap.ChildSessionID)

	close(gate.release)
	terminal := waitTerminalEvent(t, events, taskID)
	assert.Equal(t, task.StatusCompleted, terminal.Status)
	assert.Equal(t, "bg answer", terminal.Result)

	stored, _, err := mgr.Output(t.Context(), parent.ID, taskID)
	require.NoError(t, err)
	assert.Equal(t, "bg answer", stored.Text)
}

// TestAgentTool_CallerCancelDoesNotCancelAcceptedTask proves the
// lifetime invariant: cancelling the initiating request context cannot
// abort an accepted child; it runs on the workspace context and
// terminalizes completed.
func TestAgentTool_CallerCancelDoesNotCancelAcceptedTask(t *testing.T) {
	gate := newGatedModel("late answer")
	coord, mgr, env, events := taskToolEnv(t, task.Limits{}, func(env fakeEnv) SessionAgent {
		return fakeChildAgent(env, gate)
	})
	parent, err := env.sessions.Create(t.Context(), "Parent")
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(delegationCtx(t, parent.ID, "msg-cancel"))
	defer cancel()
	resp := runAgentTool(t, coord, ctx, "call-cancel", `{"prompt":"wait"}`)
	require.False(t, resp.IsError, resp.Content)
	taskID := taskIDFromResponse(t, resp.Content)

	select {
	case <-gate.entered:
	case <-time.After(15 * time.Second):
		t.Fatal("child never reached the model")
	}
	// Cancel the request context before the child terminalizes.
	cancel()
	close(gate.release)

	terminal := waitTerminalEvent(t, events, taskID)
	assert.Equal(t, task.StatusCompleted, terminal.Status,
		"request cancellation must not cancel an accepted task")
	assert.Equal(t, "late answer", terminal.Result)

	got, err := mgr.Status(t.Context(), parent.ID, taskID)
	require.NoError(t, err)
	assert.Equal(t, task.StatusCompleted, got.Status)
}

// TestAgentTool_QuotaRejection proves that an exhausted live-task quota
// is surfaced as a tool error naming the quota sentinel.
func TestAgentTool_QuotaRejection(t *testing.T) {
	gate := newGatedModel("slow answer")
	coord, _, env, events := taskToolEnv(t, task.Limits{LiveTasksPerParent: 1}, func(env fakeEnv) SessionAgent {
		return fakeChildAgent(env, gate)
	})
	parent, err := env.sessions.Create(t.Context(), "Parent")
	require.NoError(t, err)
	ctx := delegationCtx(t, parent.ID, "msg-quota")

	first := runAgentTool(t, coord, ctx, "call-q1", `{"prompt":"one"}`)
	require.False(t, first.IsError, first.Content)
	firstID := taskIDFromResponse(t, first.Content)

	// Wait until the first task actually occupies its live slot.
	select {
	case <-gate.entered:
	case <-time.After(15 * time.Second):
		t.Fatal("first child never reached the model")
	}

	second := runAgentTool(t, coord, ctx, "call-q2", `{"prompt":"two"}`)
	require.True(t, second.IsError)
	assert.Contains(t, second.Content, task.ErrQuota.Error())

	close(gate.release)
	terminal := waitTerminalEvent(t, events, firstID)
	assert.Equal(t, task.StatusCompleted, terminal.Status)
}

// TestAgentTool_ChildDepthRejection proves that a delegation attempt from
// a child context is rejected before any task or child session is
// created, using the trusted depth marker rather than tool arguments.
func TestAgentTool_ChildDepthRejection(t *testing.T) {
	coord, mgr, env, _ := taskToolEnv(t, task.Limits{}, func(env fakeEnv) SessionAgent {
		return fakeChildAgent(env, &finishStreamModel{text: "never"})
	})
	parent, err := env.sessions.Create(t.Context(), "Parent")
	require.NoError(t, err)

	ctx := delegationCtx(t, parent.ID, "msg-child")
	ctx = context.WithValue(ctx, tools.AgentDepthContextKey, 1)

	resp := runAgentTool(t, coord, ctx, "call-child", `{"prompt":"delegate"}`)
	require.True(t, resp.IsError)
	assert.Contains(t, resp.Content, task.ErrDelegation.Error())

	tasks, err := mgr.List(t.Context(), parent.ID, "")
	require.NoError(t, err)
	assert.Empty(t, tasks, "rejected delegation must not persist a task")
	_, err = env.sessions.Get(t.Context(), coord.sessions.CreateAgentToolSessionID("msg-child", "call-child"))
	require.Error(t, err, "rejected delegation must not create a child session")
}

// TestAgentTool_ContinuationValidatesOwnerAndReusesChildSession proves
// the explicit continuation contract: a task_id is rejected while its
// attempt is live, a foreign owner cannot continue it, and the owner's
// continuation is accepted against the retained child session with a
// fresh task id that records the predecessor.
func TestAgentTool_ContinuationValidatesOwnerAndReusesChildSession(t *testing.T) {
	gate := newGatedModel("first answer")
	calls := 0
	coord, mgr, env, events := taskToolEnv(t, task.Limits{}, func(env fakeEnv) SessionAgent {
		calls++
		if calls == 1 {
			return fakeChildAgent(env, gate)
		}
		return fakeChildAgent(env, &finishStreamModel{text: "second answer"})
	})

	parent, err := env.sessions.Create(t.Context(), "Parent")
	require.NoError(t, err)
	other, err := env.sessions.Create(t.Context(), "Other")
	require.NoError(t, err)
	ctx := delegationCtx(t, parent.ID, "msg-cont")

	first := acceptedOf(t, runAgentTool(t, coord, ctx, "call-cont-1", `{"prompt":"one"}`))

	// A live attempt cannot be continued.
	live := runAgentTool(t, coord, ctx, "call-cont-live", `{"prompt":"too early","task_id":"`+first.TaskID+`"}`)
	require.True(t, live.IsError)
	assert.Contains(t, live.Content, task.ErrResumeLive.Error())

	// The owner check applies even to foreign continuations of a task
	// they do not own: first seal it, then attempt from another session.
	select {
	case <-gate.entered:
	case <-time.After(15 * time.Second):
		t.Fatal("child never reached the model")
	}
	require.NoError(t, mgr.Cancel(t.Context(), parent.ID, first.TaskID))
	waitTerminalEvent(t, events, first.TaskID)

	foreign := runAgentTool(t, coord, delegationCtx(t, other.ID, "msg-cont-2"), "call-cont-foreign", `{"prompt":"hijack","task_id":"`+first.TaskID+`"}`)
	require.True(t, foreign.IsError)
	assert.Contains(t, foreign.Content, task.ErrNotOwner.Error())

	// The owner's continuation reuses the retained child session under
	// a fresh task id that names its predecessor.
	cont := acceptedOf(t, runAgentTool(t, coord, ctx, "call-cont-3", `{"prompt":"more","task_id":"`+first.TaskID+`"}`))
	assert.NotEqual(t, first.TaskID, cont.TaskID, "continuation uses a fresh task id")
	assert.Equal(t, first.ChildSessionID, cont.ChildSessionID,
		"continuation reuses the retained child session")

	contTerminal := waitTerminalEvent(t, events, cont.TaskID)
	assert.Equal(t, task.StatusCompleted, contTerminal.Status)
	assert.Equal(t, "second answer", contTerminal.Result)
	assert.Equal(t, first.TaskID, contTerminal.ResumesTaskID)

	// No extra child session was created: both attempts share one.
	child, err := env.sessions.Get(t.Context(), first.ChildSessionID)
	require.NoError(t, err)
	assert.Equal(t, parent.ID, child.ParentSessionID)
}

// TestAgentTool_WithoutTaskManagerRejectsAllDelegation proves there is
// no synchronous fallback: without a task manager no durable task can be
// admitted, so every call_agent request is a model-visible rejection and
// no child session is created.
func TestAgentTool_WithoutTaskManagerRejectsAllDelegation(t *testing.T) {
	env := testEnv(t)
	require.NoError(t, os.WriteFile(filepath.Join(env.workingDir, "crush.json"), []byte(profileTestConfig), 0o644))
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)
	cfg.SetupAgents()
	coord := &coordinator{
		cfg:         cfg,
		sessions:    env.sessions,
		messages:    env.messages,
		permissions: env.permissions,
		history:     env.history,
		filetracker: *env.filetracker,
		newChildAgent: func(_ context.Context, name, _ string) (SessionAgent, config.ResolvedProfile, error) {
			return fakeChildAgent(env, &finishStreamModel{text: "must never run"}), config.ResolvedProfile{Name: name}, nil
		},
	}
	parent, err := env.sessions.Create(t.Context(), "Parent")
	require.NoError(t, err)
	ctx := delegationCtx(t, parent.ID, "msg-nomgr")

	resp := runAgentTool(t, coord, ctx, "call-nomgr", `{"prompt":"do it"}`)
	require.True(t, resp.IsError)
	assert.Contains(t, resp.Content, "no task manager")

	cont := runAgentTool(t, coord, ctx, "call-nomgr-cont", `{"prompt":"do it","task_id":"t-1"}`)
	require.True(t, cont.IsError)
	assert.Contains(t, cont.Content, "no task manager")

	_, err = env.sessions.Get(t.Context(), coord.sessions.CreateAgentToolSessionID("msg-nomgr", "call-nomgr"))
	require.Error(t, err, "rejected delegation must not create a child session")
}
