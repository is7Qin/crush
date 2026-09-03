package task

// SQLite task-question join transactions. Every method here is one
// transaction over the shared connection: the question-row change
// and the agent_tasks transition commit or roll back together, so a
// crash can never leave a durable pending question beside a running
// (or resumed) task. The agent_task_questions schema belongs to
// internal/agent/taskquestion; batch and answers cross here as
// opaque encoded text.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// BeginQuestionWait inserts the pending question row and moves its
// task attempt running -> waiting_for_input in one transaction. The
// task transition is a conditional UPDATE fenced on (id,
// run_generation, owner, child, running) whose RETURNING row decides
// the win; the question INSERT after it is subject to the one-
// pending-row-per-task unique index. Any failure rolls both back.
func (s *SQLiteStore) BeginQuestionWait(ctx context.Context, row QuestionRow) (*Task, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin task question wait %s: %w", row.QuestionID, err)
	}
	defer tx.Rollback() //nolint:errcheck // best-effort after Commit

	t, err := scanTask(tx.QueryRowContext(ctx, `UPDATE agent_tasks
	SET status = 'waiting_for_input', updated_at = ?
	WHERE id = ? AND run_generation = ? AND owner_session_id = ?
		AND child_session_id = ? AND status = 'running'
	RETURNING `+taskColumns,
		unixOrZero(row.CreatedAt), row.TaskID, int64(row.RunGeneration),
		row.OwnerSessionID, row.ChildSessionID,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, s.questionWaitLostTx(ctx, tx, row.TaskID)
	}
	if err != nil {
		return nil, fmt.Errorf("begin task question wait %s: %w", row.QuestionID, err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO agent_task_questions
		(question_id, task_id, owner_session_id, child_session_id,
			run_generation, batch, answers, status, created_at, resolved_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, '`+questionPending+`', ?, NULL)`,
		row.QuestionID, row.TaskID, row.OwnerSessionID, row.ChildSessionID,
		int64(row.RunGeneration), row.Batch, "", unixOrZero(row.CreatedAt),
	); err != nil {
		return nil, fmt.Errorf("bind task question %s: %w", row.QuestionID, err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("begin task question wait %s: %w", row.QuestionID, err)
	}
	return t, nil
}

// questionWaitLostTx reports why the fenced wait transition lost,
// reading through the open transaction (the shared pool has one
// connection). A missing task is ErrNotFound; a task that is not
// this attempt's running self is a rejected transition.
func (s *SQLiteStore) questionWaitLostTx(ctx context.Context, tx *sql.Tx, taskID string) error {
	var status string
	err := tx.QueryRowContext(ctx,
		`SELECT status FROM agent_tasks WHERE id = ?`, taskID).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("begin task question wait check %s: %w", taskID, err)
	}
	return fmt.Errorf("%w: task %s is %s", ErrInvalidTransition, taskID, Status(status))
}

// ResolveQuestionAnswered applies the pending -> answered transition
// and verifies the attempt is still waiting_for_input for the same
// (id, run_generation, owner, child) fence, in one transaction. A
// lost conditional leaves every row untouched and reports
// ErrQuestionStale.
func (s *SQLiteStore) ResolveQuestionAnswered(ctx context.Context, out QuestionOutcome) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("answer task question %s: %w", out.QuestionID, err)
	}
	defer tx.Rollback() //nolint:errcheck // best-effort after Commit

	if err := resolveQuestionTx(ctx, tx, out); err != nil {
		return fmt.Errorf("answer task question %s: %w", out.QuestionID, err)
	}
	var status string
	err = tx.QueryRowContext(ctx,
		`SELECT status FROM agent_tasks
		WHERE id = ? AND run_generation = ? AND owner_session_id = ?
			AND child_session_id = ?`,
		out.TaskID, int64(out.RunGeneration), out.OwnerSessionID, out.ChildSessionID,
	).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("answer task question %s: %w", out.QuestionID, err)
	}
	if Status(status) != StatusWaitingForInput {
		return fmt.Errorf("answer task question %s: %w", out.QuestionID, ErrQuestionStale)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("answer task question %s: %w", out.QuestionID, err)
	}
	return nil
}

// resolveQuestionTx applies out inside tx as a conditional
// transition fenced on the full identity and on the row still being
// pending. A row that is not pending (or whose fence does not match)
// reports ErrQuestionStale with the transaction left for rollback.
func resolveQuestionTx(ctx context.Context, tx *sql.Tx, out QuestionOutcome) error {
	res, err := tx.ExecContext(ctx, `UPDATE agent_task_questions
	SET status = ?, answers = ?, resolved_at = ?
	WHERE question_id = ? AND task_id = ? AND owner_session_id = ?
		AND child_session_id = ? AND run_generation = ? AND status = '`+questionPending+`'`,
		out.Resolution, out.Answers, nullUnixNano(out.ResolvedAt),
		out.QuestionID, out.TaskID, out.OwnerSessionID, out.ChildSessionID,
		int64(out.RunGeneration),
	)
	if err != nil {
		return fmt.Errorf("resolve task question %s: %w", out.QuestionID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("resolve task question %s: %w", out.QuestionID, err)
	}
	if n != 1 {
		return ErrQuestionStale
	}
	return nil
}

// interruptPendingQuestionsTx resolves every still-pending question
// row for one attempt as interrupted inside tx. A terminalized
// attempt must never leave a durable pending question behind, so
// this runs in every terminal transaction, including startup
// recovery.
func interruptPendingQuestionsTx(ctx context.Context, tx *sql.Tx, t *Task) error {
	if _, err := tx.ExecContext(ctx, `UPDATE agent_task_questions
		SET status = ?, resolved_at = ?
		WHERE task_id = ? AND run_generation = ? AND status = ?`,
		questionInterrupted, nullUnixNano(t.CompletedAt),
		t.ID, int64(t.RunGeneration), questionPending,
	); err != nil {
		return fmt.Errorf("interrupt pending questions for task %s: %w", t.ID, err)
	}
	return nil
}

// QuestionStatus returns the stored resolution name of a question
// row ('pending' until resolved). Unknown ids report ErrNotFound.
func (s *SQLiteStore) QuestionStatus(ctx context.Context, questionID string) (string, error) {
	var status string
	err := s.db.QueryRowContext(ctx,
		`SELECT status FROM agent_task_questions WHERE question_id = ?`, questionID,
	).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("get task question %s status: %w", questionID, err)
	}
	return status, nil
}
