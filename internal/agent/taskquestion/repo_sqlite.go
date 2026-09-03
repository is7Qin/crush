package taskquestion

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/charmbracelet/crush/internal/question"
)

// SQLiteRepository is the durable Repository implementation over
// the shared crush.db connection opened by internal/db.Connect,
// mirroring the raw-SQL pattern of task.SQLiteStore. The question
// batch and answers are stored as their JSON encodings so resync
// never reconstructs a question from transient pub/sub payloads.
type SQLiteRepository struct {
	db *sql.DB
	// resolveQuery is the conditional UPDATE whose row count decides
	// the exactly-once resolution winner.
	resolveQuery string
}

var _ Repository = (*SQLiteRepository)(nil)

const tqColumns = `question_id, task_id, owner_session_id, child_session_id,
	run_generation, batch, answers, status, created_at, resolved_at`

// NewSQLiteRepository returns a repository backed by db. Migrations
// must already be applied, which db.Connect guarantees for pooled
// connections.
func NewSQLiteRepository(db *sql.DB) *SQLiteRepository {
	return &SQLiteRepository{
		db: db,
		resolveQuery: `UPDATE agent_task_questions
	SET status = ?, answers = ?, resolved_at = ?
	WHERE question_id = ? AND status = 'pending'`,
	}
}

// Save inserts or replaces the full record.
func (r *SQLiteRepository) Save(ctx context.Context, q TaskQuestion) error {
	batch, err := EncodeBatch(q.Batch)
	if err != nil {
		return fmt.Errorf("encode task question batch %s: %w", q.QuestionID, err)
	}
	answers, err := EncodeAnswers(q.Answers)
	if err != nil {
		return fmt.Errorf("encode task question answers %s: %w", q.QuestionID, err)
	}
	_, err = r.db.ExecContext(ctx, `INSERT INTO agent_task_questions (`+tqColumns+`)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT (question_id) DO UPDATE SET
		status = excluded.status,
		answers = excluded.answers,
		resolved_at = excluded.resolved_at`,
		q.QuestionID, q.TaskID, q.OwnerSessionID, q.ChildSessionID,
		int64(q.RunGeneration), batch, answers, string(q.Resolution),
		q.CreatedAt.UnixNano(), nullUnixNano(q.ResolvedAt),
	)
	if err != nil {
		return fmt.Errorf("save task question %s: %w", q.QuestionID, err)
	}
	return nil
}

// Resolve applies u only while the stored row is still pending. The
// conditional UPDATE's row count decides the winner.
func (r *SQLiteRepository) Resolve(ctx context.Context, questionID string, u ResolutionUpdate) (bool, error) {
	answers, err := EncodeAnswers(u.Answers)
	if err != nil {
		return false, fmt.Errorf("encode task question answers %s: %w", questionID, err)
	}
	res, err := r.db.ExecContext(ctx, r.resolveQuery,
		string(u.Resolution), answers, nullUnixNano(u.ResolvedAt), questionID,
	)
	if err != nil {
		return false, fmt.Errorf("resolve task question %s: %w", questionID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("resolve task question %s: %w", questionID, err)
	}
	return n == 1, nil
}

// listUnresolved runs the pending-only ordered listing, optionally
// filtered to one owner session.
func (r *SQLiteRepository) listUnresolved(ctx context.Context, ownerSessionID string) ([]TaskQuestion, error) {
	q := `SELECT ` + tqColumns + ` FROM agent_task_questions WHERE status = 'pending'`
	args := []any{}
	if ownerSessionID != "" {
		q += ` AND owner_session_id = ?`
		args = append(args, ownerSessionID)
	}
	q += ` ORDER BY created_at ASC, question_id ASC`
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list unresolved task questions: %w", err)
	}
	defer rows.Close()

	var out []TaskQuestion
	for rows.Next() {
		item, err := scanTaskQuestion(rows)
		if err != nil {
			return nil, fmt.Errorf("list unresolved task questions: %w", err)
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list unresolved task questions: %w", err)
	}
	return out, nil
}

// ListUnresolved returns copies of pending records, oldest first.
func (r *SQLiteRepository) ListUnresolved(ctx context.Context) ([]TaskQuestion, error) {
	return r.listUnresolved(ctx, "")
}

// ListUnresolvedForOwner returns copies of the owner's pending
// records, oldest first.
func (r *SQLiteRepository) ListUnresolvedForOwner(ctx context.Context, ownerSessionID string) ([]TaskQuestion, error) {
	return r.listUnresolved(ctx, ownerSessionID)
}

// rowScanner accepts both *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanTaskQuestion(row rowScanner) (TaskQuestion, error) {
	var (
		q          TaskQuestion
		batch      string
		answers    string
		status     string
		runGen     int64
		createdAt  int64
		resolvedAt sql.NullInt64
	)
	err := row.Scan(
		&q.QuestionID, &q.TaskID, &q.OwnerSessionID, &q.ChildSessionID,
		&runGen, &batch, &answers, &status, &createdAt, &resolvedAt,
	)
	if err != nil {
		return TaskQuestion{}, err
	}
	if err := json.Unmarshal([]byte(batch), &q.Batch); err != nil {
		return TaskQuestion{}, fmt.Errorf("decode task question batch %s: %w", q.QuestionID, err)
	}
	if answers != "" {
		if err := json.Unmarshal([]byte(answers), &q.Answers); err != nil {
			return TaskQuestion{}, fmt.Errorf("decode task question answers %s: %w", q.QuestionID, err)
		}
	}
	q.RunGeneration = uint64(runGen)
	q.Resolution = Resolution(status)
	q.CreatedAt = time.Unix(0, createdAt).UTC()
	if resolvedAt.Valid {
		q.ResolvedAt = time.Unix(0, resolvedAt.Int64).UTC()
	}
	return q, nil
}

// EncodeBatch renders a question batch for the durable batch column.
// It is exported so the App-owned lifecycle bridge can encode rows
// with the exact schema this package owns.
func EncodeBatch(batch question.Request) (string, error) {
	data, err := json.Marshal(batch)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// EncodeAnswers renders answers for storage; the empty string is the
// unresolved sentinel.
func EncodeAnswers(answers []question.Answer) (string, error) {
	if len(answers) == 0 {
		return "", nil
	}
	data, err := json.Marshal(answers)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// nullUnixNano encodes a possibly-zero time as NULL so zero times
// round-trip as zero.
func nullUnixNano(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UnixNano()
}
