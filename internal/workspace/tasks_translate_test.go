package workspace

import (
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/agent/task"
	"github.com/charmbracelet/crush/internal/agent/taskquestion"
	"github.com/charmbracelet/crush/internal/proto"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/stretchr/testify/require"
)

// TestTranslateEvent_TaskEventKeepsCorrelation is the spec section 5
// acceptance: the client-side workspace translator rebuilds the
// domain task event preserving task_id + tool_call_id.
func TestTranslateEvent_TaskEventKeepsCorrelation(t *testing.T) {
	t.Parallel()

	w := NewClientWorkspace(nil, proto.Workspace{})
	out := w.translateEvent(pubsub.Event[proto.AgentTaskEvent]{
		Type: pubsub.UpdatedEvent,
		Payload: proto.AgentTaskEvent{
			Type: "waiting_for_input", TaskID: "t7", ParentSessionID: "p7",
			ChildSessionID: "c7", ParentMessageID: "pm7", ToolCallID: "tc7",
			Profile: "coder", ResolvedProvider: "prov", ResolvedModel: "model",
			Status: "waiting_for_input", Summary: "asking", RunGeneration: 2,
			At: "2026-01-01T00:00:00Z",
		},
	})

	got, ok := out.(pubsub.Event[task.Event])
	require.True(t, ok, "expected pubsub.Event[task.Event], got %T", out)
	require.Equal(t, task.EventWaitingForInput, got.Payload.Type)
	require.Equal(t, "t7", got.Payload.Task.ID)
	require.Equal(t, "tc7", got.Payload.Task.ToolCallID)
	require.Equal(t, task.StatusWaitingForInput, got.Payload.Task.Status)
	require.Equal(t, "prov", got.Payload.Task.Provider)
	require.Equal(t, uint64(2), got.Payload.Task.RunGeneration)
	require.False(t, got.Payload.Task.UpdatedAt.IsZero(), "the At stamp must survive")
}

// TestTranslateEvent_TaskQuestionRoundTrip asserts the child
// question envelope translates back into the exact domain record the
// local-mode TUI receives.
func TestTranslateEvent_TaskQuestionRoundTrip(t *testing.T) {
	t.Parallel()

	resolved := "2026-01-01T00:00:09Z"
	w := NewClientWorkspace(nil, proto.Workspace{})
	out := w.translateEvent(pubsub.Event[proto.TaskQuestion]{
		Type: pubsub.CreatedEvent,
		Payload: proto.TaskQuestion{
			QuestionID: "q5", TaskID: "t5", OwnerSessionID: "o5",
			ChildSessionID: "c5", RunGeneration: 4,
			Batch: proto.QuestionRequest{
				ID:         "b5",
				SessionID:  "c5",
				ToolCallID: "tc5",
				Questions: []proto.QuestionItem{{
					ID: "qq", Type: "single_choice", Question: "Which?",
					Description: "d", Choices: []proto.QuestionChoice{
						{ID: "a", Label: "A"}, {ID: "b", Label: "B"},
					},
				}},
			},
			Answers:    []proto.TaskQuestionAnswer{{QuestionID: "qq", SelectedIDs: []string{"a"}}},
			Resolution: "answered", CreatedAt: "2026-01-01T00:00:08Z",
			ResolvedAt: &resolved,
		},
	})

	got, ok := out.(pubsub.Event[taskquestion.TaskQuestion])
	require.True(t, ok, "expected pubsub.Event[taskquestion.TaskQuestion], got %T", out)
	q := got.Payload
	require.Equal(t, "q5", q.QuestionID)
	require.Equal(t, "t5", q.TaskID)
	require.Equal(t, "o5", q.OwnerSessionID)
	require.Equal(t, uint64(4), q.RunGeneration)
	require.Equal(t, taskquestion.ResolutionAnswered, q.Resolution)
	require.Equal(t, "Which?", q.Batch.Questions[0].Text)
	require.Equal(t, "B", q.Batch.Questions[0].Choices[1].Label)
	require.Equal(t, []string{"a"}, q.Answers[0].SelectedIDs)
	require.WithinDuration(t, time.Date(2026, 1, 1, 0, 0, 9, 0, time.UTC), q.ResolvedAt, time.Second)
}

// TestTranslateEvent_TaskQuestionNotification verifies resolution
// notifications translate with their question correlation intact.
func TestTranslateEvent_TaskQuestionNotification(t *testing.T) {
	t.Parallel()

	w := NewClientWorkspace(nil, proto.Workspace{})
	out := w.translateEvent(pubsub.Event[proto.TaskQuestionNotification]{
		Type: pubsub.CreatedEvent,
		Payload: proto.TaskQuestionNotification{
			QuestionID: "q3", TaskID: "t3", BatchID: "b3", Resolution: "cancelled",
		},
	})
	got, ok := out.(pubsub.Event[taskquestion.Notification])
	require.True(t, ok, "expected pubsub.Event[taskquestion.Notification], got %T", out)
	require.Equal(t, taskquestion.Notification{
		QuestionID: "q3", TaskID: "t3", BatchID: "b3",
		Resolution: taskquestion.ResolutionCancelled,
	}, got.Payload)
}
