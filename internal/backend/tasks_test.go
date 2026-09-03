package backend

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/agent/task"
	"github.com/charmbracelet/crush/internal/agent/taskquestion"
	"github.com/charmbracelet/crush/internal/app"
	"github.com/charmbracelet/crush/internal/proto"
	"github.com/charmbracelet/crush/internal/question"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

// taskTestSessions is a live-set-only session.Service: Get reports
// membership, everything else is absent (task control only reads).
type taskTestSessions struct {
	session.Service

	mu   sync.Mutex
	live map[string]struct{}
}

func (s *taskTestSessions) Get(_ context.Context, id string) (session.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.live[id]; ok {
		return session.Session{ID: id}, nil
	}
	return session.Session{}, sql.ErrNoRows
}

func newTaskControlForTest(t *testing.T) (TaskControl, *taskTestSessions) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	a := app.NewForTest(ctx)
	sessions := &taskTestSessions{live: map[string]struct{}{}}
	a.Sessions = sessions
	tc, err := TaskControlFromApp(a)
	require.NoError(t, err)
	return tc, sessions
}

func seedTask(t *testing.T, tc TaskControl, id, owner string, status task.Status) *task.Task {
	t.Helper()
	now := time.Now().UTC()
	rec := &task.Task{
		ID: id, OwnerSessionID: owner, ParentSessionID: owner,
		ChildSessionID: "child-" + id, ParentMessageID: "pm-" + id,
		ToolCallID: "tc-" + id, Profile: "coder", Provider: "p", Model: "m",
		RunGeneration: 1, Status: status, Prompt: "do it",
		CreatedAt: now, UpdatedAt: now,
	}
	require.NoError(t, tc.Mgr.Store().Save(context.Background(), rec))
	return rec
}

// askTaskBackground blocks in AskTask on the test context and returns
// once the pending record is observable, plus a stopper that cancels
// the waiter and joins the goroutine.
func askTaskBackground(t *testing.T, tc TaskControl, taskID, owner string) (taskquestion.TaskQuestion, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = tc.Quest.AskTask(ctx, taskquestion.TaskQuestionRequest{
			TaskID:         taskID,
			OwnerSessionID: owner,
			ChildSessionID: "child-" + taskID,
			RunGeneration:  1,
			Batch:          testYesNoBatch(),
		})
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		for _, q := range tc.Quest.Unresolved() {
			if q.TaskID == taskID {
				return q, func() { cancel(); <-done }
			}
		}
		require.True(t, time.Now().Before(deadline), "task question never registered")
		time.Sleep(5 * time.Millisecond)
	}
}

func testYesNoBatch() question.Request {
	return question.Request{
		SessionID:  "child",
		ToolCallID: "tc-question",
		Questions: []question.Question{{
			Type:        question.TypeYesNo,
			Text:        "Proceed?",
			Description: "Continue with the step?",
		}},
	}
}

func boolPtr(b bool) *bool { return &b }

func TestTaskControl_GetAuthorization(t *testing.T) {
	t.Parallel()
	tc, sessions := newTaskControlForTest(t)
	sessions.live["owner"] = struct{}{}
	seedTask(t, tc, "t1", "owner", task.StatusRunning)
	seedTask(t, tc, "gone", "ghost", task.StatusRunning)

	// Owner reads its own task.
	snap, err := tc.Get(context.Background(), "owner", "t1")
	require.NoError(t, err)
	require.Equal(t, "t1", snap.ID)
	require.Equal(t, "tc-t1", snap.ToolCallID)
	require.Equal(t, "owner", snap.OwnerSessionID)

	// Unknown id is not-found.
	_, err = tc.Get(context.Background(), "owner", "nope")
	require.ErrorIs(t, err, ErrTaskNotFound)

	// Foreign live owner is forbidden.
	_, err = tc.Get(context.Background(), "intruder", "t1")
	require.ErrorIs(t, err, ErrTaskForbidden)

	// A deleted owner's retained id is gone, not forbidden.
	_, err = tc.Get(context.Background(), "owner", "gone")
	require.ErrorIs(t, err, ErrTaskOwnerDeleted)
}

func TestTaskControl_CancelRequiresLiveNonTerminal(t *testing.T) {
	t.Parallel()
	tc, sessions := newTaskControlForTest(t)
	sessions.live["owner"] = struct{}{}
	seedTask(t, tc, "done", "owner", task.StatusCompleted)

	_, err := tc.Cancel(context.Background(), "owner", "done")
	require.ErrorIs(t, err, ErrTaskAlreadyTerminal)

	_, err = tc.Cancel(context.Background(), "intruder", "done")
	require.ErrorIs(t, err, ErrTaskForbidden)
}

func TestTaskControl_ListParentFilter(t *testing.T) {
	t.Parallel()
	tc, sessions := newTaskControlForTest(t)
	sessions.live["owner"] = struct{}{}
	seedTask(t, tc, "t1", "owner", task.StatusPending)

	tasks, err := tc.List(context.Background(), "owner", "")
	require.NoError(t, err)
	require.Len(t, tasks, 1)

	// A parent filter naming another session is rejected outright.
	_, err = tc.List(context.Background(), "owner", "someone-else")
	require.ErrorIs(t, err, ErrTaskForbidden)
}

func TestTaskControl_ChildMessageAuthorization(t *testing.T) {
	t.Parallel()
	tc, sessions := newTaskControlForTest(t)
	sessions.live["owner"] = struct{}{}
	seedTask(t, tc, "t1", "owner", task.StatusRunning)

	// Empty prompt is rejected before any mailbox write.
	_, err := tc.AppendChildMessage(context.Background(), ChildMessageInput{
		CallerSessionID: "owner", TaskID: "t1",
	})
	require.ErrorIs(t, err, ErrEmptyPrompt)

	// Foreign callers cannot enqueue.
	_, err = tc.AppendChildMessage(context.Background(), ChildMessageInput{
		CallerSessionID: "intruder", TaskID: "t1", Prompt: "hi",
	})
	require.ErrorIs(t, err, ErrTaskForbidden)

	// The owner's message is accepted with FIFO metadata and the
	// derived child session id. (The seeded record bypassed admission,
	// so this appended row is sequence zero; the real FIFO-after-zero
	// path is covered by the SSE integration tests.)
	acc, err := tc.AppendChildMessage(context.Background(), ChildMessageInput{
		CallerSessionID: "owner", TaskID: "t1", Prompt: "and one more thing",
	})
	require.NoError(t, err)
	require.Equal(t, "t1", acc.TaskID)
	require.Equal(t, "child-t1", acc.ChildSessionID)
	require.Equal(t, uint64(0), acc.Sequence)
	require.Empty(t, acc.AttemptTaskID, "running tasks never claim a successor attempt")
	require.Equal(t, string(task.StatusRunning), acc.Status)
}

func TestTaskControl_QuestionAnswerFlow(t *testing.T) {
	t.Parallel()
	tc, sessions := newTaskControlForTest(t)
	sessions.live["owner"] = struct{}{}
	seedTask(t, tc, "t1", "owner", task.StatusWaitingForInput)
	seedTask(t, tc, "t2", "ghost", task.StatusWaitingForInput)

	q, stopQ := askTaskBackground(t, tc, "t1", "owner")
	defer stopQ()
	g, stopG := askTaskBackground(t, tc, "t2", "ghost")
	defer stopG()
	// "ghost" was never added to the live set: its questions and
	// tasks classify as owner-deleted for control callers.

	// Malformed data is rejected before any control runs.
	_, err := tc.AnswerQuestion(context.Background(), "owner", proto.TaskQuestionAnswerRequest{})
	require.ErrorIs(t, err, ErrInvalidTaskQuestionAnswer)
	_, err = tc.AnswerQuestion(context.Background(), "owner", proto.TaskQuestionAnswerRequest{
		QuestionID: q.QuestionID,
		Responses:  []proto.TaskQuestionAnswer{{}},
	})
	require.ErrorIs(t, err, ErrInvalidTaskQuestionAnswer)

	// Unknown ids are not-found; foreign owners never alter state.
	_, err = tc.AnswerQuestion(context.Background(), "owner", proto.TaskQuestionAnswerRequest{
		QuestionID: "nope", Responses: []proto.TaskQuestionAnswer{{QuestionID: "x"}},
	})
	require.ErrorIs(t, err, ErrTaskQuestionNotFound)
	_, err = tc.AnswerQuestion(context.Background(), "intruder", proto.TaskQuestionAnswerRequest{
		QuestionID: q.QuestionID,
		Responses:  []proto.TaskQuestionAnswer{{QuestionID: "x", Yes: boolPtr(true)}},
	})
	require.ErrorIs(t, err, ErrTaskForbidden)
	// A question owned by a deleted session cancels as owner-deleted.
	_, err = tc.CancelQuestion(context.Background(), "intruder", g.QuestionID)
	require.ErrorIs(t, err, ErrTaskOwnerDeleted)

	// The owner answers by question id with the batch question id.
	resolved, err := tc.AnswerQuestion(context.Background(), "owner", proto.TaskQuestionAnswerRequest{
		QuestionID: q.QuestionID,
		Responses:  []proto.TaskQuestionAnswer{{QuestionID: q.Batch.Questions[0].ID, Yes: boolPtr(true)}},
	})
	require.NoError(t, err)
	require.True(t, resolved)

	// A second resolution observes already-resolved.
	_, err = tc.AnswerQuestion(context.Background(), "owner", proto.TaskQuestionAnswerRequest{
		QuestionID: q.QuestionID,
		Responses:  []proto.TaskQuestionAnswer{{QuestionID: "x", Yes: boolPtr(false)}},
	})
	require.ErrorIs(t, err, ErrTaskQuestionResolved)
}

func TestTaskControl_PendingQuestionsScopedToOwner(t *testing.T) {
	t.Parallel()
	tc, sessions := newTaskControlForTest(t)
	sessions.live["owner"] = struct{}{}
	sessions.live["other"] = struct{}{}
	seedTask(t, tc, "t1", "owner", task.StatusWaitingForInput)
	seedTask(t, tc, "t2", "other", task.StatusWaitingForInput)

	q1, stop1 := askTaskBackground(t, tc, "t1", "owner")
	defer stop1()
	q2, stop2 := askTaskBackground(t, tc, "t2", "other")
	defer stop2()
	_ = q2

	own, err := tc.PendingQuestions(context.Background(), "owner")
	require.NoError(t, err)
	require.Len(t, own, 1)
	require.Equal(t, q1.QuestionID, own[0].QuestionID)
	require.Equal(t, "t1", own[0].TaskID)
	require.Equal(t, "owner", own[0].OwnerSessionID)
	require.NotEmpty(t, own[0].Batch.Questions)
	require.Equal(t, "pending", own[0].Resolution)
	require.Nil(t, own[0].ResolvedAt)
}

// TestTaskControl_HiddenTasksAreNotPubliclyAddressable proves the
// final feature boundary at the control-plane layer every public task
// route funnels through: hidden system-owned (agentic_fetch) tasks
// report not-found on list, status, output, cancel, and direct
// messages - even to their owner - while their terminal result still
// reaches the parent inbox.
func TestTaskControl_HiddenTasksAreNotPubliclyAddressable(t *testing.T) {
	t.Parallel()
	tc, sessions := newTaskControlForTest(t)
	sessions.live["owner"] = struct{}{}
	hidden := seedTask(t, tc, "hidden1", "owner", task.StatusCompleted)
	hidden.Profile = task.HiddenProfile
	require.NoError(t, tc.Mgr.Store().Save(context.Background(), hidden))
	seedTask(t, tc, "t1", "owner", task.StatusCompleted)

	_, err := tc.Get(context.Background(), "owner", "hidden1")
	require.ErrorIs(t, err, ErrTaskNotFound)
	_, _, err = tc.Mgr.Output(context.Background(), "owner", "hidden1")
	require.ErrorIs(t, err, task.ErrNotFound)
	_, err = tc.Cancel(context.Background(), "owner", "hidden1")
	require.ErrorIs(t, err, ErrTaskNotFound)
	_, err = tc.AppendChildMessage(context.Background(), ChildMessageInput{
		CallerSessionID: "owner", TaskID: "hidden1", Prompt: "more",
	})
	require.ErrorIs(t, err, ErrTaskNotFound)

	list, err := tc.List(context.Background(), "owner", "")
	require.NoError(t, err)
	require.Len(t, list, 1, "the public list carries only the call_agent task")
	require.Equal(t, "t1", list[0].ID)

	resync, err := tc.Resync(context.Background(), "owner")
	require.NoError(t, err)
	for _, snap := range resync.Tasks {
		require.NotEqual(t, "hidden1", snap.ID, "resync never surfaces hidden task snapshots")
	}

	// Internal diagnostics with the trusted workspace and owner read
	// the hidden record.
	diag, err := tc.Mgr.DiagnosticTask(context.Background(), "test", "owner", "hidden1")
	require.NoError(t, err)
	require.Equal(t, task.HiddenProfile, diag.Profile)
}

// TestTaskControl_ResyncOmitsHiddenMailboxPayloads proves the resync
// projection boundary for the durable mailbox: a hidden task's
// terminalization writes real outbox and inbox rows, and public
// resync must omit both payloads while keeping the rows durable for
// internal parent delivery, and ordinary public payloads intact.
func TestTaskControl_ResyncOmitsHiddenMailboxPayloads(t *testing.T) {
	t.Parallel()
	tc, sessions := newTaskControlForTest(t)
	sessions.live["owner"] = struct{}{}
	seedTask(t, tc, "pub1", "owner", task.StatusRunning)
	hidden := seedTask(t, tc, "hid1", "owner", task.StatusRunning)
	hidden.Profile = task.HiddenProfile
	require.NoError(t, tc.Mgr.Store().Save(context.Background(), hidden))

	// Terminalize both attempts through the durable delivery
	// transaction so each gets its outbox and inbox rows.
	now := time.Now().UTC()
	for _, id := range []string{"pub1", "hid1"} {
		_, won, err := tc.Mgr.Store().TerminalizeAndDeliver(context.Background(), id, 1, task.TerminalUpdate{
			Status: task.StatusCompleted, Result: "bounded result", Summary: "done", CompletedAt: now,
		})
		require.NoError(t, err)
		require.True(t, won, "%s terminalization must win", id)
	}

	resync, err := tc.Resync(context.Background(), "owner")
	require.NoError(t, err)

	require.Len(t, resync.Tasks, 1, "only the public snapshot is listed")
	require.Equal(t, "pub1", resync.Tasks[0].ID)

	require.Len(t, resync.Outbox, 1, "the hidden task's outbox payload is omitted")
	require.Equal(t, "pub1", resync.Outbox[0].TaskID)
	require.NotContains(t, string(resync.Outbox[0].Payload), task.HiddenProfile)

	require.Len(t, resync.Inbox, 1, "the hidden task's inbox payload is omitted")
	require.Equal(t, "pub1", resync.Inbox[0].TaskID)
	require.NotContains(t, string(resync.Inbox[0].Payload), task.HiddenProfile)

	// Filtering happens at the projection boundary only: both hidden
	// rows stay durable and undelivered for internal parent delivery.
	outbox, err := tc.Mgr.Outbox(context.Background())
	require.NoError(t, err)
	var hiddenOutbox bool
	for _, e := range outbox {
		if e.TaskID == "hid1" {
			hiddenOutbox = true
		}
	}
	require.True(t, hiddenOutbox, "the hidden outbox row must remain durable")
	inbox, err := tc.Mgr.Inbox(context.Background(), "owner")
	require.NoError(t, err)
	var hiddenInbox bool
	for _, e := range inbox {
		if e.TaskID == "hid1" {
			hiddenInbox = true
		}
	}
	require.True(t, hiddenInbox, "the hidden inbox row must remain durable")
}
