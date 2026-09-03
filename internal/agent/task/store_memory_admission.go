package task

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
)

// CreatePendingTask admits one attempt under the store lock: every
// validation runs before any mutation, so a rejected admission
// leaves no task, child session, or mailbox row behind, mirroring
// the SQLite transaction's all-or-nothing behavior.
func (s *MemoryStore) CreatePendingTask(_ context.Context, adm Admission) (*Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	t := *adm.Task
	t.FallbackModels = slices.Clone(adm.Task.FallbackModels)
	if _, dup := s.tasks[t.ID]; dup {
		return nil, fmt.Errorf("admit task %s: duplicate task id", t.ID)
	}
	if t.Status != StatusPending {
		return nil, fmt.Errorf("admit task %s: %w: admission must be pending", t.ID, ErrInvalidRequest)
	}
	if t.ResumesTaskID != "" {
		prev, ok := s.tasks[t.ResumesTaskID]
		if !ok {
			return nil, ErrNotFound
		}
		if prev.IsHidden() {
			// Hidden tasks are not continuable through the public
			// path; mirror the SQLite store's not-found denial.
			return nil, ErrNotFound
		}
		if prev.OwnerSessionID != t.OwnerSessionID {
			return nil, ErrNotOwner
		}
		if !prev.Status.Terminal() {
			return nil, ErrResumeLive
		}
		cur := s.currentAttemptLocked(prev.ChildSessionID)
		if cur == nil || !cur.Status.Terminal() {
			return nil, ErrResumeLive
		}
		t.ChildSessionID = prev.ChildSessionID
		t.ResumesTaskID = cur.ID
		t.RunGeneration = cur.RunGeneration + 1
	}
	if adm.Child.New {
		if adm.Child.ID == "" {
			return nil, fmt.Errorf("admit task %s: %w: fresh delegation needs a child session id", t.ID, ErrInvalidRequest)
		}
		if _, exists := s.children[adm.Child.ID]; exists {
			return nil, fmt.Errorf("admit task %s: child session %s already exists", t.ID, adm.Child.ID)
		}
		if t.ChildSessionID != "" && t.ChildSessionID != adm.Child.ID {
			return nil, fmt.Errorf("admit task %s: %w: child binding mismatch", t.ID, ErrInvalidRequest)
		}
		t.ChildSessionID = adm.Child.ID
	}
	if t.ChildSessionID == "" {
		return nil, fmt.Errorf("admit task %s: %w", t.ID, ErrInvalidRequest)
	}
	if s.generationTakenLocked(t.ChildSessionID, t.RunGeneration) {
		return nil, fmt.Errorf("admit task %s: generation %d already exists for child %s",
			t.ID, t.RunGeneration, t.ChildSessionID)
	}

	if adm.Child.New {
		s.children[adm.Child.ID] = memoryChild{
			parentSessionID: adm.Child.ParentID,
			title:           adm.Child.Title,
		}
	}
	msg := admissionMessage(&t)
	msg.Sequence = s.nextSequenceLocked(t.ChildSessionID)
	t.MessageID = msg.ID
	stored := t.clone()
	s.tasks[t.ID] = stored
	s.mailbox[t.ChildSessionID] = append(s.mailbox[t.ChildSessionID], msg.clone())
	return stored.clone(), nil
}

// ChildSessionExists reports whether an admission inserted the child
// session binding. Test seam for admission rollback visibility.
func (s *MemoryStore) ChildSessionExists(id string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.children[id]
	return ok
}

// currentAttemptLocked returns the child session's highest
// run-generation task, or nil when the child has no attempts. Callers
// must hold s.mu.
func (s *MemoryStore) currentAttemptLocked(childSessionID string) *Task {
	var best *Task
	for _, t := range s.tasks {
		if t.ChildSessionID != childSessionID {
			continue
		}
		if best == nil || t.RunGeneration > best.RunGeneration {
			best = t
		}
	}
	return best
}

func (s *MemoryStore) generationTakenLocked(childSessionID string, gen uint64) bool {
	for _, t := range s.tasks {
		if t.ChildSessionID == childSessionID && t.RunGeneration == gen {
			return true
		}
	}
	return false
}

func (s *MemoryStore) nextSequenceLocked(childSessionID string) uint64 {
	var next uint64
	for _, m := range s.mailbox[childSessionID] {
		if m.Sequence >= next {
			next = m.Sequence + 1
		}
	}
	return next
}

func (s *MemoryStore) lowestQueuedLocked(childSessionID string) *ChildMessage {
	var best *ChildMessage
	for _, m := range s.mailbox[childSessionID] {
		if m.State != MessageQueued {
			continue
		}
		if best == nil || m.Sequence < best.Sequence {
			best = m
		}
	}
	return best
}

// appendOutboxLocked records a lifecycle entry, idempotent per
// transition fact. Callers must hold s.mu.
func (s *MemoryStore) appendOutboxLocked(t *Task, eventType EventType, at time.Time) error {
	payload, err := json.Marshal(t)
	if err != nil {
		return fmt.Errorf("encode dispatched task %s: %w", t.ID, err)
	}
	s.appendOutboxUnlocked(&OutboxEntry{
		ID:            uuid.NewString(),
		TaskID:        t.ID,
		RunGeneration: t.RunGeneration,
		EventType:     eventType,
		Payload:       string(payload),
		CreatedAt:     at,
	})
	return nil
}
