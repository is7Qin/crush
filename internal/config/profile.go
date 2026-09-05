package config

import (
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"time"
)

// DelegationToolName is the public name of the sub-agent delegation tool
// registered by the coordinator (see internal/agent.AgentToolName). It is
// duplicated here because internal/config must not import internal/agent.
const DelegationToolName = "call_agent"

// QuestionToolName duplicates tools.QuestionToolName for the same reason
// as DelegationToolName: internal/config must not import
// internal/agent/tools.
const QuestionToolName = "question"

// AgenticFetchInternalProfile duplicates task.HiddenProfile: internal/config
// must not import internal/agent/task. It is the fixed internal profile the
// agentic_fetch tool admits hidden child tasks under. The name is reserved:
// it is not user-configurable (ValidateAgentProfiles rejects a profile with
// this key and ResolveAgentProfile never resolves to it) and it is never
// listed in tool descriptions.
const AgenticFetchInternalProfile = "agentic_fetch_internal"

// Optional records whether a profile field was present in a configuration
// source, preserving the omitted-versus-explicitly-empty distinction the
// profile merge semantics depend on. Absent keys leave the zero value
// (Present=false); JSON null is rejected for every profile field; a present
// empty string/list/map yields Present=true with the empty value.
type Optional[T any] struct {
	Value   T
	Present bool
}

// Some returns a present Optional holding v.
func Some[T any](v T) Optional[T] {
	return Optional[T]{Value: v, Present: true}
}

// UnmarshalJSON decodes a present profile field. encoding/json only calls
// this when the key exists, so absence keeps Present=false.
func (o *Optional[T]) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		return errors.New("must not be null")
	}
	var v T
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	o.Value = v
	o.Present = true
	return nil
}

// AgentProfilePatch is the pure-data source form of an agent profile. Every
// field is presence-aware so a higher-priority layer can override, clear, or
// inherit each field independently. Later stages (Markdown discovery, the
// `agent` crushrc builtin, the profile loader/snapshot) produce this same
// representation.
type AgentProfilePatch struct {
	Name            Optional[string]              `json:"name,omitempty"`
	Role            Optional[string]              `json:"role,omitempty"`
	Description     Optional[string]              `json:"description,omitempty"`
	SystemPrompt    Optional[string]              `json:"system_prompt,omitempty"`
	PromptFile      Optional[string]              `json:"prompt_file,omitempty"`
	Model           Optional[string]              `json:"model,omitempty"`
	Models          Optional[[]string]            `json:"models,omitempty"`
	ReasoningEffort Optional[string]              `json:"reasoning_effort,omitempty"`
	AllowedTools    Optional[[]string]            `json:"allowed_tools,omitempty"`
	AllowedMCP      Optional[map[string][]string] `json:"allowed_mcp,omitempty"`
	DeniedTools     Optional[[]string]            `json:"denied_tools,omitempty"`
	ContextPaths    Optional[[]string]            `json:"context_paths,omitempty"`
	Skills          Optional[[]string]            `json:"skills,omitempty"`
	CanDelegate     Optional[bool]                `json:"can_delegate,omitempty"`
	CanAskQuestions Optional[bool]                `json:"can_ask_questions,omitempty"`
	MaxSteps        Optional[int]                 `json:"max_steps,omitempty"`
	MaxDuration     Optional[time.Duration]       `json:"max_duration,omitempty"`
	Disabled        Optional[bool]                `json:"disabled,omitempty"`
}

// MarshalJSON emits only present fields so absent fields stay absent across
// a round-trip instead of materializing explicit nulls or empty values.
func (p AgentProfilePatch) MarshalJSON() ([]byte, error) {
	out := map[string]any{}
	if p.Name.Present {
		out["name"] = p.Name.Value
	}
	if p.Role.Present {
		out["role"] = p.Role.Value
	}
	if p.Description.Present {
		out["description"] = p.Description.Value
	}
	if p.SystemPrompt.Present {
		out["system_prompt"] = p.SystemPrompt.Value
	}
	if p.PromptFile.Present {
		out["prompt_file"] = p.PromptFile.Value
	}
	if p.Model.Present {
		out["model"] = p.Model.Value
	}
	if p.Models.Present {
		out["models"] = p.Models.Value
	}
	if p.ReasoningEffort.Present {
		out["reasoning_effort"] = p.ReasoningEffort.Value
	}
	if p.AllowedTools.Present {
		out["allowed_tools"] = p.AllowedTools.Value
	}
	if p.AllowedMCP.Present {
		out["allowed_mcp"] = p.AllowedMCP.Value
	}
	if p.DeniedTools.Present {
		out["denied_tools"] = p.DeniedTools.Value
	}
	if p.ContextPaths.Present {
		out["context_paths"] = p.ContextPaths.Value
	}
	if p.Skills.Present {
		out["skills"] = p.Skills.Value
	}
	if p.CanDelegate.Present {
		out["can_delegate"] = p.CanDelegate.Value
	}
	if p.CanAskQuestions.Present {
		out["can_ask_questions"] = p.CanAskQuestions.Value
	}
	if p.MaxSteps.Present {
		out["max_steps"] = p.MaxSteps.Value
	}
	if p.MaxDuration.Present {
		out["max_duration"] = p.MaxDuration.Value
	}
	if p.Disabled.Present {
		out["disabled"] = p.Disabled.Value
	}
	return json.Marshal(out)
}

// mergeOnto returns p with every field that is present in override replaced
// by override's value. Lists and maps replace rather than append; an explicit
// empty value clears the inherited one.
func (p AgentProfilePatch) mergeOnto(override AgentProfilePatch) AgentProfilePatch {
	merged := p
	if override.Name.Present {
		merged.Name = override.Name
	}
	if override.Role.Present {
		merged.Role = override.Role
	}
	if override.Description.Present {
		merged.Description = override.Description
	}
	if override.SystemPrompt.Present {
		merged.SystemPrompt = override.SystemPrompt
	}
	if override.PromptFile.Present {
		merged.PromptFile = override.PromptFile
	}
	if override.Model.Present {
		merged.Model = override.Model
	}
	if override.Models.Present {
		merged.Models = override.Models
	}
	if override.ReasoningEffort.Present {
		merged.ReasoningEffort = override.ReasoningEffort
	}
	if override.AllowedTools.Present {
		merged.AllowedTools = override.AllowedTools
	}
	if override.AllowedMCP.Present {
		merged.AllowedMCP = override.AllowedMCP
	}
	if override.DeniedTools.Present {
		merged.DeniedTools = override.DeniedTools
	}
	if override.ContextPaths.Present {
		merged.ContextPaths = override.ContextPaths
	}
	if override.Skills.Present {
		merged.Skills = override.Skills
	}
	if override.CanDelegate.Present {
		merged.CanDelegate = override.CanDelegate
	}
	if override.CanAskQuestions.Present {
		merged.CanAskQuestions = override.CanAskQuestions
	}
	if override.MaxSteps.Present {
		merged.MaxSteps = override.MaxSteps
	}
	if override.MaxDuration.Present {
		merged.MaxDuration = override.MaxDuration
	}
	if override.Disabled.Present {
		merged.Disabled = override.Disabled
	}
	return merged
}

// MergeAgentProfilePatches layers patch maps in ascending priority
// (built-in defaults < global < project) and returns the merged result.
// Iteration is key-sorted so the outcome never depends on map order.
func MergeAgentProfilePatches(layers ...map[string]AgentProfilePatch) map[string]AgentProfilePatch {
	merged := map[string]AgentProfilePatch{}
	for _, layer := range layers {
		for _, key := range slices.Sorted(maps.Keys(layer)) {
			merged[key] = merged[key].mergeOnto(layer[key])
		}
	}
	return merged
}

// applyProfilePatch projects a merged profile patch onto the derived runtime
// agent. Only fields the current runtime Agent understands are applied;
// prompt, model, and skills are consumed by the later profile resolver
// through the AgentProfiles snapshot. Delegation and question policy are
// applied here as tool-set effects: can_delegate=false strips
// call_agent and can_ask_questions=false strips question from the
// effective allow-list.
func applyProfilePatch(agent Agent, workspaceAllowed []string, patch AgentProfilePatch) Agent {
	if patch.Name.Present {
		agent.Name = patch.Name.Value
	}
	if patch.Description.Present {
		agent.Description = patch.Description.Value
	}
	if patch.AllowedTools.Present {
		if len(patch.AllowedTools.Value) == 0 {
			// An empty allow-list means all workspace tools.
			agent.AllowedTools = slices.Clone(workspaceAllowed)
		} else {
			agent.AllowedTools = filterSlice(workspaceAllowed, patch.AllowedTools.Value, true)
		}
	}
	if patch.DeniedTools.Present {
		agent.AllowedTools = filterSlice(agent.AllowedTools, patch.DeniedTools.Value, false)
	}
	if patch.CanDelegate.Present && !patch.CanDelegate.Value {
		agent.AllowedTools = filterSlice(agent.AllowedTools, []string{DelegationToolName}, false)
	}
	if patch.CanAskQuestions.Present && !patch.CanAskQuestions.Value {
		agent.AllowedTools = filterSlice(agent.AllowedTools, []string{QuestionToolName}, false)
	}
	if patch.AllowedMCP.Present {
		agent.AllowedMCP = patch.AllowedMCP.Value
	}
	if patch.ContextPaths.Present {
		agent.ContextPaths = patch.ContextPaths.Value
	}
	if patch.Disabled.Present {
		agent.Disabled = patch.Disabled.Value
	}
	return agent
}
