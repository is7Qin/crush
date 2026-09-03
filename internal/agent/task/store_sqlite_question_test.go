package task

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/stretchr/testify/require"
)

// connectQuestionStore opens a fresh SQLite store for question
// transaction tests.
func connectQuestionStore(t *testing.T) (*SQLiteStore, *sql.DB) {
	t.Helper()
	dataDir := t.TempDir()
	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
	})
	return NewSQLiteStore(conn), conn
}

func runningQuestionRow(taskID, questionID string) QuestionRow {
	return QuestionRow{
		QuestionID:     questionID,
		TaskID:         taskID,
		OwnerSessionID: "owner",
		ChildSessionID: "child",
		RunGeneration:  3,
		Batch:          `{"questions":[{"type":"yes_no","question":"Proceed?"}]}`,
		CreatedAt:      time.Unix(1_700_000_020, 0).UTC(),
	}
}

func TestSQLiteStore_BeginQuestionWaitCommitsBothRowsAtomically(t *testing.T) {
	t.Parallel()
	store, conn := connectQuestionStore(t)
	ctx := context.Background()
	task := sqliteTask()
	require.NoError(t, store.Save(ctx, task))

	committed, err := store.BeginQuestionWait(ctx, runningQuestionRow(task.ID, "q1"))
	require.NoError(t, err)
	require.Equal(t, StatusWaitingForInput, committed.Status)

	// Both halves are visible in one read of the database: no
	// observer sees a pending question beside a running task.
	var taskStatus, questionStatus string
	require.NoError(t, conn.QueryRowContext(ctx,
		`SELECT t.status, q.status FROM agent_tasks t
		JOIN agent_task_questions q ON q.task_id = t.id
		WHERE q.question_id = 'q1'`,
	).Scan(&taskStatus, &questionStatus))
	require.Equal(t, "waiting_for_input", taskStatus)
	require.Equal(t, "pending", questionStatus)

	// A second pending question for the same task is rejected by the
	// partial unique index, and the rejection leaves no row.
	_, err = store.BeginQuestionWait(ctx, runningQuestionRow(task.ID, "q2"))
	require.Error(t, err)
	var n int
	require.NoError(t, conn.QueryRowContext(ctx,
		`SELECT count(*) FROM agent_task_questions WHERE question_id = 'q2'`).Scan(&n))
	require.Zero(t, n)
}

func TestSQLiteStore_BeginQuestionWaitFenceFailureRollsBack(t *testing.T) {
	t.Parallel()
	store, conn := connectQuestionStore(t)
	ctx := context.Background()
	task := sqliteTask()
	task.Status = StatusWaitingForInput
	require.NoError(t, store.Save(ctx, task))

	// The task is not running: the wait fence rejects, and the
	// question insert inside the same transaction leaves nothing
	// behind.
	_, err := store.BeginQuestionWait(ctx, runningQuestionRow(task.ID, "q1"))
	require.ErrorIs(t, err, ErrInvalidTransition)
	var n int
	require.NoError(t, conn.QueryRowContext(ctx,
		`SELECT count(*) FROM agent_task_questions WHERE question_id = 'q1'`).Scan(&n))
	require.Zero(t, n, "a failed task transition must roll the question row back")
}

func TestSQLiteStore_TerminalizeQuestionFenceRollbackAndWin(t *testing.T) {
	t.Parallel()
	store, conn := connectQuestionStore(t)
	ctx := context.Background()
	task := sqliteTask()
	require.NoError(t, store.Save(ctx, task))
	_, err := store.BeginQuestionWait(ctx, runningQuestionRow(task.ID, "q1"))
	require.NoError(t, err)

	// A stale fence (question already resolved) rolls the whole
	// terminalization back: the task stays waiting.
	_, err = conn.ExecContext(ctx,
		`UPDATE agent_task_questions SET status = 'answered' WHERE question_id = 'q1'`)
	require.NoError(t, err)
	stale := runningOutcome(task.ID, "q1", "cancelled")
	_, won, err := store.TerminalizeAndDeliver(ctx, task.ID, task.RunGeneration,
		TerminalUpdate{Status: StatusCancelled, CompletedAt: time.Now(), Question: &stale})
	require.ErrorIs(t, err, ErrQuestionStale)
	require.False(t, won)
	var taskStatus string
	require.NoError(t, conn.QueryRowContext(ctx,
		`SELECT status FROM agent_tasks WHERE id = 't1'`).Scan(&taskStatus))
	require.Equal(t, "waiting_for_input", taskStatus, "rollback must not cancel the task")

	// Restore the pending window, then the matching fenced
	// terminalization wins in one commit.
	_, err = conn.ExecContext(ctx,
		`UPDATE agent_task_questions SET status = 'pending', resolved_at = NULL WHERE question_id = 'q1'`)
	require.NoError(t, err)
	fresh := runningOutcome(task.ID, "q1", "cancelled")
	committed, won, err := store.TerminalizeAndDeliver(ctx, task.ID, task.RunGeneration,
		TerminalUpdate{Status: StatusCancelled, CompletedAt: time.Now(), Question: &fresh})
	require.NoError(t, err)
	require.True(t, won)
	require.Equal(t, StatusCancelled, committed.Status)
	var questionStatus string
	require.NoError(t, conn.QueryRowContext(ctx,
		`SELECT status FROM agent_task_questions WHERE question_id = 'q1'`).Scan(&questionStatus))
	require.Equal(t, "cancelled", questionStatus)
}

func runningOutcome(taskID, questionID, resolution string) QuestionOutcome {
	return QuestionOutcome{
		QuestionID:     questionID,
		TaskID:         taskID,
		OwnerSessionID: "owner",
		ChildSessionID: "child",
		RunGeneration:  3,
		Resolution:     resolution,
		ResolvedAt:     time.Unix(1_700_000_030, 0).UTC(),
	}
}

// TestSQLiteStore_RecoveryTerminalizationInterruptsPendingQuestion
// proves the restart invariant end to end at the store: the same
// terminal transaction that recovery uses to mark the task
// interrupted also resolves its pending question as interrupted. No
// durable pending question survives recovery.
func TestSQLiteStore_RecoveryTerminalizationInterruptsPendingQuestion(t *testing.T) {
	t.Parallel()
	store, conn := connectQuestionStore(t)
	ctx := context.Background()
	task := sqliteTask()
	require.NoError(t, store.Save(ctx, task))
	_, err := store.BeginQuestionWait(ctx, runningQuestionRow(task.ID, "q1"))
	require.NoError(t, err)

	recovered, won, err := store.TerminalizeAndDeliver(ctx, task.ID, task.RunGeneration,
		TerminalUpdate{
			Status:      StatusInterrupted,
			Summary:     ReasonProcessRestart,
			CompletedAt: time.Now(),
		})
	require.NoError(t, err)
	require.True(t, won)
	require.Equal(t, StatusInterrupted, recovered.Status)

	var questionStatus string
	var resolvedAt sql.NullInt64
	require.NoError(t, conn.QueryRowContext(ctx,
		`SELECT status, resolved_at FROM agent_task_questions WHERE question_id = 'q1'`,
	).Scan(&questionStatus, &resolvedAt))
	require.Equal(t, "interrupted", questionStatus,
		"the recovery terminal transaction must stamp the pending question interrupted")
	require.True(t, resolvedAt.Valid)
}

func TestQuestionStatusUnknownIsNotFound(t *testing.T) {
	t.Parallel()
	store, _ := connectQuestionStore(t)
	_, err := store.QuestionStatus(context.Background(), "missing")
	require.True(t, errors.Is(err, ErrNotFound))
}
