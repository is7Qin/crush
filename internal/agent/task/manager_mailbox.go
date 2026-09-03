package task

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// MessageRequest is the trusted input of one direct child message.
// Identity comes from the caller boundary (owner session), never
// from model or wire input; the child session is derived from the
// addressed task.
type MessageRequest struct {
	OwnerSessionID string
	TaskID         string
	Origin         MessageOrigin
	Prompt         string
	// Attachments is JSON array text of the wire attachment
	// objects; empty stores as "[]".
	Attachments string
}

// MessageAccepted reports the committed mailbox row. AttemptTaskID
// names the successor attempt created immediately for a message
// appended to a terminal task; it is empty while the task is
// pending, running, or waiting for input, and when capacity or quota
// deferred the successor.
type MessageAccepted struct {
	TaskID         string `json:"task_id"`
	ChildSessionID string `json:"child_session_id"`
	Sequence       uint64 `json:"sequence"`
	AttemptTaskID  string `json:"attempt_task_id,omitempty"`
	Status         Status `json:"status"`
}

// AppendMessage queues one direct message for a child conversation.
// Messages may be appended to tasks in pending, running,
// waiting_for_input, or terminal states; they never answer a typed
// question and never resume a waiting attempt. For a terminal task
// the manager attempts to create the successor attempt immediately:
// the store transaction claims the lowest queued message (FIFO, so
// not necessarily this one) and only a freshly created attempt is
// reported as AttemptTaskID.
func (m *Manager) AppendMessage(ctx context.Context, req MessageRequest) (MessageAccepted, error) {
	switch {
	case req.Prompt == "":
		return MessageAccepted{}, fmt.Errorf("%w: empty prompt", ErrInvalidRequest)
	case req.TaskID == "" || req.OwnerSessionID == "":
		return MessageAccepted{}, fmt.Errorf("%w: task and owner are required", ErrInvalidRequest)
	case req.Origin != OriginParent && req.Origin != OriginUser:
		return MessageAccepted{}, fmt.Errorf("%w: unknown message origin %q", ErrInvalidRequest, req.Origin)
	}

	m.mu.Lock()
	t, err := m.store.Get(ctx, req.TaskID)
	if err != nil {
		m.mu.Unlock()
		return MessageAccepted{}, err
	}
	if t.IsHidden() {
		m.mu.Unlock()
		return MessageAccepted{}, ErrNotFound
	}
	if t.OwnerSessionID != req.OwnerSessionID {
		m.mu.Unlock()
		return MessageAccepted{}, ErrNotOwner
	}
	msg, err := m.store.AppendChildMessage(ctx, ChildMessage{
		ID:          uuid.NewString(),
		TaskID:      req.TaskID,
		Origin:      req.Origin,
		Prompt:      req.Prompt,
		Attachments: req.Attachments,
	})
	if err != nil {
		m.mu.Unlock()
		return MessageAccepted{}, err
	}
	m.queuedMsgs[msg.ChildSessionID]++
	accepted := MessageAccepted{
		TaskID:         t.ID,
		ChildSessionID: msg.ChildSessionID,
		Sequence:       msg.Sequence,
		Status:         t.Status,
	}

	var events []Event
	if t.Status.Terminal() {
		events = m.scheduleFollowUpLocked(ctx, msg.ChildSessionID, msg.ID, &accepted.AttemptTaskID)
	}
	m.mu.Unlock()
	m.events.publish(events...)
	return accepted, nil
}

// scheduleFollowUpLocked tries to start the next attempt for a
// terminal child conversation, preferring an immediate dispatch so
// the acceptance can name the created attempt. claimedMessageID,
// when non-empty and matched by the created attempt, is reported
// through attemptOut. Quota is reserved before the attempt exists;
// capacity defers through the normal FIFO pump.
func (m *Manager) scheduleFollowUpLocked(ctx context.Context, childSessionID, claimedMessageID string, attemptOut *string) []Event {
	child, ok := m.children[childSessionID]
	if !ok || m.closed || child.wish != nil || child.live != nil {
		// Another entry owns this child's next attempt, or the child
		// belongs to a previous process whose runner is gone: the
		// message stays queued for the next dispatch point.
		return nil
	}
	if !m.reserveQuotaLocked(child.parentSessionID) {
		return nil
	}
	item := &queueItem{child: child}
	m.queue[child.key] = append(m.queue[child.key], item)
	child.wish = item
	events := m.pumpLocked(ctx, child.key)
	for _, e := range events {
		if e.Type == EventCreated && e.Task != nil && e.Task.MessageID == claimedMessageID {
			*attemptOut = e.Task.ID
		}
	}
	return events
}
