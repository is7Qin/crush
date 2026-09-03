package config

import (
	"slices"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/charmbracelet/crush/internal/csync"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// profileCfg builds a loaded-style config with one provider catalog and the
// given profile patches, run through SetupAgents like the real pipeline.
// ProfileGeneration mirrors the post-load value setDefaults produces.
func profileCfg(t *testing.T, profiles map[string]AgentProfilePatch) *Config {
	t.Helper()
	cfg := &Config{
		Options:           &Options{DisabledTools: []string{}},
		Providers:         csync.NewMap[string, ProviderConfig](),
		AgentProfiles:     profiles,
		ProfileGeneration: 1,
	}
	cfg.Providers.Set("openai", ProviderConfig{
		ID:     "openai",
		Models: []catwalk.Model{{ID: "gpt-4o"}, {ID: "gpt-4o-mini"}},
	})
	cfg.SetupAgents()
	return cfg
}

func TestResolveAgentProfile_Builtins(t *testing.T) {
	t.Parallel()
	cfg := profileCfg(t, nil)

	t.Run("task keeps its read-only policy", func(t *testing.T) {
		prof, err := cfg.ResolveAgentProfile(AgentTask)
		require.NoError(t, err)
		assert.Equal(t, AgentTask, prof.Name)
		assert.False(t, prof.ModelSet)
		assert.Contains(t, prof.Agent.AllowedTools, "view")
		assert.False(t, slices.Contains(prof.Agent.AllowedTools, DelegationToolName))
	})

	t.Run("coder loses the delegation pair at child depth", func(t *testing.T) {
		prof, err := cfg.ResolveAgentProfile(AgentCoder)
		require.NoError(t, err)
		assert.Contains(t, prof.Agent.AllowedTools, "bash")
		assert.Contains(t, prof.Agent.AllowedTools, "edit")
		assert.False(t, slices.Contains(prof.Agent.AllowedTools, DelegationToolName))
		assert.False(t, slices.Contains(prof.Agent.AllowedTools, agenticFetchToolName))
	})

	t.Run("name is case-folded", func(t *testing.T) {
		prof, err := cfg.ResolveAgentProfile("Coder")
		require.NoError(t, err)
		assert.Equal(t, AgentCoder, prof.Name)
	})

	t.Run("disabled builtin is rejected", func(t *testing.T) {
		disabled := profileCfg(t, map[string]AgentProfilePatch{
			AgentCoder: {Disabled: Some(true)},
		})
		_, err := disabled.ResolveAgentProfile(AgentCoder)
		require.ErrorIs(t, err, ErrAgentProfileDisabled)
	})
}

func TestResolveAgentProfile_Unknown(t *testing.T) {
	t.Parallel()
	cfg := profileCfg(t, nil)

	_, err := cfg.ResolveAgentProfile("reviewer")
	require.ErrorIs(t, err, ErrUnknownAgentProfile)
}

func TestResolveAgentProfile_CustomToolPolicy(t *testing.T) {
	t.Parallel()

	t.Run("no policy means full ordinary capabilities", func(t *testing.T) {
		cfg := profileCfg(t, map[string]AgentProfilePatch{
			"reviewer": {Description: Some("reviews code")},
		})
		prof, err := cfg.ResolveAgentProfile("reviewer")
		require.NoError(t, err)
		assert.Contains(t, prof.Agent.AllowedTools, "bash")
		assert.Contains(t, prof.Agent.AllowedTools, "edit")
		assert.False(t, slices.Contains(prof.Agent.AllowedTools, DelegationToolName))
		assert.False(t, slices.Contains(prof.Agent.AllowedTools, agenticFetchToolName))
	})

	t.Run("allow-list narrows and cannot re-add delegation tools", func(t *testing.T) {
		cfg := profileCfg(t, map[string]AgentProfilePatch{
			"reviewer": {AllowedTools: Some([]string{"view", DelegationToolName, agenticFetchToolName})},
		})
		prof, err := cfg.ResolveAgentProfile("reviewer")
		require.NoError(t, err)
		assert.Equal(t, []string{"view"}, prof.Agent.AllowedTools)
	})

	t.Run("deny-list subtracts", func(t *testing.T) {
		cfg := profileCfg(t, map[string]AgentProfilePatch{
			"reviewer": {DeniedTools: Some([]string{"bash", "edit", "write"})},
		})
		prof, err := cfg.ResolveAgentProfile("reviewer")
		require.NoError(t, err)
		assert.False(t, slices.Contains(prof.Agent.AllowedTools, "bash"))
		assert.Contains(t, prof.Agent.AllowedTools, "view")
	})

	t.Run("disabled custom profile is rejected", func(t *testing.T) {
		cfg := profileCfg(t, map[string]AgentProfilePatch{
			"reviewer": {Disabled: Some(true)},
		})
		_, err := cfg.ResolveAgentProfile("reviewer")
		require.ErrorIs(t, err, ErrAgentProfileDisabled)
	})

	t.Run("key is case-folded", func(t *testing.T) {
		cfg := profileCfg(t, map[string]AgentProfilePatch{
			"reviewer": {Description: Some("d")},
		})
		prof, err := cfg.ResolveAgentProfile("Reviewer")
		require.NoError(t, err)
		assert.Equal(t, "reviewer", prof.Name)
	})
}

func TestResolveAgentProfile_ModelOverride(t *testing.T) {
	t.Parallel()

	t.Run("exact ref resolves", func(t *testing.T) {
		cfg := profileCfg(t, map[string]AgentProfilePatch{
			"fast": {Model: Some("openai/gpt-4o-mini")},
		})
		prof, err := cfg.ResolveAgentProfile("fast")
		require.NoError(t, err)
		require.True(t, prof.ModelSet)
		assert.Equal(t, SelectedModel{Provider: "openai", Model: "gpt-4o-mini"}, prof.Model)
	})

	t.Run("unavailable exact ref fails without fallback", func(t *testing.T) {
		cfg := profileCfg(t, map[string]AgentProfilePatch{
			"fast": {Model: Some("openai/gpt-1")},
		})
		_, err := cfg.ResolveAgentProfile("fast")
		require.ErrorIs(t, err, ErrUnavailableModel)
	})

	t.Run("fallback list picks first available", func(t *testing.T) {
		cfg := profileCfg(t, map[string]AgentProfilePatch{
			"fast": {Models: Some([]string{"openai/gpt-1", "openai/gpt-4o-mini"})},
		})
		prof, err := cfg.ResolveAgentProfile("fast")
		require.NoError(t, err)
		require.True(t, prof.ModelSet)
		assert.Equal(t, "gpt-4o-mini", prof.Model.Model)
	})
}

func TestResolveAgentProfile_PromptOverride(t *testing.T) {
	t.Parallel()
	cfg := profileCfg(t, map[string]AgentProfilePatch{
		"reviewer": {SystemPrompt: Some("You review code.")},
		"scribe":   {PromptFile: Some("prompts/scribe.md")},
	})

	prof, err := cfg.ResolveAgentProfile("reviewer")
	require.NoError(t, err)
	assert.Equal(t, "You review code.", prof.SystemPrompt)
	assert.Empty(t, prof.PromptFile)

	prof, err = cfg.ResolveAgentProfile("scribe")
	require.NoError(t, err)
	assert.Equal(t, "prompts/scribe.md", prof.PromptFile)
	assert.Empty(t, prof.SystemPrompt)
}

func TestResolveAgentProfile_RuntimePolicy(t *testing.T) {
	t.Parallel()

	t.Run("omitted policy fields are allowed and unlimited", func(t *testing.T) {
		cfg := profileCfg(t, map[string]AgentProfilePatch{
			"reviewer": {Description: Some("d")},
		})
		prof, err := cfg.ResolveAgentProfile("reviewer")
		require.NoError(t, err)
		assert.True(t, prof.CanDelegate)
		assert.True(t, prof.CanAskQuestions)
		assert.Zero(t, prof.MaxSteps)
		assert.Zero(t, prof.MaxDuration)
		assert.Equal(t, uint64(1), prof.Generation)
	})

	t.Run("custom profile carries concrete limits", func(t *testing.T) {
		cfg := profileCfg(t, map[string]AgentProfilePatch{
			"limited": {MaxSteps: Some(3), MaxDuration: Some(90 * time.Second)},
		})
		prof, err := cfg.ResolveAgentProfile("limited")
		require.NoError(t, err)
		assert.Equal(t, 3, prof.MaxSteps)
		assert.Equal(t, 90*time.Second, prof.MaxDuration)
	})

	t.Run("can_ask_questions=false strips question even from an allow-list", func(t *testing.T) {
		cfg := profileCfg(t, map[string]AgentProfilePatch{
			"quiet": {
				AllowedTools:    Some([]string{"view", "question"}),
				CanAskQuestions: Some(false),
			},
		})
		prof, err := cfg.ResolveAgentProfile("quiet")
		require.NoError(t, err)
		assert.False(t, prof.CanAskQuestions)
		assert.False(t, slices.Contains(prof.Agent.AllowedTools, QuestionToolName),
			"policy denial must override the allow-list")
		assert.Contains(t, prof.Agent.AllowedTools, "view")
	})

	t.Run("can_delegate=false strips call_agent from the primary projection", func(t *testing.T) {
		cfg := profileCfg(t, map[string]AgentProfilePatch{
			AgentCoder: {CanDelegate: Some(false), CanAskQuestions: Some(false)},
		})
		// The derived Agents entry is what the coordinator builds the
		// primary tool palette from.
		assert.False(t, slices.Contains(cfg.Agents[AgentCoder].AllowedTools, DelegationToolName))
		assert.False(t, slices.Contains(cfg.Agents[AgentCoder].AllowedTools, QuestionToolName))

		prof, err := cfg.ResolveAgentProfile(AgentCoder)
		require.NoError(t, err)
		assert.False(t, prof.CanDelegate)
		assert.False(t, prof.CanAskQuestions)
	})

	t.Run("can_delegate=true cannot re-add delegation to a child", func(t *testing.T) {
		cfg := profileCfg(t, map[string]AgentProfilePatch{
			"reviewer": {
				AllowedTools: Some([]string{"view", DelegationToolName}),
				CanDelegate:  Some(true),
			},
		})
		prof, err := cfg.ResolveAgentProfile("reviewer")
		require.NoError(t, err)
		assert.True(t, prof.CanDelegate)
		assert.False(t, slices.Contains(prof.Agent.AllowedTools, DelegationToolName),
			"child depth always denies delegation")
	})
}
