package taskquestion

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/charmbracelet/crush/internal/question"
)

// AnswerTask resolves the question after verifying ownership.
func (s *taskQuestionService) AnswerTask(callerSessionID, questionID string, answers []question.Answer) error {
	return s.control(callerSessionID, questionID, resolution{kind: ResolutionAnswered, answers: answers})
}

// CancelTask resolves the question as cancelled after verifying
// ownership.
func (s *taskQuestionService) CancelTask(callerSessionID, questionID string) error {
	return s.control(callerSessionID, questionID, resolution{kind: ResolutionCancelled})
}

// control authorizes the caller against the tracked record, then
// races the shared conditional resolution.
func (s *taskQuestionService) control(callerSessionID, questionID string, res resolution) error {
	s.mu.Lock()
	w, ok := s.pending[questionID]
	if !ok {
		_, resolved := s.resolved[questionID]
		s.mu.Unlock()
		if resolved {
			return ErrAlreadyResolved
		}
		return ErrNotFound
	}
	owner := w.q.OwnerSessionID
	s.mu.Unlock()
	if owner != callerSessionID {
		return ErrNotOwner
	}
	_, won, err := s.resolve(questionID, res)
	if err != nil {
		return err
	}
	if !won {
		return ErrAlreadyResolved
	}
	return nil
}

// resolve performs the one conditional resolution shared by answer,
// cancel, timeout, runner-context loss, and shutdown. The first
// caller to find the question tracked wins; every later call is a
// no-op loss. The winner commits the durable resolution first —
// through the Lifecycle, atomically with the matching task
// transition — and only then wakes the tracked runner exactly once
// and publishes a notification. A failed durable commit rolls the
// resolution off the winner state: the waiter is restored, the
// runner stays blocked, and the caller receives the persistence
// error for retry.
func (s *taskQuestionService) resolve(questionID string, res resolution) (TaskQuestion, bool, error) {
	s.mu.Lock()
	w, ok := s.pending[questionID]
	if !ok {
		s.mu.Unlock()
		return TaskQuestion{}, false, nil
	}
	delete(s.pending, questionID)
	delete(s.byTask, w.q.TaskID)
	if len(s.resolved) >= resolvedHistory && len(s.resolvedRank) > 0 {
		delete(s.resolved, s.resolvedRank[0])
		s.resolvedRank = s.resolvedRank[1:]
	}
	s.resolved[questionID] = res.kind
	s.resolvedRank = append(s.resolvedRank, questionID)
	q := w.q
	q.Resolution = res.kind
	q.Answers = res.answers
	q.ResolvedAt = time.Now().UTC()
	s.mu.Unlock()

	var err error
	if s.lifecycle != nil {
		u := ResolutionUpdate{Resolution: res.kind, Answers: res.answers, ResolvedAt: q.ResolvedAt}
		if lerr := s.lifecycle.Resolve(context.Background(), q, u); lerr != nil {
			s.restore(questionID, w)
			return q, false, lerr
		}
	} else if s.repo != nil {
		u := ResolutionUpdate{Resolution: res.kind, Answers: res.answers, ResolvedAt: q.ResolvedAt}
		if _, rerr := s.repo.Resolve(context.Background(), questionID, u); rerr != nil {
			err = fmt.Errorf("persist task question resolution: %w", rerr)
		}
	}
	w.outcome <- res
	s.notif.Publish(pubsub.CreatedEvent, Notification{
		QuestionID: questionID,
		TaskID:     q.TaskID,
		BatchID:    q.Batch.ID,
		Resolution: res.kind,
	})
	return q, true, err
}

// restore puts a waiter back after a failed durable resolution so a
// later control call can win again. When Shutdown has begun the
// waiter is dropped instead: its runner unwinds through the
// shutdown context cancellation, and the durable pending row is left
// for a later sweep to resolve.
func (s *taskQuestionService) restore(questionID string, w *waiter) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, taken := s.pending[questionID]; taken {
		return
	}
	if s.closed {
		delete(s.resolved, questionID)
		return
	}
	s.pending[questionID] = w
	s.byTask[w.q.TaskID] = questionID
	delete(s.resolved, questionID)
}

// Pending returns a copy of the unanswered question record.
func (s *taskQuestionService) Pending(questionID string) (TaskQuestion, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w, ok := s.pending[questionID]
	if !ok {
		return TaskQuestion{}, false
	}
	return w.q, true
}

// Unresolved returns copies of all unanswered questions, oldest
// first.
func (s *taskQuestionService) Unresolved() []TaskQuestion {
	s.mu.Lock()
	out := make([]TaskQuestion, 0, len(s.pending))
	for _, w := range s.pending {
		out = append(out, w.q)
	}
	s.mu.Unlock()
	slices.SortFunc(out, func(a, b TaskQuestion) int {
		if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
			return c
		}
		return cmp.Compare(a.QuestionID, b.QuestionID)
	})
	return out
}

// PendingForOwner returns the owner's unanswered questions, oldest
// first. With a repository wired the durable rows are the resync
// source; otherwise the in-memory tracked set is filtered.
func (s *taskQuestionService) PendingForOwner(ctx context.Context, ownerSessionID string) ([]TaskQuestion, error) {
	if s.repo != nil {
		return s.repo.ListUnresolvedForOwner(ctx, ownerSessionID)
	}
	out := make([]TaskQuestion, 0)
	s.mu.Lock()
	for _, w := range s.pending {
		if w.q.OwnerSessionID == ownerSessionID {
			out = append(out, w.q)
		}
	}
	s.mu.Unlock()
	slices.SortFunc(out, func(a, b TaskQuestion) int {
		if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
			return c
		}
		return cmp.Compare(a.QuestionID, b.QuestionID)
	})
	return out, nil
}

// InterruptStale resolves every question still pending in the
// durable mirror as interrupted. A fresh process tracks no waiters,
// so any pending durable row was left by an earlier process and can
// never be answered here. Questions tracked by this service are
// skipped: they belong to live waiters. The repository's
// pending-only conditional resolution keeps the sweep exactly-once.
func (s *taskQuestionService) InterruptStale(ctx context.Context) (int, error) {
	if s.repo == nil {
		return 0, nil
	}
	unresolved, err := s.repo.ListUnresolved(ctx)
	if err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	interrupted := 0
	for _, q := range unresolved {
		if _, live := s.pending[q.QuestionID]; live {
			continue
		}
		u := ResolutionUpdate{Resolution: ResolutionInterrupted, ResolvedAt: time.Now().UTC()}
		won, err := s.repo.Resolve(ctx, q.QuestionID, u)
		if err != nil {
			return interrupted, fmt.Errorf("interrupt stale task question %s: %w", q.QuestionID, err)
		}
		if won {
			interrupted++
		}
	}
	return interrupted, nil
}

// Shutdown interrupts every tracked waiter and rejects further
// AskTask calls.
func (s *taskQuestionService) Shutdown() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	ids := make([]string, 0, len(s.pending))
	for id := range s.pending {
		ids = append(ids, id)
	}
	s.mu.Unlock()
	for _, id := range ids {
		if _, _, err := s.resolve(id, resolution{kind: ResolutionInterrupted}); err != nil {
			slog.Error("Failed to persist task question interruption", "question_id", id, "error", err)
		}
	}
}

// forget drops a tracked waiter without resolving it, for the
// registration-failure path where no runner is blocked yet.
func (s *taskQuestionService) forget(questionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if w, ok := s.pending[questionID]; ok {
		delete(s.pending, questionID)
		delete(s.byTask, w.q.TaskID)
	}
}
