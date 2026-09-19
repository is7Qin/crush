-- +goose Up
-- +goose StatementBegin
ALTER TABLE agent_task_inbox ADD COLUMN consumed_at INTEGER;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE agent_task_inbox DROP COLUMN consumed_at;
-- +goose StatementEnd
