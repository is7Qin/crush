package prompt

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/filepathext"
	"github.com/charmbracelet/crush/internal/home"
	"github.com/charmbracelet/crush/internal/shell"
	"github.com/charmbracelet/crush/internal/skills"
)

// defaultGitTTL bounds how long git-derived prompt values are reused.
// Git status changes with every edit, but three shell execs per child
// dominate delegation cost, so a short TTL trades seconds of staleness
// for the common case of many children spawned in one run. Tests
// override it with WithGitTTL.
const defaultGitTTL = 30 * time.Second

// Prompt represents a template-based prompt generator.
type Prompt struct {
	name       string
	template   string
	now        func() time.Time
	platform   string
	workingDir string
	// contextPaths overrides Options.ContextPaths when set. The
	// child-agent path sets it from the resolved profile
	// (prof.Agent.ContextPaths); global context paths always apply
	// on top. Primary prompts leave it unset.
	contextPaths    []string
	contextPathsSet bool
	// gitTTL overrides defaultGitTTL. Zero when unset; WithGitTTL(0)
	// disables git caching and recomputes every build.
	gitTTL    time.Duration
	gitTTLSet bool
}

type PromptDat struct {
	Provider           string
	Model              string
	Config             config.Config
	WorkingDir         string
	IsGitRepo          bool
	Platform           string
	Date               string
	GitStatus          string
	ContextFiles       []ContextFile
	GlobalContextFiles []ContextFile
	AvailSkillXML      string
}

type ContextFile struct {
	Path    string
	Content string
}

type Option func(*Prompt)

func WithTimeFunc(fn func() time.Time) Option {
	return func(p *Prompt) {
		p.now = fn
	}
}

func WithPlatform(platform string) Option {
	return func(p *Prompt) {
		p.platform = platform
	}
}

func WithWorkingDir(workingDir string) Option {
	return func(p *Prompt) {
		p.workingDir = workingDir
	}
}

// WithContextPaths overrides the project context paths for this
// prompt. Merge rule: the override replaces Options.ContextPaths
// wholesale (an explicit empty list clears them), while
// Options.GlobalContextPaths always applies on top. Unset (the
// default) keeps the previous behavior of reading
// Options.ContextPaths, which is what primary prompts use.
func WithContextPaths(paths []string) Option {
	return func(p *Prompt) {
		p.contextPaths = slices.Clone(paths)
		p.contextPathsSet = true
	}
}

// WithGitTTL overrides how long git-derived values are reused from
// the assembly cache. The prompt's time func (see WithTimeFunc) is
// the clock, so tests control expiry without sleeping.
func WithGitTTL(ttl time.Duration) Option {
	return func(p *Prompt) {
		p.gitTTL = ttl
		p.gitTTLSet = true
	}
}

func NewPrompt(name, promptTemplate string, opts ...Option) (*Prompt, error) {
	p := &Prompt{
		name:     name,
		template: promptTemplate,
		now:      time.Now,
	}
	for _, opt := range opts {
		opt(p)
	}
	return p, nil
}

func (p *Prompt) Build(ctx context.Context, provider, model string, store *config.ConfigStore) (string, error) {
	t, err := template.New(p.name).Parse(p.template)
	if err != nil {
		return "", fmt.Errorf("parsing template: %w", err)
	}
	var sb strings.Builder
	d, err := p.promptData(ctx, provider, model, store)
	if err != nil {
		return "", err
	}
	if err := t.Execute(&sb, d); err != nil {
		return "", fmt.Errorf("executing template: %w", err)
	}

	return sb.String(), nil
}

func processFile(filePath string) *ContextFile {
	content, err := os.ReadFile(filePath)
	if err != nil {
		return nil
	}
	return &ContextFile{
		Path:    filePath,
		Content: string(content),
	}
}

func processContextPath(p string, store *config.ConfigStore) []ContextFile {
	var contexts []ContextFile
	fullPath := filepathext.SmartJoin(store.WorkingDir(), p)
	info, err := os.Stat(fullPath)
	if err != nil {
		return contexts
	}
	if info.IsDir() {
		filepath.WalkDir(fullPath, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() {
				if result := processFile(path); result != nil {
					contexts = append(contexts, *result)
				}
			}
			return nil
		})
	} else {
		result := processFile(fullPath)
		if result != nil {
			contexts = append(contexts, *result)
		}
	}
	return contexts
}

// expandPath expands ~ and environment variables in file paths
func expandPath(path string, store *config.ConfigStore) string {
	path = home.Long(path)
	// Handle environment variable expansion using the same pattern as config
	if strings.HasPrefix(path, "$") {
		if expanded, err := store.Resolver().ResolveValue(path); err == nil {
			path = expanded
		}
	}

	return path
}

// loadContextFiles loads and deduplicates context files from a list of paths.
func loadContextFiles(paths []string, store *config.ConfigStore) map[string][]ContextFile {
	files := map[string][]ContextFile{}
	for _, pth := range paths {
		expanded := expandPath(pth, store)
		pathKey := strings.ToLower(expanded)
		if _, ok := files[pathKey]; ok {
			continue
		}
		files[pathKey] = processContextPath(expanded, store)
	}
	return files
}

// fileSig identifies a file's content generation: path plus
// mtime and size. A missing path records size -1, so a file
// created later still invalidates the entry that missed it.
type fileSig struct {
	path    string
	size    int64
	modTime int64
}

// promptAssembly is the expensive, cacheable half of promptData:
// context file contents, skills XML, and git status. Entries are
// immutable once published; refreshes store a new pointer.
type promptAssembly struct {
	contextFiles []ContextFile
	ctxSigs      []fileSig
	globalFiles  []ContextFile
	globalSigs   []fileSig
	skillsXML    string
	skillSigs    []fileSig
	gitStatus    string
	gitAt        time.Time
	isGit        bool
}

// assemblyCache is the process-wide prompt assembly cache. The key
// covers template name, working directory, and the sorted
// context/global/skills/disabled lists, so concurrent children in
// one run share one entry. The config generation is deliberately
// not part of the key: a reload that leaves every input identical
// yields identical output, so reuse stays correct, while any
// content change trips the mtime+size validation below.
var assemblyCache = struct {
	sync.Mutex
	entries map[string]*promptAssembly
}{entries: make(map[string]*promptAssembly)}

// sortedJoin renders a path list deterministically for cache keys.
func sortedJoin(paths []string) string {
	return strings.Join(slices.Sorted(slices.Values(paths)), "\x1f")
}

// assemblyKey identifies one reusable assembly. Profile identity
// needs no extra component: the template name differs per profile
// and the profile's context paths are part of ctxPaths.
func assemblyKey(name, workingDir string, ctx, global, skillsPaths, disabled []string) string {
	return strings.Join([]string{
		name,
		workingDir,
		sortedJoin(ctx),
		sortedJoin(global),
		sortedJoin(skillsPaths),
		sortedJoin(disabled),
	}, "\x1e")
}

func sigsEqual(a, b []fileSig) bool {
	return slices.Equal(a, b)
}

// snapshotPaths stats every file reachable from paths without
// reading contents. Directory entries and the directories
// themselves are recorded, so added, removed, or edited files all
// change the snapshot.
func snapshotPaths(paths []string, store *config.ConfigStore) []fileSig {
	var sigs []fileSig
	for _, pth := range paths {
		fullPath := filepathext.SmartJoin(store.WorkingDir(), expandPath(pth, store))
		info, err := os.Stat(fullPath)
		if err != nil {
			sigs = append(sigs, fileSig{path: fullPath, size: -1})
			continue
		}
		sigs = append(sigs, fileSig{
			path:    fullPath,
			size:    info.Size(),
			modTime: info.ModTime().UnixNano(),
		})
		if !info.IsDir() {
			continue
		}
		_ = filepath.WalkDir(fullPath, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			if fi, err := d.Info(); err == nil {
				sigs = append(sigs, fileSig{
					path:    path,
					size:    fi.Size(),
					modTime: fi.ModTime().UnixNano(),
				})
			}
			return nil
		})
	}
	slices.SortFunc(sigs, func(a, b fileSig) int { return strings.Compare(a.path, b.path) })
	return sigs
}

// snapshotSkills stats every SKILL.md under the given (already
// expanded) skills directories without parsing any of them.
// Symlinked files and directories are followed via Stat, so edits
// through links still invalidate. Missing bases record a marker,
// so a directory created later invalidates too.
func snapshotSkills(paths []string) []fileSig {
	var sigs []fileSig
	seen := map[string]bool{}
	stack := slices.Clone(paths)
	for len(stack) > 0 {
		dir := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		resolved, err := filepath.EvalSymlinks(dir)
		if err != nil {
			sigs = append(sigs, fileSig{path: dir, size: -1})
			continue
		}
		if seen[resolved] {
			continue
		}
		seen[resolved] = true
		entries, err := os.ReadDir(dir)
		if err != nil {
			sigs = append(sigs, fileSig{path: dir, size: -1})
			continue
		}
		for _, e := range entries {
			full := filepath.Join(dir, e.Name())
			info, err := os.Stat(full)
			if err != nil {
				continue
			}
			if info.IsDir() {
				stack = append(stack, full)
				continue
			}
			if e.Name() == skills.SkillFileName {
				sigs = append(sigs, fileSig{
					path:    full,
					size:    info.Size(),
					modTime: info.ModTime().UnixNano(),
				})
			}
		}
	}
	slices.SortFunc(sigs, func(a, b fileSig) int { return strings.Compare(a.path, b.path) })
	return sigs
}

func cachedAssembly(key string) *promptAssembly {
	assemblyCache.Lock()
	defer assemblyCache.Unlock()
	return assemblyCache.entries[key]
}

func storeAssembly(key string, asm *promptAssembly) {
	assemblyCache.Lock()
	defer assemblyCache.Unlock()
	assemblyCache.entries[key] = asm
}

// discoverSkillsXML runs the full skills pipeline: builtins, user
// discovery, dedup, disabled filtering, and XML rendering.
func discoverSkillsXML(cfg *config.Config, expandedSkillsPaths []string) string {
	// Start with builtin skills.
	allSkills := skills.DiscoverBuiltin()
	builtinNames := make(map[string]bool, len(allSkills))
	for _, s := range allSkills {
		builtinNames[s.Name] = true
	}

	// Discover user skills from configured paths.
	if len(expandedSkillsPaths) > 0 {
		for _, userSkill := range skills.Discover(expandedSkillsPaths) {
			if builtinNames[userSkill.Name] {
				slog.Warn("User skill overrides builtin skill", "name", userSkill.Name)
			}
			allSkills = append(allSkills, userSkill)
		}
	}

	// Deduplicate: user skills override builtins with the same name.
	allSkills = skills.Deduplicate(allSkills)

	// Filter out disabled skills.
	allSkills = skills.Filter(allSkills, cfg.Options.DisabledSkills)

	if len(allSkills) > 0 {
		return skills.ToPromptXML(allSkills)
	}
	return ""
}

// flattenContextFiles preserves loadContextFiles' existing order.
func flattenContextFiles(grouped map[string][]ContextFile) []ContextFile {
	var out []ContextFile
	for _, files := range grouped {
		out = append(out, files...)
	}
	return out
}

func (p *Prompt) effectiveGitTTL() time.Duration {
	if p.gitTTLSet {
		return p.gitTTL
	}
	return defaultGitTTL
}

func (p *Prompt) promptData(ctx context.Context, provider, model string, store *config.ConfigStore) (PromptDat, error) {
	workingDir := cmp.Or(p.workingDir, store.WorkingDir())
	platform := cmp.Or(p.platform, runtime.GOOS)

	cfg := store.Config()
	// See WithContextPaths for the merge rule: the profile
	// override replaces the project list, globals always apply.
	ctxPaths := cfg.Options.ContextPaths
	if p.contextPathsSet {
		ctxPaths = p.contextPaths
	}
	globalPaths := cfg.Options.GlobalContextPaths
	expandedSkillsPaths := make([]string, 0, len(cfg.Options.SkillsPaths))
	for _, pth := range cfg.Options.SkillsPaths {
		expandedSkillsPaths = append(expandedSkillsPaths, expandPath(pth, store))
	}

	key := assemblyKey(p.name, workingDir, ctxPaths, globalPaths, expandedSkillsPaths, cfg.Options.DisabledSkills)
	now := p.now()
	isGit := isGitRepo(store.WorkingDir())

	// Stat-only snapshots: no file contents, no shell, no parsing.
	ctxSigs := snapshotPaths(ctxPaths, store)
	globalSigs := snapshotPaths(globalPaths, store)
	skillSigs := snapshotSkills(expandedSkillsPaths)

	asm := cachedAssembly(key)
	if asm == nil || !sigsEqual(asm.ctxSigs, ctxSigs) ||
		!sigsEqual(asm.globalSigs, globalSigs) ||
		!sigsEqual(asm.skillSigs, skillSigs) ||
		asm.isGit != isGit {
		// Cache miss: pay for reads, discovery, and git once.
		contextFiles := flattenContextFiles(loadContextFiles(ctxPaths, store))
		globalFiles := flattenContextFiles(loadContextFiles(globalPaths, store))
		skillsXML := discoverSkillsXML(cfg, expandedSkillsPaths)
		var gitStatus string
		if isGit {
			var err error
			gitStatus, err = getGitStatus(ctx, store.WorkingDir())
			if err != nil {
				return PromptDat{}, err
			}
		}
		asm = &promptAssembly{
			contextFiles: contextFiles,
			ctxSigs:      ctxSigs,
			globalFiles:  globalFiles,
			globalSigs:   globalSigs,
			skillsXML:    skillsXML,
			skillSigs:    skillSigs,
			gitStatus:    gitStatus,
			gitAt:        now,
			isGit:        isGit,
		}
		storeAssembly(key, asm)
	} else if isGit && now.Sub(asm.gitAt) >= p.effectiveGitTTL() {
		// Files and skills are fresh; only git went stale.
		// Publish a new entry rather than mutating the shared one.
		gitStatus, err := getGitStatus(ctx, store.WorkingDir())
		if err != nil {
			return PromptDat{}, err
		}
		refreshed := *asm
		refreshed.gitStatus = gitStatus
		refreshed.gitAt = now
		storeAssembly(key, &refreshed)
		asm = &refreshed
	}

	data := PromptDat{
		Provider:           provider,
		Model:              model,
		Config:             *cfg,
		WorkingDir:         filepath.ToSlash(workingDir),
		IsGitRepo:          isGit,
		Platform:           platform,
		Date:               p.now().Format("1/2/2006"),
		GitStatus:          asm.gitStatus,
		ContextFiles:       asm.contextFiles,
		GlobalContextFiles: asm.globalFiles,
		AvailSkillXML:      asm.skillsXML,
	}
	return data, nil
}

func isGitRepo(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}

func getGitStatus(ctx context.Context, dir string) (string, error) {
	sh := shell.NewShell(&shell.Options{
		WorkingDir: dir,
	})
	branch, err := getGitBranch(ctx, sh)
	if err != nil {
		return "", err
	}
	status, err := getGitStatusSummary(ctx, sh)
	if err != nil {
		return "", err
	}
	commits, err := getGitRecentCommits(ctx, sh)
	if err != nil {
		return "", err
	}
	return branch + status + commits, nil
}

func getGitBranch(ctx context.Context, sh *shell.Shell) (string, error) {
	out, _, err := sh.Exec(ctx, "git branch --show-current 2>/dev/null")
	if err != nil {
		return "", nil
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return "", nil
	}
	return fmt.Sprintf("Current branch: %s\n", out), nil
}

func getGitStatusSummary(ctx context.Context, sh *shell.Shell) (string, error) {
	out, _, err := sh.Exec(ctx, "git status --short 2>/dev/null | head -20")
	if err != nil {
		return "", nil
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return "Status: clean\n", nil
	}
	return fmt.Sprintf("Status:\n%s\n", out), nil
}

func getGitRecentCommits(ctx context.Context, sh *shell.Shell) (string, error) {
	out, _, err := sh.Exec(ctx, "git log --oneline -n 3 2>/dev/null")
	if err != nil || out == "" {
		return "", nil
	}
	out = strings.TrimSpace(out)
	return fmt.Sprintf("Recent commits:\n%s\n", out), nil
}

func (p *Prompt) Name() string {
	return p.name
}
