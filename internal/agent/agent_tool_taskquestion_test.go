package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent/task"
	"github.com/charmbracelet/crush/internal/agent/taskquestion"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/question"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// bridgeTaskQuestions wires a real taskquestion service to a real task
// manager through the same Suspend/Resume seam app.New uses: the
// callbacks run on the asking runner's context, which carries the
// trusted task correlation, and drive the task's runner handle between
// running and waiting_for_input.
func bridgeTaskQuestions(mgr *task.Manager) taskquestion.TaskQuestionService {
	handle := func(ctx context.Context) (*task.Handle, error) {
		tc, ok := tools.GetTaskQuestionContextFromContext(ctx)
		if !ok {
			return nil, taskquestion.ErrNotFound
		}
		h, ok := mgr.Handle(tc.TaskID)
		if !ok {
			return nil, taskquestion.ErrNotFound
		}
		return h, nil
	}
	return taskquestion.NewService(taskquestion.Config{
		Suspend: func(ctx context.Context, _ string) error {
			h, err := handle(ctx)
			if err != nil {
				return err
			}
			return h.WaitingForInput(ctx)
		},
		Resume: func(ctx context.Context, _ string) error {
			h, err := handle(ctx)
			if err != nil {
				return err
			}
			return h.Resumed(ctx)
		},
	})
}

// questionAskingChild is a child agent that immediately invokes the
// task-aware question tool with its own run context, mirroring what a
// real child does when the model calls `question`. It records the
// trusted carrier it observed for assertions.
type questionAskingChild struct {
	SessionAgent
	svc     taskquestion.TaskQuestionService
	carrier chan tools.TaskQuestionContext
}

func (c *questionAskingChild) Run(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
	// The real sessionAgent stamps the child session id onto the tool
	// context; mimic that so the batch carries the child's identity.
	ctx = context.WithValue(ctx, tools.SessionIDContextKey, call.SessionID)
	tc, ok := tools.GetTaskQuestionContextFromContext(ctx)
	if !ok {
		return nil, errors.New("child run carries no task question context")
	}
	c.carrier <- tc

	tool := tools.NewTaskQuestionTool(c.svc)
	resp, err := tool.Run(ctx, fantasy.ToolCall{
		ID:    "question-call-1",
		Name:  tools.QuestionToolName,
		Input: `{"questions":[{"type":"yes_no","question":"Proceed?","description":"Confirm the step."}]}`,
	})
	if err != nil {
		return nil, err
	}
	if resp.IsError {
		return nil, errors.New(resp.Content)
	}
	return &fantasy.AgentResult{
		Response: fantasy.Response{
			Content: fantasy.ResponseContent{fantasy.TextContent{Text: resp.Content}},
		},
	}, nil
}

// tqEnv builds a coordinator wired to a real task manager and a real
// taskquestion service, with a child factory that asks one question.
func tqEnv(
	t *testing.T,
	carrier chan tools.TaskQuestionContext,
) (*coordinator, *task.Manager, fakeEnv, taskquestion.TaskQuestionService, chan task.Event) {
	t.Helper()
	env := testEnv(t)
	require.NoError(t, os.WriteFile(filepath.Join(env.workingDir, "crush.json"), []byte(profileTestConfig), 0o644))
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)
	cfg.SetupAgents()

	mgr := task.New(t.Context(), task.Config{
		WorkspaceID: env.workingDir,
		Store:       task.NewSQLiteStore(env.conn),
	})
	svc := bridgeTaskQuestions(mgr)

	coord := &coordinator{
		cfg:           cfg,
		sessions:      env.sessions,
		messages:      env.messages,
		permissions:   env.permissions,
		history:       env.history,
		filetracker:   *env.filetracker,
		tasks:         mgr,
		taskQuestions: svc,
	}
	coord.newChildAgent = func(_ context.Context, name, _ string) (SessionAgent, config.ResolvedProfile, error) {
		return &questionAskingChild{
			SessionAgent: fakeChildAgent(env, &finishStreamModel{text: "unused"}),
			svc:          svc,
			carrier:      carrier,
		}, config.ResolvedProfile{Name: name}, nil
	}
	events := make(chan task.Event, 64)
	mgr.Subscribe(func(ev task.Event) { events <- ev })
	t.Cleanup(func() {
		svc.Shutdown()
		_ = mgr.Shutdown(context.WithoutCancel(t.Context()))
	})
	return coord, mgr, env, svc, events
}

// waitTaskEvent waits for the next event of the given type.
func waitTaskEvent(t *testing.T, events chan task.Event, want task.EventType) task.Event {
	t.Helper()
	deadline := time.After(15 * time.Second)
	for {
		select {
		case ev := <-events:
			if ev.Type == want {
				return ev
			}
		case <-deadline:
			t.Fatalf("timeout waiting for task event %s", want)
		}
	}
}

// waitPendingQuestion waits until the service tracks exactly one
// unresolved question and returns it.
func waitPendingQuestion(t *testing.T, svc taskquestion.TaskQuestionService) taskquestion.TaskQuestion {
	t.Helper()
	deadline := time.After(15 * time.Second)
	for {
		if unresolved := svc.Unresolved(); len(unresolved) == 1 {
			return unresolved[0]
		}
		select {
		case <-deadline:
			t.Fatal("timeout waiting for a pending task question")
			return taskquestion.TaskQuestion{}
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// TestBuildTools_ChildQuestionToolRegistration proves the palette
// gating: children get the question tool only when the task-aware
// transport is wired, and the primary interactive path is untouched.
func TestBuildTools_ChildQuestionToolRegistration(t *testing.T) {
	coord := profileTestCoordinator(t)
	prof, err := coord.cfg.Config().ResolveAgentProfile(config.AgentCoder)
	require.NoError(t, err)

	hasQuestion := func(ts []fantasy.AgentTool) bool {
		return slices.ContainsFunc(ts, func(ft fantasy.AgentTool) bool {
			return ft.Info().Name == tools.QuestionToolName
		})
	}

	got, err := coord.buildTools(t.Context(), prof.Agent, true)
	require.NoError(t, err)
	assert.False(t, hasQuestion(got), "children without a transport keep the question-free palette")

	coord.taskQuestions = taskquestion.NewService(taskquestion.Config{})
	got, err = coord.buildTools(t.Context(), prof.Agent, true)
	require.NoError(t, err)
	assert.True(t, hasQuestion(got), "children with a transport get the task-aware question tool")

	got, err = coord.buildTools(t.Context(), coord.cfg.Config().Agents[config.AgentCoder], false)
	require.NoError(t, err)
	assert.False(t, hasQuestion(got), "non-interactive primary keeps its current behavior")
}

// TestTaskQuestion_ChildSuspendAnswerResumeLifecycle drives the full
// transport under the unified asynchronous contract: the delegation
// returns its acceptance immediately, the child asks through the
// task-aware tool, the task suspends to waiting_for_input, the
// correlated request is published, a foreign caller is rejected, the
// owner answers, the task resumes, and the child completes; the answer
// reaches the parent through the terminal task result, never through
// the already-returned tool response.
func TestTaskQuestion_ChildSuspendAnswerResumeLifecycle(t *testing.T) {
	carrier := make(chan tools.TaskQuestionContext, 1)
	coord, mgr, env, svc, events := tqEnv(t, carrier)
	parent, err := env.sessions.Create(t.Context(), "Parent")
	require.NoError(t, err)

	batches := svc.Subscribe(t.Context())
	ctx := delegationCtx(t, parent.ID, "msg-tq")
	resp := runAgentTool(t, coord, ctx, "call-tq", `{"prompt":"ask"}`)
	require.False(t, resp.IsError, resp.Content)
	taskID := taskIDFromResponse(t, resp.Content)

	waitTaskEvent(t, events, task.EventWaitingForInput)
	q := waitPendingQuestion(t, svc)
	assert.Equal(t, taskID, q.TaskID)
	assert.Equal(t, parent.ID, q.OwnerSessionID)
	assert.NotEmpty(t, q.ChildSessionID)
	assert.Equal(t, uint64(1), q.RunGeneration)

	// The correlated batch reaches subscribers with the child's
	// session identity.
	select {
	case ev := <-batches:
		assert.Equal(t, q.ChildSessionID, ev.Payload.Batch.SessionID)
		assert.Equal(t, "Proceed?", ev.Payload.Batch.Questions[0].Text)
	case <-time.After(15 * time.Second):
		t.Fatal("task question batch was never published")
	}

	// Only the owner session may answer.
	yes := true
	answers := []question.Answer{{QuestionID: q.Batch.Questions[0].ID, Yes: &yes}}
	require.ErrorIs(t, svc.AnswerTask("intruder-session", q.QuestionID, answers), taskquestion.ErrNotOwner)
	require.NoError(t, svc.AnswerTask(parent.ID, q.QuestionID, answers))

	waitTaskEvent(t, events, task.EventResumed)

	// The child observed the trusted carrier for its own task run.
	tc := <-carrier
	assert.Equal(t, q.TaskID, tc.TaskID)
	assert.Equal(t, parent.ID, tc.OwnerSessionID)
	assert.Equal(t, q.ChildSessionID, tc.ChildSessionID)
	assert.Equal(t, uint64(1), tc.RunGeneration)

	terminal := waitTerminalEvent(t, events, taskID)
	assert.Equal(t, task.StatusCompleted, terminal.Status)
	assert.Equal(t, "Q1: Proceed?\nUser answered: yes", terminal.Result)

	tasks, err := mgr.List(t.Context(), parent.ID, "")
	require.NoError(t, err)
	require.Len(t, tasks, 1)
}

// TestTaskQuestion_OwnerCancelTerminalizesChild proves the cancellation
// path: an owner cancellation wakes the blocked AskTask with
// question.ErrCancelled, the child's run stops on that error, and the
// task terminalizes failed without a resume. The tool response already
// returned its acceptance and is unaffected.
func TestTaskQuestion_OwnerCancelTerminalizesChild(t *testing.T) {
	carrier := make(chan tools.TaskQuestionContext, 1)
	coord, mgr, env, svc, events := tqEnv(t, carrier)
	parent, err := env.sessions.Create(t.Context(), "Parent")
	require.NoError(t, err)

	ctx := delegationCtx(t, parent.ID, "msg-tq-cancel")
	resp := runAgentTool(t, coord, ctx, "call-tq-cancel", `{"prompt":"ask"}`)
	require.False(t, resp.IsError, resp.Content)
	taskID := taskIDFromResponse(t, resp.Content)

	waitTaskEvent(t, events, task.EventWaitingForInput)
	q := waitPendingQuestion(t, svc)
	require.ErrorIs(t, svc.CancelTask(parent.ID, q.QuestionID), nil)

	_, pending := svc.Pending(q.QuestionID)
	assert.False(t, pending)
	terminal := waitTerminalEvent(t, events, taskID)
	assert.Equal(t, task.StatusFailed, terminal.Status)
	assert.Contains(t, terminal.Err, "User cancelled this question")

	tasks, err := mgr.List(t.Context(), parent.ID, "")
	require.NoError(t, err)
	require.Len(t, tasks, 1)
}
