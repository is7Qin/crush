-- +goose Up
-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS agent_task_questions (
    question_id TEXT PRIMARY KEY,
    task_id TEXT NOT NULL,
    owner_session_id TEXT NOT NULL,
    child_session_id TEXT NOT NULL,
    run_generation INTEGER NOT NULL DEFAULT 1,
    batch TEXT NOT NULL,                -- encoded question.Request
    answers TEXT NOT NULL DEFAULT '',   -- encoded []question.Answer; '' until resolved
    status TEXT NOT NULL DEFAULT 'pending',  -- pending | answered | cancelled | timed_out | interrupted
    created_at INTEGER NOT NULL,        -- Unix timestamp in nanoseconds
    resolved_at INTEGER                 -- Unix nanoseconds; NULL until resolved
);

-- One unresolved question per task, enforced in storage the same way
-- the service enforces it in memory.
CREATE UNIQUE INDEX IF NOT EXISTS idx_agent_task_questions_pending ON agent_task_questions (task_id) WHERE status = 'pending';
CREATE INDEX IF NOT EXISTS idx_agent_task_questions_status ON agent_task_questions (status, created_at);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_agent_task_questions_status;
DROP INDEX IF EXISTS idx_agent_task_questions_pending;
DROP TABLE IF EXISTS agent_task_questions;
-- +goose StatementEnd
