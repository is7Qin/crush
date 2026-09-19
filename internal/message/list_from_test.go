package message

import (
	"context"
	"strings"
	"testing"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

// loadCounter records which list query the service actually runs,
// proving the bounded path never falls back to the full scan.
type loadCounter struct {
	db.Querier
	fullCalls int
	fromCalls int
}

func (c *loadCounter) ListMessagesBySession(ctx context.Context, sessionID string) ([]db.Message, error) {
	c.fullCalls++
	return c.Querier.ListMessagesBySession(ctx, sessionID)
}

func (c *loadCounter) ListMessagesBySessionFrom(ctx context.Context, arg db.ListMessagesBySessionFromParams) ([]db.Message, error) {
	c.fromCalls++
	return c.Querier.ListMessagesBySessionFrom(ctx, arg)
}

// newCountedService builds a message service over a real database
// while counting full versus bounded list queries.
func newCountedService(t *testing.T) (Service, *loadCounter, string) {
	t.Helper()
	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	q := db.New(conn)
	sessions := session.NewService(q, conn)
	sess, err := sessions.Create(t.Context(), "list-from")
	require.NoError(t, err)

	counter := &loadCounter{Querier: q}
	return NewService(counter), counter, sess.ID
}

func mustCreate(t *testing.T, svc Service, sessionID string, params CreateMessageParams) Message {
	t.Helper()
	msg, err := svc.Create(t.Context(), sessionID, params)
	require.NoError(t, err)
	return msg
}

func textMsg(text string) CreateMessageParams {
	return CreateMessageParams{
		Role:  User,
		Parts: []ContentPart{TextContent{Text: text}},
	}
}

func messageIDs(msgs []Message) []string {
	ids := make([]string, len(msgs))
	for i, m := range msgs {
		ids[i] = m.ID
	}
	return ids
}

// TestListFrom_SkipsPreBoundaryRows proves the bounded path loads
// fewer rows: a large pre-summary message is never fetched, while the
// full path still returns everything.
func TestListFrom_SkipsPreBoundaryRows(t *testing.T) {
	t.Parallel()

	svc, counter, sessionID := newCountedService(t)

	pre := mustCreate(t, svc, sessionID, textMsg(strings.Repeat("x", 1<<20)))
	mustCreate(t, svc, sessionID, textMsg("pre-boundary small"))
	summary := mustCreate(t, svc, sessionID, CreateMessageParams{
		Role:             Assistant,
		Parts:            []ContentPart{TextContent{Text: "summary"}},
		IsSummaryMessage: true,
	})
	post := mustCreate(t, svc, sessionID, textMsg("post-boundary"))

	full, err := svc.List(t.Context(), sessionID)
	require.NoError(t, err)
	require.Len(t, full, 4, "the full path still returns everything")

	bounded, err := svc.ListFrom(t.Context(), sessionID, summary.ID)
	require.NoError(t, err)
	require.Equal(t, []string{summary.ID, post.ID}, messageIDs(bounded))
	require.NotContains(t, messageIDs(bounded), pre.ID)

	require.Equal(t, 1, counter.fullCalls, "only the explicit List call may scan everything")
	require.Equal(t, 1, counter.fromCalls, "the bounded path runs exactly one bounded query")
}

// TestListFrom_CorruptPreBoundaryPartsAreNeverParsed proves
// pre-boundary rows are not even parsed: corrupting one breaks the
// full List but leaves the bounded path untouched.
func TestListFrom_CorruptPreBoundaryPartsAreNeverParsed(t *testing.T) {
	t.Parallel()

	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	q := db.New(conn)
	sessions := session.NewService(q, conn)
	sess, err := sessions.Create(t.Context(), "list-from-corrupt")
	require.NoError(t, err)
	svc := NewService(q)

	pre := mustCreate(t, svc, sess.ID, textMsg("pre-boundary"))
	summary := mustCreate(t, svc, sess.ID, CreateMessageParams{
		Role:             Assistant,
		Parts:            []ContentPart{TextContent{Text: "summary"}},
		IsSummaryMessage: true,
	})

	_, err = conn.ExecContext(t.Context(), "UPDATE messages SET parts = 'not-json' WHERE id = ?", pre.ID)
	require.NoError(t, err)

	_, err = svc.List(t.Context(), sess.ID)
	require.Error(t, err, "the full path parses the corrupt pre-boundary row")

	bounded, err := svc.ListFrom(t.Context(), sess.ID, summary.ID)
	require.NoError(t, err)
	require.Len(t, bounded, 1)
	require.Equal(t, summary.ID, bounded[0].ID)
}

// TestListFrom_MissingBoundaryFallsBackToFull matches the in-memory
// slice for a stale pointer: a boundary ID that is not in the session
// returns every row.
func TestListFrom_MissingBoundaryFallsBackToFull(t *testing.T) {
	t.Parallel()

	svc, counter, sessionID := newCountedService(t)

	mustCreate(t, svc, sessionID, textMsg("one"))
	mustCreate(t, svc, sessionID, textMsg("two"))

	bounded, err := svc.ListFrom(t.Context(), sessionID, "does-not-exist")
	require.NoError(t, err)
	require.Len(t, bounded, 2)

	full, err := svc.List(t.Context(), sessionID)
	require.NoError(t, err)
	require.Equal(t, messageIDs(full), messageIDs(bounded))
	require.Equal(t, 1, counter.fromCalls)
}
