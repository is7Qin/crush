package task

import (
	"time"
	"unicode/utf8"
)

// MaxResultBytes bounds stored result text. Truncation happens only at
// a valid UTF-8 rune boundary.
const MaxResultBytes = 32 * 1024

// TruncateResult caps s at MaxResultBytes bytes without splitting a
// rune. The bool reports whether truncation occurred.
func TruncateResult(s string) (string, bool) {
	if len(s) <= MaxResultBytes {
		return s, false
	}
	b := s[:MaxResultBytes]
	for !utf8.ValidString(b) {
		b = b[:len(b)-1]
	}
	return b, true
}

// CapacityKey identifies one running-model slot pool. FIFO queues are
// per key.
type CapacityKey struct {
	WorkspaceID string `json:"workspace_id"`
	Provider    string `json:"provider"`
	Model       string `json:"model"`
}

// Task is a task record. Identity fields (ID, OwnerSessionID,
// ParentSessionID, ChildSessionID, ParentMessageID, ToolCallID,
// Profile, ProfileGeneration, Provider, Model, RunGeneration,
// ResumesTaskID, MessageID, Prompt, CreatedAt) are immutable after
// creation; lifecycle fields change only through manager transitions
// and dispatch. Values returned across the manager/store boundary
// are always copies.
type Task struct {
	ID              string `json:"id"`
	OwnerSessionID  string `json:"owner_session_id"`
	ParentSessionID string `json:"parent_session_id"`
	ChildSessionID  string `json:"child_session_id,omitempty"`
	// ParentMessageID and ToolCallID correlate the admission tool
	// call in the parent session. They come from trusted tool
	// context, never from model input.
	ParentMessageID string `json:"parent_message_id"`
	ToolCallID      string `json:"tool_call_id"`
	Profile         string `json:"profile"`
	// ProfileGeneration is the config generation the profile policy
	// was resolved from; a config reload only affects new tasks.
	ProfileGeneration uint64 `json:"profile_generation"`
	// RequestedModel is the caller-supplied "provider/model"
	// override, empty when the profile selection was used.
	// FallbackModels records the profile's ordered fallback list.
	RequestedModel string   `json:"requested_model,omitempty"`
	FallbackModels []string `json:"fallback_models,omitempty"`
	// PromptFingerprint and ToolFingerprint bind the attempt to the
	// exact prompt text and effective tool set it was admitted with.
	PromptFingerprint string `json:"prompt_fingerprint"`
	ToolFingerprint   string `json:"tool_fingerprint"`
	Provider          string `json:"resolved_provider"`
	Model             string `json:"resolved_model"`
	RunGeneration     uint64 `json:"run_generation"`
	// ResumesTaskID names the immediately preceding terminal
	// attempt in the same child session; MessageID is the mailbox
	// message this attempt was created to deliver. Both are set by
	// the dispatch repository, never by callers.
	ResumesTaskID string `json:"resumes_task_id,omitempty"`
	MessageID     string `json:"message_id,omitempty"`
	// TerminalGeneration and CostAggregatedGeneration fence the
	// exactly-once terminal delivery and parent cost aggregation.
	// Zero means unset.
	TerminalGeneration       uint64 `json:"terminal_generation,omitempty"`
	CostAggregatedGeneration uint64 `json:"cost_aggregated_generation,omitempty"`
	Status                   Status `json:"status"`
	Prompt                   string `json:"prompt"`
	Result                   string `json:"result,omitempty"`
	Summary                  string `json:"summary,omitempty"`
	Err                      string `json:"error,omitempty"`
	ResultTruncated          bool   `json:"result_truncated"`
	// PromptTokens, CompletionTokens, and Cost are this attempt's
	// terminal usage snapshot; parent aggregation is applied once
	// per run generation by the terminal transaction.
	PromptTokens     int64     `json:"prompt_tokens"`
	CompletionTokens int64     `json:"completion_tokens"`
	Cost             float64   `json:"cost"`
	CreatedAt        time.Time `json:"created_at"`
	StartedAt        time.Time `json:"started_at,omitempty"`
	CompletedAt      time.Time `json:"completed_at,omitempty"`
	// UpdatedAt is stamped by every persisted state change.
	UpdatedAt time.Time `json:"updated_at"`
}

func (t *Task) clone() *Task {
	c := *t
	return &c
}

// UsageDelta is one attempt's terminal usage snapshot. The terminal
// transaction aggregates it into the parent session exactly once per
// run generation.
type UsageDelta struct {
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	Cost             float64 `json:"cost"`
}

// TerminalUpdate is the payload of an exactly-once terminalization.
// Usage is this attempt's terminal usage snapshot: the terminal
// transaction stores it on the task row and aggregates it into the
// parent session once for the fenced run generation.
type TerminalUpdate struct {
	Status          Status
	Result          string
	Summary         string
	Err             string
	ResultTruncated bool
	CompletedAt     time.Time
	Usage           UsageDelta
	// Question optionally binds the terminalization to a task
	// question row: the terminal transaction conditionally resolves
	// that pending row with this outcome and rolls the whole
	// terminalization back if the fence does not hold
	// (ErrQuestionStale). Nil leaves the behavior unchanged apart
	// from the pending-question sweep.
	Question *QuestionOutcome
}

// Result is the bounded output of a completed task run.
type Result struct {
	Text    string
	Summary string
	// Usage is the attempt's terminal usage snapshot, aggregated
	// into the parent session by the terminal transaction exactly
	// once per run generation. Runners that cannot attribute usage
	// leave it zero.
	Usage UsageDelta
}
