package workspace

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/crush/internal/client"
	"github.com/charmbracelet/crush/internal/proto"
	"github.com/stretchr/testify/require"
)

func TestClientWorkspace_AfterReconnectRequestsTaskResync(t *testing.T) {
	t.Parallel()

	// Given a recovered client that is attached to its current session.
	var resyncs atomic.Int32
	want := proto.TaskResyncResponse{
		Tasks: []proto.TaskSnapshot{{ID: "task-1", Status: "completed"}},
		Outbox: []proto.OutboxEntry{{
			ID: "outbox-1", TaskID: "task-1", EventType: "completed",
			Payload: []byte(`{"id":"task-1"}`),
		}},
		Inbox: []proto.InboxEntry{{
			ID: "inbox-1", OwnerSessionID: "session-1", TaskID: "task-1",
			Payload: []byte(`{"task_id":"task-1"}`),
		}},
		Questions: []proto.TaskQuestion{{QuestionID: "question-1", TaskID: "task-1"}},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/workspaces/ws-1/current-session":
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/workspaces/ws-1/tasks/resync":
			resyncs.Add(1)
			require.NoError(t, json.NewEncoder(w).Encode(want))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	u, err := url.Parse(srv.URL)
	require.NoError(t, err)
	c, err := client.NewClient(t.TempDir(), "tcp", u.Host)
	require.NoError(t, err)
	ws := NewClientWorkspace(c, proto.Workspace{ID: "ws-1"})
	ws.mu.Lock()
	ws.lastSession = "session-1"
	ws.mu.Unlock()

	// When the event stream is re-established.
	var recovered ConnectionEvent
	ws.afterReconnect(func(msg tea.Msg) {
		recovered = msg.(ConnectionEvent)
	})

	// Then the durable task recovery read is performed once.
	require.Equal(t, int32(1), resyncs.Load())
	require.NotNil(t, recovered.TaskResync)
	require.Equal(t, want, *recovered.TaskResync)
}
