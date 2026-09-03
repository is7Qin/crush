package task

// Task-question join transactions. The agent_task_questions schema is
// owned by internal/agent/taskquestion; this file only carries opaque
// encoded batch/answer blobs and resolution names, and recognizes
// the single durable state the task side must fence on: "pending".
// The App-owned lifecycle bridge maps taskquestion records onto
// these payloads, so neither package imports the other.

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// questionPending is the unresolved marker of an
// agent_task_questions row. It is the only resolution name the task
// core interprets; every other name is opaque data it stamps.
const questionPending = "pending"

// questionInterrupted is the resolution name the terminal
// transaction stamps on any pending question left behind by a
// terminalized attempt.
const questionInterrupted = "interrupted"

// ReasonTaskQuestionTimeout is the stable terminal reason recorded
// when a task fails because its question timed out.
const ReasonTaskQuestionTimeout = "task_question_timeout"

// ErrQuestionStale reports that a question-row transition lost its
// conditional fence: the row was not pending (or the identity fence
// did not match) when the transaction ran, so nothing changed.
var ErrQuestionStale = errors.New("task question transition lost")

// QuestionRow describes the pending agent_task_questions row bound
// to a task attempt by BeginQuestionWait. Batch is the opaque
// encoded question.Request owned by the question layer.
type QuestionRow struct {
	QuestionID     string
	TaskID         string
	OwnerSessionID string
	ChildSessionID string
	RunGeneration  uint64
	Batch          string
	CreatedAt      time.Time
}

// QuestionOutcome describes one conditional question-row transition
// from pending to a terminal resolution name. Answers is the opaque
// encoded answer payload.
type QuestionOutcome struct {
	QuestionID     string
	TaskID         string
	OwnerSessionID string
	ChildSessionID string
	RunGeneration  uint64
	Resolution     string
	Answers        string
	ResolvedAt     time.Time
}

func (o QuestionOutcome) identityFencesMatch(t *Task) bool {
	return t != nil &&
		t.ID == o.TaskID &&
		t.OwnerSessionID == o.OwnerSessionID &&
		t.ChildSessionID == o.ChildSessionID &&
		t.RunGeneration == o.RunGeneration
}

// BeginQuestionWait durably binds row to its live attempt: one
// transaction inserts the pending question and moves the task
// running -> waiting_for_input, then the manager releases the
// running-model slot, pumps waiting work, and publishes the waiting
// event. The question batch is published to clients by the caller
// only after this returns, so no client observes a question whose
// task is not already waiting.
func (m *Manager) BeginQuestionWait(ctx context.Context, row QuestionRow) error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return ErrShuttingDown
	}
	at, live := m.live[row.TaskID]
	if !live || at.runGeneration != row.RunGeneration {
		m.mu.Unlock()
		return ErrNotFound
	}
	// A hidden task never carries a public question transport, and a
	// durable pending question would make it resolvable through the
	// public question routes, so the wait is fenced out here too.
	if t, err := m.store.Get(ctx, row.TaskID); err != nil {
		m.mu.Unlock()
		return err
	} else if t.IsHidden() {
		m.mu.Unlock()
		return ErrNotFound
	}
	t, err := m.store.BeginQuestionWait(ctx, row)
	if err != nil {
		m.mu.Unlock()
		return err
	}
	if at.slotHeld {
		at.slotHeld = false
		m.running[at.child.key]--
	}
	events := append(m.pumpLocked(ctx, at.child.key), eventForTransition(t, StatusRunning))
	m.mu.Unlock()
	m.events.publish(events...)
	return nil
}

// AnswerQuestion durably resolves out as answered in one
// conditional transaction. The task stays waiting_for_input: model
// capacity is reacquired by ResumeQuestion, and the runner only
// continues after the question row commits. A resolution already
// committed durably elsewhere (restart interruption, for example)
// reports itself and returns nil.
func (m *Manager) AnswerQuestion(ctx context.Context, out QuestionOutcome) error {
	m.mu.Lock()
	err := m.store.ResolveQuestionAnswered(ctx, out)
	m.mu.Unlock()
	if errors.Is(err, ErrQuestionStale) {
		return m.questionAlreadyResolved(ctx, out)
	}
	return err
}

// TerminalizeQuestion resolves out and terminalizes its task attempt
// with u in one transaction (u's status must be the outcome's
// matching task terminal: cancelled, failed, or interrupted). The
// full terminal delivery (outbox, inbox, cost aggregation) commits
// together with the question row; the runner is woken by this
// call's caller only after the commit. A question whose resolution
// already committed elsewhere reports nil.
func (m *Manager) TerminalizeQuestion(ctx context.Context, out QuestionOutcome, u TerminalUpdate) error {
	m.mu.Lock()
	at, live := m.live[out.TaskID]
	if !live || at.runGeneration != out.RunGeneration {
		m.mu.Unlock()
		return m.questionAlreadyResolved(ctx, out)
	}
	u.Question = &out
	t, won, err := m.store.TerminalizeAndDeliver(ctx, out.TaskID, out.RunGeneration, u)
	if err != nil {
		m.mu.Unlock()
		if errors.Is(err, ErrQuestionStale) {
			return m.questionAlreadyResolved(ctx, out)
		}
		return err
	}
	if !won {
		// The task terminalized concurrently; the transaction rolled
		// the question change back, but that terminalization's sweep
		// resolved the pending row. Report the committed outcome.
		m.mu.Unlock()
		return m.questionAlreadyResolved(ctx, out)
	}
	events := m.terminalizedLocked(ctx, t, at)
	m.mu.Unlock()
	m.events.publish(events...)
	return nil
}

// ResumeQuestion reacquires a running-model slot for the answered
// question's task and conditionally moves it waiting_for_input ->
// running. It blocks in the per-key FIFO until capacity,
// cancellation, or terminalization intervenes, and it is only valid
// on the asking runner's context.
func (m *Manager) ResumeQuestion(ctx context.Context, taskID string, runGeneration uint64) error {
	h, ok := m.Handle(taskID)
	if !ok {
		return ErrNotFound
	}
	if h.RunGeneration() != runGeneration {
		return ErrInvalidTransition
	}
	return h.Resumed(ctx)
}

// questionAlreadyResolved consults the durable question row after a
// lost conditional transition: a resolved row means the outcome is
// already committed and callers may treat the resolve as complete;
// a still-pending row means nothing committed.
func (m *Manager) questionAlreadyResolved(ctx context.Context, out QuestionOutcome) error {
	status, err := m.store.QuestionStatus(ctx, out.QuestionID)
	if err != nil {
		return err
	}
	if status != questionPending {
		return nil
	}
	return fmt.Errorf("%w: question %s still pending", ErrQuestionStale, out.QuestionID)
}
