package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/charmbracelet/crush/internal/proto"
	"github.com/stretchr/testify/require"
)

func TestSetPrimaryAgent_SendsRouteQueryAndBody(t *testing.T) {
	t.Parallel()

	var lastReq *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastReq = r
		if r.URL.Path != "/v1/workspaces/ws1/agent/primary" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := captureClient(t, srv)
	require.NoError(t, c.SetPrimaryAgent(context.Background(), "ws1", "reviewer"))

	require.Equal(t, http.MethodPost, lastReq.Method)
	require.Equal(t, c.ClientID(), lastReq.URL.Query().Get("client_id"),
		"the primary route must carry the attached client id")
}

func TestSetPrimaryAgent_SurfacesServerOutcome(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		status      int
		body        proto.Error
		wantMessage string
	}{
		{
			name:        "busy",
			status:      http.StatusConflict,
			body:        proto.Error{Message: "primary agent is busy"},
			wantMessage: "primary agent is busy",
		},
		{
			name:        "unknown",
			status:      http.StatusBadRequest,
			body:        proto.Error{Message: "unknown agent profile: missing"},
			wantMessage: "unknown agent profile: missing",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_ = json.NewEncoder(w).Encode(tt.body)
			}))
			defer srv.Close()

			c := captureClient(t, srv)
			err := c.SetPrimaryAgent(context.Background(), "ws1", "off")
			require.Error(t, err)
			require.Contains(t, err.Error(), tt.wantMessage)
			require.Contains(t, err.Error(), "status code")
		})
	}
}
