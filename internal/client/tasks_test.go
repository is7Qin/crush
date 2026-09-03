package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/proto"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/stretchr/testify/require"
)

// envelope wraps an inner event into a pubsub.Payload SSE body.
func envelope(t *testing.T, typ pubsub.PayloadType, inner any) pubsub.Payload {
	t.Helper()
	raw, err := json.Marshal(inner)
	require.NoError(t, err)
	return pubsub.Payload{Type: typ, Payload: raw}
}

func TestSubscribeEventsDecodesTaskCorrelationAndDropsMalformed(t *testing.T) {
	t.Parallel()

	resolved := "2026-01-01T00:00:05Z"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		frames := []pubsub.Payload{
			envelope(t, pubsub.PayloadTypeTaskEvent, pubsub.Event[proto.AgentTaskEvent]{
				Type: pubsub.UpdatedEvent,
				Payload: proto.AgentTaskEvent{
					Type: "started", TaskID: "t-1", ParentSessionID: "parent-1",
					ChildSessionID: "child-1", ParentMessageID: "pm-1",
					ToolCallID: "tc-1", Profile: "coder", ResolvedProvider: "prov",
					ResolvedModel: "model", Status: "running", RunGeneration: 3,
					At: "2026-01-01T00:00:00.000000001Z",
				},
			}),
			// A malformed task event (missing tool_call_id) must be
			// rejected by the strict wire decoder and dropped.
			{Type: pubsub.PayloadTypeTaskEvent, Payload: []byte(`{"type":"started","task_id":"x","parent_session_id":"p","profile":"coder","status":"running","at":"t"}`)},
			envelope(t, pubsub.PayloadTypeTaskQuestionRequest, pubsub.Event[proto.TaskQuestion]{
				Type: pubsub.CreatedEvent,
				Payload: proto.TaskQuestion{
					QuestionID: "q-9", TaskID: "t-1", OwnerSessionID: "owner",
					ChildSessionID: "child-1", RunGeneration: 3,
					Batch: proto.QuestionRequest{ID: "b1", Questions: []proto.QuestionItem{
						{ID: "qq", Type: "yes_no", Question: "Proceed?", Description: "d"},
					}},
					Resolution: "pending", CreatedAt: "2026-01-01T00:00:00Z",
					ResolvedAt: &resolved,
				},
			}),
			envelope(t, pubsub.PayloadTypeTaskQuestionNotification, pubsub.Event[proto.TaskQuestionNotification]{
				Type: pubsub.CreatedEvent,
				Payload: proto.TaskQuestionNotification{
					QuestionID: "q-9", TaskID: "t-1", BatchID: "b1", Resolution: "answered",
				},
			}),
		}
		for _, f := range frames {
			raw, err := json.Marshal(f)
			require.NoError(t, err)
			_, _ = w.Write([]byte("data: "))
			_, _ = w.Write(raw)
			_, _ = w.Write([]byte("\n\n"))
			flusher.Flush()
		}
		<-r.Context().Done()
	}))
	defer srv.Close()

	c := captureClient(t, srv)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events, err := c.SubscribeEvents(ctx, "ws1")
	require.NoError(t, err)

	// The started fact keeps both correlation ids across the wire.
	got := requireEvent[pubsub.Event[proto.AgentTaskEvent]](t, events)
	require.Equal(t, "t-1", got.Payload.TaskID)
	require.Equal(t, "tc-1", got.Payload.ToolCallID)
	require.Equal(t, uint64(3), got.Payload.RunGeneration)

	// The malformed frame was dropped; the next event is the
	// task-question request envelope.
	tq := requireEvent[pubsub.Event[proto.TaskQuestion]](t, events)
	require.Equal(t, "q-9", tq.Payload.QuestionID)
	require.Equal(t, "t-1", tq.Payload.TaskID)
	require.Equal(t, "b1", tq.Payload.Batch.ID)
	require.Equal(t, "Proceed?", tq.Payload.Batch.Questions[0].Question)

	tn := requireEvent[pubsub.Event[proto.TaskQuestionNotification]](t, events)
	require.Equal(t, "q-9", tn.Payload.QuestionID)
	require.Equal(t, "answered", tn.Payload.Resolution)

	// Nothing else may arrive (especially not the malformed "x" event).
	select {
	case ev, ok := <-events:
		if ok {
			t.Fatalf("unexpected trailing event %T", ev)
		}
	case <-time.After(100 * time.Millisecond):
	}
}

func requireEvent[T any](t *testing.T, evc <-chan any) T {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-evc:
			if !ok {
				t.Fatal("event channel closed")
			}
			if typed, is := ev.(T); is {
				return typed
			}
		case <-timeout:
			var zero T
			t.Fatalf("timed out waiting for %T", zero)
		}
	}
}

func TestTaskRoutes_SendPathsAndStatuses(t *testing.T) {
	t.Parallel()

	var lastReq *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastReq = r
		switch {
		case r.URL.Path == "/v1/workspaces/ws1/tasks" && r.Method == http.MethodGet:
			require.Equal(t, "parent-1", r.URL.Query().Get("parent_session_id"))
			_ = json.NewEncoder(w).Encode(proto.TaskListResponse{Tasks: []proto.TaskSnapshot{
				{ID: "t1", Status: "running"},
			}})
		case r.URL.Path == "/v1/workspaces/ws1/tasks/t1/messages":
			require.Equal(t, http.MethodPost, r.Method)
			var in proto.ChildMessageRequest
			_ = json.NewDecoder(r.Body).Decode(&in)
			require.Equal(t, "hi", in.Prompt)
			require.NotEmpty(t, in.Attachments, "attachments ride the request")
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(proto.ChildMessageAccepted{
				TaskID: "t1", ChildSessionID: "c1", Sequence: 4, Status: "running",
			})
		case r.URL.Path == "/v1/workspaces/ws1/task-questions/answer":
			var in proto.TaskQuestionAnswerRequest
			_ = json.NewDecoder(r.Body).Decode(&in)
			require.Equal(t, "q9", in.QuestionID)
			_ = json.NewEncoder(w).Encode(proto.TaskQuestionResolutionResponse{Resolved: true})
		case r.URL.Path == "/v1/workspaces/ws1/tasks/t1/cancel":
			_ = json.NewEncoder(w).Encode(proto.AgentCancelAccepted{TaskID: "t1", Status: "cancelled"})
		case r.URL.Path == "/v1/workspaces/ws1/tasks/resync":
			_ = json.NewEncoder(w).Encode(proto.TaskResyncResponse{
				Tasks: []proto.TaskSnapshot{{ID: "t1", Status: "completed"}},
			})
		case r.URL.Path == "/v1/workspaces/ws1/task-questions/pending":
			_ = json.NewEncoder(w).Encode(proto.TaskQuestionListResponse{
				Questions: []proto.TaskQuestion{{QuestionID: "q1", TaskID: "t1"}},
			})
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	c := captureClient(t, srv)

	list, err := c.TaskList(context.Background(), "ws1", "parent-1")
	require.NoError(t, err)
	require.Len(t, list.Tasks, 1)
	require.Equal(t, c.ClientID(), lastReq.URL.Query().Get("client_id"),
		"task routes always send the attached client id")

	acc, err := c.SendChildMessage(context.Background(), "ws1", "t1", "hi",
		[]proto.Attachment{{FilePath: "/p", FileName: "p", MimeType: "text/plain", Content: []byte("x")}})
	require.NoError(t, err)
	require.Equal(t, uint64(4), acc.Sequence)
	require.Equal(t, "c1", acc.ChildSessionID)

	resolved, err := c.AnswerTaskQuestion(context.Background(), "ws1", proto.TaskQuestionAnswerRequest{
		QuestionID: "q9", Responses: []proto.TaskQuestionAnswer{{QuestionID: "x"}},
	})
	require.NoError(t, err)
	require.True(t, resolved)

	cancelled, err := c.TaskCancel(context.Background(), "ws1", "t1")
	require.NoError(t, err)
	require.Equal(t, "cancelled", cancelled.Status)

	resync, err := c.TasksResync(context.Background(), "ws1")
	require.NoError(t, err)
	require.Equal(t, "completed", resync.Tasks[0].Status)

	pending, err := c.TaskQuestionsPending(context.Background(), "ws1")
	require.NoError(t, err)
	require.Equal(t, "q1", pending.Questions[0].QuestionID)
}

func TestTaskRoutes_ErrorCodeSurfaced(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(proto.Error{Message: "task owner forbidden", Code: "task_owner_forbidden"})
	}))
	defer srv.Close()

	c := captureClient(t, srv)
	_, err := c.TaskGet(context.Background(), "ws1", "t1")
	require.Error(t, err)
	require.Contains(t, err.Error(), "task owner forbidden")
}
