package agent

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/charmbracelet/crush/internal/config"
)

// buildPrimaryAgent resolves and fully constructs a primary agent before it
// can be published to the coordinator. Unlike buildProfileAgent, the primary
// keeps its profile's delegation and task-control capabilities.
func (c *coordinator) buildPrimaryAgent(ctx context.Context, name string) (SessionAgent, config.ResolvedProfile, error) {
	profile, err := c.cfg.Config().ResolvePrimaryAgentProfile(name)
	if err != nil {
		return nil, config.ResolvedProfile{}, err
	}
	if !slices.Contains(c.cfg.Config().DiscoverableAgentProfiles(), profile.Name) {
		return nil, config.ResolvedProfile{}, fmt.Errorf("%w: %s", config.ErrUnknownAgentProfile, profile.Name)
	}

	large, small, err := c.buildPrimaryProfileModels(ctx, profile)
	if err != nil {
		return nil, config.ResolvedProfile{}, err
	}
	systemPrompt, err := c.profileSystemPrompt(ctx, profile, large, true)
	if err != nil {
		return nil, config.ResolvedProfile{}, err
	}
	agentTools, err := c.buildTools(ctx, profile.Agent, false)
	if err != nil {
		return nil, config.ResolvedProfile{}, err
	}

	largeProviderCfg, ok := c.cfg.Config().Providers.Get(large.ModelCfg.Provider)
	if !ok {
		return nil, config.ResolvedProfile{}, fmt.Errorf("model provider not configured: %s", large.ModelCfg.Provider)
	}
	return NewSessionAgent(SessionAgentOptions{
		LargeModel:           large,
		SmallModel:           small,
		SystemPromptPrefix:   largeProviderCfg.SystemPromptPrefix,
		SystemPrompt:         systemPrompt,
		IsSubAgent:           false,
		DisableAutoSummarize: c.cfg.Config().Options.DisableAutoSummarize,
		IsYolo:               c.permissions.SkipRequests(),
		Sessions:             c.sessions,
		Messages:             c.messages,
		Tools:                agentTools,
		Notify:               c.notify,
		RunComplete:          c.runComplete,
		MaxSteps:             profile.MaxSteps,
	}), profile, nil
}

func (c *coordinator) buildPrimaryProfileModels(ctx context.Context, profile config.ResolvedProfile) (Model, Model, error) {
	if !profile.ModelSet {
		large, small, err := c.buildAgentModels(ctx, false)
		if err != nil {
			return Model{}, Model{}, err
		}
		if profile.ReasoningEffort.Present {
			large.ModelCfg.ReasoningEffort = profile.ReasoningEffort.Value
		}
		return large, small, nil
	}

	large, err := c.buildModel(ctx, profile.Model, false)
	if err != nil {
		return Model{}, Model{}, err
	}
	smallCfg, ok := c.cfg.Config().Models[config.SelectedModelTypeSmall]
	if !ok {
		return Model{}, Model{}, errSmallModelNotSelected
	}
	small, err := c.buildModel(ctx, smallCfg, true)
	if err != nil {
		return Model{}, Model{}, err
	}
	if profile.ReasoningEffort.Present {
		large.ModelCfg.ReasoningEffort = profile.ReasoningEffort.Value
	}
	return large, small, nil
}

func (c *coordinator) beginPrimaryRun() (SessionAgent, func(), error) {
	c.primaryMu.Lock()
	defer c.primaryMu.Unlock()
	if c.currentAgent == nil {
		return nil, func() {}, errPrimaryAgentNotInitialized
	}
	c.primaryRuns++
	return c.currentAgent, c.endPrimaryRun, nil
}

func (c *coordinator) endPrimaryRun() {
	c.primaryMu.Lock()
	defer c.primaryMu.Unlock()
	if c.primaryRuns > 0 {
		c.primaryRuns--
	}
}

func (c *coordinator) primaryAgentSnapshot() (SessionAgent, string) {
	c.primaryMu.RLock()
	defer c.primaryMu.RUnlock()
	return c.currentAgent, c.primaryProfile
}

func (c *coordinator) primaryBusyLocked() bool {
	return c.primaryRuns > 0 || c.currentAgent != nil && c.currentAgent.IsBusy()
}

// SetPrimaryAgent builds a complete replacement and atomically selects it.
// The existing agent remains untouched if resolution or construction fails.
func (c *coordinator) SetPrimaryAgent(ctx context.Context, profile string) error {
	c.primaryMu.Lock()
	if c.primaryBusyLocked() {
		c.primaryMu.Unlock()
		return ErrPrimaryAgentBusy
	}
	c.primaryMu.Unlock()

	replacement, resolved, err := c.buildPrimaryAgent(ctx, profile)
	if err != nil {
		return err
	}

	c.primaryMu.Lock()
	defer c.primaryMu.Unlock()
	if c.primaryBusyLocked() {
		return ErrPrimaryAgentBusy
	}
	c.currentAgent = replacement
	c.primaryProfile = resolved.Name
	if c.agents == nil {
		c.agents = make(map[string]SessionAgent)
	}
	c.agents[resolved.Name] = replacement
	return nil
}

// PrimaryAgent returns the canonical name of the selected primary profile.
func (c *coordinator) PrimaryAgent() string {
	_, profile := c.primaryAgentSnapshot()
	return profile
}

var errPrimaryAgentNotInitialized = errors.New("primary agent is not initialized")
