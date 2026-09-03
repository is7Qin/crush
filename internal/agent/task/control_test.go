package task

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestManager_ControlOperationsRequireOwnership(t *testing.T) {
	t.Parallel()
	m := newTestManager(t, Limits{})
	task, err := m.Start(t.Context(), StartRequest{
		CallerSessionID: "owner", Prompt: "work", Provider: "p", Model: "m",
		ChildSessionID: uniqueChild(), Run: nopRun("r"),
	})
	require.NoError(t, err)
	waitStatus(t, m, "owner", task.ID, StatusCompleted)

	_, err = m.Status(t.Context(), "intruder", task.ID)
	require.ErrorIs(t, err, ErrNotOwner)
	_, _, err = m.Output(t.Context(), "intruder", task.ID)
	require.ErrorIs(t, err, ErrNotOwner)
	require.ErrorIs(t, m.Cancel(t.Context(), "intruder", task.ID), ErrNotOwner)

	_, err = m.Status(t.Context(), "owner", "unknown")
	require.ErrorIs(t, err, ErrNotFound)
	require.ErrorIs(t, m.Cancel(t.Context(), "owner", "unknown"), ErrNotFound)

	// Leaked ids cannot list another parent's tasks.
	_, err = m.List(t.Context(), "intruder", "owner")
	require.ErrorIs(t, err, ErrNotOwner)
	mine, err := m.List(t.Context(), "owner", "")
	require.NoError(t, err)
	require.Len(t, mine, 1)
}

func TestManager_CancelPendingNeverStartsRunner(t *testing.T) {
	t.Parallel()
	m, mem := newTestManagerStore(t, Limits{RunningPerModel: 1})
	r := record(m)
	release := make(chan struct{})
	defer close(release)
	ran := make(chan string, 4)
	block := func(ctx context.Context, h *Handle) (Result, error) {
		ran <- h.TaskID()
		select {
		case <-release:
			return Result{}, nil
		case <-ctx.Done():
			return Result{}, ctx.Err()
		}
	}
	a, err := m.Start(t.Context(), StartRequest{
		CallerSessionID: "owner", Prompt: "a", Provider: "p", Model: "m",
		ChildSessionID: uniqueChild(), Run: block,
	})
	require.NoError(t, err)
	require.Equal(t, a.ID, <-ran, "a owns the only slot")

	bChild := uniqueChild()
	b, err := m.Start(t.Context(), StartRequest{
		CallerSessionID: "owner", Prompt: "b", Provider: "p", Model: "m",
		ChildSessionID: bChild, Run: block,
	})
	require.NoError(t, err)
	require.Equal(t, StatusPending, b.Status)

	require.NoError(t, m.Cancel(t.Context(), "owner", b.ID))
	got := waitStatus(t, m, "owner", b.ID, StatusCancelled)
	require.Equal(t, StatusCancelled, got.Status)
	for _, e := range r.taskEvents(b.ID) {
		require.NotEqual(t, EventStarted, e.Type, "cancelled pending task must never start")
	}

	// The cancellation rejected b's undelivered admission message.
	msgs, err := mem.ListChildMessages(t.Context(), bChild)
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	require.Equal(t, MessageRejected, msgs[0].State)
	require.Equal(t, ReasonTaskCancelled, msgs[0].Reason)

	// Cancelling the running task frees its slot for the next queued
	// task once the runner settles.
	require.NoError(t, m.Cancel(t.Context(), "owner", a.ID))
	got = waitStatus(t, m, "owner", a.ID, StatusCancelled)
	require.Equal(t, StatusCancelled, got.Status)
}

func TestManager_CancelRunningWaitsForRunnerToSettle(t *testing.T) {
	t.Parallel()
	m := newTestManager(t, Limits{})
	observed := make(chan struct{})
	settled := make(chan struct{})
	task, err := m.Start(t.Context(), StartRequest{
		CallerSessionID: "owner", Prompt: "work", Provider: "p", Model: "m",
		ChildSessionID: uniqueChild(),
		Run: func(ctx context.Context, h *Handle) (Result, error) {
			<-ctx.Done()
			close(observed)
			// Simulate slow cleanup after cancellation.
			time.Sleep(50 * time.Millisecond)
			return Result{}, ctx.Err()
		},
	})
	require.NoError(t, err)
	waitStatus(t, m, "owner", task.ID, StatusRunning)

	go func() {
		defer close(settled)
		require.NoError(t, m.Cancel(t.Context(), "owner", task.ID))
	}()
	<-observed
	select {
	case <-settled:
		t.Fatal("Cancel returned before the runner settled")
	case <-time.After(20 * time.Millisecond):
	}
	<-settled

	got := waitStatus(t, m, "owner", task.ID, StatusCancelled)
	require.Equal(t, StatusCancelled, got.Status)
	require.NoError(t, m.Cancel(t.Context(), "owner", task.ID), "cancel is idempotent")
}

func TestManager_ContinuationAdmitsFreshAttemptOnRetainedChild(t *testing.T) {
	t.Parallel()
	m := newTestManager(t, Limits{})
	old, err := m.Start(t.Context(), StartRequest{
		CallerSessionID: "owner", Prompt: "first", Provider: "p", Model: "m",
		ChildSessionID: "child-9", Run: nopRun("done"),
	})
	require.NoError(t, err)
	waitStatus(t, m, "owner", old.ID, StatusCompleted)

	fresh, err := m.Start(t.Context(), StartRequest{
		CallerSessionID: "owner", Prompt: "continue", Provider: "p", Model: "m",
		ResumesTaskID: old.ID, Run: nopRun("resumed"),
	})
	require.NoError(t, err)
	require.NotEqual(t, old.ID, fresh.ID, "continuation uses a new task id")
	require.Equal(t, old.ID, fresh.ResumesTaskID)
	require.Equal(t, "child-9", fresh.ChildSessionID, "continuation reuses the retained child session")
	require.Equal(t, uint64(2), fresh.RunGeneration, "continuation increments the run generation")
	got := waitStatus(t, m, "owner", fresh.ID, StatusCompleted)
	require.Equal(t, "resumed", got.Result)

	// The original terminal attempt is immutable.
	stillOld, err := m.Status(t.Context(), "owner", old.ID)
	require.NoError(t, err)
	require.Equal(t, "done", stillOld.Result)
	require.Equal(t, StatusCompleted, stillOld.Status)
}

func TestManager_ContinuationRejections(t *testing.T) {
	t.Parallel()
	m := newTestManager(t, Limits{})
	release := make(chan struct{})
	defer close(release)
	live, err := m.Start(t.Context(), StartRequest{
		CallerSessionID: "owner", Prompt: "a", Provider: "p", Model: "m",
		ChildSessionID: uniqueChild(),
		Run: func(ctx context.Context, h *Handle) (Result, error) {
			select {
			case <-release:
				return Result{}, nil
			case <-ctx.Done():
				return Result{}, ctx.Err()
			}
		},
	})
	require.NoError(t, err)
	waitStatus(t, m, "owner", live.ID, StatusRunning)

	_, err = m.Start(t.Context(), StartRequest{
		CallerSessionID: "owner", Prompt: "x", Provider: "p", Model: "m",
		ResumesTaskID: live.ID, Run: nopRun(""),
	})
	require.ErrorIs(t, err, ErrResumeLive)

	_, err = m.Start(t.Context(), StartRequest{
		CallerSessionID: "owner", Prompt: "x", Provider: "p", Model: "m",
		ResumesTaskID: "nope", Run: nopRun(""),
	})
	require.ErrorIs(t, err, ErrNotFound)

	require.NoError(t, m.Cancel(t.Context(), "owner", live.ID))
	_, err = m.Start(t.Context(), StartRequest{
		CallerSessionID: "intruder", Prompt: "x", Provider: "p", Model: "m",
		ResumesTaskID: live.ID, Run: nopRun(""),
	})
	require.ErrorIs(t, err, ErrNotOwner)
}
