package task

import (
	"context"
	"log/slog"
	"time"
)

// fenceDrainBudget bounds the wait for in-flight child tool calls to
// release shared admission before a terminalization commits. Matches
// the shutdown grace for unsettled runners.
const fenceDrainBudget = 5 * time.Second

// finish applies an exactly-once terminalization. Only the winning
// call releases capacity, frees quota, and emits the terminal event;
// losers observe the committed state unchanged. Exclusive admission
// runs before any state change: shared admission closes, in-flight
// child tool calls drain, and a late runner cannot admit another tool.
// A store transaction error is reported to the caller and arms the
// attempt's bounded retry chain (see retryTerminalize): nothing is
// released and no bookkeeping changes until a retry commits.
func (m *Manager) finish(ctx context.Context, at *attemptState, u TerminalUpdate) (*Task, bool, error) {
	fenceCtx, cancel := context.WithTimeout(ctx, fenceDrainBudget)
	defer cancel()
	if err := at.fence.Fence(fenceCtx); err != nil {
		slog.Warn("In-flight tool calls did not drain before terminalization",
			"task_id", at.id, "error", err)
	}
	m.mu.Lock()
	t, won, events, err := m.finishLocked(ctx, at, u)
	m.mu.Unlock()
	m.events.publish(events...)
	return t, won, err
}

// finishLocked is the locked half of finish. It returns the stored
// record, whether this call won, events to publish after unlock, and
// the store transaction error, if any. Manager bookkeeping is
// released only after the store reports the durable terminal commit:
// the conditional update, parent cost aggregation, outbox row, and
// inbox row are one transaction, so a store error means nothing was
// delivered and nothing is released. A failed terminalization keeps
// the attempt's quota, slot, and m.live entry and arms the retry
// chain to re-apply the same update.
func (m *Manager) finishLocked(ctx context.Context, at *attemptState, u TerminalUpdate) (*Task, bool, []Event, error) {
	t, won, err := m.store.TerminalizeAndDeliver(ctx, at.id, at.runGeneration, u)
	if err != nil {
		slog.Warn("Failed to terminalize task", "task_id", at.id, "error", err)
		m.scheduleTerminalRetryLocked(at, u)
		return t, false, nil, err
	}
	if !won {
		return t, false, nil, nil
	}
	return t, true, m.terminalizedLocked(ctx, t, at), nil
}

// terminalizedLocked owns the bookkeeping of a committed terminal
// transition: quota/slot release, the terminal event, and the
// follow-up dispatch of any queued messages for the child session.
// The durable terminal records (task row, outbox row, inbox row,
// parent cost) were committed by the store transaction before this
// runs. It must run exactly once per winning transition.
func (m *Manager) terminalizedLocked(ctx context.Context, t *Task, at *attemptState) []Event {
	events := m.releaseLocked(at)
	if at.child.live == at {
		at.child.live = nil
	}
	delete(m.live, at.id)
	if !at.started {
		at.closeDone()
	}
	close(at.stop)
	if at.cancel != nil {
		at.cancel()
	}
	events = append(events, eventForTransition(t, StatusPending))
	events = append(events, m.followUpLocked(ctx, at.child)...)
	return events
}

// releaseLocked frees live quota and, if held, the running slot, then
// pumps the freed slot to the next queued entry.
func (m *Manager) releaseLocked(at *attemptState) []Event {
	m.releaseQuotaLocked(at.child.parentSessionID)
	if at.slotHeld {
		at.slotHeld = false
		m.running[at.child.key]--
		return m.pumpLocked(m.ctx, at.child.key)
	}
	m.dequeueLocked(at)
	return nil
}

// dequeueLocked drops the queued admission entry of at, if any.
func (m *Manager) dequeueLocked(at *attemptState) {
	child := at.child
	q := m.queue[child.key]
	for i, item := range q {
		if item.at == at {
			q = append(q[:i:i], q[i+1:]...)
			if child.wish == item {
				child.wish = nil
			}
			break
		}
	}
	if len(q) == 0 {
		delete(m.queue, child.key)
	} else {
		m.queue[child.key] = q
	}
}

// queueLocked appends item to its child's capacity queue and marks
// the child as wishing, returning the item for wish bookkeeping.
// Callers must hold m.mu.
func (m *Manager) queueLocked(child *childState, item *queueItem) *queueItem {
	m.queue[child.key] = append(m.queue[child.key], item)
	child.wish = item
	return item
}

// pumpLocked grants free running slots on key to queued entries in
// FIFO order. A start entry delivers the child's next queued
// message; a resume entry transitions its waiting attempt back to
// running. Callers must publish the returned events after releasing
// m.mu.
func (m *Manager) pumpLocked(ctx context.Context, key CapacityKey) []Event {
	var events []Event
	if m.closed {
		return nil
	}
	q := m.queue[key]
	for len(q) > 0 && m.running[key] < m.limits.RunningPerModel {
		item := q[0]
		if item.resume {
			q = q[1:]
			item.child.wish = nil
			t, from, ok := m.transitionLocked(ctx, item.at.id, StatusRunning)
			if !ok {
				continue // attempt terminalized or vanished while queued
			}
			m.running[key]++
			item.at.slotHeld = true
			events = append(events, eventForTransition(t, from))
			item.at.grantResume()
			continue
		}
		_, found, ev, err := m.dispatchLocked(ctx, item.child, item.at)
		if err != nil {
			// Keep the entry at the head and stop: the message
			// stays queued and the next release retries this child
			// first. Dropping it would strand a committed pending
			// attempt with nobody left to dispatch it.
			slog.Error("Failed to dispatch next child message",
				"child_session_id", item.child.id, "error", err)
			break
		}
		q = q[1:]
		item.child.wish = nil
		if !found {
			// Unreachable while admission commits its mailbox row
			// atomically: a pending attempt always owns a queued
			// admission message, and terminal children only wish
			// when messages remain. Log loudly rather than
			// re-terminalize mid-pump (releaseLocked would fight
			// this loop over the queue slice).
			slog.Error("Dispatch found no queued message",
				"child_session_id", item.child.id, "pending", item.at != nil)
			continue
		}
		events = append(events, ev...)
	}
	if len(q) == 0 {
		delete(m.queue, key)
	} else {
		m.queue[key] = q
	}
	return events
}

// dispatchLocked claims one running slot for child and delivers its
// lowest queued message through the store transaction: the bound
// pending attempt (pending) is promoted, or a successor attempt is
// created when the child's current attempt is terminal. It reports
// the dispatched record, whether a message was claimed, and the
// events to publish.
func (m *Manager) dispatchLocked(ctx context.Context, child *childState, pending *attemptState) (*Task, bool, []Event, error) {
	m.running[child.key]++
	t, found, err := m.store.DispatchNextChildMessage(ctx, child.id)
	if err != nil {
		m.running[child.key]--
		return nil, false, nil, err
	}
	if !found {
		m.running[child.key]--
		return nil, false, nil, nil
	}
	var events []Event
	at := pending
	if at == nil {
		at = m.newAttemptLocked(t, child)
		m.live[at.id] = at
		if m.queuedMsgs[child.id] > 0 {
			m.queuedMsgs[child.id]--
		}
		events = append(events, Event{Type: EventCreated, Task: t.clone()})
	} else {
		at.prompt = t.Prompt
		at.runGeneration = t.RunGeneration
	}
	at.slotHeld = true
	events = append(events, eventForTransition(t, StatusPending))
	at.started = true
	go m.runAttempt(at)
	return t, true, events, nil
}

// followUpLocked queues the next attempt for a child session whose
// current attempt just terminalized while messages remain queued.
// The successor reserves live quota first; when quota is exhausted
// the messages stay queued (durable) instead of starting an attempt
// the quota policy forbids.
func (m *Manager) followUpLocked(ctx context.Context, child *childState) []Event {
	if m.closed || child == nil || child.wish != nil || child.live != nil {
		return nil
	}
	if m.queuedMsgs[child.id] <= 0 {
		return nil
	}
	if !m.reserveQuotaLocked(child.parentSessionID) {
		slog.Info("Follow-up attempt deferred: live task quota exhausted",
			"child_session_id", child.id)
		return nil
	}
	item := &queueItem{child: child}
	m.queue[child.key] = append(m.queue[child.key], item)
	child.wish = item
	return m.pumpLocked(ctx, child.key)
}

// transitionLocked moves a stored task to to when the state machine
// allows it. It reports the previous status on success.
func (m *Manager) transitionLocked(ctx context.Context, id string, to Status) (*Task, Status, bool) {
	t, err := m.store.Get(ctx, id)
	if err != nil {
		return nil, "", false
	}
	from := t.Status
	if !from.CanTransitionTo(to) {
		return t, from, false
	}
	t.Status = to
	t.UpdatedAt = time.Now()
	if to == StatusRunning && t.StartedAt.IsZero() {
		t.StartedAt = t.UpdatedAt
	}
	if err := m.store.Save(ctx, t); err != nil {
		return t, from, false
	}
	return t, from, true
}

// Shutdown stops accepting new tasks, cancels every live runner,
// waits for started attempts to settle until ctx expires, and
// terminalizes any task still live as interrupted. Late runner
// results cannot overwrite a committed terminal record or publish a
// second terminal event.
func (m *Manager) Shutdown(ctx context.Context) error {
	m.mu.Lock()
	m.closed = true
	pending := make([]*attemptState, 0, len(m.live))
	started := make(map[string]bool, len(m.live))
	for _, at := range m.live {
		at.cancelRequested.Store(true)
		if at.cancel != nil {
			at.cancel()
		}
		pending = append(pending, at)
		started[at.id] = at.started
	}
	m.mu.Unlock()

	settled := true
	for _, at := range pending {
		if !started[at.id] {
			continue // never dispatched; interruptRemaining settles it
		}
		select {
		case <-at.done:
		case <-ctx.Done():
			settled = false
		}
	}
	m.interruptRemaining(pending)
	if !settled {
		return ctx.Err()
	}
	return nil
}

// interruptRemaining force-terminalizes attempts whose runners never
// settled or were never dispatched. Already-terminal attempts lose
// the conditional update and are left untouched.
func (m *Manager) interruptRemaining(rss []*attemptState) {
	for _, at := range rss {
		m.finish(context.WithoutCancel(m.ctx), at, TerminalUpdate{
			Status:      StatusInterrupted,
			Summary:     "process shutdown",
			CompletedAt: time.Now(),
		})
	}
}
