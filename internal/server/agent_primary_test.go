package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/backend"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/proto"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// primaryHarness builds a controller over one workspace with a stub
// coordinator, an attached client bound to session S1, and that
// binding as the only session in the store.
func primaryHarness(t *testing.T) (*controllerV1, *backend.Workspace, *stubCoordinator, string) {
	t.Helper()
	c, ws := buildMultiSessionWorkspace(t, "S1")
	coord, ok := ws.App.AgentCoordinator.(*stubCoordinator)
	require.True(t, ok)
	cid := uuid.New().String()
	require.NoError(t, c.backend.AttachClient(ws.ID, cid))
	t.Cleanup(func() { c.backend.DetachClient(ws.ID, cid) })
	require.NoError(t, c.backend.SetCurrentSession(ws.ID, cid, "S1"))
	return c, ws, coord, cid
}

// postPrimary drives the primary-agent handler directly and returns
// the recorder. Empty cid omits the client_id query entirely.
func postPrimary(t *testing.T, c *controllerV1, wsID, cid, profile string) *httptest.ResponseRecorder {
	t.Helper()
	target := "/v1/workspaces/" + wsID + "/agent/primary"
	if cid != "" {
		target += "?client_id=" + cid
	}
	body, err := json.Marshal(proto.AgentPrimaryRequest{Profile: profile})
	require.NoError(t, err)
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, target, bytes.NewReader(body))
	req.SetPathValue("id", wsID)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c.handlePostWorkspaceAgentPrimary(rec, req)
	return rec
}

func getAgent(t *testing.T, c *controllerV1, wsID string) proto.AgentInfo {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet,
		"/v1/workspaces/"+wsID+"/agent", nil)
	req.SetPathValue("id", wsID)
	rec := httptest.NewRecorder()
	c.handleGetWorkspaceAgent(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	var info proto.AgentInfo
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &info))
	return info
}

func TestAgentPrimary_RejectsMissingOrInvalidClientID(t *testing.T) {
	t.Parallel()
	c, ws, coord, _ := primaryHarness(t)

	rec := postPrimary(t, c, ws.ID, "", "fast")
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	var e proto.Error
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &e))
	require.Equal(t, "client_required", e.Code)

	rec = postPrimary(t, c, ws.ID, "not-a-uuid", "fast")
	require.Equal(t, http.StatusUnauthorized, rec.Code)

	require.Equal(t, "", coord.primary, "unauthenticated calls must not switch the agent")
}

func TestAgentPrimary_UnattachedClientIsConflict(t *testing.T) {
	t.Parallel()
	c, ws, _, _ := primaryHarness(t)

	rec := postPrimary(t, c, ws.ID, uuid.New().String(), "fast")
	require.Equal(t, http.StatusConflict, rec.Code)
}

func TestAgentPrimary_UnknownWorkspaceIsNotFound(t *testing.T) {
	t.Parallel()
	c, _, _, cid := primaryHarness(t)

	rec := postPrimary(t, c, uuid.New().String(), cid, "fast")
	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestAgentPrimary_SwitchesAndReportsAgentInfo(t *testing.T) {
	t.Parallel()
	c, ws, coord, cid := primaryHarness(t)

	rec := postPrimary(t, c, ws.ID, cid, "reviewer")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "reviewer", coord.primary)

	require.Equal(t, "reviewer", getAgent(t, c, ws.ID).PrimaryAgent)

	// The switch leaves the current session and its bindings intact:
	// no session was created, and the client's binding is preserved.
	require.Len(t, listSessions(t, c, ws.ID), 1)
	bound, err := c.backend.ClientCurrentSession(ws.ID, cid)
	require.NoError(t, err)
	require.Equal(t, "S1", bound)
}

func TestAgentPrimary_ErrorStatusMapping(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		failErr    error
		wantStatus int
	}{
		{name: "busy", failErr: agent.ErrPrimaryAgentBusy, wantStatus: http.StatusConflict},
		{name: "unknown", failErr: config.ErrUnknownAgentProfile, wantStatus: http.StatusBadRequest},
		{name: "disabled", failErr: config.ErrAgentProfileDisabled, wantStatus: http.StatusBadRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c, ws, coord, cid := primaryHarness(t)
			coord.failPrimary = tt.failErr

			rec := postPrimary(t, c, ws.ID, cid, "off")
			require.Equal(t, tt.wantStatus, rec.Code)

			// The old primary agent stays active on failure.
			require.Equal(t, "", coord.primary)
			require.Equal(t, "", getAgent(t, c, ws.ID).PrimaryAgent)
		})
	}
}
