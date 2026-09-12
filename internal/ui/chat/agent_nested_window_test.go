package chat

import (
	"fmt"
	"testing"

	"github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

// nestedBashItem builds a finished compact bash tool item with a
// unique command so tail-window assertions can tell items apart.
func nestedBashItem(t *testing.T, sty *styles.Styles, i int) ToolMessageItem {
	t.Helper()
	return NewToolMessageItem(sty, "m-nested", message.ToolCall{
		ID:       fmt.Sprintf("tc-nested-%d", i),
		Name:     "bash",
		Input:    fmt.Sprintf(`{"command":"nested-cmd-%d","description":"d"}`, i),
		Finished: true,
	}, &message.ToolResult{
		ToolCallID: fmt.Sprintf("tc-nested-%d", i),
		Name:       "bash",
		Content:    "ok",
	}, false, "")
}

// agentItemWithNested builds a call_agent delegation item carrying n
// finished nested tool calls.
func agentItemWithNested(t *testing.T, sty *styles.Styles, n int) *AgentToolMessageItem {
	t.Helper()
	item, ok := NewToolMessageItem(sty, "m-agent", message.ToolCall{
		ID:       "tc-agent",
		Name:     agent.AgentToolName,
		Input:    `{"prompt":"do things"}`,
		Finished: true,
	}, &message.ToolResult{
		ToolCallID: "tc-agent",
		Name:       agent.AgentToolName,
		Content:    "done",
	}, false, "").(*AgentToolMessageItem)
	require.True(t, ok, "call_agent must render as an agent delegation item")
	nested := make([]ToolMessageItem, 0, n)
	for i := range n {
		nested = append(nested, nestedBashItem(t, sty, i))
	}
	item.SetNestedTools(nested)
	return item
}

// TestAgentNestedToolsCollapseToTailWindow pins the fixed-block
// contract: a delegation with many nested tool calls renders only a
// tail window plus an "earlier" summary when collapsed, so a busy
// subagent cannot push the chat down without bound. Expanding shows
// the full list again.
func TestAgentNestedToolsCollapseToTailWindow(t *testing.T) {
	t.Parallel()
	sty := styles.CharmtonePantera()

	item := agentItemWithNested(t, &sty, 8)

	collapsed := item.Render(100)
	require.Contains(t, collapsed, "nested-cmd-7",
		"collapsed view must keep the latest nested tool call")
	require.NotContains(t, collapsed, "nested-cmd-0",
		"collapsed view must not render the full nested history")
	require.Contains(t, collapsed, "3 earlier tool calls",
		"collapsed view must summarize the hidden calls with a count")

	item.ToggleExpanded()
	expanded := item.Render(100)
	require.Contains(t, expanded, "nested-cmd-0",
		"expanded view must restore the full nested history")
	require.NotContains(t, expanded, "earlier tool calls",
		"expanded view must not show the summary")
}

// TestAgentNestedToolsShortListRendersFully pins the boundary: at or
// below the window size nothing is hidden and no summary appears.
func TestAgentNestedToolsShortListRendersFully(t *testing.T) {
	t.Parallel()
	sty := styles.CharmtonePantera()

	item := agentItemWithNested(t, &sty, 3)

	rendered := item.Render(100)
	require.Contains(t, rendered, "nested-cmd-0")
	require.Contains(t, rendered, "nested-cmd-2")
	require.NotContains(t, rendered, "earlier tool calls")
}

// TestAgenticFetchNestedToolsCollapseToTailWindow pins the same
// fixed-block contract for agentic-fetch delegations: collapsed
// renders the tail plus a count, Expand restores everything.
func TestAgenticFetchNestedToolsCollapseToTailWindow(t *testing.T) {
	t.Parallel()
	sty := styles.CharmtonePantera()

	item := NewAgenticFetchToolMessageItem(&sty, message.ToolCall{
		ID:       "tc-fetch",
		Name:     "agentic_fetch",
		Input:    `{"prompt":"fetch things","url":"https://example.com"}`,
		Finished: true,
	}, &message.ToolResult{
		ToolCallID: "tc-fetch",
		Name:       "agentic_fetch",
		Content:    "done",
	}, false)
	nested := make([]ToolMessageItem, 0, 7)
	for i := range 7 {
		nested = append(nested, nestedBashItem(t, &sty, i))
	}
	item.SetNestedTools(nested)

	collapsed := item.Render(100)
	require.Contains(t, collapsed, "nested-cmd-6",
		"collapsed view must keep the latest nested tool call")
	require.NotContains(t, collapsed, "nested-cmd-0",
		"collapsed view must not render the full nested history")
	require.Contains(t, collapsed, "2 earlier tool calls",
		"collapsed view must summarize the hidden calls with a count")

	item.ToggleExpanded()
	expanded := item.Render(100)
	require.Contains(t, expanded, "nested-cmd-0",
		"expanded view must restore the full nested history")
	require.NotContains(t, expanded, "earlier tool calls",
		"expanded view must not show the summary")
}
