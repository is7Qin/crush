package task

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// promptRecorder is a manager test runner that records the prompt
// each attempt was dispatched with and serializes attempts through a
// shared in-flight probe.
type promptRecorder struct {
	mu       sync.Mutex
	prompts  []string
	inFlight atomic.Int32
	maxSeen  atomic.Int32
	gate     chan struct{}
}

func newPromptRecorder() *promptRecorder {
	return &promptRecorder{gate: make(chan struct{})}
}

func (p *promptRecorder) run(ctx context.Context, h *Handle) (Result, error) {
	cur := p.inFlight.Add(1)
	for {
		mx := p.maxSeen.Load()
		if cur <= mx || p.maxSeen.CompareAndSwap(mx, cur) {
			break
		}
	}
	defer p.inFlight.Add(-1)

	p.mu.Lock()
	p.prompts = append(p.prompts, h.Prompt())
	p.mu.Unlock()

	select {
	case <-p.gate:
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
	return Result{Text: "answer for " + h.Prompt()}, nil
}

func (p *promptRecorder) observed() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string{}, p.prompts...)
}

func appendMessage(t *testing.T, m *Manager, owner, taskID, prompt string) MessageAccepted {
	t.Helper()
	acc, err := m.AppendMessage(t.Context(), MessageRequest{
		OwnerSessionID: owner, TaskID: taskID, Origin: OriginParent, Prompt: prompt,
	})
	require.NoError(t, err)
	return acc
}

// attemptWithGeneration picks the owner's attempt with the given run
// generation; index-order assumptions would tie on identical
// nanosecond created_at stamps.
func attemptWithGeneration(tasks []*Task, gen uint64) *Task {
	for _, t := range tasks {
		if t.RunGeneration == gen {
			return t
		}
	}
	return nil
}

func TestManager_QueuedMessageRunsAsSuccessorAttempt(t *testing.T) {
	t.Parallel()
	m, mem := newTestManagerStore(t, Limits{RunningPerModel: 1})
	rec := newPromptRecorder()

	child := uniqueChild()
	first, err := m.Start(t.Context(), StartRequest{
		CallerSessionID: "owner", Prompt: "initial", Provider: "p", Model: "m",
		ChildSessionID: child, Run: rec.run,
	})
	require.NoError(t, err)
	waitStatus(t, m, "owner", first.ID, StatusRunning)

	// While running, a message queues and creates no attempt.
	acc := appendMessage(t, m, "owner", first.ID, "m1")
	require.Equal(t, StatusRunning, acc.Status)
	require.Empty(t, acc.AttemptTaskID, "a running task defers the attempt to the turn boundary")
	require.Equal(t, uint64(1), acc.Sequence)

	// A second message queues behind the first, FIFO.
	acc2 := appendMessage(t, m, "owner", first.ID, "m2")
	require.Equal(t, uint64(2), acc2.Sequence)

	close(rec.gate)
	waitStatus(t, m, "owner", first.ID, StatusCompleted)

	attempts := waitAttemptCount(t, m, "owner", 2)
	succ1 := attemptWithGeneration(attempts, 2)
	require.NotNil(t, succ1)
	require.Equal(t, first.ID, succ1.ResumesTaskID)
	require.Equal(t, "m1", succ1.Prompt, "the successor delivers the lowest queued message")
	waitStatus(t, m, "owner", succ1.ID, StatusCompleted)

	// The second message then gets its own attempt; release the
	// recorder gate is already closed, so wait for three prompts.
	require.Eventually(t, func() bool {
		got, err := m.List(context.Background(), "owner", "")
		if err != nil || len(got) != 3 {
			return false
		}
		succ2 := attemptWithGeneration(got, 3)
		return succ2 != nil && succ2.Prompt == "m2"
	}, 5*time.Second, 5*time.Millisecond, "m2 never became the third attempt")

	require.Equal(t, []string{"initial", "m1", "m2"}, rec.observed(),
		"attempts run in mailbox order with the delivered prompt")
	require.EqualValues(t, 1, rec.maxSeen.Load(), "no two child attempts ran concurrently")

	// Every delivered message records its claim and lineage.
	msgs, err := mem.ListChildMessages(t.Context(), child)
	require.NoError(t, err)
	require.Len(t, msgs, 3)
	for i, want := range []MessageState{MessageDelivered, MessageDelivered, MessageDelivered} {
		require.Equal(t, want, msgs[i].State, "row %d", i)
		require.False(t, msgs[i].DeliveredAt.IsZero(), "row %d delivered_at", i)
	}
}

func TestManager_AppendToTerminalReportsImmediateAttempt(t *testing.T) {
	t.Parallel()
	m := newTestManager(t, Limits{})
	quick := nopRun("first answer")

	first, err := m.Start(t.Context(), StartRequest{
		CallerSessionID: "owner", Prompt: "initial", Provider: "p", Model: "m",
		ChildSessionID: uniqueChild(), Run: quick,
	})
	require.NoError(t, err)
	waitStatus(t, m, "owner", first.ID, StatusCompleted)

	acc := appendMessage(t, m, "owner", first.ID, "after terminal")
	require.NotEmpty(t, acc.AttemptTaskID,
		"appending to a terminal task creates the next attempt immediately")
	require.Equal(t, StatusCompleted, acc.Status, "the addressed task status is reported as-is")

	got := waitStatus(t, m, "owner", acc.AttemptTaskID, StatusCompleted)
	require.Equal(t, "after terminal", got.Prompt)
	require.Equal(t, uint64(2), got.RunGeneration)
	require.Equal(t, "first answer", func() string {
		old, err := m.Status(context.Background(), "owner", first.ID)
		require.NoError(t, err)
		return old.Result
	}())
}

func TestManager_MessageNeverResumesWaitingAttempt(t *testing.T) {
	t.Parallel()
	m, mem := newTestManagerStore(t, Limits{RunningPerModel: 1})
	child := uniqueChild()
	asked := make(chan struct{})
	releaseResume := make(chan struct{})
	var first sync.Once
	var askedOnce sync.Once
	var firstPrompt string

	firstTask, err := m.Start(t.Context(), StartRequest{
		CallerSessionID: "owner", Prompt: "initial", Provider: "p", Model: "m",
		ChildSessionID: child,
		Run: func(ctx context.Context, h *Handle) (Result, error) {
			first.Do(func() { firstPrompt = h.Prompt() })
			require.NoError(t, h.WaitingForInput(ctx))
			askedOnce.Do(func() { close(asked) })
			select {
			case <-releaseResume:
			case <-ctx.Done():
				return Result{}, ctx.Err()
			}
			require.NoError(t, h.Resumed(ctx))
			return Result{Text: "done waiting"}, nil
		},
	})
	require.NoError(t, err)
	<-asked
	waitStatus(t, m, "owner", firstTask.ID, StatusWaitingForInput)

	acc := appendMessage(t, m, "owner", firstTask.ID, "wrong channel")
	require.Empty(t, acc.AttemptTaskID, "a message never starts an attempt for a waiting task")

	msgs, err := mem.ListChildMessages(t.Context(), child)
	require.NoError(t, err)
	require.Equal(t, MessageQueued, msgs[1].State, "the message waits untouched")

	close(releaseResume)
	waitStatus(t, m, "owner", firstTask.ID, StatusCompleted)
	require.Equal(t, "initial", firstPrompt,
		"the waiting attempt keeps its own prompt; the message is not folded in")

	// The message runs as a successor once the attempt terminalizes.
	attempts := waitAttemptCount(t, m, "owner", 2)
	succ := attemptWithGeneration(attempts, 2)
	require.NotNil(t, succ)
	require.Equal(t, "wrong channel", succ.Prompt)
}

func TestManager_PendingMessagesStayQueuedUntilInitialDispatch(t *testing.T) {
	t.Parallel()
	m, mem := newTestManagerStore(t, Limits{RunningPerModel: 1})
	release := make(chan struct{})
	defer close(release)
	block := func(ctx context.Context, h *Handle) (Result, error) {
		select {
		case <-release:
			return Result{}, nil
		case <-ctx.Done():
			return Result{}, ctx.Err()
		}
	}
	// Occupy the single slot on the model key with another child.
	busy, err := m.Start(t.Context(), StartRequest{
		CallerSessionID: "owner", Prompt: "busy", Provider: "p", Model: "m",
		ChildSessionID: uniqueChild(), Run: block,
	})
	require.NoError(t, err)
	waitStatus(t, m, "owner", busy.ID, StatusRunning)

	second := uniqueChild()
	pending, err := m.Start(t.Context(), StartRequest{
		CallerSessionID: "owner", Prompt: "held", Provider: "p", Model: "m",
		ChildSessionID: second, Run: nopRun("late"),
	})
	require.NoError(t, err)
	require.Equal(t, StatusPending, pending.Status)

	acc := appendMessage(t, m, "owner", pending.ID, "while pending")
	require.Equal(t, uint64(1), acc.Sequence, "pending-task messages queue behind the admission row")
	require.Empty(t, acc.AttemptTaskID)

	msgs, err := mem.ListChildMessages(t.Context(), second)
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	require.Equal(t, MessageQueued, msgs[0].State)
	require.Equal(t, MessageQueued, msgs[1].State)
}

func TestManager_AppendMessageAuthorization(t *testing.T) {
	t.Parallel()
	m := newTestManager(t, Limits{})
	task, err := m.Start(t.Context(), StartRequest{
		CallerSessionID: "owner", Prompt: "work", Provider: "p", Model: "m",
		ChildSessionID: uniqueChild(), Run: nopRun("done"),
	})
	require.NoError(t, err)
	waitStatus(t, m, "owner", task.ID, StatusCompleted)

	_, err = m.AppendMessage(t.Context(), MessageRequest{
		OwnerSessionID: "intruder", TaskID: task.ID, Origin: OriginUser, Prompt: "x",
	})
	require.ErrorIs(t, err, ErrNotOwner)

	_, err = m.AppendMessage(t.Context(), MessageRequest{
		OwnerSessionID: "owner", TaskID: "unknown", Origin: OriginUser, Prompt: "x",
	})
	require.ErrorIs(t, err, ErrNotFound)

	_, err = m.AppendMessage(t.Context(), MessageRequest{
		OwnerSessionID: "owner", TaskID: task.ID, Origin: OriginUser,
	})
	require.ErrorIs(t, err, ErrInvalidRequest)
}
