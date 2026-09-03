package chat

import (
	"testing"

	"github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/ui/styles"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNewToolMessageItem_LegacyAgentHistoricalMessages asserts the
// historical-boundary routing decision: persisted tool calls created
// under the old `agent` name keep rendering through the agent
// delegation item (legacy data stays displayable), while the live
// call_agent name routes there as well. No live tool is named agent.
func TestNewToolMessageItem_LegacyAgentHistoricalMessages(t *testing.T) {
	t.Parallel()
	sty := styles.CharmtonePantera()

	legacy := NewToolMessageItem(&sty, "m1", message.ToolCall{
		ID: "tc-legacy", Name: agent.LegacyAgentToolName,
		Input: `{"prompt":"old delegation"}`, Finished: true,
	}, nil, false, "")
	_, ok := legacy.(*AgentToolMessageItem)
	require.True(t, ok, "historical agent tool calls must render as agent delegation items")

	current := NewToolMessageItem(&sty, "m2", message.ToolCall{
		ID: "tc-current", Name: agent.AgentToolName,
		Input: `{"prompt":"new delegation"}`, Finished: true,
	}, nil, false, "")
	_, ok = current.(*AgentToolMessageItem)
	require.True(t, ok, "call_agent must render as the agent delegation item")

	// The legacy item renders a non-empty block (it does not fall back
	// to the generic renderer path).
	assert.NotEmpty(t, legacy.Render(80))
}
