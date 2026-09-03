package tools

import (
	"context"
	"testing"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent/taskquestion"
	"github.com/charmbracelet/crush/internal/question"
	"github.com/stretchr/testify/require"
)

// recordingTransport captures the AskTask request and replays a fixed
// outcome so tests can assert the trusted identity the tool forwards.
type recordingTransport struct {
	req     taskquestion.TaskQuestionRequest
	answers []question.Answer
	err     error
	called  bool
}

func (r *recordingTransport) AskTask(_ context.Context, req taskquestion.TaskQuestionRequest) ([]question.Answer, error) {
	r.called = true
	r.req = req
	return r.answers, r.err
}

func taskQuestionCtx(t *testing.T) context.Context {
	t.Helper()
	ctx := context.WithValue(t.Context(), SessionIDContextKey, "child-1")
	return WithTaskQuestionContext(ctx, TaskQuestionContext{
		TaskID:         "task-1",
		OwnerSessionID: "owner-1",
		ChildSessionID: "child-1",
		RunGeneration:  3,
	})
}

const taskQuestionInput = `{"questions":[{"type":"yes_no","question":"Proceed?","description":"Confirm the step."}]}`

func TestTaskQuestionTool_ForwardsTrustedIdentityAndFormatsAnswers(t *testing.T) {
	t.Parallel()
	yes := true
	svc := &recordingTransport{answers: []question.Answer{{QuestionID: "q1", Yes: &yes}}}

	resp, err := NewTaskQuestionTool(svc).Run(
		taskQuestionCtx(t),
		fantasy.ToolCall{ID: "tc-9", Name: QuestionToolName, Input: taskQuestionInput},
	)
	require.NoError(t, err)
	require.False(t, resp.IsError, resp.Content)
	require.True(t, svc.called)

	// Identity comes from the trusted context, not the model input.
	require.Equal(t, "task-1", svc.req.TaskID)
	require.Equal(t, "owner-1", svc.req.OwnerSessionID)
	require.Equal(t, "child-1", svc.req.ChildSessionID)
	require.Equal(t, uint64(3), svc.req.RunGeneration)
	require.Equal(t, "child-1", svc.req.Batch.SessionID)
	require.Equal(t, "tc-9", svc.req.Batch.ToolCallID)
	require.Equal(t, "Proceed?", svc.req.Batch.Questions[0].Text)

	// Answer formatting matches the primary tool.
	require.Equal(t, "Q1: Proceed?\nUser answered: yes", resp.Content)
}

func TestTaskQuestionTool_WithoutTaskContextFailsClosed(t *testing.T) {
	t.Parallel()
	svc := &recordingTransport{}
	ctx := context.WithValue(t.Context(), SessionIDContextKey, "child-1")

	resp, err := NewTaskQuestionTool(svc).Run(
		ctx,
		fantasy.ToolCall{ID: "tc", Name: QuestionToolName, Input: taskQuestionInput},
	)
	require.NoError(t, err)
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, taskquestion.ErrTransportUnavailable.Error())
	require.False(t, svc.called, "no task context must never reach the transport")
}

func TestTaskQuestionTool_CancelStopsTurn(t *testing.T) {
	t.Parallel()
	svc := &recordingTransport{err: question.ErrCancelled}

	resp, err := NewTaskQuestionTool(svc).Run(
		taskQuestionCtx(t),
		fantasy.ToolCall{ID: "tc", Name: QuestionToolName, Input: taskQuestionInput},
	)
	require.NoError(t, err)
	require.True(t, resp.IsError)
	require.True(t, resp.StopTurn)
	require.Contains(t, resp.Content, "cancelled")
}

func TestTaskQuestionTool_InvalidParamsRejectedBeforeTransport(t *testing.T) {
	t.Parallel()
	svc := &recordingTransport{}

	resp, err := NewTaskQuestionTool(svc).Run(
		taskQuestionCtx(t),
		fantasy.ToolCall{ID: "tc", Name: QuestionToolName, Input: `{"questions":[]}`},
	)
	require.NoError(t, err)
	require.True(t, resp.IsError)
	require.False(t, svc.called)
}
