package config_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestProfileGeneration_ReloadIncrementsOncePerCommit pins the profile
// snapshot generation contract: a fresh load starts at 1, every committed
// reload bumps it exactly once, a failed reload leaves it untouched, and
// ResolvedProfile carries the generation of the snapshot it came from so
// an already-resolved profile keeps its old generation across reloads.
func TestProfileGeneration_ReloadIncrementsOncePerCommit(t *testing.T) {
	workDir, dataDir := isolateReloadEnv(t)
	rcPath := filepath.Join(workDir, "crushrc")
	// A configured provider is required so Load runs past its early
	// "not configured" return and reaches SetupAgents, which builds the
	// built-in agent entries ResolveAgentProfile reads.
	require.NoError(t, os.WriteFile(rcPath, []byte("provider add openai --api-key k\n"), 0o644))

	store, err := config.Load(workDir, dataDir, false)
	require.NoError(t, err)
	require.Equal(t, uint64(1), store.Config().ProfileGeneration,
		"a fresh load is generation 1")

	prof1, err := store.Config().ResolveAgentProfile(config.AgentCoder)
	require.NoError(t, err)
	require.Equal(t, uint64(1), prof1.Generation)

	require.NoError(t, os.WriteFile(rcPath, []byte("provider add openai --api-key k\noption debug true\n"), 0o644))
	require.NoError(t, store.ReloadFromDisk(context.Background()))
	require.Equal(t, uint64(2), store.Config().ProfileGeneration)

	prof2, err := store.Config().ResolveAgentProfile(config.AgentCoder)
	require.NoError(t, err)
	assert.Equal(t, uint64(2), prof2.Generation,
		"a profile resolved after the reload uses the new generation")
	assert.Equal(t, uint64(1), prof1.Generation,
		"a profile resolved before the reload keeps its captured generation")

	// A reload that fails validation must not bump the generation: the
	// swap (and with it the bump) only happens on a committed reload.
	require.NoError(t, os.WriteFile(rcPath, []byte("option totally-bogus-key value\n"), 0o644))
	require.Error(t, store.ReloadFromDisk(context.Background()))
	require.Equal(t, uint64(2), store.Config().ProfileGeneration,
		"a failed reload must not advance the generation")
}
