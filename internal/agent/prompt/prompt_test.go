package prompt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/stretchr/testify/require"
)

// TestDefaultedConfig_IncludesOMOMemoryIndex pins the OMO memory
// wiring: a defaulted config must carry the memory index in its
// project context paths, so a project using oh-my-openagent gets its
// durable learnings offered to the agent without extra configuration.
func TestDefaultedConfig_IncludesOMOMemoryIndex(t *testing.T) {
	t.Parallel()

	store, err := config.Init(t.TempDir(), "", false)
	require.NoError(t, err)

	require.Contains(t, store.Config().Options.ContextPaths, ".omo/memory/MEMORY.md",
		"the OMO memory index must be part of the default context paths")
}

// TestLoadContextFiles_ReadsOMOMemoryIndex proves the reader half, and
// pins the deliberate "index only" choice: the index is injected, but
// the memory bodies it links to are not, so the agent reads a specific
// memory on demand rather than carrying the whole directory.
func TestLoadContextFiles_ReadsOMOMemoryIndex(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	memoryDir := filepath.Join(workingDir, ".omo", "memory")
	require.NoError(t, os.MkdirAll(filepath.Join(memoryDir, "topics"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(memoryDir, "MEMORY.md"),
		[]byte("# index\n\n- [use pnpm](use-pnpm-not-npm.md) — deps via pnpm only\n"),
		0o644))
	require.NoError(t, os.WriteFile(
		filepath.Join(memoryDir, "use-pnpm-not-npm.md"),
		[]byte("ALWAYS pnpm, never npm."),
		0o644))

	store, err := config.Init(workingDir, "", false)
	require.NoError(t, err)

	files := loadContextFiles([]string{".omo/memory/MEMORY.md"}, store)

	var got []string
	for _, group := range files {
		for _, f := range group {
			got = append(got, f.Content)
		}
	}
	require.Len(t, got, 1, "exactly the index must be injected")
	require.Contains(t, got[0], "use pnpm")
	require.NotContains(t, strings.Join(got, "\n"), "ALWAYS pnpm, never npm.",
		"memory bodies must stay out of the prompt; the agent reads them on demand")
}

// TestLoadContextFiles_MissingOMOIndexIsNoOp proves a project without
// OMO pays nothing: the default path simply does not resolve.
func TestLoadContextFiles_MissingOMOIndexIsNoOp(t *testing.T) {
	t.Parallel()

	store, err := config.Init(t.TempDir(), "", false)
	require.NoError(t, err)

	files := loadContextFiles([]string{".omo/memory/MEMORY.md"}, store)
	for path, group := range files {
		require.Empty(t, group, "a missing memory index must load no files: %s", path)
	}
}
