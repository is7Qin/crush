package task

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
)

// AppendChildMessage queues a message under the addressed task's
// child session with the next allocated sequence.
func (s *MemoryStore) AppendChildMessage(_ context.Context, msg ChildMessage) (ChildMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if msg.Prompt == "" {
		return ChildMessage{}, fmt.Errorf("append child message: %w: empty prompt", ErrInvalidRequest)
	}
	t, ok := s.tasks[msg.TaskID]
	if !ok {
		return ChildMessage{}, ErrNotFound
	}
	msg.ChildSessionID = t.ChildSessionID
	msg.OwnerSessionID = t.OwnerSessionID
	if msg.ID == "" {
		msg.ID = uuid.NewString()
	}
	if msg.Attachments == "" {
		msg.Attachments = "[]"
	}
	if msg.CreatedAt.IsZero() {
		msg.CreatedAt = time.Now()
	}
	msg.State = MessageQueued
	msg.Reason = ""
	msg.DeliveredAt = time.Time{}
	msg.Sequence = s.nextSequenceLocked(msg.ChildSessionID)
	s.mailbox[msg.ChildSessionID] = append(s.mailbox[msg.ChildSessionID], msg.clone())
	return msg, nil
}

// DispatchNextChildMessage claims the lowest queued message and
// promotes the bound pending attempt or inserts its successor,
// matching the SQLite transaction's semantics.
func (s *MemoryStore) DispatchNextChildMessage(_ context.Context, childSessionID string) (*Task, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if childSessionID == "" {
		return nil, false, fmt.Errorf("dispatch: %w: child session required", ErrInvalidRequest)
	}
	msg := s.lowestQueuedLocked(childSessionID)
	if msg == nil {
		return nil, false, nil
	}
	cur := s.currentAttemptLocked(childSessionID)
	if cur == nil || (!cur.Status.Terminal() && cur.Status != StatusPending) {
		// No attempt (orphan) or a running/waiting attempt owns the
		// child turn: messages wait for the next boundary.
		return nil, false, nil
	}

	now := time.Now()
	var dispatched *Task
	if cur.Status == StatusPending {
		next := cur.clone()
		next.Status = StatusRunning
		if next.StartedAt.IsZero() {
			next.StartedAt = now
		}
		// The claimed message is the attempt's input, FIFO first.
		next.Prompt = msg.Prompt
		next.MessageID = msg.ID
		next.UpdatedAt = now
		s.tasks[next.ID] = next
		dispatched = next
	} else {
		next := *cur
		next.ID = uuid.NewString()
		next.ChildSessionID = cur.ChildSessionID
		next.Prompt = msg.Prompt
		next.MessageID = msg.ID
		next.ResumesTaskID = cur.ID
		next.RunGeneration = cur.RunGeneration + 1
		next.Status = StatusRunning
		next.Result, next.Summary, next.Err = "", "", ""
		next.ResultTruncated = false
		next.TerminalGeneration, next.CostAggregatedGeneration = 0, 0
		next.PromptTokens, next.CompletionTokens, next.Cost = 0, 0, 0
		next.CreatedAt, next.StartedAt, next.CompletedAt = now, now, time.Time{}
		next.UpdatedAt = now
		next.FallbackModels = slices.Clone(cur.FallbackModels)
		if s.generationTakenLocked(childSessionID, next.RunGeneration) {
			return nil, false, fmt.Errorf("dispatch %s: generation %d already exists", childSessionID, next.RunGeneration)
		}
		stored := next.clone()
		s.tasks[next.ID] = stored
		dispatched = stored
	}

	msg.State = MessageDelivered
	msg.DeliveredAt = now
	if err := s.appendOutboxLocked(dispatched, EventStarted, now); err != nil {
		return nil, false, err
	}
	return dispatched.clone(), true, nil
}

// CancelPendingIfLive terminalizes a pending attempt, delivers its
// terminal records, and rejects its bound admission message. Higher
// queued sequences are unchanged.
func (s *MemoryStore) CancelPendingIfLive(_ context.Context, id string, u TerminalUpdate, reason string) (*Task, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[id]
	if !ok {
		return nil, false, ErrNotFound
	}
	if t.Status != StatusPending {
		return t.clone(), false, nil
	}
	s.deliverLocked(t, u)
	for _, m := range s.mailbox[t.ChildSessionID] {
		if m.ID == t.MessageID && m.State == MessageQueued && m.DeliveredAt.IsZero() {
			m.State = MessageRejected
			m.Reason = reason
		}
	}
	return t.clone(), true, nil
}

// ListChildMessages returns copies of the child session's mailbox
// rows in sequence order.
func (s *MemoryStore) ListChildMessages(_ context.Context, childSessionID string) ([]ChildMessage, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]ChildMessage, 0, len(s.mailbox[childSessionID]))
	for _, m := range s.mailbox[childSessionID] {
		out = append(out, *m.clone())
	}
	slices.SortFunc(out, func(a, b ChildMessage) int {
		if a.Sequence < b.Sequence {
			return -1
		}
		if a.Sequence > b.Sequence {
			return 1
		}
		return 0
	})
	return out, nil
}
