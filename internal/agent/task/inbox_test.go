package task

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/stretchr/testify/require"
)

// fakeGate answers ParentReady from an atomic flag.
type fakeGate struct {
	ready atomic.Bool
	calls atomic.Int64
}

func (g *fakeGate) ParentReady(context.Context, string) (bool, error) {
	g.calls.Add(1)
	return g.ready.Load(), nil
}

// fakeWriter records delivered envelopes; fail rejects every write
// while counting attempts.
type fakeWriter struct {
	mu       sync.Mutex
	written  []TaskResultEnvelope
	inFlight int
	maxSeen  int
	fail     atomic.Bool
}

func (w *fakeWriter) WriteResult(_ context.Context, _ string, env TaskResultEnvelope) error {
	w.mu.Lock()
	w.inFlight++
	if w.inFlight > w.maxSeen {
		w.maxSeen = w.inFlight
	}
	w.mu.Unlock()

	if w.fail.Load() {
		w.mu.Lock()
		w.inFlight--
		w.mu.Unlock()
		return errors.New("write failed")
	}
	time.Sleep(2 * time.Millisecond) // widen the overlap window
	w.mu.Lock()
	w.written = append(w.written, env)
	w.inFlight--
	w.mu.Unlock()
	return nil
}

func (w *fakeWriter) snapshot() []TaskResultEnvelope {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]TaskResultEnvelope{}, w.written...)
}

func (w *fakeWriter) maxConcurrency() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.maxSeen
}

// terminalInbox seeds s with one terminalized task and returns its
// undelivered inbox row.
func terminalInbox(t *testing.T, s *MemoryStore, id, child, owner string) *InboxEntry {
	t.Helper()
	ctx := t.Context()
	_, err := s.CreatePendingTask(ctx, admissionForWith(id, child, owner))
	require.NoError(t, err)
	_, found, err := s.DispatchNextChildMessage(ctx, child)
	require.NoError(t, err)
	require.True(t, found)
	_, won, err := s.TerminalizeAndDeliver(ctx, id, 1, TerminalUpdate{
		Status: StatusCompleted, Result: "answer", CompletedAt: time.Now(),
	})
	require.NoError(t, err)
	require.True(t, won)
	entries, err := s.ListInbox(ctx, owner)
	require.NoError(t, err)
	for _, e := range entries {
		if e.TaskID == id {
			return e
		}
	}
	t.Fatalf("no inbox row for %s", id)
	return nil
}

// admissionForWith is admissionFor with a parameterized owner.
func admissionForWith(id, child, owner string) Admission {
	a := admissionFor(id, child, "initial")
	a.Task.OwnerSessionID = owner
	a.Task.ParentSessionID = owner
	a.Child.ParentID = owner
	return a
}

func TestInboxDrainer_DeliversAndAcksAfterCommit(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := NewMemoryStore()
	terminalInbox(t, s, "t1", "child-1", "owner")
	gate := &fakeGate{}
	gate.ready.Store(true)
	writer := &fakeWriter{}
	d := NewInboxDrainer(s, gate, writer)

	n, err := d.Drain(ctx, "owner")
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.Len(t, writer.snapshot(), 1)
	require.Equal(t, "t1", writer.snapshot()[0].TaskID)

	pending, err := s.ListInbox(ctx, "owner")
	require.NoError(t, err)
	require.Empty(t, pending, "acked only after the message commit")

	// A second drain finds nothing pending and writes nothing.
	n, err = d.Drain(ctx, "owner")
	require.NoError(t, err)
	require.Zero(t, n)
	require.Len(t, writer.snapshot(), 1)
}

func TestInboxDrainer_ContinuesParentAfterAck(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := NewMemoryStore()
	terminalInbox(t, s, "t1", "child-1", "owner")
	gate := &fakeGate{}
	gate.ready.Store(true)
	writer := &fakeWriter{}
	var continued atomic.Int64

	d := NewInboxDrainer(s, gate, writer, func(context.Context, string) error {
		continued.Add(1)
		return nil
	})
	n, err := d.Drain(ctx, "owner")
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.Equal(t, int64(1), continued.Load())
	require.Empty(t, func() []*InboxEntry {
		entries, _ := s.ListInbox(ctx, "owner")
		return entries
	}())
}

func TestInboxDrainer_BusyParentKeepsRowsPending(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := NewMemoryStore()
	terminalInbox(t, s, "t1", "child-1", "owner")
	gate := &fakeGate{} // not ready: busy or deleted
	writer := &fakeWriter{}

	n, err := NewInboxDrainer(s, gate, writer).Drain(ctx, "owner")
	require.NoError(t, err)
	require.Zero(t, n)
	require.Empty(t, writer.snapshot())

	pending, err := s.ListInbox(ctx, "owner")
	require.NoError(t, err)
	require.Len(t, pending, 1, "a busy or deleted parent keeps its rows retained")

	// Once the parent is idle again the retained row is delivered.
	gate.ready.Store(true)
	n, err = NewInboxDrainer(s, gate, writer).Drain(ctx, "owner")
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.Len(t, writer.snapshot(), 1)
}

func TestInboxDrainer_WriteFailureLeavesRowUndelivered(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := NewMemoryStore()
	terminalInbox(t, s, "t1", "child-1", "owner")
	gate := &fakeGate{}
	gate.ready.Store(true)
	writer := &fakeWriter{}
	writer.fail.Store(true)

	_, err := NewInboxDrainer(s, gate, writer).Drain(ctx, "owner")
	require.Error(t, err)
	pending, err := s.ListInbox(ctx, "owner")
	require.NoError(t, err)
	require.Len(t, pending, 1, "delivered_at is stamped only after a successful commit")
	require.True(t, pending[0].DeliveredAt.IsZero())

	// Recovery: the retained row is delivered exactly once later.
	writer.fail.Store(false)
	n, err := NewInboxDrainer(s, gate, writer).Drain(ctx, "owner")
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.Len(t, writer.snapshot(), 1)
}

func TestInboxDrainer_PerParentSerialization(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := NewMemoryStore()
	for i, child := range []string{"child-1", "child-2", "child-3"} {
		terminalInbox(t, s, string(rune('t'+i)), child, "owner")
	}
	gate := &fakeGate{}
	gate.ready.Store(true)
	writer := &fakeWriter{}
	d := NewInboxDrainer(s, gate, writer)

	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			_, err := d.Drain(ctx, "owner")
			require.NoError(t, err)
		})
	}
	wg.Wait()

	// Drain reloads pending rows under the parent lock, so four
	// concurrent drains deliver the three rows once each and never
	// write concurrently for one parent.
	require.Len(t, writer.snapshot(), 3)
	require.Equal(t, 1, writer.maxConcurrency(), "drain is serialized per parent")
	pending, err := s.ListInbox(ctx, "owner")
	require.NoError(t, err)
	require.Empty(t, pending)
}

func TestInboxDrainer_AllOwnersDrainEachParent(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := NewMemoryStore()
	terminalInbox(t, s, "t1", "child-1", "owner-a")
	terminalInbox(t, s, "t2", "child-2", "owner-b")
	gate := &fakeGate{}
	gate.ready.Store(true)
	writer := &fakeWriter{}

	n, err := NewInboxDrainer(s, gate, writer).Drain(ctx, "")
	require.NoError(t, err)
	require.Equal(t, 2, n)
	seen := map[string]bool{}
	for _, env := range writer.snapshot() {
		seen[env.TaskID] = true
	}
	require.True(t, seen["t1"] && seen["t2"])
}

// TestDroppedBrokerResync proves the completion report survives total
// live-event loss: with no subscriber attached at terminalization,
// the durable outbox and inbox rows replay through the notifier and
// the drainer, and the task result stays queryable.
func TestDroppedBrokerResync(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	broker := pubsub.NewBroker[Event]()
	t.Cleanup(broker.Shutdown)
	// No subscriber ever attached: every live publish is dropped.

	m := newTestManager(t, Limits{})
	task := completeTask(t, m, "owner", "final answer")

	// The task snapshot is resyncable by owner.
	got, err := m.Status(ctx, "owner", task.ID)
	require.NoError(t, err)
	require.Equal(t, StatusCompleted, got.Status)

	// A subscriber that appears after the loss recovers the terminal
	// event through the durable outbox replay.
	ch := broker.Subscribe(ctx)
	require.NoError(t, NewOutboxNotifier(m, broker).Drain(ctx, "owner"))

	// And the parent's completion report is resyncable through the
	// durable inbox: the late-joining parent receives the result
	// envelope and the row is acked only after the write commits.
	gate := &fakeGate{}
	gate.ready.Store(true)
	writer := &fakeWriter{}
	n, err := NewInboxDrainer(m.store, gate, writer).Drain(ctx, "owner")
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.Equal(t, "final answer", writer.snapshot()[0].Result)

	ev := waitForEvent(t, ch, func(e Event) bool {
		return e.Type == EventCompleted && e.Task.ID == task.ID
	})
	require.Equal(t, "final answer", ev.Task.Result)
}

// TestManager_ShutdownCommitsTerminalDeliveryBeforeReturning pins
// the shutdown ordering the spec requires: when Shutdown returns,
// every unsettled run's interrupted record is already durable in the
// outbox and parent inbox, before message flush and DB close.
func TestManager_ShutdownCommitsTerminalDeliveryBeforeReturning(t *testing.T) {
	t.Parallel()
	m, s := newTestManagerStore(t, Limits{})
	entered := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
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

	// No polling: the delivery committed synchronously inside
	// Shutdown.
	got, err := m.Status(t.Context(), "owner", task.ID)
	require.NoError(t, err)
	require.Equal(t, StatusInterrupted, got.Status)
	in, err := s.ListInbox(t.Context(), "owner")
	require.NoError(t, err)
	require.Len(t, in, 1, "terminal delivery is durable when Shutdown returns")
	env, err := in[0].Envelope()
	require.NoError(t, err)
	require.Equal(t, StatusInterrupted, env.Status)
	out, err := s.ListOutbox(t.Context())
	require.NoError(t, err)
	require.NotNil(t, terminalEntry(out, task.ID))
}
