package task

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/stretchr/testify/require"
)

// childBinding reports the retained binding and queued hint for a
// child session. Callers use it to assert release without reaching
// into the manager lock themselves.
func childBinding(m *Manager, child string) (bound bool, queued int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, bound = m.children[child]
	return bound, m.queuedMsgs[child]
}

// countFactory builds a factory that counts rebuilds and runs every
// continuation with the claimed mailbox prompt.
func countFactory(rebuilds *atomic.Int32) RunnerFactory {
	return func(_ context.Context, t Task) (RebuiltChild, error) {
		rebuilds.Add(1)
		return RebuiltChild{
			Run: func(_ context.Context, h *Handle) (Result, error) {
				return Result{Text: "rebuilt for " + h.Prompt()}, nil
			},
			Key:             CapacityKey{WorkspaceID: "ws", Provider: t.Provider, Model: t.Model},
			ParentSessionID: t.ParentSessionID,
		}, nil
	}
}

func TestManager_IdleChildRunnerReleased(t *testing.T) {
	t.Parallel()
	var rebuilds atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	m := New(ctx, Config{
		WorkspaceID:   "ws",
		Limits:        Limits{},
		Store:         NewMemoryStore(),
		RunnerFactory: countFactory(&rebuilds),
	})

	child := uniqueChild()
	first, err := m.Start(t.Context(), StartRequest{
		CallerSessionID: "owner", Provider: "p", Model: "m",
		ChildSessionID: child, Prompt: "work", Run: nopRun("done"),
	})
	require.NoError(t, err)
	waitStatus(t, m, "owner", first.ID, StatusCompleted)

	require.Eventually(t, func() bool {
		bound, _ := childBinding(m, child)
		return !bound
	}, 5*time.Second, 5*time.Millisecond, "idle child must be released")
	bound, queued := childBinding(m, child)
	require.False(t, bound)
	require.Zero(t, queued, "the queued hint must go with the binding")
	require.Zero(t, rebuilds.Load(), "no rebuild without a continuation")
}

func TestManager_ReleasedChildContinuationRebuilds(t *testing.T) {
	t.Parallel()
	var rebuilds atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	m := New(ctx, Config{
		WorkspaceID:   "ws",
		Limits:        Limits{},
		Store:         NewMemoryStore(),
		RunnerFactory: countFactory(&rebuilds),
	})
	r := record(m)

	child := uniqueChild()
	first, err := m.Start(t.Context(), StartRequest{
		CallerSessionID: "owner", Provider: "p", Model: "m",
		ChildSessionID: child, Prompt: "initial", Run: nopRun("first"),
	})
	require.NoError(t, err)
	waitStatus(t, m, "owner", first.ID, StatusCompleted)
	require.Eventually(t, func() bool {
		bound, _ := childBinding(m, child)
		return !bound
	}, 5*time.Second, 5*time.Millisecond, "precondition: child released")

	acc := appendMessage(t, m, "owner", first.ID, "after terminal")
	require.NotEmpty(t, acc.AttemptTaskID, "a released child must still dispatch")
	require.Equal(t, int32(1), rebuilds.Load(), "exactly one rebuild per release")

	got := waitStatus(t, m, "owner", acc.AttemptTaskID, StatusCompleted)
	require.Equal(t, "after terminal", got.Prompt)
	require.Equal(t, "rebuilt for after terminal", got.Result)
	require.Equal(t, uint64(2), got.RunGeneration)
	require.Equal(t, first.ID, got.ResumesTaskID)

	// The successor terminalizes exactly once and releases again.
	require.Eventually(t, func() bool {
		return countTerminal(r, acc.AttemptTaskID) == 1
	}, 5*time.Second, 5*time.Millisecond)
	require.Eventually(t, func() bool {
		bound, _ := childBinding(m, child)
		return !bound
	}, 5*time.Second, 5*time.Millisecond, "successor must release too")
	old, err := m.Status(context.Background(), "owner", first.ID)
	require.NoError(t, err)
	require.Equal(t, "first", old.Result, "the predecessor record is untouched")
}

func TestManager_BusyChildNotReleasedPrematurely(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	var rebuilds atomic.Int32
	m := New(ctx, Config{
		WorkspaceID:   "ws",
		Limits:        Limits{},
		Store:         NewMemoryStore(),
		RunnerFactory: countFactory(&rebuilds),
	})
	release := make(chan struct{})
	entered := make(chan struct{})
	var once atomic.Bool

	child := uniqueChild()
	first, err := m.Start(t.Context(), StartRequest{
		CallerSessionID: "owner", Provider: "p", Model: "m",
		ChildSessionID: child, Prompt: "initial",
		Run: func(ctx context.Context, h *Handle) (Result, error) {
			if once.CompareAndSwap(false, true) {
				close(entered)
			}
			select {
			case <-release:
				return Result{Text: "late"}, nil
			case <-ctx.Done():
				return Result{}, ctx.Err()
			}
		},
	})
	require.NoError(t, err)
	waitStatus(t, m, "owner", first.ID, StatusRunning)

	// A live attempt pins the binding even with nothing queued.
	bound, _ := childBinding(m, child)
	require.True(t, bound, "a running child must stay bound")

	acc := appendMessage(t, m, "owner", first.ID, "while running")
	require.Empty(t, acc.AttemptTaskID)
	bound, _ = childBinding(m, child)
	require.True(t, bound, "a child with queued messages must stay bound")

	close(release)
	<-entered
	waitStatus(t, m, "owner", first.ID, StatusCompleted)
	attempts := waitAttemptCount(t, m, "owner", 2)
	succ := attemptWithGeneration(attempts, 2)
	require.NotNil(t, succ)
	waitStatus(t, m, "owner", succ.ID, StatusCompleted)
	require.Eventually(t, func() bool {
		bound, _ := childBinding(m, child)
		return !bound
	}, 5*time.Second, 5*time.Millisecond, "quiesced child must release")
}

func TestManager_RetainedChildWithoutFactoryDispatches(t *testing.T) {
	t.Parallel()
	m := newTestManager(t, Limits{})

	first, err := m.Start(t.Context(), StartRequest{
		CallerSessionID: "owner", Provider: "p", Model: "m",
		ChildSessionID: uniqueChild(), Prompt: "initial", Run: nopRun("done"),
	})
	require.NoError(t, err)
	waitStatus(t, m, "owner", first.ID, StatusCompleted)

	// Without a factory a release would strand continuations, so
	// the binding is retained and the message dispatches at once.
	bound, _ := childBinding(m, first.ChildSessionID)
	require.True(t, bound, "no factory means no release")
	acc := appendMessage(t, m, "owner", first.ID, "follow-up")
	require.NotEmpty(t, acc.AttemptTaskID)
	got := waitStatus(t, m, "owner", acc.AttemptTaskID, StatusCompleted)
	require.Equal(t, "follow-up", got.Prompt)
}

func TestManager_SQLiteReleasedChildContinuationRebuilds(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	conn, err := db.Connect(t.Context(), dir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Release(dir)) })

	var rebuilds atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	m := New(ctx, Config{
		WorkspaceID:   "ws",
		Store:         NewSQLiteStore(conn),
		RunnerFactory: countFactory(&rebuilds),
	})

	child := uniqueChild()
	first, err := m.Start(t.Context(), StartRequest{
		CallerSessionID: "owner", Provider: "p", Model: "m",
		ChildSessionID: child, Prompt: "initial", Run: nopRun("first"),
	})
	require.NoError(t, err)
	waitStatus(t, m, "owner", first.ID, StatusCompleted)
	require.Eventually(t, func() bool {
		bound, _ := childBinding(m, child)
		return !bound
	}, 5*time.Second, 5*time.Millisecond, "precondition: child released")

	acc := appendMessage(t, m, "owner", first.ID, "after terminal")
	require.NotEmpty(t, acc.AttemptTaskID)
	got := waitStatus(t, m, "owner", acc.AttemptTaskID, StatusCompleted)
	require.Equal(t, "rebuilt for after terminal", got.Result)
	require.Eventually(t, func() bool {
		bound, _ := childBinding(m, child)
		return !bound
	}, 5*time.Second, 5*time.Millisecond, "sqlite successor must release too")
}
