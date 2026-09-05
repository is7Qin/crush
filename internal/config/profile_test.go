package config

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// decodePatch is a test helper that decodes one JSON object into an
// AgentProfilePatch.
func decodePatch(t *testing.T, data string) AgentProfilePatch {
	t.Helper()
	var p AgentProfilePatch
	require.NoError(t, json.Unmarshal([]byte(data), &p))
	return p
}

func TestOptional_PresenceSemantics(t *testing.T) {
	t.Parallel()

	t.Run("absent key yields Present=false", func(t *testing.T) {
		p := decodePatch(t, `{}`)
		assert.False(t, p.Model.Present)
		assert.False(t, p.AllowedTools.Present)
		assert.False(t, p.Disabled.Present)
	})

	t.Run("present empty value yields Present=true with empty value", func(t *testing.T) {
		p := decodePatch(t, `{"system_prompt":"","allowed_tools":[],"allowed_mcp":{}}`)
		require.True(t, p.SystemPrompt.Present)
		assert.Equal(t, "", p.SystemPrompt.Value)
		require.True(t, p.AllowedTools.Present)
		assert.Empty(t, p.AllowedTools.Value)
		require.True(t, p.AllowedMCP.Present)
		assert.Empty(t, p.AllowedMCP.Value)
	})

	t.Run("present value is decoded", func(t *testing.T) {
		p := decodePatch(t, `{"model":"openai/gpt-4o","reasoning_effort":"xhigh","max_steps":12,"disabled":true,"max_duration":90000000000}`)
		assert.Equal(t, Some("openai/gpt-4o"), p.Model)
		assert.Equal(t, Some("xhigh"), p.ReasoningEffort)
		assert.Equal(t, Some(12), p.MaxSteps)
		assert.Equal(t, Some(true), p.Disabled)
		assert.Equal(t, Some(90*time.Second), p.MaxDuration)
	})

	t.Run("null is rejected for every field", func(t *testing.T) {
		for _, field := range []string{"model", "models", "reasoning_effort", "allowed_tools", "disabled", "max_steps"} {
			var p AgentProfilePatch
			err := json.Unmarshal([]byte(`{"`+field+`":null}`), &p)
			require.Error(t, err, "field %s", field)
			assert.Contains(t, err.Error(), "null", "field %s", field)
		}
	})
}

func TestAgentProfilePatch_MarshalRoundTrip(t *testing.T) {
	t.Parallel()

	in := AgentProfilePatch{
		Model:           Some("openai/gpt-4o"),
		ReasoningEffort: Some("high"),
		AllowedTools:    Some([]string{}),
		MaxSteps:        Some(5),
	}

	data, err := json.Marshal(in)
	require.NoError(t, err)
	assert.JSONEq(t, `{"model":"openai/gpt-4o","reasoning_effort":"high","allowed_tools":[],"max_steps":5}`, string(data))

	var out AgentProfilePatch
	require.NoError(t, json.Unmarshal(data, &out))
	assert.Equal(t, in, out, "round-trip must preserve presence semantics")
}

func TestMergeAgentProfilePatches(t *testing.T) {
	t.Parallel()

	builtin := map[string]AgentProfilePatch{
		"coder": {
			Name:         Some("Coder"),
			Description:  Some("default desc"),
			AllowedTools: Some([]string{"bash", "edit"}),
			ContextPaths: Some([]string{"/default"}),
		},
	}
	global := map[string]AgentProfilePatch{
		"coder": {Description: Some("global desc"), ReasoningEffort: Some("medium")},
		"reviewer": {
			Description: Some("reviews code"),
		},
	}
	project := map[string]AgentProfilePatch{
		"coder": {
			Description:     Some("project desc"),
			AllowedTools:    Some([]string{"view"}), // replaces, never appends
			ContextPaths:    Some([]string{}),       // explicit empty clears
			ReasoningEffort: Some("max"),            // higher layer overrides
		},
	}

	merged := MergeAgentProfilePatches(builtin, global, project)

	coder := merged["coder"]
	assert.Equal(t, Some("Coder"), coder.Name, "absent higher-priority fields inherit")
	assert.Equal(t, Some("project desc"), coder.Description, "highest layer wins")
	assert.Equal(t, Some("max"), coder.ReasoningEffort, "highest present layer wins the effort")
	assert.Equal(t, Some([]string{"view"}), coder.AllowedTools, "lists replace")
	require.True(t, coder.ContextPaths.Present)
	assert.Empty(t, coder.ContextPaths.Value, "explicit empty clears the inherited list")

	assert.Contains(t, merged, "reviewer", "new profile keys are retained")

	// An omitted effort inherits the lower layer's value instead of
	// being materialized as an explicit empty.
	inherit := MergeAgentProfilePatches(
		map[string]AgentProfilePatch{"coder": {ReasoningEffort: Some("low")}},
		map[string]AgentProfilePatch{"coder": {Description: Some("d")}},
	)
	assert.Equal(t, Some("low"), inherit["coder"].ReasoningEffort, "omitted effort inherits")
}

func TestSetupAgents_AppliesProfilePatches(t *testing.T) {
	t.Parallel()

	newCfg := func(profiles map[string]AgentProfilePatch) *Config {
		return &Config{
			Options:       &Options{DisabledTools: []string{}},
			AgentProfiles: profiles,
		}
	}

	t.Run("defaults are unchanged without patches", func(t *testing.T) {
		cfg := newCfg(nil)
		cfg.SetupAgents()
		assert.Equal(t, allToolNames(), cfg.Agents[AgentCoder].AllowedTools)
		assert.False(t, slices.Contains(cfg.Agents[AgentTask].AllowedTools, DelegationToolName))
	})

	t.Run("coder allow-list narrows workspace tools", func(t *testing.T) {
		cfg := newCfg(map[string]AgentProfilePatch{
			AgentCoder: {AllowedTools: Some([]string{"bash", "view", "not_a_tool"})},
		})
		cfg.SetupAgents()
		assert.Equal(t, []string{"bash", "view"}, cfg.Agents[AgentCoder].AllowedTools)
	})

	t.Run("empty allow-list means all workspace tools", func(t *testing.T) {
		cfg := newCfg(map[string]AgentProfilePatch{
			AgentCoder: {AllowedTools: Some([]string{})},
		})
		cfg.SetupAgents()
		assert.Equal(t, allToolNames(), cfg.Agents[AgentCoder].AllowedTools)
	})

	t.Run("denied tools win over allow-list", func(t *testing.T) {
		cfg := newCfg(map[string]AgentProfilePatch{
			AgentCoder: {
				AllowedTools: Some([]string{"bash", "edit"}),
				DeniedTools:  Some([]string{"edit"}),
			},
		})
		cfg.SetupAgents()
		assert.Equal(t, []string{"bash"}, cfg.Agents[AgentCoder].AllowedTools)
	})

	t.Run("scalar fields override", func(t *testing.T) {
		cfg := newCfg(map[string]AgentProfilePatch{
			AgentCoder: {Name: Some("Overridden"), Description: Some("d"), Disabled: Some(true)},
		})
		cfg.SetupAgents()
		coder := cfg.Agents[AgentCoder]
		assert.Equal(t, "Overridden", coder.Name)
		assert.Equal(t, "d", coder.Description)
		assert.True(t, coder.Disabled)
	})

	t.Run("task child never sees the delegation tool even when allowed", func(t *testing.T) {
		cfg := newCfg(map[string]AgentProfilePatch{
			AgentTask: {AllowedTools: Some([]string{"view", DelegationToolName})},
		})
		cfg.SetupAgents()
		task := cfg.Agents[AgentTask]
		assert.Equal(t, []string{"view"}, task.AllowedTools)
		assert.False(t, slices.Contains(task.AllowedTools, DelegationToolName))
	})

	t.Run("unknown profiles stay out of the runtime map", func(t *testing.T) {
		cfg := newCfg(map[string]AgentProfilePatch{
			"reviewer": {Description: Some("reviews")},
		})
		cfg.SetupAgents()
		assert.Len(t, cfg.Agents, 2)
		assert.Contains(t, cfg.AgentProfiles, "reviewer", "source data is retained for the later resolver")
	})
}

func TestLoadFromBytes_DecodesAgentProfiles(t *testing.T) {
	t.Parallel()

	t.Run("agents key is decoded, not silently ignored", func(t *testing.T) {
		cfg, err := loadFromBytes([][]byte{
			[]byte(`{"agents":{"coder":{"description":"global"},"Reviewer":{"model":"openai/gpt-4o"}}}`),
			[]byte(`{"agents":{"coder":{"description":"project"}}}`),
		})
		require.NoError(t, err)
		require.Len(t, cfg.AgentProfiles, 2)
		assert.Equal(t, Some("project"), cfg.AgentProfiles["coder"].Description)
		assert.Equal(t, Some("openai/gpt-4o"), cfg.AgentProfiles["Reviewer"].Model)
	})

	t.Run("null profile field fails the load", func(t *testing.T) {
		_, err := loadFromBytes([][]byte{[]byte(`{"agents":{"coder":{"model":null}}}`)})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "null")
	})
}
