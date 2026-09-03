package taskquestion

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/charmbracelet/crush/internal/question"
	"github.com/google/uuid"
)

// resolution is the payload of the one conditional resolution.
type resolution struct {
	kind    Resolution
	answers []question.Answer
}

// waiter is the tracked runner for one pending question. The outcome
// channel is buffered with capacity 1 and only the resolution winner
// ever sends on it, so the send never blocks and an outcome nobody
// drains cannot leak a goroutine.
type waiter struct {
	q       TaskQuestion
	outcome chan resolution
}

type taskQuestionService struct {
	repo      Repository
	lifecycle Lifecycle
	suspend   func(ctx context.Context, questionID string) error
	resume    func(ctx context.Context, questionID string) error

	broker *pubsub.Broker[TaskQuestion]
	notif  *pubsub.Broker[Notification]

	mu      sync.Mutex
	closed  bool
	pending map[string]*waiter // question id -> tracked waiter
	byTask  map[string]string  // task id -> pending question id
	// resolved remembers recent winners so duplicate controls
	// report ErrAlreadyResolved instead of ErrNotFound.
	// ponytail: bounded FIFO; durable rows are the resync source
	// beyond the cap.
	resolved     map[string]Resolution
	resolvedRank []string
}

// resolvedHistory bounds the in-memory resolution memory.
const resolvedHistory = 256

var _ TaskQuestionService = (*taskQuestionService)(nil)

// NewService creates a task-aware question service.
func NewService(cfg Config) *taskQuestionService {
	return &taskQuestionService{
		repo:      cfg.Repo,
		lifecycle: cfg.Lifecycle,
		suspend:   cfg.Suspend,
		resume:    cfg.Resume,
		broker:    pubsub.NewBroker[TaskQuestion](),
		notif:     pubsub.NewBroker[Notification](),
		pending:   map[string]*waiter{},
		byTask:    map[string]string{},
		resolved:  map[string]Resolution{},
	}
}

// Subscribe returns a channel for task question request events.
func (s *taskQuestionService) Subscribe(ctx context.Context) <-chan pubsub.Event[TaskQuestion] {
	return s.broker.Subscribe(ctx)
}

// SubscribeNotifications returns a channel for resolution events.
func (s *taskQuestionService) SubscribeNotifications(ctx context.Context) <-chan pubsub.Event[Notification] {
	return s.notif.Subscribe(ctx)
}

// AskTask registers a question for a task, durably moves the task to
// waiting, publishes the batch, and blocks until the question
// resolves.
func (s *taskQuestionService) AskTask(ctx context.Context, req TaskQuestionRequest) ([]question.Answer, error) {
	if err := req.validate(); err != nil {
		return nil, err
	}
	batch, err := prepareBatch(req.Batch)
	if err != nil {
		return nil, err
	}
	w := &waiter{
		q: TaskQuestion{
			QuestionID:     uuid.NewString(),
			TaskID:         req.TaskID,
			OwnerSessionID: req.OwnerSessionID,
			ChildSessionID: req.ChildSessionID,
			RunGeneration:  req.RunGeneration,
			Batch:          batch,
			Resolution:     ResolutionPending,
			CreatedAt:      time.Now().UTC(),
		},
		outcome: make(chan resolution, 1),
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, ErrShuttingDown
	}
	if _, ok := s.byTask[w.q.TaskID]; ok {
		s.mu.Unlock()
		return nil, ErrQuestionPending
	}
	s.pending[w.q.QuestionID] = w
	s.byTask[w.q.TaskID] = w.q.QuestionID
	s.mu.Unlock()

	if s.lifecycle != nil {
		// Durable-first: the question row and the task's
		// waiting_for_input transition commit together before the
		// batch is published, so no client can ever see a question
		// whose task is not waiting and no crash can leave a
		// pending row beside a running task.
		if err := s.lifecycle.BeginWait(ctx, w.q); err != nil {
			s.forget(w.q.QuestionID)
			return nil, err
		}
		s.broker.Publish(pubsub.CreatedEvent, w.q)
	} else {
		if s.repo != nil {
			if err := s.repo.Save(ctx, w.q); err != nil {
				s.forget(w.q.QuestionID)
				return nil, fmt.Errorf("persist task question: %w", err)
			}
		}
		s.broker.Publish(pubsub.CreatedEvent, w.q)
		if s.suspend != nil {
			if err := s.suspend(ctx, w.q.QuestionID); err != nil {
				s.resolve(w.q.QuestionID, resolution{kind: ResolutionCancelled})
				return nil, err
			}
		}
	}

	var timeoutC <-chan time.Time
	if req.Timeout > 0 {
		timer := time.NewTimer(req.Timeout)
		defer timer.Stop()
		timeoutC = timer.C
	}

	select {
	case <-ctx.Done():
		// A committed resolution may already be queued: the durable
		// outcome always wins over the bare context cancellation, so
		// a cancelled-and-resolved task observes its resolution once
		// and never races a second resolve into the seam.
		select {
		case res := <-w.outcome:
			return s.finish(ctx, w.q, res)
		default:
		}
		s.resolve(w.q.QuestionID, resolution{kind: ResolutionCancelled})
		return nil, ctx.Err()
	case <-timeoutC:
		_, won, err := s.resolve(w.q.QuestionID, resolution{kind: ResolutionTimedOut})
		if err != nil {
			return nil, err
		}
		if !won {
			// An answer or cancellation committed first; honor it.
			return s.finish(ctx, w.q, <-w.outcome)
		}
		return nil, ErrTimeout
	case res := <-w.outcome:
		return s.finish(ctx, w.q, res)
	}
}

// finish maps a winning resolution back onto the runner contract.
func (s *taskQuestionService) finish(ctx context.Context, q TaskQuestion, res resolution) ([]question.Answer, error) {
	switch res.kind {
	case ResolutionAnswered:
		// The resolution committed before the runner woke; Resume
		// reacquires model capacity and conditionally moves the
		// still-waiting task back to running.
		if s.lifecycle != nil {
			if err := s.lifecycle.Resume(ctx, q); err != nil {
				return nil, err
			}
		} else if s.resume != nil {
			if err := s.resume(ctx, q.QuestionID); err != nil {
				return nil, err
			}
		}
		return res.answers, nil
	case ResolutionCancelled:
		return nil, question.ErrCancelled
	case ResolutionTimedOut:
		return nil, ErrTimeout
	case ResolutionInterrupted:
		return nil, ErrShuttingDown
	}
	return nil, ErrInvalidRequest
}

// prepareBatch fills question and batch ids and applies the
// multi-question confirm defaults, mirroring the primary service.
func prepareBatch(batch question.Request) (question.Request, error) {
	if batch.ID == "" {
		batch.ID = uuid.NewString()
	}
	for i := range batch.Questions {
		if batch.Questions[i].ID == "" {
			batch.Questions[i].ID = uuid.NewString()
		}
	}
	if len(batch.Questions) >= 2 {
		if batch.ConfirmTitle == "" {
			batch.ConfirmTitle = "Ready to go?"
		}
		if batch.ConfirmDescription == "" {
			batch.ConfirmDescription = "Review your answers above and confirm."
		}
	}
	if err := batch.Validate(); err != nil {
		return batch, fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	return batch, nil
}
