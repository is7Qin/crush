package app

import (
	"context"
	"fmt"

	"github.com/charmbracelet/crush/internal/agent/task"
	"github.com/charmbracelet/crush/internal/agent/taskquestion"
)

// taskQuestionLifecycle is the App-owned TaskQuestionLifecycle bridge
// from docs/specs/crush-agents/10-remaining-integration.md section 4.
// It maps taskquestion records onto the manager's join transactions
// so the question row and the task state always commit or roll back
// together, and a crash can never leave a durable pending question
// beside a running task.
type taskQuestionLifecycle struct {
	mgr *task.Manager
}

var _ taskquestion.Lifecycle = taskQuestionLifecycle{}

// newTaskQuestionLifecycle returns the production lifecycle bridge
// for the App's task manager.
func newTaskQuestionLifecycle(mgr *task.Manager) taskQuestionLifecycle {
	return taskQuestionLifecycle{mgr: mgr}
}

// BeginWait atomically persists the pending question and moves the
// asking task running -> waiting_for_input.
func (l taskQuestionLifecycle) BeginWait(ctx context.Context, q taskquestion.TaskQuestion) error {
	batch, err := taskquestion.EncodeBatch(q.Batch)
	if err != nil {
		return fmt.Errorf("%w: encode task question batch: %w", taskquestion.ErrPersistenceFailed, err)
	}
	if err := l.mgr.BeginQuestionWait(ctx, task.QuestionRow{
		QuestionID:     q.QuestionID,
		TaskID:         q.TaskID,
		OwnerSessionID: q.OwnerSessionID,
		ChildSessionID: q.ChildSessionID,
		RunGeneration:  q.RunGeneration,
		Batch:          batch,
		CreatedAt:      q.CreatedAt,
	}); err != nil {
		return fmt.Errorf("%w: begin task question wait: %w", taskquestion.ErrPersistenceFailed, err)
	}
	return nil
}

// Resolve applies the question resolution together with its matching
// task transition in one durable step. The runner is woken by the
// service only after this commits.
func (l taskQuestionLifecycle) Resolve(ctx context.Context, q taskquestion.TaskQuestion, u taskquestion.ResolutionUpdate) error {
	answers, err := taskquestion.EncodeAnswers(u.Answers)
	if err != nil {
		return fmt.Errorf("%w: encode task question answers: %w", taskquestion.ErrPersistenceFailed, err)
	}
	out := task.QuestionOutcome{
		QuestionID:     q.QuestionID,
		TaskID:         q.TaskID,
		OwnerSessionID: q.OwnerSessionID,
		ChildSessionID: q.ChildSessionID,
		RunGeneration:  q.RunGeneration,
		Resolution:     string(u.Resolution),
		Answers:        answers,
		ResolvedAt:     u.ResolvedAt,
	}
	var terr error
	switch u.Resolution {
	case taskquestion.ResolutionAnswered:
		// The task stays waiting_for_input; Resume reacquires
		// capacity before the runner continues.
		terr = l.mgr.AnswerQuestion(ctx, out)
	case taskquestion.ResolutionCancelled:
		terr = l.mgr.TerminalizeQuestion(ctx, out, task.TerminalUpdate{
			Status:      task.StatusCancelled,
			Summary:     "task question cancelled",
			CompletedAt: u.ResolvedAt,
		})
	case taskquestion.ResolutionTimedOut:
		terr = l.mgr.TerminalizeQuestion(ctx, out, task.TerminalUpdate{
			Status:      task.StatusFailed,
			Summary:     "task question timed out",
			Err:         task.ReasonTaskQuestionTimeout,
			CompletedAt: u.ResolvedAt,
		})
	case taskquestion.ResolutionInterrupted:
		terr = l.mgr.TerminalizeQuestion(ctx, out, task.TerminalUpdate{
			Status:      task.StatusInterrupted,
			Summary:     "task question interrupted",
			CompletedAt: u.ResolvedAt,
		})
	default:
		return fmt.Errorf("%w: unknown task question resolution %q",
			taskquestion.ErrInvalidRequest, u.Resolution)
	}
	if terr != nil {
		return fmt.Errorf("%w: resolve task question: %w", taskquestion.ErrPersistenceFailed, terr)
	}
	return nil
}

// Resume reacquires model capacity and moves the answered question's
// still-waiting task back to running.
func (l taskQuestionLifecycle) Resume(ctx context.Context, q taskquestion.TaskQuestion) error {
	if err := l.mgr.ResumeQuestion(ctx, q.TaskID, q.RunGeneration); err != nil {
		return fmt.Errorf("%w: resume task after answer: %w", taskquestion.ErrPersistenceFailed, err)
	}
	return nil
}
