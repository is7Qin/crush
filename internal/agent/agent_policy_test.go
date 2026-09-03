package agent

import (
	"slices"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent/task"
	"github.com/charmbracelet/crush/internal/agent/taskquestion"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSessionAgent_MaxSteps_BlocksSecondStep proves the loop-boundary
// enforcement: with MaxSteps=1 the first assistant step runs, the second
// is refused at PrepareStep with the stable sentinel, and the model is
// never asked for the follow-up completion.
func TestSessionAgent_MaxSteps_BlocksSecondStep(t *testing.T) {
	env := testEnv(t)
	sess, err := env.sessions.Create(t.Context(), "steps")
	require.NoError(t, err)

	model := &toolCallStreamModel{}
	sa := stepsChildAgent(env, model, 1)

	_, err = sa.Run(t.Context(), SessionAgentCall{SessionID: sess.ID, Prompt: "go"})
	require.ErrorIs(t, err, task.ErrStepLimit)
	assert.Equal(t, int32(1), model.calls.Load(), "only the first step may reach the model")
}

// TestSessionAgent_MaxSteps_BoundaryIsExact proves MaxSteps=N allows
// exactly N assistant steps: with N=2 the second step runs and the third
// is refused.
func TestSessionAgent_MaxSteps_BoundaryIsExact(t *testing.T) {
	env := testEnv(t)
	sess, err := env.sessions.Create(t.Context(), "steps2")
	require.NoError(t, err)

	model := &toolCallStreamModel{}
	sa := stepsChildAgent(env, model, 2)

	_, err = sa.Run(t.Context(), SessionAgentCall{SessionID: sess.ID, Prompt: "go"})
	require.ErrorIs(t, err, task.ErrStepLimit)
	assert.Equal(t, int32(2), model.calls.Load(), "exactly max_steps steps may reach the model")
}

// TestSessionAgent_MaxSteps_AllowsEarlierFinish proves the limit does not
// disturb runs that end before it: a single-step answer under MaxSteps=1
// completes normally.
func TestSessionAgent_MaxSteps_AllowsEarlierFinish(t *testing.T) {
	env := testEnv(t)
	sess, err := env.sessions.Create(t.Context(), "early")
	require.NoError(t, err)

	sa := stepsChildAgent(env, &finishStreamModel{text: "done"}, 1)
	result, err := sa.Run(t.Context(), SessionAgentCall{SessionID: sess.ID, Prompt: "go"})
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, "done", result.Response.Content.Text())
}

// TestBuildProfileAgent_CarriesMaxSteps proves the production seam: the
// resolved profile's max_steps reaches the constructed child SessionAgent
// and the returned ResolvedProfile.
func TestBuildProfileAgent_CarriesMaxSteps(t *testing.T) {
	coord := profileTestCoordinator(t)

	agent, prof, err := coord.buildProfileAgent(t.Context(), "limited", "")
	require.NoError(t, err)
	assert.Equal(t, 1, prof.MaxSteps)
	assert.Equal(t, uint64(1), prof.Generation, "resolved profile carries the config snapshot generation")
	assert.Equal(t, 1, agent.(*sessionAgent).maxSteps, "child loop carries the profile limit")

	_, prof, err = coord.buildProfileAgent(t.Context(), "reviewer", "")
	require.NoError(t, err)
	assert.Zero(t, prof.MaxSteps, "omitted max_steps means unlimited")
}

// TestDelegateTask_StepLimitTerminalizesFailed drives the full
// delegation under the unified asynchronous contract: the call returns
// its acceptance immediately, and a profile with max_steps=1 whose
// child keeps calling tools terminalizes the task as failed with the
// stable reason task_step_limit.
func TestDelegateTask_StepLimitTerminalizesFailed(t *testing.T) {
	coord, mgr, env, events := policyToolEnv(t, func(config.ResolvedProfile) fantasy.LanguageModel {
		return &toolCallStreamModel{}
	})
	parent, err := env.sessions.Create(t.Context(), "Parent")
	require.NoError(t, err)
	ctx := delegationCtx(t, parent.ID, "msg-steps")

	resp := runAgentTool(t, coord, ctx, "call-steps", `{"prompt":"loop","profile":"limited"}`)
	require.False(t, resp.IsError, resp.Content)
	taskID := taskIDFromResponse(t, resp.Content)

	terminal := waitTerminalEvent(t, events, taskID)
	assert.Equal(t, task.StatusFailed, terminal.Status)
	assert.Equal(t, task.ErrStepLimit.Error(), terminal.Err)

	tasks, err := mgr.List(t.Context(), parent.ID, "")
	require.NoError(t, err)
	require.Len(t, tasks, 1)
}

// TestDelegateTask_DurationTimeoutTerminalizesFailed drives the full
// delegation under the unified asynchronous contract: the call returns
// its acceptance immediately even while the child blocks in the model,
// and a profile with a 50ms max_duration terminalizes the task as
// failed with the stable reason task_timeout.
func TestDelegateTask_DurationTimeoutTerminalizesFailed(t *testing.T) {
	model := newBlockingModel()
	coord, mgr, env, events := policyToolEnv(t, func(config.ResolvedProfile) fantasy.LanguageModel {
		return model
	})
	parent, err := env.sessions.Create(t.Context(), "Parent")
	require.NoError(t, err)
	ctx := delegationCtx(t, parent.ID, "msg-timeout")

	resp := runAgentTool(t, coord, ctx, "call-brief", `{"prompt":"hang","profile":"brief"}`)
	require.False(t, resp.IsError, resp.Content)
	taskID := taskIDFromResponse(t, resp.Content)

	terminal := waitTerminalEvent(t, events, taskID)
	assert.Equal(t, task.StatusFailed, terminal.Status)
	assert.Equal(t, task.ErrTimeout.Error(), terminal.Err)
	select {
	case <-model.entered:
	default:
		t.Fatal("child never reached the model")
	}

	tasks, err := mgr.List(t.Context(), parent.ID, "")
	require.NoError(t, err)
	require.Len(t, tasks, 1)
}

// TestDelegateTask_TimeoutCancelRaceSingleTerminal proves that an
// owner cancellation racing the profile deadline produces exactly one
// terminal state: cancelled when cancellation won, failed/task_timeout
// otherwise. The caller's request context is not a cancellation source
// under the unified contract, so the race is driven through the
// task-control path.
func TestDelegateTask_TimeoutCancelRaceSingleTerminal(t *testing.T) {
	model := newBlockingModel()
	coord, mgr, env, events := policyToolEnv(t, func(config.ResolvedProfile) fantasy.LanguageModel {
		return model
	})
	parent, err := env.sessions.Create(t.Context(), "Parent")
	require.NoError(t, err)
	ctx := delegationCtx(t, parent.ID, "msg-race")

	resp := runAgentTool(t, coord, ctx, "call-race", `{"prompt":"hang","profile":"brief"}`)
	require.False(t, resp.IsError, resp.Content)
	taskID := taskIDFromResponse(t, resp.Content)

	// Race the owner cancellation against the 50ms profile deadline.
	_ = mgr.Cancel(t.Context(), parent.ID, taskID)

	// First terminal event must arrive; then give a duplicate
	// terminalization a window to show up and prove none does.
	first := waitTerminalEvent(t, events, taskID)
	settled := false
	dupWindow := time.After(300 * time.Millisecond)
	for !settled {
		select {
		case ev := <-events:
			require.False(t, ev.Task.ID == taskID && ev.Task.Status.Terminal(),
				"a second terminal event must not be published")
		case <-dupWindow:
			settled = true
		}
	}
	switch first.Status {
	case task.StatusCancelled:
	case task.StatusFailed:
		assert.Equal(t, task.ErrTimeout.Error(), first.Err)
	default:
		t.Fatalf("unexpected terminal status %s", first.Status)
	}
}

// TestBuildTools_QuietProfileExcludesTaskQuestionTool proves the
// acceptance rule: a profile with can_ask_questions=false has no
// task-aware question tool even when the transport is wired; transport
// availability never grants a tool excluded by the profile.
func TestBuildTools_QuietProfileExcludesTaskQuestionTool(t *testing.T) {
	coord := profileTestCoordinator(t)
	svc := taskquestion.NewService(taskquestion.Config{})
	coord.taskQuestions = svc
	t.Cleanup(svc.Shutdown)

	hasQuestion := func(ts []fantasy.AgentTool) bool {
		return slices.ContainsFunc(ts, func(ft fantasy.AgentTool) bool {
			return ft.Info().Name == tools.QuestionToolName
		})
	}

	quiet, err := coord.cfg.Config().ResolveAgentProfile("quiet")
	require.NoError(t, err)
	require.False(t, quiet.CanAskQuestions)

	quietTools, err := coord.buildTools(t.Context(), quiet.Agent, true)
	require.NoError(t, err)
	assert.False(t, hasQuestion(quietTools),
		"question-excluded profile must not get the task-aware question tool")

	// Control: the default coder profile keeps the tool with the same
	// transport wired.
	coder, err := coord.cfg.Config().ResolveAgentProfile(config.AgentCoder)
	require.NoError(t, err)
	got, err := coord.buildTools(t.Context(), coder.Agent, true)
	require.NoError(t, err)
	assert.True(t, hasQuestion(got))
}
