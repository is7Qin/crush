package task

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestInboxDrainer_DirtyCoalescesIntoOneBatchOnIdle pins the
// coalescing invariant across a deferred delivery: several reports
// pending while the parent is busy deliver as exactly one batched
// message with exactly one continuation on the next idle drain.
func TestInboxDrainer_DirtyCoalescesIntoOneBatchOnIdle(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := NewMemoryStore()
	for i, child := range []string{"child-1", "child-2", "child-3"} {
		terminalInbox(t, s, string(rune('t'+i)), child, "owner")
	}
	gate := &fakeGate{} // busy: not ready.
	writer := &fakeWriter{}
	var continued atomic.Int64
	d := NewInboxDrainer(s, gate, writer, func(context.Context, string) error {
		continued.Add(1)
		return nil
	})

	n, err := d.Drain(ctx, "owner")
	require.NoError(t, err)
	require.Zero(t, n, "a busy parent keeps every row pending")
	require.Zero(t, writer.batches)
	require.Zero(t, continued.Load())

	gate.ready.Store(true)
	n, err = d.DrainDirty(ctx)
	require.NoError(t, err)
	require.Equal(t, 3, n)
	require.Equal(t, 1, writer.batches, "one idle drain is ONE message")
	require.Len(t, writer.snapshot(), 3)
	require.Equal(t, int64(1), continued.Load(), "one batch is one continuation")

	pending, err := s.ListInbox(ctx, "owner")
	require.NoError(t, err)
	require.Empty(t, pending)

	// A second idle edge delivers nothing and continues nothing.
	n, err = d.DrainDirty(ctx)
	require.NoError(t, err)
	require.Zero(t, n)
	require.Equal(t, int64(1), continued.Load())
}

// TestInboxDrainer_IdleRaceAgainstTerminalDrainContinuesOnce pins
// the no-duplicate-continuation invariant: an idle edge racing a
// second child's terminal drain delivers every row exactly once
// and continues the parent exactly once, whichever drain wins the
// per-owner lock. The first continuation starts the parent's next
// turn, so the gate reports busy from there on.
func TestInboxDrainer_IdleRaceAgainstTerminalDrainContinuesOnce(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := NewMemoryStore()
	terminalInbox(t, s, "t1", "child-1", "owner")
	terminalInbox(t, s, "t2", "child-2", "owner")
	gate := &fakeGate{}
	writer := &fakeWriter{}
	var continued atomic.Int64
	d := NewInboxDrainer(s, gate, writer, func(context.Context, string) error {
		continued.Add(1)
		gate.ready.Store(false)
		return nil
	})

	// Seed the deferred delivery: drain while busy drops and marks.
	n, err := d.Drain(ctx, "owner")
	require.NoError(t, err)
	require.Zero(t, n)

	gate.ready.Store(true)
	var wg sync.WaitGroup
	got := make([]int, 2)
	wg.Go(func() {
		n, err := d.DrainIdle(ctx, "owner")
		require.NoError(t, err)
		got[0] = n
	})
	wg.Go(func() {
		n, err := d.Drain(ctx, "owner")
		require.NoError(t, err)
		got[1] = n
	})
	wg.Wait()

	require.Equal(t, 2, got[0]+got[1], "every row is delivered exactly once")
	require.Equal(t, int64(1), continued.Load(), "one delivered batch is one continuation")
	envs := writer.snapshot()
	require.Len(t, envs, 2)
	require.NotEqual(t, envs[0].TaskID, envs[1].TaskID)

	pending, err := s.ListInbox(ctx, "owner")
	require.NoError(t, err)
	require.Empty(t, pending)
}
