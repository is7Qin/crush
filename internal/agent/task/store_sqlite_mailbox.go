package task

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
)

const childMessageColumns = `id, child_session_id, task_id, owner_session_id,
	sequence, origin, prompt, attachments, state, reason, created_at, delivered_at`

// taskPlaceholders matches taskColumns: 32 bind markers, one per
// column of the dispatch record.
var taskPlaceholders = strings.TrimSuffix(strings.Repeat("?, ", 32), ", ")

// DispatchNextChildMessage claims the lowest queued message for the
// child session and, in the same transaction, either promotes the
// bound pending attempt to running or inserts its successor attempt.
// While the current attempt is running or waiting for input nothing
// is claimed: queued messages wait for the next turn boundary, so no
// two attempts for one child session can execute concurrently.
func (s *SQLiteStore) DispatchNextChildMessage(ctx context.Context, childSessionID string) (*Task, bool, error) {
	if childSessionID == "" {
		return nil, false, fmt.Errorf("dispatch: %w: child session required", ErrInvalidRequest)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, fmt.Errorf("dispatch %s: %w", childSessionID, err)
	}
	defer tx.Rollback() //nolint:errcheck // best-effort after Commit

	claimed, err := scanMessage(tx.QueryRowContext(ctx,
		`SELECT `+childMessageColumns+` FROM agent_task_messages
		WHERE child_session_id = ? AND state = ?
		ORDER BY sequence ASC LIMIT 1`, childSessionID, string(MessageQueued)))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("dispatch %s: %w", childSessionID, err)
	}

	cur, err := scanTask(tx.QueryRowContext(ctx,
		`SELECT `+taskColumns+` FROM agent_tasks
		WHERE child_session_id = ?
		ORDER BY run_generation DESC LIMIT 1`, childSessionID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("dispatch %s: %w", childSessionID, err)
	}

	now := time.Now()
	var dispatched *Task
	switch {
	case cur.Status == StatusPending:
		dispatched, err = promotePendingTx(ctx, tx, cur, claimed, now)
	case cur.Status.Terminal():
		dispatched, err = insertSuccessorTx(ctx, tx, cur, claimed, now)
	default:
		// running or waiting_for_input: the attempt owns the child
		// turn; the message stays queued for the next boundary.
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("dispatch %s: %w", childSessionID, err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE agent_task_messages SET state = ?, delivered_at = ? WHERE id = ?`,
		string(MessageDelivered), now.UnixNano(), claimed.ID,
	); err != nil {
		return nil, false, fmt.Errorf("dispatch %s: claim message %d: %w", childSessionID, claimed.Sequence, err)
	}
	if err := appendStartOutboxTx(ctx, tx, dispatched, now); err != nil {
		return nil, false, fmt.Errorf("dispatch %s: %w", childSessionID, err)
	}
	if err := tx.Commit(); err != nil {
		return nil, false, fmt.Errorf("dispatch %s: %w", childSessionID, err)
	}
	return dispatched, true, nil
}

// promotePendingTx transitions the bound pending attempt to running,
// adopts the claimed message as its input prompt, and records the
// message id on it.
func promotePendingTx(ctx context.Context, tx *sql.Tx, cur *Task, claimed ChildMessage, now time.Time) (*Task, error) {
	t, err := scanTask(tx.QueryRowContext(ctx,
		`UPDATE agent_tasks
		SET status = ?, started_at = ?, prompt = ?, message_id = ?, updated_at = ?
		WHERE id = ? AND status = ?
		RETURNING `+taskColumns,
		string(StatusRunning), now.UnixNano(), claimed.Prompt, claimed.ID, now.UnixNano(),
		cur.ID, string(StatusPending)))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: pending promote lost for task %s", ErrInvalidTransition, cur.ID)
	}
	return t, err
}

// insertSuccessorTx creates the next attempt for a terminal child
// session: a fresh task id, the predecessor's identity and policy,
// run_generation + 1, resumes_task_id, the claimed message as prompt
// input, and running status. The UNIQUE(child_session_id,
// run_generation) index makes a double successor impossible.
func insertSuccessorTx(ctx context.Context, tx *sql.Tx, cur *Task, claimed ChildMessage, now time.Time) (*Task, error) {
	next := *cur
	next.ID = uuid.NewString()
	next.ChildSessionID = cur.ChildSessionID
	next.Prompt = claimed.Prompt
	next.MessageID = claimed.ID
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

	fallbacks, err := encodeFallbackModels(next.FallbackModels)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO agent_tasks (`+taskColumns+`) VALUES (`+taskPlaceholders+`)`,
		taskArgs(&next, fallbacks)...,
	); err != nil {
		return nil, err
	}
	return &next, nil
}

// appendStartOutboxTx durably records the dispatch of one attempt so
// a dropped live event can be resynced from SQLite.
func appendStartOutboxTx(ctx context.Context, tx *sql.Tx, t *Task, at time.Time) error {
	payload, err := json.Marshal(t)
	if err != nil {
		return fmt.Errorf("encode dispatched task %s: %w", t.ID, err)
	}
	return appendOutboxTx(ctx, tx, &OutboxEntry{
		ID:            uuid.NewString(),
		TaskID:        t.ID,
		RunGeneration: t.RunGeneration,
		EventType:     EventStarted,
		Payload:       string(payload),
		CreatedAt:     at,
	})
}

// CancelPendingIfLive terminalizes a pending attempt and rejects its
// bound admission message with reason, in one transaction, including
// the durable terminal delivery (outbox row, inbox row, and cost
// aggregation). Higher queued sequences are unchanged and dispatch as
// successor attempts after the cancellation terminalizes.
func (s *SQLiteStore) CancelPendingIfLive(ctx context.Context, id string, u TerminalUpdate, reason string) (*Task, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, fmt.Errorf("cancel pending task %s: %w", id, err)
	}
	defer tx.Rollback() //nolint:errcheck // best-effort after Commit

	t, err := scanTask(tx.QueryRowContext(ctx,
		`UPDATE agent_tasks
		SET status = ?, result = ?, summary = ?, error = ?,
			result_truncated = ?, completed_at = ?, updated_at = ?,
			terminal_generation = run_generation,
			prompt_tokens = ?, completion_tokens = ?, cost = ?
		WHERE id = ? AND status = ?
		RETURNING `+taskColumns,
		string(u.Status), u.Result, u.Summary, u.Err,
		boolInt(u.ResultTruncated), nullUnixNano(u.CompletedAt), unixOrZero(u.CompletedAt),
		u.Usage.PromptTokens, u.Usage.CompletionTokens, u.Usage.Cost,
		id, string(StatusPending)))
	if errors.Is(err, sql.ErrNoRows) {
		// Read through the open transaction: the shared pool has one
		// connection, and a second acquisition here would deadlock.
		current, getErr := scanTask(tx.QueryRowContext(ctx,
			`SELECT `+taskColumns+` FROM agent_tasks WHERE id = ?`, id))
		if getErr != nil {
			return nil, false, fmt.Errorf("cancel pending task %s: %w", id, getErr)
		}
		return current, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("cancel pending task %s: %w", id, err)
	}
	if err := deliverTerminalTx(ctx, tx, t, u.Usage); err != nil {
		return nil, false, fmt.Errorf("cancel pending task %s: %w", id, err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE agent_task_messages SET state = ?, reason = ?
		WHERE id = ? AND state = ? AND delivered_at IS NULL`,
		string(MessageRejected), reason, t.MessageID, string(MessageQueued),
	); err != nil {
		return nil, false, fmt.Errorf("cancel pending task %s: reject admission message: %w", id, err)
	}
	if err := tx.Commit(); err != nil {
		return nil, false, fmt.Errorf("cancel pending task %s: %w", id, err)
	}
	return t, true, nil
}

// ListChildMessages returns the child session's mailbox rows in
// sequence order.
func (s *SQLiteStore) ListChildMessages(ctx context.Context, childSessionID string) ([]ChildMessage, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+childMessageColumns+`
		FROM agent_task_messages
		WHERE child_session_id = ?
		ORDER BY sequence ASC`, childSessionID)
	if err != nil {
		return nil, fmt.Errorf("list mailbox for %s: %w", childSessionID, err)
	}
	defer rows.Close()

	var out []ChildMessage
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, fmt.Errorf("list mailbox for %s: %w", childSessionID, err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list mailbox for %s: %w", childSessionID, err)
	}
	return out, nil
}

func scanMessage(row rowScanner) (ChildMessage, error) {
	var (
		m           ChildMessage
		sequence    int64
		reason      sql.NullString
		deliveredAt sql.NullInt64
		createdAt   int64
	)
	err := row.Scan(
		&m.ID, &m.ChildSessionID, &m.TaskID, &m.OwnerSessionID,
		&sequence, &m.Origin, &m.Prompt, &m.Attachments, &m.State,
		&reason, &createdAt, &deliveredAt,
	)
	if err != nil {
		return ChildMessage{}, err
	}
	m.Sequence = uint64(sequence)
	m.Reason = reason.String
	m.CreatedAt = decodeStoredUnixNano(createdAt)
	m.DeliveredAt = decodeUnixNano(deliveredAt)
	return m, nil
}
