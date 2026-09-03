package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent/task"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/proto"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func runControlTool(t *testing.T, tool fantasy.AgentTool, ctx context.Context, input string) fantasy.ToolResponse {
	t.Helper()
	resp, err := tool.Run(ctx, fantasy.ToolCall{ID: "ctrl-" + tool.Info().Name, Name: tool.Info().Name, Input: input})
	require.NoError(t, err)
	return resp
}

func toolNames(tools []fantasy.AgentTool) []string {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Info().Name)
	}
	return names
}

// TestAgentStatusTool_SnapshotAndAuthorization proves the status tool
// reports the caller's own task and surfaces unknown and foreign ids as
// model-visible errors instead of run failures.
func TestAgentStatusTool_SnapshotAndAuthorization(t *testing.T) {
	gate := newGatedModel("slow answer")
	coord, mgr, env, events := taskToolEnv(t, task.Limits{}, func(env fakeEnv) SessionAgent {
		return fakeChildAgent(env, gate)
	})
	parent, err := env.sessions.Create(t.Context(), "Parent")
	require.NoError(t, err)
	other, err := env.sessions.Create(t.Context(), "Other")
	require.NoError(t, err)

	ctx := delegationCtx(t, parent.ID, "msg-status")
	dispatched := runAgentTool(t, coord, ctx, "call-status", `{"prompt":"go"}`)
	require.False(t, dispatched.IsError, dispatched.Content)
	taskID := taskIDFromResponse(t, dispatched.Content)

	tool := newAgentStatusTool(mgr)

	ok := runControlTool(t, tool, ctx, `{"task_id":"`+taskID+`"}`)
	require.False(t, ok.IsError, ok.Content)
	assert.Contains(t, ok.Content, "Task ID: "+taskID)
	assert.Contains(t, ok.Content, "Status: ")
	assert.Contains(t, ok.Content, "Profile: coder")

	unknown := runControlTool(t, tool, ctx, `{"task_id":"does-not-exist"}`)
	require.True(t, unknown.IsError)
	assert.Contains(t, unknown.Content, task.ErrNotFound.Error())

	foreign := runControlTool(t, tool, delegationCtx(t, other.ID, "msg-status-2"), `{"task_id":"`+taskID+`"}`)
	require.True(t, foreign.IsError)
	assert.Contains(t, foreign.Content, task.ErrNotOwner.Error())

	missing := runControlTool(t, tool, ctx, `{}`)
	require.True(t, missing.IsError)

	close(gate.release)
	waitTerminalEvent(t, events, taskID)
}

// TestAgentOutputTool_TruncationMetadata proves the output tool returns
// the stored result and reports truncation in the response metadata for
// both a capped and a short task.
func TestAgentOutputTool_TruncationMetadata(t *testing.T) {
	big := strings.Repeat("x", task.MaxResultBytes+1024)
	calls := 0
	coord, mgr, env, events := taskToolEnv(t, task.Limits{}, func(env fakeEnv) SessionAgent {
		calls++
		if calls == 1 {
			return fakeChildAgent(env, &finishStreamModel{text: big})
		}
		return fakeChildAgent(env, &finishStreamModel{text: "small answer"})
	})
	parent, err := env.sessions.Create(t.Context(), "Parent")
	require.NoError(t, err)
	ctx := delegationCtx(t, parent.ID, "msg-output")

	first := runAgentTool(t, coord, ctx, "call-out-1", `{"prompt":"big"}`)
	require.False(t, first.IsError, first.Content)
	firstID := taskIDFromResponse(t, first.Content)
	second := runAgentTool(t, coord, ctx, "call-out-2", `{"prompt":"small"}`)
	require.False(t, second.IsError, second.Content)
	secondID := taskIDFromResponse(t, second.Content)

	waitTerminalEvents(t, events, firstID, secondID)

	tool := newAgentOutputTool(mgr)

	truncated := runControlTool(t, tool, ctx, `{"task_id":"`+firstID+`"}`)
	require.False(t, truncated.IsError, truncated.Content)
	assert.True(t, strings.HasPrefix(truncated.Content, big[:task.MaxResultBytes]))
	assert.Contains(t, truncated.Content, "[output truncated")
	assert.Contains(t, truncated.Metadata, `"truncated":true`)

	short := runControlTool(t, tool, ctx, `{"task_id":"`+secondID+`"}`)
	require.False(t, short.IsError, short.Content)
	assert.Equal(t, "small answer", short.Content)
	assert.Contains(t, short.Metadata, `"truncated":false`)

	other, err := env.sessions.Create(t.Context(), "Other")
	require.NoError(t, err)
	denied := runControlTool(t, tool, delegationCtx(t, other.ID, "msg-output-3"), `{"task_id":"`+firstID+`"}`)
	require.True(t, denied.IsError)
	assert.Contains(t, denied.Content, task.ErrNotOwner.Error())
}

// TestAgentListTool_ScopedToOwner proves the list tool shows only the
// caller's tasks, oldest first.
func TestAgentListTool_ScopedToOwner(t *testing.T) {
	gate := newGatedModel("listed answer")
	coord, mgr, env, events := taskToolEnv(t, task.Limits{}, func(env fakeEnv) SessionAgent {
		return fakeChildAgent(env, gate)
	})
	parent, err := env.sessions.Create(t.Context(), "Parent")
	require.NoError(t, err)
	other, err := env.sessions.Create(t.Context(), "Other")
	require.NoError(t, err)

	ctx := delegationCtx(t, parent.ID, "msg-list")
	first := runAgentTool(t, coord, ctx, "call-list-1", `{"prompt":"one"}`)
	require.False(t, first.IsError, first.Content)
	firstID := taskIDFromResponse(t, first.Content)
	second := runAgentTool(t, coord, ctx, "call-list-2", `{"prompt":"two"}`)
	require.False(t, second.IsError, second.Content)
	secondID := taskIDFromResponse(t, second.Content)

	listed := runControlTool(t, newAgentListTool(mgr), ctx, `{}`)
	require.False(t, listed.IsError, listed.Content)
	assert.Contains(t, listed.Content, firstID)
	assert.Contains(t, listed.Content, secondID)
	assert.Less(t,
		strings.Index(listed.Content, firstID),
		strings.Index(listed.Content, secondID),
		"tasks must be listed oldest first",
	)

	foreign := runControlTool(t, newAgentListTool(mgr), delegationCtx(t, other.ID, "msg-list-2"), `{}`)
	require.False(t, foreign.IsError, foreign.Content)
	assert.Equal(t, "no agent tasks", foreign.Content)

	close(gate.release)
	terms := waitTerminalEvents(t, events, firstID, secondID)
	assert.Equal(t, task.StatusCompleted, terms[firstID].Status)
	assert.Equal(t, task.StatusCompleted, terms[secondID].Status)
}

// TestAgentCancelTool_PendingAndErrors proves cancellation of a queued
// (pending) task terminalizes it as cancelled without waiting on the
// running sibling, and that unknown/foreign ids are model-visible
// errors.
func TestAgentCancelTool_PendingAndErrors(t *testing.T) {
	gate := newGatedModel("slow answer")
	coord, mgr, env, events := taskToolEnv(t, task.Limits{RunningPerModel: 1}, func(env fakeEnv) SessionAgent {
		return fakeChildAgent(env, gate)
	})
	parent, err := env.sessions.Create(t.Context(), "Parent")
	require.NoError(t, err)
	other, err := env.sessions.Create(t.Context(), "Other")
	require.NoError(t, err)

	ctx := delegationCtx(t, parent.ID, "msg-cancel")
	running := runAgentTool(t, coord, ctx, "call-cancel-1", `{"prompt":"one"}`)
	require.False(t, running.IsError, running.Content)
	runningID := taskIDFromResponse(t, running.Content)

	select {
	case <-gate.entered:
	case <-time.After(15 * time.Second):
		t.Fatal("first child never reached the model")
	}

	queued := runAgentTool(t, coord, ctx, "call-cancel-2", `{"prompt":"two"}`)
	require.False(t, queued.IsError, queued.Content)
	queuedID := taskIDFromResponse(t, queued.Content)

	tool := newAgentCancelTool(mgr)

	cancelled := runControlTool(t, tool, ctx, `{"task_id":"`+queuedID+`"}`)
	require.False(t, cancelled.IsError, cancelled.Content)
	assert.Contains(t, cancelled.Content, "Task ID: "+queuedID)
	assert.Contains(t, cancelled.Content, "Status: "+string(task.StatusCancelled))

	terminal := waitTerminalEvent(t, events, queuedID)
	assert.Equal(t, task.StatusCancelled, terminal.Status)

	unknown := runControlTool(t, tool, ctx, `{"task_id":"does-not-exist"}`)
	require.True(t, unknown.IsError)
	assert.Contains(t, unknown.Content, task.ErrNotFound.Error())

	foreign := runControlTool(t, tool, delegationCtx(t, other.ID, "msg-cancel-3"), `{"task_id":"`+runningID+`"}`)
	require.True(t, foreign.IsError)
	assert.Contains(t, foreign.Content, task.ErrNotOwner.Error())

	close(gate.release)
	waitTerminalEvent(t, events, runningID)
}

// TestBuildTools_TaskControlVisibility proves the control tools are
// built only for the primary agent and only when the wired task
// manager exposes the control plane.
func TestBuildTools_TaskControlVisibility(t *testing.T) {
	coord, _, _, _ := taskToolEnv(t, task.Limits{}, func(env fakeEnv) SessionAgent {
		return fakeChildAgent(env, &finishStreamModel{text: "unused"})
	})
	agentCfg := config.Agent{
		ID: config.AgentCoder,
		AllowedTools: []string{
			AgentStatusToolName, AgentOutputToolName, AgentListToolName,
			AgentCancelToolName, AgentMessageToolName,
		},
	}

	primary, err := coord.buildTools(t.Context(), agentCfg, false)
	require.NoError(t, err)
	for _, name := range agentCfg.AllowedTools {
		assert.Contains(t, toolNames(primary), name)
	}

	child, err := coord.buildTools(t.Context(), agentCfg, true)
	require.NoError(t, err)
	assert.Empty(t, toolNames(child), "children must never see task control tools")

	coord.tasks = nil
	legacy, err := coord.buildTools(t.Context(), agentCfg, false)
	require.NoError(t, err)
	assert.Empty(t, toolNames(legacy), "no control tools without a task manager")
}

// agentMessageAccepted decodes the typed AgentMessageAccepted metadata
// carried by an agent_message acceptance response.
func agentMessageAccepted(t *testing.T, resp fantasy.ToolResponse) proto.AgentMessageAccepted {
	t.Helper()
	require.False(t, resp.IsError, resp.Content)
	var acc proto.AgentMessageAccepted
	require.NoError(t, json.Unmarshal([]byte(resp.Metadata), &acc))
	return acc
}

// TestAgentMessageTool_QueuesAndAuthorizes proves the primary-only
// agent_message contract over the real manager mailbox: an accepted
// append names the derived child session and the FIFO sequence, never
// a synchronous attempt for a live task; identity comes only from the
// trusted caller context; attachments are stored as wire JSON array
// text; and missing input, unknown ids, foreign owners, and a missing
// session context are rejected.
func TestAgentMessageTool_QueuesAndAuthorizes(t *testing.T) {
	gate := newGatedModel("slow answer")
	coord, mgr, env, events := taskToolEnv(t, task.Limits{}, func(env fakeEnv) SessionAgent {
		return fakeChildAgent(env, gate)
	})
	parent, err := env.sessions.Create(t.Context(), "Parent")
	require.NoError(t, err)
	other, err := env.sessions.Create(t.Context(), "Other")
	require.NoError(t, err)

	ctx := delegationCtx(t, parent.ID, "msg-message")
	dispatched := runAgentTool(t, coord, ctx, "call-msg-1", `{"prompt":"go"}`)
	require.False(t, dispatched.IsError, dispatched.Content)
	taskID := taskIDFromResponse(t, dispatched.Content)
	childID := childSessionIDFromResponse(t, dispatched.Content)
	select {
	case <-gate.entered:
	case <-time.After(15 * time.Second):
		t.Fatal("child never reached the model")
	}

	tool := newAgentMessageTool(mgr)

	// Appending to a live task queues at the next turn boundary: the
	// acceptance names the derived child and the sequence, and creates
	// no immediate attempt.
	first := agentMessageAccepted(t, runControlTool(t, tool, ctx, `{"task_id":"`+taskID+`","prompt":"first follow-up"}`))
	assert.Equal(t, taskID, first.TaskID)
	assert.Equal(t, childID, first.ChildSessionID)
	assert.Equal(t, uint64(1), first.Sequence)
	assert.Empty(t, first.AttemptTaskID, "a running task defers the successor to the turn boundary")
	assert.Equal(t, string(task.StatusRunning), first.Status)

	// FIFO: the next append takes the next sequence.
	second := agentMessageAccepted(t, runControlTool(t, tool, ctx, `{"task_id":"`+taskID+`","prompt":"second follow-up"}`))
	assert.Equal(t, uint64(2), second.Sequence)

	// Attachments are stored verbatim as the JSON array wire text.
	att := agentMessageAccepted(t, runControlTool(t, tool, ctx,
		`{"task_id":"`+taskID+`","prompt":"see the file","attachments":[{"file_path":"notes/a.txt","file_name":"a.txt","mime_type":"text/plain","content":"aGk="}]}`))
	msgs, err := mgr.Store().ListChildMessages(t.Context(), childID)
	require.NoError(t, err)
	var row task.ChildMessage
	for _, m := range msgs {
		if m.Sequence == att.Sequence {
			row = m
		}
	}
	require.Equal(t, "see the file", row.Prompt, "the attached message row is missing")
	assert.Equal(t, task.OriginParent, row.Origin, "agent appends carry the parent origin")
	var stored []proto.Attachment
	require.NoError(t, json.Unmarshal([]byte(row.Attachments), &stored))
	require.Len(t, stored, 1)
	assert.Equal(t, proto.Attachment{
		FilePath: "notes/a.txt", FileName: "a.txt", MimeType: "text/plain", Content: []byte("hi"),
	}, stored[0])

	// Invalid input is model-visible.
	missingPrompt := runControlTool(t, tool, ctx, `{"task_id":"`+taskID+`"}`)
	require.True(t, missingPrompt.IsError)
	assert.Contains(t, missingPrompt.Content, "missing prompt")

	missingTask := runControlTool(t, tool, ctx, `{"prompt":"x"}`)
	require.True(t, missingTask.IsError)
	assert.Contains(t, missingTask.Content, "missing task_id")

	// Unknown and foreign addresses are rejected without touching the
	// mailbox; the owner identity is the context session, never input.
	unknown := runControlTool(t, tool, ctx, `{"task_id":"does-not-exist","prompt":"x"}`)
	require.True(t, unknown.IsError)
	assert.Contains(t, unknown.Content, task.ErrNotFound.Error())

	foreign := runControlTool(t, tool, delegationCtx(t, other.ID, "msg-message-2"), `{"task_id":"`+taskID+`","prompt":"x"}`)
	require.True(t, foreign.IsError)
	assert.Contains(t, foreign.Content, task.ErrNotOwner.Error())

	_, err = tool.Run(context.Background(), fantasy.ToolCall{
		ID: "no-ctx", Name: AgentMessageToolName, Input: `{"task_id":"` + taskID + `","prompt":"x"}`,
	})
	require.Error(t, err, "a caller without trusted session context fails the run")

	close(gate.release)
	waitTerminalEvent(t, events, taskID)
}

// TestAgentMessageTool_TerminalSuccessor proves the acceptance names
// the successor attempt created immediately when a message is appended
// to a terminal task: a fresh task id on the retained child session,
// with the addressed task's terminal status reported as-is.
func TestAgentMessageTool_TerminalSuccessor(t *testing.T) {
	coord, mgr, env, events := taskToolEnv(t, task.Limits{}, func(env fakeEnv) SessionAgent {
		return fakeChildAgent(env, &finishStreamModel{text: "first answer"})
	})
	parent, err := env.sessions.Create(t.Context(), "Parent")
	require.NoError(t, err)

	ctx := delegationCtx(t, parent.ID, "msg-msgterm")
	dispatched := runAgentTool(t, coord, ctx, "call-msg-term", `{"prompt":"go"}`)
	require.False(t, dispatched.IsError, dispatched.Content)
	taskID := taskIDFromResponse(t, dispatched.Content)
	waitTerminalEvent(t, events, taskID)

	acc := agentMessageAccepted(t, runControlTool(t, newAgentMessageTool(mgr), ctx,
		`{"task_id":"`+taskID+`","prompt":"after terminal"}`))
	assert.Equal(t, taskID, acc.TaskID)
	assert.Equal(t, string(task.StatusCompleted), acc.Status, "the addressed task status is reported as-is")
	require.NotEmpty(t, acc.AttemptTaskID, "appending to a terminal task creates the successor attempt immediately")
	require.NotEqual(t, taskID, acc.AttemptTaskID, "the successor runs under a fresh task id")
	assert.Contains(t, runControlTool(t, newAgentListTool(mgr), ctx, `{}`).Content, acc.AttemptTaskID,
		"the successor attempt is a listed task of the owner")

	waitTerminalEvent(t, events, acc.AttemptTaskID)
}
