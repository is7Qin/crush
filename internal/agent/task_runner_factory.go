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
		run := c.childAttemptRunner(agent, prof, subAgentParams{
			SessionID:    parent,
			SessionTitle: "New Agent Session",
		})
		return task.RebuiltChild{
			Run:             run,
			Key:             task.CapacityKey{Provider: model.ModelCfg.Provider, Model: model.ModelCfg.Model},
			ParentSessionID: parent,
		}, nil
	}
}

// childAttemptRunner is the single shared Runner constructor that
// executes one delegated child attempt. Both the admission path in
// delegateTask and the release/rebuild path in TaskRunnerFactory
// use it: each attempt delivers its own mailbox prompt from the
// handle against the retained child session, and cost/report
// anchoring match on both paths. What differs per caller travels
// in base: the admission path passes its full subAgentParams (real
// parent session, agent message id, tool call id, session title),
// while the rebuild path passes only the stored parent session and
// its own session title. The prompt always comes from the handle,
// never from base.
func (c *coordinator) childAttemptRunner(agent SessionAgent, prof config.ResolvedProfile, base subAgentParams) task.Runner {
	return func(runCtx context.Context, h *task.Handle) (task.Result, error) {
		// Every attempt delivers its own mailbox prompt: the
		// admission prompt for the first, then each claimed
		// message in FIFO order for successors.
		attemptParams := base
		attemptParams.Agent = agent
		attemptParams.Prompt = h.Prompt()
		childID := h.ChildSessionID()
		// Trusted task-run identity: the execution fence gates
		// every child tool call, and the derived max_duration
		// deadline below bounds its write-lease wait.
		runCtx = tools.WithTaskRunContext(runCtx, tools.TaskRunContext{Fence: h.Fence()})
		if prof.MaxDuration > 0 {
			var cancel context.CancelFunc
			runCtx, cancel = context.WithTimeout(runCtx, prof.MaxDuration)
			defer cancel()
		}
		if c.taskQuestions != nil {
			// Trusted correlation for the child's question
			// transport: the identity comes from the task
			// record and the manager handle, never from model
			// arguments, so a child can only ask on behalf of
			// its own task run.
			runCtx = tools.WithTaskQuestionContext(runCtx, tools.TaskQuestionContext{
				TaskID:         h.TaskID(),
				OwnerSessionID: attemptParams.SessionID,
				ChildSessionID: childID,
				RunGeneration:  h.RunGeneration(),
			})
		}
		resp, err := c.executeSubAgent(runCtx, attemptParams, childID)
		if err != nil {
			return task.Result{}, err
		}
		if resp.IsError {
			// A child that observed its derived deadline failed
			// on the profile's max_duration. Manager-level
			// cancellation still wins in settle, so a racing
			// cancel terminalizes as cancelled, not timeout.
			if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
				return task.Result{}, task.ErrTimeout
			}
			return task.Result{}, errors.New(resp.Content)
		}
		out := task.Result{Text: resp.Content}
		// Anchor the report against the revision the child read:
		// child reads land under the child session id, so the
		// latest anchored read is this report's revision. A child
		// that never read file content reports no anchor.
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
