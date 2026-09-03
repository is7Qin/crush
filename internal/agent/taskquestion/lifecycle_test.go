package taskquestion

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/question"
	"github.com/stretchr/testify/require"
)

// Injected seam failures for the lifecycle tests.
var (
	errBegin   = errors.New("begin boom")
	errResolve = errors.New("resolve boom")
	errResume  = errors.New("resume boom")
)

func TestLifecycle_BeginWaitCommitsBeforePublish(t *testing.T) {
	t.Parallel()
	svc, life := newLifecycleService()
	events := svc.Subscribe(t.Context())
	go func() { _, _ = svc.AskTask(context.Background(), askRequest("task-1")) }()

	var batch question.Request
	select {
	case ev := <-events:
		batch = ev.Payload.Batch
	case <-time.After(2 * time.Second):
		t.Fatal("no batch event published")
	}
	// At the moment the batch becomes observable, the durable wait
	// already committed: publication never precedes BeginWait.
	require.Equal(t, 1, life.count("begin:"),
		"BeginWait must commit before the batch is published")
	require.NotEmpty(t, batch.ID)
	un := svc.Unresolved()
	require.Len(t, un, 1)
	require.Equal(t, "task-1", un[0].TaskID)
	require.Equal(t, "owner-1", un[0].OwnerSessionID)
	require.Equal(t, "child-1", un[0].ChildSessionID)
	require.Equal(t, uint64(1), un[0].RunGeneration)
}

func TestLifecycle_BeginWaitFailureLeavesNoTrace(t *testing.T) {
	t.Parallel()
	svc, life := newLifecycleService()
	life.mu.Lock()
	life.beginErr = errBegin
	life.mu.Unlock()

	events := svc.Subscribe(t.Context())
	_, err := svc.AskTask(t.Context(), askRequest("task-1"))
	require.ErrorIs(t, err, errBegin)
	require.Empty(t, svc.Unresolved(), "failed BeginWait must leave no tracked waiter")
	select {
	case <-events:
		t.Fatal("a question whose wait failed must never be published")
	default:
	}
	require.Zero(t, life.count("resolve:"), "no resolution runs for an uncommitted question")
}

func TestLifecycle_ResolveFailureRestoresWaiter(t *testing.T) {
	t.Parallel()
	svc, life := newLifecycleService()
	ch, qid := askLifecycle(t, svc, askRequest("task-1"))

	life.mu.Lock()
	life.resErr = errResolve
	life.mu.Unlock()
	require.ErrorIs(t, svc.AnswerTask("owner-1", qid, answer("q", true)), errResolve)

	// Nothing committed: the waiter is back, the runner is still
	// blocked, and the question remains resolvable.
	_, ok := svc.Pending(qid)
	require.True(t, ok, "failed resolution must restore the waiter")
	select {
	case <-ch:
		t.Fatal("runner must stay blocked after a failed resolution")
	default:
	}

	life.mu.Lock()
	life.resErr = nil
	life.mu.Unlock()
	require.NoError(t, svc.AnswerTask("owner-1", qid, answer("q", true)))
	got := <-ch
	require.NoError(t, got.err)
	require.Len(t, got.answers, 1)
	require.Equal(t, 2, life.count("resolve:"), "both attempts reached the seam")
	require.Equal(t, 1, life.count("resume:"), "resume runs once on the committed answer")
}

func TestLifecycle_AnswerCommitsResolveBeforeResume(t *testing.T) {
	t.Parallel()
	svc, life := newLifecycleService()
	ch, qid := askLifecycle(t, svc, askRequest("task-1"))

	want := answer("q", true)
	require.NoError(t, svc.AnswerTask("owner-1", qid, want))
	got := <-ch
	require.NoError(t, got.err)
	require.Equal(t, want, got.answers)

	order := life.snapshot()
	require.Equal(t, []string{
		"begin:" + qid,
		"resolve:" + qid + ":" + string(ResolutionAnswered),
		"resume:" + qid,
	}, order, "wait, durable resolve, then capacity resume — in that order")
	life.mu.Lock()
	u := life.resolved[qid]
	life.mu.Unlock()
	require.Equal(t, ResolutionAnswered, u.Resolution)
	require.Equal(t, want, u.Answers)
	require.False(t, u.ResolvedAt.IsZero())
}

func TestLifecycle_ResumeFailureSurfacesToRunner(t *testing.T) {
	t.Parallel()
	svc, life := newLifecycleService()
	ch, qid := askLifecycle(t, svc, askRequest("task-1"))
	life.mu.Lock()
	life.resumeEr = errResume
	life.mu.Unlock()

	require.NoError(t, svc.AnswerTask("owner-1", qid, answer("q", true)))
	got := <-ch
	require.ErrorIs(t, got.err, errResume,
		"a task that cannot re-acquire capacity must not silently continue")
}

func TestLifecycle_RaceResolvesSeamExactlyOnce(t *testing.T) {
	t.Parallel()
	svc, life := newLifecycleService()
	ch, qid := askLifecycle(t, svc, askRequest("task-1"))

	const racers = 8
	var wg sync.WaitGroup
	errs := make([]error, racers)
	for i := range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if i%2 == 0 {
				errs[i] = svc.AnswerTask("owner-1", qid, answer("q", true))
			} else {
				errs[i] = svc.CancelTask("owner-1", qid)
			}
		}()
	}
	wg.Wait()

	var wins int
	for _, err := range errs {
		if err == nil {
			wins++
			continue
		}
		require.ErrorIs(t, err, ErrAlreadyResolved)
	}
	require.Equal(t, 1, wins)
	<-ch
	require.Equal(t, 1, life.count("resolve:"), "the durable seam runs exactly once")

	// A late control cannot re-commit the resolution.
	require.ErrorIs(t, svc.CancelTask("owner-1", qid), ErrAlreadyResolved)
	require.Equal(t, 1, life.count("resolve:"))
}

func TestLifecycle_TimeoutResolvesWithoutResume(t *testing.T) {
	t.Parallel()
	svc, life := newLifecycleService()
	req := askRequest("task-1")
	req.Timeout = 50 * time.Millisecond
	ch, qid := askLifecycle(t, svc, req)

	got := <-ch
	require.ErrorIs(t, got.err, ErrTimeout)
	require.Equal(t, []string{
		"begin:" + qid,
		"resolve:" + qid + ":" + string(ResolutionTimedOut),
	}, life.snapshot(), "timeout must not resume the task")
	require.ErrorIs(t, svc.AnswerTask("owner-1", qid, answer("q", true)), ErrAlreadyResolved)
	require.Equal(t, 1, life.count("resolve:"), "the later control cannot re-commit")
}

func TestLifecycle_ShutdownInterruptsThroughSeam(t *testing.T) {
	t.Parallel()
	svc, life := newLifecycleService()
	ch, qid := askLifecycle(t, svc, askRequest("task-1"))

	svc.Shutdown()
	got := <-ch
	require.ErrorIs(t, got.err, ErrShuttingDown)
	require.Equal(t, []string{
		"begin:" + qid,
		"resolve:" + qid + ":" + string(ResolutionInterrupted),
	}, life.snapshot())
	require.Zero(t, life.count("resume:"))
}

func TestLifecycle_ForeignOwnerNeverTouchesSeam(t *testing.T) {
	t.Parallel()
	svc, life := newLifecycleService()
	_, qid := askLifecycle(t, svc, askRequest("task-1"))

	require.ErrorIs(t, svc.AnswerTask("intruder", qid, answer("q", true)), ErrNotOwner)
	require.ErrorIs(t, svc.CancelTask("intruder", qid), ErrNotOwner)
	require.Zero(t, life.count("resolve:"), "rejected controls must not reach durable state")
	require.Equal(t, 1, life.count("begin:"))
	_, ok := svc.Pending(qid)
	require.True(t, ok)
}
