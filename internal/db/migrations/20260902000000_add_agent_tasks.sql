-- +goose Up
-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS agent_tasks (
    id TEXT PRIMARY KEY,
    owner_session_id TEXT NOT NULL,
    parent_session_id TEXT NOT NULL,
    child_session_id TEXT NOT NULL DEFAULT '',
    resumes_task_id TEXT NOT NULL DEFAULT '',
    profile TEXT NOT NULL DEFAULT '',
    resolved_provider TEXT NOT NULL DEFAULT '',
    resolved_model TEXT NOT NULL DEFAULT '',
    run_generation INTEGER NOT NULL DEFAULT 1,
    prompt TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL,
    result TEXT NOT NULL DEFAULT '',
    summary TEXT NOT NULL DEFAULT '',
    error TEXT NOT NULL DEFAULT '',
    result_truncated INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL,  -- Unix timestamp in nanoseconds
    started_at INTEGER,           -- Unix nanoseconds; NULL until first start
    completed_at INTEGER          -- Unix nanoseconds; NULL until terminal
);

CREATE INDEX IF NOT EXISTS idx_agent_tasks_owner_created ON agent_tasks (owner_session_id, created_at, id);
CREATE INDEX IF NOT EXISTS idx_agent_tasks_parent_status ON agent_tasks (parent_session_id, status);

CREATE TABLE IF NOT EXISTS agent_task_outbox (
    id TEXT PRIMARY KEY,
    task_id TEXT NOT NULL,
    run_generation INTEGER NOT NULL,
    event_type TEXT NOT NULL,
    payload TEXT NOT NULL,
    delivered_at INTEGER,  -- Unix timestamp in nanoseconds; NULL until delivered
    created_at INTEGER NOT NULL,
    UNIQUE (task_id, run_generation, event_type)
);

CREATE INDEX IF NOT EXISTS idx_agent_task_outbox_undelivered ON agent_task_outbox (delivered_at, created_at);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_agent_task_outbox_undelivered;
DROP TABLE IF EXISTS agent_task_outbox;
DROP INDEX IF EXISTS idx_agent_tasks_parent_status;
DROP INDEX IF EXISTS idx_agent_tasks_owner_created;
DROP TABLE IF EXISTS agent_tasks;
-- +goose StatementEnd
