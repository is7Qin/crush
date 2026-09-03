package task

// MemoryStore half of the task-question join transactions: a simple
// in-memory mirror of the agent_task_questions rows so both Store
// implementations honor one contract. Every method validates the
// full fence before mutating, mirroring the SQLite transactions in
// store_sqlite_question.go.

import (
	"context"
	"time"
)

// questionPending and questionInterrupted are the resolution names
// the task core recognizes on question rows; see question.go.

// memoryQuestion is one mirrored question row. Status uses the same
// names as the durable layer ('pending' until resolved).
type memoryQuestion struct {
	QuestionRow
	Status     string
	Answers    string
	ResolvedAt time.Time
}

// BeginQuestionWait inserts the pending question row and moves its
// running task to waiting_for_input under the store lock, rejecting
// the call wholesale when the fence does not hold.
func (s *MemoryStore) BeginQuestionWait(_ context.Context, row QuestionRow) (*Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[row.TaskID]
	if !ok {
		return nil, ErrNotFound
	}
	identity := QuestionOutcome{
		TaskID:         row.TaskID,
		OwnerSessionID: row.OwnerSessionID,
		ChildSessionID: row.ChildSessionID,
		RunGeneration:  row.RunGeneration,
	}
	if t.Status != StatusRunning || !identity.identityFencesMatch(t) {
		return nil, ErrInvalidTransition
	}
	if s.pendingQuestionForLocked(row.TaskID) {
		return nil, ErrQuestionStale
	}
	if _, dup := s.questions[row.QuestionID]; dup {
		return nil, ErrQuestionStale
	}
	s.questions[row.QuestionID] = &memoryQuestion{
		QuestionRow: row,
		Status:      questionPending,
	}
	t.Status = StatusWaitingForInput
	t.UpdatedAt = row.CreatedAt
	return t.clone(), nil
}

// ResolveQuestionAnswered applies the answered transition and the
// still-waiting task check under the store lock.
func (s *MemoryStore) ResolveQuestionAnswered(_ context.Context, out QuestionOutcome) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	q, ok := s.questions[out.QuestionID]
	if !ok {
		return ErrNotFound
	}
	t, ok := s.tasks[out.TaskID]
	if !ok {
		return ErrNotFound
	}
	if q.Status != questionPending || !out.identityFencesMatch(t) || t.Status != StatusWaitingForInput {
		return ErrQuestionStale
	}
	q.Status = out.Resolution
	q.Answers = out.Answers
	q.ResolvedAt = out.ResolvedAt
	return nil
}

// QuestionStatus returns the stored resolution name of a question.
func (s *MemoryStore) QuestionStatus(_ context.Context, questionID string) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	q, ok := s.questions[questionID]
	if !ok {
		return "", ErrNotFound
	}
	return q.Status, nil
}

// fenceQuestionLocked verifies u.Question can transition the bound
// task's pending question row before the terminal mutation. Callers
// must hold s.mu.
func (s *MemoryStore) fenceQuestionLocked(t *Task, u QuestionOutcome) error {
	q, ok := s.questions[u.QuestionID]
	if !ok {
		return ErrNotFound
	}
	if q.Status != questionPending || !u.identityFencesMatch(t) {
		return ErrQuestionStale
	}
	return nil
}

// resolveQuestionsLocked applies the terminal transaction's question
// half: the fenced outcome (when the caller supplied one) stamps the
// exact resolution, then every row still pending for the attempt
// resolves as interrupted. Callers must hold s.mu.
func (s *MemoryStore) resolveQuestionsLocked(t *Task, u TerminalUpdate) {
	if u.Question != nil {
		if q, ok := s.questions[u.Question.QuestionID]; ok {
			q.Status = u.Question.Resolution
			q.Answers = u.Question.Answers
			q.ResolvedAt = u.Question.ResolvedAt
		}
	}
	s.interruptPendingQuestionsLocked(t.ID, t.RunGeneration, u.CompletedAt)
}

// interruptPendingQuestionsLocked resolves every still-pending
// question for one attempt as interrupted. Callers must hold s.mu.
func (s *MemoryStore) interruptPendingQuestionsLocked(taskID string, runGeneration uint64, at time.Time) {
	for _, q := range s.questions {
		if q.TaskID == taskID && q.RunGeneration == runGeneration && q.Status == questionPending {
			q.Status = questionInterrupted
			q.ResolvedAt = at
		}
	}
}

// pendingQuestionForLocked reports whether taskID already has a
// pending question row. Callers must hold s.mu.
func (s *MemoryStore) pendingQuestionForLocked(taskID string) bool {
	for _, q := range s.questions {
		if q.TaskID == taskID && q.Status == questionPending {
			return true
		}
	}
	return false
}
