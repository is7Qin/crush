package taskquestion

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/charmbracelet/crush/internal/question"
	"github.com/stretchr/testify/require"
)

func yesNoBatch() question.Request {
	return question.Request{
		SessionID:  "child-1",
		ToolCallID: "tc-1",
		Questions: []question.Question{{
			Type:        question.TypeYesNo,
			Text:        "Proceed?",
			Description: "Confirm the destructive step.",
		}},
	}
}

func askRequest(taskID string) TaskQuestionRequest {
	return TaskQuestionRequest{
		TaskID:         taskID,
		OwnerSessionID: "owner-1",
		ChildSessionID: "child-1",
		RunGeneration:  1,
		Batch:          yesNoBatch(),
	}
}

func answer(id string, yes bool) []question.Answer {
	return []question.Answer{{QuestionID: id, Yes: &yes}}
}

// harness wires a service with recording suspend/resume callbacks so
// tests can wait for the exact moment the question is registered.
type harness struct {
	svc        *taskQuestionService
	suspended  chan string
	resumed    chan string
	suspendErr error
}

func newHarness() *harness {
	h := &harness{
		suspended: make(chan string, 8),
		resumed:   make(chan string, 8),
	}
	h.svc = NewService(Config{
		Suspend: func(_ context.Context, id string) error {
			h.suspended <- id
			return h.suspendErr
		},
		Resume: func(_ context.Context, id string) error {
			h.resumed <- id
			return nil
		},
	})
	return h
}

type askResult struct {
	answers []question.Answer
	err     error
}

// ask starts AskTask on a background runner context and returns once
// the question is registered and the task suspended.
func (h *harness) ask(t *testing.T, req TaskQuestionRequest) (<-chan askResult, string) {
	t.Helper()
	return h.askCtx(t, context.Background(), req)
}

func (h *harness) askCtx(t *testing.T, ctx context.Context, req TaskQuestionRequest) (<-chan askResult, string) {
	t.Helper()
	ch := make(chan askResult, 1)
	go func() {
		answers, err := h.svc.AskTask(ctx, req)
		ch <- askResult{answers: answers, err: err}
	}()
	select {
	case id := <-h.suspended:
		return ch, id
	case <-time.After(2 * time.Second):
		t.Fatal("AskTask never suspended the task")
		return nil, ""
	}
}

func TestAskTask_SuspendFailureCancelsQuestion(t *testing.T) {
	t.Parallel()
	h := newHarness()
	h.suspendErr = errors.New("manager refused")

	_, err := h.svc.AskTask(t.Context(), askRequest("task-1"))
	require.ErrorIs(t, err, h.suspendErr)
	require.Empty(t, h.svc.Unresolved(), "failed suspension must leave no tracked waiter")
	require.ErrorIs(t, h.svc.CancelTask("owner-1", <-h.suspended), ErrAlreadyResolved)
}

func TestAskTask_AnswerWakesRunnerAndResumes(t *testing.T) {
	t.Parallel()
	h := newHarness()
	notes := h.svc.SubscribeNotifications(t.Context())
	events := h.svc.Subscribe(t.Context())

	req := askRequest("task-1")
	ch, qid := h.ask(t, req)

	// The batch reaches subscribers with ids assigned.
	var batch question.Request
	select {
	case ev := <-events:
		batch = ev.Payload.Batch
	case <-time.After(2 * time.Second):
		t.Fatal("no batch event published")
	}
	require.NotEmpty(t, batch.ID)
	require.NotEmpty(t, batch.Questions[0].ID)

	pending, ok := h.svc.Pending(qid)
	require.True(t, ok)
	require.Equal(t, "task-1", pending.TaskID)
	require.Equal(t, "owner-1", pending.OwnerSessionID)
	require.Equal(t, ResolutionPending, pending.Resolution)

	want := answer(batch.Questions[0].ID, true)
	require.NoError(t, h.svc.AnswerTask("owner-1", qid, want))

	select {
	case got := <-ch:
		require.NoError(t, got.err)
		require.Equal(t, want, got.answers)
	case <-time.After(2 * time.Second):
		t.Fatal("runner never woke")
	}
	require.Equal(t, qid, <-h.resumed, "resume runs on the waking runner")
	_, ok = h.svc.Pending(qid)
	require.False(t, ok, "waiter must be untracked after resolution")

	ev := <-notes
	require.Equal(t, pubsub.CreatedEvent, ev.Type)
	require.Equal(t, Notification{QuestionID: qid, TaskID: "task-1", BatchID: batch.ID, Resolution: ResolutionAnswered}, ev.Payload)
}

func TestAskTask_SecondQuestionForSameTaskRejected(t *testing.T) {
	t.Parallel()
	h := newHarness()
	ch, qid := h.ask(t, askRequest("task-1"))

	_, err := h.svc.AskTask(t.Context(), askRequest("task-1"))
	require.ErrorIs(t, err, ErrQuestionPending)

	_, ok := h.svc.Pending(qid)
	require.True(t, ok, "first question must be untouched")
	require.Len(t, h.svc.Unresolved(), 1)

	require.NoError(t, h.svc.AnswerTask("owner-1", qid, answer("q", false)))
	require.NoError(t, (<-ch).err)
}

func TestAskTask_PerTaskSlotsAreIndependent(t *testing.T) {
	t.Parallel()
	h := newHarness()
	_, id1 := h.ask(t, askRequest("task-1"))
	_, id2 := h.ask(t, askRequest("task-2"))
	require.NotEqual(t, id1, id2)

	// Pin creation times so the oldest-first ordering is
	// deterministic; near-simultaneous time.Now stamps would
	// otherwise let the QuestionID tie-break decide the order.
	base := time.Now().UTC()
	h.svc.mu.Lock()
	h.svc.pending[id1].q.CreatedAt = base
	h.svc.pending[id2].q.CreatedAt = base.Add(time.Second)
	h.svc.mu.Unlock()

	require.Len(t, h.svc.Unresolved(), 2)
	require.Equal(t, []string{"task-1", "task-2"},
		[]string{h.svc.Unresolved()[0].TaskID, h.svc.Unresolved()[1].TaskID},
		"oldest first")
}

func TestAskTask_TimeoutWinsWhenNoAnswer(t *testing.T) {
	t.Parallel()
	h := newHarness()
	req := askRequest("task-1")
	req.Timeout = 50 * time.Millisecond
	ch, qid := h.ask(t, req)

	got := <-ch
	require.ErrorIs(t, got.err, ErrTimeout)
	select {
	case <-h.resumed:
		t.Fatal("timeout must not resume the task")
	default:
	}
	require.ErrorIs(t, h.svc.AnswerTask("owner-1", qid, answer("q", true)), ErrAlreadyResolved)
}

func TestAskTask_TimeoutLosesToAnswer(t *testing.T) {
	t.Parallel()
	h := newHarness()
	req := askRequest("task-1")
	req.Timeout = 50 * time.Millisecond
	ch, qid := h.ask(t, req)

	// Answer well before the timer fires; the timer is stopped on
	// return and the late control call reports the committed win.
	require.NoError(t, h.svc.AnswerTask("owner-1", qid, answer("q", true)))
	got := <-ch
	require.NoError(t, got.err)
	require.Len(t, got.answers, 1)
	require.Equal(t, qid, <-h.resumed)
}

func TestAskTask_RunnerContextCancelUntracksWaiter(t *testing.T) {
	t.Parallel()
	h := newHarness()
	ctx, cancel := context.WithCancel(context.Background())
	ch, qid := h.askCtx(t, ctx, askRequest("task-1"))

	cancel()
	got := <-ch
	require.ErrorIs(t, got.err, context.Canceled)
	_, ok := h.svc.Pending(qid)
	require.False(t, ok, "no waiter may stay tracked after the runner leaves")
	require.ErrorIs(t, h.svc.CancelTask("owner-1", qid), ErrAlreadyResolved)
}

func TestAskTask_InvalidRequests(t *testing.T) {
	t.Parallel()
	svc := NewService(Config{})

	_, err := svc.AskTask(t.Context(), TaskQuestionRequest{})
	require.ErrorIs(t, err, ErrInvalidRequest)

	req := askRequest("task-1")
	req.Batch = question.Request{}
	_, err = svc.AskTask(t.Context(), req)
	require.ErrorIs(t, err, ErrInvalidRequest)

	_, ok := svc.Pending("anything")
	require.False(t, ok)
	require.Empty(t, svc.Unresolved())
}

func TestAskTask_MultiQuestionBatchGetsConfirmDefaults(t *testing.T) {
	t.Parallel()
	svc := NewService(Config{})
	events := svc.Subscribe(t.Context())

	req := askRequest("task-1")
	req.Batch.Questions = append(req.Batch.Questions, question.Question{
		Type:        question.TypeFreeText,
		Text:        "Which env?",
		Description: "Target environment.",
	})
	done := make(chan askResult, 1)
	go func() {
		a, err := svc.AskTask(context.Background(), req)
		done <- askResult{a, err}
	}()

	ev := <-events
	require.Equal(t, "Ready to go?", ev.Payload.Batch.ConfirmTitle)
	require.NotEmpty(t, ev.Payload.Batch.ConfirmDescription)
	require.NoError(t, svc.CancelTask("owner-1", svc.Unresolved()[0].QuestionID))
	require.ErrorIs(t, (<-done).err, question.ErrCancelled)
}
