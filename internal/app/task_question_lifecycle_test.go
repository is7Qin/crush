package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/agent/task"
	"github.com/charmbracelet/crush/internal/agent/taskquestion"
	"github.com/charmbracelet/crush/internal/question"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestTaskQuestionLifecycle_BackgroundChildAnswer drives the whole
// production path: a background child task asks, the durable
// question+waiting-task pair is observable before publication, a
// foreign owner cannot change anything, a direct child message never
// implicitly resumes the attempt, and the owner answering by
// question id commits the answer, resumes the same attempt, and lets
// the child complete.
func TestTaskQuestionLifecycle_BackgroundChildAnswer(t *testing.T) {
	t.Parallel()
	env := newQuestionTestEnv(t)
	events := env.svc.Subscribe(t.Context())

	taskID, settled := startAskingTask(t, env, 0)
	batch, q := committedQuestion(t, env, events, taskID)

	require.Equal(t, "owner", q.OwnerSessionID)
	require.NotEmpty(t, q.ChildSessionID)
	require.Equal(t, uint64(1), q.RunGeneration)
	require.NotEmpty(t, batch.ID)
	status, answers, resolved := queryQuestionRow(t, env.conn, q.QuestionID)
	require.Equal(t, "pending", status)
	require.Empty(t, answers)
	require.False(t, resolved)

	// A foreign owner is rejected and durable state is untouched.
	yes := true
	want := []question.Answer{{QuestionID: batch.Questions[0].ID, Yes: &yes}}
	require.ErrorIs(t, env.svc.AnswerTask("intruder", q.QuestionID, want), taskquestion.ErrNotOwner)
	require.ErrorIs(t, env.svc.CancelTask("intruder", q.QuestionID), taskquestion.ErrNotOwner)
	require.Equal(t, "waiting_for_input", queryTaskStatus(t, env.conn, taskID))
	status, _, _ = queryQuestionRow(t, env.conn, q.QuestionID)
	require.Equal(t, "pending", status)

	// A direct child message queues for later: it never answers the
	// question and never resumes the waiting attempt.
	_, err := env.mgr.AppendMessage(t.Context(), task.MessageRequest{
		OwnerSessionID: "owner",
		TaskID:         taskID,
		Origin:         task.OriginParent,
		Prompt:         "hurry up",
	})
	require.NoError(t, err)
	require.Equal(t, "waiting_for_input", queryTaskStatus(t, env.conn, taskID))
	select {
	case <-settled:
		t.Fatal("a direct message must not wake the blocked question")
	default:
	}

	// The owner answers by question id; the answer commits durably
	// before the runner continues, and the same attempt resumes.
	require.NoError(t, env.svc.AnswerTask("owner", q.QuestionID, want))
	select {
	case err := <-settled:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("runner never woke after the committed answer")
	}
	status, storedAnswers, resolvedAt := queryQuestionRow(t, env.conn, q.QuestionID)
	require.Equal(t, "answered", status)
	require.Contains(t, storedAnswers, "yes")
	require.True(t, resolvedAt)

	require.Eventually(t, func() bool {
		return queryTaskStatus(t, env.conn, taskID) == "completed"
	}, 10*time.Second, 10*time.Millisecond, "the resumed child should complete")

	// The answered question left the resync set for every owner.
	mine, err := env.svc.PendingForOwner(t.Context(), "owner")
	require.NoError(t, err)
	require.Empty(t, mine)
	foreign, err := env.svc.PendingForOwner(t.Context(), "intruder")
	require.NoError(t, err)
	require.Empty(t, foreign)
}

// TestTaskQuestionLifecycle_TimeoutFailsTaskWithReason proves the
// timeout race outcome end to end: the question commits timed_out
// and the same transaction fails the task with the stable
// task_question_timeout reason; no resume happens.
func TestTaskQuestionLifecycle_TimeoutFailsTaskWithReason(t *testing.T) {
	t.Parallel()
	env := newQuestionTestEnv(t)
	events := env.svc.Subscribe(t.Context())

	taskID, settled := startAskingTask(t, env, 60*time.Millisecond)
	_, q := committedQuestion(t, env, events, taskID)

	select {
	case err := <-settled:
		require.ErrorIs(t, err, taskquestion.ErrTimeout)
	case <-time.After(10 * time.Second):
		t.Fatal("timed-out runner never settled")
	}
	status, _, resolved := queryQuestionRow(t, env.conn, q.QuestionID)
	require.Equal(t, "timed_out", status)
	require.True(t, resolved)
	require.Equal(t, "failed", queryTaskStatus(t, env.conn, taskID))
	require.Contains(t, queryTaskError(t, env.conn, taskID), task.ReasonTaskQuestionTimeout)
}

// TestTaskQuestionLifecycle_CancelRacesShutdown proves exactly-once
// across an owner cancellation and service shutdown: the runner
// settles once, the question row holds one terminal resolution, and
// the task terminalizes with the winner's state.
func TestTaskQuestionLifecycle_CancelRacesShutdown(t *testing.T) {
	t.Parallel()
	env := newQuestionTestEnv(t)
	events := env.svc.Subscribe(t.Context())

	taskID, settled := startAskingTask(t, env, 0)
	_, q := committedQuestion(t, env, events, taskID)

	go env.svc.Shutdown()
	cancelErr := env.svc.CancelTask("owner", q.QuestionID)
	if cancelErr != nil {
		require.True(t,
			errors.Is(cancelErr, taskquestion.ErrAlreadyResolved) ||
				errors.Is(cancelErr, taskquestion.ErrPersistenceFailed),
			"unexpected cancel outcome: %v", cancelErr)
	}

	select {
	case err := <-settled:
		require.True(t,
			errors.Is(err, question.ErrCancelled) || errors.Is(err, taskquestion.ErrShuttingDown),
			"unexpected runner outcome: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("runner hung through the cancel/shutdown race")
	}
	status, _, resolved := queryQuestionRow(t, env.conn, q.QuestionID)
	require.Contains(t, []string{"cancelled", "interrupted"}, status)
	require.True(t, resolved)
	require.Contains(t, []string{"cancelled", "interrupted"},
		queryTaskStatus(t, env.conn, taskID),
		"the task terminalizes with the winning resolution")

	// The losing control never re-commits a resolution.
	require.ErrorIs(t, env.svc.AnswerTask("owner", q.QuestionID,
		[]question.Answer{{QuestionID: "x", Yes: &[]bool{true}[0]}}), taskquestion.ErrAlreadyResolved)
}

// TestTaskQuestionLifecycle_RestartInterruptsPending proves the
// restart invariant: a pending question beside a waiting task from
// an earlier process resolves as interrupted in the same terminal
// transaction that interrupts the task, so the later pending sweep
// finds nothing and a reconnecting client can read but never answer
// the record.
func TestTaskQuestionLifecycle_RestartInterruptsPending(t *testing.T) {
	t.Parallel()
	env := newQuestionTestEnv(t)

	// Hand-build the crashed-process durable state: a waiting task
	// and its pending question, bound through the production bridge.
	blocked := make(chan struct{})
	accepted, err := env.mgr.Start(context.Background(), task.StartRequest{
		CallerSessionID: "owner",
		Prompt:          "hang",
		Provider:        "p",
		Model:           "m",
		ChildSessionID:  uuid.NewString(),
		Run: func(context.Context, *task.Handle) (task.Result, error) {
			<-blocked
			return task.Result{}, context.Canceled
		},
	})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		return queryTaskStatus(t, env.conn, accepted.ID) == "running"
	}, 10*time.Second, 10*time.Millisecond)
	require.NoError(t, newTaskQuestionLifecycle(env.mgr).BeginWait(t.Context(), taskquestion.TaskQuestion{
		QuestionID:     "q-crash",
		TaskID:         accepted.ID,
		OwnerSessionID: "owner",
		ChildSessionID: accepted.ChildSessionID,
		RunGeneration:  accepted.RunGeneration,
		Batch:          testQuestionBatch(),
		CreatedAt:      time.Now().UTC(),
	}))
	require.Equal(t, "waiting_for_input", queryTaskStatus(t, env.conn, accepted.ID))
	status, _, _ := queryQuestionRow(t, env.conn, "q-crash")
	require.Equal(t, "pending", status)
	t.Cleanup(func() { close(blocked) })

	// A fresh process: the recovery terminal transaction interrupts
	// the task, and must interrupt its pending question with it.
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	mgr2 := task.New(ctx, task.Config{
		WorkspaceID: env.dataDir,
		Store:       task.NewSQLiteStore(env.conn),
	})
	svc2 := taskquestion.NewService(taskquestion.Config{
		Repo:      taskquestion.NewSQLiteRepository(env.conn),
		Lifecycle: newTaskQuestionLifecycle(mgr2),
	})
	recovered, err := mgr2.RecoverLiveTasks(ctx)
	require.NoError(t, err)
	require.Len(t, recovered, 1)
	require.Equal(t, task.StatusInterrupted, recovered[0].Status)

	status, _, resolved := queryQuestionRow(t, env.conn, "q-crash")
	require.Equal(t, "interrupted", status,
		"the recovery terminal transaction must resolve the pending question itself")
	require.True(t, resolved)
	require.Equal(t, "interrupted", queryTaskStatus(t, env.conn, accepted.ID))

	// The service-side sweep adds nothing: recovery was atomic.
	n, err := svc2.InterruptStale(ctx)
	require.NoError(t, err)
	require.Zero(t, n)

	// Reconnect lists nothing pending and can never answer the
	// resolved record.
	mine, err := svc2.PendingForOwner(ctx, "owner")
	require.NoError(t, err)
	require.Empty(t, mine)
	require.ErrorIs(t, svc2.AnswerTask("owner", "q-crash",
		[]question.Answer{{QuestionID: "x", Yes: &[]bool{true}[0]}}), taskquestion.ErrNotFound)
}
