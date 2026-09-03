package task

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// terminalStatusList mirrors Status.Terminal(); the consistency test
// in store_sqlite_test.go pins the two together.
var terminalStatusList = []Status{
	StatusCompleted,
	StatusFailed,
	StatusCancelled,
	StatusInterrupted,
}

const taskColumns = `id, owner_session_id, parent_session_id, child_session_id,
	parent_message_id, tool_call_id, profile, profile_generation,
	requested_model, resolved_provider, resolved_model, fallback_models,
	prompt_fingerprint, tool_fingerprint, run_generation, resumes_task_id,
	message_id, terminal_generation, cost_aggregated_generation, status,
	prompt, result, summary, error, result_truncated, prompt_tokens,
	completion_tokens, cost, created_at, started_at, completed_at, updated_at`

// SQLiteStore is the durable Store implementation over the shared
// crush.db connection opened by internal/db.Connect. It adds no
// behavior beyond the Store contract: the manager remains the single
// writer and serializes its calls. Every dispatch method runs on one
// transaction over that connection; callers never see a raw
// *sql.Tx.
type SQLiteStore struct {
	db *sql.DB
	// terminalizeQuery is the fenced conditional UPDATE ... RETURNING
	// whose presence of a returned row decides the exactly-once
	// terminalization winner. The winning record comes back with the
	// statement itself, so a committed update can never be
	// misreported by a follow-up read. The WHERE clause fences on
	// the caller's run generation so a stale runner from a retired
	// attempt cannot terminalize its successor.
	terminalizeQuery string
}

var _ Store = (*SQLiteStore)(nil)

// NewSQLiteStore returns a store backed by db. Migrations must
// already be applied, which db.Connect guarantees for pooled
// connections.
func NewSQLiteStore(db *sql.DB) *SQLiteStore {
	placeholders := strings.TrimSuffix(strings.Repeat("?, ", len(terminalStatusList)), ", ")
	q := `UPDATE agent_tasks
	SET status = ?, result = ?, summary = ?, error = ?,
		result_truncated = ?, completed_at = ?, updated_at = ?,
		terminal_generation = run_generation,
		prompt_tokens = ?, completion_tokens = ?, cost = ?
	WHERE id = ? AND run_generation = ? AND status NOT IN (` + placeholders + `)
	RETURNING ` + taskColumns
	return &SQLiteStore{db: db, terminalizeQuery: q}
}

// Save inserts or replaces the full record.
func (s *SQLiteStore) Save(ctx context.Context, t *Task) error {
	fallbacks, err := encodeFallbackModels(t.FallbackModels)
	if err != nil {
		return fmt.Errorf("save task %s: %w", t.ID, err)
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO agent_tasks (`+taskColumns+`)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT (id) DO UPDATE SET
		owner_session_id = excluded.owner_session_id,
		parent_session_id = excluded.parent_session_id,
		child_session_id = excluded.child_session_id,
		parent_message_id = excluded.parent_message_id,
		tool_call_id = excluded.tool_call_id,
		profile = excluded.profile,
		profile_generation = excluded.profile_generation,
		requested_model = excluded.requested_model,
		resolved_provider = excluded.resolved_provider,
		resolved_model = excluded.resolved_model,
		fallback_models = excluded.fallback_models,
		prompt_fingerprint = excluded.prompt_fingerprint,
		tool_fingerprint = excluded.tool_fingerprint,
		run_generation = excluded.run_generation,
		resumes_task_id = excluded.resumes_task_id,
		message_id = excluded.message_id,
		terminal_generation = excluded.terminal_generation,
		cost_aggregated_generation = excluded.cost_aggregated_generation,
		status = excluded.status,
		prompt = excluded.prompt,
		result = excluded.result,
		summary = excluded.summary,
		error = excluded.error,
		result_truncated = excluded.result_truncated,
		prompt_tokens = excluded.prompt_tokens,
		completion_tokens = excluded.completion_tokens,
		cost = excluded.cost,
		created_at = excluded.created_at,
		started_at = excluded.started_at,
		completed_at = excluded.completed_at,
		updated_at = excluded.updated_at`,
		taskArgs(t, fallbacks)...,
	)
	if err != nil {
		return fmt.Errorf("save task %s: %w", t.ID, err)
	}
	return nil
}

// taskArgs renders t as the positional arguments of an INSERT over
// taskColumns, in column order.
func taskArgs(t *Task, fallbackModelsJSON string) []any {
	return []any{
		t.ID, t.OwnerSessionID, nullString(t.ParentSessionID), t.ChildSessionID,
		t.ParentMessageID, t.ToolCallID, t.Profile, int64(t.ProfileGeneration),
		nullString(t.RequestedModel), t.Provider, t.Model, fallbackModelsJSON,
		t.PromptFingerprint, t.ToolFingerprint, int64(t.RunGeneration),
		nullString(t.ResumesTaskID), nullString(t.MessageID),
		nullGeneration(t.TerminalGeneration), nullGeneration(t.CostAggregatedGeneration),
		string(t.Status), t.Prompt, t.Result, t.Summary, t.Err,
		boolInt(t.ResultTruncated), t.PromptTokens, t.CompletionTokens, t.Cost,
		unixOrZero(t.CreatedAt), nullUnixNano(t.StartedAt), nullUnixNano(t.CompletedAt),
		unixOrZero(t.UpdatedAt),
	}
}

// Get returns a copy of the stored task or ErrNotFound.
func (s *SQLiteStore) Get(ctx context.Context, id string) (*Task, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+taskColumns+` FROM agent_tasks WHERE id = ?`, id)
	t, err := scanTask(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get task %s: %w", id, err)
	}
	return t, nil
}

// ListByOwner returns copies of the owner's tasks, oldest first.
func (s *SQLiteStore) ListByOwner(ctx context.Context, ownerSessionID string) ([]*Task, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+taskColumns+` FROM agent_tasks
		WHERE owner_session_id = ?
		ORDER BY created_at ASC, id ASC`, ownerSessionID)
	if err != nil {
		return nil, fmt.Errorf("list tasks for owner %s: %w", ownerSessionID, err)
	}
	defer rows.Close()

	var out []*Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, fmt.Errorf("list tasks for owner %s: %w", ownerSessionID, err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list tasks for owner %s: %w", ownerSessionID, err)
	}
	return out, nil
}

// rowScanner accepts both *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanTask(row rowScanner) (*Task, error) {
	var (
		t             Task
		parentSession sql.NullString
		requested     sql.NullString
		fallbacks     string
		resumes       sql.NullString
		messageID     sql.NullString
		terminalGen   sql.NullInt64
		costGen       sql.NullInt64
		status        string
		runGen        int64
		profileGen    int64
		truncated     int64
		createdAt     int64
		updatedAt     int64
		startedAt     sql.NullInt64
		completedAt   sql.NullInt64
	)
	err := row.Scan(
		&t.ID, &t.OwnerSessionID, &parentSession, &t.ChildSessionID,
		&t.ParentMessageID, &t.ToolCallID, &t.Profile, &profileGen,
		&requested, &t.Provider, &t.Model, &fallbacks,
		&t.PromptFingerprint, &t.ToolFingerprint, &runGen, &resumes,
		&messageID, &terminalGen, &costGen, &status,
		&t.Prompt, &t.Result, &t.Summary, &t.Err, &truncated,
		&t.PromptTokens, &t.CompletionTokens, &t.Cost,
		&createdAt, &startedAt, &completedAt, &updatedAt,
	)
	if err != nil {
		return nil, err
	}
	t.ParentSessionID = parentSession.String
	t.RequestedModel = requested.String
	t.ResumesTaskID = resumes.String
	t.MessageID = messageID.String
	t.ProfileGeneration = uint64(profileGen)
	t.RunGeneration = uint64(runGen)
	t.TerminalGeneration = uint64(terminalGen.Int64)
	t.CostAggregatedGeneration = uint64(costGen.Int64)
	t.Status = Status(status)
	t.ResultTruncated = truncated != 0
	t.CreatedAt = decodeStoredUnixNano(createdAt)
	t.UpdatedAt = decodeStoredUnixNano(updatedAt)
	t.StartedAt = decodeUnixNano(startedAt)
	t.CompletedAt = decodeUnixNano(completedAt)
	if fallbacks != "" && fallbacks != "[]" {
		if err := json.Unmarshal([]byte(fallbacks), &t.FallbackModels); err != nil {
			return nil, fmt.Errorf("decode fallback models for task: %w", err)
		}
	}
	return &t, nil
}
