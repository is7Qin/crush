package task

import (
	"context"
	"sync"
	"sync/atomic"
)

// childState is the manager-side binding for one child conversation:
// the runner shared by every attempt plus the capacity identity of
// the current policy. A continuation replaces the runner and key
// with the re-resolved ones.
type childState struct {
	id              string
	parentSessionID string
	key             CapacityKey
	run             Runner
	wish            *queueItem // queued slot request, if any
	live            *attemptState
}

// attemptState is the runtime bookkeeping for one live task attempt.
// It is guarded by Manager.mu except where noted.
type attemptState struct {
	id              string
	child           *childState
	prompt          string
	slotHeld        bool
	stop            chan struct{} // closed on terminalization
	done            chan struct{} // closed when the attempt settles
	doneOnce        sync.Once
	started         bool          // runner goroutine launched
	slot            chan struct{} // buffered 1: resume slot granted
	runCtx          context.Context
	cancel          context.CancelFunc
	handle          *Handle
	runGeneration   uint64
	cancelRequested atomic.Bool
	// fence is this attempt's tool admission gate: child tool calls
	// hold shared admission for their invocation, terminalization
	// takes exclusive admission before the durable transition.
	fence *Fence
	// terminalRetryArmed marks that the bounded retry chain for a
	// failed terminalization transaction is already running for this
	// attempt. Guarded by Manager.mu.
	terminalRetryArmed bool
}

// queueItem is one FIFO entry on a capacity key: a start wish (a
// child session waiting to deliver its next queued message, with an
// optional pre-existing pending attempt) or a resume (a waiting
// attempt reacquiring a running slot).
type queueItem struct {
	child  *childState
	at     *attemptState // non-nil for a pending-admission start
	resume bool          // slot reacquire for waiting_for_input
}

func (at *attemptState) closeDone() { at.doneOnce.Do(func() { close(at.done) }) }

// bindChildLocked registers (or rebinds, for a continuation) the
// runner and capacity identity for the attempt's child session.
func (m *Manager) bindChildLocked(t *Task, parent string, run Runner) *childState {
	child, ok := m.children[t.ChildSessionID]
	if !ok {
		child = &childState{id: t.ChildSessionID}
		m.children[t.ChildSessionID] = child
	}
	child.parentSessionID = parent
	child.key = CapacityKey{WorkspaceID: m.workspaceID, Provider: t.Provider, Model: t.Model}
	child.run = run
	return child
}

// newAttemptLocked creates the runtime state for a committed task
// record and marks it as the child session's live attempt.
func (m *Manager) newAttemptLocked(t *Task, child *childState) *attemptState {
	runCtx, cancel := context.WithCancel(m.ctx)
	at := &attemptState{
		id:            t.ID,
		child:         child,
		prompt:        t.Prompt,
		stop:          make(chan struct{}),
		done:          make(chan struct{}),
		slot:          make(chan struct{}, 1),
		runCtx:        runCtx,
		cancel:        cancel,
		runGeneration: t.RunGeneration,
		fence:         NewFence(),
	}
	at.handle = &Handle{m: m, at: at}
	child.live = at
	return at
}
