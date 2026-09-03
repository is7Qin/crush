package task

import (
	"context"
	"errors"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// errTerminalBoom is the fixed rejection of flakyTerminalStore.
var errTerminalBoom = errors.New("terminalize boom")

// flakyTerminalStore fails the first failLeft TerminalizeAndDeliver
// calls and forwards the rest, modeling a transiently or permanently
// unavailable store behind the manager.
type flakyTerminalStore struct {
	Store
	mu       sync.Mutex
	failLeft int
	calls    int
}

func (f *flakyTerminalStore) TerminalizeAndDeliver(ctx context.Context, id string, runGeneration uint64, u TerminalUpdate) (*Task, bool, error) {
	f.mu.Lock()
	f.calls++
	fail := f.failLeft > 0
	if fail {
		f.failLeft--
	}
	f.mu.Unlock()
	if fail {
		return nil, false, errTerminalBoom
	}
	return f.Store.TerminalizeAndDeliver(ctx, id, runGeneration, u)
}

func (f *flakyTerminalStore) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// allow makes every later terminalization succeed.
func (f *flakyTerminalStore) allow() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failLeft = 0
}

func newFlakyManager(t *testing.T, limits Limits, failLeft int) (*Manager, *flakyTerminalStore, *MemoryStore) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	mem := NewMemoryStore()
	flaky := &flakyTerminalStore{Store: mem, failLeft: failLeft}
	m := New(ctx, Config{WorkspaceID: "ws", Limits: limits, Store: flaky})
	m.terminalRetryBaseDelay = 10 * time.Millisecond
	m.terminalRetryMaxDelay = 40 * time.Millisecond
	return m, flaky, mem
}

func terminalBookkeeping(t *testing.T, m *Manager) (live int, liveTotal, running int) {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.live), m.liveTotal, m.running[CapacityKey{WorkspaceID: "ws", Provider: "p", Model: "m"}]
}

// TestManager_TransientTerminalizeFailureReconcilesViaRetry pins the
// recovery path: a terminalization whose store transaction fails
// transiently is retried by the bounded chain, and when a retry
// commits, m.live, quota, and the running slot are released exactly
// once, exactly one terminal event and inbox row are delivered, and
// the runner never re-runs.
func TestManager_TransientTerminalizeFailureReconcilesViaRetry(t *testing.T) {
	t.Parallel()
	// Fail the settle attempt and the first retry; the second retry commits.
	m, flaky, mem := newFlakyManager(t, Limits{LiveTasksPerParent: 1, RunningPerModel: 1}, 2)
	r := record(m)
	var runs atomic.Int32

	task, err := m.Start(t.Context(), StartRequest{
		CallerSessionID: "owner", Prompt: "work", Provider: "p", Model: "m",
		ChildSessionID: uniqueChild(),
		Run: func(context.Context, *Handle) (Result, error) {
			runs.Add(1)
			return Result{Text: "done"}, nil
		},
	})
	require.NoError(t, err)

	got := waitStatus(t, m, "owner", task.ID, StatusCompleted)
	require.Equal(t, "done", got.Result)

	live, liveTotal, running := terminalBookkeeping(t, m)
	require.Zero(t, live, "m.live must clear on the committed retry")
	require.Zero(t, liveTotal, "quota must release exactly once")
	require.Zero(t, running, "the running slot must release exactly once")
	require.Equal(t, int32(1), runs.Load(), "retries must never re-run the runner")
	// Settle plus two failed retries plus the committing third call.
	require.Eventually(t, func() bool {
		return flaky.callCount() == 3
	}, 5*time.Second, 5*time.Millisecond)
	require.Eventually(t, func() bool {
		return countTerminal(r, task.ID) == 1
	}, 5*time.Second, 5*time.Millisecond)
	time.Sleep(50 * time.Millisecond)
	require.Equal(t, 3, flaky.callCount(), "the chain stops once a retry commits")
	require.Equal(t, 1, countTerminal(r, task.ID),
		"exactly one terminal event for the task")

	in, err := mem.ListInbox(t.Context(), "owner")
	require.NoError(t, err)
	require.Len(t, in, 1, "exactly one durable inbox row for the terminalization")

	// The freed quota admits the next task: the leak is reconciled.
	next, err := m.Start(t.Context(), StartRequest{
		CallerSessionID: "owner", Prompt: "next", Provider: "p", Model: "m",
		ChildSessionID: uniqueChild(), Run: nopRun("next done"),
	})
	require.NoError(t, err)
	waitStatus(t, m, "owner", next.ID, StatusCompleted)
}

// countTerminal counts terminal-status events recorded for one task.
func countTerminal(r *recorder, id string) int {
	n := 0
	for _, e := range r.taskEvents(id) {
		if e.Task.Status.Terminal() {
			n++
		}
	}
	return n
}

// TestManager_PermanentTerminalizeFailureIsBoundedAndShutdownReconciles
// pins the degraded path: while the store keeps rejecting the
// terminalization, the retry chain stops after its budget (no
// unbounded polling), nothing is speculatively released, the record
// stays non-terminal, and no event or inbox row is produced. The
// attempt stays in m.live; when the store recovers, Shutdown's
// interruptRemaining terminalizes it durably, exactly once.
func TestManager_PermanentTerminalizeFailureIsBoundedAndShutdownReconciles(t *testing.T) {
	t.Parallel()
	m, flaky, mem := newFlakyManager(t, Limits{}, math.MaxInt)
	m.terminalRetryAttempts = 3
	r := record(m)
	var runs atomic.Int32

	task, err := m.Start(t.Context(), StartRequest{
		CallerSessionID: "owner", Prompt: "work", Provider: "p", Model: "m",
		ChildSessionID: uniqueChild(),
		Run: func(context.Context, *Handle) (Result, error) {
			runs.Add(1)
			return Result{Text: "done"}, nil
		},
	})
	require.NoError(t, err)

	// One settle attempt plus exactly the bounded retries, then
	// silence: the exhausted chain must not keep polling.
	require.Eventually(t, func() bool {
		return flaky.callCount() == 1+m.terminalRetryAttempts
	}, 5*time.Second, 5*time.Millisecond)
	time.Sleep(200 * time.Millisecond)
	require.Equal(t, 1+m.terminalRetryAttempts, flaky.callCount(),
		"the retry budget must be bounded")

	live, liveTotal, running := terminalBookkeeping(t, m)
	require.Equal(t, 1, live, "the uncommitted attempt must stay in m.live")
	require.Equal(t, 1, liveTotal, "quota must not be speculatively released")
	require.Equal(t, 1, running, "the running slot must stay held until a durable commit")
	require.Equal(t, int32(1), runs.Load(), "the runner must never re-run")

	got, err := m.Status(t.Context(), "owner", task.ID)
	require.NoError(t, err)
	require.Equal(t, StatusRunning, got.Status,
		"no terminal record may surface without a durable commit")
	require.Zero(t, countTerminal(r, task.ID), "no terminal event before the commit")
	in, err := mem.ListInbox(t.Context(), "owner")
	require.NoError(t, err)
	require.Empty(t, in, "no inbox row before the commit")

	// The store recovers; shutdown is the final durable reconciler.
	flaky.allow()
	require.NoError(t, m.Shutdown(context.Background()))

	stored, err := mem.Get(t.Context(), task.ID)
	require.NoError(t, err)
	require.Equal(t, StatusInterrupted, stored.Status)
	require.Equal(t, 1, countTerminal(r, task.ID), "exactly one terminal event on shutdown")
	live, liveTotal, running = terminalBookkeeping(t, m)
	require.Zero(t, live)
	require.Zero(t, liveTotal)
	require.Zero(t, running)
	in, err = mem.ListInbox(t.Context(), "owner")
	require.NoError(t, err)
	require.Len(t, in, 1, "the shutdown terminalization delivers exactly one inbox row")
}
