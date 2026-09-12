package agent

import (
	"testing"

	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/require"
)

// TestHealOrphanedToolCalls_PersistsTerminalResults proves the
// forever-spinner repair: an assistant ToolCall with no persisted
// ToolResult gets one terminal error result written, a second heal
// writes nothing, and the healed history no longer needs the
// in-memory synthetic injection.
func TestHealOrphanedToolCalls_PersistsTerminalResults(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	a := NewSessionAgent(SessionAgentOptions{
		Sessions: env.sessions,
		Messages: env.messages,
	}).(*sessionAgent)

	sess, err := env.sessions.Create(t.Context(), "orphan-heal")
	require.NoError(t, err)

	_, err = env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role: message.User,
		Parts: []message.ContentPart{
			message.TextContent{Text: "run the command"},
		},
	})
	require.NoError(t, err)
	_, err = env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role:  message.Assistant,
		Model: "mock-model",
		Parts: []message.ContentPart{
			message.TextContent{Text: "running it now"},
			message.ToolCall{ID: "tc-orphan-1", Name: "bash", Input: "{}", Finished: true},
		},
	})
	require.NoError(t, err)

	msgs, err := env.messages.List(t.Context(), sess.ID)
	require.NoError(t, err)

	healed, err := a.healOrphanedToolCalls(t.Context(), sess.ID, msgs)
	require.NoError(t, err)
	require.Equal(t, 1, healed, "one orphaned tool call must produce one repair row")

	msgs, err = env.messages.List(t.Context(), sess.ID)
	require.NoError(t, err)
	var found *message.ToolResult
	for _, msg := range msgs {
		if msg.Role != message.Tool {
			continue
		}
		for _, tr := range msg.ToolResults() {
			if tr.ToolCallID == "tc-orphan-1" {
				got := tr
				found = &got
			}
		}
	}
	require.NotNil(t, found, "the healed result must be persisted as a tool message")
	require.True(t, found.IsError)
	require.Equal(t, "bash", found.Name)
	require.Contains(t, found.Content, "interrupted")

	healed, err = a.healOrphanedToolCalls(t.Context(), sess.ID, msgs)
	require.NoError(t, err)
	require.Zero(t, healed, "healing is idempotent: a second pass writes nothing")
}

// TestHealOrphanedToolCalls_NoOrphansNoWrites proves the common path
// stays untouched: sessions without orphans produce no extra rows.
func TestHealOrphanedToolCalls_NoOrphansNoWrites(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	a := NewSessionAgent(SessionAgentOptions{
		Sessions: env.sessions,
		Messages: env.messages,
	}).(*sessionAgent)

	sess, err := env.sessions.Create(t.Context(), "orphan-clean")
	require.NoError(t, err)

	_, err = env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role: message.User,
		Parts: []message.ContentPart{
			message.TextContent{Text: "hello"},
		},
	})
	require.NoError(t, err)

	msgs, err := env.messages.List(t.Context(), sess.ID)
	require.NoError(t, err)

	healed, err := a.healOrphanedToolCalls(t.Context(), sess.ID, msgs)
	require.NoError(t, err)
	require.Zero(t, healed)

	after, err := env.messages.List(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Len(t, after, len(msgs), "no repair rows may be added without orphans")
}
