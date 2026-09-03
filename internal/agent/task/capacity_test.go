package task

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestManager_CapacityQueuesFIFOAndDispatchesOnRelease(t *testing.T) {
	t.Parallel()
	m := newTestManager(t, Limits{RunningPerModel: 1})
	release := make(chan struct{})

	block := func(ctx context.Context, h *Handle) (Result, error) {
		select {
		case <-release:
			return Result{Text: "ok"}, nil
		case <-ctx.Done():
			return Result{}, ctx.Err()
		}
	}
	req := func(prompt string) StartRequest {
		return StartRequest{
			CallerSessionID: "owner", Prompt: prompt, Provider: "p", Model: "m",
			ChildSessionID: uniqueChild(), Run: block,
		}
	}

	a, err := m.Start(t.Context(), req("a"))
	require.NoError(t, err)
	waitStatus(t, m, "owner", a.ID, StatusRunning)

	b, err := m.Start(t.Context(), req("b"))
	require.NoError(t, err)
	c, err := m.Start(t.Context(), req("c"))
	require.NoError(t, err)
	require.Equal(t, StatusPending, b.Status, "queued behind the only slot")
	require.Equal(t, StatusPending, c.Status)

	close(release)
	requireEventually(t, func() bool {
		got, err := m.Status(t.Context(), "owner", c.ID)
		return err == nil && (got.Status == StatusRunning || got.Status == StatusCompleted)
	}, "c never reached running")

	bGot := waitStatus(t, m, "owner", b.ID, StatusCompleted)
	require.Equal(t, "ok", bGot.Result, "b was first in queue and finished first")
}

func TestManager_CapacityKeysAreIndependent(t *testing.T) {
	t.Parallel()
	m := newTestManager(t, Limits{RunningPerModel: 1})
	release := make(chan struct{})
	defer close(release)
	block := func(context.Context, *Handle) (Result, error) {
		<-release
		return Result{}, nil
	}

	a, err := m.Start(t.Context(), StartRequest{
		CallerSessionID: "owner", Prompt: "a", Provider: "p1", Model: "m1",
		ChildSessionID: uniqueChild(), Run: block,
	})
	require.NoError(t, err)
	b, err := m.Start(t.Context(), StartRequest{
		CallerSessionID: "owner", Prompt: "b", Provider: "p2", Model: "m2",
		ChildSessionID: uniqueChild(), Run: block,
	})
	require.NoError(t, err)
	waitStatus(t, m, "owner", a.ID, StatusRunning)
	waitStatus(t, m, "owner", b.ID, StatusRunning)
	// different (provider, model) keys do not queue
}

func TestManager_LiveQuotaRejectsImmediately(t *testing.T) {
	t.Parallel()
	m := newTestManager(t, Limits{LiveTasksPerParent: 2})
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
	for _, prompt := range []string{"a", "b"} {
		_, err := m.Start(t.Context(), StartRequest{
			CallerSessionID: "owner", Prompt: prompt, Provider: "p", Model: "m",
			ChildSessionID: uniqueChild(), Run: block,
		})
		require.NoError(t, err)
	}
	_, err := m.Start(t.Context(), StartRequest{
		CallerSessionID: "owner", Prompt: "c", Provider: "p", Model: "m",
		ChildSessionID: uniqueChild(), Run: block,
	})
	require.ErrorIs(t, err, ErrQuota)

	// A different parent has its own quota.
	_, err = m.Start(t.Context(), StartRequest{
		CallerSessionID: "other", Prompt: "c", Provider: "p", Model: "m",
		ChildSessionID: uniqueChild(), Run: block,
	})
	require.NoError(t, err)
}

func TestManager_WorkspaceQuotaRejects(t *testing.T) {
	t.Parallel()
	m := newTestManager(t, Limits{LiveTasksPerWorkspace: 1})
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
	_, err := m.Start(t.Context(), StartRequest{
		CallerSessionID: "owner", Prompt: "a", Provider: "p", Model: "m",
		ChildSessionID: uniqueChild(), Run: block,
	})
	require.NoError(t, err)
	_, err = m.Start(t.Context(), StartRequest{
		CallerSessionID: "other", Prompt: "b", Provider: "p", Model: "m",
		ChildSessionID: uniqueChild(), Run: block,
	})
	require.ErrorIs(t, err, ErrQuota)
}

func TestManager_WaitingForInputReleasesSlotAndResumeReacquires(t *testing.T) {
	t.Parallel()
	m := newTestManager(t, Limits{RunningPerModel: 1})
	r := record(m)

	asked := make(chan struct{})
	resume := make(chan struct{})
	var waitErr, resumeErr error
	a, err := m.Start(t.Context(), StartRequest{
		CallerSessionID: "owner", Prompt: "a", Provider: "p", Model: "m",
		ChildSessionID: uniqueChild(),
		Run: func(ctx context.Context, h *Handle) (Result, error) {
			waitErr = h.WaitingForInput(ctx)
			close(asked)
			select {
			case <-resume:
			case <-ctx.Done():
				return Result{}, ctx.Err()
			}
			resumeErr = h.Resumed(ctx)
			return Result{Text: "after answer"}, nil
		},
	})
	require.NoError(t, err)
	<-asked
	require.NoError(t, waitErr)

	got := waitStatus(t, m, "owner", a.ID, StatusWaitingForInput, StatusRunning, StatusCompleted)
	require.Equal(t, StatusWaitingForInput, got.Status, "waiting task is live, not running")

	// The released slot lets a queued sibling run while a waits.
	b, err := m.Start(t.Context(), StartRequest{
		CallerSessionID: "owner", Prompt: "b", Provider: "p", Model: "m",
		ChildSessionID: uniqueChild(),
		Run: func(context.Context, *Handle) (Result, error) {
			return Result{Text: "b done"}, nil
		},
	})
	require.NoError(t, err)
	waitStatus(t, m, "owner", b.ID, StatusCompleted)

	// a's Resumed reacquires the now-free slot.
	close(resume)
	gotA := waitStatus(t, m, "owner", a.ID, StatusCompleted)
	require.Equal(t, "after answer", gotA.Result)
	require.NoError(t, resumeErr)

	require.Eventually(t, func() bool {
		return r.count(EventWaitingForInput) == 1 && r.count(EventResumed) == 1
	}, 5*time.Second, 5*time.Millisecond)
	var resumedStatus Status
	for _, e := range r.taskEvents(a.ID) {
		if e.Type == EventResumed {
			resumedStatus = e.Task.Status
		}
	}
	require.Equal(t, StatusRunning, resumedStatus)
}

func TestManager_HandleTransitionEdges(t *testing.T) {
	t.Parallel()
	m := newTestManager(t, Limits{})
	var firstWaitErr, secondWaitErr, resumeErr, doubleResumeErr error
	task, err := m.Start(t.Context(), StartRequest{
		CallerSessionID: "owner", Prompt: "a", Provider: "p", Model: "m",
		ChildSessionID: uniqueChild(),
		Run: func(ctx context.Context, h *Handle) (Result, error) {
			firstWaitErr = h.WaitingForInput(ctx)
			// waiting -> waiting is invalid.
			secondWaitErr = h.WaitingForInput(ctx)
			resumeErr = h.Resumed(ctx)
			// Resumed from running is an idempotent no-op.
			doubleResumeErr = h.Resumed(ctx)
			return Result{Text: "ok"}, nil
		},
	})
	require.NoError(t, err)
	waitStatus(t, m, "owner", task.ID, StatusCompleted)
	require.NoError(t, firstWaitErr)
	require.ErrorIs(t, secondWaitErr, ErrInvalidTransition)
	require.NoError(t, resumeErr)
	require.NoError(t, doubleResumeErr)
}
