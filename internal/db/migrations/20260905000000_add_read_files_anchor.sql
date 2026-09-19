-- +goose Up
-- +goose StatementBegin
ALTER TABLE read_files ADD COLUMN content_sha256_8 TEXT;
ALTER TABLE read_files ADD COLUMN lines INTEGER;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE read_files DROP COLUMN lines;
ALTER TABLE read_files DROP COLUMN content_sha256_8;
-- +goose StatementEnd
