-- +goose Up
-- +goose StatementBegin
-- Rebuild agent_tasks with the full dispatch/metadata schema from
-- docs/specs/crush-agents/10-remaining-integration.md: parent message
-- and tool-call correlation, profile generation, requested/fallback
-- models, fingerprints, continuation lineage (resumes_task_id,
-- message_id), terminal/cost fencing generations, usage, and
-- updated_at. The UNIQUE(child_session_id, run_generation) pair is a
-- partial index so pre-dispatch rows without a bound child can
-- coexist until the dispatch transaction binds one.
CREATE TABLE agent_tasks_new (
    id TEXT PRIMARY KEY,
    owner_session_id TEXT NOT NULL,
    parent_session_id TEXT,                          -- NULL after parent deletion
    child_session_id TEXT NOT NULL,
    parent_message_id TEXT NOT NULL,
    tool_call_id TEXT NOT NULL,
    profile TEXT NOT NULL,
    profile_generation INTEGER NOT NULL,
    requested_model TEXT,                            -- NULL when the profile model was used
    resolved_provider TEXT NOT NULL,
    resolved_model TEXT NOT NULL,
    fallback_models TEXT NOT NULL DEFAULT '[]',      -- JSON array text
    prompt_fingerprint TEXT NOT NULL,
    tool_fingerprint TEXT NOT NULL,
    run_generation INTEGER NOT NULL,
    resumes_task_id TEXT,                            -- prior terminal attempt in the same child session
    message_id TEXT,                                 -- mailbox message that caused this attempt
    terminal_generation INTEGER,                     -- NULL until terminalized
    cost_aggregated_generation INTEGER,              -- NULL until usage aggregated into the parent
    status TEXT NOT NULL,
    prompt TEXT NOT NULL,
    result TEXT NOT NULL DEFAULT '',
    summary TEXT NOT NULL DEFAULT '',
    error TEXT NOT NULL DEFAULT '',
    result_truncated INTEGER NOT NULL DEFAULT 0,
    prompt_tokens INTEGER NOT NULL DEFAULT 0,
    completion_tokens INTEGER NOT NULL DEFAULT 0,
    cost REAL NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL,                     -- Unix timestamp in nanoseconds
    started_at INTEGER,                              -- Unix nanoseconds; NULL until first start
    completed_at INTEGER,                            -- Unix nanoseconds; NULL until terminal
    updated_at INTEGER NOT NULL DEFAULT 0            -- Unix nanoseconds
);

INSERT INTO agent_tasks_new (
    id, owner_session_id, parent_session_id, child_session_id,
    parent_message_id, tool_call_id, profile, profile_generation,
    requested_model, resolved_provider, resolved_model, fallback_models,
    prompt_fingerprint, tool_fingerprint, run_generation, resumes_task_id,
    message_id, terminal_generation, cost_aggregated_generation, status,
    prompt, result, summary, error, result_truncated, prompt_tokens,
    completion_tokens, cost, created_at, started_at, completed_at, updated_at
)
SELECT
    id, owner_session_id, NULLIF(parent_session_id, ''), child_session_id,
    '', '', profile, 0,
    NULL, resolved_provider, resolved_model, '[]',
    '', '', run_generation, NULLIF(resumes_task_id, ''),
    NULL, NULL, NULL, status,
    prompt, result, summary, error, result_truncated, 0,
    0, 0, created_at, started_at, completed_at, created_at
FROM agent_tasks;

DROP TABLE agent_tasks;
ALTER TABLE agent_tasks_new RENAME TO agent_tasks;

CREATE INDEX IF NOT EXISTS idx_agent_tasks_owner_created ON agent_tasks (owner_session_id, created_at, id);
CREATE INDEX IF NOT EXISTS idx_agent_tasks_parent_status ON agent_tasks (parent_session_id, status);
CREATE INDEX IF NOT EXISTS idx_agent_tasks_child ON agent_tasks (child_session_id);
CREATE INDEX IF NOT EXISTS idx_agent_tasks_status_updated ON agent_tasks (status, updated_at);
CREATE UNIQUE INDEX IF NOT EXISTS idx_agent_tasks_child_generation ON agent_tasks (child_session_id, run_generation) WHERE child_session_id != '';
-- +goose StatementEnd

-- +goose StatementBegin
-- Durable child mailbox: every child-session turn (the initial
-- call_agent prompt as sequence zero, then parent/user messages in
-- FIFO order) is one row claimed by a task attempt at dispatch time.
CREATE TABLE IF NOT EXISTS agent_task_messages (
    id TEXT PRIMARY KEY,
    child_session_id TEXT NOT NULL,
    task_id TEXT NOT NULL,
    owner_session_id TEXT NOT NULL,
    sequence INTEGER NOT NULL,
    origin TEXT NOT NULL,                            -- parent | user
    prompt TEXT NOT NULL,
    attachments TEXT NOT NULL DEFAULT '[]',          -- JSON array text
    state TEXT NOT NULL,                             -- queued | delivered | rejected
    reason TEXT,                                     -- stable rejection code; NULL unless rejected
    created_at INTEGER NOT NULL,                     -- Unix timestamp in nanoseconds
    delivered_at INTEGER,                            -- Unix nanoseconds; NULL until claimed
    UNIQUE (child_session_id, sequence)
);

CREATE INDEX IF NOT EXISTS idx_agent_task_messages_queued ON agent_task_messages (child_session_id, state, sequence);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_agent_task_messages_queued;
DROP TABLE IF EXISTS agent_task_messages;

CREATE TABLE agent_tasks_old (
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
    created_at INTEGER NOT NULL,
    started_at INTEGER,
    completed_at INTEGER
);

INSERT INTO agent_tasks_old (
    id, owner_session_id, parent_session_id, child_session_id,
    resumes_task_id, profile, resolved_provider, resolved_model,
    run_generation, prompt, status, result, summary, error,
    result_truncated, created_at, started_at, completed_at
)
SELECT
    id, owner_session_id, COALESCE(parent_session_id, ''), child_session_id,
    COALESCE(resumes_task_id, ''), profile, resolved_provider, resolved_model,
    run_generation, prompt, status, result, summary, error,
    result_truncated, created_at, started_at, completed_at
FROM agent_tasks;

DROP TABLE agent_tasks;
ALTER TABLE agent_tasks_old RENAME TO agent_tasks;

CREATE INDEX IF NOT EXISTS idx_agent_tasks_owner_created ON agent_tasks (owner_session_id, created_at, id);
CREATE INDEX IF NOT EXISTS idx_agent_tasks_parent_status ON agent_tasks (parent_session_id, status);
-- +goose StatementEnd
