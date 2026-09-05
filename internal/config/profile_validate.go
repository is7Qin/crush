package config

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// asciiLower returns s with ASCII letters folded to lower case. Non-ASCII
// bytes are left untouched so they can never fold-collide.
func asciiLower(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + 32
		}
		return r
	}, s)
}

// NormalizeAgentProfileKeys folds profile keys to their canonical ASCII
// lower-case form. Two keys that collide after folding are a duplicate and
// are rejected.
func NormalizeAgentProfileKeys(profiles map[string]AgentProfilePatch) (map[string]AgentProfilePatch, error) {
	normalized := make(map[string]AgentProfilePatch, len(profiles))
	for _, key := range slices.Sorted(maps.Keys(profiles)) {
		canonical := asciiLower(key)
		if _, dup := normalized[canonical]; dup {
			return nil, fmt.Errorf("duplicate agent profile key %q after case normalization", canonical)
		}
		normalized[canonical] = profiles[key]
	}
	return normalized, nil
}

// profileReasoningEfforts is the deterministic allow-list of profile
// reasoning strengths, matching the OMO vocabulary. Per-model support is
// enforced later at call time: effectiveReasoningEffort ignores a level the
// model does not list and falls back to the model default, so a valid
// strength is never silently sent to a model that cannot consume it.
var profileReasoningEfforts = []string{"minimal", "low", "medium", "high", "xhigh", "max"}

// ValidateAgentProfiles rejects malformed profile data with diagnostics that
// name the profile key and field. It does not check model availability; the
// resolver does that against the live provider catalog.
func ValidateAgentProfiles(profiles map[string]AgentProfilePatch) error {
	var errs []error
	for _, key := range slices.Sorted(maps.Keys(profiles)) {
		p := profiles[key]
		if key == "" {
			errs = append(errs, fmt.Errorf("agent profile: name must not be empty"))
			continue
		}
		if key == AgenticFetchInternalProfile {
			// The name is the task core's hidden-task marker; a user
			// profile claiming it would forge the reserved identity,
			// so the load rejects it outright.
			errs = append(errs, fmt.Errorf("agent profile %q: name is reserved for internal use", key))
		}
		if strings.IndexFunc(key, func(r rune) bool {
			return r <= ' ' || r == 0x7f
		}) >= 0 {
			errs = append(errs, fmt.Errorf("agent profile %q: name must not contain whitespace or control characters", key))
		}
		if p.Model.Present && p.Models.Present {
			errs = append(errs, fmt.Errorf("agent profile %q: model and models are mutually exclusive", key))
		}
		if p.Model.Present {
			if err := validateModelRefs(key, "model", []string{p.Model.Value}); err != nil {
				errs = append(errs, err)
			}
		}
		if p.Models.Present {
			if err := validateModelRefs(key, "models", p.Models.Value); err != nil {
				errs = append(errs, err)
			}
		}
		if p.ReasoningEffort.Present && !slices.Contains(profileReasoningEfforts, p.ReasoningEffort.Value) {
			errs = append(errs, fmt.Errorf("agent profile %q: reasoning_effort must be one of %s, got %q",
				key, strings.Join(profileReasoningEfforts, ", "), p.ReasoningEffort.Value))
		}
		if p.SystemPrompt.Present && p.PromptFile.Present {
			errs = append(errs, fmt.Errorf("agent profile %q: system_prompt and prompt_file are mutually exclusive", key))
		}
		if p.MaxSteps.Present && p.MaxSteps.Value <= 0 {
			errs = append(errs, fmt.Errorf("agent profile %q: max_steps must be greater than 0", key))
		}
		if p.MaxDuration.Present && p.MaxDuration.Value <= 0 {
			errs = append(errs, fmt.Errorf("agent profile %q: max_duration must be greater than 0", key))
		}
	}
	return errors.Join(errs...)
}

func validateModelRefs(key, field string, refs []string) error {
	for _, ref := range refs {
		if _, err := ParseModelRef(ref); err != nil {
			return fmt.Errorf("agent profile %q: %s: %w", key, field, err)
		}
	}
	return nil
}
