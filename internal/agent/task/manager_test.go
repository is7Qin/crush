package task

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

var childCounter atomic.Uint64

// uniqueChild names a fresh deterministic child session per test
// admission, mirroring the messageID$$toolCallID ids the call_agent
// boundary supplies in production.
func uniqueChild() string {
	return "child-" + strconv.FormatUint(childCounter.Add(1), 10)
}

func newTestManager(t *testing.T, limits Limits) *Manager {
	t.Helper()
	m, _ := newTestManagerStore(t, limits)
	return m
}

// newTestManagerStore builds a MemoryStore-backed manager and returns
// its store for mailbox and child-session assertions.
func newTestManagerStore(t *testing.T, limits Limits) (*Manager, *MemoryStore) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	mem := NewMemoryStore()
	return New(ctx, Config{WorkspaceID: "ws", Limits: limits, Store: mem}), mem
}

// recorder captures lifecycle events in subscription order.
type recorder struct {
	mu     sync.Mutex
	events []Event
}

func record(m *Manager) *recorder {
	r := &recorder{}
	m.Subscribe(func(e Event) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.events = append(r.events, e)
	})
	return r
}

func (r *recorder) types() []EventType {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]EventType, 0, len(r.events))
	for _, e := range r.events {
		out = append(out, e.Type)
	}
	return out
}

func (r *recorder) count(typ EventType) int {
	n := 0
	for _, t := range r.types() {
		if t == typ {
			n++
		}
	}
	return n
}

func (r *recorder) taskEvents(id string) []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Event
	for _, e := range r.events {
		if e.Task != nil && e.Task.ID == id {
			out = append(out, e)
		}
	}
	return out
}

func requireEventually(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	require.Eventually(t, cond, 5*time.Second, 5*time.Millisecond, msg)
}

// waitStatus polls until the task reaches one of the wanted states
// and returns that snapshot.
func waitStatus(t *testing.T, m *Manager, owner, id string, want ...Status) *Task {
	t.Helper()
	require.Eventually(t, func() bool {
		got, err := m.Status(context.Background(), owner, id)
		return err == nil && slices.Contains(want, got.Status)
	}, 5*time.Second, 5*time.Millisecond, "task %s never reached %v", id, want)
	got, err := m.Status(context.Background(), owner, id)
	require.NoError(t, err)
	return got
}

// waitAttemptCount waits until the owner's task list has at least n
// attempts (successor attempts get ids the test cannot predict).
func waitAttemptCount(t *testing.T, m *Manager, owner string, n int) []*Task {
	t.Helper()
	require.Eventually(t, func() bool {
		tasks, err := m.List(context.Background(), owner, "")
		return err == nil && len(tasks) >= n
	}, 5*time.Second, 5*time.Millisecond, "owner %s never reached %d attempts", owner, n)
	tasks, err := m.List(context.Background(), owner, "")
	require.NoError(t, err)
	return tasks
}

func TestManager_StartAdmitsAtomicallyWithoutWaiting(t *testing.T) {
	t.Parallel()
	m, mem := newTestManagerStore(t, Limits{})
	r := record(m)
	release := make(chan struct{})
	defer close(release)
	started := make(chan struct{})

	run := func(ctx context.Context, h *Handle) (Result, error) {
		close(started)
		select {
		case <-release:
			return Result{Text: "late"}, nil
		case <-ctx.Done():
			return Result{}, ctx.Err()
		}
	}
	child := uniqueChild()
	task, err := m.Start(t.Context(), StartRequest{
		CallerSessionID: "owner", Profile: "coder", Provider: "p", Model: "m",
		ChildSessionID: child, ChildTitle: "child", Prompt: "work", Run: run,
	})
	require.NoError(t, err)
	require.NotEmpty(t, task.ID)
	// The acceptance snapshot is the committed pending admission.
	require.Equal(t, StatusPending, task.Status)
	require.Equal(t, child, task.ChildSessionID, "a queued task owns its child session immediately")

	<-started
	// The row is running and its sequence-zero message is delivered.
	got := waitStatus(t, m, "owner", task.ID, StatusRunning, StatusCompleted)
	require.Equal(t, child, got.ChildSessionID)
	require.NotEmpty(t, got.MessageID, "the attempt records the claimed message")
	msgs, err := mem.ListChildMessages(t.Context(), child)
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	require.Equal(t, MessageDelivered, msgs[0].State)
	require.Equal(t, uint64(0), msgs[0].Sequence)
	require.True(t, mem.ChildSessionExists(child))
	require.Eventually(t, func() bool {
		return r.count(EventCreated) == 1 && r.count(EventStarted) == 1
	}, 5*time.Second, 5*time.Millisecond, "created and started events must follow the commits")
}

func TestManager_FailedAdmissionReleasesQuota(t *testing.T) {
	t.Parallel()
	mem := NewMemoryStore()
	fs := &failAdmissionStore{Store: mem, fail: true}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	m := New(ctx, Config{WorkspaceID: "ws", Limits: Limits{LiveTasksPerParent: 1}, Store: fs})

	_, err := m.Start(t.Context(), StartRequest{
		CallerSessionID: "owner", Prompt: "work", Provider: "p", Model: "m",
		ChildSessionID: uniqueChild(),
		Run:            func(context.Context, *Handle) (Result, error) { return Result{}, nil },
	})
	require.ErrorIs(t, err, errAdmissionBoom)

	tasks, err := m.List(t.Context(), "owner", "")
	require.NoError(t, err)
	require.Empty(t, tasks, "a failed admission leaves no task visible")

	// The quota reservation was released: the next admission fits.
	fs.fail = false
	_, err = m.Start(t.Context(), StartRequest{
		CallerSessionID: "owner", Prompt: "work", Provider: "p", Model: "m",
		ChildSessionID: uniqueChild(),
		Run:            func(context.Context, *Handle) (Result, error) { return Result{Text: "ok"}, nil },
	})
	require.NoError(t, err)
}

// errAdmissionBoom is the fixed rejection of failAdmissionStore.
var errAdmissionBoom = errors.New("admission boom")

// failAdmissionStore rejects CreatePendingTask while fail is set.
type failAdmissionStore struct {
	Store
	fail bool
}

func (f *failAdmissionStore) CreatePendingTask(ctx context.Context, adm Admission) (*Task, error) {
	if f.fail {
		return nil, errAdmissionBoom
	}
	return f.Store.CreatePendingTask(ctx, adm)
}

func nopRun(text string) Runner {
	return func(context.Context, *Handle) (Result, error) {
		return Result{Text: text}, nil
	}
}

func TestManager_AsyncRunCompletesAndTerminalizes(t *testing.T) {
	t.Parallel()
	m := newTestManager(t, Limits{})
	r := record(m)

	task, err := m.Start(t.Context(), StartRequest{
		CallerSessionID: "owner", Provider: "p", Model: "m",
		ChildSessionID: "child-1", Prompt: "work", Run: nopRun("answer"),
	})
	require.NoError(t, err)

	got := waitStatus(t, m, "owner", task.ID, StatusCompleted)
	require.Equal(t, "answer", got.Result)
	require.False(t, got.StartedAt.IsZero())
	require.False(t, got.CompletedAt.IsZero())
	require.Equal(t, "child-1", got.ChildSessionID)
	// Events publish after the state commit, so poll for the full
	// sequence rather than assuming same-instant visibility.
	require.Eventually(t, func() bool {
		return slices.Equal([]EventType{EventCreated, EventStarted, EventCompleted}, r.types())
	}, 5*time.Second, 5*time.Millisecond)
}

// TestManager_CancellationWinsOverRunnerSuccess pins the
// terminal-decision rule: once cancellation is requested, a runner
// that settles afterwards terminalizes as cancelled, never as
// completed.
func TestManager_CancellationWinsOverRunnerSuccess(t *testing.T) {
	t.Parallel()
	m := newTestManager(t, Limits{})
	observed := make(chan struct{})
	task, err := m.Start(t.Context(), StartRequest{
		CallerSessionID: "owner", Provider: "p", Model: "m",
		ChildSessionID: uniqueChild(), Prompt: "work",
		Run: func(ctx context.Context, h *Handle) (Result, error) {
			<-ctx.Done()
			close(observed)
			return Result{Text: "too late"}, nil
		},
	})
	require.NoError(t, err)
	require.NoError(t, m.Cancel(t.Context(), "owner", task.ID))
	<-observed

	got := waitStatus(t, m, "owner", task.ID, StatusCancelled)
	require.Equal(t, StatusCancelled, got.Status,
		"cancellation wins the status over a runner that settles with success")
}

func TestManager_RunnerErrorFailsTaskAndReleasesSlot(t *testing.T) {
	t.Parallel()
	m := newTestManager(t, Limits{RunningPerModel: 1})
	boom := errors.New("boom")

	first, err := m.Start(t.Context(), StartRequest{
		CallerSessionID: "owner", Provider: "p", Model: "m",
		ChildSessionID: uniqueChild(), Prompt: "a",
		Run: func(context.Context, *Handle) (Result, error) { return Result{}, boom },
	})
	require.NoError(t, err)
	got := waitStatus(t, m, "owner", first.ID, StatusFailed)
	require.Contains(t, got.Err, "boom")

	// The failed run released its slot: a second task runs, it does
	// not stay pending forever.
	second, err := m.Start(t.Context(), StartRequest{
		CallerSessionID: "owner", Provider: "p", Model: "m",
		ChildSessionID: uniqueChild(), Prompt: "b", Run: nopRun("ok"),
	})
	require.NoError(t, err)
	got = waitStatus(t, m, "owner", second.ID, StatusCompleted)
	require.Equal(t, "ok", got.Result)
}

func TestManager_TerminalizationIsExactlyOnce(t *testing.T) {
	t.Parallel()
	m := newTestManager(t, Limits{})
	r := record(m)

	task, err := m.Start(t.Context(), StartRequest{
		CallerSessionID: "owner", Provider: "p", Model: "m",
		ChildSessionID: uniqueChild(), Prompt: "work", Run: nopRun("done"),
	})
	require.NoError(t, err)
	waitStatus(t, m, "owner", task.ID, StatusCompleted)

	// Cancelling a completed task is an idempotent no-op and cannot
	// publish a second terminal event or rewrite the record.
	require.NoError(t, m.Cancel(t.Context(), "owner", task.ID))
	require.NoError(t, m.Cancel(t.Context(), "owner", task.ID))
	got, err := m.Status(t.Context(), "owner", task.ID)
	require.NoError(t, err)
	require.Equal(t, StatusCompleted, got.Status)
	require.Equal(t, "done", got.Result)
	require.Eventually(t, func() bool {
		return r.count(EventCompleted) == 1 && r.count(EventCancelled) == 0
	}, 5*time.Second, 5*time.Millisecond)
}

func TestManager_ResultIsTruncated(t *testing.T) {
	t.Parallel()
	m := newTestManager(t, Limits{})
	huge := make([]byte, MaxResultBytes+10)
	for i := range huge {
		huge[i] = 'x'
	}
	task, err := m.Start(t.Context(), StartRequest{
		CallerSessionID: "owner", Provider: "p", Model: "m",
		ChildSessionID: uniqueChild(), Prompt: "work",
		Run: func(context.Context, *Handle) (Result, error) {
			return Result{Text: string(huge)}, nil
		},
	})
	require.NoError(t, err)
	got := waitStatus(t, m, "owner", task.ID, StatusCompleted)
	require.True(t, got.ResultTruncated)
	require.Len(t, got.Result, MaxResultBytes)

	out, truncated, err := m.Output(t.Context(), "owner", task.ID)
	require.NoError(t, err)
	require.True(t, truncated)
	require.Len(t, out.Text, MaxResultBytes)
}

func TestManager_StartValidation(t *testing.T) {
	t.Parallel()
	m := newTestManager(t, Limits{})
	nop := nopRun("")

	_, err := m.Start(t.Context(), StartRequest{
		CallerSessionID: "owner", CallerDepth: 1, Prompt: "x", Run: nop,
	})
	require.ErrorIs(t, err, ErrDelegation)

	_, err = m.Start(t.Context(), StartRequest{CallerSessionID: "owner", Run: nop})
	require.ErrorIs(t, err, ErrInvalidRequest)

	_, err = m.Start(t.Context(), StartRequest{Prompt: "x", Run: nop})
	require.ErrorIs(t, err, ErrInvalidRequest)

	_, err = m.Start(t.Context(), StartRequest{CallerSessionID: "owner", Prompt: "x"})
	require.ErrorIs(t, err, ErrInvalidRequest)

	// A fresh delegation without a child session id cannot be bound.
	_, err = m.Start(t.Context(), StartRequest{
		CallerSessionID: "owner", Prompt: "x", Run: nop,
	})
	require.ErrorIs(t, err, ErrInvalidRequest)

	tasks, err := m.List(t.Context(), "owner", "")
	require.NoError(t, err)
	require.Empty(t, tasks, "rejected starts must not create records")
}
