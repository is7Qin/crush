package agent

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"charm.land/fantasy"

	"github.com/charmbracelet/crush/internal/agent/prompt"
	"github.com/charmbracelet/crush/internal/agent/task"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/permission"
)

//go:embed templates/agentic_fetch.md
var agenticFetchToolDescription string

// agenticFetchValidationResult holds the validated parameters from the tool call context.
type agenticFetchValidationResult struct {
	SessionID      string
	AgentMessageID string
}

// validateAgenticFetchParams validates the tool call parameters and extracts required context values.
func validateAgenticFetchParams(ctx context.Context, params tools.AgenticFetchParams) (agenticFetchValidationResult, error) {
	if params.Prompt == "" {
		return agenticFetchValidationResult{}, errors.New("prompt is required")
	}

	sessionID := tools.GetSessionFromContext(ctx)
	if sessionID == "" {
		return agenticFetchValidationResult{}, errors.New("session id missing from context")
	}

	agentMessageID := tools.GetMessageFromContext(ctx)
	if agentMessageID == "" {
		return agenticFetchValidationResult{}, errors.New("agent message id missing from context")
	}

	return agenticFetchValidationResult{
		SessionID:      sessionID,
		AgentMessageID: agentMessageID,
	}, nil
}

//go:embed templates/agentic_fetch_prompt.md.tpl
var agenticFetchPromptTmpl []byte

// agenticFetchChildToolNames is the fixed specialized fetch/web tool
// set the hidden fetch child runs with: read and web tools only, no
// call_agent, no agentic_fetch, no task-control tools, no question,
// and no workspace-mutating tool. It is pinned against
// buildFetchTools by test.
var agenticFetchChildToolNames = []string{
	tools.WebFetchToolName,
	tools.WebSearchToolName,
	tools.GlobToolName,
	tools.GrepToolName,
	tools.SourcegraphToolName,
	tools.ViewToolName,
}

// agenticFetchChildSpec carries the attempt-stable inputs the hidden
// fetch child needs. Identity fields come from trusted tool context
// captured at admission, never from model input.
type agenticFetchChildSpec struct {
	Small           Model
	ProviderCfg     config.ProviderConfig
	Client          *http.Client
	FetchParams     tools.AgenticFetchParams
	CallerSessionID string
	ParentMessageID string
	ToolCallID      string
}

// agenticFetchTool registers agentic_fetch as a primary-only
// capability whose child execution runs on the unified asynchronous
// task model: it admits one hidden system-owned task through the task
// manager (fixed internal profile, trusted owner session from the
// tool context) and returns the durable acceptance immediately. It
// never creates a child session directly; the admission transaction
// owns the binding. Hidden tasks are excluded from every public task
// control path and prompt the user once, for the originating
// agentic_fetch call itself.
func (c *coordinator) agenticFetchTool(_ context.Context, client *http.Client) (fantasy.AgentTool, error) {
	if client == nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.MaxIdleConns = 100
		transport.MaxIdleConnsPerHost = 10
		transport.IdleConnTimeout = 90 * time.Second

		client = &http.Client{
			Timeout:   30 * time.Second,
			Transport: transport,
		}
	}

	return fantasy.NewParallelAgentTool(
		tools.AgenticFetchToolName,
		agenticFetchToolDescription,
		func(ctx context.Context, params tools.AgenticFetchParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			validationResult, err := validateAgenticFetchParams(ctx, params)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}

			// Children never spawn nested fetch agents. The profile
			// tool policy already hides the tool from child palettes;
			// the trusted depth marker closes a hand-built call.
			if depth := tools.GetAgentDepthFromContext(ctx); depth > 0 {
				return fantasy.NewTextErrorResponse(task.ErrDelegation.Error()), nil
			}
			if c.tasks == nil {
				// Durable task admission is the only execution mode:
				// with no task manager there is nowhere to persist
				// the hidden attempt, so the call is rejected rather
				// than run synchronously, and no child session is
				// created.
				return fantasy.NewTextErrorResponse(
					"agentic_fetch unavailable: no task manager is configured for this workspace"), nil
			}

			// Determine description based on mode.
			var description string
			if params.URL != "" {
				description = fmt.Sprintf("Fetch and analyze content from URL: %s", params.URL)
			} else {
				description = "Search the web and analyze results"
			}

			p, err := c.permissions.Request(
				ctx,
				permission.CreatePermissionRequest{
					SessionID:   validationResult.SessionID,
					Path:        c.cfg.WorkingDir(),
					ToolCallID:  call.ID,
					ToolName:    tools.AgenticFetchToolName,
					Action:      "fetch",
					Description: description,
					Params:      tools.AgenticFetchPermissionsParams(params),
				},
			)
			if err != nil {
				return fantasy.ToolResponse{}, err
			}
			if !p {
				return tools.NewPermissionDeniedResponse(), nil
			}

			// Resolve the small model (and its provider) before
			// admission so a misconfigured provider fails the call as
			// a model-visible error instead of a durably failed task.
			_, small, err := c.buildAgentModels(ctx, true)
			if err != nil {
				return fantasy.ToolResponse{}, fmt.Errorf("error building models: %s", err)
			}
			smallProviderCfg, ok := c.cfg.Config().Providers.Get(small.ModelCfg.Provider)
			if !ok {
				return fantasy.NewTextErrorResponse("small model provider not configured"), nil
			}

			sortedNames := slices.Clone(agenticFetchChildToolNames)
			slices.Sort(sortedNames)
			spec := agenticFetchChildSpec{
				Small:           small,
				ProviderCfg:     smallProviderCfg,
				Client:          client,
				FetchParams:     params,
				CallerSessionID: validationResult.SessionID,
				ParentMessageID: validationResult.AgentMessageID,
				ToolCallID:      call.ID,
			}
			req := task.StartRequest{
				CallerSessionID:   validationResult.SessionID,
				CallerDepth:       tools.GetAgentDepthFromContext(ctx),
				ParentSessionID:   validationResult.SessionID,
				ChildSessionID:    c.sessions.CreateAgentToolSessionID(validationResult.AgentMessageID, call.ID),
				ChildTitle:        "Fetch Analysis",
				ParentMessageID:   validationResult.AgentMessageID,
				ToolCallID:        call.ID,
				Profile:           task.HiddenProfile,
				ProfileGeneration: c.cfg.Config().ProfileGeneration,
				PromptFingerprint: fingerprintText(params.Prompt),
				ToolFingerprint:   fingerprintText(strings.Join(sortedNames, ",")),
				Provider:          small.ModelCfg.Provider,
				Model:             small.ModelCfg.Model,
				Prompt:            params.Prompt,
				Run: func(runCtx context.Context, h *task.Handle) (task.Result, error) {
					return c.runAgenticFetchChild(runCtx, h, spec)
				},
			}
			t, err := c.tasks.Start(ctx, req)
			if err != nil {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("agentic fetch failed: %v", err)), nil
			}
			if t == nil {
				return fantasy.NewTextErrorResponse("agentic fetch failed: task manager returned no task record"), nil
			}
			// The child session committed with the task; publish it
			// after the transaction like the call_agent path does.
			c.publishTaskSessionCreated(ctx, t.ChildSessionID)
			return acceptanceToolResponse(t), nil
		},
	), nil
}

// runAgenticFetchChild executes one hidden fetch attempt against the
// child session bound by the admission transaction. The temporary
// fetch directory is owned here: it is created when the attempt
// starts, exists for the full attempt so every child turn can view
// and grep the saved page, and is removed when the attempt finishes,
// before the manager commits the terminal delivery. The attempt runs
// under the manager's workspace-derived context with the execution
// fence stamped on it, so terminalization fences out late tool
// admission and only this runner performs the directory cleanup.
func (c *coordinator) runAgenticFetchChild(ctx context.Context, h *task.Handle, spec agenticFetchChildSpec) (task.Result, error) {
	tmpDir, err := os.MkdirTemp(c.cfg.Config().Options.DataDirectory, "crush-fetch-*")
	if err != nil {
		return task.Result{}, fmt.Errorf("failed to create temporary directory: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	// Trusted task-run identity: the execution fence gates every
	// child tool call. Deliberately no task-question context: hidden
	// tasks have no question tool and no public question surface.
	ctx = tools.WithTaskRunContext(ctx, tools.TaskRunContext{Fence: h.Fence()})

	fullPrompt, err := c.renderAgenticFetchPrompt(ctx, spec, h.Prompt(), tmpDir)
	if err != nil {
		return task.Result{}, err
	}

	agent, err := c.newAgenticFetchAgent(ctx, tmpDir, spec)
	if err != nil {
		return task.Result{}, err
	}

	params := subAgentParams{
		Agent:          agent,
		SessionID:      spec.CallerSessionID,
		AgentMessageID: spec.ParentMessageID,
		ToolCallID:     spec.ToolCallID,
		Prompt:         fullPrompt,
		SessionSetup: func(sessionID string) {
			c.permissions.AutoApproveSession(sessionID)
		},
	}
	resp, err := c.executeSubAgent(ctx, params, h.ChildSessionID())
	if err != nil {
		return task.Result{}, err
	}
	if resp.IsError {
		return task.Result{}, errors.New(resp.Content)
	}
	return task.Result{Text: resp.Content}, nil
}

// renderAgenticFetchPrompt turns the mailbox prompt and fetch params
// into the child's full prompt: URL mode prefetches the page (saving
// large content into the attempt's temporary directory for the
// child's view/grep tools) and search mode instructs the child to
// search on its own.
func (c *coordinator) renderAgenticFetchPrompt(ctx context.Context, spec agenticFetchChildSpec, promptText, tmpDir string) (string, error) {
	params := spec.FetchParams
	if params.URL == "" {
		// Search mode: let the sub-agent search and fetch as needed.
		return fmt.Sprintf("%s\n\nUse the web_search tool to find relevant information. Break down the question into smaller, focused searches if needed. After searching, use web_fetch to get detailed content from the most relevant results.", promptText), nil
	}

	content, err := tools.FetchURLAndConvert(ctx, spec.Client, params.URL)
	if err != nil {
		return "", fmt.Errorf("failed to fetch URL: %w", err)
	}
	if len(content) <= tools.LargeContentThreshold {
		return fmt.Sprintf("%s\n\nWeb page URL: %s\n\n<webpage_content>\n%s\n</webpage_content>", promptText, params.URL, content), nil
	}

	tempFile, err := os.CreateTemp(tmpDir, "page-*.md")
	if err != nil {
		return "", fmt.Errorf("failed to create temporary file: %w", err)
	}
	defer tempFile.Close()
	if _, err := tempFile.WriteString(content); err != nil {
		return "", fmt.Errorf("failed to write content to file: %w", err)
	}
	return fmt.Sprintf("%s\n\nThe web page from %s has been saved to: %s\n\nUse the view and grep tools to analyze this file and extract the requested information.", promptText, params.URL, tempFile.Name()), nil
}

// newAgenticFetchAgent builds the hidden fetch child agent. Tests
// substitute newFetchChild to drive the routing without a provider;
// production leaves it nil.
func (c *coordinator) newAgenticFetchAgent(ctx context.Context, tmpDir string, spec agenticFetchChildSpec) (SessionAgent, error) {
	if c.newFetchChild != nil {
		return c.newFetchChild(ctx, tmpDir, spec)
	}
	return c.buildAgenticFetchAgent(ctx, tmpDir, spec)
}

// buildAgenticFetchAgent constructs the fetch child SessionAgent:
// small model for both slots (the fetch analysis does not need the
// large model), the fixed specialized fetch/web tool set wrapped by
// the workspace admission and execution-fence layer, and no hook
// interception. The sub-agent tools run without hook interception:
// the top-level agentic_fetch call itself is already wrapped from the
// coder's side, and firing hooks again for every inner tool call
// would run the user's hooks N times per delegated turn.
func (c *coordinator) buildAgenticFetchAgent(ctx context.Context, tmpDir string, spec agenticFetchChildSpec) (SessionAgent, error) {
	promptTemplate, err := prompt.NewPrompt("agentic_fetch", string(agenticFetchPromptTmpl), prompt.WithWorkingDir(tmpDir))
	if err != nil {
		return nil, fmt.Errorf("error creating prompt: %s", err)
	}
	systemPrompt, err := promptTemplate.Build(ctx, spec.Small.Model.Provider(), spec.Small.Model.Model(), c.cfg)
	if err != nil {
		return nil, fmt.Errorf("error building system prompt: %s", err)
	}
	return NewSessionAgent(SessionAgentOptions{
		LargeModel:           spec.Small, // Use small model for both (fetch doesn't need large)
		SmallModel:           spec.Small,
		SystemPromptPrefix:   spec.ProviderCfg.SystemPromptPrefix,
		SystemPrompt:         systemPrompt,
		IsSubAgent:           true,
		DisableAutoSummarize: c.cfg.Config().Options.DisableAutoSummarize,
		MaxRetries:           c.cfg.Config().Options.MaxRetries,
		IsYolo:               c.permissions.SkipRequests(),
		Sessions:             c.sessions,
		Messages:             c.messages,
		Tools: tools.WrapToolsExclusive(
			c.buildFetchTools(tmpDir, spec.Client),
			c.cfg.WorkingDir(),
			tools.WorkspaceLeases(),
		),
	}), nil
}

// buildFetchTools returns the fixed specialized fetch/web tool set
// rooted at the attempt's temporary directory. It contains no
// delegation, task-control, question, or workspace-mutating tool.
func (c *coordinator) buildFetchTools(tmpDir string, client *http.Client) []fantasy.AgentTool {
	return []fantasy.AgentTool{
		tools.NewWebFetchTool(tmpDir, client),
		tools.NewWebSearchTool(client),
		tools.NewGlobTool(tmpDir, c.cfg.Config().Tools.Glob),
		tools.NewGrepTool(tmpDir, c.cfg.Config().Tools.Grep),
		tools.NewSourcegraphTool(client),
		tools.NewViewTool(c.lspManager, c.permissions, c.filetracker, nil, tmpDir),
	}
}
