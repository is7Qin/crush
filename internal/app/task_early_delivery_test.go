package app

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/agent/notify"
	"github.com/charmbracelet/crush/internal/agent/task"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestEarlyChildDeliveredOnParentIdleWithoutSiblingEvent locks the
// deferred-delivery contract: child A terminalizes while the parent
// is busy and child B is still running, so the terminal-event drain
// is gate-skipped and the row stays pending with nothing delivered
// and no continuation. The parent then goes idle with no further
// child event at all — B never finishes — and the deferred wake-up
// alone must deliver exactly one envelope and continue the parent
// exactly once. Without the dirty-set wake-up there is no idle edge
// that delivers here, so this fails before the fix.
func TestEarlyChildDeliveredOnParentIdleWithoutSiblingEvent(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	app := NewForTest(ctx)
	defer app.ShutdownForTest()

	gate := &idleGate{}
	gate.busy.Store(true)
	rec := &envelopeRecorder{}
	var continued atomic.Int64
	app.taskInbox = task.NewInboxDrainer(app.taskManager.Store(), gate, rec,
		func(context.Context, string) error {
			continued.Add(1)
			return nil
		})

	release := make(chan struct{})
	defer close(release)
	_, err := app.Tasks().Start(ctx, task.StartRequest{
		CallerSessionID: "parent",
		Prompt:          "first",
		Provider:        "p",
		Model:           "m",
		ChildSessionID:  uuid.NewString(),
		Run: func(context.Context, *task.Handle) (task.Result, error) {
			return task.Result{Text: "first-done"}, nil
		},
	})
	require.NoError(t, err)
	_, err = app.Tasks().Start(ctx, task.StartRequest{
		CallerSessionID: "parent",
		Prompt:          "second",
		Provider:        "p",
		Model:           "m",
		ChildSessionID:  uuid.NewString(),
		Run: func(ctx context.Context, _ *task.Handle) (task.Result, error) {
			select {
			case <-release:
				return task.Result{Text: "later"}, nil
			case <-ctx.Done():
				return task.Result{}, ctx.Err()
			}
		},
	})
	require.NoError(t, err)

	// Child A terminalized while the parent was busy: its row is
	// retained, and the terminal-event drain already attempted (and
	// dropped) it, so the assertions below cannot race that drain.
	require.Eventually(t, func() bool {
		entries, err := app.Tasks().Inbox(ctx, "parent")
		return err == nil && len(entries) == 1 && gate.calls.Load() >= 1
	}, 5*time.Second, 10*time.Millisecond,
		"busy parent must retain the early child's row after the drain attempt")
	require.Zero(t, rec.count(), "no delivery may occur while the parent is busy")
	require.Zero(t, continued.Load(), "no continuation may start while the parent is busy")

	// The parent goes idle with no further child event at all. The
	// deferred wake-up delivers the one envelope and continues the
	// parent exactly once.
	gate.busy.Store(false)
	n, err := app.taskInbox.DrainDirty(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.Equal(t, 1, rec.count())
	require.Equal(t, int64(1), continued.Load())
	entries, err := app.Tasks().Inbox(ctx, "parent")
	require.NoError(t, err)
	require.Empty(t, entries, "the delivered row must leave the pending set")

	// A second idle edge is a no-op: no duplicate delivery and no
	// duplicate continuation.
	n, err = app.taskInbox.DrainDirty(ctx)
	require.NoError(t, err)
	require.Zero(t, n)
	require.Equal(t, 1, rec.count())
	require.Equal(t, int64(1), continued.Load())

	// The app's own parent-idle edge (the RunComplete path) reaps
	// nothing further: the row is already acked, so it must not
	// deliver or continue again.
	app.runCompletions.PublishMustDeliver(ctx, pubsub.UpdatedEvent,
		notify.RunComplete{SessionID: "parent"})
	require.Never(t, func() bool {
		return rec.count() != 1 || continued.Load() != 1
	}, 500*time.Millisecond, 10*time.Millisecond,
		"a repeat idle edge must not re-deliver or re-continue")
}
