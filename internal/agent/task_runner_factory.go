package agent

import (
	"cmp"
	"context"
	"errors"

	"github.com/charmbracelet/crush/internal/agent/task"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/config"
)

// TaskRunnerFactory rebuilds a runner for a released child session
// from the stored task record. It re-resolves the current
// profile/model policy the same way a fresh delegation does and
// mirrors delegateTask's runner: each attempt delivers its own
// mailbox prompt against the retained child session. A released
// child therefore resumes without pinning its SessionAgent for the
// process lifetime.
func (c *coordinator) TaskRunnerFactory() task.RunnerFactory {
	return func(ctx context.Context, t task.Task) (task.RebuiltChild, error) {
		profile := cmp.Or(t.Profile, c.cfg.Config().FallbackDiscoverableAgent())
		agent, prof, err := c.buildProfileAgent(ctx, profile, t.RequestedModel)
		if err != nil {
			return task.RebuiltChild{}, err
		}
		model := agent.Model()
		parent := cmp.Or(t.ParentSessionID, t.OwnerSessionID)
		run := c.rebuiltRunner(agent, prof, parent)
		return task.RebuiltChild{
			Run:             run,
			Key:             task.CapacityKey{Provider: model.ModelCfg.Provider, Model: model.ModelCfg.Model},
			ParentSessionID: parent,
		}, nil
	}
}

// rebuiltRunner mirrors delegateTask's runner for a factory-rebuilt
// child: the attempt prompt comes from the mailbox handle, execution
// runs against the retained child session, and cost/report anchoring
// match the admission path.
func (c *coordinator) rebuiltRunner(agent SessionAgent, prof config.ResolvedProfile, parent string) task.Runner {
	return func(runCtx context.Context, h *task.Handle) (task.Result, error) {
		params := subAgentParams{
			Agent:        agent,
			SessionID:    parent,
			Prompt:       h.Prompt(),
			SessionTitle: "New Agent Session",
		}
		childID := h.ChildSessionID()
		runCtx = tools.WithTaskRunContext(runCtx, tools.TaskRunContext{Fence: h.Fence()})
		if prof.MaxDuration > 0 {
			var cancel context.CancelFunc
			runCtx, cancel = context.WithTimeout(runCtx, prof.MaxDuration)
			defer cancel()
		}
		if c.taskQuestions != nil {
			runCtx = tools.WithTaskQuestionContext(runCtx, tools.TaskQuestionContext{
				TaskID:         h.TaskID(),
				OwnerSessionID: parent,
				ChildSessionID: childID,
				RunGeneration:  h.RunGeneration(),
			})
		}
		resp, err := c.executeSubAgent(runCtx, params, childID)
		if err != nil {
			return task.Result{}, err
		}
		if resp.IsError {
			if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
				return task.Result{}, task.ErrTimeout
			}
			return task.Result{}, errors.New(resp.Content)
		}
		out := task.Result{Text: resp.Content}
		if c.filetracker != nil {
			if anchor, ok := c.filetracker.LatestAnchor(runCtx, childID); ok {
				out.Anchor = &task.ReportAnchor{
					Path:     anchor.Path,
					SHA256_8: anchor.SHA8,
					Lines:    anchor.Lines,
				}
			}
		}
		return out, nil
	}
}
