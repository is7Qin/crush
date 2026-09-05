package config

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"
)

// agenticFetchToolName duplicates tools.AgenticFetchToolName because
// internal/config must not import internal/agent/tools. See the note on
// DelegationToolName.
const agenticFetchToolName = "agentic_fetch"

// childDeniedTools are removed from every child's tool policy: children
// never delegate or spawn nested fetch agents, regardless of which profile
// was selected.
var childDeniedTools = []string{DelegationToolName, agenticFetchToolName}

// Profile selection errors returned by ResolveAgentProfile. Callers
// classify with errors.Is, never with string matching.
var (
	ErrUnknownAgentProfile  = errors.New("unknown agent profile")
	ErrAgentProfileDisabled = errors.New("agent profile disabled")
)

// ResolvedProfile is the runtime view of a call_agent profile selection:
// the child's projected tool policy plus the model and prompt overrides
// the coordinator consumes when constructing the child SessionAgent. The
// policy fields carry concrete effective values: omitted booleans default
// to allowed, and zero max_steps/max_duration mean unlimited.
type ResolvedProfile struct {
	Name  string
	Agent Agent
	// Model is the profile's exact model selection, valid only when
	// ModelSet is true; otherwise the child uses the global large model.
	Model    SelectedModel
	ModelSet bool
	// ReasoningEffort is the profile's reasoning strength applied to the
	// child's large model when Present. It overrides the selected model's
	// own effort; call-time validation against the model's reasoning
	// levels still governs what reaches the provider.
	ReasoningEffort Optional[string]
	// SystemPrompt and PromptFile carry the profile's prompt override;
	// both empty means the built-in template for the profile.
	SystemPrompt string
	PromptFile   string
	// CanDelegate and CanAskQuestions are the validated effective
	// policies. The tool-set effect is already projected into
	// Agent.AllowedTools; the booleans let the runtime distinguish an
	// explicit denial from a tool that was never in the palette.
	CanDelegate     bool
	CanAskQuestions bool
	// MaxSteps and MaxDuration bound the child run; zero means unlimited.
	MaxSteps    int
	MaxDuration time.Duration
	// Generation is the config snapshot this profile was resolved from.
	// It increments once per committed config reload, so a task records
	// which profile generation its policy came from.
	Generation uint64
}

// applyProfilePolicy copies the effective lifecycle and capability policy
// from a merged patch onto a resolved profile. Absent fields keep their
// permissive defaults.
func applyProfilePolicy(prof *ResolvedProfile, patch AgentProfilePatch) {
	prof.CanDelegate = !patch.CanDelegate.Present || patch.CanDelegate.Value
	prof.CanAskQuestions = !patch.CanAskQuestions.Present || patch.CanAskQuestions.Value
	if patch.MaxSteps.Present {
		prof.MaxSteps = patch.MaxSteps.Value
	}
	if patch.MaxDuration.Present {
		prof.MaxDuration = patch.MaxDuration.Value
	}
	if patch.ReasoningEffort.Present {
		prof.ReasoningEffort = patch.ReasoningEffort
	}
}

// ResolveAgentProfile projects the named profile onto a child runtime
// agent. The child-only recursion gate (the delegation pair) is applied.
// A disabled profile is rejected with ErrAgentProfileDisabled; disabled
// profiles are also omitted from DiscoverableAgentProfiles.
func (c *Config) ResolveAgentProfile(name string) (ResolvedProfile, error) {
	return c.resolveAgentProfile(name, true)
}

// ResolvePrimaryAgentProfile projects the named profile onto a primary
// runtime agent. Tool policy is role-level, not profile-level: every
// selected primary exposes the same complete built-in palette as coder
// (subject only to options.disabled_tools and the coordinator's runtime
// tool availability), so delegation, task control, and ordinary coding
// tools are never gated by a child-role restriction such as a research
// profile's read-only allow-list. The profile's own AllowedTools,
// denied_tools, can_delegate, and allowed_mcp policy applies only to its
// child projection through ResolveAgentProfile; its model, prompt, and
// lifecycle overrides are kept here. A disabled profile is rejected as
// well.
func (c *Config) ResolvePrimaryAgentProfile(name string) (ResolvedProfile, error) {
	prof, err := c.resolveAgentProfile(name, false)
	if err != nil {
		return ResolvedProfile{}, err
	}
	var disabled []string
	if c.Options != nil {
		disabled = c.Options.DisabledTools
	}
	// Fresh slices from allToolNames/filterSlice: never alias the
	// runtime Agents map's coder entry, whose slice the child
	// projections share.
	prof.Agent.AllowedTools = resolveAllowedTools(allToolNames(), disabled)
	prof.Agent.AllowedMCP = nil
	return prof, nil
}

func (c *Config) resolveAgentProfile(name string, child bool) (ResolvedProfile, error) {
	key := asciiLower(name)
	if key == AgenticFetchInternalProfile {
		// The reserved internal profile is not a public selection.
		// Report it as unknown so call_agent can never admit a task
		// under the hidden marker from model input.
		return ResolvedProfile{}, fmt.Errorf("%w: %s", ErrUnknownAgentProfile, key)
	}
	if agent, ok := c.Agents[key]; ok {
		if agent.Disabled {
			return ResolvedProfile{}, fmt.Errorf("%w: %s", ErrAgentProfileDisabled, key)
		}
		if child {
			agent.AllowedTools = filterSlice(agent.AllowedTools, childDeniedTools, false)
		}
		prof := ResolvedProfile{Name: key, Agent: agent, Generation: c.ProfileGeneration}
		applyProfilePolicy(&prof, c.AgentProfiles[key])
		return prof, nil
	}

	patch, patched := c.AgentProfiles[key]
	merged, builtin := effectiveProfilePatch(key, patch, patched)
	if !patched && !builtin {
		return ResolvedProfile{}, fmt.Errorf("%w: %s", ErrUnknownAgentProfile, key)
	}
	if merged.Disabled.Present && merged.Disabled.Value {
		return ResolvedProfile{}, fmt.Errorf("%w: %s", ErrAgentProfileDisabled, key)
	}

	base := slices.Clone(c.Agents[AgentCoder].AllowedTools)
	if child {
		base = filterSlice(base, childDeniedTools, false)
	}
	// Options may be nil on a bare or client-synced snapshot; a missing
	// options block carries no context paths rather than crashing the
	// caller that is projecting the profile for display.
	var contextPaths []string
	if c.Options != nil {
		contextPaths = c.Options.ContextPaths
	}
	agent := applyProfilePatch(Agent{
		ID:           key,
		Name:         key,
		Model:        SelectedModelTypeLarge,
		ContextPaths: contextPaths,
		AllowedTools: base,
	}, base, merged)

	prof := ResolvedProfile{Name: key, Agent: agent, Generation: c.ProfileGeneration}
	applyProfilePolicy(&prof, merged)
	switch {
	case merged.Model.Present:
		model, err := c.ResolveModelRef(merged.Model.Value)
		if err != nil {
			return ResolvedProfile{}, fmt.Errorf("agent profile %q: %w", key, err)
		}
		prof.Model, prof.ModelSet = model, true
	case merged.Models.Present:
		model, err := c.FirstAvailableModelRef(merged.Models.Value)
		if err != nil {
			return ResolvedProfile{}, fmt.Errorf("agent profile %q: %w", key, err)
		}
		prof.Model, prof.ModelSet = model, true
	}
	if merged.SystemPrompt.Present {
		prof.SystemPrompt = merged.SystemPrompt.Value
	}
	if merged.PromptFile.Present {
		prof.PromptFile = merged.PromptFile.Value
	}
	return prof, nil
}

// effectiveProfilePatch layers a user patch over the built-in catalog
// entry for key, returning the merged patch and whether the catalog owns
// the name. Precedence is field-level: a present user field wins, an
// omitted user field inherits the built-in default. An explicit user
// prompt_file drops the built-in inline prompt, which would otherwise
// shadow it in the coordinator's prompt routing.
func effectiveProfilePatch(key string, user AgentProfilePatch, patched bool) (AgentProfilePatch, bool) {
	builtin, ok := builtinAgentProfiles()[key]
	if !ok {
		return user, false
	}
	if !patched {
		return builtin, true
	}
	merged := builtin.mergeOnto(user)
	if user.PromptFile.Present {
		merged.SystemPrompt = Optional[string]{}
	}
	return merged, true
}

// DiscoverableAgentProfiles lists the profile names selectable right now:
// the two Crush base agents (coder, task) and the OMO-native roster,
// merged with the user-configured profile keys. A profile whose effective
// disabled policy is true is omitted, so disabling coder or task through
// the agents config removes it from discovery exactly like a disabled
// roster or custom profile. Order is deterministic: built-in names in
// declaration order, then remaining user profile keys sorted. The
// reserved internal profile is never listed.
func (c *Config) DiscoverableAgentProfiles() []string {
	listed := map[string]bool{}
	names := make([]string, 0, len(BuiltinAgentProfileNames())+len(c.AgentProfiles))
	add := func(key string) {
		key = asciiLower(key)
		if listed[key] || key == AgenticFetchInternalProfile || c.profileDisabled(key) {
			return
		}
		listed[key] = true
		names = append(names, key)
	}
	for _, name := range BuiltinAgentProfileNames() {
		add(name)
	}
	for _, key := range slices.Sorted(maps.Keys(c.AgentProfiles)) {
		add(key)
	}
	return names
}

// profileDisabled reports the effective disabled policy of a candidate
// profile name: coder/task carry it on their derived Agents entry (applied
// by SetupAgents), roster and custom names on the merged patch.
func (c *Config) profileDisabled(key string) bool {
	if agent, ok := c.Agents[key]; ok {
		return agent.Disabled
	}
	patch, patched := c.AgentProfiles[key]
	merged, _ := effectiveProfilePatch(key, patch, patched)
	return merged.Disabled.Present && merged.Disabled.Value
}
