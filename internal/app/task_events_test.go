package app

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/agent/notify"
	"github.com/charmbracelet/crush/internal/agent/task"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestHandleTaskEvent_TerminalDrainAcksSupersededRows locks the
// production outbox wiring (review finding B2): a task terminalized
// through the manager has its superseded durable rows republished
// onto the task event stream and acked by the app itself, with no
// consumer calling Drain, while the triggering terminal row stays
// pending for resync. Reverting the handleTaskEvent outbox drain or
// the Subscribe registration fails the superseded-row assertion.
func TestHandleTaskEvent_TerminalDrainAcksSupersededRows(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	app := NewForTest(ctx)
	defer app.ShutdownForTest()

	events := app.taskEvents.Subscribe(ctx)
	rec, err := app.Tasks().Start(ctx, task.StartRequest{
		CallerSessionID: "owner",
		Prompt:          "work",
		Provider:        "p",
		Model:           "m",
		ChildSessionID:  uuid.NewString(),
		Run: func(context.Context, *task.Handle) (task.Result, error) {
			return task.Result{Text: "done"}, nil
		},
	})
	require.NoError(t, err)

	// The terminal fact reaches consumers through the event bridge.
	deadline := time.After(10 * time.Second)
	for sawTerminal := false; !sawTerminal; {
		select {
		case ev := <-events:
			sawTerminal = ev.Payload.Task != nil &&
				task.Status(ev.Payload.Type).Terminal()
		case <-deadline:
			t.Fatal("no terminal task event was bridged")
		}
	}

	// The app's own drain acked the dispatch-time started row...
	require.Eventually(t, func() bool {
		entries, err := app.Tasks().Outbox(ctx)
		if err != nil {
			return false
		}
		for _, e := range entries {
			if e.EventType == task.EventStarted {
				return false
			}
		}
		return true
	}, 5*time.Second, 10*time.Millisecond,
		"superseded outbox rows must be acked after the terminal event")

	// ...while the triggering terminal row survives for resync: a
	// live publish with no proven consumer must not ack into a void.
	entries, err := app.Tasks().Outbox(ctx)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, rec.ID, entries[0].TaskID)
	require.Equal(t, task.EventCompleted, entries[0].EventType)
}

// TestStartupOutboxDrain_ReachesEventFanIn pins the startup replay
// ordering fixed in review: New() drains the recovered outbox only
// after setupEvents, because a PublishMustDeliver onto a broker with
// zero subscribers is a silent no-op and the drain acks what it
// "published". NewForTest wires the same fan-in synchronously, so
// running the startup drain right after construction must make every
// recovered record observable on App.Events, and only then may it
// leave the durable pending set. Moving the drain before the fan-in
// wiring (or making subscription installation async again) loses the
// event and fails the receive.
func TestStartupOutboxDrain_ReachesEventFanIn(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	app := NewForTest(ctx)
	defer app.ShutdownForTest()

	// Durable state as a previous process would leave it: one
	// terminalized task whose lifecycle record was never delivered.
	store := app.taskManager.Store()
	require.NoError(t, store.Save(ctx, &task.Task{
		ID: "t1", OwnerSessionID: "owner", Status: task.StatusRunning, RunGeneration: 1,
	}))
	_, won, err := store.TerminalizeAndDeliver(ctx, "t1", 1,
		task.TerminalUpdate{Status: task.StatusCompleted, CompletedAt: time.Now()})
	require.NoError(t, err)
	require.True(t, won)

	events := app.Events(ctx)
	require.NoError(t, app.TaskOutbox().Drain(ctx, ""))

	select {
	case ev := <-events:
		fanned, ok := ev.Payload.(pubsub.Event[task.Event])
		require.True(t, ok, "expected a bridged task event, got %T", ev.Payload)
		require.NotNil(t, fanned.Payload.Task)
		require.Equal(t, "t1", fanned.Payload.Task.ID)
		require.Equal(t, task.EventCompleted, fanned.Payload.Type)
	case <-time.After(5 * time.Second):
		t.Fatal("recovered outbox event never reached the app event fan-in")
	}

	entries, err := app.Tasks().Outbox(ctx)
	require.NoError(t, err)
	require.Empty(t, entries, "the record must leave the pending set after replay")
}

// eventSpy collects every payload published onto the app's task
// events broker while a test waits for a durable condition.
type eventSpy struct {
	mu     sync.Mutex
	events []task.Event
}

func (s *eventSpy) record(ev pubsub.Event[task.Event]) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, ev.Payload)
}

func (s *eventSpy) hiddenCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, ev := range s.events {
		if ev.Task.IsHidden() {
			n++
		}
	}
	return n
}

// TestHandleTaskEvent_HiddenNotBridged locks the source-side gates:
// a hidden (agentic_fetch) task's facts must never enter the shared
// taskEvents broker, neither through the manager bridge nor through
// the terminal outbox replay, while the durable drain still runs for
// the hidden task (superseded rows acked, the triggering terminal row
// retained pending, never deleted). Reverting the handleTaskEvent or
// OutboxNotifier.drain gates fails the zero-hidden assertion; skipping
// hidden tasks in the drain entirely fails the ack assertion.
func TestHandleTaskEvent_HiddenNotBridged(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	app := NewForTest(ctx)
	defer app.ShutdownForTest()

	spy := &eventSpy{}
	events := app.taskEvents.Subscribe(ctx)
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case ev := <-events:
				spy.record(ev)
			case <-done:
				return
			}
		}
	}()

	rec, err := app.Tasks().Start(ctx, task.StartRequest{
		CallerSessionID: "owner",
		Profile:         task.HiddenProfile,
		Prompt:          "fetch",
		Provider:        "p",
		Model:           "m",
		ChildSessionID:  uuid.NewString(),
		Run: func(context.Context, *task.Handle) (task.Result, error) {
			return task.Result{Text: "done"}, nil
		},
	})
	require.NoError(t, err)

	// The hidden terminalization still triggers the app's drain: the
	// superseded started row is acked off the pending set.
	require.Eventually(t, func() bool {
		entries, err := app.Tasks().Outbox(ctx)
		if err != nil {
			return false
		}
		for _, e := range entries {
			if e.EventType == task.EventStarted {
				return false
			}
		}
		return true
	}, 5*time.Second, 10*time.Millisecond,
		"hidden task's superseded outbox row must still be acked")

	// The triggering terminal row survives durably: gating live
	// delivery must never delete hidden rows.
	entries, err := app.Tasks().Outbox(ctx)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, rec.ID, entries[0].TaskID)
	require.Equal(t, task.EventCompleted, entries[0].EventType)

	// Nothing hidden ever reached the broker. Never (not Zero) because
	// the spy's record races the store ack it is gated behind: if a
	// gate regressed, its leaked publishes would surface in this
	// window.
	require.Never(t, func() bool {
		return spy.hiddenCount() > 0
	}, 500*time.Millisecond, 10*time.Millisecond,
		"hidden task events must not be bridged or replayed onto the broker")
}

// idleGate is a ParentGate whose readiness the test controls: while
// busy it reports every parent not ready, mirroring a parent mid-turn
// whose gate must retain durable rows.
type idleGate struct{ busy atomic.Bool }

func (g *idleGate) ParentReady(context.Context, string) (bool, error) {
	return !g.busy.Load(), nil
}

// envelopeRecorder captures the result envelopes a drain commits.
type envelopeRecorder struct {
	mu   sync.Mutex
	envs []task.TaskResultEnvelope
}

func (r *envelopeRecorder) WriteResult(_ context.Context, _ string, env task.TaskResultEnvelope) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.envs = append(r.envs, env)
	return nil
}

func (r *envelopeRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.envs)
}

// TestRunComplete_RedrainsInboxWhenParentGoesIdle locks the review H1
// wiring: a child terminalizing while its parent is busy leaves the
// durable inbox row pending (the terminal-event drain is gate-skipped),
// and the parent's own notify.RunComplete must re-trigger that parent's
// drain so the report lands exactly once when it goes idle. Reverting
// watchParentIdle or its registration leaves the row pending and fails
// the delivery assertion; bypassing the gate or acking without a commit
// fails the count/pending checks.
func TestRunComplete_RedrainsInboxWhenParentGoesIdle(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	app := NewForTest(ctx)
	defer app.ShutdownForTest()

	gate := &idleGate{}
	gate.busy.Store(true)
	rec := &envelopeRecorder{}
	app.taskInbox = task.NewInboxDrainer(app.taskManager.Store(), gate, rec)

	_, err := app.Tasks().Start(ctx, task.StartRequest{
		CallerSessionID: "parent",
		Prompt:          "work",
		Provider:        "p",
		Model:           "m",
		ChildSessionID:  uuid.NewString(),
		Run: func(context.Context, *task.Handle) (task.Result, error) {
			return task.Result{Text: "done"}, nil
		},
	})
	require.NoError(t, err)

	// The terminal report is retained durably while the parent is busy:
	// the terminalization-time drain runs and the gate skips it.
	require.Eventually(t, func() bool {
		entries, err := app.Tasks().Inbox(ctx, "parent")
		return err == nil && len(entries) == 1
	}, 5*time.Second, 10*time.Millisecond,
		"busy parent must retain the terminal inbox row")
	require.Zero(t, rec.count(), "no delivery may occur while the parent is busy")

	// Parent goes idle and its turn emits the terminal RunComplete.
	// That lifecycle event alone must redrain and deliver the row.
	gate.busy.Store(false)
	app.runCompletions.PublishMustDeliver(ctx, pubsub.UpdatedEvent,
		notify.RunComplete{SessionID: "parent"})

	require.Eventually(t, func() bool {
		entries, err := app.Tasks().Inbox(ctx, "parent")
		return err == nil && len(entries) == 0 && rec.count() == 1
	}, 5*time.Second, 10*time.Millisecond,
		"parent RunComplete must redrain the retained row exactly once")
	env := rec.envs[0]
	require.Equal(t, task.StatusCompleted, env.Status)
}
