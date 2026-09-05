package model

import (
	"context"
	"errors"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/charmbracelet/crush/internal/ui/dialog"
	"github.com/charmbracelet/crush/internal/workspace"
	"github.com/stretchr/testify/require"
)

// agentsTestWorkspace is a fake workspace covering the reads and the
// single mutation the Switch Agent flow performs.
type agentsTestWorkspace struct {
	workspace.Workspace
	cfg          *config.Config
	primary      string
	primaryCalls int
	setCtx       context.Context
	setProfile   string
	setErr       error
	loadedID     string
}

func (w *agentsTestWorkspace) Config() *config.Config { return w.cfg }

func (w *agentsTestWorkspace) PrimaryAgent() string {
	w.primaryCalls++
	return w.primary
}

func (w *agentsTestWorkspace) SetPrimaryAgent(ctx context.Context, profile string) error {
	w.setCtx, w.setProfile = ctx, profile
	return w.setErr
}

func (w *agentsTestWorkspace) GetSession(_ context.Context, id string) (session.Session, error) {
	w.loadedID = id
	return session.Session{ID: id}, nil
}

func (w *agentsTestWorkspace) AgentIsReady() bool { return true }
func (w *agentsTestWorkspace) AgentIsBusy() bool  { return false }
func (w *agentsTestWorkspace) AgentModel() workspace.AgentModel {
	return workspace.AgentModel{}
}
func (w *agentsTestWorkspace) PermissionSkipRequests() bool { return false }

var _ workspace.Workspace = (*agentsTestWorkspace)(nil)

func agentsTestConfig() *config.Config {
	return &config.Config{
		Agents: map[string]config.Agent{
			config.AgentCoder: {ID: config.AgentCoder, Name: config.AgentCoder, Description: "The everyday coding agent"},
			config.AgentTask:  {ID: config.AgentTask, Name: config.AgentTask, Description: "Delegates child tasks"},
		},
		AgentProfiles: map[string]config.AgentProfilePatch{
			"reviewer": {Description: config.Some("Reviews finished work")},
		},
	}
}

func newTestUIWithAgentsWorkspace(ws workspace.Workspace) *UI {
	u := newTestUI()
	u.com.Workspace = ws
	u.dialog = dialog.NewOverlay()
	return u
}

func TestOpenAgentsDialogPopulatesFromConfigWithoutIO(t *testing.T) {
	ws := &agentsTestWorkspace{cfg: agentsTestConfig(), primary: "task"}
	u := newTestUIWithAgentsWorkspace(ws)

	cmd := u.openAgentsDialog()
	require.True(t, u.dialog.ContainsDialog(dialog.AgentsID))
	require.Zero(t, ws.primaryCalls, "opening must not probe the workspace inline in Update")

	// The profile list is already in the dialog (pure config read):
	// enter on the first row selects "coder".
	agents := u.dialog.Dialog(dialog.AgentsID).(*dialog.Agents)
	require.Equal(t,
		dialog.ActionSelectAgentProfile{Profile: config.AgentCoder},
		agents.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter}))

	// The current profile lands via the fetch command + apply.
	var current currentAgentMsg
	for _, msg := range runCmd(t, cmd) {
		if c, ok := msg.(currentAgentMsg); ok {
			current = c
		}
	}
	require.Equal(t, 1, ws.primaryCalls, "the probe runs inside the tea.Cmd")
	require.Equal(t, "task", current.profile)
	require.NotPanics(t, func() { u.applyCurrentAgent(current) })

	// Applying a result with no open dialog is a no-op.
	u.dialog.CloseDialog(dialog.AgentsID)
	require.NotPanics(t, func() { u.applyCurrentAgent(current) })
}

func TestSwitchAgentSuccessClosesDialogAndRefreshes(t *testing.T) {
	ws := &agentsTestWorkspace{cfg: agentsTestConfig(), primary: "coder"}
	u := newTestUIWithAgentsWorkspace(ws)
	u.session = &session.Session{ID: "keep-me"}

	u.openAgentsDialog()
	cmd := u.handleDialogMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.True(t, u.dialog.ContainsDialog(dialog.AgentsID),
		"the dialog stays open until the async switch lands")

	var switched switchAgentMsg
	for _, msg := range runCmd(t, cmd) {
		if s, ok := msg.(switchAgentMsg); ok {
			switched = s
		}
	}
	require.NoError(t, switched.err)
	require.Equal(t, config.AgentCoder, switched.profile)
	require.NotNil(t, ws.setCtx, "the switch must go through the workspace with a context")
	require.Equal(t, config.AgentCoder, ws.setProfile)

	cmds := u.applySwitchAgent(switched)
	require.NotEmpty(t, cmds)
	require.False(t, u.dialog.ContainsDialog(dialog.AgentsID),
		"a successful switch closes the dialog")
	require.Equal(t, "keep-me", u.session.ID)
	require.Empty(t, ws.loadedID, "a switch must not reload or replace the session")
	// The busy/model caches were invalidated and a refresh was dispatched.
	require.False(t, u.agentBusyCache.fresh(busyCacheTTL))
}

func TestSwitchAgentFailureKeepsDialogAndSession(t *testing.T) {
	ws := &agentsTestWorkspace{
		cfg:     agentsTestConfig(),
		primary: "coder",
		setErr:  errors.New("primary agent is busy"),
	}
	u := newTestUIWithAgentsWorkspace(ws)
	u.session = &session.Session{ID: "keep-me"}

	u.openAgentsDialog()
	cmd := u.handleDialogMsg(tea.KeyPressMsg{Code: tea.KeyEnter})

	var switched switchAgentMsg
	for _, msg := range runCmd(t, cmd) {
		if s, ok := msg.(switchAgentMsg); ok {
			switched = s
		}
	}
	require.Error(t, switched.err)

	cmds := u.applySwitchAgent(switched)
	require.Len(t, cmds, 1, "a failed switch only reports the error")
	require.True(t, u.dialog.ContainsDialog(dialog.AgentsID),
		"a failed switch keeps the dialog open")
	require.Equal(t, "keep-me", u.session.ID)
	require.Empty(t, ws.loadedID)
}

func TestSwitchAgentBusyPrecheckSkipsWorkspaceCall(t *testing.T) {
	ws := &agentsTestWorkspace{cfg: agentsTestConfig(), primary: "coder"}
	u := newTestUIWithAgentsWorkspace(ws)
	u.session = &session.Session{ID: "keep-me"}
	u.agentBusyCache.set(true)

	u.openAgentsDialog()
	cmd := u.handleDialogMsg(tea.KeyPressMsg{Code: tea.KeyEnter})

	require.Empty(t, ws.setProfile, "the busy precheck must not call the workspace")
	require.True(t, u.dialog.ContainsDialog(dialog.AgentsID),
		"the busy precheck keeps the dialog open")
	require.Equal(t, "keep-me", u.session.ID)
	require.NotNil(t, cmd, "the busy case reports a warning")
}

func TestAgentProfileOptionsProjectDescriptions(t *testing.T) {
	ws := &agentsTestWorkspace{cfg: agentsTestConfig()}
	u := newTestUIWithAgentsWorkspace(ws)

	options := u.agentProfileOptions()

	byName := map[string]string{}
	for _, option := range options {
		byName[option.Name] = option.Description
	}
	require.Equal(t, "The everyday coding agent", byName[config.AgentCoder])
	require.Equal(t, "Delegates child tasks", byName[config.AgentTask])
	// Built-in roster entries resolve with their catalog descriptions.
	require.Contains(t, byName, config.AgentSisyphus)
	require.NotEmpty(t, byName[config.AgentSisyphus])
	// A missing config yields no rows instead of a panic.
	u.com.Workspace = &agentsTestWorkspace{}
	require.Nil(t, u.agentProfileOptions())
}
