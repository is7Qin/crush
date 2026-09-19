package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/stretchr/testify/require"
)

// profileContextTestConfig patches only the coder profile's
// context_paths, so the child/primary split is observable on the
// coder template (the only template rendering context files).
const profileContextTestConfig = `{
  "options": {"disable_default_providers": true, "disable_provider_auto_update": true},
  "providers": {"mock": {"id": "mock", "name": "Mock", "type": "openai",
    "base_url": "http://127.0.0.1:9/v1", "api_key": "test-key",
    "models": [
      {"id": "mock-model", "name": "Mock", "context_window": 8192, "default_max_tokens": 128}]}},
  "models": {"large": {"provider": "mock", "model": "mock-model"},
             "small": {"provider": "mock", "model": "mock-model"}},
  "agents": {
    "coder": {"context_paths": ["PROFILE_CTX.md"]}
  }
}`

func profileContextCoordinator(t *testing.T) *coordinator {
	t.Helper()
	env := testEnv(t)
	require.NoError(t, os.WriteFile(
		filepath.Join(env.workingDir, "crush.json"), []byte(profileContextTestConfig), 0o644))
	require.NoError(t, os.WriteFile(
		filepath.Join(env.workingDir, "PROFILE_CTX.md"), []byte("profile-ctx-marker"), 0o644))
	require.NoError(t, os.WriteFile(
		filepath.Join(env.workingDir, "GLOBAL_CTX.md"), []byte("global-ctx-marker"), 0o644))
	require.NoError(t, os.WriteFile(
		filepath.Join(env.workingDir, "IGNORED_CTX.md"), []byte("ignored-ctx-marker"), 0o644))
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)
	cfg.SetupAgents()
	cfg.Config().Options.ContextPaths = []string{"IGNORED_CTX.md"}
	cfg.Config().Options.GlobalContextPaths = []string{"GLOBAL_CTX.md"}
	cfg.Config().Options.SkillsPaths = nil
	return &coordinator{
		cfg:         cfg,
		sessions:    env.sessions,
		messages:    env.messages,
		permissions: env.permissions,
		history:     env.history,
		filetracker: *env.filetracker,
	}
}

// TestBuildProfileAgent_ProfileContextPaths pins the child merge
// rule end to end: the resolved profile's context_paths replaces
// the project list in the built child prompt while the global
// list still applies.
func TestBuildProfileAgent_ProfileContextPaths(t *testing.T) {
	t.Parallel()

	coord := profileContextCoordinator(t)
	agent, prof, err := coord.buildProfileAgent(t.Context(), config.AgentCoder, "")
	require.NoError(t, err)
	require.Equal(t, []string{"PROFILE_CTX.md"}, prof.Agent.ContextPaths)

	systemPrompt := agent.(*sessionAgent).systemPrompt.Get()
	require.Contains(t, systemPrompt, "profile-ctx-marker")
	require.Contains(t, systemPrompt, "global-ctx-marker",
		"global context paths always apply to child prompts")
	require.NotContains(t, systemPrompt, "ignored-ctx-marker",
		"the profile patch replaces the project context list")
}

// TestProfileSystemPrompt_PrimaryIgnoresProfileContextPaths pins
// the primary half of the merge rule: a coder context_paths patch
// never leaks into the primary prompt, which keeps reading the
// global options list exactly as before.
func TestProfileSystemPrompt_PrimaryIgnoresProfileContextPaths(t *testing.T) {
	t.Parallel()

	coord := profileContextCoordinator(t)
	prof, err := coord.cfg.Config().ResolvePrimaryAgentProfile(config.AgentCoder)
	require.NoError(t, err)
	systemPrompt, err := coord.profileSystemPrompt(
		t.Context(), prof, Model{Model: &toolCallStreamModel{}}, true)
	require.NoError(t, err)
	require.Contains(t, systemPrompt, "ignored-ctx-marker")
	require.Contains(t, systemPrompt, "global-ctx-marker")
	require.NotContains(t, systemPrompt, "profile-ctx-marker",
		"primary prompts must not take the profile context override")
}
