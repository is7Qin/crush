package task

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// Handle is the runner-facing view of its own attempt. The runner is
// trusted internal code; control-plane authorization applies to
// Status, Output, List, Cancel, and AppendMessage.
type Handle struct {
	m  *Manager
	at *attemptState
}

// TaskID returns the id of the task being run.
func (h *Handle) TaskID() string { return h.at.id }

// ChildSessionID returns the child session bound to the task being
// run. For a continuation or a successor attempt it is the retained
// session resolved by the manager at admission, so a runner never
// has to carry the binding across the Start boundary.
func (h *Handle) ChildSessionID() string { return h.at.child.id }

// RunGeneration returns the run generation of the task being run.
func (h *Handle) RunGeneration() uint64 { return h.at.runGeneration }

// Fence returns the attempt's tool admission gate so the runner can
// stamp it onto the child execution context; every child tool call
// then takes shared admission for its invocation and terminalization
// fences late admission.
func (h *Handle) Fence() *Fence { return h.at.fence }

// Prompt returns the mailbox prompt this attempt was dispatched to
// deliver: the admission prompt for the first attempt, and the
// claimed message text for every successor. The runner executes
// against the child session with exactly this input, so queued
// messages drive successive turns without a second call path.
func (h *Handle) Prompt() string { return h.at.prompt }

// grantResume signals a waiting Resumed caller that its slot arrived.
func (at *attemptState) grantResume() {
	select {
	case at.slot <- struct{}{}:
	default:
	}
}

// Handle returns the runner-facing handle of a live task, and whether
// the task is currently running under this manager. It lets question
// transports drive WaitingForInput/Resumed for the asking task without
// holding the manager's run-state map.
func (m *Manager) Handle(taskID string) (*Handle, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	at, ok := m.live[taskID]
	if !ok {
		return nil, false
	}
	return at.handle, true
}

// WaitingForInput transitions running -> waiting_for_input and
// releases the running-model slot. The task stays live for quota.
func (h *Handle) WaitingForInput(ctx context.Context) error {
	m := h.m
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return ErrShuttingDown
	}
	t, from, ok := m.transitionLocked(ctx, h.at.id, StatusWaitingForInput)
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("%w: %s -> waiting_for_input", ErrInvalidTransition, from)
	}
	if h.at.slotHeld {
		h.at.slotHeld = false
		m.running[h.at.child.key]--
	}
	events := append(m.pumpLocked(ctx, h.at.child.key), eventForTransition(t, from))
	m.mu.Unlock()
	m.events.publish(events...)
	return nil
}

// Resumed reacquires a running-model slot and transitions
// waiting_for_input -> running. It blocks in the per-key FIFO queue
// until capacity, cancellation, or terminalization intervenes.
func (h *Handle) Resumed(ctx context.Context) error {
	m := h.m
	at := h.at
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return ErrShuttingDown
	}
	t, err := m.store.Get(ctx, at.id)
	if err != nil {
		m.mu.Unlock()
		return err
	}
	switch t.Status {
	case StatusRunning:
		m.mu.Unlock()
		return nil
	case StatusWaitingForInput:
	default:
		m.mu.Unlock()
		return fmt.Errorf("%w: %s -> running", ErrInvalidTransition, t.Status)
	}
	child := at.child
	if m.running[child.key] < m.limits.RunningPerModel && child.wish == nil {
		t2, from, ok := m.transitionLocked(ctx, at.id, StatusRunning)
		if !ok {
			m.mu.Unlock()
			return fmt.Errorf("%w: %s -> running", ErrInvalidTransition, from)
		}
		m.running[child.key]++
		at.slotHeld = true
		m.mu.Unlock()
		m.events.publish(eventForTransition(t2, from))
		return nil
	}
	item := &queueItem{child: child, at: at, resume: true}
	m.queue[child.key] = append(m.queue[child.key], item)
	m.mu.Unlock()

	select {
	case <-at.slot:
		if !m.stillLive(at.id) {
			return context.Canceled
		}
		return nil
	case <-at.stop:
		return context.Canceled
	case <-ctx.Done():
		m.mu.Lock()
		select {
		case <-at.slot:
			// Granted concurrently with the cancel; return it.
			if at.slotHeld {
				at.slotHeld = false
				m.running[child.key]--
				m.pumpLocked(ctx, child.key)
			}
		default:
			if child.wish == item {
				child.wish = nil
			}
			q := m.queue[child.key]
			for i, e := range q {
				if e == item {
					q = append(q[:i:i], q[i+1:]...)
					break
				}
			}
			if len(q) == 0 {
				delete(m.queue, child.key)
			} else {
				m.queue[child.key] = q
			}
		}
		m.mu.Unlock()
		return ctx.Err()
	}
}

// Status returns the current snapshot of an owned task.
func (m *Manager) Status(ctx context.Context, callerSessionID, taskID string) (*Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.authorize(ctx, callerSessionID, taskID)
}

// Output returns the bounded stored result of an owned task. The bool
// reports truncation.
func (m *Manager) Output(ctx context.Context, callerSessionID, taskID string) (Result, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, err := m.authorize(ctx, callerSessionID, taskID)
	if err != nil {
		return Result{}, false, err
	}
	return Result{Text: t.Result, Summary: t.Summary}, t.ResultTruncated, nil
}

// List returns the caller's public tasks, oldest first. A non-empty
// parentSessionID must equal the trusted caller or the request is
// rejected. Hidden system-owned tasks (agentic_fetch) never appear:
// the public list is scoped to owner-created call_agent tasks.
//
// Reads run under the manager lock: the terminal writer commits the
// full terminal transaction (task update, outbox row, inbox row) and
// releases quota inside the same section, so a status observed
// through the manager always implies the terminal's durable records
// are already in place.
func (m *Manager) List(ctx context.Context, callerSessionID, parentSessionID string) ([]*Task, error) {
	if parentSessionID != "" && parentSessionID != callerSessionID {
		return nil, ErrNotOwner
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	tasks, err := m.store.ListByOwner(ctx, callerSessionID)
	if err != nil {
		return nil, err
	}
	out := make([]*Task, 0, len(tasks))
	for _, t := range tasks {
		if t.IsHidden() {
			continue
		}
		out = append(out, t)
	}
	return out, nil
}

// Cancel is an owner-authorized, idempotent cancellation by task id.
// A pending task terminalizes without starting and its undelivered
// admission message is rejected with reason task_cancelled; higher
// queued messages stay queued and dispatch as successor attempts.
// Running and waiting tasks have their context cancelled and the call
// waits for the runner to settle before the terminal event is
// published.
func (m *Manager) Cancel(ctx context.Context, callerSessionID, taskID string) error {
	m.mu.Lock()
	t, err := m.store.Get(ctx, taskID)
	if err != nil {
		m.mu.Unlock()
		return err
	}
	if t.IsHidden() {
		m.mu.Unlock()
		return ErrNotFound
	}
	if t.OwnerSessionID != callerSessionID {
		m.mu.Unlock()
		return ErrNotOwner
	}
	at := m.live[taskID]
	if t.Status.Terminal() || at == nil {
		m.mu.Unlock()
		return nil
	}
	if t.Status == StatusPending {
		u := TerminalUpdate{
			Status:      StatusCancelled,
			Summary:     "cancelled before start",
			CompletedAt: time.Now(),
		}
		saved, won, err := m.store.CancelPendingIfLive(ctx, taskID, u, ReasonTaskCancelled)
		if err != nil {
			m.mu.Unlock()
			return err
		}
		if won {
			// Exclusive admission before the durable transition. A
			// pending attempt never started its runner, so nothing
			// holds shared admission and this returns immediately.
			if err := at.fence.Fence(ctx); err != nil {
				slog.Warn("In-flight tool calls did not drain before terminalization",
					"task_id", taskID, "error", err)
			}
			events := m.terminalizedLocked(ctx, saved, at)
			m.mu.Unlock()
			m.events.publish(events...)
			return nil
		}
		// Lost the promotion race: the attempt is already running (or
		// terminal) and falls through to the runner path below.
		t = saved
	}
	at.cancelRequested.Store(true)
	if t.Status.Terminal() {
		m.mu.Unlock()
		return nil
	}
	at.cancel()
	m.mu.Unlock()

	select {
	case <-at.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// authorize loads a task and enforces owner-session equality. Hidden
// system-owned tasks are not publicly addressable at all: the check
// runs before the owner comparison so a hidden task reports
// ErrNotFound to every caller, its owner included. Only
// DiagnosticTask reads one.
func (m *Manager) authorize(ctx context.Context, callerSessionID, taskID string) (*Task, error) {
	t, err := m.store.Get(ctx, taskID)
	if err != nil {
		return nil, err
	}
	if t.IsHidden() {
		return nil, ErrNotFound
	}
	if t.OwnerSessionID != callerSessionID {
		return nil, ErrNotOwner
	}
	return t, nil
}

// DiagnosticTask is the internal diagnostics read for hidden
// system-owned tasks, which every public control path reports as
// not-found. It requires the trusted originating workspace identity
// (the manager's own workspace) and the task's owner session to
// match; a mismatched workspace or owner reports not-found/not-owner
// exactly like the public paths. Production callers pass the
// workspace-resident identity they already hold; nothing on the
// public wire can name a hidden task.
func (m *Manager) DiagnosticTask(ctx context.Context, workspaceID, ownerSessionID, taskID string) (*Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if workspaceID == "" || workspaceID != m.workspaceID {
		return nil, ErrNotFound
	}
	t, err := m.store.Get(ctx, taskID)
	if err != nil {
		return nil, err
	}
	if t.OwnerSessionID != ownerSessionID {
		return nil, ErrNotOwner
	}
	return t, nil
}

// stillLive reports whether the task is still tracked as live.
func (m *Manager) stillLive(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.live[id]
	return ok
}
