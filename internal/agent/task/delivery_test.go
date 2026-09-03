package task

import (
	"context"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/stretchr/testify/require"
)

// deliveryHarness is one Store implementation plus the observers the
// terminal-delivery contract is asserted through: parent usage (the
// SQLite path reads the sessions row, the memory path its aggregation
// map) and parent-session seeding.
type deliveryHarness struct {
	store Store
	usage func(ctx context.Context, sessionID string) UsageDelta
	seed  func(ctx context.Context, sessionID string) error
}

func eachDeliveryStore(t *testing.T, fn func(t *testing.T, h *deliveryHarness)) {
	t.Helper()

	t.Run("memory", func(t *testing.T) {
		t.Parallel()
		s := NewMemoryStore()
		fn(t, &deliveryHarness{
			store: s,
			usage: func(_ context.Context, id string) UsageDelta { return s.ParentUsage(id) },
			seed:  func(context.Context, string) error { return nil },
		})
	})
	t.Run("sqlite", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		conn, err := db.Connect(t.Context(), dir)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, db.Release(dir)) })
		fn(t, &deliveryHarness{
			store: NewSQLiteStore(conn),
			usage: func(ctx context.Context, id string) UsageDelta {
				var u UsageDelta
				err := conn.QueryRowContext(ctx,
					`SELECT prompt_tokens, completion_tokens, cost FROM sessions WHERE id = ?`, id,
				).Scan(&u.PromptTokens, &u.CompletionTokens, &u.Cost)
				require.NoErrorf(t, err, "parent session %s must exist for usage observation", id)
				return u
			},
			seed: func(ctx context.Context, id string) error {
				_, err := conn.ExecContext(ctx, `INSERT INTO sessions
					(id, parent_session_id, title, message_count, prompt_tokens,
					 completion_tokens, cost, summary_message_id, updated_at, created_at)
					VALUES (?, NULL, ?, 0, 0, 0, 0, NULL, strftime('%s', 'now'), strftime('%s', 'now'))`,
					id, "parent "+id)
				return err
			},
		})
	})
}

// runningTask admits t1, dispatches it to running, and returns the
// live attempt for delivery tests.
func runningTask(t *testing.T, h *deliveryHarness) *Task {
	t.Helper()
	ctx := t.Context()
	require.NoError(t, h.seed(ctx, "parent"))
	_, err := h.store.CreatePendingTask(ctx, admissionFor("t1", "child-1", "initial"))
	require.NoError(t, err)
	_, found, err := h.store.DispatchNextChildMessage(ctx, "child-1")
	require.NoError(t, err)
	require.True(t, found)
	cur, err := h.store.Get(ctx, "t1")
	require.NoError(t, err)
	require.Equal(t, StatusRunning, cur.Status)
	return cur
}

// TestTerminalDelivery_AtomicRowsAndCostOnce proves the whole
// contract in one transaction: the terminal task update, the parent
// cost increment applied exactly once per run generation, the outbox
// row, and the owner's inbox row. Replays add none of them again.
func TestTerminalDelivery_AtomicRowsAndCostOnce(t *testing.T) {
	eachDeliveryStore(t, func(t *testing.T, h *deliveryHarness) {
		ctx := t.Context()
		runningTask(t, h)

		usage := UsageDelta{PromptTokens: 7, CompletionTokens: 3, Cost: 0.5}
		before := h.usage(ctx, "parent")
		got, won, err := h.store.TerminalizeAndDeliver(ctx, "t1", 1, TerminalUpdate{
			Status: StatusCompleted, Result: "done", Summary: "all good",
			CompletedAt: time.Now(), Usage: usage,
		})
		require.NoError(t, err)
		require.True(t, won)
		require.Equal(t, uint64(1), got.TerminalGeneration)
		require.Equal(t, uint64(1), got.CostAggregatedGeneration)
		require.Equal(t, int64(7), got.PromptTokens, "the attempt's usage snapshot is stored on the task")
		require.Equal(t, 0.5, got.Cost)

		// The parent cost update is observable and numeric: exactly
		// the reported delta was aggregated.
		after := h.usage(ctx, "parent")
		require.Equal(t, before.PromptTokens+7, after.PromptTokens)
		require.Equal(t, before.CompletionTokens+3, after.CompletionTokens)
		require.InDelta(t, before.Cost+0.5, after.Cost, 1e-9)

		out, err := h.store.ListOutbox(ctx)
		require.NoError(t, err)
		term := terminalEntry(out, "t1")
		require.NotNil(t, term)
		require.Equal(t, EventCompleted, term.EventType)

		in, err := h.store.ListInbox(ctx, "owner")
		require.NoError(t, err)
		require.Len(t, in, 1)
		require.Equal(t, "t1", in[0].TaskID)
		require.Equal(t, uint64(1), in[0].TerminalGeneration)
		require.True(t, in[0].DeliveredAt.IsZero())
		env, err := in[0].Envelope()
		require.NoError(t, err)
		require.Equal(t, StatusCompleted, env.Status)
		require.Equal(t, "child-1", env.ChildSessionID)
		require.Equal(t, "coder", env.Profile)
		require.Equal(t, "done", env.Result)

		// Repeating the same terminalization increases the parent
		// cost by nothing and adds no rows.
		_, won, err = h.store.TerminalizeAndDeliver(ctx, "t1", 1, TerminalUpdate{
			Status: StatusCompleted, Result: "done", CompletedAt: time.Now(), Usage: usage,
		})
		require.NoError(t, err)
		require.False(t, won)
		repeated := h.usage(ctx, "parent")
		require.Equal(t, after, repeated)
		out, err = h.store.ListOutbox(ctx)
		require.NoError(t, err)
		require.Len(t, undeliveredFor(out, "t1"), 2, "started + exactly one terminal row")
		in, err = h.store.ListInbox(ctx, "owner")
		require.NoError(t, err)
		require.Len(t, in, 1, "exactly one inbox row per terminalization")
	})
}

// TestTerminalDelivery_StaleGenerationLosesEverything proves the
// run-generation fence: an update from a retired attempt neither
// terminalizes the successor, aggregates cost, nor writes rows.
func TestTerminalDelivery_StaleGenerationLosesEverything(t *testing.T) {
	eachDeliveryStore(t, func(t *testing.T, h *deliveryHarness) {
		ctx := t.Context()
		runningTask(t, h)

		_, won, err := h.store.TerminalizeAndDeliver(ctx, "t1", 99, TerminalUpdate{
			Status: StatusFailed, CompletedAt: time.Now(),
			Usage: UsageDelta{PromptTokens: 100, Cost: 9},
		})
		require.NoError(t, err)
		require.False(t, won)

		cur, err := h.store.Get(ctx, "t1")
		require.NoError(t, err)
		require.Equal(t, StatusRunning, cur.Status)
		require.Zero(t, h.usage(ctx, "parent"))
		out, err := h.store.ListOutbox(ctx)
		require.NoError(t, err)
		require.Nil(t, terminalEntry(out, "t1"))
		in, err := h.store.ListInbox(ctx, "owner")
		require.NoError(t, err)
		require.Empty(t, in)
	})
}

// TestSQLiteTerminalDelivery_RollbackOnDeliveryFailure proves that a
// database error in the delivery half of the transaction rolls the
// task update back: no committed terminal row without its records.
func TestSQLiteTerminalDelivery_RollbackOnDeliveryFailure(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ctx := t.Context()
	conn, err := db.Connect(ctx, dir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Release(dir)) })
	h := &deliveryHarness{
		store: NewSQLiteStore(conn),
		seed:  func(context.Context, string) error { return nil },
	}
	_, err = conn.ExecContext(ctx,
		`INSERT INTO sessions
		(id, parent_session_id, title, message_count, prompt_tokens,
		 completion_tokens, cost, summary_message_id, updated_at, created_at)
		VALUES ('parent', NULL, 'p', 0, 1, 1, 0.1, NULL, strftime('%s','now'), strftime('%s','now'))`)
	require.NoError(t, err)
	h.usage = func(ctx context.Context, id string) UsageDelta {
		var u UsageDelta
		require.NoError(t, conn.QueryRowContext(ctx,
			`SELECT prompt_tokens, completion_tokens, cost FROM sessions WHERE id = ?`, id,
		).Scan(&u.PromptTokens, &u.CompletionTokens, &u.Cost))
		return u
	}
	runningTask(t, h)

	// Break the delivery target so the transaction must fail.
	_, err = conn.ExecContext(ctx, `DROP TABLE agent_task_inbox`)
	require.NoError(t, err)
	_, won, err := h.store.TerminalizeAndDeliver(ctx, "t1", 1, TerminalUpdate{
		Status: StatusCompleted, CompletedAt: time.Now(),
		Usage: UsageDelta{PromptTokens: 5, Cost: 1},
	})
	require.Error(t, err)
	require.False(t, won)

	cur, err := h.store.Get(ctx, "t1")
	require.NoError(t, err)
	require.Equal(t, StatusRunning, cur.Status, "the whole transaction rolled back")
	require.Zero(t, cur.PromptTokens, "task columns kept their pre-transaction values")
	require.EqualValues(t, 1, h.usage(ctx, "parent").PromptTokens, "parent cost untouched")
	out, err := h.store.ListOutbox(ctx)
	require.NoError(t, err)
	require.Nil(t, terminalEntry(out, "t1"), "no outbox row behind a rolled-back terminal")
}

// TestTaskResultEnvelope_RendersUntrustedDelimiters pins the
// structural tokens the model-facing serialization must carry so the
// child result cannot read as an instruction.
func TestTaskResultEnvelope_RendersUntrustedDelimiters(t *testing.T) {
	t.Parallel()
	env := TaskResultEnvelope{
		TaskID: "t1", ChildSessionID: "c1", Profile: "coder",
		RunGeneration: 2, Status: StatusCompleted, Result: "text",
		ResultTruncated: true,
	}
	s := env.Render()
	for _, token := range []string{
		"<untrusted-agent-result>", "</untrusted-agent-result>",
		"task_id: t1", "child_session_id: c1", "status: completed",
		"agent_output",
	} {
		require.Contains(t, s, token)
	}
}
