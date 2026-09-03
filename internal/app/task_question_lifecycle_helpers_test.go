package app

import (
	"context"
	"database/sql"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/agent/task"
	"github.com/charmbracelet/crush/internal/agent/taskquestion"
	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/charmbracelet/crush/internal/question"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// questionTestEnv wires the production combination: a real task
// manager over the shared SQLite store with a taskquestion service
// driven by the App-owned lifecycle bridge, never in-memory
// callbacks.
type questionTestEnv struct {
	dataDir string
	conn    *sql.DB
	mgr     *task.Manager
	svc     taskquestion.TaskQuestionService
}

func newQuestionTestEnv(t *testing.T) *questionTestEnv {
	t.Helper()
	dataDir := t.TempDir()
	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	env := &questionTestEnv{dataDir: dataDir, conn: conn}
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
	})
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	env.mgr = task.New(ctx, task.Config{
		WorkspaceID: dataDir,
		Store:       task.NewSQLiteStore(conn),
	})
	env.svc = taskquestion.NewService(taskquestion.Config{
		Repo:      taskquestion.NewSQLiteRepository(conn),
		Lifecycle: newTaskQuestionLifecycle(env.mgr),
	})
	return env
}

func testQuestionBatch() question.Request {
	return question.Request{
		ToolCallID: "tc-question",
		Questions: []question.Question{{
			Type:        question.TypeYesNo,
			Text:        "Proceed?",
			Description: "Confirm the step.",
		}},
	}
}

// startAskingTask admits a background task whose runner immediately
// asks one question through the production service, mirroring a
// unified call_agent child. Successor attempts (queued mailbox
// messages) complete without asking again so the test observes only
// the first attempt's question. It returns the task id and a channel
// reporting the first attempt's AskTask outcome.
func startAskingTask(
	t *testing.T,
	env *questionTestEnv,
	timeout time.Duration,
) (string, chan error) {
	t.Helper()
	settled := make(chan error, 1)
	entered := make(chan string, 1)
	var attempts atomic.Int32
	_, err := env.mgr.Start(context.Background(), task.StartRequest{
		CallerSessionID: "owner",
		Prompt:          "ask something",
		Provider:        "p",
		Model:           "m",
		ChildSessionID:  uuid.NewString(),
		Run: func(ctx context.Context, h *task.Handle) (task.Result, error) {
			if attempts.Add(1) > 1 {
				// Successor attempts (queued mailbox messages)
				// complete without asking: the test pins the first
				// attempt's question lifecycle only.
				return task.Result{Text: "successor done"}, nil
			}
			entered <- h.TaskID()
			answers, err := env.svc.AskTask(ctx, taskquestion.TaskQuestionRequest{
				TaskID:         h.TaskID(),
				OwnerSessionID: "owner",
				ChildSessionID: h.ChildSessionID(),
				RunGeneration:  h.RunGeneration(),
				Timeout:        timeout,
				Batch:          testQuestionBatch(),
			})
			if err != nil {
				settled <- err
				return task.Result{}, err
			}
			settled <- nil
			return task.Result{Text: "answered " + answers[0].QuestionID}, nil
		},
	})
	require.NoError(t, err)
	select {
	case id := <-entered:
		return id, settled
	case <-time.After(10 * time.Second):
		t.Fatal("child runner never entered AskTask")
		return "", nil
	}
}

// committedQuestion returns the batch event once it has been
// published, pinning that at that instant the question row is
// durable-pending and the task row is already waiting_for_input.
func committedQuestion(
	t *testing.T,
	env *questionTestEnv,
	events <-chan pubsub.Event[taskquestion.TaskQuestion],
	taskID string,
) (question.Request, taskquestion.TaskQuestion) {
	t.Helper()
	select {
	case ev := <-events:
		require.Equal(t, "waiting_for_input", queryTaskStatus(t, env.conn, taskID),
			"the task must be durably waiting before the question event is observable")
		pending, err := env.svc.PendingForOwner(t.Context(), "owner")
		require.NoError(t, err)
		require.Len(t, pending, 1, "the published question must already be durable-pending")
		require.Equal(t, taskID, pending[0].TaskID)
		return ev.Payload.Batch, pending[0]
	case <-time.After(10 * time.Second):
		t.Fatal("task question batch was never published")
		return question.Request{}, taskquestion.TaskQuestion{}
	}
}

func queryTaskStatus(t *testing.T, conn *sql.DB, taskID string) string {
	t.Helper()
	var status string
	require.NoError(t, conn.QueryRowContext(t.Context(),
		`SELECT status FROM agent_tasks WHERE id = ?`, taskID).Scan(&status))
	return status
}

func queryTaskError(t *testing.T, conn *sql.DB, taskID string) string {
	t.Helper()
	var errText string
	require.NoError(t, conn.QueryRowContext(t.Context(),
		`SELECT error FROM agent_tasks WHERE id = ?`, taskID).Scan(&errText))
	return errText
}

func queryQuestionRow(t *testing.T, conn *sql.DB, questionID string) (string, string, bool) {
	t.Helper()
	var status, answers string
	var resolvedAt sql.NullInt64
	require.NoError(t, conn.QueryRowContext(t.Context(),
		`SELECT status, answers, resolved_at FROM agent_task_questions WHERE question_id = ?`,
		questionID).Scan(&status, &answers, &resolvedAt))
	return status, answers, resolvedAt.Valid
}
