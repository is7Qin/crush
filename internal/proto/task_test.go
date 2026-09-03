package proto

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestAgentTaskEventDecodeRequiresContractFields(t *testing.T) {
	t.Parallel()

	full := AgentTaskEvent{
		Type: "created", TaskID: "t1", ParentSessionID: "p1",
		ParentMessageID: "pm1", ToolCallID: "tc1", Profile: "coder",
		ResolvedProvider: "prov", ResolvedModel: "model", Status: "pending",
		RunGeneration: 1, At: "2026-01-01T00:00:00Z",
	}
	raw, err := json.Marshal(full)
	require.NoError(t, err)

	// Exact tags: the contract's snake_case surface.
	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &fields))
	for _, key := range []string{
		"type", "task_id", "parent_session_id", "parent_message_id",
		"tool_call_id", "profile", "resolved_provider", "resolved_model",
		"status", "run_generation", "at",
	} {
		require.Contains(t, fields, key)
	}

	// Round-trip is lossless.
	var back AgentTaskEvent
	require.NoError(t, json.Unmarshal(raw, &back))
	require.Equal(t, full, back)

	// Missing required fields are rejected.
	for _, drop := range []string{"type", "task_id", "parent_session_id", "tool_call_id", "profile", "status", "at"} {
		var m map[string]any
		require.NoError(t, json.Unmarshal(raw, &m))
		delete(m, drop)
		bad, err := json.Marshal(m)
		require.NoError(t, err)
		var ev AgentTaskEvent
		require.Error(t, json.Unmarshal(bad, &ev), "missing %q must be rejected", drop)
	}
}

func TestTaskQuestionWireShape(t *testing.T) {
	t.Parallel()

	resolved := "2026-01-01T00:00:01Z"
	q := TaskQuestion{
		QuestionID: "q1", TaskID: "t1", OwnerSessionID: "o1",
		ChildSessionID: "c1", RunGeneration: 2,
		Batch: QuestionRequest{ID: "b1", Questions: []QuestionItem{{
			ID: "x", Type: "yes_no", Question: "Proceed?", Description: "d",
		}}},
		Resolution: "answered", CreatedAt: "2026-01-01T00:00:00Z",
		ResolvedAt: &resolved,
	}
	raw, err := json.Marshal(q)
	require.NoError(t, err)
	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &fields))
	for _, key := range []string{
		"question_id", "task_id", "owner_session_id", "child_session_id",
		"run_generation", "batch", "resolution", "created_at", "resolved_at",
	} {
		require.Contains(t, fields, key)
	}

	// Nullable resolved_at emits JSON null; answers omit when empty.
	q.ResolvedAt = nil
	q.Resolution = "pending"
	raw, err = json.Marshal(q)
	require.NoError(t, err)
	require.Contains(t, string(raw), `"resolved_at":null`)
	require.NotContains(t, string(raw), `"answers"`)

	// QuestionResponse fields use the contract tags.
	type probe struct {
		Responses []TaskQuestionAnswer `json:"responses"`
	}
	pb, err := json.Marshal(probe{Responses: []TaskQuestionAnswer{{QuestionID: "x"}}})
	require.NoError(t, err)
	require.Contains(t, string(pb), `"question_id":"x"`)
	require.NotContains(t, string(pb), `"request_id"`)
}

func TestTaskSnapshotWireShape(t *testing.T) {
	t.Parallel()

	snap := TaskSnapshot{ID: "t1", OwnerSessionID: "o1", ParentMessageID: "pm", ToolCallID: "tc", Profile: "p", Provider: "pr", Model: "m", Status: "pending", CreatedAt: "x"}
	raw, err := json.Marshal(snap)
	require.NoError(t, err)
	for _, key := range []string{`"id"`, `"owner_session_id"`, `"parent_message_id"`, `"tool_call_id"`, `"resolved_provider"`, `"resolved_model"`, `"run_generation"`, `"status"`, `"truncated"`, `"created_at"`} {
		require.Contains(t, string(raw), key)
	}
	// Optional empties stay absent.
	require.NotContains(t, string(raw), `"parent_session_id"`)
	require.NotContains(t, string(raw), `"started_at"`)
}

func TestWireTimeRoundTrip(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 1, 2, 3, 4, 5, 123456700, time.UTC)
	utc, ok := WireTime(now)
	require.True(t, ok)
	require.True(t, strings.HasSuffix(utc, "Z"), "RFC3339Nano UTC")
	back, err := ParseWireTime(utc)
	require.NoError(t, err)
	require.True(t, back.Equal(now))

	_, ok = WireTime(time.Time{})
	require.False(t, ok, "zero time is unset")

	zoned, ok := WireTime(time.Date(2026, 1, 2, 3, 4, 5, 0, time.FixedZone("x", 3600)))
	require.True(t, ok)
	require.True(t, strings.HasSuffix(zoned, "Z"), "wire stamps normalize to UTC")
}

func TestChildMessageRequestShape(t *testing.T) {
	t.Parallel()

	raw, err := json.Marshal(ChildMessageRequest{Prompt: "hi", Attachments: []Attachment{
		{FilePath: "/p", FileName: "p", MimeType: "text/plain", Content: []byte("x")},
	}})
	require.NoError(t, err)
	require.Contains(t, string(raw), `"prompt":"hi"`)
	require.Contains(t, string(raw), `"file_path":"/p"`)
	require.Contains(t, string(raw), `"mime_type":"text/plain"`)
	// Attachments content is base64 on the wire.
	require.Contains(t, string(raw), `"content":"eA=="`)

	var back ChildMessageRequest
	require.NoError(t, json.Unmarshal(raw, &back))
	require.Equal(t, []byte("x"), back.Attachments[0].Content)

	// Empty attachments omit the field entirely.
	raw, err = json.Marshal(ChildMessageRequest{Prompt: "hi"})
	require.NoError(t, err)
	require.NotContains(t, string(raw), "attachments")
}
