package model

import (
	"encoding/json"
	"testing"

	"github.com/charmbracelet/crush/internal/agent/task"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/proto"
	"github.com/charmbracelet/crush/internal/ui/chat"
	"github.com/stretchr/testify/require"
)

func TestUI_ApplyTaskResyncRestoresTaskStateAndQuestions(t *testing.T) {
	// Given a loaded parent chat with a call_agent item and durable
	// recovery data for that delegation.
	u := newTestUI()
	item := chat.NewToolMessageItem(
		u.com.Styles,
		"parent-message",
		message.ToolCall{ID: "call-resync", Name: "call_agent", Input: `{}`},
		nil,
		false,
		"",
	)
	u.chat.SetMessages(item)

	payload, err := json.Marshal(task.Task{
		ID:              "task-resync",
		ParentSessionID: "parent-session",
		ParentMessageID: "parent-message",
		ToolCallID:      "call-resync",
		Profile:         "coder",
		Status:          task.StatusCompleted,
		RunGeneration:   3,
	})
	require.NoError(t, err)
	resync := proto.TaskResyncResponse{
		Tasks: []proto.TaskSnapshot{{
			ID:              "task-resync",
			OwnerSessionID:  "parent-session",
			ParentSessionID: "parent-session",
			ParentMessageID: "parent-message",
			ToolCallID:      "call-resync",
			Profile:         "coder",
			Status:          "completed",
			RunGeneration:   3,
			CreatedAt:       "2026-01-01T00:00:00Z",
			CompletedAt:     "2026-01-01T00:00:01Z",
		}},
		Outbox: []proto.OutboxEntry{{
			ID: "outbox-resync", TaskID: "task-resync", EventType: "completed", Payload: payload,
		}},
		Questions: []proto.TaskQuestion{{
			QuestionID:     "question-resync",
			TaskID:         "task-resync",
			OwnerSessionID: "parent-session",
			ChildSessionID: "child-session",
			RunGeneration:  3,
			Resolution:     "pending",
			Batch: proto.QuestionRequest{
				ID:         "batch-resync",
				SessionID:  "child-session",
				ToolCallID: "question-call",
				Questions: []proto.QuestionItem{{
					ID: "question-item", Type: "yes_no", Question: "Proceed?",
				}},
			},
		}},
	}

	// When the durable response is applied through the UI state path.
	u.applyTaskResync(resync)

	// Then the latest task state is correlated to the existing tool item.
	tracker, ok := item.(chat.TaskStateTracker)
	require.True(t, ok)
	require.Equal(t, chat.TaskState{
		TaskID: "task-resync", Event: "completed", Status: "completed", Revision: 3,
	}, tracker.TaskState())
	require.Equal(t, "question-resync", u.activeQuestionKey)
	require.Len(t, u.pendingTaskQuestions, 0)

	// A repeated resync does not duplicate the still-pending question.
	u.applyTaskResync(resync)
	require.Equal(t, "question-resync", u.activeQuestionKey)
	require.Empty(t, u.pendingTaskQuestions)

	// A response with no pending questions dismisses a form resolved while
	// the event stream was unavailable.
	u.applyTaskResync(proto.TaskResyncResponse{})
	require.Nil(t, u.activeInline)
	require.Empty(t, u.activeQuestionKey)
}
