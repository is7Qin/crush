package task

import (
	"slices"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/stretchr/testify/require"
)

// TestManager_RecoverLiveTasksInterruptsOnceWithoutReplay proves the
// startup recovery contract: every task left pending, running, or
// waiting is force-terminalized as interrupted with reason
// process_restart, exactly one terminal outbox and inbox row per
// generation, no run-generation increment, no parent replay, and
// undelivered mailbox messages still queued. A second recovery run
// adds nothing.
func TestManager_RecoverLiveTasksInterruptsOnceWithoutReplay(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ctx := t.Context()
	conn, err := db.Connect(ctx, dir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Release(dir)) })
	store := NewSQLiteStore(conn)
	_, err = conn.ExecContext(ctx, `INSERT INTO sessions
		(id, parent_session_id, title, message_count, prompt_tokens,
		 completion_tokens, cost, summary_message_id, updated_at, created_at)
		VALUES ('parent', NULL, 'p', 0, 0, 0, 0, NULL, strftime('%s','now'), strftime('%s','now'))`)
	require.NoError(t, err)

	// A crashed process leaves one undispatched pending attempt and
	// one running attempt with a queued follow-up message.
	_, err = store.CreatePendingTask(ctx, admissionFor("pend", "child-1", "initial"))
	require.NoError(t, err)
	_, err = store.CreatePendingTask(ctx, admissionFor("run", "child-2", "initial"))
	require.NoError(t, err)
	_, found, err := store.DispatchNextChildMessage(ctx, "child-2")
	require.NoError(t, err)
	require.True(t, found)
	_, err = conn.ExecContext(ctx,
		`UPDATE agent_tasks SET prompt_tokens = 4, completion_tokens = 2, cost = 0.25 WHERE id = 'run'`)
	require.NoError(t, err)
	msg := appendMsg(store, "run", "queued follow-up")

	// A fresh manager over the same store — no runners, no live map.
	recovered, err := New(ctx, Config{WorkspaceID: "ws", Store: store}).RecoverLiveTasks(ctx)
	require.NoError(t, err)
	require.Len(t, recovered, 2)

	for _, id := range []string{"pend", "run"} {
		got, err := store.Get(ctx, id)
		require.NoError(t, err)
		require.Equal(t, StatusInterrupted, got.Status)
		require.Equal(t, ReasonProcessRestart, got.Summary)
		require.Equal(t, uint64(1), got.RunGeneration, "recovery increments no run generation")
		require.Equal(t, uint64(1), got.TerminalGeneration)
	}

	// Exactly one terminal delivery per generation: one interrupted
	// outbox row and one inbox row per task, plus the start row the
	// pre-crash dispatch committed.
	out, err := store.ListOutbox(ctx)
	require.NoError(t, err)
	require.Len(t, out, 3)
	for _, id := range []string{"pend", "run"} {
		interrupts := 0
		starts := 0
		for _, e := range out {
			if e.TaskID != id {
				continue
			}
			switch e.EventType {
			case EventInterrupted:
				interrupts++
			case EventStarted:
				starts++
			}
		}
		require.Equal(t, 1, interrupts, "exactly one terminal row for %s", id)
		if id == "run" {
			require.Equal(t, 1, starts, "the dispatch start row survives")
		} else {
			require.Zero(t, starts, "a pending task never started")
		}
	}
	in, err := store.ListInbox(ctx, "owner")
	require.NoError(t, err)
	require.Len(t, in, 2, "one parent completion report per recovered task")
	for _, e := range in {
		require.Zero(t, e.DeliveredAt)
		env, err := e.Envelope()
		require.NoError(t, err)
		require.Equal(t, StatusInterrupted, env.Status)
		require.Equal(t, ReasonProcessRestart, env.Summary)
	}

	// A task with dispatch-time usage had that usage aggregated
	// exactly once into its parent by the recovery transaction.
	var cost float64
	var tokens int64
	require.NoError(t, conn.QueryRowContext(ctx,
		`SELECT cost, prompt_tokens FROM sessions WHERE id = 'parent'`).Scan(&cost, &tokens))
	require.InDelta(t, 0.25, cost, 1e-9)
	require.EqualValues(t, 4, tokens)

	// Undelivered mailbox messages remain queued; delivered admission
	// rows stay delivered. No provider replay is possible: nothing
	// started.
	msgs, err := store.ListChildMessages(ctx, "child-1")
	require.NoError(t, err)
	require.Equal(t, MessageQueued, msgs[0].State)
	msgs, err = store.ListChildMessages(ctx, "child-2")
	require.NoError(t, err)
	require.Equal(t, MessageDelivered, msgs[0].State)
	require.Equal(t, msg.ID, msgs[1].ID)
	require.Equal(t, MessageQueued, msgs[1].State, "queued messages survive recovery")

	// Recovery is idempotent: a second pass sees no live tasks and
	// adds no deliveries.
	again, err := New(ctx, Config{WorkspaceID: "ws", Store: store}).RecoverLiveTasks(ctx)
	require.NoError(t, err)
	require.Empty(t, again)
	out, err = store.ListOutbox(ctx)
	require.NoError(t, err)
	require.Len(t, out, 3)
	in, err = store.ListInbox(ctx, "owner")
	require.NoError(t, err)
	require.Len(t, in, 2)
}

// TestManager_RecoverLiveTasksSkipsTerminal proves committed terminal
// records (e.g. from a clean shutdown) survive recovery untouched.
func TestManager_RecoverLiveTasksSkipsTerminal(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ctx := t.Context()
	conn, err := db.Connect(ctx, dir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Release(dir)) })
	store := NewSQLiteStore(conn)
	require.NoError(t, store.Save(ctx, &Task{
		ID: "done", OwnerSessionID: "owner", Status: StatusCompleted,
		RunGeneration: 1, Result: "committed", CompletedAt: time.Unix(1_700_000_000, 0).UTC(),
		CreatedAt: time.Unix(1_700_000_000, 0).UTC(),
	}))

	recovered, err := New(ctx, Config{Store: store}).RecoverLiveTasks(ctx)
	require.NoError(t, err)
	require.Empty(t, recovered)
	got, err := store.Get(ctx, "done")
	require.NoError(t, err)
	require.Equal(t, StatusCompleted, got.Status)
}

// TestLiveStatusListMatchesStatusTerminal pins the SQLite recovery
// guard against Status.Terminal() drift, mirroring the terminal-list
// consistency test.
func TestLiveStatusListMatchesStatusTerminal(t *testing.T) {
	t.Parallel()
	for _, s := range []Status{
		StatusPending, StatusRunning, StatusWaitingForInput,
		StatusCompleted, StatusFailed, StatusCancelled, StatusInterrupted,
	} {
		require.Equal(t, !s.Terminal(), slices.Contains(liveStatusList(), s),
			"live guard drift for %s", s)
	}
}
