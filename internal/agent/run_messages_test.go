package agent

import (
	"strings"
	"testing"

	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/require"
)

// seedSummarizedSession builds a session with pre-boundary history,
// a summary message, and post-boundary history (including an orphaned
// tool call), then points the session at the summary message.
func seedSummarizedSession(t *testing.T, env fakeEnv, title string) (string, string) {
	t.Helper()
	ctx := t.Context()

	sess, err := env.sessions.Create(ctx, title)
	require.NoError(t, err)

	_, err = env.messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role: message.User,
		Parts: []message.ContentPart{
			message.TextContent{Text: strings.Repeat("pre-boundary", 1000)},
		},
	})
	require.NoError(t, err)

	summary, err := env.messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role:             message.Assistant,
		Parts:            []message.ContentPart{message.TextContent{Text: "summary"}},
		IsSummaryMessage: true,
	})
	require.NoError(t, err)

	_, err = env.messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role: message.User,
		Parts: []message.ContentPart{
			message.TextContent{Text: "post-boundary question"},
		},
	})
	require.NoError(t, err)
	_, err = env.messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role:  message.Assistant,
		Model: "mock-model",
		Parts: []message.ContentPart{
			message.ToolCall{ID: "tc-post-1", Name: "bash", Input: "{}", Finished: true},
		},
	})
	require.NoError(t, err)

	sess.SummaryMessageID = summary.ID
	_, err = env.sessions.Save(ctx, sess)
	require.NoError(t, err)
	sess, err = env.sessions.Get(ctx, sess.ID)
	require.NoError(t, err)
	return sess.ID, summary.ID
}

// TestGetRunMessages_MatchesSummarySlice proves the bounded per-turn
// loader returns exactly what the full load plus in-memory slice
// returns today: the summary row itself, rewritten to a user message,
// followed by every later row — and that both build the same prompt.
func TestGetRunMessages_MatchesSummarySlice(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	a := NewSessionAgent(SessionAgentOptions{
		Sessions: env.sessions,
		Messages: env.messages,
	}).(*sessionAgent)

	sessionID, summaryID := seedSummarizedSession(t, env, "run-messages")
	sess, err := env.sessions.Get(t.Context(), sessionID)
	require.NoError(t, err)

	full, err := a.getSessionMessages(t.Context(), sess)
	require.NoError(t, err)
	run, err := a.getRunMessages(t.Context(), sess)
	require.NoError(t, err)

	require.Equal(t, full, run, "bounded loader must match the slice-in-memory behavior")
	require.NotEmpty(t, run)
	require.Equal(t, summaryID, run[0].ID, "the summary row itself is kept")
	require.Equal(t, message.User, run[0].Role, "the summary row is presented as a user message")

	fullPrompt, _ := a.preparePrompt(full, true)
	runPrompt, _ := a.preparePrompt(run, true)
	require.Equal(t, fullPrompt, runPrompt, "the resulting prompt must be unchanged")
}

// TestGetRunMessages_NoBoundaryLoadsEverything proves sessions
// without a summary boundary still load the full history.
func TestGetRunMessages_NoBoundaryLoadsEverything(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	a := NewSessionAgent(SessionAgentOptions{
		Sessions: env.sessions,
		Messages: env.messages,
	}).(*sessionAgent)

	sess, err := env.sessions.Create(t.Context(), "run-messages-no-boundary")
	require.NoError(t, err)
	_, err = env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "hello"}},
	})
	require.NoError(t, err)
	sess, err = env.sessions.Get(t.Context(), sess.ID)
	require.NoError(t, err)

	full, err := a.getSessionMessages(t.Context(), sess)
	require.NoError(t, err)
	run, err := a.getRunMessages(t.Context(), sess)
	require.NoError(t, err)
	require.Equal(t, full, run)
}

// TestGetRunMessages_StaleBoundaryLoadsEverything matches the
// in-memory slice for a boundary ID that is not in the session: the
// whole history is returned with no role rewrite.
func TestGetRunMessages_StaleBoundaryLoadsEverything(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	a := NewSessionAgent(SessionAgentOptions{
		Sessions: env.sessions,
		Messages: env.messages,
	}).(*sessionAgent)

	sess, err := env.sessions.Create(t.Context(), "run-messages-stale")
	require.NoError(t, err)
	_, err = env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "hello"}},
	})
	require.NoError(t, err)
	sess.SummaryMessageID = "does-not-exist"
	_, err = env.sessions.Save(t.Context(), sess)
	require.NoError(t, err)
	sess, err = env.sessions.Get(t.Context(), sess.ID)
	require.NoError(t, err)

	full, err := a.getSessionMessages(t.Context(), sess)
	require.NoError(t, err)
	run, err := a.getRunMessages(t.Context(), sess)
	require.NoError(t, err)
	require.Equal(t, full, run)
	require.Equal(t, message.User, run[0].Role)
}
