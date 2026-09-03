package task

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestManager_ShutdownInterruptsUnsettledRunners(t *testing.T) {
	t.Parallel()
	m := newTestManager(t, Limits{})
	r := record(m)
	release := make(chan struct{})

	entered := make(chan struct{})
	// The runner ignores cancellation, so Shutdown must hit its
	// deadline and force-interrupt the task.
	task, err := m.Start(t.Context(), StartRequest{
		CallerSessionID: "owner", Prompt: "work", Provider: "p", Model: "m",
		ChildSessionID: uniqueChild(),
		Run: func(context.Context, *Handle) (Result, error) {
			close(entered)
			<-release
			return Result{Text: "too late"}, nil
		},
	})
	require.NoError(t, err)
	<-entered

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, m.Shutdown(ctx), context.DeadlineExceeded)

	got, err := m.Status(t.Context(), "owner", task.ID)
	require.NoError(t, err)
	require.Equal(t, StatusInterrupted, got.Status)
	require.Equal(t, 1, r.count(EventInterrupted))

	// The late runner result cannot overwrite the interrupted record
	// or publish a second terminal event.
	close(release)
	time.Sleep(50 * time.Millisecond)
	got, err = m.Status(t.Context(), "owner", task.ID)
	require.NoError(t, err)
	require.Equal(t, StatusInterrupted, got.Status)
	require.Empty(t, got.Result)
	require.Equal(t, 1, r.count(EventInterrupted))
	require.Zero(t, r.count(EventCompleted))
	require.Zero(t, r.count(EventCancelled))
}

func TestManager_ShutdownCancelsSettledRunnersCleanly(t *testing.T) {
	t.Parallel()
	m := newTestManager(t, Limits{})
	running := make(chan struct{})
	task, err := m.Start(t.Context(), StartRequest{
		CallerSessionID: "owner", Prompt: "work", Provider: "p", Model: "m",
		ChildSessionID: uniqueChild(),
		Run: func(ctx context.Context, h *Handle) (Result, error) {
			close(running)
			<-ctx.Done()
			return Result{}, ctx.Err()
		},
	})
	require.NoError(t, err)
	<-running

	require.NoError(t, m.Shutdown(context.Background()))
	got, err := m.Status(t.Context(), "owner", task.ID)
	require.NoError(t, err)
	require.Equal(t, StatusCancelled, got.Status,
		"a runner that settles after shutdown cancellation reports cancelled")

	_, err = m.Start(t.Context(), StartRequest{
		CallerSessionID: "owner", Prompt: "new", Provider: "p", Model: "m",
		ChildSessionID: uniqueChild(), Run: nopRun(""),
	})
	require.ErrorIs(t, err, ErrShuttingDown)
}

func TestManager_ShutdownInterruptsQueuedTasks(t *testing.T) {
	t.Parallel()
	m := newTestManager(t, Limits{RunningPerModel: 1})
	release := make(chan struct{})
	defer close(release)
	running := make(chan struct{})
	block := func(ctx context.Context, h *Handle) (Result, error) {
		select {
		case <-release:
			return Result{}, nil
		case <-ctx.Done():
			return Result{}, ctx.Err()
		}
	}
	first, err := m.Start(t.Context(), StartRequest{
		CallerSessionID: "owner", Prompt: "a", Provider: "p", Model: "m",
		ChildSessionID: uniqueChild(),
		Run: func(ctx context.Context, h *Handle) (Result, error) {
			close(running)
			return block(ctx, h)
		},
	})
	require.NoError(t, err)
	<-running
	queued, err := m.Start(t.Context(), StartRequest{
		CallerSessionID: "owner", Prompt: "b", Provider: "p", Model: "m",
		ChildSessionID: uniqueChild(), Run: block,
	})
	require.NoError(t, err)
	require.Equal(t, StatusPending, queued.Status)

	require.NoError(t, m.Shutdown(context.Background()))
	got, err := m.Status(t.Context(), "owner", first.ID)
	require.NoError(t, err)
	require.Equal(t, StatusCancelled, got.Status, "settled runner reports cancelled")
	got, err = m.Status(t.Context(), "owner", queued.ID)
	require.NoError(t, err)
	require.Equal(t, StatusInterrupted, got.Status, "queued task never started; interrupted")
}
