package task

import (
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/stretchr/testify/require"
)

func TestSQLiteStore_OutboxTerminalRowsIdempotentAndAck(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := connectStore(t, t.TempDir())
	base := time.Unix(1_700_000_000, 0).UTC()
	mk := func(id string) {
		require.NoError(t, store.Save(ctx, &Task{
			ID: id, OwnerSessionID: "o", Status: StatusRunning, RunGeneration: 1,
		}))
	}
	mk("t1")
	mk("t2")

	_, won, err := store.TerminalizeAndDeliver(ctx, "t1", 1,
		TerminalUpdate{Status: StatusCompleted, CompletedAt: base})
	require.NoError(t, err)
	require.True(t, won)
	// A replay of the same terminalization loses; the unique
	// (task_id, run_generation, event_type) constraint guarantees no
	// second row even if one were attempted.
	_, won, err = store.TerminalizeAndDeliver(ctx, "t1", 1,
		TerminalUpdate{Status: StatusCompleted, CompletedAt: base})
	require.NoError(t, err)
	require.False(t, won)
	_, won, err = store.TerminalizeAndDeliver(ctx, "t2", 1,
		TerminalUpdate{Status: StatusInterrupted, CompletedAt: base.Add(time.Second)})
	require.NoError(t, err)
	require.True(t, won)

	got, err := store.ListOutbox(ctx)
	require.NoError(t, err)
	require.Len(t, got, 2, "duplicate terminalization must not add a row")
	require.Equal(t, "t1", got[0].TaskID, "oldest first")
	require.Equal(t, "t2", got[1].TaskID)
	require.Equal(t, EventCompleted, got[0].EventType)
	require.True(t, base.Equal(got[0].CreatedAt), "created_at round-trip")

	require.NoError(t, store.AckOutbox(ctx, []string{got[0].ID, "unknown"}, base.Add(time.Minute)))
	got, err = store.ListOutbox(ctx)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "t2", got[0].TaskID)
}

func TestSQLiteStore_OutboxSurvivesReload(t *testing.T) {
	dataDir := t.TempDir()
	ctx := t.Context()
	base := time.Unix(1_700_000_000, 0).UTC()

	store := connectStore(t, dataDir)
	for i, id := range []string{"t1", "t2"} {
		require.NoError(t, store.Save(ctx, &Task{
			ID: id, OwnerSessionID: "o", Status: StatusRunning, RunGeneration: 1,
		}))
		_, won, err := store.TerminalizeAndDeliver(ctx, id, 1, TerminalUpdate{
			Status: StatusCompleted, CompletedAt: base.Add(time.Duration(i) * time.Second),
		})
		require.NoError(t, err)
		require.True(t, won)
	}
	got, err := store.ListOutbox(ctx)
	require.NoError(t, err)
	require.NoError(t, store.AckOutbox(ctx, []string{got[0].ID}, base.Add(time.Minute)))
	require.NoError(t, db.Release(dataDir))

	reopened := connectStore(t, dataDir)
	got, err = reopened.ListOutbox(ctx)
	require.NoError(t, err)
	require.Len(t, got, 1, "acked rows stay delivered; undelivered rows stay queryable")
	require.Equal(t, "t2", got[0].TaskID)
}

func TestManager_SQLiteTerminalTransitionWritesOutboxRow(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	m := New(ctx, Config{WorkspaceID: "ws", Store: connectStore(t, t.TempDir())})

	task, err := m.Start(ctx, StartRequest{
		CallerSessionID: "owner", Prompt: "work", Provider: "p", Model: "m",
		ChildSessionID: uniqueChild(), Run: nopRun("done"),
	})
	require.NoError(t, err)
	waitStatus(t, m, "owner", task.ID, StatusCompleted)

	var got []*OutboxEntry
	require.Eventually(t, func() bool {
		entries, err := m.Outbox(ctx)
		if err != nil {
			return false
		}
		got = entries
		return len(entries) == 2 && terminalEntry(entries, task.ID) != nil
	}, 5*time.Second, 10*time.Millisecond)
	require.Equal(t, EventStarted, startedEntry(got, task.ID).EventType,
		"dispatch wrote the start row in its transaction")
	term := terminalEntry(got, task.ID)
	require.Equal(t, EventCompleted, term.EventType)
	snap, err := term.Task()
	require.NoError(t, err)
	require.Equal(t, task.ID, snap.ID)
	require.Equal(t, "done", snap.Result)

	require.NoError(t, m.AckOutbox(ctx, startedEntry(got, task.ID).ID, term.ID))
	remaining, err := m.Outbox(ctx)
	require.NoError(t, err)
	require.Empty(t, remaining)
}
