package chat

import (
	"encoding/json"
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/tree"
	"github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/ui/styles"
)

// -----------------------------------------------------------------------------
// Agent Tool
// -----------------------------------------------------------------------------

// NestedToolContainer is an interface for tool items that can contain nested tool calls.
type NestedToolContainer interface {
	NestedTools() []ToolMessageItem
	SetNestedTools(tools []ToolMessageItem)
	AddNestedTool(tool ToolMessageItem)
}

// AgentToolMessageItem is a message item that represents an agent tool call.
type AgentToolMessageItem struct {
	*baseToolMessageItem

	nestedTools []ToolMessageItem
	// task holds the latest durable call_agent task fact mirrored by
	// tool call id. It starts zero until a task event for this
	// delegation's tool call arrives.
	task TaskState
}

// TaskState is the latest durable call_agent task fact mirrored onto
// an agent tool item. It carries both correlation ids plus the run
// generation so a superseded attempt's late event cannot replace a
// newer attempt's state.
type TaskState struct {
	TaskID string
	// Event names the lifecycle fact (created, started, ...) and
	// Status the task's current status.
	Event    string
	Status   string
	Revision uint64
}

// TaskStateTracker is an opt-in interface for tool items that mirror
// durable task lifecycle facts correlated by task id + tool call id.
type TaskStateTracker interface {
	SetTaskState(TaskState)
	TaskState() TaskState
}

var _ TaskStateTracker = (*AgentToolMessageItem)(nil)

// SetTaskState records a task lifecycle fact for this delegation. A
// state belonging to a different task is accepted only at a newer or
// equal run generation, keeping continuation attempts monotonic.
func (a *AgentToolMessageItem) SetTaskState(s TaskState) {
	if s.TaskID == "" {
		return
	}
	if a.task.TaskID == s.TaskID && s.Revision < a.task.Revision {
		return
	}
	a.task = s
	if status, ok := terminalToolStatus(s.Status); ok {
		a.SetStatus(status)
	}
	a.clearCache()
	a.Bump()
}

func terminalToolStatus(status string) (ToolStatus, bool) {
	switch status {
	case "completed":
		return ToolStatusSuccess, true
	case "failed":
		return ToolStatusError, true
	case "cancelled", "interrupted":
		return ToolStatusCanceled, true
	default:
		return ToolStatusRunning, false
	}
}

// TaskState returns the most recent task fact mirrored on this item.
func (a *AgentToolMessageItem) TaskState() TaskState {
	return a.task
}

var (
	_ ToolMessageItem     = (*AgentToolMessageItem)(nil)
	_ NestedToolContainer = (*AgentToolMessageItem)(nil)
)

// NewAgentToolMessageItem creates a new [AgentToolMessageItem].
func NewAgentToolMessageItem(
	sty *styles.Styles,
	toolCall message.ToolCall,
	result *message.ToolResult,
	canceled bool,
) *AgentToolMessageItem {
	t := &AgentToolMessageItem{}
	t.baseToolMessageItem = newBaseToolMessageItem(sty, toolCall, result, &AgentToolRenderContext{agent: t}, canceled)
	// For the agent tool we keep spinning until the tool call is finished.
	t.spinningFunc = func(state SpinningState) bool {
		return !state.HasResult() && !state.IsCanceled()
	}
	return t
}

// Advance implements [Animatable].
//
// Advances the parent's own spinner and every spinning nested tool in
// one frame, bumping the parent's F6 list-cache version. Nested tools
// are not list entries of their own — their IDs map to this parent's
// index in idInxMap and their renders are embedded inline in this
// parent's output — so the list only checks the parent's version.
// Without the bump, the list cache would serve the previously rendered
// frame indefinitely and the spinner would appear frozen.
func (a *AgentToolMessageItem) Advance() bool {
	if a.result != nil || a.Status() == ToolStatusCanceled {
		return false
	}
	changed := a.anim.Advance()
	changed = advanceNested(a.nestedTools) || changed
	if changed {
		a.Bump()
	}
	return changed
}

// advanceNested advances every spinning animatable tool in tools and
// reports whether any of them changed.
func advanceNested(tools []ToolMessageItem) bool {
	changed := false
	for _, nestedTool := range tools {
		if s, ok := nestedTool.(Animatable); ok && s.Spinning() && s.Advance() {
			changed = true
		}
	}
	return changed
}

// NestedTools returns the nested tools.
func (a *AgentToolMessageItem) NestedTools() []ToolMessageItem {
	return a.nestedTools
}

// SetNestedTools sets the nested tools.
//
// SetNestedTools always bumps the version. The previous design
// deduped when the slice's length and element pointers were
// unchanged, but the live update path in internal/ui/model/ui.go
// mutates existing children in place (SetToolCall / SetResult on the
// same pointers) and then calls SetNestedTools with the same slice.
// Pointer-equality dedupe in that case skips the parent Bump even
// though the parent's rendered output (which embeds the children
// inline) has changed, leaving a stale parent entry in the list
// cache. Always bumping is cheap (one uint64 increment) and called
// at most once per agent event; in the rare case the slice is
// truly unchanged the worst case is one extra parent re-render
// while every child cache hit stays warm.
func (a *AgentToolMessageItem) SetNestedTools(tools []ToolMessageItem) {
	a.nestedTools = tools
	a.clearCache()
	a.Bump()
}

// AddNestedTool adds a nested tool.
func (a *AgentToolMessageItem) AddNestedTool(tool ToolMessageItem) {
	// Mark nested tools as simple (compact) rendering.
	if s, ok := tool.(Compactable); ok {
		s.SetCompact(true)
	}
	a.nestedTools = append(a.nestedTools, tool)
	a.clearCache()
	a.Bump()
}

// maxCollapsedNestedTools caps how many nested tool calls a
// delegation renders while collapsed, mirroring the thinking
// block's fixed-height contract. Older calls collapse into an
// "earlier tool calls" summary line; Expand shows everything.
const maxCollapsedNestedTools = 5

// tailNestedTools returns the visible tail of a nested tool list and
// how many leading items were hidden. Expanded views show everything.
func tailNestedTools(tools []ToolMessageItem, expanded bool) ([]ToolMessageItem, int) {
	if expanded || len(tools) <= maxCollapsedNestedTools {
		return tools, 0
	}
	skipped := len(tools) - maxCollapsedNestedTools
	return tools[len(tools)-maxCollapsedNestedTools:], skipped
}

// earlierToolCallsSummary renders the collapsed summary line for
// hidden nested tool calls.
func earlierToolCallsSummary(sty *styles.Styles, skipped int) string {
	summary := fmt.Sprintf("… %d earlier tool calls", skipped)
	if skipped == 1 {
		summary = "… 1 earlier tool call"
	}
	return sty.Tool.AgentPrompt.Render(summary)
}

// AgentToolRenderContext renders agent tool messages.
type AgentToolRenderContext struct {
	agent *AgentToolMessageItem
}

// RenderTool implements the [ToolRenderer] interface.
func (r *AgentToolRenderContext) RenderTool(sty *styles.Styles, width int, opts *ToolRenderOpts) string {
	cappedWidth := cappedMessageWidth(width)
	if !opts.ToolCall.Finished && !opts.IsCanceled() && len(r.agent.nestedTools) == 0 {
		return pendingTool(sty, "Agent", opts.Anim, opts.Compact)
	}

	var params agent.AgentParams
	_ = json.Unmarshal([]byte(opts.ToolCall.Input), &params)

	prompt := params.Prompt
	if !opts.ExpandedContent {
		prompt = strings.ReplaceAll(prompt, "\n", " ")
	}

	header := toolHeader(sty, opts.Status, "Agent", cappedWidth, opts)
	if opts.Compact {
		return header
	}

	// Build the task tag and prompt.
	taskTag := sty.Tool.AgentTaskTag.Render("Task")
	taskTagWidth := lipgloss.Width(taskTag)

	// Calculate remaining width for prompt.
	remainingWidth := min(cappedWidth-taskTagWidth-3, maxTextWidth-taskTagWidth-3) // -3 for spacing

	promptText := sty.Tool.AgentPrompt.Width(remainingWidth).Render(prompt)

	header = lipgloss.JoinVertical(
		lipgloss.Left,
		header,
		"",
		lipgloss.JoinHorizontal(
			lipgloss.Left,
			taskTag,
			" ",
			promptText,
		),
	)

	// Mirror the durable task lifecycle once task events have
	// correlated to this delegation. Items without task state render
	// exactly as before.
	if r.agent.task.TaskID != "" && r.agent.task.Status != "" {
		statusLine := sty.Tool.AgentTaskTag.Render("Task " + r.agent.task.Status)
		idLine := sty.Tool.AgentPrompt.Width(remainingWidth).Render(r.agent.task.TaskID)
		header = lipgloss.JoinVertical(
			lipgloss.Left,
			header,
			"",
			lipgloss.JoinHorizontal(lipgloss.Left, statusLine, " ", idLine),
		)
	}

	// Collapse the nested activity into a fixed tail window so a
	// busy subagent cannot push the chat down without bound; the
	// full list is one Expand toggle away.
	nested, skipped := tailNestedTools(r.agent.nestedTools, opts.ExpandedContent)

	// Build tree with nested tool calls.
	childTools := tree.Root(header)

	for _, nestedTool := range nested {
		childView := nestedTool.Render(remainingWidth)
		childTools.Child(childView)
	}

	// Build parts.
	var parts []string
	parts = append(parts, childTools.Enumerator(roundedEnumerator(2, taskTagWidth-5)).String())
	if skipped > 0 {
		parts = append(parts, earlierToolCallsSummary(sty, skipped))
	}

	// Show animation if still running.
	if !opts.HasResult() && !opts.IsCanceled() {
		parts = append(parts, "", opts.Anim.Render())
	}

	result := lipgloss.JoinVertical(lipgloss.Left, parts...)

	// Add body content when completed.
	if opts.HasResult() && opts.Result.Content != "" {
		body := toolOutputMarkdownContent(sty, opts.Result.Content, cappedWidth-toolBodyLeftPaddingTotal, opts.ExpandedContent)
		return joinToolParts(result, body)
	}

	return result
}

// -----------------------------------------------------------------------------
// Agentic Fetch Tool
// -----------------------------------------------------------------------------

// AgenticFetchToolMessageItem is a message item that represents an agentic fetch tool call.
type AgenticFetchToolMessageItem struct {
	*baseToolMessageItem

	nestedTools []ToolMessageItem
}

var (
	_ ToolMessageItem     = (*AgenticFetchToolMessageItem)(nil)
	_ NestedToolContainer = (*AgenticFetchToolMessageItem)(nil)
)

// NewAgenticFetchToolMessageItem creates a new [AgenticFetchToolMessageItem].
func NewAgenticFetchToolMessageItem(
	sty *styles.Styles,
	toolCall message.ToolCall,
	result *message.ToolResult,
	canceled bool,
) *AgenticFetchToolMessageItem {
	t := &AgenticFetchToolMessageItem{}
	t.baseToolMessageItem = newBaseToolMessageItem(sty, toolCall, result, &AgenticFetchToolRenderContext{fetch: t}, canceled)
	// For the agentic fetch tool we keep spinning until the tool call is finished.
	t.spinningFunc = func(state SpinningState) bool {
		return !state.HasResult() && !state.IsCanceled()
	}
	return t
}

// Advance implements [Animatable]. See [AgentToolMessageItem.Advance]
// for the parent-bump rationale; without an override the embedded base
// Advance would never advance the nested children.
func (a *AgenticFetchToolMessageItem) Advance() bool {
	if a.result != nil || a.Status() == ToolStatusCanceled {
		return false
	}
	changed := a.anim.Advance()
	changed = advanceNested(a.nestedTools) || changed
	if changed {
		a.Bump()
	}
	return changed
}

// NestedTools returns the nested tools.
func (a *AgenticFetchToolMessageItem) NestedTools() []ToolMessageItem {
	return a.nestedTools
}

// SetNestedTools sets the nested tools. Always bumps the version;
// see [AgentToolMessageItem.SetNestedTools] for the rationale.
func (a *AgenticFetchToolMessageItem) SetNestedTools(tools []ToolMessageItem) {
	a.nestedTools = tools
	a.clearCache()
	a.Bump()
}

// AddNestedTool adds a nested tool.
func (a *AgenticFetchToolMessageItem) AddNestedTool(tool ToolMessageItem) {
	// Mark nested tools as simple (compact) rendering.
	if s, ok := tool.(Compactable); ok {
		s.SetCompact(true)
	}
	a.nestedTools = append(a.nestedTools, tool)
	a.clearCache()
	a.Bump()
}

// AgenticFetchToolRenderContext renders agentic fetch tool messages.
type AgenticFetchToolRenderContext struct {
	fetch *AgenticFetchToolMessageItem
}

// agenticFetchParams matches tools.AgenticFetchParams.
type agenticFetchParams struct {
	URL    string `json:"url,omitempty"`
	Prompt string `json:"prompt"`
}

// RenderTool implements the [ToolRenderer] interface.
func (r *AgenticFetchToolRenderContext) RenderTool(sty *styles.Styles, width int, opts *ToolRenderOpts) string {
	cappedWidth := cappedMessageWidth(width)
	if !opts.ToolCall.Finished && !opts.IsCanceled() && len(r.fetch.nestedTools) == 0 {
		return pendingTool(sty, "Agentic Fetch", opts.Anim, opts.Compact)
	}

	var params agenticFetchParams
	_ = json.Unmarshal([]byte(opts.ToolCall.Input), &params)

	prompt := params.Prompt
	if !opts.ExpandedContent {
		prompt = strings.ReplaceAll(prompt, "\n", " ")
	}

	// Build header with optional URL param.
	var toolParams []string
	if params.URL != "" {
		toolParams = append(toolParams, params.URL)
	}

	header := toolHeader(sty, opts.Status, "Agentic Fetch", cappedWidth, opts, toolParams...)
	if opts.Compact {
		return header
	}

	// Build the prompt tag.
	promptTag := sty.Tool.AgenticFetchPromptTag.Render("Prompt")
	promptTagWidth := lipgloss.Width(promptTag)

	// Calculate remaining width for prompt text.
	remainingWidth := min(cappedWidth-promptTagWidth-3, maxTextWidth-promptTagWidth-3) // -3 for spacing

	promptText := sty.Tool.AgentPrompt.Width(remainingWidth).Render(prompt)

	header = lipgloss.JoinVertical(
		lipgloss.Left,
		header,
		"",
		lipgloss.JoinHorizontal(
			lipgloss.Left,
			promptTag,
			" ",
			promptText,
		),
	)

	// Collapse the nested activity exactly like agent delegations
	// above: fixed tail window, full list one Expand toggle away.
	nested, skipped := tailNestedTools(r.fetch.nestedTools, opts.ExpandedContent)

	// Build tree with nested tool calls.
	childTools := tree.Root(header)

	for _, nestedTool := range nested {
		childView := nestedTool.Render(remainingWidth)
		childTools.Child(childView)
	}

	// Build parts.
	var parts []string
	parts = append(parts, childTools.Enumerator(roundedEnumerator(2, promptTagWidth-5)).String())
	if skipped > 0 {
		parts = append(parts, earlierToolCallsSummary(sty, skipped))
	}

	// Show animation if still running.
	if !opts.HasResult() && !opts.IsCanceled() {
		parts = append(parts, "", opts.Anim.Render())
	}

	result := lipgloss.JoinVertical(lipgloss.Left, parts...)

	// Add body content when completed.
	if opts.HasResult() && opts.Result.Content != "" {
		body := toolOutputMarkdownContent(sty, opts.Result.Content, cappedWidth-toolBodyLeftPaddingTotal, opts.ExpandedContent)
		return joinToolParts(result, body)
	}

	return result
}
