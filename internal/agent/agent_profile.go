package agent

import (
	"context"
	"fmt"
	"os"

	"github.com/charmbracelet/crush/internal/agent/prompt"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/filepathext"
)

// buildProfileAgent resolves a call_agent profile selection (plus an
// optional exact request-model override) and constructs a fresh child
// SessionAgent for one delegation. A new agent is built per call so
// child sessions never share mutable agent state. The resolved profile
// is returned alongside so the task runner can apply the concrete
// lifecycle policy (max_duration, generation) captured at delegation
// time; a later config reload never affects an in-flight attempt. An
// unavailable request-model override fails without fallback.
func (c *coordinator) buildProfileAgent(ctx context.Context, name, requestModel string) (SessionAgent, config.ResolvedProfile, error) {
	// Test seam: delegateTask drives the full task lifecycle, so tests
	// substitute a fake-model child agent here without a provider.
	if c.newChildAgent != nil {
		return c.newChildAgent(ctx, name, requestModel)
	}

	prof, err := c.cfg.Config().ResolveAgentProfile(name)
	if err != nil {
		return nil, config.ResolvedProfile{}, err
	}
	if requestModel != "" {
		sel, err := c.cfg.Config().ResolveModelRef(requestModel)
		if err != nil {
			return nil, config.ResolvedProfile{}, err
		}
		prof.Model, prof.ModelSet = sel, true
	}

	large, small, err := c.buildProfileModels(ctx, prof)
	if err != nil {
		return nil, config.ResolvedProfile{}, err
	}

	systemPrompt, err := c.profileSystemPrompt(ctx, prof, large, false)
	if err != nil {
		return nil, config.ResolvedProfile{}, err
	}

	agentTools, err := c.buildTools(ctx, prof.Agent, true)
	if err != nil {
		return nil, config.ResolvedProfile{}, err
	}

	largeProviderCfg, _ := c.cfg.Config().Providers.Get(large.ModelCfg.Provider)
	return NewSessionAgent(SessionAgentOptions{
		LargeModel:           large,
		SmallModel:           small,
		SystemPromptPrefix:   largeProviderCfg.SystemPromptPrefix,
		SystemPrompt:         systemPrompt,
		IsSubAgent:           true,
		DisableAutoSummarize: c.cfg.Config().Options.DisableAutoSummarize,
		IsYolo:               c.permissions.SkipRequests(),
		Sessions:             c.sessions,
		Messages:             c.messages,
		Tools:                agentTools,
		Notify:               c.notify,
		RunComplete:          c.runComplete,
		MaxSteps:             prof.MaxSteps,
	}), prof, nil
}

// buildProfileModels resolves the child's model pair, honoring the
// profile's exact model override for the large model when configured. A
// configured profile reasoning_effort overrides the large selection's own
// effort; whether the model supports it is decided later by
// effectiveReasoningEffort, which falls back to the model default instead
// of sending an unsupported level.
func (c *coordinator) buildProfileModels(ctx context.Context, prof config.ResolvedProfile) (Model, Model, error) {
	var large, small Model
	var err error
	if prof.ModelSet {
		large, err = c.buildModel(ctx, prof.Model, true)
		if err != nil {
			return Model{}, Model{}, err
		}
		smallCfg, ok := c.cfg.Config().Models[config.SelectedModelTypeSmall]
		if !ok {
			return Model{}, Model{}, errSmallModelNotSelected
		}
		small, err = c.buildModel(ctx, smallCfg, true)
		if err != nil {
			return Model{}, Model{}, err
		}
	} else {
		large, small, err = c.buildAgentModels(ctx, true)
		if err != nil {
			return Model{}, Model{}, err
		}
	}
	if prof.ReasoningEffort.Present {
		large.ModelCfg.ReasoningEffort = prof.ReasoningEffort.Value
	}
	return large, small, nil
}

const childPromptRestriction = `

CHILD SESSION RESTRICTION: you are a child agent. You must not call
call_agent or otherwise delegate work to another agent. Complete the assigned
task with the tools available to you and report the result to the parent.`

// profileSystemPrompt renders a profile's system prompt in one of two
// explicit modes: primary prompts are returned unchanged; child prompts get
// the child-only delegation restriction. A prompt file and the built-in
// coder/task templates are returned unchanged in primary mode.
func (c *coordinator) profileSystemPrompt(ctx context.Context, prof config.ResolvedProfile, large Model, primary bool) (string, error) {
	withChildRestriction := func(prompt string) string {
		if primary {
			return prompt
		}
		return prompt + childPromptRestriction
	}
	if prof.SystemPrompt != "" {
		return withChildRestriction(prof.SystemPrompt), nil
	}
	if prof.PromptFile != "" {
		path := filepathext.SmartJoin(c.cfg.WorkingDir(), prof.PromptFile)
		data, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("read profile prompt file: %w", err)
		}
		return withChildRestriction(string(data)), nil
	}

	tmpl := taskPromptTmpl
	if prof.Name == config.AgentCoder {
		tmpl = coderPromptTmpl
	}
	p, err := prompt.NewPrompt(prof.Name, string(tmpl), prompt.WithWorkingDir(c.cfg.WorkingDir()))
	if err != nil {
		return "", err
	}
	rendered, err := p.Build(ctx, large.Model.Provider(), large.Model.Model(), c.cfg)
	if err != nil {
		return "", err
	}
	return withChildRestriction(rendered), nil
}
