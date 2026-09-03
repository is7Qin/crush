-- +goose Up
-- +goose StatementBegin
-- Durable parent inbox: one result envelope per terminalized task
-- generation, written inside the terminal transaction alongside the
-- task update, cost aggregation, and outbox row. owner_session_id is
-- the immutable task owner (never an FK, never cleared), so a deleted
-- parent's rows stay retained until authorized resync drains them.
CREATE TABLE IF NOT EXISTS agent_task_inbox (
    id TEXT PRIMARY KEY,
    owner_session_id TEXT NOT NULL,
    task_id TEXT NOT NULL,
    terminal_generation INTEGER NOT NULL,
    payload TEXT NOT NULL,                           -- JSON result envelope
    delivered_at INTEGER,                            -- Unix nanoseconds; NULL until delivered
    created_at INTEGER NOT NULL,                     -- Unix nanoseconds
    UNIQUE (owner_session_id, task_id, terminal_generation)
);

CREATE INDEX IF NOT EXISTS idx_agent_task_inbox_undelivered ON agent_task_inbox (owner_session_id, delivered_at, created_at);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_agent_task_inbox_undelivered;
DROP TABLE IF EXISTS agent_task_inbox;
-- +goose StatementEnd
