package backend

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/stretchr/testify/require"
)

// primaryCoordinator records SetPrimaryAgent calls on top of the
// blocking coordinator and can be armed with a canned failure to model
// the busy/unknown/disabled outcomes without booting a real agent.
type primaryCoordinator struct {
	*blockingCoordinator
	mu       sync.Mutex
	profile  string
	failWith error
}

func newPrimaryCoordinator() *primaryCoordinator {
	return &primaryCoordinator{blockingCoordinator: newBlockingCoordinator()}
}

func (c *primaryCoordinator) failWithErr(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failWith = err
}

func (c *primaryCoordinator) selectedProfile() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.profile
}

func (c *primaryCoordinator) SetPrimaryAgent(_ context.Context, profile string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failWith != nil {
		return c.failWith
	}
	c.profile = profile
	return nil
}

func (c *primaryCoordinator) PrimaryAgent() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.profile
}

func TestSetPrimaryAgent_SwitchesAndReportsAgentInfo(t *testing.T) {
	t.Parallel()
	b, _ := newTestBackend(t)
	coord := newPrimaryCoordinator()
	ws := insertAgentWorkspace(t, b, coord)

	cid := newClientID(t)
	require.NoError(t, b.AttachClient(ws.ID, cid))
	t.Cleanup(func() { b.DetachClient(ws.ID, cid) })
	require.NoError(t, b.SetCurrentSession(ws.ID, cid, "S1"))

	require.NoError(t, b.SetPrimaryAgent(context.Background(), ws.ID, cid, "fast"))

	info, err := b.GetAgentInfo(ws.ID)
	require.NoError(t, err)
	require.Equal(t, "fast", info.PrimaryAgent)

	// The switch never touches the client's session binding.
	got, err := b.ClientCurrentSession(ws.ID, cid)
	require.NoError(t, err)
	require.Equal(t, "S1", got)
}

func TestSetPrimaryAgent_RequiresAttachedClient(t *testing.T) {
	t.Parallel()
	b, _ := newTestBackend(t)
	b.createGrace = time.Hour // keep the hold alive for the whole test
	coord := newPrimaryCoordinator()
	ws := insertAgentWorkspace(t, b, coord)

	ctx := context.Background()
	require.ErrorIs(t, b.SetPrimaryAgent(ctx, ws.ID, "", "fast"), ErrInvalidClientID)
	require.ErrorIs(t, b.SetPrimaryAgent(ctx, ws.ID, "not-a-uuid", "fast"), ErrInvalidClientID)
	require.ErrorIs(t, b.SetPrimaryAgent(ctx, "nope", newClientID(t), "fast"), ErrWorkspaceNotFound)
	require.ErrorIs(t, b.SetPrimaryAgent(ctx, ws.ID, newClientID(t), "fast"), ErrClientNotAttached)

	// Hold-only (registered, no live stream) is not attached.
	hold := newClientID(t)
	b.registerClient(ws, hold)
	require.ErrorIs(t, b.SetPrimaryAgent(ctx, ws.ID, hold, "fast"), ErrClientNotAttached)

	// A retired client stays rejected even after re-attachment mechanics.
	b.mu.Lock()
	if b.retired == nil {
		b.retired = make(map[string]struct{})
	}
	b.retired[hold] = struct{}{}
	b.mu.Unlock()
	require.ErrorIs(t, b.SetPrimaryAgent(ctx, ws.ID, hold, "fast"), ErrClientRetired)

	require.Equal(t, "", coord.selectedProfile(), "rejected calls never reach the coordinator")
	require.NoError(t, b.releaseHold(ws.ID, hold))
}

func TestSetPrimaryAgent_AgentNotInitialized(t *testing.T) {
	t.Parallel()
	b, _ := newTestBackend(t)
	ws := insertAgentWorkspace(t, b, nil)

	cid := newClientID(t)
	require.NoError(t, b.AttachClient(ws.ID, cid))
	t.Cleanup(func() { b.DetachClient(ws.ID, cid) })

	require.ErrorIs(t, b.SetPrimaryAgent(context.Background(), ws.ID, cid, "fast"),
		ErrAgentNotInitialized)
}

func TestSetPrimaryAgent_SelectionErrorsPassThrough(t *testing.T) {
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
			b, _ := newTestBackend(t)
			coord := newPrimaryCoordinator()
			coord.failWithErr(tt.failErr)
			ws := insertAgentWorkspace(t, b, coord)

			cid := newClientID(t)
			require.NoError(t, b.AttachClient(ws.ID, cid))
			t.Cleanup(func() { b.DetachClient(ws.ID, cid) })

			err := b.SetPrimaryAgent(context.Background(), ws.ID, cid, "off")
			require.ErrorIs(t, err, tt.failErr)

			// The failed switch leaves the active profile untouched.
			require.Equal(t, "", coord.selectedProfile())
			info, err := b.GetAgentInfo(ws.ID)
			require.NoError(t, err)
			require.Equal(t, "", info.PrimaryAgent)
		})
	}
}
