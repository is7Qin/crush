package task

// SQLite terminal-delivery transaction: the fenced terminalization,
// its parent cost aggregation, the durable outbox and inbox rows, and
// the owner-scoped inbox reads. One file owns the whole "commit or
// roll back everything" path.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// TerminalizeAndDeliver runs the whole terminal transaction: the
// conditional UPDATE fenced on (id, run_generation, not terminal)
// decides the winner and returns the updated record with the
// statement itself, so a committed terminalization never depends on
// a follow-up read that could misreport it as a failure. The same
// transaction aggregates parent usage once for the generation and
// inserts the outbox and inbox rows; any error rolls all of it back.
// Only the no-row (lost or unknown id) path reads again, inside the
// still-open transaction, where a failure cannot have hidden a win.
func (s *SQLiteStore) TerminalizeAndDeliver(ctx context.Context, id string, runGeneration uint64, u TerminalUpdate) (*Task, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, fmt.Errorf("terminalize task %s: %w", id, err)
	}
	defer tx.Rollback() //nolint:errcheck // best-effort after Commit

	args := []any{
		string(u.Status), u.Result, u.Summary, u.Err,
		boolInt(u.ResultTruncated), nullUnixNano(u.CompletedAt), unixOrZero(u.CompletedAt),
		u.Usage.PromptTokens, u.Usage.CompletionTokens, u.Usage.Cost,
		id, int64(runGeneration),
	}
	for _, st := range terminalStatusList {
		args = append(args, string(st))
	}
	// The optional question fence runs first inside the same
	// transaction: a question row that is no longer pending (or
	// whose identity does not match) rolls everything back before
	// the task is touched, so a lost resolution never terminalizes.
	if u.Question != nil {
		if err := resolveQuestionTx(ctx, tx, *u.Question); err != nil {
			return nil, false, fmt.Errorf("terminalize task %s: %w", id, err)
		}
	}
	t, err := scanTask(tx.QueryRowContext(ctx, s.terminalizeQuery, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return s.lostTerminalTx(ctx, tx, id)
	}
	if err != nil {
		return nil, false, fmt.Errorf("terminalize task %s: %w", id, err)
	}
	if err := deliverTerminalTx(ctx, tx, t, u.Usage); err != nil {
		return nil, false, fmt.Errorf("terminalize task %s: %w", id, err)
	}
	if err := tx.Commit(); err != nil {
		return nil, false, fmt.Errorf("terminalize task %s: %w", id, err)
	}
	return t, true, nil
}

// lostTerminalTx reports the committed record for a call that lost
// the conditional terminalization. It reads through the open
// transaction: the shared pool has one connection, and a second
// acquisition here would deadlock.
func (s *SQLiteStore) lostTerminalTx(ctx context.Context, tx *sql.Tx, id string) (*Task, bool, error) {
	current, err := scanTask(tx.QueryRowContext(ctx,
		`SELECT `+taskColumns+` FROM agent_tasks WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, ErrNotFound
	}
	if err != nil {
		return nil, false, fmt.Errorf("terminalize task %s: %w", id, err)
	}
	return current, false, nil
}

// ListLiveTasks returns copies of every non-terminal task, oldest
// first, for startup recovery.
func (s *SQLiteStore) ListLiveTasks(ctx context.Context) ([]*Task, error) {
	live := liveStatusList()
	args := make([]any, 0, len(live))
	for _, st := range live {
		args = append(args, string(st))
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+taskColumns+` FROM agent_tasks
		WHERE status IN (`+strings.TrimSuffix(strings.Repeat("?, ", len(live)), ", ")+`)
		ORDER BY created_at ASC, id ASC`, args...)
	if err != nil {
		return nil, fmt.Errorf("list live tasks: %w", err)
	}
	defer rows.Close()

	var out []*Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, fmt.Errorf("list live tasks: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list live tasks: %w", err)
	}
	return out, nil
}

// liveStatusList is the complement of terminalStatusList over the
// live statuses recovery and dispatch observe.
func liveStatusList() []Status {
	return []Status{StatusPending, StatusRunning, StatusWaitingForInput}
}

const inboxColumns = `id, owner_session_id, task_id, terminal_generation,
  payload, delivered_at, created_at`

// deliverTerminalTx performs the durable half of a won
// terminalization inside tx: aggregate this generation's usage into
// the parent session exactly once, insert the terminal outbox row,
// and insert the parent inbox row. Outbox and inbox inserts are
// conflict-ignore on their documented unique keys, so an interrupted
// recovery replay can never duplicate a row. Any error aborts the
// caller's whole terminal transaction.
func deliverTerminalTx(ctx context.Context, tx *sql.Tx, t *Task, usage UsageDelta) error {
	if t.ParentSessionID != "" && t.CostAggregatedGeneration != t.RunGeneration {
		if _, err := tx.ExecContext(ctx, `UPDATE sessions
			SET prompt_tokens = prompt_tokens + ?,
				completion_tokens = completion_tokens + ?,
				cost = cost + ?,
				updated_at = strftime('%s', 'now')
			WHERE id = ?`,
			usage.PromptTokens, usage.CompletionTokens, usage.Cost, t.ParentSessionID,
		); err != nil {
			return fmt.Errorf("aggregate parent %s usage: %w", t.ParentSessionID, err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE agent_tasks SET cost_aggregated_generation = ? WHERE id = ?`,
			int64(t.RunGeneration), t.ID,
		); err != nil {
			return fmt.Errorf("fence cost aggregation for task %s: %w", t.ID, err)
		}
		t.CostAggregatedGeneration = t.RunGeneration
	}
	if err := appendOutboxTx(ctx, tx, terminalOutboxEntry(t)); err != nil {
		return err
	}
	if err := appendInboxTx(ctx, tx, inboxEntryFor(t)); err != nil {
		return err
	}
	// Recovery and shutdown semantics require that a terminalized
	// task never leaves a durable pending question: this task's
	// still-pending question rows resolve as interrupted in the
	// same transaction.
	return interruptPendingQuestionsTx(ctx, tx, t)
}

// appendOutboxTx inserts one lifecycle entry, ignoring a repeat of
// the unique (task_id, run_generation, event_type) key.
func appendOutboxTx(ctx context.Context, tx *sql.Tx, e *OutboxEntry) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO agent_task_outbox
		(id, task_id, run_generation, event_type, payload, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (task_id, run_generation, event_type) DO NOTHING`,
		e.ID, e.TaskID, int64(e.RunGeneration), string(e.EventType), e.Payload,
		unixOrZero(e.CreatedAt),
	)
	return err
}

// appendInboxTx inserts one parent delivery row, ignoring a repeat
// of the unique (owner_session_id, task_id, terminal_generation) key.
func appendInboxTx(ctx context.Context, tx *sql.Tx, e *InboxEntry) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO agent_task_inbox
		(id, owner_session_id, task_id, terminal_generation, payload, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (owner_session_id, task_id, terminal_generation) DO NOTHING`,
		e.ID, e.OwnerSessionID, e.TaskID, int64(e.TerminalGeneration), e.Payload,
		unixOrZero(e.CreatedAt),
	)
	return err
}

// ListInbox returns copies of the undelivered inbox rows, oldest
// first. An empty ownerSessionID lists every owner's rows.
func (s *SQLiteStore) ListInbox(ctx context.Context, ownerSessionID string) ([]*InboxEntry, error) {
	q := `SELECT ` + inboxColumns + ` FROM agent_task_inbox WHERE delivered_at IS NULL`
	args := []any{}
	if ownerSessionID != "" {
		q += ` AND owner_session_id = ?`
		args = append(args, ownerSessionID)
	}
	q += ` ORDER BY created_at ASC, id ASC`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list task inbox: %w", err)
	}
	defer rows.Close()

	var out []*InboxEntry
	for rows.Next() {
		e, err := scanInbox(rows)
		if err != nil {
			return nil, fmt.Errorf("list task inbox: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list task inbox: %w", err)
	}
	return out, nil
}

// AckInbox stamps the given rows delivered. Unknown ids are ignored.
func (s *SQLiteStore) AckInbox(ctx context.Context, ids []string, deliveredAt time.Time) error {
	if len(ids) == 0 {
		return nil
	}
	args := make([]any, 0, len(ids)+1)
	args = append(args, deliveredAt.UnixNano())
	for _, id := range ids {
		args = append(args, id)
	}
	q := `UPDATE agent_task_inbox SET delivered_at = ?
		WHERE id IN (` + strings.TrimSuffix(strings.Repeat("?, ", len(ids)), ", ") + `)`
	if _, err := s.db.ExecContext(ctx, q, args...); err != nil {
		return fmt.Errorf("ack task inbox: %w", err)
	}
	return nil
}

func scanInbox(row rowScanner) (*InboxEntry, error) {
	var (
		e           InboxEntry
		terminalGen int64
		deliveredAt sql.NullInt64
		createdAt   int64
	)
	err := row.Scan(
		&e.ID, &e.OwnerSessionID, &e.TaskID, &terminalGen,
		&e.Payload, &deliveredAt, &createdAt,
	)
	if err != nil {
		return nil, err
	}
	e.TerminalGeneration = uint64(terminalGen)
	e.DeliveredAt = decodeUnixNano(deliveredAt)
	e.CreatedAt = decodeStoredUnixNano(createdAt)
	return &e, nil
}
