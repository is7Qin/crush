package task

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
)

// nextSequenceSQL allocates the next mailbox sequence for a child
// session; the initial admission message is sequence zero.
const nextSequenceSQL = `SELECT COALESCE(MAX(sequence), -1) + 1 FROM agent_task_messages WHERE child_session_id = ?`

// CreatePendingTask admits one attempt in a single transaction: the
// pending task row, the child session binding (including the
// sessions INSERT for a fresh delegation), and the mailbox row
// carrying the admission prompt. Any error rolls the whole admission
// back, so no partial child session or mailbox row can be observed.
func (s *SQLiteStore) CreatePendingTask(ctx context.Context, adm Admission) (*Task, error) {
	t := *adm.Task
	t.FallbackModels = slices.Clone(adm.Task.FallbackModels)
	if t.ResumesTaskID != "" {
		if err := s.validateContinuation(ctx, &t); err != nil {
			return nil, err
		}
	}
	fallbacks, err := encodeFallbackModels(t.FallbackModels)
	if err != nil {
		return nil, fmt.Errorf("admit task %s: %w", t.ID, err)
	}
	if adm.Child.New && adm.Child.ID == "" {
		return nil, fmt.Errorf("admit task %s: %w: fresh delegation needs a child session id", t.ID, ErrInvalidRequest)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("admit task %s: %w", t.ID, err)
	}
	defer tx.Rollback() //nolint:errcheck // best-effort after Commit

	if adm.Child.New {
		if _, err := tx.ExecContext(ctx, `INSERT INTO sessions
			(id, parent_session_id, title, message_count, prompt_tokens,
			 completion_tokens, cost, summary_message_id, updated_at, created_at)
			VALUES (?, ?, ?, 0, 0, 0, 0, NULL, strftime('%s', 'now'), strftime('%s', 'now'))`,
			adm.Child.ID, nullString(adm.Child.ParentID), adm.Child.Title,
		); err != nil {
			return nil, fmt.Errorf("admit task %s: create child session: %w", t.ID, err)
		}
		t.ChildSessionID = adm.Child.ID
	}
	// The admission message is inserted first so the task row can
	// record the message that caused it; the sequence needs the
	// child binding, which exists from here on.
	if t.ChildSessionID == "" {
		return nil, fmt.Errorf("admit task %s: %w", t.ID, ErrInvalidRequest)
	}
	admMsg := admissionMessage(&t)
	if err := appendMessageTx(ctx, tx, &admMsg); err != nil {
		return nil, fmt.Errorf("admit task %s: %w", t.ID, err)
	}
	t.MessageID = admMsg.ID
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO agent_tasks (`+taskColumns+`) VALUES (`+taskPlaceholders+`)`,
		taskArgs(&t, fallbacks)...,
	); err != nil {
		return nil, fmt.Errorf("admit task %s: %w", t.ID, err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("admit task %s: %w", t.ID, err)
	}
	return &t, nil
}

// validateContinuation loads the predecessor and the child
// session's current attempt and enforces the continuation rules:
// same owner, terminal, and a strictly increasing run generation on
// the retained child session. The new attempt records the current
// attempt as its immediate predecessor.
func (s *SQLiteStore) validateContinuation(ctx context.Context, t *Task) error {
	prev, err := s.getTaskRow(ctx, t.ResumesTaskID)
	if err != nil {
		return err
	}
	if prev.IsHidden() {
		// Hidden system-owned tasks are not continuable through the
		// public call_agent path: report the same not-found as for an
		// unknown id so their existence never leaks.
		return ErrNotFound
	}
	if prev.OwnerSessionID != t.OwnerSessionID {
		return ErrNotOwner
	}
	if !prev.Status.Terminal() {
		return ErrResumeLive
	}
	cur, err := s.currentAttemptRow(ctx, prev.ChildSessionID)
	if err != nil {
		return err
	}
	if !cur.Status.Terminal() {
		return ErrResumeLive
	}
	t.ChildSessionID = prev.ChildSessionID
	t.ResumesTaskID = cur.ID
	t.RunGeneration = cur.RunGeneration + 1
	return nil
}

func (s *SQLiteStore) getTaskRow(ctx context.Context, id string) (*Task, error) {
	t, err := scanTask(s.db.QueryRowContext(ctx,
		`SELECT `+taskColumns+` FROM agent_tasks WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("resume target: %w", err)
	}
	return t, nil
}

func (s *SQLiteStore) currentAttemptRow(ctx context.Context, childSessionID string) (*Task, error) {
	t, err := scanTask(s.db.QueryRowContext(ctx,
		`SELECT `+taskColumns+` FROM agent_tasks
		WHERE child_session_id = ?
		ORDER BY run_generation DESC LIMIT 1`, childSessionID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("current attempt for child %s: %w", childSessionID, err)
	}
	return t, nil
}

// admissionMessage renders the sequence-zero mailbox row for one
// admitted attempt.
func admissionMessage(t *Task) ChildMessage {
	return ChildMessage{
		ID:             uuid.NewString(),
		ChildSessionID: t.ChildSessionID,
		TaskID:         t.ID,
		OwnerSessionID: t.OwnerSessionID,
		Origin:         OriginParent,
		Prompt:         t.Prompt,
		Attachments:    "[]",
		State:          MessageQueued,
		CreatedAt:      t.CreatedAt,
	}
}

// AppendChildMessage queues one mailbox row under the addressed
// task's child session with a transactionally allocated sequence.
func (s *SQLiteStore) AppendChildMessage(ctx context.Context, msg ChildMessage) (ChildMessage, error) {
	if msg.Prompt == "" {
		return ChildMessage{}, fmt.Errorf("append child message: %w: empty prompt", ErrInvalidRequest)
	}
	t, err := s.getTaskRow(ctx, msg.TaskID)
	if err != nil {
		return ChildMessage{}, err
	}
	// The task's binding is canonical: callers address a task, and
	// the mailbox belongs to that task's child session.
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

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ChildMessage{}, fmt.Errorf("append child message: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // best-effort after Commit
	if err := appendMessageTx(ctx, tx, &msg); err != nil {
		return ChildMessage{}, fmt.Errorf("append child message: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return ChildMessage{}, fmt.Errorf("append child message: %w", err)
	}
	return msg, nil
}

// appendMessageTx allocates the next sequence for the child session
// and inserts msg as a queued row inside tx, writing the allocated
// sequence back into msg.
func appendMessageTx(ctx context.Context, tx *sql.Tx, msg *ChildMessage) error {
	var seq int64
	if err := tx.QueryRowContext(ctx, nextSequenceSQL, msg.ChildSessionID).
		Scan(&seq); err != nil {
		return fmt.Errorf("allocate message sequence: %w", err)
	}
	msg.Sequence = uint64(seq)
	_, err := tx.ExecContext(ctx, `INSERT INTO agent_task_messages
		(id, child_session_id, task_id, owner_session_id, sequence, origin,
		 prompt, attachments, state, reason, created_at, delivered_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, ?, NULL)`,
		msg.ID, msg.ChildSessionID, msg.TaskID, msg.OwnerSessionID,
		seq, string(msg.Origin), msg.Prompt, msg.Attachments,
		string(MessageQueued), msg.CreatedAt.UnixNano(),
	)
	return err
}
