package config

import (
	"errors"
	"strings"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/charmbracelet/crush/internal/csync"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseModelRef(t *testing.T) {
	t.Parallel()

	tests := []struct {
		ref      string
		provider string
		model    string
		wantErr  bool
	}{
		{ref: "openai/gpt-4o", provider: "openai", model: "gpt-4o"},
		{ref: "ollama/qwen3:30b", provider: "ollama", model: "qwen3:30b"},
		{ref: "vertexai/claude-sonnet-4@20250514", provider: "vertexai", model: "claude-sonnet-4@20250514"},
		{ref: "openrouter/openai/gpt-4o", provider: "openrouter", model: "openai/gpt-4o"},
		{ref: "gpt-4o", wantErr: true},
		{ref: "/gpt-4o", wantErr: true},
		{ref: "openai/", wantErr: true},
		{ref: "", wantErr: true},
	}

	for _, tt := range tests {
		got, err := ParseModelRef(tt.ref)
		if tt.wantErr {
			require.Error(t, err, "ref %q", tt.ref)
			assert.True(t, errors.Is(err, ErrInvalidModelReference), "ref %q", tt.ref)
			continue
		}
		require.NoError(t, err, "ref %q", tt.ref)
		assert.Equal(t, SelectedModel{Provider: tt.provider, Model: tt.model}, got, "ref %q", tt.ref)
	}
}

// catalogConfig builds a Config whose provider catalog contains the given
// provider IDs, each with the listed model IDs. A provider name prefixed
// with "!" is disabled.
func catalogConfig(t *testing.T, entries map[string][]string) *Config {
	t.Helper()
	providers := map[string]ProviderConfig{}
	for id, models := range entries {
		disabled := strings.HasPrefix(id, "!")
		id = strings.TrimPrefix(id, "!")
		ms := make([]catwalk.Model, 0, len(models))
		for _, m := range models {
			ms = append(ms, catwalk.Model{ID: m})
		}
		providers[id] = ProviderConfig{ID: id, Disable: disabled, Models: ms}
	}
	return &Config{Providers: csync.NewMapFrom(providers)}
}

func TestResolveModelRef(t *testing.T) {
	t.Parallel()

	cfg := catalogConfig(t, map[string][]string{
		"openai":  {"gpt-4o"},
		"!closed": {"ghost-model"},
	})

	t.Run("available model resolves", func(t *testing.T) {
		got, err := cfg.ResolveModelRef("openai/gpt-4o")
		require.NoError(t, err)
		assert.Equal(t, SelectedModel{Provider: "openai", Model: "gpt-4o"}, got)
	})

	t.Run("unknown model fails with unavailable class", func(t *testing.T) {
		_, err := cfg.ResolveModelRef("openai/missing")
		assert.True(t, errors.Is(err, ErrUnavailableModel))
	})

	t.Run("model on disabled provider fails with unavailable class", func(t *testing.T) {
		_, err := cfg.ResolveModelRef("closed/ghost-model")
		assert.True(t, errors.Is(err, ErrUnavailableModel))
	})

	t.Run("malformed reference fails with invalid class", func(t *testing.T) {
		_, err := cfg.ResolveModelRef("gpt-4o")
		assert.True(t, errors.Is(err, ErrInvalidModelReference))
	})
}

func TestFirstAvailableModelRef(t *testing.T) {
	t.Parallel()

	cfg := catalogConfig(t, map[string][]string{
		"openai":    {"gpt-4o"},
		"anthropic": {"claude-sonnet-4"},
	})

	t.Run("first available entry wins, unavailable ones are skipped", func(t *testing.T) {
		got, err := cfg.FirstAvailableModelRef([]string{
			"anthropic/missing-model",
			"openai/gpt-4o",
			"anthropic/claude-sonnet-4",
		})
		require.NoError(t, err)
		assert.Equal(t, SelectedModel{Provider: "openai", Model: "gpt-4o"}, got)
	})

	t.Run("exhausted list fails with unavailable class", func(t *testing.T) {
		_, err := cfg.FirstAvailableModelRef([]string{"openai/nope", "anthropic/nope"})
		assert.True(t, errors.Is(err, ErrUnavailableModel))
	})

	t.Run("invalid entry fails immediately", func(t *testing.T) {
		_, err := cfg.FirstAvailableModelRef([]string{"openai/nope", "malformed"})
		assert.True(t, errors.Is(err, ErrInvalidModelReference))
	})
}
