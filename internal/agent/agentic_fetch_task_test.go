package agent

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent/task"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fetchTaskEnv wires a coordinator to a real SQLite-backed task
// manager with a fake fetch child. nextChild observes the attempt's
// temporary directory, which is the seam the cleanup and lifetime
// assertions need.
func fetchTaskEnv(
	t *testing.T,
	limits task.Limits,
	nextChild func(env fakeEnv, tmpDir string) SessionAgent,
) (*coordinator, *task.Manager, fakeEnv, chan task.Event) {
	t.Helper()
	env := testEnv(t)
	require.NoError(t, os.WriteFile(filepath.Join(env.workingDir, "crush.json"), []byte(profileTestConfig), 0o644))
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)
	cfg.SetupAgents()
	// The attempt creates its temporary directory under the data
	// directory; production startup guarantees it exists.
	require.NoError(t, os.MkdirAll(cfg.Config().Options.DataDirectory, 0o755))

	coord := &coordinator{
		cfg:         cfg,
		sessions:    env.sessions,
		messages:    env.messages,
		permissions: env.permissions,
		history:     env.history,
		filetracker: *env.filetracker,
	}
	mgr := task.New(t.Context(), task.Config{
		WorkspaceID: env.workingDir,
		Limits:      limits,
		Store:       task.NewSQLiteStore(env.conn),
	})
	events := make(chan task.Event, 64)
	mgr.Subscribe(func(ev task.Event) { events <- ev })
	t.Cleanup(func() {
		_ = mgr.Shutdown(context.WithoutCancel(t.Context()))
	})

	coord.tasks = mgr
	if nextChild != nil {
		coord.newFetchChild = func(_ context.Context, tmpDir string, _ agenticFetchChildSpec) (SessionAgent, error) {
			return nextChild(env, tmpDir), nil
		}
	}
	return coord, mgr, env, events
}

func runFetchTool(t *testing.T, coord *coordinator, ctx context.Context, callID, input string) fantasy.ToolResponse {
	t.Helper()
	tool, err := coord.agenticFetchTool(ctx, nil)
	require.NoError(t, err)
	resp, err := tool.Run(ctx, fantasy.ToolCall{ID: callID, Name: tools.AgenticFetchToolName, Input: input})
	require.NoError(t, err)
	return resp
}

// fetchTempDirs lists the hidden attempts' live temporary fetch
// directories under the data directory.
func fetchTempDirs(t *testing.T, coord *coordinator) []string {
	t.Helper()
	entries, err := os.ReadDir(coord.cfg.Config().Options.DataDirectory)
	require.NoError(t, err)
	var out []string
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), "crush-fetch-") {
			out = append(out, filepath.Join(coord.cfg.Config().Options.DataDirectory, e.Name()))
		}
	}
	return out
}

// TestAgenticFetch_HiddenTaskRouting proves the unified execution
// contract: the tool admits one hidden task (no direct session
// creation, no synchronous child result), the child observes a live
// temp directory, the directory is gone after terminalization, the
// bounded result is durably delivered to the parent inbox/outbox, and
// every public control path reports task_not_found while internal
// diagnostics with the matching workspace and owner can read it.
func TestAgenticFetch_HiddenTaskRouting(t *testing.T) {
	gate := newGatedModel("fetched answer")
	var observedTmpDir string
	coord, mgr, env, events := fetchTaskEnv(t, task.Limits{}, func(env fakeEnv, tmpDir string) SessionAgent {
		observedTmpDir = tmpDir
		return fakeChildAgent(env, gate)
	})
	parent, err := env.sessions.Create(t.Context(), "Parent")
	require.NoError(t, err)
	ctx := delegationCtx(t, parent.ID, "msg-fetch")

	resp := runFetchTool(t, coord, ctx, "call-fetch", `{"prompt":"what is the web saying"}`)
	acc := acceptedOf(t, resp)
	assert.Contains(t, resp.Content, "accepted")
	assert.NotContains(t, resp.Content, "fetched answer", "no synchronous child result")

	// One hidden task row, bound to the deterministic child session
	// the admission transaction inserted (never a direct create).
	require.Equal(t, task.HiddenProfile, acc.Profile)
	childID := coord.sessions.CreateAgentToolSessionID("msg-fetch", "call-fetch")
	require.Equal(t, childID, acc.ChildSessionID)

	<-gate.entered
	// The attempt owns a live temporary fetch directory.
	dirs := fetchTempDirs(t, coord)
	require.Len(t, dirs, 1, "the running attempt owns exactly one temp fetch directory")
	require.Equal(t, dirs[0], observedTmpDir)

	// Public denial while live: not found even for the owner.
	_, err = mgr.Status(t.Context(), parent.ID, acc.TaskID)
	require.ErrorIs(t, err, task.ErrNotFound)

	close(gate.release)
	waitTerminalEvent(t, events, acc.TaskID)

	// Temporary directory removed after terminalization.
	require.Eventually(t, func() bool { return len(fetchTempDirs(t, coord)) == 0 },
		5*time.Second, 10*time.Millisecond, "the attempt's temp fetch directory must be cleaned up")

	// Durable terminal delivery: one bounded result in the parent
	// inbox and a terminal outbox row for the task.
	inbox, err := mgr.Inbox(t.Context(), parent.ID)
	require.NoError(t, err)
	require.Len(t, inbox, 1)
	envl, err := inbox[0].Envelope()
	require.NoError(t, err)
	assert.Equal(t, acc.TaskID, envl.TaskID)
	assert.Equal(t, task.HiddenProfile, envl.Profile)
	assert.Equal(t, "fetched answer", envl.Result)
	outbox, err := mgr.Outbox(t.Context())
	require.NoError(t, err)
	var terminal int
	for _, e := range outbox {
		if e.TaskID == acc.TaskID && task.Status(e.EventType).Terminal() {
			terminal++
		}
	}
	assert.Equal(t, 1, terminal, "exactly one terminal outbox row for the hidden task")

	// Public control plane denies the hidden task in every state.
	_, err = mgr.Status(t.Context(), parent.ID, acc.TaskID)
	require.ErrorIs(t, err, task.ErrNotFound)
	_, _, err = mgr.Output(t.Context(), parent.ID, acc.TaskID)
	require.ErrorIs(t, err, task.ErrNotFound)
	require.ErrorIs(t, mgr.Cancel(t.Context(), parent.ID, acc.TaskID), task.ErrNotFound)
	_, err = mgr.AppendMessage(t.Context(), task.MessageRequest{
		OwnerSessionID: parent.ID, TaskID: acc.TaskID, Origin: task.OriginUser, Prompt: "more",
	})
	require.ErrorIs(t, err, task.ErrNotFound)
	list, err := mgr.List(t.Context(), parent.ID, "")
	require.NoError(t, err)
	for _, tk := range list {
		require.NotEqual(t, acc.TaskID, tk.ID, "hidden tasks never appear in the public list")
	}

	// Internal diagnostics with the trusted workspace and owner reads
	// the durable bounded record; a foreign workspace or owner does
	// not.
	diag, err := mgr.DiagnosticTask(t.Context(), env.workingDir, parent.ID, acc.TaskID)
	require.NoError(t, err)
	assert.Equal(t, task.HiddenProfile, diag.Profile)
	assert.Equal(t, "fetched answer", diag.Result)
	assert.Equal(t, "call-fetch", diag.ToolCallID)
	assert.Equal(t, "msg-fetch", diag.ParentMessageID)
	_, err = mgr.DiagnosticTask(t.Context(), "other-workspace", parent.ID, acc.TaskID)
	require.ErrorIs(t, err, task.ErrNotFound)
	_, err = mgr.DiagnosticTask(t.Context(), env.workingDir, "intruder", acc.TaskID)
	require.ErrorIs(t, err, task.ErrNotOwner)

	// The child conversation transcript persists in the child
	// session created by the admission transaction.
	sess, err := env.sessions.Get(t.Context(), childID)
	require.NoError(t, err)
	assert.Equal(t, parent.ID, sess.ParentSessionID)
	msgs, err := env.messages.List(t.Context(), childID)
	require.NoError(t, err)
	assert.NotEmpty(t, msgs, "the hidden child keeps its transcript")
}

// TestAgenticFetch_ChildToolSet pins the fixed specialized fetch/web
// tool set: read and web tools only, with no delegation, task-control,
// question, or workspace-mutating tool.
func TestAgenticFetch_ChildToolSet(t *testing.T) {
	coord := profileTestCoordinator(t)

	got := make([]string, 0)
	for _, tool := range coord.buildFetchTools(t.TempDir(), nil) {
		got = append(got, tool.Info().Name)
	}
	want := slices.Clone(agenticFetchChildToolNames)
	slices.Sort(got)
	slices.Sort(want)
	assert.Equal(t, want, got, "buildFetchTools must match the fingerprinted tool set")

	assertNoDelegation(t, got)
	for _, forbidden := range []string{
		tools.QuestionToolName, tools.BashToolName, tools.WriteToolName, tools.EditToolName,
		AgentStatusToolName, AgentOutputToolName, AgentListToolName, AgentCancelToolName,
		AgentMessageToolName,
	} {
		assert.False(t, slices.Contains(got, forbidden), "hidden fetch children never see %s", forbidden)
	}
}

// TestAgenticFetch_RejectionsCreateNoSession proves admission-side
// guarantees: with no task manager and with exhausted quota the call
// is a model-visible rejection and leaves no child session behind;
// from child depth it is refused as delegation.
func TestAgenticFetch_RejectionsCreateNoSession(t *testing.T) {
	t.Run("no task manager", func(t *testing.T) {
		coord := profileTestCoordinator(t)
		parent, err := coord.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)
		ctx := delegationCtx(t, parent.ID, "msg-nofetch")

		resp := runFetchTool(t, coord, ctx, "call-nofetch", `{"prompt":"go"}`)
		require.True(t, resp.IsError)
		assert.Contains(t, resp.Content, "no task manager")
		_, err = coord.sessions.Get(t.Context(), coord.sessions.CreateAgentToolSessionID("msg-nofetch", "call-nofetch"))
		require.Error(t, err, "a rejected fetch must not create a child session")
	})

	t.Run("quota exhausted", func(t *testing.T) {
		gate := newGatedModel("slow fetch")
		coord, _, env, _ := fetchTaskEnv(t, task.Limits{LiveTasksPerParent: 1}, func(env fakeEnv, _ string) SessionAgent {
			return fakeChildAgent(env, gate)
		})
		parent, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)
		ctx := delegationCtx(t, parent.ID, "msg-fq")

		first := runFetchTool(t, coord, ctx, "call-fq1", `{"prompt":"one"}`)
		require.False(t, first.IsError, first.Content)
		select {
		case <-gate.entered:
		case <-time.After(15 * time.Second):
			t.Fatal("fetch child never reached the model")
		}

		second := runFetchTool(t, coord, ctx, "call-fq2", `{"prompt":"two"}`)
		require.True(t, second.IsError)
		assert.Contains(t, second.Content, task.ErrQuota.Error())
		_, err = env.sessions.Get(t.Context(), coord.sessions.CreateAgentToolSessionID("msg-fq", "call-fq2"))
		require.Error(t, err, "a failed hidden admission must leave no child session")
		close(gate.release)
	})

	t.Run("child depth", func(t *testing.T) {
		coord, mgr, env, _ := fetchTaskEnv(t, task.Limits{}, nil)
		parent, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)
		ctx := delegationCtx(t, parent.ID, "msg-fd")
		ctx = context.WithValue(ctx, tools.AgentDepthContextKey, 1)

		resp := runFetchTool(t, coord, ctx, "call-fd", `{"prompt":"nested fetch"}`)
		require.True(t, resp.IsError)
		assert.Contains(t, resp.Content, task.ErrDelegation.Error())
		_, err = mgr.List(t.Context(), parent.ID, "")
		require.NoError(t, err)
		_, err = env.sessions.Get(t.Context(), coord.sessions.CreateAgentToolSessionID("msg-fd", "call-fd"))
		require.Error(t, err, "depth rejection must not create a child session")
	})
}

// TestAgenticFetch_HiddenTaskCannotBeContinued proves the public
// call_agent path cannot resume a hidden child conversation: the
// continuation admission reports the hidden task as not found.
func TestAgenticFetch_HiddenTaskCannotBeContinued(t *testing.T) {
	coord, mgr, env, events := fetchTaskEnv(t, task.Limits{}, func(env fakeEnv, _ string) SessionAgent {
		return fakeChildAgent(env, &finishStreamModel{text: "done"})
	})
	coord.newChildAgent = func(_ context.Context, name, _ string) (SessionAgent, config.ResolvedProfile, error) {
		return fakeChildAgent(env, &finishStreamModel{text: "cont"}), config.ResolvedProfile{Name: name}, nil
	}
	parent, err := env.sessions.Create(t.Context(), "Parent")
	require.NoError(t, err)
	ctx := delegationCtx(t, parent.ID, "msg-hc")

	acc := acceptedOf(t, runFetchTool(t, coord, ctx, "call-hc", `{"prompt":"fetch it"}`))
	waitTerminalEvent(t, events, acc.TaskID)

	cont := runAgentTool(t, coord, ctx, "call-hc-cont", `{"prompt":"continue","task_id":"`+acc.TaskID+`"}`)
	require.True(t, cont.IsError)
	assert.Contains(t, cont.Content, task.ErrNotFound.Error())

	// And a public task's id cannot be laundered hidden: the hidden
	// profile stays out of the public listing throughout.
	list, err := mgr.List(t.Context(), parent.ID, "")
	require.NoError(t, err)
	assert.Empty(t, list)
}
