package config

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateAgentProfiles(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		json    string
		wantErr string
	}{
		{
			name: "valid patch",
			json: `{"coder":{"model":"openai/gpt-4o","max_steps":10,"max_duration":60000000000}}`,
		},
		{
			name:    "empty key",
			json:    `{"":{"description":"x"}}`,
			wantErr: "name must not be empty",
		},
		{
			name:    "key with whitespace",
			json:    `{"my agent":{"description":"x"}}`,
			wantErr: "whitespace or control",
		},
		{
			name:    "model and models together",
			json:    `{"coder":{"model":"openai/gpt-4o","models":["anthropic/claude"]}}`,
			wantErr: "model and models are mutually exclusive",
		},
		{
			name:    "empty model reference",
			json:    `{"coder":{"model":""}}`,
			wantErr: "model",
		},
		{
			name:    "model reference without provider",
			json:    `{"coder":{"model":"gpt-4o"}}`,
			wantErr: "invalid model reference",
		},
		{
			name:    "empty entry in models list",
			json:    `{"coder":{"models":["openai/gpt-4o",""]}}`,
			wantErr: "invalid model reference",
		},
		{
			name:    "zero max_steps",
			json:    `{"coder":{"max_steps":0}}`,
			wantErr: "max_steps must be greater than 0",
		},
		{
			name:    "negative max_duration",
			json:    `{"coder":{"max_duration":-1}}`,
			wantErr: "max_duration must be greater than 0",
		},
		{
			name:    "system_prompt with prompt_file",
			json:    `{"coder":{"system_prompt":"hi","prompt_file":"p.md"}}`,
			wantErr: "system_prompt and prompt_file are mutually exclusive",
		},
		{
			name: "explicit empty system_prompt is a valid clear",
			json: `{"coder":{"system_prompt":""}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var profiles map[string]AgentProfilePatch
			require.NoError(t, json.Unmarshal([]byte(tt.json), &profiles))
			err := ValidateAgentProfiles(profiles)
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}

	t.Run("invalid model reference is classifiable without string matching", func(t *testing.T) {
		var profiles map[string]AgentProfilePatch
		require.NoError(t, json.Unmarshal([]byte(`{"coder":{"model":"nope"}}`), &profiles))
		err := ValidateAgentProfiles(profiles)
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrInvalidModelReference))
	})
}

func TestNormalizeAgentProfileKeys(t *testing.T) {
	t.Parallel()

	t.Run("folds ASCII case", func(t *testing.T) {
		got, err := NormalizeAgentProfileKeys(map[string]AgentProfilePatch{
			"Coder": {Disabled: Some(true)},
		})
		require.NoError(t, err)
		require.Contains(t, got, "coder")
		assert.Equal(t, Some(true), got["coder"].Disabled)
	})

	t.Run("case-fold collision is a duplicate", func(t *testing.T) {
		_, err := NormalizeAgentProfileKeys(map[string]AgentProfilePatch{
			"coder":  {},
			"CODER":  {},
			"review": {},
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "duplicate")
	})
}
