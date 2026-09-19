-- name: RecordFileRead :exec
INSERT INTO read_files (
    session_id,
    path,
    read_at,
    content_sha256_8,
    lines
) VALUES (
    ?,
    ?,
    strftime('%s', 'now'),
    ?,
    ?
) ON CONFLICT(path, session_id) DO UPDATE SET
    read_at = excluded.read_at,
    content_sha256_8 = excluded.content_sha256_8,
    lines = excluded.lines;

-- name: GetFileRead :one
SELECT session_id, path, read_at, content_sha256_8, lines FROM read_files
WHERE session_id = ? AND path = ? LIMIT 1;

-- name: ListSessionReadFiles :many
SELECT session_id, path, read_at, content_sha256_8, lines FROM read_files
WHERE session_id = ?
ORDER BY read_at DESC;
