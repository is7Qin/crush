package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/charmbracelet/crush/internal/agent/task"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type busySessionAgent struct {
	mockSessionAgent
	busy bool
}

func (a *busySessionAgent) IsBusy() bool { return a.busy }

type primaryTaskControllerStub struct{}

func (primaryTaskControllerStub) Start(context.Context, task.StartRequest) (*task.Task, error) {
	return nil, nil
}

func (primaryTaskControllerStub) Status(context.Context, string, string) (*task.Task, error) {
	return nil, nil
}

func (primaryTaskControllerStub) Output(context.Context, string, string) (task.Result, bool, error) {
	return task.Result{}, false, nil
}

func (primaryTaskControllerStub) List(context.Context, string, string) ([]*task.Task, error) {
	return nil, nil
}

func (primaryTaskControllerStub) Cancel(context.Context, string, string) error { return nil }

func (primaryTaskControllerStub) AppendMessage(context.Context, task.MessageRequest) (task.MessageAccepted, error) {
	return task.MessageAccepted{}, nil
}

func TestCoordinator_SetPrimaryAgent_switchesProfile(t *testing.T) {
	coord := profileTestCoordinator(t)
	old := &mockSessionAgent{}
	coord.currentAgent = old
	coord.agents = map[string]SessionAgent{config.AgentCoder: old}

	err := coord.SetPrimaryAgent(t.Context(), "FAST")

	// Then
	require.NoError(t, err)
	assert.Equal(t, "fast", coord.PrimaryAgent())
	assert.NotSame(t, old, coord.currentAgent)
	assert.Equal(t, "other-model", coord.Model().ModelCfg.Model)
	assert.False(t, coord.currentAgent.(*sessionAgent).isSubAgent)
}

func TestCoordinator_SetPrimaryAgent_rejectsInvalidProfile(t *testing.T) {
	tests := []struct {
		name    string
		profile string
		wantErr error
	}{
		{name: "unknown", profile: "missing", wantErr: config.ErrUnknownAgentProfile},
		{name: "disabled", profile: "off", wantErr: config.ErrAgentProfileDisabled},
		{name: "reserved", profile: config.AgenticFetchInternalProfile, wantErr: config.ErrUnknownAgentProfile},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			coord := profileTestCoordinator(t)
			old := &mockSessionAgent{}
			coord.currentAgent = old
			coord.agents = map[string]SessionAgent{config.AgentCoder: old}

			err := coord.SetPrimaryAgent(t.Context(), tt.profile)

			require.ErrorIs(t, err, tt.wantErr)
			assert.Equal(t, "", coord.PrimaryAgent())
			assert.Same(t, old, coord.currentAgent)
		})
	}
}

func TestCoordinator_SetPrimaryAgent_acceptsCustomProfile(t *testing.T) {
	coord := profileTestCoordinator(t)
	old := &mockSessionAgent{}
	coord.currentAgent = old
	coord.agents = map[string]SessionAgent{config.AgentCoder: old}

	err := coord.SetPrimaryAgent(t.Context(), "reviewer")

	require.NoError(t, err)
	assert.Equal(t, "reviewer", coord.PrimaryAgent())
}

func TestCoordinator_SetPrimaryAgent_refusesWhileBusy(t *testing.T) {
	coord := profileTestCoordinator(t)
	old := &busySessionAgent{busy: true}
	coord.currentAgent = old
	coord.agents = map[string]SessionAgent{config.AgentCoder: old}

	err := coord.SetPrimaryAgent(t.Context(), "fast")

	require.ErrorIs(t, err, ErrPrimaryAgentBusy)
	assert.Equal(t, "", coord.PrimaryAgent())
	assert.Same(t, old, coord.currentAgent)
}

func TestCoordinator_SetPrimaryAgent_refusesAcceptedRun(t *testing.T) {
	coord := profileTestCoordinator(t)
	old := &mockSessionAgent{}
	coord.currentAgent = old
	coord.agents = map[string]SessionAgent{config.AgentCoder: old}

	accept := coord.BeginAccepted("session")
	require.NotNil(t, accept)

	err := coord.SetPrimaryAgent(t.Context(), "fast")

	require.ErrorIs(t, err, ErrPrimaryAgentBusy)
	accept.Close()
	require.NoError(t, coord.SetPrimaryAgent(t.Context(), "fast"))
}

func TestCoordinator_IsBusy_includesAcceptedRun(t *testing.T) {
	coord := profileTestCoordinator(t)
	primary := &mockSessionAgent{}
	coord.currentAgent = primary
	coord.agents = map[string]SessionAgent{config.AgentCoder: primary}

	accept := coord.BeginAccepted("session")
	require.NotNil(t, accept)
	require.True(t, coord.IsBusy())

	accept.Close()
	require.False(t, coord.IsBusy())
}

func TestCoordinator_SetPrimaryAgent_preservesOldAgentOnBuildFailure(t *testing.T) {
	coord := profileTestCoordinator(t)
	coord.cfg.Config().AgentProfiles["broken"] = config.AgentProfilePatch{
		PromptFile: config.Some("missing-prompt.md"),
	}
	old := &mockSessionAgent{}
	coord.currentAgent = old
	coord.agents = map[string]SessionAgent{config.AgentCoder: old}

	err := coord.SetPrimaryAgent(t.Context(), "broken")

	require.Error(t, err)
	assert.Same(t, old, coord.currentAgent)
	assert.Equal(t, "", coord.PrimaryAgent())
}

// TestCoordinator_SetPrimaryAgent_researchProfileGetsCoderPalette locks
// the clarified primary/child boundary: a research roster profile
// (oracle) switched to primary exposes the same complete built-in coder
// palette (call_agent, task controls, and the ordinary coding tools),
// while the same profile as a child stays read-only and non-recursive.
func TestCoordinator_SetPrimaryAgent_researchProfileGetsCoderPalette(t *testing.T) {
	coord := profileTestCoordinator(t)
	coord.tasks = primaryTaskControllerStub{}
	old := &mockSessionAgent{}
	coord.currentAgent = old
	coord.agents = map[string]SessionAgent{config.AgentCoder: old}

	require.NoError(t, coord.SetPrimaryAgent(t.Context(), "oracle"))
	primaryNames := toolNames(coord.currentAgent.(*sessionAgent).tools.Copy())
	assert.Contains(t, primaryNames, AgentToolName)
	for _, name := range []string{"agent_status", "agent_output", "agent_list", "agent_cancel", "agent_message"} {
		assert.Contains(t, primaryNames, name)
	}
	for _, name := range []string{"bash", "edit", "write", "fetch", "view", "grep", "todos", "lsp_symbols"} {
		assert.Contains(t, primaryNames, name)
	}

	child, _, err := coord.buildProfileAgent(t.Context(), "oracle", "")
	require.NoError(t, err)
	childNames := toolNames(child.(*sessionAgent).tools.Copy())
	assertNoDelegation(t, childNames)
	for _, name := range []string{"agent_status", "agent_output", "agent_list", "agent_cancel", "agent_message", "edit"} {
		assert.NotContains(t, childNames, name)
	}
	assert.Contains(t, childNames, "bash")
	assert.Contains(t, childNames, "view")
}

// TestCoordinator_SetPrimaryAgent_primaryPromptOmitsChildRestriction locks
// the primary/child prompt split: a roster profile selected as primary
// keeps its inline prompt without child restrictions, while the same profile
// as a child receives the no-delegation restriction.
func TestCoordinator_SetPrimaryAgent_primaryPromptOmitsChildRestriction(t *testing.T) {
	coord := profileTestCoordinator(t)
	coord.tasks = primaryTaskControllerStub{}
	old := &mockSessionAgent{}
	coord.currentAgent = old
	coord.agents = map[string]SessionAgent{config.AgentCoder: old}

	require.NoError(t, coord.SetPrimaryAgent(t.Context(), config.AgentSisyphus))
	primaryPrompt := coord.currentAgent.(*sessionAgent).systemPrompt.Get()
	assert.Contains(t, primaryPrompt, "relentless executor",
		"the profile's own prompt text must be preserved")
	assert.NotContains(t, primaryPrompt, "CHILD SESSION RESTRICTION")

	child, _, err := coord.buildProfileAgent(t.Context(), config.AgentSisyphus, "")
	require.NoError(t, err)
	childPrompt := child.(*sessionAgent).systemPrompt.Get()
	assert.Contains(t, childPrompt, "CHILD SESSION RESTRICTION")
	assert.Contains(t, childPrompt, "must not call",
		"the child prompt must keep its no-delegation constraint")

	// A custom inline profile prompt gets the same primary override and
	// stays verbatim as a child.
	require.NoError(t, coord.SetPrimaryAgent(t.Context(), "silen"))
	primarySilen := coord.currentAgent.(*sessionAgent).systemPrompt.Get()
	assert.True(t, strings.HasPrefix(primarySilen, "You are silent."))
	assert.NotContains(t, primarySilen, "CHILD SESSION RESTRICTION")
}

func TestCoordinator_UpdateModels_usesSelectedPrimaryProfile(t *testing.T) {
	coord := profileTestCoordinator(t)
	old := &mockSessionAgent{}
	coord.currentAgent = old
	coord.agents = map[string]SessionAgent{config.AgentCoder: old}
	require.NoError(t, coord.SetPrimaryAgent(t.Context(), "fast"))

	coord.cfg.Config().AgentProfiles["fast"] = config.AgentProfilePatch{
		Model: config.Some("mock/mock-model"),
	}
	coord.cfg.Config().Models[config.SelectedModelTypeLarge] = config.SelectedModel{
		Provider: "mock",
		Model:    "other-model",
	}

	err := coord.UpdateModels(t.Context())

	require.NoError(t, err)
	assert.Equal(t, "mock-model", coord.Model().ModelCfg.Model)
}

func TestCoordinator_SetPrimaryAgent_keepsPrimaryTaskControlsAndChildDepth(t *testing.T) {
	coord := profileTestCoordinator(t)
	coord.tasks = primaryTaskControllerStub{}
	old := &mockSessionAgent{}
	coord.currentAgent = old
	coord.agents = map[string]SessionAgent{config.AgentCoder: old}

	require.NoError(t, coord.SetPrimaryAgent(t.Context(), "reviewer"))
	primary := coord.currentAgent.(*sessionAgent)
	primaryNames := toolNames(primary.tools.Copy())
	for _, name := range []string{"agent_status", "agent_output", "agent_list", "agent_cancel", "agent_message"} {
		assert.Contains(t, primaryNames, name)
	}

	child, _, err := coord.buildProfileAgent(t.Context(), "reviewer", "")
	require.NoError(t, err)
	childNames := toolNames(child.(*sessionAgent).tools.Copy())
	for _, name := range []string{"agent_status", "agent_output", "agent_list", "agent_cancel", "agent_message", AgentToolName, tools.AgenticFetchToolName} {
		assert.NotContains(t, childNames, name)
	}
}
