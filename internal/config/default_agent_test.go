package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestDefaultPrimaryAgent pins the startup-agent selection: an unset,
// unknown, or disabled options.default_agent must never leave Crush
// without a primary agent, so each falls back to the coder base agent,
// while a discoverable name is honoured (case-insensitively).
func TestDefaultPrimaryAgent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		options *Options
		patches map[string]AgentProfilePatch
		want    string
	}{
		{
			name: "nil options falls back to coder",
			want: AgentCoder,
		},
		{
			name:    "unset falls back to coder",
			options: &Options{},
			want:    AgentCoder,
		},
		{
			name:    "blank is treated as unset",
			options: &Options{DefaultAgent: "   "},
			want:    AgentCoder,
		},
		{
			name:    "discoverable roster name is honoured",
			options: &Options{DefaultAgent: AgentOracle},
			want:    AgentOracle,
		},
		{
			name:    "name is matched case-insensitively",
			options: &Options{DefaultAgent: "Oracle"},
			want:    AgentOracle,
		},
		{
			name:    "base agent is honoured",
			options: &Options{DefaultAgent: AgentTask},
			want:    AgentTask,
		},
		{
			name:    "unknown name falls back to coder",
			options: &Options{DefaultAgent: "not-a-profile"},
			want:    AgentCoder,
		},
		{
			name:    "internal profile is never selectable",
			options: &Options{DefaultAgent: AgenticFetchInternalProfile},
			want:    AgentCoder,
		},
		{
			name:    "disabled profile falls back to coder",
			options: &Options{DefaultAgent: AgentOracle},
			patches: map[string]AgentProfilePatch{
				AgentOracle: {Disabled: Some(true)},
			},
			want: AgentCoder,
		},
		{
			name:    "configured roster name wins while base agents are disabled",
			options: &Options{DefaultAgent: AgentSisyphus},
			patches: disabledProfiles(AgentCoder, AgentTask),
			want:    AgentSisyphus,
		},
		{
			name:    "unset default with the base agents disabled falls to a discoverable profile",
			options: &Options{},
			patches: disabledProfiles(AgentCoder, AgentTask),
			want:    AgentSisyphus,
		},
		{
			name:    "unknown default with the base agents disabled falls to a discoverable profile",
			options: &Options{DefaultAgent: "not-a-profile"},
			patches: disabledProfiles(AgentCoder, AgentTask),
			want:    AgentSisyphus,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			c := &Config{Options: tt.options, AgentProfiles: tt.patches}
			require.Equal(t, tt.want, c.DefaultPrimaryAgent())
		})
	}
}

// disabledProfiles builds the agents-config patch that disables each name.
func disabledProfiles(names ...string) map[string]AgentProfilePatch {
	patches := make(map[string]AgentProfilePatch, len(names))
	for _, name := range names {
		patches[name] = AgentProfilePatch{Disabled: Some(true)}
	}
	return patches
}

// TestFallbackDiscoverableAgent pins the profile used when no specific one
// is requested. The coder base agent is preferred, but disabling it (or
// both base agents) must still yield a discoverable profile, because
// startup and profile-less delegations have nothing else to fall back on.
func TestFallbackDiscoverableAgent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		patches map[string]AgentProfilePatch
		want    string
	}{
		{
			name: "prefers the coder base agent",
			want: AgentCoder,
		},
		{
			name:    "falls to task when coder is disabled",
			patches: disabledProfiles(AgentCoder),
			want:    AgentTask,
		},
		{
			name:    "falls to the first roster profile when both base agents are disabled",
			patches: disabledProfiles(AgentCoder, AgentTask),
			want:    AgentSisyphus,
		},
		{
			name:    "keeps coder as a last resort when nothing is discoverable",
			patches: disabledProfiles(BuiltinAgentProfileNames()...),
			want:    AgentCoder,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			c := &Config{AgentProfiles: tt.patches}
			require.Equal(t, tt.want, c.FallbackDiscoverableAgent())
		})
	}
}
