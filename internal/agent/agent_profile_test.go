package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// profileTestConfig is a hermetic crushrc-era JSON config: one offline
// provider with two models, plus agent profiles exercising every routing
// branch (default policy, narrowed policy, model override, disabled,
// inline prompt, runtime lifecycle policy).
const profileTestConfig = `{
  "options": {"disable_default_providers": true, "disable_provider_auto_update": true},
  "providers": {"mock": {"id": "mock", "name": "Mock", "type": "openai",
    "base_url": "http://127.0.0.1:9/v1", "api_key": "test-key",
    "models": [
      {"id": "mock-model", "name": "Mock", "context_window": 8192, "default_max_tokens": 128},
      {"id": "other-model", "name": "Other", "context_window": 8192, "default_max_tokens": 128}]}},
  "models": {"large": {"provider": "mock", "model": "mock-model"},
             "small": {"provider": "mock", "model": "mock-model"}},
  "agents": {
    "reviewer": {"description": "reviews code"},
    "narrow": {"allowed_tools": ["view", "call_agent", "agentic_fetch"]},
    "fast": {"model": "mock/other-model"},
    "off": {"disabled": true},
    "silen": {"system_prompt": "You are silent."},
    "limited": {"max_steps": 1},
    "brief": {"max_duration": 50000000},
    "quiet": {"can_ask_questions": false}
  }
}`

func profileTestCoordinator(t *testing.T) *coordinator {
	t.Helper()
	env := testEnv(t)
	require.NoError(t, os.WriteFile(filepath.Join(env.workingDir, "crush.json"), []byte(profileTestConfig), 0o644))
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)
	cfg.SetupAgents()
	return &coordinator{
		cfg:         cfg,
		sessions:    env.sessions,
		messages:    env.messages,
		permissions: env.permissions,
		history:     env.history,
		filetracker: *env.filetracker,
	}
}

func childToolNames(t *testing.T, sa SessionAgent) []string {
	t.Helper()
	agent, ok := sa.(*sessionAgent)
	require.True(t, ok, "buildProfileAgent must return a *sessionAgent")
	var names []string
	for _, tool := range agent.tools.Copy() {
		names = append(names, tool.Info().Name)
	}
	return names
}

func assertNoDelegation(t *testing.T, names []string) {
	t.Helper()
	assert.False(t, slices.Contains(names, AgentToolName), "children never see call_agent")
	assert.False(t, slices.Contains(names, tools.AgenticFetchToolName), "children never see agentic_fetch")
}

func TestAgentToolSchema(t *testing.T) {
	coord := profileTestCoordinator(t)

	tool, err := coord.agentTool(t.Context())
	require.NoError(t, err)
	info := tool.Info()

	assert.Equal(t, "call_agent", info.Name)
	require.Contains(t, info.Parameters, "prompt")
	require.Contains(t, info.Parameters, "profile")
	assert.Contains(t, info.Required, "prompt")
	assert.False(t, slices.Contains(info.Required, "profile"), "profile is optional")
}

func TestBuildProfileAgent_Builtins(t *testing.T) {
	coord := profileTestCoordinator(t)

	t.Run("coder child keeps ordinary tools minus the delegation pair", func(t *testing.T) {
		agent, _, err := coord.buildProfileAgent(t.Context(), config.AgentCoder, "")
		require.NoError(t, err)
		names := childToolNames(t, agent)
		assert.Contains(t, names, "bash")
		assert.Contains(t, names, "edit")
		assertNoDelegation(t, names)
		assert.True(t, agent.(*sessionAgent).isSubAgent, "child must be flagged as sub-agent")
	})

	t.Run("task child stays read-only", func(t *testing.T) {
		agent, _, err := coord.buildProfileAgent(t.Context(), config.AgentTask, "")
		require.NoError(t, err)
		names := childToolNames(t, agent)
		assert.Contains(t, names, "view")
		assert.Contains(t, names, "grep")
		assert.False(t, slices.Contains(names, "bash"))
		assert.False(t, slices.Contains(names, "edit"))
		assertNoDelegation(t, names)
	})
}

func TestBuildProfileAgent_CustomProfile(t *testing.T) {
	coord := profileTestCoordinator(t)

	t.Run("no tool policy means full ordinary capabilities", func(t *testing.T) {
		agent, _, err := coord.buildProfileAgent(t.Context(), "reviewer", "")
		require.NoError(t, err)
		names := childToolNames(t, agent)
		assert.Contains(t, names, "bash")
		assert.Contains(t, names, "edit")
		assertNoDelegation(t, names)
	})

	t.Run("allow-list narrows and cannot re-add delegation tools", func(t *testing.T) {
		agent, _, err := coord.buildProfileAgent(t.Context(), "narrow", "")
		require.NoError(t, err)
		assert.Equal(t, []string{"view"}, childToolNames(t, agent))
	})

	t.Run("model override is honored", func(t *testing.T) {
		agent, _, err := coord.buildProfileAgent(t.Context(), "fast", "")
		require.NoError(t, err)
		assert.Equal(t, "other-model", agent.Model().ModelCfg.Model)
		assert.Equal(t, "mock", agent.Model().ModelCfg.Provider)
	})

	t.Run("inline system prompt is used verbatim", func(t *testing.T) {
		agent, _, err := coord.buildProfileAgent(t.Context(), "silen", "")
		require.NoError(t, err)
		prompt := agent.(*sessionAgent).systemPrompt.Get()
		assert.True(t, strings.HasPrefix(prompt, "You are silent."))
		assert.Contains(t, prompt, "CHILD SESSION RESTRICTION")
	})

	t.Run("each call builds a fresh agent", func(t *testing.T) {
		first, _, err := coord.buildProfileAgent(t.Context(), "reviewer", "")
		require.NoError(t, err)
		second, _, err := coord.buildProfileAgent(t.Context(), "reviewer", "")
		require.NoError(t, err)
		assert.NotSame(t, first, second)
	})
}

func TestBuildProfileAgent_BuiltinRoster(t *testing.T) {
	// The OMO-native roster resolves with no user configuration:
	// profileTestConfig defines none of these names.
	coord := profileTestCoordinator(t)

	t.Run("research roster child is read-only", func(t *testing.T) {
		agent, prof, err := coord.buildProfileAgent(t.Context(), "oracle", "")
		require.NoError(t, err)
		names := childToolNames(t, agent)
		assert.Contains(t, names, "view")
		assert.Contains(t, names, "grep")
		assert.False(t, slices.Contains(names, "bash"))
		assert.False(t, slices.Contains(names, "edit"))
		assertNoDelegation(t, names)
		assert.False(t, prof.CanDelegate)
		assert.False(t, prof.CanAskQuestions)
		require.NotEmpty(t, agent.(*sessionAgent).systemPrompt.Get(),
			"the built-in roster prompt must reach the child")
	})

	t.Run("planning roster child is read-only", func(t *testing.T) {
		agent, prof, err := coord.buildProfileAgent(t.Context(), "prometheus", "")
		require.NoError(t, err)
		names := childToolNames(t, agent)
		assert.Contains(t, names, "view")
		assert.Contains(t, names, "grep")
		assert.False(t, slices.Contains(names, "bash"))
		assert.False(t, slices.Contains(names, "edit"))
		assert.False(t, slices.Contains(names, "write"))
		assertNoDelegation(t, names)
		assert.False(t, prof.CanDelegate)
		require.NotEmpty(t, agent.(*sessionAgent).systemPrompt.Get(),
			"prometheus ships a planning prompt")
	})

	t.Run("coding roster child keeps ordinary tools", func(t *testing.T) {
		agent, _, err := coord.buildProfileAgent(t.Context(), "sisyphus", "")
		require.NoError(t, err)
		names := childToolNames(t, agent)
		assert.Contains(t, names, "bash")
		assert.Contains(t, names, "edit")
		assertNoDelegation(t, names)
	})
}

func TestBuildProfileAgent_Errors(t *testing.T) {
	coord := profileTestCoordinator(t)

	_, _, err := coord.buildProfileAgent(t.Context(), "nope", "")
	require.ErrorIs(t, err, config.ErrUnknownAgentProfile)

	_, _, err = coord.buildProfileAgent(t.Context(), "off", "")
	require.ErrorIs(t, err, config.ErrAgentProfileDisabled)
}

func TestAgentTool_ProfileErrorsAreToolResponses(t *testing.T) {
	coord := profileTestCoordinator(t)

	parent, err := coord.sessions.Create(t.Context(), "Parent")
	require.NoError(t, err)
	ctx := context.WithValue(t.Context(), tools.SessionIDContextKey, parent.ID)
	ctx = context.WithValue(ctx, tools.MessageIDContextKey, "msg-1")

	tool, err := coord.agentTool(ctx)
	require.NoError(t, err)

	t.Run("unknown profile", func(t *testing.T) {
		resp, err := tool.Run(ctx, fantasy.ToolCall{
			ID:    "call-1",
			Name:  AgentToolName,
			Input: `{"prompt":"do it","profile":"nope"}`,
		})
		require.NoError(t, err)
		require.True(t, resp.IsError)
		assert.Contains(t, resp.Content, "unknown agent profile")
	})

	t.Run("disabled profile", func(t *testing.T) {
		resp, err := tool.Run(ctx, fantasy.ToolCall{
			ID:    "call-2",
			Name:  AgentToolName,
			Input: `{"prompt":"do it","profile":"off"}`,
		})
		require.NoError(t, err)
		require.True(t, resp.IsError)
		assert.Contains(t, resp.Content, "agent profile disabled")
	})

	t.Run("missing prompt", func(t *testing.T) {
		resp, err := tool.Run(ctx, fantasy.ToolCall{
			ID:    "call-3",
			Name:  AgentToolName,
			Input: `{"profile":"reviewer"}`,
		})
		require.NoError(t, err)
		require.True(t, resp.IsError)
		assert.Contains(t, resp.Content, "prompt is required")
	})
}

func TestBuildProfileAgent_ReasoningEffort(t *testing.T) {
	env := testEnv(t)
	cfgJSON := `{
	  "options": {"disable_default_providers": true, "disable_provider_auto_update": true},
	  "providers": {
	    "mock": {
	      "id": "mock", "name": "Mock", "type": "openai",
	      "base_url": "http://127.0.0.1:9/v1", "api_key": "test-key",
	      "models": [
	        {"id": "mock-model", "name": "Mock", "context_window": 8192, "default_max_tokens": 128,
	         "can_reason": true, "reasoning_levels": ["low", "medium", "high", "max"],
	         "default_reasoning_effort": "medium"},
	        {"id": "other-model", "name": "Other", "context_window": 8192, "default_max_tokens": 128,
	         "can_reason": true, "reasoning_levels": ["low", "max"]}
	      ]
	    }
	  },
	  "models": {"large": {"provider": "mock", "model": "mock-model", "reasoning_effort": "low"},
	             "small": {"provider": "mock", "model": "mock-model"}},
	  "agents": {
	    "thinky": {"reasoning_effort": "high"},
	    "plain": {"description": "no effort set"},
	    "swapped": {"model": "mock/other-model", "reasoning_effort": "max"},
	    "overreacher": {"model": "mock/other-model", "reasoning_effort": "xhigh"}
	  }
	}`
	require.NoError(t, os.WriteFile(filepath.Join(env.workingDir, "crush.json"), []byte(cfgJSON), 0o644))
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)
	cfg.SetupAgents()
	coord := &coordinator{
		cfg:         cfg,
		sessions:    env.sessions,
		messages:    env.messages,
		permissions: env.permissions,
	}

	t.Run("profile effort overrides the global selection on the default model", func(t *testing.T) {
		agent, prof, err := coord.buildProfileAgent(t.Context(), "thinky", "")
		require.NoError(t, err)
		assert.Equal(t, config.Some("high"), prof.ReasoningEffort)
		assert.Equal(t, "mock-model", agent.Model().ModelCfg.Model)
		assert.Equal(t, "high", agent.Model().ModelCfg.ReasoningEffort)
	})

	t.Run("absent effort inherits the global selection", func(t *testing.T) {
		agent, _, err := coord.buildProfileAgent(t.Context(), "plain", "")
		require.NoError(t, err)
		assert.Equal(t, "low", agent.Model().ModelCfg.ReasoningEffort)
	})

	t.Run("effort applies alongside a model override", func(t *testing.T) {
		agent, _, err := coord.buildProfileAgent(t.Context(), "swapped", "")
		require.NoError(t, err)
		assert.Equal(t, "other-model", agent.Model().ModelCfg.Model)
		assert.Equal(t, "max", agent.Model().ModelCfg.ReasoningEffort)
	})

	t.Run("a level the model does not list never reaches provider options", func(t *testing.T) {
		// other-model supports low/max only; xhigh is a valid profile
		// strength but unsupported by this model, so the call-time
		// fallback must own it: the effective effort sent to the
		// provider is the model's first listed level, not xhigh.
		agent, _, err := coord.buildProfileAgent(t.Context(), "overreacher", "")
		require.NoError(t, err)
		require.Equal(t, "other-model", agent.Model().ModelCfg.Model)
		assert.Equal(t, "xhigh", agent.Model().ModelCfg.ReasoningEffort,
			"the selection carries the requested strength")
		assert.Equal(t, "low", effectiveReasoningEffort(agent.Model()),
			"an unsupported strength falls back instead of being sent")
	})
}

func TestBuildProfileAgent_UnknownModelOverrideFails(t *testing.T) {
	env := testEnv(t)
	cfgJSON := `{"options":{"disable_default_providers":true,"disable_provider_auto_update":true},
	 "providers":{"mock":{"id":"mock","name":"Mock","type":"openai",
	   "base_url":"http://127.0.0.1:9/v1","api_key":"test-key",
	   "models":[{"id":"mock-model","context_window":8192,"default_max_tokens":128}]}},
	 "models":{"large":{"provider":"mock","model":"mock-model"},
	           "small":{"provider":"mock","model":"mock-model"}},
	 "agents":{"ghost":{"model":"mock/missing-model"}}}`
	require.NoError(t, os.WriteFile(filepath.Join(env.workingDir, "crush.json"), []byte(cfgJSON), 0o644))
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)
	cfg.SetupAgents()
	coord := &coordinator{
		cfg:         cfg,
		sessions:    env.sessions,
		messages:    env.messages,
		permissions: env.permissions,
	}

	_, _, err = coord.buildProfileAgent(t.Context(), "ghost", "")
	require.True(t, errors.Is(err, config.ErrUnavailableModel), "got: %v", err)
}
