package config

import (
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// rosterNames are the OMO-native roster profile names a call_agent
// selection must resolve with no user configuration. The Crush base
// agents (coder, task) are exercised alongside them but are not roster
// entries.
var rosterNames = []string{
	AgentSisyphus, AgentHephaestus, AgentOracle, AgentLibrarian,
	AgentExplore, AgentMultimodalLooker, AgentPrometheus, AgentMetis,
	AgentMomus, AgentAtlas, AgentSisyphusJunior,
}

// researchRoster can inspect repositories through shell/git but cannot edit;
// multimodal-looker is the only strict read-only profile.
var researchRoster = []string{
	AgentOracle, AgentLibrarian, AgentExplore, AgentMetis, AgentMomus,
}

func TestResolveAgentProfile_RosterResolvesWithoutConfig(t *testing.T) {
	t.Parallel()
	cfg := profileCfg(t, nil)

	seen := map[string]string{}
	for _, name := range slices.Concat([]string{AgentCoder, AgentTask}, rosterNames) {
		prof, err := cfg.ResolveAgentProfile(name)
		require.NoError(t, err, "roster profile %s", name)
		assert.Equal(t, strings.ToLower(name), prof.Name)
		require.NotEmpty(t, prof.Agent.Description, "profile %s has a description", name)

		// Every child loses the delegation pair regardless of profile.
		assert.False(t, slices.Contains(prof.Agent.AllowedTools, DelegationToolName),
			"profile %s must not delegate", name)
		assert.False(t, slices.Contains(prof.Agent.AllowedTools, agenticFetchToolName),
			"profile %s must not spawn fetch agents", name)

		if desc := prof.Agent.Description; desc != "" {
			require.NotContains(t, seen, desc, "descriptions are distinct (shared with %s)", seen[desc])
			seen[desc] = name
		}
	}

	// Roster entries carry an inline prompt; coder/task keep their
	// embedded templates.
	for _, name := range rosterNames {
		prof, err := cfg.ResolveAgentProfile(name)
		require.NoError(t, err)
		assert.NotEmpty(t, prof.SystemPrompt, "roster profile %s ships a prompt", name)
	}
}

func TestResolveAgentProfile_RosterToolPolicy(t *testing.T) {
	t.Parallel()
	cfg := profileCfg(t, nil)

	for _, name := range rosterNames {
		prof, err := cfg.ResolveAgentProfile(name)
		require.NoError(t, err)
		isResearch := slices.Contains(researchRoster, name)

		if name == AgentMultimodalLooker {
			assert.Equal(t, []string{"view"}, prof.Agent.AllowedTools)
			continue
		}

		if isResearch {
			assert.Contains(t, prof.Agent.AllowedTools, "view", "profile %s", name)
			assert.Contains(t, prof.Agent.AllowedTools, "fetch", "profile %s", name)
			assert.Contains(t, prof.Agent.AllowedTools, "bash", "profile %s", name)
			for _, forbidden := range []string{"edit", "write", "multiedit", "todos"} {
				assert.False(t, slices.Contains(prof.Agent.AllowedTools, forbidden),
					"research profile %s must not carry %s", name, forbidden)
			}
			assert.NotNil(t, prof.Agent.AllowedMCP, "research profile %s gets no MCPs", name)
			assert.Empty(t, prof.Agent.AllowedMCP, "research profile %s gets no MCPs", name)
			assert.False(t, prof.CanDelegate, "research profile %s never delegates", name)
			assert.False(t, prof.CanAskQuestions, "research profile %s never blocks on questions", name)
			continue
		}

		assert.Contains(t, prof.Agent.AllowedTools, "bash", "coding profile %s", name)
		assert.Contains(t, prof.Agent.AllowedTools, "edit", "coding profile %s", name)
	}
}

func TestResolveAgentProfile_RosterChildGates(t *testing.T) {
	t.Parallel()

	t.Run("can_delegate=true cannot re-add delegation to a roster child", func(t *testing.T) {
		cfg := profileCfg(t, map[string]AgentProfilePatch{
			AgentSisyphus: {CanDelegate: Some(true)},
		})
		prof, err := cfg.ResolveAgentProfile(AgentSisyphus)
		require.NoError(t, err)
		assert.False(t, slices.Contains(prof.Agent.AllowedTools, DelegationToolName))
		assert.False(t, slices.Contains(prof.Agent.AllowedTools, agenticFetchToolName))
	})

	t.Run("workspace-disabled tools stay disabled for roster profiles", func(t *testing.T) {
		cfg := profileCfg(t, nil)
		cfg.Options.DisabledTools = []string{"bash"}
		cfg.SetupAgents()
		prof, err := cfg.ResolveAgentProfile(AgentSisyphus)
		require.NoError(t, err)
		assert.False(t, slices.Contains(prof.Agent.AllowedTools, "bash"))
		assert.Contains(t, prof.Agent.AllowedTools, "view")
	})
}

func TestResolveAgentProfile_RosterUserOverridePrecedence(t *testing.T) {
	t.Parallel()

	t.Run("present fields override, omitted fields inherit", func(t *testing.T) {
		cfg := profileCfg(t, map[string]AgentProfilePatch{
			AgentOracle: {
				Description:  Some("my oracle"),
				SystemPrompt: Some("You are my oracle."),
				Model:        Some("openai/gpt-4o-mini"),
			},
		})
		prof, err := cfg.ResolveAgentProfile(AgentOracle)
		require.NoError(t, err)
		assert.Equal(t, "my oracle", prof.Agent.Description)
		assert.Equal(t, "You are my oracle.", prof.SystemPrompt)
		require.True(t, prof.ModelSet)
		assert.Equal(t, "gpt-4o-mini", prof.Model.Model)
		// Inherited built-in policy: still read-only, still no delegation.
		assert.Contains(t, prof.Agent.AllowedTools, "view")
		assert.Contains(t, prof.Agent.AllowedTools, "bash")
	})

	t.Run("explicit empty system_prompt clears the built-in prompt", func(t *testing.T) {
		cfg := profileCfg(t, map[string]AgentProfilePatch{
			AgentExplore: {SystemPrompt: Some("")},
		})
		prof, err := cfg.ResolveAgentProfile(AgentExplore)
		require.NoError(t, err)
		assert.Empty(t, prof.SystemPrompt, "explicit empty must replace the roster prompt")
	})

	t.Run("prompt_file override drops the built-in inline prompt", func(t *testing.T) {
		cfg := profileCfg(t, map[string]AgentProfilePatch{
			AgentMomus: {PromptFile: Some("prompts/momus.md")},
		})
		prof, err := cfg.ResolveAgentProfile(AgentMomus)
		require.NoError(t, err)
		assert.Empty(t, prof.SystemPrompt, "prompt_file must not be shadowed by the built-in prompt")
		assert.Equal(t, "prompts/momus.md", prof.PromptFile)
	})

	t.Run("allow-list override replaces the read-only set", func(t *testing.T) {
		cfg := profileCfg(t, map[string]AgentProfilePatch{
			AgentExplore: {AllowedTools: Some([]string{"bash", "view", DelegationToolName})},
		})
		prof, err := cfg.ResolveAgentProfile(AgentExplore)
		require.NoError(t, err)
		assert.Equal(t, []string{"bash", "view"}, prof.Agent.AllowedTools)
	})

	t.Run("deny-list override subtracts from the built-in set", func(t *testing.T) {
		cfg := profileCfg(t, map[string]AgentProfilePatch{
			AgentLibrarian: {DeniedTools: Some([]string{"fetch", "sourcegraph"})},
		})
		prof, err := cfg.ResolveAgentProfile(AgentLibrarian)
		require.NoError(t, err)
		assert.False(t, slices.Contains(prof.Agent.AllowedTools, "fetch"))
		assert.Contains(t, prof.Agent.AllowedTools, "view")
	})

	t.Run("user can disable a roster profile", func(t *testing.T) {
		cfg := profileCfg(t, map[string]AgentProfilePatch{
			AgentAtlas: {Disabled: Some(true)},
		})
		_, err := cfg.ResolveAgentProfile(AgentAtlas)
		require.ErrorIs(t, err, ErrAgentProfileDisabled)
	})

	t.Run("user can extend a coding roster profile", func(t *testing.T) {
		cfg := profileCfg(t, map[string]AgentProfilePatch{
			AgentHephaestus: {MaxSteps: Some(40)},
		})
		prof, err := cfg.ResolveAgentProfile(AgentHephaestus)
		require.NoError(t, err)
		assert.Equal(t, 40, prof.MaxSteps)
		assert.False(t, prof.CanDelegate, "the built-in can_delegate=false survives")
	})
}

func TestResolveAgentProfile_RosterUnknownAndReserved(t *testing.T) {
	t.Parallel()
	cfg := profileCfg(t, map[string]AgentProfilePatch{
		AgenticFetchInternalProfile: {Description: Some("forged")},
	})

	_, err := cfg.ResolveAgentProfile("not-a-profile")
	require.ErrorIs(t, err, ErrUnknownAgentProfile)

	_, err = cfg.ResolveAgentProfile(AgenticFetchInternalProfile)
	require.ErrorIs(t, err, ErrUnknownAgentProfile,
		"the reserved internal name never resolves, roster or not")

	err = ValidateAgentProfiles(cfg.AgentProfiles)
	require.Error(t, err, "a user patch forging the reserved name is rejected at load")
}

func TestSetupAgents_RosterStaysOutOfRuntimeMap(t *testing.T) {
	t.Parallel()
	cfg := profileCfg(t, map[string]AgentProfilePatch{
		AgentSisyphus: {Description: Some("patched sisyphus")},
	})
	assert.Len(t, cfg.Agents, 2,
		"roster names resolve through the catalog, not the runtime agent map")
	assert.Contains(t, cfg.AgentProfiles, AgentSisyphus,
		"the user patch is retained unmutated for the resolver")
	assert.Equal(t, Some("patched sisyphus"), cfg.AgentProfiles[AgentSisyphus].Description)
}

func TestBuiltinAgentProfiles_PureDataAndNames(t *testing.T) {
	t.Parallel()
	profiles := builtinAgentProfiles()
	require.Len(t, profiles, len(rosterNames))

	for _, name := range BuiltinAgentProfileNames() {
		assert.Equal(t, strings.ToLower(name), name, "catalog keys are canonical")
	}
	for name, patch := range profiles {
		assert.True(t, patch.Description.Present, "profile %s", name)
		assert.True(t, patch.SystemPrompt.Present, "profile %s", name)
		assert.True(t, patch.CanDelegate.Present && !patch.CanDelegate.Value,
			"built-in roster profiles never delegate: %s", name)
	}
}
