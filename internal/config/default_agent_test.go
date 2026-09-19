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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			c := &Config{Options: tt.options, AgentProfiles: tt.patches}
			require.Equal(t, tt.want, c.DefaultPrimaryAgent())
		})
	}
}
