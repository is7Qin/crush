package server

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/agent/task"
	"github.com/charmbracelet/crush/internal/agent/taskquestion"
	"github.com/charmbracelet/crush/internal/proto"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/charmbracelet/crush/internal/question"
	"github.com/stretchr/testify/require"
)

// TestWrapEvent_TaskLifecycleEnvelope asserts every contract task
// event type wraps into a task_event envelope with all required
// fields present (the client-side decoder rejects otherwise).
func TestWrapEvent_TaskLifecycleEnvelope(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 1, 1, 0, 0, 0, 5, time.UTC)
	for _, evType := range []task.EventType{
		task.EventCreated, task.EventStarted, task.EventWaitingForInput,
		task.EventResumed, task.EventCompleted, task.EventFailed,
		task.EventCancelled, task.EventInterrupted,
	} {
		rec := &task.Task{
			ID: "t1", OwnerSessionID: "owner", ParentSessionID: "p1",
			ChildSessionID: "c1", ParentMessageID: "pm1", ToolCallID: "tc1",
			Profile: "coder", Provider: "prov", Model: "model",
			Status: task.Status(evType), RunGeneration: 1, UpdatedAt: now,
		}
		payload := wrapEvent(pubsub.Event[task.Event]{
			Type:    pubsub.UpdatedEvent,
			Payload: task.Event{Type: evType, Task: rec},
		})
		require.NotNil(t, payload, "task events must not be dropped")
		require.Equal(t, pubsub.PayloadTypeTaskEvent, payload.Type)

		var ev pubsub.Event[proto.AgentTaskEvent]
		require.NoError(t, json.Unmarshal(payload.Payload, &ev), "envelope must satisfy the strict decoder for %s", evType)
		require.Equal(t, string(evType), ev.Payload.Type)
		require.Equal(t, "t1", ev.Payload.TaskID)
		require.Equal(t, "tc1", ev.Payload.ToolCallID)
		require.Equal(t, now.Format(time.RFC3339Nano), ev.Payload.At)
	}
}

// TestWrapEvent_TaskQuestionEnvelopes checks the child question
// request and notification wrappers carry the extended envelope: the
// batch plus question/task/child/run correlation.
func TestWrapEvent_TaskQuestionEnvelopes(t *testing.T) {
	t.Parallel()

	q := taskquestion.TaskQuestion{
		QuestionID: "q1", TaskID: "t1", OwnerSessionID: "o1",
		ChildSessionID: "c1", RunGeneration: 2,
		Batch: question.Request{
			ID: "b1", SessionID: "c1", ToolCallID: "tc1",
			Questions: []question.Question{{
				ID: "qq", Type: question.TypeYesNo, Text: "Proceed?", Description: "d",
			}},
		},
		Resolution: taskquestion.ResolutionPending,
		CreatedAt:  time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	payload := wrapEvent(pubsub.Event[taskquestion.TaskQuestion]{
		Type: pubsub.CreatedEvent, Payload: q,
	})
	require.NotNil(t, payload)
	require.Equal(t, pubsub.PayloadTypeTaskQuestionRequest, payload.Type)
	var ev pubsub.Event[proto.TaskQuestion]
	require.NoError(t, json.Unmarshal(payload.Payload, &ev))
	require.Equal(t, "q1", ev.Payload.QuestionID)
	require.Equal(t, "t1", ev.Payload.TaskID)
	require.Equal(t, "c1", ev.Payload.ChildSessionID)
	require.Equal(t, uint64(2), ev.Payload.RunGeneration)
	require.Equal(t, "Proceed?", ev.Payload.Batch.Questions[0].Question)
	require.Nil(t, ev.Payload.ResolvedAt)

	notif := wrapEvent(pubsub.Event[taskquestion.Notification]{
		Type: pubsub.CreatedEvent,
		Payload: taskquestion.Notification{
			QuestionID: "q1", TaskID: "t1", BatchID: "b1",
			Resolution: taskquestion.ResolutionAnswered,
		},
	})
	require.NotNil(t, notif)
	require.Equal(t, pubsub.PayloadTypeTaskQuestionNotification, notif.Type)
	var ne pubsub.Event[proto.TaskQuestionNotification]
	require.NoError(t, json.Unmarshal(notif.Payload, &ne))
	require.Equal(t, "q1", ne.Payload.QuestionID)
	require.Equal(t, "answered", ne.Payload.Resolution)
}

// TestWrapEvent_HiddenTaskEventDropped locks the SSE gate: lifecycle
// facts of hidden system-owned (agentic_fetch) tasks never serialize
// onto the shared workspace stream at any event type, while ordinary
// task events and metadata-free nil-record events still wrap.
// Reverting the IsHidden gate in wrapEvent fails this test.
func TestWrapEvent_HiddenTaskEventDropped(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 1, 1, 0, 0, 0, 5, time.UTC)
	for _, evType := range []task.EventType{
		task.EventCreated, task.EventStarted, task.EventWaitingForInput,
		task.EventResumed, task.EventCompleted, task.EventFailed,
		task.EventCancelled, task.EventInterrupted,
	} {
		hidden := &task.Task{
			ID: "h1", OwnerSessionID: "owner", ParentSessionID: "p1",
			ChildSessionID: "c1", ParentMessageID: "pm1", ToolCallID: "tc1",
			Profile: task.HiddenProfile, Provider: "prov", Model: "model",
			Status: task.Status(evType), RunGeneration: 1, UpdatedAt: now,
			Summary: "hidden transcript metadata",
		}
		require.Nil(t, wrapEvent(pubsub.Event[task.Event]{
			Type:    pubsub.UpdatedEvent,
			Payload: task.Event{Type: evType, Task: hidden},
		}), "hidden task events must never reach the SSE wire (%s)", evType)
	}

	// The ordinary path is untouched: a visible profile still wraps.
	visible := &task.Task{ID: "t1", Profile: "coder", Status: task.StatusRunning}
	require.NotNil(t, wrapEvent(pubsub.Event[task.Event]{
		Type:    pubsub.UpdatedEvent,
		Payload: task.Event{Type: task.EventStarted, Task: visible},
	}))
	// A record-less fact carries no hidden metadata and still wraps.
	require.NotNil(t, wrapEvent(pubsub.Event[task.Event]{
		Type:    pubsub.UpdatedEvent,
		Payload: task.Event{Type: task.EventStarted},
	}))
}

// TestWrapEvent_UnknownStillDroppable keeps the contract's tolerance:
// an unrecognized event type still returns nil so SSE drops it with a
// diagnostic rather than forwarding garbage.
func TestWrapEvent_UnknownStillDroppable(t *testing.T) {
	t.Parallel()
	require.Nil(t, wrapEvent(struct{ Odd int }{Odd: 1}))
}
