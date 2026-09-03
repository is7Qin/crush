package agent

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"charm.land/fantasy"

	"github.com/charmbracelet/crush/internal/agent/task"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/proto"
)

// Task-control tool names. They are the primary session's control plane
// over its own call_agent tasks; child sessions never see
// them (see the buildTools gate).
const (
	AgentStatusToolName  = "agent_status"
	AgentOutputToolName  = "agent_output"
	AgentListToolName    = "agent_list"
	AgentCancelToolName  = "agent_cancel"
	AgentMessageToolName = "agent_message"
)

//go:embed templates/agent_status.md
var agentStatusDescription string

//go:embed templates/agent_output.md
var agentOutputDescription string

//go:embed templates/agent_list.md
var agentListDescription string

//go:embed templates/agent_cancel.md
var agentCancelDescription string

//go:embed templates/agent_message.md
var agentMessageDescription string

// agentTaskIDParams is the request schema shared by agent_status,
// agent_output, and agent_cancel.
type agentTaskIDParams struct {
	TaskID string `json:"task_id" description:"The task id returned by a call_agent delegation"`
}

// agentListParams is the empty request schema for agent_list.
type agentListParams struct{}

// AgentOutputResponseMetadata is the machine-readable truncation report
// on an agent_output response.
type AgentOutputResponseMetadata struct {
	TaskID    string `json:"task_id"`
	Truncated bool   `json:"truncated"`
}

// taskControlCaller returns the trusted caller session that authorizes
// every task-control operation. The session id comes from the context
// the coordinator stamps, never from tool arguments.
func taskControlCaller(ctx context.Context) (string, error) {
	sessionID := tools.GetSessionFromContext(ctx)
	if sessionID == "" {
		return "", errors.New("session id missing from context")
	}
	return sessionID, nil
}

// taskStatusText renders one task snapshot for the model.
func taskStatusText(t *task.Task) string {
	lines := []string{
		"Task ID: " + t.ID,
		"Status: " + string(t.Status),
		"Profile: " + t.Profile,
		"Model: " + t.Provider + "/" + t.Model,
		"Created: " + t.CreatedAt.Format(time.RFC3339),
	}
	if t.ChildSessionID != "" {
		lines = append(lines, "Child session ID: "+t.ChildSessionID)
	}
	if t.Summary != "" {
		lines = append(lines, "Summary: "+t.Summary)
	}
	if t.Err != "" {
		lines = append(lines, "Error: "+t.Err)
	}
	return strings.Join(lines, "\n")
}

func newAgentStatusTool(ctrl TaskController) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		AgentStatusToolName,
		agentStatusDescription,
		func(ctx context.Context, params agentTaskIDParams, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			caller, err := taskControlCaller(ctx)
			if err != nil {
				return fantasy.ToolResponse{}, err
			}
			if params.TaskID == "" {
				return fantasy.NewTextErrorResponse("missing task_id"), nil
			}
			t, err := ctrl.Status(ctx, caller, params.TaskID)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			return fantasy.NewTextResponse(taskStatusText(t)), nil
		},
	)
}

func newAgentOutputTool(ctrl TaskController) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		AgentOutputToolName,
		agentOutputDescription,
		func(ctx context.Context, params agentTaskIDParams, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			caller, err := taskControlCaller(ctx)
			if err != nil {
				return fantasy.ToolResponse{}, err
			}
			if params.TaskID == "" {
				return fantasy.NewTextErrorResponse("missing task_id"), nil
			}
			res, truncated, err := ctrl.Output(ctx, caller, params.TaskID)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			text := res.Text
			if truncated {
				text += fmt.Sprintf("\n\n[output truncated to %d bytes]", task.MaxResultBytes)
			}
			if text == "" {
				text = "(no stored output; the task may still be running)"
			}
			resp := fantasy.NewTextResponse(text)
			metadata := AgentOutputResponseMetadata{TaskID: params.TaskID, Truncated: truncated}
			return fantasy.WithResponseMetadata(resp, metadata), nil
		},
	)
}

func newAgentListTool(ctrl TaskController) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		AgentListToolName,
		agentListDescription,
		func(ctx context.Context, _ agentListParams, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			caller, err := taskControlCaller(ctx)
			if err != nil {
				return fantasy.ToolResponse{}, err
			}
			tasks, err := ctrl.List(ctx, caller, "")
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			if len(tasks) == 0 {
				return fantasy.NewTextResponse("no agent tasks"), nil
			}
			lines := make([]string, 0, len(tasks))
			for _, t := range tasks {
				lines = append(lines, fmt.Sprintf(
					"- %s status=%s profile=%s model=%s/%s",
					t.ID, t.Status, t.Profile, t.Provider, t.Model,
				))
			}
			return fantasy.NewTextResponse(strings.Join(lines, "\n")), nil
		},
	)
}

func newAgentCancelTool(ctrl TaskController) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		AgentCancelToolName,
		agentCancelDescription,
		func(ctx context.Context, params agentTaskIDParams, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			caller, err := taskControlCaller(ctx)
			if err != nil {
				return fantasy.ToolResponse{}, err
			}
			if params.TaskID == "" {
				return fantasy.NewTextErrorResponse("missing task_id"), nil
			}
			if err := ctrl.Cancel(ctx, caller, params.TaskID); err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			t, err := ctrl.Status(ctx, caller, params.TaskID)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			return fantasy.NewTextResponse("Cancellation requested.\n" + taskStatusText(t)), nil
		},
	)
}

// newAgentMessageTool builds the primary-only agent_message tool. It
// commits one FIFO mailbox message for the caller's own call_agent
// task through the same AppendMessage operation that backs the
// user-facing route, then returns the acceptance immediately: the
// child runs the message later, on the workspace context. Identity is
// always the trusted caller session; the input carries only the task
// lookup id, prompt text, and optional attachments.
func newAgentMessageTool(ctrl TaskController) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		AgentMessageToolName,
		agentMessageDescription,
		func(ctx context.Context, params proto.AgentMessageRequest, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			caller, err := taskControlCaller(ctx)
			if err != nil {
				return fantasy.ToolResponse{}, err
			}
			if params.TaskID == "" {
				return fantasy.NewTextErrorResponse("missing task_id"), nil
			}
			if params.Prompt == "" {
				return fantasy.NewTextErrorResponse("missing prompt"), nil
			}
			attachments := "[]"
			if len(params.Attachments) > 0 {
				raw, err := json.Marshal(params.Attachments)
				if err != nil {
					return fantasy.NewTextErrorResponse("failed to encode attachments"), nil
				}
				attachments = string(raw)
			}
			accepted, err := ctrl.AppendMessage(ctx, task.MessageRequest{
				OwnerSessionID: caller,
				TaskID:         params.TaskID,
				Origin:         task.OriginParent,
				Prompt:         params.Prompt,
				Attachments:    attachments,
			})
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			acc := proto.AgentMessageAccepted{
				TaskID:         accepted.TaskID,
				ChildSessionID: accepted.ChildSessionID,
				Sequence:       accepted.Sequence,
				AttemptTaskID:  accepted.AttemptTaskID,
				Status:         string(accepted.Status),
			}
			lines := []string{
				"Message accepted.",
				"Task ID: " + acc.TaskID,
				"Child session ID: " + acc.ChildSessionID,
				fmt.Sprintf("Sequence: %d", acc.Sequence),
				"Status: " + acc.Status,
			}
			if acc.AttemptTaskID != "" {
				lines = append(lines, "Successor attempt task ID: "+acc.AttemptTaskID)
			}
			resp := fantasy.NewTextResponse(strings.Join(lines, "\n"))
			return fantasy.WithResponseMetadata(resp, acc), nil
		},
	)
}
