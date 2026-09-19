package prompt

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/stretchr/testify/require"
)

// cacheTestTemplate renders only the cached inputs, so assertions
// stay independent of the coder/task template text.
const cacheTestTemplate = `ctx:{{range .ContextFiles}}[{{.Content}}]{{end}}|` +
	`global:{{range .GlobalContextFiles}}[{{.Content}}]{{end}}|` +
	`git:{{.GitStatus}}`

// cacheTestStore returns a hermetic store whose context, global,
// and skills inputs are fully controlled by the test.
func cacheTestStore(t *testing.T, workingDir string) *config.ConfigStore {
	t.Helper()
	store, err := config.Init(workingDir, "", false)
	require.NoError(t, err)
	store.Config().Options.ContextPaths = nil
	store.Config().Options.GlobalContextPaths = nil
	store.Config().Options.SkillsPaths = nil
	store.Config().Options.DisabledSkills = nil
	return store
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}

// TestBuild_ProfileContextPathsOverride pins the merge rule: a
// profile override replaces Options.ContextPaths wholesale while
// Options.GlobalContextPaths still applies.
func TestBuild_ProfileContextPathsOverride(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	writeTestFile(t, filepath.Join(workingDir, "PROFILE.md"), "profile-marker")
	writeTestFile(t, filepath.Join(workingDir, "GLOBAL.md"), "global-marker")
	writeTestFile(t, filepath.Join(workingDir, "IGNORED.md"), "ignored-marker")

	store := cacheTestStore(t, workingDir)
	store.Config().Options.ContextPaths = []string{"IGNORED.md"}
	store.Config().Options.GlobalContextPaths = []string{"GLOBAL.md"}

	ctx := context.Background()

	child, err := NewPrompt("cache-child", cacheTestTemplate,
		WithWorkingDir(workingDir),
		WithContextPaths([]string{"PROFILE.md"}))
	require.NoError(t, err)
	out, err := child.Build(ctx, "p", "m", store)
	require.NoError(t, err)
	require.Contains(t, out, "profile-marker")
	require.Contains(t, out, "global-marker")
	require.NotContains(t, out, "ignored-marker",
		"the profile override replaces the project list")

	// An explicit empty override clears the project list; globals
	// still apply.
	empty, err := NewPrompt("cache-empty", cacheTestTemplate,
		WithWorkingDir(workingDir),
		WithContextPaths([]string{}))
	require.NoError(t, err)
	out, err = empty.Build(ctx, "p", "m", store)
	require.NoError(t, err)
	require.Contains(t, out, "global-marker")
	require.NotContains(t, out, "ignored-marker")

	// Without the override the global options list is used.
	plain, err := NewPrompt("cache-plain", cacheTestTemplate,
		WithWorkingDir(workingDir))
	require.NoError(t, err)
	out, err = plain.Build(ctx, "p", "m", store)
	require.NoError(t, err)
	require.Contains(t, out, "ignored-marker")
	require.NotContains(t, out, "profile-marker")
}

// TestBuild_AssemblyCacheConsistentAndRefreshes pins both halves of
// file invalidation: identical inputs give byte-identical output,
// and an edit (mtime+size) refreshes it.
func TestBuild_AssemblyCacheConsistentAndRefreshes(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	ctxPath := filepath.Join(workingDir, "NOTES.md")
	writeTestFile(t, ctxPath, "version-one")

	store := cacheTestStore(t, workingDir)
	store.Config().Options.ContextPaths = []string{"NOTES.md"}

	ctx := context.Background()
	p, err := NewPrompt("cache-refresh", cacheTestTemplate,
		WithWorkingDir(workingDir))
	require.NoError(t, err)

	first, err := p.Build(ctx, "p", "m", store)
	require.NoError(t, err)
	second, err := p.Build(ctx, "p", "m", store)
	require.NoError(t, err)
	require.Equal(t, first, second, "repeated builds must be consistent")
	require.Contains(t, first, "version-one")

	// Rewrite with a guaranteed mtime bump so even filesystems
	// with coarse granularity observe the change.
	writeTestFile(t, ctxPath, "version-two-much-longer")
	future := time.Now().Add(2 * time.Second)
	require.NoError(t, os.Chtimes(ctxPath, future, future))

	third, err := p.Build(ctx, "p", "m", store)
	require.NoError(t, err)
	require.Contains(t, third, "version-two-much-longer")
	require.NotContains(t, third, "version-one")

	fourth, err := p.Build(ctx, "p", "m", store)
	require.NoError(t, err)
	require.Equal(t, third, fourth, "post-refresh builds are consistent again")
}

// TestBuild_GitStatusHonorsTTL pins git invalidation without a
// flaky timing test: the prompt's own clock (WithTimeFunc) drives
// expiry, and entry identity proves reuse versus recompute.
func TestBuild_GitStatusHonorsTTL(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(workingDir, ".git"), 0o755))

	store := cacheTestStore(t, workingDir)

	now := time.Now()
	current := now
	p, err := NewPrompt("cache-git-ttl", cacheTestTemplate,
		WithWorkingDir(workingDir),
		WithTimeFunc(func() time.Time { return current }),
		WithGitTTL(time.Minute))
	require.NoError(t, err)

	ctx := context.Background()
	_, err = p.Build(ctx, "p", "m", store)
	require.NoError(t, err)
	key := assemblyKey("cache-git-ttl", workingDir, nil, nil, []string{}, nil)
	first := cachedAssembly(key)
	require.NotNil(t, first)
	require.True(t, first.isGit)
	require.Equal(t, now, first.gitAt)

	// Within the TTL the cached entry (and its git value) is reused.
	current = now.Add(30 * time.Second)
	secondOut, err := p.Build(ctx, "p", "m", store)
	require.NoError(t, err)
	require.Same(t, first, cachedAssembly(key), "git must be reused within the TTL")

	// Past the TTL the entry is refreshed with the new timestamp.
	current = now.Add(2 * time.Minute)
	thirdOut, err := p.Build(ctx, "p", "m", store)
	require.NoError(t, err)
	refreshed := cachedAssembly(key)
	require.NotSame(t, first, refreshed, "git must refresh past the TTL")
	require.Equal(t, current, refreshed.gitAt)
	require.Equal(t, secondOut, thirdOut, "git content is unchanged, only its timestamp moved")
}

// TestBuild_ConcurrentBuilds proves the process-wide cache is safe
// when many children build at once: every build succeeds and all
// outputs agree.
func TestBuild_ConcurrentBuilds(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	writeTestFile(t, filepath.Join(workingDir, "SHARED.md"), "shared-content")

	store := cacheTestStore(t, workingDir)
	store.Config().Options.ContextPaths = []string{"SHARED.md"}

	p, err := NewPrompt("cache-race", cacheTestTemplate,
		WithWorkingDir(workingDir))
	require.NoError(t, err)

	const builders = 16
	outs := make([]string, builders)
	var wg sync.WaitGroup
	errs := make([]error, builders)
	for i := range builders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			outs[i], errs[i] = p.Build(context.Background(), "p", "m", store)
		}()
	}
	wg.Wait()
	for i := range builders {
		require.NoError(t, errs[i])
		require.Equal(t, outs[0], outs[i], "concurrent builds must agree")
	}
	require.Contains(t, outs[0], "shared-content")
}

// TestBuild_DistinctProfilesDoNotShareEntries pins key isolation:
// two profiles over the same directory with different context
// paths must not serve each other's content.
func TestBuild_DistinctProfilesDoNotShareEntries(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	writeTestFile(t, filepath.Join(workingDir, "AAA.md"), "aaa-content")
	writeTestFile(t, filepath.Join(workingDir, "BBB.md"), "bbb-content")

	store := cacheTestStore(t, workingDir)
	ctx := context.Background()

	for i, tc := range []struct{ file, want, avoid string }{
		{"AAA.md", "aaa-content", "bbb-content"},
		{"BBB.md", "bbb-content", "aaa-content"},
	} {
		p, err := NewPrompt(fmt.Sprintf("cache-isolation-%d", i), cacheTestTemplate,
			WithWorkingDir(workingDir),
			WithContextPaths([]string{tc.file}))
		require.NoError(t, err)
		out, err := p.Build(ctx, "p", "m", store)
		require.NoError(t, err)
		require.Contains(t, out, tc.want)
		require.NotContains(t, out, tc.avoid)
	}
}
