package filetracker

import (
	"testing"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/stretchr/testify/require"
)

func TestService_RecordReadStoresAnchor(t *testing.T) {
	env := setupTest(t)

	sessionID := "anchor-session"
	env.createSession(t, sessionID)

	env.svc.RecordRead(env.ctx, sessionID, "rel/file.go", "package main\n")

	got, err := env.q.GetFileRead(env.ctx, db.GetFileReadParams{
		SessionID: sessionID,
		Path:      relpath("rel/file.go"),
	})
	require.NoError(t, err)
	require.True(t, got.ContentSha256_8.Valid)
	require.True(t, got.Lines.Valid)
	require.Equal(t, int64(1), got.Lines.Int64)
	sha, lines := HashContent("package main\n")
	require.Equal(t, sha, got.ContentSha256_8.String)
	require.Equal(t, int64(lines), got.Lines.Int64)
}

func TestService_LatestAnchor(t *testing.T) {
	env := setupTest(t)

	sessionID := "latest-anchor-session"
	env.createSession(t, sessionID)

	_, ok := env.svc.LatestAnchor(env.ctx, sessionID)
	require.False(t, ok, "no anchored read yet")

	env.svc.RecordRead(env.ctx, sessionID, "a.go", "")
	_, ok = env.svc.LatestAnchor(env.ctx, sessionID)
	require.False(t, ok, "a content-less record anchors nothing")

	env.svc.RecordRead(env.ctx, sessionID, "b.go", "one\ntwo\n")
	anchor, ok := env.svc.LatestAnchor(env.ctx, sessionID)
	require.True(t, ok)
	require.Contains(t, anchor.Path, "b.go")
	sha, lines := HashContent("one\ntwo\n")
	require.Equal(t, sha, anchor.SHA8)
	require.Equal(t, lines, anchor.Lines)
}

func TestService_ContentLessRecordPreservesAnchor(t *testing.T) {
	env := setupTest(t)

	sessionID := "preserve-session"
	env.createSession(t, sessionID)

	env.svc.RecordRead(env.ctx, sessionID, "keep.go", "v1\n")
	before, ok := env.svc.LatestAnchor(env.ctx, sessionID)
	require.True(t, ok)

	// An edit or rename mark without content must not wipe the
	// stored anchor for the same path.
	env.svc.RecordRead(env.ctx, sessionID, "keep.go", "")
	after, ok := env.svc.LatestAnchor(env.ctx, sessionID)
	require.True(t, ok)
	require.Equal(t, before, after)
}
