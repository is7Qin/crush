package tools

import (
	"bytes"
	"context"
	"html/template"
	"os/exec"
	"testing"

	"charm.land/fantasy"
)

type (
	sessionIDContextKey string
	messageIDContextKey string
	supportsImagesKey   string
	modelNameKey        string
	agentDepthKey       string
)

const (
	// SessionIDContextKey is the key for the session ID in the context.
	SessionIDContextKey sessionIDContextKey = "session_id"
	// MessageIDContextKey is the key for the message ID in the context.
	MessageIDContextKey messageIDContextKey = "message_id"
	// SupportsImagesContextKey is the key for the model's image support capability.
	SupportsImagesContextKey supportsImagesKey = "supports_images"
	// ModelNameContextKey is the key for the model name in the context.
	ModelNameContextKey modelNameKey = "model_name"
	// AgentDepthContextKey is the key for the trusted delegation depth of
	// the running agent: 0 for a top-level session, N+1 inside a child
	// spawned N levels down. It is set by the coordinator, never by tool
	// arguments, so a forged tool call cannot reset it.
	AgentDepthContextKey agentDepthKey = "agent_depth"
)

// getContextValue is a generic helper that retrieves a typed value from context.
// If the value is not found or has the wrong type, it returns the default value.
func getContextValue[T any](ctx context.Context, key any, defaultValue T) T {
	value := ctx.Value(key)
	if value == nil {
		return defaultValue
	}
	if typedValue, ok := value.(T); ok {
		return typedValue
	}
	return defaultValue
}

// GetSessionFromContext retrieves the session ID from the context.
func GetSessionFromContext(ctx context.Context) string {
	return getContextValue(ctx, SessionIDContextKey, "")
}

// GetMessageFromContext retrieves the message ID from the context.
func GetMessageFromContext(ctx context.Context) string {
	return getContextValue(ctx, MessageIDContextKey, "")
}

// GetSupportsImagesFromContext retrieves whether the model supports images from the context.
func GetSupportsImagesFromContext(ctx context.Context) bool {
	return getContextValue(ctx, SupportsImagesContextKey, false)
}

// GetModelNameFromContext retrieves the model name from the context.
func GetModelNameFromContext(ctx context.Context) string {
	return getContextValue(ctx, ModelNameContextKey, "")
}

// GetAgentDepthFromContext retrieves the trusted delegation depth of the
// running agent. A missing key means the top-level session, so the default
// is 0.
func GetAgentDepthFromContext(ctx context.Context) int {
	return getContextValue(ctx, AgentDepthContextKey, 0)
}

// taskQuestionContextKey is the context key for the trusted task
// question correlation of a child run.
type taskQuestionContextKey struct{}

// TaskQuestionContext carries the trusted correlation a child run's
// question transport needs: which task is asking, whose owner session
// answers, and which child session and run generation the question
// belongs to. The coordinator sets it when a child executes through
// the task manager, never from tool arguments, so a forged call
// cannot redirect a question to another task.
type TaskQuestionContext struct {
	TaskID         string
	OwnerSessionID string
	ChildSessionID string
	RunGeneration  uint64
}

// WithTaskQuestionContext returns a context carrying the trusted task
// question correlation tc.
func WithTaskQuestionContext(ctx context.Context, tc TaskQuestionContext) context.Context {
	return context.WithValue(ctx, taskQuestionContextKey{}, tc)
}

// GetTaskQuestionContextFromContext retrieves the task question
// correlation. A missing value means the run is not a task child, so
// the task-aware question transport is unavailable.
func GetTaskQuestionContextFromContext(ctx context.Context) (TaskQuestionContext, bool) {
	tc, ok := ctx.Value(taskQuestionContextKey{}).(TaskQuestionContext)
	return tc, ok
}

// NewPermissionDeniedResponse returns a tool response indicating the user
// denied permission, with StopTurn set so the agent loop does not retry.
func NewPermissionDeniedResponse() fantasy.ToolResponse {
	resp := fantasy.NewTextErrorResponse("User denied permission")
	resp.StopTurn = true
	return resp
}

// ghAvailable indicates whether the `gh` CLI is available on PATH.
var ghAvailable = func() bool {
	if testing.Testing() {
		return false
	}
	_, err := exec.LookPath("gh")
	return err == nil
}()

// toolDescriptionData is the common data structure for tool description templates.
type toolDescriptionData struct {
	GhAvailable bool
}

// renderToolDescription renders a tool description template with the given data.
func renderToolDescription(tmpl *template.Template) string {
	data := toolDescriptionData{
		GhAvailable: ghAvailable,
	}
	var out bytes.Buffer
	if err := tmpl.Execute(&out, data); err != nil {
		panic("failed to execute tool description template: " + err.Error())
	}
	return out.String()
}

// renderTemplate renders a Go template with the given data.
func renderTemplate(tmpl *template.Template, data any) string {
	var out bytes.Buffer
	if err := tmpl.Execute(&out, data); err != nil {
		panic("failed to execute tool description template: " + err.Error())
	}
	return out.String()
}
