package workspace

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/app"
	"github.com/charmbracelet/crush/internal/client"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/proto"
	"github.com/stretchr/testify/require"
)

// fakePrimaryCoordinator is a minimal agent.Coordinator for local-mode
// tests: it records SetPrimaryAgent selections and can be armed with a
// canned failure to model the busy/unknown/disabled outcomes.
type fakePrimaryCoordinator struct {
	mu      sync.Mutex
	profile string
	failErr error
}

func (c *fakePrimaryCoordinator) Run(context.Context, string, string, ...message.Attachment) (*fantasy.AgentResult, error) {
	return nil, nil
}

func (c *fakePrimaryCoordinator) Continue(context.Context, string) (*fantasy.AgentResult, error) {
	return nil, nil
}

func (c *fakePrimaryCoordinator) RunAccepted(context.Context, *agent.AcceptedRun, string, string, ...message.Attachment) (*fantasy.AgentResult, error) {
	return nil, nil
}

func (c *fakePrimaryCoordinator) BeginAccepted(string) *agent.AcceptedRun       { return nil }
func (c *fakePrimaryCoordinator) Cancel(string)                                 {}
func (c *fakePrimaryCoordinator) CancelAll()                                    {}
func (c *fakePrimaryCoordinator) IsBusy() bool                                  { return false }
func (c *fakePrimaryCoordinator) IsSessionBusy(string) bool                     { return false }
func (c *fakePrimaryCoordinator) QueuedPrompts(string) int                      { return 0 }
func (c *fakePrimaryCoordinator) QueuedPromptsList(string) []string             { return nil }
func (c *fakePrimaryCoordinator) ClearQueue(string)                             {}
func (c *fakePrimaryCoordinator) Summarize(context.Context, string) error       { return nil }
func (c *fakePrimaryCoordinator) Model() agent.Model                            { return agent.Model{} }
func (c *fakePrimaryCoordinator) UpdateModels(context.Context) error            { return nil }
func (c *fakePrimaryCoordinator) GenerateTitle(context.Context, string, string) {}
func (c *fakePrimaryCoordinator) SetPrimaryAgent(_ context.Context, profile string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failErr != nil {
		return c.failErr
	}
	c.profile = profile
	return nil
}

func (c *fakePrimaryCoordinator) PrimaryAgent() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.profile
}

func TestAppWorkspace_SetPrimaryAgent_LocalModeSwitch(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	a := app.NewForTest(ctx)
	coord := &fakePrimaryCoordinator{}
	a.AgentCoordinator = coord
	var w Workspace = NewAppWorkspace(a, nil)

	require.NoError(t, w.SetPrimaryAgent(ctx, "fast"))
	require.Equal(t, "fast", coord.PrimaryAgent())
}

func TestAppWorkspace_SetPrimaryAgent_SelectionErrorsPassThrough(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		failErr error
	}{
		{name: "busy", failErr: agent.ErrPrimaryAgentBusy},
		{name: "unknown", failErr: config.ErrUnknownAgentProfile},
		{name: "disabled", failErr: config.ErrAgentProfileDisabled},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			a := app.NewForTest(ctx)
			coord := &fakePrimaryCoordinator{failErr: tt.failErr}
			a.AgentCoordinator = coord
			var w Workspace = NewAppWorkspace(a, nil)

			require.ErrorIs(t, w.SetPrimaryAgent(ctx, "off"), tt.failErr)
			// The failed switch leaves the previous selection intact.
			require.Equal(t, "", coord.PrimaryAgent())
		})
	}
}

func TestAppWorkspace_SetPrimaryAgent_NoCoordinator(t *testing.T) {
	t.Parallel()
	a := app.NewForTest(t.Context())
	a.AgentCoordinator = nil
	var w Workspace = NewAppWorkspace(a, nil)

	require.ErrorContains(t, w.SetPrimaryAgent(t.Context(), "fast"), "agent configuration is missing")
}

// TestClientWorkspace_SetPrimaryAgent_RemoteMode proves the remote
// Workspace reaches the same capability: the client SDK posts the
// profile to the primary route carrying the attached client id.
func TestClientWorkspace_SetPrimaryAgent_RemoteMode(t *testing.T) {
	t.Parallel()

	var got struct {
		path, method, cid, profile string
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.path = r.URL.Path
		got.method = r.Method
		got.cid = r.URL.Query().Get("client_id")
		var req proto.AgentPrimaryRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("failed to decode primary request: %v", err)
		}
		got.profile = req.Profile
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	u, err := url.Parse(srv.URL)
	require.NoError(t, err)
	c, err := client.NewClient(t.TempDir(), "tcp", u.Host)
	require.NoError(t, err)
	var w Workspace = NewClientWorkspace(c, proto.Workspace{ID: "ws-1"})

	require.NoError(t, w.SetPrimaryAgent(t.Context(), "fast"))
	require.Equal(t, http.MethodPost, got.method)
	require.Equal(t, "/v1/workspaces/ws-1/agent/primary", got.path)
	require.Equal(t, c.ClientID(), got.cid)
	require.Equal(t, "fast", got.profile)
}
