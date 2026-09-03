package task

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestMemoryStore_SaveGetReturnsCopies(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := NewMemoryStore()
	orig := &Task{ID: "t1", OwnerSessionID: "o1", Status: StatusPending, CreatedAt: time.Now()}
	require.NoError(t, s.Save(ctx, orig))

	got, err := s.Get(ctx, "t1")
	require.NoError(t, err)
	got.Status = StatusRunning
	require.NoError(t, s.Save(ctx, got))

	again, err := s.Get(ctx, "t1")
	require.NoError(t, err)
	require.Equal(t, StatusRunning, again.Status)
	require.Equal(t, StatusPending, orig.Status, "caller mutation must not leak")
}

func TestMemoryStore_GetNotFound(t *testing.T) {
	t.Parallel()
	_, err := NewMemoryStore().Get(t.Context(), "missing")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestMemoryStore_ListByOwner(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := NewMemoryStore()
	base := time.Now()
	for i, owner := range []string{"a", "b", "a"} {
		require.NoError(t, s.Save(ctx, &Task{
			ID: string(rune('1' + i)), OwnerSessionID: owner,
			CreatedAt: base.Add(time.Duration(i) * time.Second),
		}))
	}
	got, err := s.ListByOwner(ctx, "a")
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Equal(t, "1", got[0].ID, "oldest first")
	require.Equal(t, "3", got[1].ID)
}

func TestMemoryStore_TerminalizeAndDeliverWinsOnce(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := NewMemoryStore()
	require.NoError(t, s.Save(ctx, &Task{ID: "t1", Status: StatusRunning}))

	u := TerminalUpdate{Status: StatusCompleted, Result: "first", CompletedAt: time.Now()}
	got, won, err := s.TerminalizeAndDeliver(ctx, "t1", 0, u)
	require.NoError(t, err)
	require.True(t, won)
	require.Equal(t, StatusCompleted, got.Status)
	require.Equal(t, "first", got.Result)

	// A conflicting second update loses and cannot rewrite the record.
	_, won, err = s.TerminalizeAndDeliver(ctx, "t1", 0, TerminalUpdate{Status: StatusFailed, Err: "late"})
	require.NoError(t, err)
	require.False(t, won)
	got, _ = s.Get(ctx, "t1")
	require.Equal(t, StatusCompleted, got.Status)
	require.Empty(t, got.Err)

	// A repeated identical terminal update is also a no-op loss.
	_, won, err = s.TerminalizeAndDeliver(ctx, "t1", 0, u)
	require.NoError(t, err)
	require.False(t, won)

	// A stale run generation loses against the live record's fence.
	require.NoError(t, s.Save(ctx, &Task{ID: "t2", Status: StatusRunning, RunGeneration: 5}))
	_, won, err = s.TerminalizeAndDeliver(ctx, "t2", 4, u)
	require.NoError(t, err)
	require.False(t, won, "a retired attempt cannot terminalize its successor")

	_, _, err = s.TerminalizeAndDeliver(ctx, "nope", 0, u)
	require.ErrorIs(t, err, ErrNotFound)
}
