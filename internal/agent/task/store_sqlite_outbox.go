package task

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

const outboxColumns = `id, task_id, run_generation, event_type, payload,
  delivered_at, created_at`

// ListOutbox returns the undelivered entries, oldest first.
func (s *SQLiteStore) ListOutbox(ctx context.Context) ([]*OutboxEntry, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+outboxColumns+`
		FROM agent_task_outbox
		WHERE delivered_at IS NULL
		ORDER BY created_at ASC, id ASC`)
	if err != nil {
		return nil, fmt.Errorf("list task outbox: %w", err)
	}
	defer rows.Close()

	var out []*OutboxEntry
	for rows.Next() {
		e, err := scanOutbox(rows)
		if err != nil {
			return nil, fmt.Errorf("list task outbox: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list task outbox: %w", err)
	}
	return out, nil
}

// AckOutbox stamps the given entries delivered. Unknown ids are
// ignored.
func (s *SQLiteStore) AckOutbox(ctx context.Context, ids []string, deliveredAt time.Time) error {
	if len(ids) == 0 {
		return nil
	}
	args := make([]any, 0, len(ids)+1)
	args = append(args, deliveredAt.UnixNano())
	for _, id := range ids {
		args = append(args, id)
	}
	q := `UPDATE agent_task_outbox SET delivered_at = ?
		WHERE id IN (` + strings.TrimSuffix(strings.Repeat("?, ", len(ids)), ", ") + `)`
	if _, err := s.db.ExecContext(ctx, q, args...); err != nil {
		return fmt.Errorf("ack task outbox: %w", err)
	}
	return nil
}

func scanOutbox(row rowScanner) (*OutboxEntry, error) {
	var (
		e           OutboxEntry
		runGen      int64
		deliveredAt sql.NullInt64
		createdAt   int64
	)
	err := row.Scan(
		&e.ID, &e.TaskID, &runGen, &e.EventType, &e.Payload,
		&deliveredAt, &createdAt,
	)
	if err != nil {
		return nil, err
	}
	e.RunGeneration = uint64(runGen)
	e.DeliveredAt = decodeUnixNano(deliveredAt)
	e.CreatedAt = time.Unix(0, createdAt).UTC()
	return &e, nil
}
