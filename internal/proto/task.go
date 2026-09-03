package proto

import (
	"encoding/json"
	"fmt"
	"time"
)

// Task lifecycle event wire types. These mirror the exact snake_case
// shapes from docs/specs/crush-agents/10-remaining-integration.md.
// Events are facts: consumers query the task routes for full state.

// AgentTaskEvent is one task lifecycle fact published over SSE. Every
// field required by the contract is enforced on decode.
type AgentTaskEvent struct {
	Type             string `json:"type"`
	TaskID           string `json:"task_id"`
	ParentSessionID  string `json:"parent_session_id"`
	ChildSessionID   string `json:"child_session_id,omitempty"`
	ParentMessageID  string `json:"parent_message_id"`
	ToolCallID       string `json:"tool_call_id"`
	Profile          string `json:"profile"`
	ResolvedProvider string `json:"resolved_provider"`
	ResolvedModel    string `json:"resolved_model"`
	Status           string `json:"status"`
	Summary          string `json:"summary,omitempty"`
	Error            string `json:"error,omitempty"`
	RunGeneration    uint64 `json:"run_generation"`
	At               string `json:"at"`
}

// UnmarshalJSON implements the [json.Unmarshaler] interface. It
// rejects events missing any contract-required field so a malformed
// envelope can never reach the workspace layer.
func (e *AgentTaskEvent) UnmarshalJSON(data []byte) error {
	type Alias AgentTaskEvent
	aux := (*Alias)(e)
	if err := json.Unmarshal(data, aux); err != nil {
		return err
	}
	for _, f := range []struct {
		name, value string
	}{
		{"type", e.Type},
		{"task_id", e.TaskID},
		{"parent_session_id", e.ParentSessionID},
		{"tool_call_id", e.ToolCallID},
		{"profile", e.Profile},
		{"status", e.Status},
		{"at", e.At},
	} {
		if f.value == "" {
			return fmt.Errorf("task event missing required field %q", f.name)
		}
	}
	return nil
}

// TaskSnapshot is the durable task record projected for REST reads.
type TaskSnapshot struct {
	ID              string `json:"id"`
	OwnerSessionID  string `json:"owner_session_id"`
	ParentSessionID string `json:"parent_session_id,omitempty"`
	ChildSessionID  string `json:"child_session_id,omitempty"`
	ParentMessageID string `json:"parent_message_id"`
	ToolCallID      string `json:"tool_call_id"`
	Profile         string `json:"profile"`
	Provider        string `json:"resolved_provider"`
	Model           string `json:"resolved_model"`
	RunGeneration   uint64 `json:"run_generation"`
	Status          string `json:"status"`
	Result          string `json:"result,omitempty"`
	Summary         string `json:"summary,omitempty"`
	Error           string `json:"error,omitempty"`
	Truncated       bool   `json:"truncated"`
	CreatedAt       string `json:"created_at"`
	StartedAt       string `json:"started_at,omitempty"`
	CompletedAt     string `json:"completed_at,omitempty"`
}

// TaskListResponse is the body of GET /tasks.
type TaskListResponse struct {
	Tasks []TaskSnapshot `json:"tasks"`
}

// TaskOutputResponse is the body of GET /tasks/{tid}/output. It
// carries the bounded stored result only, never raw store objects.
type TaskOutputResponse struct {
	TaskID    string `json:"task_id"`
	Status    string `json:"status"`
	Result    string `json:"result,omitempty"`
	Summary   string `json:"summary,omitempty"`
	Error     string `json:"error,omitempty"`
	Truncated bool   `json:"truncated"`
}

// TaskResyncResponse is the durable reconnect recovery body of
// GET /tasks/resync, scoped to the caller's current session.
type TaskResyncResponse struct {
	Tasks     []TaskSnapshot `json:"tasks"`
	Outbox    []OutboxEntry  `json:"outbox"`
	Inbox     []InboxEntry   `json:"inbox"`
	Questions []TaskQuestion `json:"questions"`
}

// OutboxEntry is one undelivered durable lifecycle record.
type OutboxEntry struct {
	ID            string          `json:"id"`
	TaskID        string          `json:"task_id"`
	RunGeneration uint64          `json:"run_generation"`
	EventType     string          `json:"event_type"`
	Payload       json.RawMessage `json:"payload"`
	DeliveredAt   *string         `json:"delivered_at"`
	CreatedAt     string          `json:"created_at"`
}

// InboxEntry is one undelivered durable parent completion report.
type InboxEntry struct {
	ID                 string          `json:"id"`
	OwnerSessionID     string          `json:"owner_session_id"`
	TaskID             string          `json:"task_id"`
	TerminalGeneration uint64          `json:"terminal_generation"`
	Payload            json.RawMessage `json:"payload"`
	DeliveredAt        *string         `json:"delivered_at"`
	CreatedAt          string          `json:"created_at"`
}

// TaskQuestion is the public transport projection of one
// task-correlated question batch. Raw repository rows never reach the
// wire; timestamps are RFC3339Nano UTC strings and resolved_at is
// nullable.
type TaskQuestion struct {
	QuestionID     string               `json:"question_id"`
	TaskID         string               `json:"task_id"`
	OwnerSessionID string               `json:"owner_session_id"`
	ChildSessionID string               `json:"child_session_id"`
	RunGeneration  uint64               `json:"run_generation"`
	Batch          QuestionRequest      `json:"batch"`
	Answers        []TaskQuestionAnswer `json:"answers,omitempty"`
	Resolution     string               `json:"resolution"`
	CreatedAt      string               `json:"created_at"`
	ResolvedAt     *string              `json:"resolved_at"`
}

// TaskQuestionAnswer is one answer within a task-question request or
// projection. It shares the existing answer schema shape with
// snake_case tags.
type TaskQuestionAnswer struct {
	QuestionID  string            `json:"question_id"`
	SelectedIDs []string          `json:"selected_ids,omitempty"`
	FillInText  string            `json:"fill_in_text,omitempty"`
	Yes         *bool             `json:"yes,omitempty"`
	Notes       map[string]string `json:"notes,omitempty"`
}

// TaskQuestionAnswerRequest is the body of POST
// /task-questions/answer. It carries only the question lookup id and
// answer data; never owner or generation authority fields.
type TaskQuestionAnswerRequest struct {
	QuestionID string               `json:"question_id"`
	Responses  []TaskQuestionAnswer `json:"responses"`
}

// TaskQuestionCancelRequest is the body of POST
// /task-questions/cancel.
type TaskQuestionCancelRequest struct {
	QuestionID string `json:"question_id"`
}

// TaskQuestionListResponse is the body of GET
// /task-questions/pending.
type TaskQuestionListResponse struct {
	Questions []TaskQuestion `json:"questions"`
}

// TaskQuestionResolutionResponse is the success envelope of a
// task-question answer or cancel write.
type TaskQuestionResolutionResponse struct {
	Resolved bool `json:"resolved"`
}

// TaskQuestionNotification extends the question resolution envelope
// with the task correlation ids consumers reconcile by question id.
type TaskQuestionNotification struct {
	QuestionID string `json:"question_id"`
	TaskID     string `json:"task_id"`
	BatchID    string `json:"batch_id"`
	Resolution string `json:"resolution"`
}

// ChildMessageRequest is the body of POST /tasks/{tid}/messages. The
// route path supplies the task id; the body carries no owner, parent,
// child, workspace, or generation authority fields.
type ChildMessageRequest struct {
	Prompt      string       `json:"prompt"`
	Attachments []Attachment `json:"attachments,omitempty"`
}

// ChildMessageAccepted is the 202 response of POST
// /tasks/{tid}/messages.
type ChildMessageAccepted struct {
	TaskID         string `json:"task_id"`
	ChildSessionID string `json:"child_session_id"`
	Sequence       uint64 `json:"sequence"`
	AttemptTaskID  string `json:"attempt_task_id"`
	Status         string `json:"status"`
}

// AgentMessageRequest is the primary-only agent_message tool input
// shape: task lookup id, prompt text, and optional attachments.
// Identity is derived from trusted main-session context, never here.
type AgentMessageRequest struct {
	TaskID      string       `json:"task_id"`
	Prompt      string       `json:"prompt"`
	Attachments []Attachment `json:"attachments,omitempty"`
}

// AgentMessageAccepted is the agent_message tool response after the
// mailbox row commits.
type AgentMessageAccepted struct {
	TaskID         string `json:"task_id"`
	ChildSessionID string `json:"child_session_id"`
	Sequence       uint64 `json:"sequence"`
	AttemptTaskID  string `json:"attempt_task_id,omitempty"`
	Status         string `json:"status"`
}

// AgentCancelRequest is the primary-only agent_cancel tool input.
type AgentCancelRequest struct {
	TaskID string `json:"task_id"`
}

// AgentCancelAccepted is the agent_cancel / cancel-route response:
// the task id and its status after the cancellation settled.
type AgentCancelAccepted struct {
	TaskID string `json:"task_id"`
	Status string `json:"status"`
}

// WireTime formats t as an RFC3339Nano UTC timestamp string. The
// bool reports whether t was set (non-zero).
func WireTime(t time.Time) (string, bool) {
	if t.IsZero() {
		return "", false
	}
	return t.UTC().Format(time.RFC3339Nano), true
}

// ParseWireTime parses an RFC3339Nano timestamp written by
// [WireTime]. An empty string parses to the zero time.
func ParseWireTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339Nano, s)
}
