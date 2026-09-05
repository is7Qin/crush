package task

import (
	"context"
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLimits_ApplyDefaults(t *testing.T) {
	t.Parallel()

	var l Limits
	l.applyDefaults()
	require.Equal(t, math.MaxInt, l.LiveTasksPerParent, "zero live quota is unlimited")
	require.Equal(t, math.MaxInt, l.LiveTasksPerWorkspace, "zero live quota is unlimited")
	require.Equal(t, DefaultRunningPerModel, l.RunningPerModel, "model capacity stays bounded")

	l2 := Limits{LiveTasksPerParent: 3, LiveTasksPerWorkspace: -1, RunningPerModel: 2}
	l2.applyDefaults()
	require.Equal(t, 3, l2.LiveTasksPerParent, "positive live quota is kept")
	require.Equal(t, math.MaxInt, l2.LiveTasksPerWorkspace, "negative live quota is unlimited")
	require.Equal(t, 2, l2.RunningPerModel, "positive model capacity is kept")
}

func TestManager_DefaultLimitsUnboundedLiveBoundedModel(t *testing.T) {
	t.Parallel()
	m := newTestManager(t, Limits{})
	release := make(chan struct{})
	block := func(ctx context.Context, h *Handle) (Result, error) {
		select {
		case <-release:
			return Result{Text: "ok"}, nil
		case <-ctx.Done():
			return Result{}, ctx.Err()
		}
	}
	start := func() string {
		tsk, err := m.Start(t.Context(), StartRequest{
			CallerSessionID: "owner", Prompt: "x", Provider: "p", Model: "m",
			ChildSessionID: uniqueChild(), Run: block,
		})
		require.NoError(t, err, "default live quota is unlimited")
		return tsk.ID
	}

	ids := make([]string, 0, DefaultRunningPerModel+1)
	for range DefaultRunningPerModel {
		ids = append(ids, start())
	}
	for _, id := range ids {
		waitStatus(t, m, "owner", id, StatusRunning)
	}
	// One task past the model capacity is admitted, not quota-rejected,
	// and stays pending while all slots are occupied.
	extra := start()
	got, err := m.Status(t.Context(), "owner", extra)
	require.NoError(t, err)
	require.Equal(t, StatusPending, got.Status, "model capacity queues the 11th task")

	// Releasing the running set drains the queue: the extra task runs.
	close(release)
	waitStatus(t, m, "owner", extra, StatusCompleted)
}
