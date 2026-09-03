package task

import (
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/stretchr/testify/require"
)

// sqliteTask returns a fully populated record with monotonic-free UTC
// timestamps so stored and decoded times compare structurally.
func sqliteTask() *Task {
	return &Task{
		ID:                       "t1",
		OwnerSessionID:           "owner",
		ParentSessionID:          "parent",
		ChildSessionID:           "child",
		ParentMessageID:          "parent-msg",
		ToolCallID:               "tool-call",
		ResumesTaskID:            "prev",
		Profile:                  "reviewer",
		ProfileGeneration:        7,
		RequestedModel:           "openai/gpt-5",
		FallbackModels:           []string{"anthropic/fallback", "openai/backup"},
		PromptFingerprint:        "prompt-fp",
		ToolFingerprint:          "tool-fp",
		Provider:                 "anthropic",
		Model:                    "claude-opus-4-1",
		RunGeneration:            3,
		TerminalGeneration:       0,
		CostAggregatedGeneration: 2,
		Prompt:                   "do the thing",
		Status:                   StatusRunning,
		PromptTokens:             11,
		CompletionTokens:         22,
		Cost:                     0.25,
		CreatedAt:                time.Unix(1_700_000_000, 123).UTC(),
		StartedAt:                time.Unix(1_700_000_010, 456).UTC(),
		UpdatedAt:                time.Unix(1_700_000_011, 0).UTC(),
	}
}

func requireTaskEqual(t *testing.T, want, got *Task) {
	t.Helper()
	w, g := *want, *got
	require.Truef(t, w.CreatedAt.Equal(g.CreatedAt), "created_at: want %v, got %v", w.CreatedAt, g.CreatedAt)
	require.Truef(t, w.StartedAt.Equal(g.StartedAt), "started_at: want %v, got %v", w.StartedAt, g.StartedAt)
	require.Truef(t, w.CompletedAt.Equal(g.CompletedAt), "completed_at: want %v, got %v", w.CompletedAt, g.CompletedAt)
	w.CreatedAt, g.CreatedAt = time.Time{}, time.Time{}
	w.StartedAt, g.StartedAt = time.Time{}, time.Time{}
	w.CompletedAt, g.CompletedAt = time.Time{}, time.Time{}
	// updated_at is stamped by the write path, not carried by the
	// fixture; it is pinned explicitly where the transition matters.
	w.UpdatedAt, g.UpdatedAt = time.Time{}, time.Time{}
	require.Equal(t, w, g)
}

func connectStore(t *testing.T, dataDir string) *SQLiteStore {
	t.Helper()
	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
	})
	return NewSQLiteStore(conn)
}

func TestSQLiteStore_SaveGetRoundTripsAllFields(t *testing.T) {
	t.Parallel()
	store := connectStore(t, t.TempDir())

	want := sqliteTask()
	require.NoError(t, store.Save(t.Context(), want))

	got, err := store.Get(t.Context(), want.ID)
	require.NoError(t, err)
	requireTaskEqual(t, want, got)
}

func TestSQLiteStore_SaveReplacesFullRecord(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := connectStore(t, t.TempDir())

	require.NoError(t, store.Save(ctx, sqliteTask()))

	updated := sqliteTask()
	updated.Status = StatusCompleted
	updated.Result = "done"
	updated.ResultTruncated = true
	updated.CompletedAt = time.Unix(1_700_000_099, 789).UTC()
	require.NoError(t, store.Save(ctx, updated))

	got, err := store.Get(ctx, updated.ID)
	require.NoError(t, err)
	requireTaskEqual(t, updated, got)
}

func TestSQLiteStore_GetNotFound(t *testing.T) {
	t.Parallel()
	_, err := connectStore(t, t.TempDir()).Get(t.Context(), "missing")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestSQLiteStore_ListByOwnerOldestFirst(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := connectStore(t, t.TempDir())

	base := time.Unix(1_700_000_000, 0).UTC()
	for i, owner := range []string{"a", "b", "a"} {
		require.NoError(t, store.Save(ctx, &Task{
			ID:             string(rune('1' + i)),
			OwnerSessionID: owner,
			Status:         StatusPending,
			CreatedAt:      base.Add(time.Duration(i) * time.Second),
		}))
	}

	got, err := store.ListByOwner(ctx, "a")
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Equal(t, "1", got[0].ID, "oldest first")
	require.Equal(t, "3", got[1].ID)

	empty, err := store.ListByOwner(ctx, "nobody")
	require.NoError(t, err)
	require.Empty(t, empty)
}

func TestSQLiteStore_TerminalizeAndDeliverWinsOnce(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := connectStore(t, t.TempDir())

	require.NoError(t, store.Save(ctx, &Task{
		ID: "t1", OwnerSessionID: "o", ParentSessionID: "p", Status: StatusRunning,
		Prompt: "work", Profile: "coder",
	}))

	u := TerminalUpdate{Status: StatusCompleted, Result: "first", CompletedAt: time.Unix(1_700_000_050, 0).UTC()}
	got, won, err := store.TerminalizeAndDeliver(ctx, "t1", 0, u)
	require.NoError(t, err)
	require.True(t, won)
	require.Equal(t, StatusCompleted, got.Status)
	require.Equal(t, "first", got.Result)
	// The win result comes back from the update statement itself, so
	// a committed terminalization never depends on (or fails behind)
	// a follow-up read: the full record, prompt and profile included,
	// is the winning write's row.
	require.Equal(t, "work", got.Prompt)
	require.Equal(t, "coder", got.Profile)
	require.True(t, got.UpdatedAt.Equal(u.CompletedAt))

	// A conflicting second update loses and cannot rewrite the record.
	_, won, err = store.TerminalizeAndDeliver(ctx, "t1", 0, TerminalUpdate{Status: StatusFailed, Err: "late"})
	require.NoError(t, err)
	require.False(t, won)
	got, _ = store.Get(ctx, "t1")
	require.Equal(t, StatusCompleted, got.Status)
	require.Empty(t, got.Err)

	// A repeated identical terminal update is also a no-op loss.
	_, won, err = store.TerminalizeAndDeliver(ctx, "t1", 0, u)
	require.NoError(t, err)
	require.False(t, won)

	// A stale run generation loses against the live record's fence.
	require.NoError(t, store.Save(ctx, &Task{
		ID: "t2", OwnerSessionID: "o", Status: StatusRunning, RunGeneration: 5,
	}))
	_, won, err = store.TerminalizeAndDeliver(ctx, "t2", 4, u)
	require.NoError(t, err)
	require.False(t, won, "a retired attempt cannot terminalize its successor")

	_, _, err = store.TerminalizeAndDeliver(ctx, "nope", 0, u)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestSQLiteStore_SurvivesReload(t *testing.T) {
	dataDir := t.TempDir()
	ctx := t.Context()

	store := connectStore(t, dataDir)
	want := sqliteTask()
	require.NoError(t, store.Save(ctx, want))
	_, won, err := store.TerminalizeAndDeliver(ctx, want.ID, want.RunGeneration, TerminalUpdate{
		Status:      StatusInterrupted,
		Err:         "restart",
		CompletedAt: time.Unix(1_700_000_120, 0).UTC(),
		Usage:       UsageDelta{PromptTokens: 11, CompletionTokens: 22, Cost: 0.25},
	})
	require.NoError(t, err)
	require.True(t, won)

	require.NoError(t, db.Release(dataDir))

	reopened := connectStore(t, dataDir)
	got, err := reopened.Get(ctx, want.ID)
	require.NoError(t, err)

	reloaded := sqliteTask()
	reloaded.Status = StatusInterrupted
	reloaded.Err = "restart"
	reloaded.CompletedAt = time.Unix(1_700_000_120, 0).UTC()
	// The terminal transaction fences the delivery generations with
	// the run generation it committed.
	reloaded.TerminalGeneration = reloaded.RunGeneration
	reloaded.CostAggregatedGeneration = reloaded.RunGeneration
	requireTaskEqual(t, reloaded, got)
	require.True(t, got.UpdatedAt.Equal(got.CompletedAt),
		"the terminal write stamps updated_at")
	require.Equal(t, "reviewer", got.Profile, "run/profile/model metadata must persist")
	require.Equal(t, "anthropic", got.Provider)
	require.Equal(t, "claude-opus-4-1", got.Model)
	require.Equal(t, uint64(3), got.RunGeneration)
}

func TestTerminalStatusListMatchesStatusTerminal(t *testing.T) {
	t.Parallel()
	set := map[Status]bool{}
	for _, s := range terminalStatusList {
		set[s] = true
	}
	for _, s := range []Status{
		StatusPending, StatusRunning, StatusWaitingForInput,
		StatusCompleted, StatusFailed, StatusCancelled, StatusInterrupted,
	} {
		require.Equal(t, s.Terminal(), set[s], "terminal guard drift for %s", s)
	}
}
