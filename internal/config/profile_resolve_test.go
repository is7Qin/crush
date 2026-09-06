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

// TestResolvePrimaryAgentProfile_UsesCoderPalette locks the primary/child
// tool boundary: every selected primary exposes the complete built-in
// coder palette (subject only to options.disabled_tools), regardless of
// how narrow its child policy is, while child projections keep the
// profile policy and never gain the delegation pair or task controls.
func TestResolvePrimaryAgentProfile_UsesCoderPalette(t *testing.T) {
	t.Parallel()
	cfg := profileCfg(t, map[string]AgentProfilePatch{
		"narrow": {
			AllowedTools: Some([]string{"view", DelegationToolName, agenticFetchToolName}),
			AllowedMCP:   Some(map[string][]string{}),
		},
	})

	coder, err := cfg.ResolvePrimaryAgentProfile(AgentCoder)
	require.NoError(t, err)

	for _, name := range []string{AgentOracle, AgentExplore, "narrow"} {
		primary, err := cfg.ResolvePrimaryAgentProfile(name)
		require.NoError(t, err)
		assert.ElementsMatch(t, coder.Agent.AllowedTools, primary.Agent.AllowedTools,
			"primary %s gets the same palette as coder", name)
		for _, tool := range []string{
			DelegationToolName, agenticFetchToolName,
			"agent_status", "agent_output", "agent_list", "agent_cancel", "agent_message",
			"bash", "edit", "write", "fetch", "lsp_references", "list_mcp_resources",
		} {
			assert.Contains(t, primary.Agent.AllowedTools, tool,
				"primary %s exposes %s", name, tool)
		}
		assert.Nil(t, primary.Agent.AllowedMCP,
			"primary %s carries no MCP restriction", name)
	}

	// Role-level tool policy must not drop the profile's own overrides:
	// oracle keeps its built-in prompt on the primary projection.
	oraclePrimary, err := cfg.ResolvePrimaryAgentProfile(AgentOracle)
	require.NoError(t, err)
	assert.NotEmpty(t, oraclePrimary.SystemPrompt,
		"the profile prompt override survives the palette swap")

	// Children keep the profile policy after the primary resolutions
	// above: the projection must not have leaked through slice aliasing
	// of the shared coder Agents entry.
	oracleChild, err := cfg.ResolveAgentProfile(AgentOracle)
	require.NoError(t, err)
	assert.Contains(t, oracleChild.Agent.AllowedTools, "view")
	assert.Empty(t, oracleChild.Agent.AllowedMCP,
		"the research child still carries its no-MCP policy")
	for _, tool := range []string{
		"edit", "write", DelegationToolName, agenticFetchToolName, "agent_status",
	} {
		assert.False(t, slices.Contains(oracleChild.Agent.AllowedTools, tool),
			"child oracle never sees %s", tool)
	}
	assert.Contains(t, oracleChild.Agent.AllowedTools, "bash")
	narrowChild, err := cfg.ResolveAgentProfile("narrow")
	require.NoError(t, err)
	assert.Equal(t, []string{"view"}, narrowChild.Agent.AllowedTools,
		"child depth stays at the profile allow-list; the delegation pair is denied")

	// options.disabled_tools subtracts from the primary baseline.
	cfgDisabled := &Config{
		Options:   &Options{DisabledTools: []string{DelegationToolName, "agent_status", "bash"}},
		Providers: csync.NewMap[string, ProviderConfig](),
	}
	cfgDisabled.SetupAgents()
	disabledPrimary, err := cfgDisabled.ResolvePrimaryAgentProfile(AgentOracle)
	require.NoError(t, err)
	assert.False(t, slices.Contains(disabledPrimary.Agent.AllowedTools, DelegationToolName))
	assert.False(t, slices.Contains(disabledPrimary.Agent.AllowedTools, "agent_status"))
	assert.False(t, slices.Contains(disabledPrimary.Agent.AllowedTools, "bash"))
	assert.Contains(t, disabledPrimary.Agent.AllowedTools, "agent_list")
	assert.Contains(t, disabledPrimary.Agent.AllowedTools, "edit")
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

func TestResolveAgentProfile_ReasoningEffort(t *testing.T) {
	t.Parallel()

	t.Run("omitted effort is not present", func(t *testing.T) {
		cfg := profileCfg(t, map[string]AgentProfilePatch{
			"reviewer": {Model: Some("openai/gpt-4o")},
		})
		prof, err := cfg.ResolveAgentProfile("reviewer")
		require.NoError(t, err)
		assert.False(t, prof.ReasoningEffort.Present)
		assert.Empty(t, prof.Model.ReasoningEffort, "the model ref stays untouched")
	})

	t.Run("effort survives with a model override", func(t *testing.T) {
		cfg := profileCfg(t, map[string]AgentProfilePatch{
			"deep": {Model: Some("openai/gpt-4o"), ReasoningEffort: Some("xhigh")},
		})
		prof, err := cfg.ResolveAgentProfile("deep")
		require.NoError(t, err)
		assert.Equal(t, Some("xhigh"), prof.ReasoningEffort)
		require.True(t, prof.ModelSet)
		assert.Equal(t, "gpt-4o", prof.Model.Model)
	})

	t.Run("effort survives without a model override", func(t *testing.T) {
		cfg := profileCfg(t, map[string]AgentProfilePatch{
			"thinky": {ReasoningEffort: Some("minimal")},
		})
		prof, err := cfg.ResolveAgentProfile("thinky")
		require.NoError(t, err)
		assert.Equal(t, Some("minimal"), prof.ReasoningEffort)
		assert.False(t, prof.ModelSet)
	})

	t.Run("user patch effort overrides the builtin roster default", func(t *testing.T) {
		cfg := profileCfg(t, map[string]AgentProfilePatch{
			"oracle": {ReasoningEffort: Some("max")},
		})
		prof, err := cfg.ResolveAgentProfile("oracle")
		require.NoError(t, err)
		assert.Equal(t, Some("max"), prof.ReasoningEffort)
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

// TestDiscoverableAgentProfiles locks the discovery semantics: every
// built-in and configured profile is listed while none is disabled, and a
// disabled profile drops out of discovery for built-ins (coder/task),
// roster entries, and custom profiles alike.
func TestDiscoverableAgentProfiles(t *testing.T) {
	t.Parallel()

	t.Run("lists built-ins and custom profiles deterministically", func(t *testing.T) {
		cfg := profileCfg(t, map[string]AgentProfilePatch{
			"Reviewer": {Description: Some("d")},
			"scribe":   {Description: Some("s")},
		})
		names := cfg.DiscoverableAgentProfiles()
		roster := BuiltinAgentProfileNames()
		require.Len(t, names, len(roster)+2)
		assert.Equal(t, roster, names[:len(roster)],
			"roster names lead in declaration order")
		assert.Equal(t, []string{"reviewer", "scribe"}, names[len(roster):],
			"extra user keys follow sorted, case-folded, deduplicated")
		assert.Equal(t, names, cfg.DiscoverableAgentProfiles(),
			"repeated calls are identical")
	})

	t.Run("disabling coder or task removes them from discovery", func(t *testing.T) {
		cfg := profileCfg(t, map[string]AgentProfilePatch{
			AgentCoder: {Disabled: Some(true)},
		})
		assert.NotContains(t, cfg.DiscoverableAgentProfiles(), AgentCoder)
		assert.Contains(t, cfg.DiscoverableAgentProfiles(), AgentTask)

		cfg = profileCfg(t, map[string]AgentProfilePatch{
			AgentTask: {Disabled: Some(true)},
		})
		assert.NotContains(t, cfg.DiscoverableAgentProfiles(), AgentTask)
		assert.Contains(t, cfg.DiscoverableAgentProfiles(), AgentCoder)
	})

	t.Run("disabling a roster or custom profile removes it from discovery", func(t *testing.T) {
		cfg := profileCfg(t, map[string]AgentProfilePatch{
			AgentAtlas: {Disabled: Some(true)},
			"reviewer": {Description: Some("d")},
			"off":      {Disabled: Some(true)},
		})
		names := cfg.DiscoverableAgentProfiles()
		assert.NotContains(t, names, AgentAtlas)
		assert.NotContains(t, names, "off")
		assert.Contains(t, names, "reviewer")
		assert.Contains(t, names, AgentPrometheus, "other roster entries survive")
	})

	t.Run("unlisted profiles cannot be resolved", func(t *testing.T) {
		cfg := profileCfg(t, map[string]AgentProfilePatch{
			AgentCoder: {Disabled: Some(true)},
			"off":      {Disabled: Some(true)},
		})
		_, err := cfg.ResolveAgentProfile(AgentCoder)
		require.ErrorIs(t, err, ErrAgentProfileDisabled)
		_, err = cfg.ResolveAgentProfile("off")
		require.ErrorIs(t, err, ErrAgentProfileDisabled)
	})

	t.Run("the reserved internal profile is never listed", func(t *testing.T) {
		cfg := profileCfg(t, map[string]AgentProfilePatch{
			AgenticFetchInternalProfile: {Description: Some("forged")},
		})
		assert.NotContains(t, cfg.DiscoverableAgentProfiles(), AgenticFetchInternalProfile)
	})
}
