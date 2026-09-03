package task

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/charmbracelet/crush/internal/pubsub"
)

func TestMemoryStore_OutboxTerminalRowsIdempotentAndAck(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := NewMemoryStore()
	base := time.Unix(1_700_000_000, 0).UTC()
	mk := func(id string) {
		require.NoError(t, s.Save(ctx, &Task{
			ID: id, OwnerSessionID: "o", Status: StatusRunning, RunGeneration: 1,
		}))
	}
	mk("t1")
	mk("t2")

	_, won, err := s.TerminalizeAndDeliver(ctx, "t1", 1,
		TerminalUpdate{Status: StatusCompleted, CompletedAt: base})
	require.NoError(t, err)
	require.True(t, won)
	// A replay of the same terminalization loses and cannot add a
	// second row for the transition fact.
	_, won, err = s.TerminalizeAndDeliver(ctx, "t1", 1,
		TerminalUpdate{Status: StatusCompleted, CompletedAt: base})
	require.NoError(t, err)
	require.False(t, won)
	_, won, err = s.TerminalizeAndDeliver(ctx, "t2", 1,
		TerminalUpdate{Status: StatusInterrupted, CompletedAt: base.Add(time.Second)})
	require.NoError(t, err)
	require.True(t, won)

	got, err := s.ListOutbox(ctx)
	require.NoError(t, err)
	require.Len(t, got, 2, "duplicate terminalization must not add a row")
	require.Equal(t, "t1", got[0].TaskID, "oldest first")
	require.Equal(t, "t2", got[1].TaskID)
	require.Equal(t, EventCompleted, got[0].EventType)

	require.NoError(t, s.AckOutbox(ctx, []string{got[0].ID, "unknown"}, base.Add(time.Minute)))
	got, err = s.ListOutbox(ctx)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "t2", got[0].TaskID)
}

func TestMemoryStore_InboxRowsIdempotentAndAck(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := NewMemoryStore()
	base := time.Unix(1_700_000_000, 0).UTC()
	require.NoError(t, s.Save(ctx, &Task{
		ID: "t1", OwnerSessionID: "o", Status: StatusRunning, RunGeneration: 1,
	}))
	require.NoError(t, s.Save(ctx, &Task{
		ID: "t2", OwnerSessionID: "x", Status: StatusRunning, RunGeneration: 1,
	}))

	_, _, err := s.TerminalizeAndDeliver(ctx, "t1", 1,
		TerminalUpdate{Status: StatusCompleted, CompletedAt: base})
	require.NoError(t, err)
	// The losing replay adds no inbox row.
	_, won, err := s.TerminalizeAndDeliver(ctx, "t1", 1,
		TerminalUpdate{Status: StatusCompleted, CompletedAt: base})
	require.NoError(t, err)
	require.False(t, won)
	_, _, err = s.TerminalizeAndDeliver(ctx, "t2", 1,
		TerminalUpdate{Status: StatusCompleted, CompletedAt: base.Add(time.Second)})
	require.NoError(t, err)

	mine, err := s.ListInbox(ctx, "o")
	require.NoError(t, err)
	require.Len(t, mine, 1, "exactly one inbox row per terminalization")
	require.Equal(t, "t1", mine[0].TaskID)
	require.Equal(t, uint64(1), mine[0].TerminalGeneration)
	env, err := mine[0].Envelope()
	require.NoError(t, err)
	require.Equal(t, StatusCompleted, env.Status)

	// An unrelated owner's rows never appear in an owner-scoped list,
	// and an empty owner lists everything.
	all, err := s.ListInbox(ctx, "")
	require.NoError(t, err)
	require.Len(t, all, 2)
	require.NoError(t, s.AckInbox(ctx, []string{mine[0].ID}, base.Add(time.Minute)))
	mine, err = s.ListInbox(ctx, "o")
	require.NoError(t, err)
	require.Empty(t, mine, "acked rows leave the pending set")
}

func completeTask(t *testing.T, m *Manager, owner, result string) *Task {
	t.Helper()
	task, err := m.Start(t.Context(), StartRequest{
		CallerSessionID: owner, Prompt: "work", Provider: "p", Model: "m",
		ChildSessionID: uniqueChild(), Run: nopRun(result),
	})
	require.NoError(t, err)
	return waitStatus(t, m, owner, task.ID, StatusCompleted)
}

func terminalEntry(entries []*OutboxEntry, taskID string) *OutboxEntry {
	for _, e := range entries {
		if e.TaskID == taskID && e.EventType != EventStarted {
			return e
		}
	}
	return nil
}

func startedEntry(entries []*OutboxEntry, taskID string) *OutboxEntry {
	for _, e := range entries {
		if e.TaskID == taskID && e.EventType == EventStarted {
			return e
		}
	}
	return nil
}

func TestManager_TerminalTransitionWritesOutboxOnce(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	m := newTestManager(t, Limits{})

	task := completeTask(t, m, "owner", "done")

	// The terminal transaction commits the outbox row with the task
	// update, so a terminal status observed through the manager
	// already implies the durable records are in place.
	got, err := m.Outbox(ctx)
	require.NoError(t, err)
	entries := undeliveredFor(got, task.ID)
	require.Len(t, entries, 2, "one started row from dispatch plus one durable terminal record")
	require.NotNil(t, startedEntry(entries, task.ID))
	term := terminalEntry(entries, task.ID)
	require.NotNil(t, term, "exactly one durable terminal record")
	require.Equal(t, EventCompleted, term.EventType)
	require.Equal(t, task.RunGeneration, term.RunGeneration)
	require.True(t, term.DeliveredAt.IsZero(), "fresh entries are undelivered")

	snap, err := term.Task()
	require.NoError(t, err)
	require.Equal(t, StatusCompleted, snap.Status)
	require.Equal(t, "done", snap.Result)

	// Idempotent post-terminal operations cannot add rows.
	require.NoError(t, m.Cancel(ctx, "owner", task.ID))
	require.NoError(t, m.Cancel(ctx, "owner", task.ID))
	got, err = m.Outbox(ctx)
	require.NoError(t, err)
	require.Len(t, undeliveredFor(got, task.ID), 2)
}

func undeliveredFor(entries []*OutboxEntry, taskID string) []*OutboxEntry {
	var out []*OutboxEntry
	for _, e := range entries {
		if e.TaskID == taskID {
			out = append(out, e)
		}
	}
	return out
}

func TestManager_FailedTerminalWritesFailedEntry(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	m := newTestManager(t, Limits{})

	task, err := m.Start(ctx, StartRequest{
		CallerSessionID: "owner", Prompt: "work", Provider: "p", Model: "m",
		ChildSessionID: uniqueChild(),
		Run: func(context.Context, *Handle) (Result, error) {
			return Result{}, errors.New("boom")
		},
	})
	require.NoError(t, err)
	got := waitStatus(t, m, "owner", task.ID, StatusFailed)
	require.Contains(t, got.Err, "boom")

	var entries []*OutboxEntry
	require.Eventually(t, func() bool {
		var err error
		entries, err = m.Outbox(ctx)
		return err == nil && terminalEntry(entries, task.ID) != nil
	}, 5*time.Second, 5*time.Millisecond)
	require.Equal(t, EventFailed, terminalEntry(entries, task.ID).EventType)
}

func TestOutboxNotifier_DrainPublishesAndAcksOwnerEntries(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	broker := pubsub.NewBroker[Event]()
	t.Cleanup(broker.Shutdown)
	ch := broker.Subscribe(ctx)

	m := newTestManager(t, Limits{})
	mine := completeTask(t, m, "owner", "answer")
	theirs := completeTask(t, m, "other", "elsewhere")
	n := NewOutboxNotifier(m, broker)

	require.NoError(t, n.Drain(ctx, "owner"))

	// Drain order within one task is timestamp-tied, so accumulate
	// the owner's replayed facts by identity rather than arrival
	// order.
	seen := make(map[EventType]int, 2)
	var result string
	for len(seen) < 2 {
		e := waitForEvent(t, ch, func(ev Event) bool { return ev.Task.ID == mine.ID })
		seen[e.Type]++
		if e.Type == EventCompleted {
			result = e.Task.Result
		}
	}
	require.Equal(t, 1, seen[EventCompleted])
	require.Equal(t, 1, seen[EventStarted])
	require.Equal(t, "answer", result)

	// The other owner's records stay pending until they are drained.
	got, err := m.Outbox(ctx)
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Equal(t, theirs.ID, got[0].TaskID)

	// An empty owner replays everything, then the outbox is empty.
	require.NoError(t, n.Drain(ctx, ""))
	waitForEvent(t, ch, func(e Event) bool {
		return e.Type == EventCompleted && e.Task.ID == theirs.ID
	})
	got, err = m.Outbox(ctx)
	require.NoError(t, err)
	require.Empty(t, got)
}

// waitForEvent drains delivered events until one matches, failing on
// timeout.
func waitForEvent(t *testing.T, ch <-chan pubsub.Event[Event], match func(Event) bool) Event {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case delivered := <-ch:
			if match(delivered.Payload) {
				return delivered.Payload
			}
		case <-deadline:
			t.Fatal("matching outbox event never arrived")
			return Event{}
		}
	}
}

// TestOutboxNotifier_DrainExceptRetainsTriggeringEntry proves the
// terminal-event drain shape: the owner's superseded rows are
// republished and acked, while the entry recording the event the
// caller just live-published stays pending for a resync consumer
// whose stream dropped it.
func TestOutboxNotifier_DrainExceptRetainsTriggeringEntry(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	broker := pubsub.NewBroker[Event]()
	t.Cleanup(broker.Shutdown)
	ch := broker.Subscribe(ctx)

	m := newTestManager(t, Limits{})
	mine := completeTask(t, m, "owner", "answer")
	require.NoError(t, NewOutboxNotifier(m, broker).
		DrainExcept(ctx, "owner", Event{Type: EventCompleted, Task: mine}))

	waitForEvent(t, ch, func(e Event) bool {
		return e.Type == EventStarted && e.Task.ID == mine.ID
	})
	got, err := m.Outbox(ctx)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, mine.ID, got[0].TaskID)
	require.Equal(t, EventCompleted, got[0].EventType)

	// The retained record leaves the set on the next plain drain.
	require.NoError(t, NewOutboxNotifier(m, broker).Drain(ctx, "owner"))
	got, err = m.Outbox(ctx)
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestOutboxNotifier_DrainForUnrelatedOwnerIsNoOp(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	broker := pubsub.NewBroker[Event]()
	t.Cleanup(broker.Shutdown)
	ch := broker.Subscribe(ctx)

	m := newTestManager(t, Limits{})
	completeTask(t, m, "owner", "answer")
	require.NoError(t, NewOutboxNotifier(m, broker).Drain(ctx, "nobody"))

	select {
	case ev := <-ch:
		t.Fatalf("unexpected event published: %+v", ev)
	default:
	}
	got, err := m.Outbox(ctx)
	require.NoError(t, err)
	require.Len(t, got, 2, "undrained entries stay queryable after dropped delivery")
}
