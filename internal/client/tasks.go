package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/charmbracelet/crush/internal/proto"
)

// Task and task-question control-plane calls. Every route requires
// the process-scoped client id as a query parameter; the server
// validates attachment and derives the owner session from the
// client's current-session binding, so requests never carry authority
// fields.

// TaskList lists the caller's tasks. parentSessionID is an optional
// filter the server only accepts when it equals the caller's current
// session; pass "" for every task the caller owns.
func (c *Client) TaskList(ctx context.Context, id, parentSessionID string) (proto.TaskListResponse, error) {
	q := c.taskQuery()
	if parentSessionID != "" {
		q.Set("parent_session_id", parentSessionID)
	}
	return taskGet[proto.TaskListResponse](ctx, c, id, "/tasks", q, "list tasks")
}

// TaskGet returns one caller-owned task snapshot.
func (c *Client) TaskGet(ctx context.Context, id, taskID string) (proto.TaskSnapshot, error) {
	return taskGet[proto.TaskSnapshot](ctx, c, id, "/tasks/"+url.PathEscape(taskID),
		c.taskQuery(), "get task")
}

// TaskOutput returns the bounded stored result of a caller-owned task.
func (c *Client) TaskOutput(ctx context.Context, id, taskID string) (proto.TaskOutputResponse, error) {
	return taskGet[proto.TaskOutputResponse](ctx, c, id, "/tasks/"+url.PathEscape(taskID)+"/output",
		c.taskQuery(), "get task output")
}

// TaskCancel cancels the caller's non-terminal task attempt.
func (c *Client) TaskCancel(ctx context.Context, id, taskID string) (proto.AgentCancelAccepted, error) {
	return taskPost[proto.AgentCancelAccepted](ctx, c, id, "/tasks/"+url.PathEscape(taskID)+"/cancel",
		c.taskQuery(), nil, "cancel task")
}

// SendChildMessage appends a direct user message to a task's child
// conversation and reports the accepted mailbox row. A successfully
// accepted message is HTTP 202.
func (c *Client) SendChildMessage(ctx context.Context, id, taskID, prompt string, attachments []proto.Attachment) (proto.ChildMessageAccepted, error) {
	return taskPost[proto.ChildMessageAccepted](ctx, c, id, "/tasks/"+url.PathEscape(taskID)+"/messages",
		c.taskQuery(), proto.ChildMessageRequest{Prompt: prompt, Attachments: attachments},
		"send child message", http.StatusAccepted)
}

// TasksResync is the durable reconnect recovery read, scoped
// server-side to the caller's current session.
func (c *Client) TasksResync(ctx context.Context, id string) (proto.TaskResyncResponse, error) {
	return taskGet[proto.TaskResyncResponse](ctx, c, id, "/tasks/resync",
		c.taskQuery(), "resync tasks")
}

// TaskQuestionsPending lists the caller's unresolved task questions.
func (c *Client) TaskQuestionsPending(ctx context.Context, id string) (proto.TaskQuestionListResponse, error) {
	return taskGet[proto.TaskQuestionListResponse](ctx, c, id, "/task-questions/pending",
		c.taskQuery(), "list pending task questions")
}

// AnswerTaskQuestion resolves a task question with owner
// authorization derived from the client's current session.
func (c *Client) AnswerTaskQuestion(ctx context.Context, id string, req proto.TaskQuestionAnswerRequest) (bool, error) {
	resp, err := taskPost[proto.TaskQuestionResolutionResponse](ctx, c, id, "/task-questions/answer",
		c.taskQuery(), req, "answer task question")
	return resp.Resolved, err
}

// CancelTaskQuestion resolves a task question as cancelled with owner
// authorization derived from the client's current session.
func (c *Client) CancelTaskQuestion(ctx context.Context, id, questionID string) (bool, error) {
	resp, err := taskPost[proto.TaskQuestionResolutionResponse](ctx, c, id, "/task-questions/cancel",
		c.taskQuery(), proto.TaskQuestionCancelRequest{QuestionID: questionID}, "cancel task question")
	return resp.Resolved, err
}

// taskQuery is the client_id query shared by every task route.
func (c *Client) taskQuery() url.Values {
	return url.Values{"client_id": []string{c.clientID}}
}

// taskGet performs a task GET and decodes the JSON response into T.
func taskGet[T any](ctx context.Context, c *Client, wsID, path string, q url.Values, what string, ok ...int) (T, error) {
	var out T
	rsp, err := c.get(ctx, "/workspaces/"+wsID+path, q, nil)
	if err != nil {
		return out, fmt.Errorf("failed to %s: %w", what, err)
	}
	defer rsp.Body.Close()
	if err := checkStatus(rsp, ok...); err != nil {
		return out, fmt.Errorf("failed to %s: %w", what, err)
	}
	if err := json.NewDecoder(rsp.Body).Decode(&out); err != nil {
		return out, fmt.Errorf("failed to decode %s response: %w", what, err)
	}
	return out, nil
}

// taskPost performs a task POST with an optional JSON body and
// decodes the JSON response into T.
func taskPost[T any](ctx context.Context, c *Client, wsID, path string, q url.Values, body any, what string, ok ...int) (T, error) {
	var out T
	var reqBody io.Reader
	var headers http.Header
	if body != nil {
		reqBody = jsonBody(body)
		headers = http.Header{"Content-Type": []string{"application/json"}}
	}
	rsp, err := c.post(ctx, "/workspaces/"+wsID+path, q, reqBody, headers)
	if err != nil {
		return out, fmt.Errorf("failed to %s: %w", what, err)
	}
	defer rsp.Body.Close()
	if err := checkStatus(rsp, ok...); err != nil {
		return out, fmt.Errorf("failed to %s: %w", what, err)
	}
	if err := json.NewDecoder(rsp.Body).Decode(&out); err != nil {
		return out, fmt.Errorf("failed to decode %s response: %w", what, err)
	}
	return out, nil
}
