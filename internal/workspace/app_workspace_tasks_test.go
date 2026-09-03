package workspace

import (
	"context"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/agent/task"
	"github.com/charmbracelet/crush/internal/agent/taskquestion"
	"github.com/charmbracelet/crush/internal/app"
	"github.com/charmbracelet/crush/internal/proto"
	"github.com/charmbracelet/crush/internal/question"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestAppWorkspace_TaskControlLocalMode proves the local (in-process)
// Workspace implementation drives the same durable control plane:
// the owner identity is the tracked current session, messages are
// FIFO-sequenced, reads are owner-scoped, and question answers reach
// the task-question service by question id.
func TestAppWorkspace_TaskControlLocalMode(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	a := app.NewForTest(ctx)
	w := NewAppWorkspace(a, nil)

	// Without a current session there is no owner to authorize as.
	_, err := w.TaskList(ctx, "")
	require.ErrorIs(t, err, ErrNoCurrentSession)

	require.NoError(t, w.SetCurrentSession(ctx, "owner"))

	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	rec, err := a.Tasks().Start(ctx, task.StartRequest{
		CallerSessionID: "owner",
		ParentSessionID: "owner",
		ChildSessionID:  uuid.NewString(),
		ParentMessageID: "pm-1",
		ToolCallID:      "tc-1",
		Profile:         "coder",
		Provider:        "prov",
		Model:           "model",
		Prompt:          "work",
		Run: func(ctx context.Context, h *task.Handle) (task.Result, error) {
			entered <- struct{}{}
			select {
			case <-release:
				return task.Result{Text: "out"}, nil
			case <-ctx.Done():
				return task.Result{}, ctx.Err()
			}
		},
	})
	require.NoError(t, err)
	<-entered

	// Owner-scoped list and get.
	list, err := w.TaskList(ctx, "")
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.Equal(t, rec.ID, list[0].ID)

	snap, err := w.TaskGet(ctx, rec.ID)
	require.NoError(t, err)
	require.Equal(t, "tc-1", snap.ToolCallID)

	// Direct child message: accepted with FIFO sequence.
	acc, err := w.TaskSendMessage(ctx, rec.ID, "direct hi",
		proto.Attachment{FilePath: "/p", FileName: "p", MimeType: "text/plain", Content: []byte("x")})
	require.NoError(t, err)
	require.Equal(t, uint64(1), acc.Sequence)
	require.Equal(t, rec.ChildSessionID, acc.ChildSessionID)

	// A child question answered through the workspace path by id.
	answered := make(chan error, 1)
	go func() {
		_, err := a.TaskQuestions().AskTask(ctx, taskquestion.TaskQuestionRequest{
			TaskID: rec.ID, OwnerSessionID: "owner", ChildSessionID: rec.ChildSessionID, RunGeneration: rec.RunGeneration,
			Batch: question.Request{ToolCallID: "tc-q", Questions: []question.Question{{
				Type: question.TypeYesNo, Text: "Proceed?", Description: "d",
			}}},
		})
		answered <- err
	}()
	var q taskquestion.TaskQuestion
	deadline := time.Now().Add(10 * time.Second)
	for {
		if pend, err := w.TaskQuestionsPending(ctx); err == nil && len(pend) == 1 {
			q = taskquestion.TaskQuestion{QuestionID: pend[0].QuestionID}
			break
		}
		require.True(t, time.Now().Before(deadline), "pending question never surfaced")
		time.Sleep(5 * time.Millisecond)
	}
	require.True(t, w.TaskQuestionAnswer(q.QuestionID, []question.Answer{{QuestionID: "missing"}}),
		"answer routes by question id")

	close(release)
}
