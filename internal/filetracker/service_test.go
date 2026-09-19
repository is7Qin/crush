package filetracker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/stretchr/testify/require"
)

type testEnv struct {
	ctx context.Context
	q   *db.Queries
	svc Service
}

func setupTest(t *testing.T) *testEnv {
	t.Helper()

	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })

	q := db.New(conn)
	return &testEnv{
		ctx: t.Context(),
		q:   q,
		svc: NewService(q),
	}
}

func (e *testEnv) createSession(t *testing.T, sessionID string) {
	t.Helper()
	_, err := e.q.CreateSession(e.ctx, db.CreateSessionParams{
		ID:    sessionID,
		Title: "Test Session",
	})
	require.NoError(t, err)
}

func TestService_RecordRead(t *testing.T) {
	env := setupTest(t)

	sessionID := "test-session-1"
	path := "/path/to/file.go"
	env.createSession(t, sessionID)

	env.svc.RecordRead(env.ctx, sessionID, path, "")

	lastRead := env.svc.LastReadTime(env.ctx, sessionID, path)
	require.False(t, lastRead.IsZero(), "expected non-zero time after recording read")
	require.WithinDuration(t, time.Now(), lastRead, 2*time.Second)
}

func TestService_LastReadTime_NotFound(t *testing.T) {
	env := setupTest(t)

	lastRead := env.svc.LastReadTime(env.ctx, "nonexistent-session", "/nonexistent/path")
	require.True(t, lastRead.IsZero(), "expected zero time for unread file")
}

func TestService_RecordRead_UpdatesTimestamp(t *testing.T) {
	env := setupTest(t)

	sessionID := "test-session-2"
	path := "/path/to/file.go"
	env.createSession(t, sessionID)

	env.svc.RecordRead(env.ctx, sessionID, path, "")
	firstRead := env.svc.LastReadTime(env.ctx, sessionID, path)
	require.False(t, firstRead.IsZero())

	synctest.Test(t, func(t *testing.T) {
		time.Sleep(100 * time.Millisecond)
		synctest.Wait()
		env.svc.RecordRead(env.ctx, sessionID, path, "")
		secondRead := env.svc.LastReadTime(env.ctx, sessionID, path)

		require.False(t, secondRead.Before(firstRead), "second read time should not be before first")
	})
}

func TestService_RecordRead_DifferentSessions(t *testing.T) {
	env := setupTest(t)

	path := "/shared/file.go"
	session1, session2 := "session-1", "session-2"
	env.createSession(t, session1)
	env.createSession(t, session2)

	env.svc.RecordRead(env.ctx, session1, path, "")

	lastRead1 := env.svc.LastReadTime(env.ctx, session1, path)
	require.False(t, lastRead1.IsZero())

	lastRead2 := env.svc.LastReadTime(env.ctx, session2, path)
	require.True(t, lastRead2.IsZero(), "session 2 should not see session 1's read")
}

// TestService_RecordRead_DrivelessPath covers a rooted path without
// a volume (e.g. `\a\b` on Windows), as echoed back from
// drive-stripped display output. The stored path must be a clean
// relative path that round-trips through GetFileRead and anchors.
func TestService_RecordRead_DrivelessPath(t *testing.T) {
	env := setupTest(t)

	sessionID := "driveless-session"
	env.createSession(t, sessionID)

	cwd, err := os.Getwd()
	require.NoError(t, err)
	abs := filepath.Join(cwd, "sub", "file.go")
	driveLess := strings.TrimPrefix(abs, filepath.VolumeName(abs))

	env.svc.RecordRead(env.ctx, sessionID, driveLess, "content\n")

	stored := relpath(driveLess)
	require.Equal(t, filepath.Join("sub", "file.go"), stored)

	got, err := env.q.GetFileRead(env.ctx, db.GetFileReadParams{
		SessionID: sessionID,
		Path:      stored,
	})
	require.NoError(t, err)
	require.True(t, got.ContentSha256_8.Valid)

	// The same file must be found again via its absolute form.
	lastRead := env.svc.LastReadTime(env.ctx, sessionID, abs)
	require.False(t, lastRead.IsZero())

	// Anchors and listings must resolve to the real location.
	anchor, ok := env.svc.LatestAnchor(env.ctx, sessionID)
	require.True(t, ok)
	require.Equal(t, abs, anchor.Path)

	listed, err := env.svc.ListReadFiles(env.ctx, sessionID)
	require.NoError(t, err)
	require.Contains(t, listed, abs)
}

func TestService_RecordRead_DifferentPaths(t *testing.T) {
	env := setupTest(t)

	sessionID := "test-session-3"
	path1, path2 := "/path/to/file1.go", "/path/to/file2.go"
	env.createSession(t, sessionID)

	env.svc.RecordRead(env.ctx, sessionID, path1, "")

	lastRead1 := env.svc.LastReadTime(env.ctx, sessionID, path1)
	require.False(t, lastRead1.IsZero())

	lastRead2 := env.svc.LastReadTime(env.ctx, sessionID, path2)
	require.True(t, lastRead2.IsZero(), "path2 should not be recorded")
}
