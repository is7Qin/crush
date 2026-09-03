package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrateLegacyPermissionName(t *testing.T) {
	t.Parallel()

	t.Run("no legacy entry passes through unchanged", func(t *testing.T) {
		t.Parallel()
		in := []string{"bash", "view"}
		assert.Equal(t, in, migrateLegacyPermissionName(in))
	})

	t.Run("legacy agent loads as call_agent in place", func(t *testing.T) {
		t.Parallel()
		got := migrateLegacyPermissionName([]string{"bash", "agent", "view"})
		assert.Equal(t, []string{"bash", "call_agent", "view"}, got)
	})

	t.Run("explicit call_agent wins over the legacy entry", func(t *testing.T) {
		t.Parallel()
		got := migrateLegacyPermissionName([]string{"agent", "view", "call_agent"})
		assert.Equal(t, []string{"view", "call_agent"}, got,
			"the migrated list names the delegation tool exactly once, at its explicit position")
	})

	t.Run("repeated legacy entries collapse", func(t *testing.T) {
		t.Parallel()
		got := migrateLegacyPermissionName([]string{"agent", "agent"})
		assert.Equal(t, []string{"call_agent"}, got)
	})
}

func TestValidateAgentProfiles_ReservesHiddenProfile(t *testing.T) {
	t.Parallel()
	err := ValidateAgentProfiles(map[string]AgentProfilePatch{
		AgenticFetchInternalProfile: {},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reserved")

	// The reservation keys on the canonical normalized form; an
	// unnormalized mixed-case alias is a different key until folded.
	normalized, err := NormalizeAgentProfileKeys(map[string]AgentProfilePatch{
		"Agentic_Fetch_Internal": {},
	})
	require.NoError(t, err)
	err = ValidateAgentProfiles(normalized)
	require.Error(t, err, "case-folded aliases hit the same reservation")
}

func TestResolveAgentProfile_ReservedHiddenProfile(t *testing.T) {
	cfg := &Config{
		Options: &Options{},
		Agents:  map[string]Agent{},
		AgentProfiles: map[string]AgentProfilePatch{
			AgenticFetchInternalProfile: {},
		},
	}
	_, err := cfg.ResolveAgentProfile(AgenticFetchInternalProfile)
	assert.ErrorIs(t, err, ErrUnknownAgentProfile)
}

// isolateConfigEnv points every global config/data location at an empty
// temp dir so only the project file under test contributes.
func isolateConfigEnv(t *testing.T) {
	t.Helper()
	isolated := t.TempDir()
	t.Setenv("HOME", isolated)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(isolated, ".config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(isolated, ".local", "share"))
	t.Setenv("CRUSH_GLOBAL_CONFIG", filepath.Join(isolated, ".config", "crush"))
	t.Setenv("CRUSH_GLOBAL_DATA", filepath.Join(isolated, ".local", "share", "crush"))
}

func loadProjectJSON(t *testing.T, name, body string) *ConfigStore {
	t.Helper()
	isolateConfigEnv(t)
	workDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(workDir, name), []byte(body), 0o644))
	store, err := Load(workDir, t.TempDir(), false)
	require.NoError(t, err)
	return store
}

// TestLoad_LegacyAgentPermissionEntriesLoadAsCallAgent pins the
// migration boundary: a stored crush.json that still names the old
// `agent` tool loads as call_agent in both the allow and the deny
// list, an explicit call_agent entry wins over the legacy one, and
// re-serialization emits only call_agent.
func TestLoad_LegacyAgentPermissionEntriesLoadAsCallAgent(t *testing.T) {
	t.Run("allow and deny lists migrate", func(t *testing.T) {
		store := loadProjectJSON(t, "crush.json", `{
			"options": {"disable_provider_auto_update": true, "disabled_tools": ["agent"]},
			"permissions": {"allowed_tools": ["agent", "view"]}
		}`)
		cfg := store.Config()
		require.NotNil(t, cfg.Permissions)
		assert.Equal(t, []string{"call_agent", "view"}, cfg.Permissions.AllowedTools)
		assert.Equal(t, []string{"call_agent"}, cfg.Options.DisabledTools)

		out := string(mustMarshalConfig(cfg))
		assert.Contains(t, out, `"call_agent"`)
		assert.NotContains(t, out, `"agent"`,
			"new serialization emits only call_agent")
	})

	t.Run("explicit call_agent wins", func(t *testing.T) {
		store := loadProjectJSON(t, "crush.json", `{
			"options": {"disable_provider_auto_update": true},
			"permissions": {"allowed_tools": ["agent", "view", "call_agent"]}
		}`)
		assert.Equal(t, []string{"view", "call_agent"},
			store.Config().Permissions.AllowedTools)
	})
}

// TestLoad_CrushrcLegacyAgentWriteEmitsCallAgent proves the write
// path: a `permissions allow agent` builtin stores call_agent.
func TestLoad_CrushrcLegacyAgentWriteEmitsCallAgent(t *testing.T) {
	store := loadProjectJSON(t, "crushrc", `permissions allow agent view`)
	cfg := store.Config()
	require.NotNil(t, cfg.Permissions)
	assert.Equal(t, []string{"call_agent", "view"}, cfg.Permissions.AllowedTools)
	assert.False(t, strings.Contains(string(mustMarshalConfig(cfg)), `"agent"`))
}

// TestLoad_ReservedHiddenProfileFailsLoad proves the internal profile
// name is not user-configurable.
func TestLoad_ReservedHiddenProfileFailsLoad(t *testing.T) {
	isolateConfigEnv(t)
	workDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(workDir, "crush.json"), []byte(`{
		"options": {"disable_provider_auto_update": true},
		"agents": {"agentic_fetch_internal": {"description": "mine"}}
	}`), 0o644))
	_, err := Load(workDir, t.TempDir(), false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reserved")
}
