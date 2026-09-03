package config

import (
	"errors"
	"fmt"
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
}

// ResolveAgentProfile projects the named profile onto a runtime agent for
// a delegation. Built-in coder/task names resolve to their derived Agents
// entries; custom names resolve against the AgentProfiles snapshot with
// full ordinary capabilities (the coder policy minus the delegation pair)
// as the default tool policy. The delegation pair is stripped from every
// result, so a child can never re-add it via its allow-list.
func (c *Config) ResolveAgentProfile(name string) (ResolvedProfile, error) {
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
		agent.AllowedTools = filterSlice(agent.AllowedTools, childDeniedTools, false)
		prof := ResolvedProfile{Name: key, Agent: agent, Generation: c.ProfileGeneration}
		applyProfilePolicy(&prof, c.AgentProfiles[key])
		return prof, nil
	}

	patch, ok := c.AgentProfiles[key]
	if !ok {
		return ResolvedProfile{}, fmt.Errorf("%w: %s", ErrUnknownAgentProfile, key)
	}
	if patch.Disabled.Present && patch.Disabled.Value {
		return ResolvedProfile{}, fmt.Errorf("%w: %s", ErrAgentProfileDisabled, key)
	}

	base := slices.Clone(c.Agents[AgentCoder].AllowedTools)
	base = filterSlice(base, childDeniedTools, false)
	agent := applyProfilePatch(Agent{
		ID:           key,
		Name:         key,
		Model:        SelectedModelTypeLarge,
		ContextPaths: c.Options.ContextPaths,
		AllowedTools: base,
	}, base, patch)

	prof := ResolvedProfile{Name: key, Agent: agent, Generation: c.ProfileGeneration}
	applyProfilePolicy(&prof, patch)
	switch {
	case patch.Model.Present:
		model, err := c.ResolveModelRef(patch.Model.Value)
		if err != nil {
			return ResolvedProfile{}, fmt.Errorf("agent profile %q: %w", key, err)
		}
		prof.Model, prof.ModelSet = model, true
	case patch.Models.Present:
		model, err := c.FirstAvailableModelRef(patch.Models.Value)
		if err != nil {
			return ResolvedProfile{}, fmt.Errorf("agent profile %q: %w", key, err)
		}
		prof.Model, prof.ModelSet = model, true
	}
	if patch.SystemPrompt.Present {
		prof.SystemPrompt = patch.SystemPrompt.Value
	}
	if patch.PromptFile.Present {
		prof.PromptFile = patch.PromptFile.Value
	}
	return prof, nil
}
