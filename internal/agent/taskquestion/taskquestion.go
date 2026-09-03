// Package taskquestion implements the task-correlated question
// transport for background and child agent questions. It is a
// sibling of the workspace-global question.Service, which keeps its
// single-slot primary semantics untouched.
//
// The service is in-memory-first: waiters live in memory and every
// question is mirrored through an optional durable Repository so
// reconnecting clients can resync by question id. Production wiring
// supplies a Lifecycle bridge instead of the mirror path: BeginWait
// durably persists the pending question together with the task's
// waiting transition before the batch is published, and Resolve
// commits the question's terminal resolution together with the
// matching task transition before any runner is woken. Only one
// unresolved question may exist per task. Answer, cancel, timeout,
// runner-context loss, and shutdown all race through one
// conditional resolution; the first winner wakes the tracked runner
// exactly once and every later attempt observes ErrAlreadyResolved.
// A failed durable resolution restores the waiter so the winner can
// retry without the runner drifting.
//
// The package must not import the agent, coordinator, provider, or
// Fantasy layers. task.Manager integration happens only through the
// narrow Lifecycle seam in Config (Suspend/Resume callbacks remain
// for in-memory tests); the production bridge is App-owned.
package taskquestion

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/charmbracelet/crush/internal/question"
)

// Sentinel errors. Callers match with errors.Is; cancellation of an
// answered-or-cancelled question reuses question.ErrCancelled so
// existing tool-side handling keeps working.
var (
	// ErrNotFound names an unknown or already-resolved question id
	// that no longer has a tracked record.
	ErrNotFound = errors.New("task question not found")
	// ErrNotOwner rejects answers and cancellations whose trusted
	// caller session is not the question's owner session.
	ErrNotOwner = errors.New("caller session does not own the task question")
	// ErrAlreadyResolved reports that another answer, cancellation,
	// timeout, or shutdown won the conditional resolution first.
	ErrAlreadyResolved = errors.New("task question already resolved")
	// ErrQuestionPending rejects a second question for a task that
	// still has one unanswered.
	ErrQuestionPending = errors.New("task already has a pending question")
	// ErrTimeout is returned by AskTask when the wait expires.
	ErrTimeout = errors.New("task question timed out")
	// ErrShuttingDown is returned by AskTask after Shutdown began,
	// and to waiters interrupted by shutdown.
	ErrShuttingDown = errors.New("task question service is shutting down")
	// ErrInvalidRequest reports a malformed task question request.
	ErrInvalidRequest = errors.New("invalid task question request")
	// ErrPersistenceFailed reports that a Lifecycle BeginWait or
	// Resolve rolled its durable question-and-task transaction back.
	// The failed resolution never commits: the service keeps the
	// question pending, does not wake the runner, and returns the
	// error to the caller so the control can retry.
	ErrPersistenceFailed = errors.New("task question persistence failed")
	// ErrTransportUnavailable is returned by adapters when no
	// task-aware transport is wired for the caller's mode.
	ErrTransportUnavailable = errors.New("question transport unavailable")
)

// Resolution is the terminal outcome of a task question.
type Resolution string

const (
	// ResolutionPending marks an unanswered question.
	ResolutionPending Resolution = "pending"
	// ResolutionAnswered marks a question answered by its owner.
	ResolutionAnswered Resolution = "answered"
	// ResolutionCancelled marks a question cancelled by its owner
	// or by the runner losing its context.
	ResolutionCancelled Resolution = "cancelled"
	// ResolutionTimedOut marks a question whose wait expired.
	ResolutionTimedOut Resolution = "timed_out"
	// ResolutionInterrupted marks a question dropped by shutdown.
	ResolutionInterrupted Resolution = "interrupted"
)

// TaskQuestionRequest asks the owner session of a live task to
// answer a question batch on the task's behalf. Identity fields are
// trusted values supplied by the child tool boundary, never by model
// arguments. Timeout bounds the wait; zero waits until an answer,
// cancellation, or shutdown.
type TaskQuestionRequest struct {
	TaskID         string
	OwnerSessionID string
	ChildSessionID string
	RunGeneration  uint64
	Timeout        time.Duration
	Batch          question.Request
}

// TaskQuestion is the durable record of one task-correlated
// question batch. Identity fields are immutable; resolution fields
// are set exactly once by the winning conditional resolution.
type TaskQuestion struct {
	QuestionID     string            `json:"question_id"`
	TaskID         string            `json:"task_id"`
	OwnerSessionID string            `json:"owner_session_id"`
	ChildSessionID string            `json:"child_session_id"`
	RunGeneration  uint64            `json:"run_generation"`
	Batch          question.Request  `json:"batch"`
	Answers        []question.Answer `json:"answers,omitempty"`
	Resolution     Resolution        `json:"resolution"`
	CreatedAt      time.Time         `json:"created_at"`
	ResolvedAt     time.Time         `json:"resolved_at,omitempty"`
}

// Notification is published when a task question resolves so clients
// can dismiss open forms and resync by question id.
type Notification struct {
	QuestionID string     `json:"question_id"`
	TaskID     string     `json:"task_id"`
	BatchID    string     `json:"batch_id"`
	Resolution Resolution `json:"resolution"`
}

// TaskQuestionService is the task-aware sibling of question.Service.
type TaskQuestionService interface {
	// Subscribe returns a channel of task-question request events.
	// The payload is the full correlated record (question id, task
	// id, child session id, run generation, and batch), not the bare
	// batch, so transports can never confuse a child question with a
	// primary one.
	pubsub.Subscriber[TaskQuestion]

	// SubscribeNotifications returns a channel for resolution
	// notifications.
	SubscribeNotifications(ctx context.Context) <-chan pubsub.Event[Notification]

	// AskTask registers the question, durably waits the task through
	// the configured Lifecycle (or the legacy Suspend callback),
	// publishes the batch only after the waiting state is committed,
	// blocks until one conditional resolution wins, then resumes the
	// task before returning answers. It returns question.ErrCancelled
	// on cancellation, ErrTimeout on expiry, ErrShuttingDown on
	// interruption, and ctx.Err() when the runner context is
	// cancelled first.
	AskTask(ctx context.Context, req TaskQuestionRequest) ([]question.Answer, error)

	// AnswerTask resolves the question with answers after verifying
	// caller ownership.
	AnswerTask(callerSessionID, questionID string, answers []question.Answer) error

	// CancelTask resolves the question as cancelled after verifying
	// caller ownership.
	CancelTask(callerSessionID, questionID string) error

	// Pending returns a copy of the unanswered question record.
	Pending(questionID string) (TaskQuestion, bool)

	// Unresolved returns copies of all unanswered questions, oldest
	// first, for client resync.
	Unresolved() []TaskQuestion

	// PendingForOwner returns the durable unanswered questions owned
	// by ownerSessionID, oldest first, so a reconnecting client can
	// resync and answer by question id. With no repository wired it
	// filters the in-memory tracked set.
	PendingForOwner(ctx context.Context, ownerSessionID string) ([]TaskQuestion, error)

	// InterruptStale resolves every question still pending in the
	// durable mirror as interrupted. A fresh process tracks no
	// waiters, so any durable pending row was left by an earlier
	// process and can never be answered. It is called once during
	// startup recovery and returns how many rows it resolved.
	InterruptStale(ctx context.Context) (int, error)

	// Shutdown interrupts every tracked waiter and rejects further
	// AskTask calls. It is idempotent.
	Shutdown()
}

// Config wires a service. Repo may be nil for pure in-memory
// operation. Lifecycle is the durable task-state bridge production
// wiring must supply: BeginWait commits the pending question and the
// task's running -> waiting_for_input transition in one atomic step
// before the batch is published, and Resolve commits the question's
// terminal resolution together with the matching task transition
// before any runner is woken. Suspend and Resume remain as the
// narrow in-memory test seam and are used only when Lifecycle is
// nil; both run on the asking runner's context.
type Config struct {
	Repo      Repository
	Lifecycle Lifecycle
	Suspend   func(ctx context.Context, questionID string) error
	Resume    func(ctx context.Context, questionID string) error
}

// Lifecycle is the App-owned bridge to the task manager. The
// taskquestion package stays independent of task types: records
// cross this seam as plain TaskQuestion values, and the bridge maps
// them onto durable task and question rows. Implementations must be
// safe for concurrent use and must make each method atomic.
type Lifecycle interface {
	// BeginWait atomically persists the pending question and moves
	// the matching task attempt running -> waiting_for_input,
	// releasing its running-model slot. It must return before the
	// question batch is published, so a client can never observe a
	// published question whose task is not durably waiting.
	BeginWait(ctx context.Context, q TaskQuestion) error

	// Resolve atomically applies the conditional question transition
	// (pending -> u.Resolution) and the matching task transition:
	// answered leaves the task waiting for Resume; cancelled
	// terminalizes it cancelled; timed_out fails it with reason
	// task_question_timeout; interrupted marks it interrupted. The
	// bridge must verify task id, owner session, child session, and
	// run generation against the durable rows and return an error
	// wrapping ErrPersistenceFailed when a fenced transition rolls
	// the whole change back, so the service never wakes a runner
	// onto an unstamped task. A resolution that lost the durable
	// race to an earlier committed resolution (shutdown or restart
	// interruption, for example) must report that committed outcome
	// rather than an error.
	Resolve(ctx context.Context, q TaskQuestion, u ResolutionUpdate) error

	// Resume reacquires model capacity for the answered question's
	// task and conditionally moves it waiting_for_input -> running.
	// It is valid only for an answered question whose task is still
	// waiting_for_input; the runner continues once it returns.
	Resume(ctx context.Context, q TaskQuestion) error
}

func (r TaskQuestionRequest) validate() error {
	if r.TaskID == "" || r.OwnerSessionID == "" || r.ChildSessionID == "" {
		return fmt.Errorf(
			"%w: task id, owner session, and child session are required",
			ErrInvalidRequest,
		)
	}
	return nil
}
