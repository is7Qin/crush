package agent

import (
	"cmp"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"

	"charm.land/fantasy"

	"github.com/charmbracelet/crush/internal/agent/task"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/charmbracelet/crush/internal/session"
)

//go:embed templates/agent_tool.md
var agentToolDescription string

// AgentParams is the public call_agent request schema. Every
// delegation is asynchronous: the call returns a durable task
// acceptance, never a synchronous child result. TaskID names a
// terminal task whose retained child session should receive a new
// attempt; Model is an optional exact "provider/model" override that
// replaces the profile's model selection.
type AgentParams struct {
	Profile string `json:"profile,omitempty" description:"Optional agent profile to run the task under: a built-in (the Crush base agents coder or task, or the OMO-native roster: sisyphus, hephaestus, oracle, librarian, explore, multimodal-looker, prometheus, metis, momus, atlas, sisyphus-junior) or a profile configured via the agents config key, which patches the built-in of the same name (default: coder with full ordinary capabilities)"`
	Prompt  string `json:"prompt" description:"The task for the agent to perform"`
	Model   string `json:"model,omitempty" description:"Optional exact model override for the child as provider/model (e.g. mock/other-model). An unavailable model fails without fallback"`
	TaskID  string `json:"task_id,omitempty" description:"Optional id of one of your own terminal call_agent tasks to continue: a new attempt runs against the retained child session with the current profile and model policy"`
}

// AgentCallAccepted is the machine-readable acknowledgement returned
// for every call_agent delegation. The task is already durably
// admitted when this response is produced; the child runs on the
// workspace context and terminalizes later.
type AgentCallAccepted struct {
	TaskID         string `json:"task_id"`
	ChildSessionID string `json:"child_session_id,omitempty"`
	Profile        string `json:"profile"`
	Provider       string `json:"resolved_provider"`
	Model          string `json:"resolved_model"`
	Status         string `json:"status"`
}

const (
	// AgentToolName is the public delegation tool name.
	AgentToolName = "call_agent"

	// LegacyAgentToolName is the historical name of the delegation
	// tool. It is not a live tool anywhere: persisted historical
	// tool-call messages still carry it, and the chat UI renders
	// those as agent delegations.
	LegacyAgentToolName = "agent"

	// defaultAgentProfile is selected when a call omits the profile.
	defaultAgentProfile = config.AgentCoder
)

func (c *coordinator) agentTool(_ context.Context) (fantasy.AgentTool, error) {
	return fantasy.NewParallelAgentTool(
		AgentToolName,
		agentToolDescription,
		func(ctx context.Context, params AgentParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.Prompt == "" {
				return fantasy.NewTextErrorResponse("prompt is required"), nil
			}

			sessionID := tools.GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.ToolResponse{}, errors.New("session id missing from context")
			}

			agentMessageID := tools.GetMessageFromContext(ctx)
			if agentMessageID == "" {
				return fantasy.ToolResponse{}, errors.New("agent message id missing from context")
			}

			// Children never delegate. The depth marker is set by the
			// coordinator, never from tool arguments, so a forged call
			// cannot reset it. The task manager re-checks the same value.
			depth := tools.GetAgentDepthFromContext(ctx)
			if depth > 0 {
				return fantasy.NewTextErrorResponse(task.ErrDelegation.Error()), nil
			}

			// Build a fresh child agent per call: SessionAgent carries
			// mutable per-session state and must never be shared across
			// delegations. The resolved profile is captured by value so
			// the attempt runs under the policy and generation resolved
			// here; a config reload cannot change it mid-run. A
			// continuation re-resolves the current profile/model policy
			// the same way a fresh delegation does.
			profile := cmp.Or(params.Profile, defaultAgentProfile)
			agent, prof, err := c.buildProfileAgent(ctx, profile, params.Model)
			if err != nil {
				// Profile selection failures (unknown or disabled
				// profile, unavailable model override) are model-visible
				// tool errors, not parent-run failures.
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}

			if c.tasks == nil {
				// Durable task admission is the only execution mode.
				// With no task manager there is nowhere to persist the
				// attempt, so the delegation is rejected rather than
				// run synchronously in the caller's request; no child
				// session is created.
				return fantasy.NewTextErrorResponse(
					"call_agent unavailable: no task manager is configured for this workspace"), nil
			}

			subParams := subAgentParams{
				Agent:          agent,
				SessionID:      sessionID,
				AgentMessageID: agentMessageID,
				ToolCallID:     call.ID,
				Prompt:         params.Prompt,
				SessionTitle:   "New Agent Session",
			}

			return c.delegateTask(ctx, subParams, prof, params.TaskID, params.Model)
		},
	), nil
}

// delegateTask admits one call_agent delegation through the task
// manager and returns the durable acceptance acknowledgement before
// the child terminalizes. There is no synchronous result path: the
// child runs on the manager's workspace-derived context, so the
// parent's request context cannot cancel it (agent_cancel is the
// cancellation path). The dispatch repository creates the child
// session inside the admission transaction: delegateTask only names
// the deterministic child session id and title derived from trusted
// tool context, and a failed admission leaves no child session or
// mailbox row behind. When taskID is set the call is a continuation:
// the manager validates the target belongs to the caller and is
// terminal, then binds the attempt to the retained child session.
//
// The profile's resolved lifecycle policy is applied here at the task
// boundary: max_duration derives a deadline on the manager context (a
// child that observes it terminalizes as failed/task_timeout unless a
// cancellation already won), and the child's max_steps enforcement inside
// the SessionAgent loop surfaces as failed/task_step_limit.
func (c *coordinator) delegateTask(ctx context.Context, params subAgentParams, prof config.ResolvedProfile, taskID, requestedModel string) (fantasy.ToolResponse, error) {
	childSessionID := ""
	if taskID == "" {
		// Name the child deterministically from trusted tool context;
		// the admission transaction inserts the sessions row itself.
		childSessionID = c.sessions.CreateAgentToolSessionID(params.AgentMessageID, params.ToolCallID)
	}

	model := params.Agent.Model()
	req := task.StartRequest{
		CallerSessionID:   params.SessionID,
		CallerDepth:       tools.GetAgentDepthFromContext(ctx),
		ParentSessionID:   params.SessionID,
		ChildSessionID:    childSessionID,
		ChildTitle:        params.SessionTitle,
		ParentMessageID:   params.AgentMessageID,
		ToolCallID:        params.ToolCallID,
		Profile:           prof.Name,
		ProfileGeneration: prof.Generation,
		RequestedModel:    requestedModel,
		PromptFingerprint: fingerprintText(params.Prompt),
		ToolFingerprint:   fingerprintTools(prof),
		Provider:          model.ModelCfg.Provider,
		Model:             model.ModelCfg.Model,
		Prompt:            params.Prompt,
		ResumesTaskID:     taskID,
		Run: func(runCtx context.Context, h *task.Handle) (task.Result, error) {
			// Every attempt delivers its own mailbox prompt: the
			// admission prompt for the first, then each claimed
			// message in FIFO order for successors.
			attemptParams := params
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
					OwnerSessionID: params.SessionID,
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
			return task.Result{Text: resp.Content}, nil
		},
	}

	t, err := c.tasks.Start(ctx, req)
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("agent delegation failed: %v", err)), nil
	}
	if t == nil {
		return fantasy.NewTextErrorResponse("agent delegation failed: task manager returned no task record"), nil
	}
	if taskID == "" {
		// The child session committed with the task; publish it now,
		// after the transaction, so observers never see a session
		// whose task admission failed.
		c.publishTaskSessionCreated(ctx, t.ChildSessionID)
	}
	return acceptanceToolResponse(t), nil
}

// acceptanceToolResponse renders the acceptance acknowledgement as
// model-visible text plus the typed AgentCallAccepted metadata for
// machine consumers.
func acceptanceToolResponse(t *task.Task) fantasy.ToolResponse {
	text := fmt.Sprintf(
		"Agent task accepted: status: %s.\nTask ID: %s\nChild session ID: %s\nProfile: %s\nModel: %s/%s",
		t.Status, t.ID, t.ChildSessionID, t.Profile, t.Provider, t.Model,
	)
	resp := fantasy.NewTextResponse(text)
	metadata := AgentCallAccepted{
		TaskID:         t.ID,
		ChildSessionID: t.ChildSessionID,
		Profile:        t.Profile,
		Provider:       t.Provider,
		Model:          t.Model,
		Status:         string(t.Status),
	}
	return fantasy.WithResponseMetadata(resp, metadata)
}

// publishTaskSessionCreated notifies the session stream about a child
// session that the admission transaction committed. Publication
// stays outside the transaction on purpose; the task event already
// fired for the admission itself, so a publisher-less session
// implementation (tests) is simply skipped.
func (c *coordinator) publishTaskSessionCreated(ctx context.Context, sessionID string) {
	pub, ok := c.sessions.(pubsub.Publisher[session.Session])
	if !ok {
		return
	}
	sess, err := c.sessions.Get(context.WithoutCancel(ctx), sessionID)
	if err != nil {
		slog.Warn("Failed to load task child session for notification",
			"session_id", sessionID, "error", err)
		return
	}
	pub.Publish(pubsub.CreatedEvent, sess)
}

// fingerprintText is the stable SHA-256 hex digest used for task
// prompt and tool fingerprints.
func fingerprintText(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// fingerprintTools digests the profile's effective tool policy: the
// sorted ordinary-tool allow-list plus each MCP server's sorted tool
// allow-list.
func fingerprintTools(prof config.ResolvedProfile) string {
	allowed := slices.Clone(prof.Agent.AllowedTools)
	slices.Sort(allowed)
	var b strings.Builder
	b.WriteString(strings.Join(allowed, ","))
	for _, server := range slices.Sorted(maps.Keys(prof.Agent.AllowedMCP)) {
		mcpTools := slices.Clone(prof.Agent.AllowedMCP[server])
		slices.Sort(mcpTools)
		b.WriteString("|")
		b.WriteString(server)
		b.WriteString(":")
		b.WriteString(strings.Join(mcpTools, ","))
	}
	return fingerprintText(b.String())
}
