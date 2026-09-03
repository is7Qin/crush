package taskquestion

import (
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/question"
	"github.com/stretchr/testify/require"
)

func connectRepo(t *testing.T, dataDir string) *SQLiteRepository {
	t.Helper()
	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
	})
	return NewSQLiteRepository(conn)
}

func storedQuestion(id, taskID string) TaskQuestion {
	return TaskQuestion{
		QuestionID:     id,
		TaskID:         taskID,
		OwnerSessionID: "owner-1",
		ChildSessionID: "child-1",
		RunGeneration:  2,
		Batch:          yesNoBatch(),
		Resolution:     ResolutionPending,
		CreatedAt:      time.Unix(1_700_000_000, 123).UTC(),
	}
}

func TestConnect_AppliesAgentTaskQuestionMigration(t *testing.T) {
	t.Cleanup(db.ResetPool)
	dataDir := t.TempDir()
	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)

	var name string
	err = conn.QueryRowContext(t.Context(),
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'agent_task_questions'`,
	).Scan(&name)
	require.NoError(t, err, "table missing after migrations")
	require.Equal(t, "agent_task_questions", name)
	require.NoError(t, db.Release(dataDir))
}

func TestSQLiteRepository_RoundTripsAllFields(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	repo := connectRepo(t, t.TempDir())

	want := storedQuestion("q1", "task-1")
	require.NoError(t, repo.Save(ctx, want))

	got, err := repo.ListUnresolved(ctx)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, want.QuestionID, got[0].QuestionID)
	require.Equal(t, want.TaskID, got[0].TaskID)
	require.Equal(t, want.OwnerSessionID, got[0].OwnerSessionID)
	require.Equal(t, want.ChildSessionID, got[0].ChildSessionID)
	require.Equal(t, want.RunGeneration, got[0].RunGeneration)
	require.Equal(t, want.Resolution, got[0].Resolution)
	require.True(t, want.CreatedAt.Equal(got[0].CreatedAt))
	require.Equal(t, want.Batch.Questions[0].Text, got[0].Batch.Questions[0].Text,
		"encoded batch must survive resync decode")
	require.Equal(t, want.Batch.Questions[0].Type, got[0].Batch.Questions[0].Type)
	require.Empty(t, got[0].Answers)
	require.True(t, got[0].ResolvedAt.IsZero())
}

func TestSQLiteRepository_ResolveWinsOnceAndStoresAnswers(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	repo := connectRepo(t, t.TempDir())

	require.NoError(t, repo.Save(ctx, storedQuestion("q1", "task-1")))

	yes := true
	at := time.Unix(1_700_000_050, 0).UTC()
	u := ResolutionUpdate{
		Resolution: ResolutionAnswered,
		Answers:    []question.Answer{{QuestionID: "qq", Yes: &yes}},
		ResolvedAt: at,
	}
	won, err := repo.Resolve(ctx, "q1", u)
	require.NoError(t, err)
	require.True(t, won)

	// A conflicting second resolution loses and cannot rewrite.
	won, err = repo.Resolve(ctx, "q1", ResolutionUpdate{Resolution: ResolutionCancelled})
	require.NoError(t, err)
	require.False(t, won)

	un, err := repo.ListUnresolved(ctx)
	require.NoError(t, err)
	require.Empty(t, un, "resolved rows leave the pending set")

	// The winner's payload is durably stored, readable straight from
	// the row the conditional UPDATE committed.
	var (
		status     string
		answers    string
		resolvedAt sql.NullInt64
	)
	require.NoError(t, repo.db.QueryRowContext(ctx,
		`SELECT status, answers, resolved_at FROM agent_task_questions WHERE question_id = 'q1'`,
	).Scan(&status, &answers, &resolvedAt))
	require.Equal(t, string(ResolutionAnswered), status)
	require.True(t, resolvedAt.Valid)
	require.Equal(t, u.ResolvedAt.UnixNano(), resolvedAt.Int64)
	var stored []question.Answer
	require.NoError(t, json.Unmarshal([]byte(answers), &stored))
	require.Equal(t, u.Answers, stored)
}

func TestSQLiteRepository_EnforcesOnePendingPerTask(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	repo := connectRepo(t, t.TempDir())

	require.NoError(t, repo.Save(ctx, storedQuestion("q1", "task-1")))
	err := repo.Save(ctx, storedQuestion("q2", "task-1"))
	require.Error(t, err, "partial unique index must reject a second pending row")

	// After the first resolves, the task may hold a new pending row.
	_, err = repo.Resolve(ctx, "q1", ResolutionUpdate{
		Resolution: ResolutionAnswered,
		ResolvedAt: time.Unix(1_700_000_050, 0).UTC(),
	})
	require.NoError(t, err)
	require.NoError(t, repo.Save(ctx, storedQuestion("q2", "task-1")))
}

func TestSQLiteRepository_ListUnresolvedForOwner(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	repo := connectRepo(t, t.TempDir())

	mine := storedQuestion("q-mine", "task-1")
	foreign := storedQuestion("q-other", "task-2")
	foreign.OwnerSessionID = "owner-2"
	require.NoError(t, repo.Save(ctx, mine))
	require.NoError(t, repo.Save(ctx, foreign))

	got, err := repo.ListUnresolvedForOwner(ctx, "owner-1")
	require.NoError(t, err)
	require.Len(t, got, 1, "only the owner's pending rows are listed")
	require.Equal(t, "q-mine", got[0].QuestionID)

	// Resolving the owner's row empties their resync set.
	_, err = repo.Resolve(ctx, "q-mine", ResolutionUpdate{
		Resolution: ResolutionAnswered,
		ResolvedAt: time.Unix(1_700_000_050, 0).UTC(),
	})
	require.NoError(t, err)
	got, err = repo.ListUnresolvedForOwner(ctx, "owner-1")
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestServiceWithSQLiteRepository_EndToEnd(t *testing.T) {
	t.Parallel()
	repo := connectRepo(t, t.TempDir())
	svc := NewService(Config{Repo: repo})

	done := make(chan askResult, 1)
	go func() {
		a, err := svc.AskTask(t.Context(), askRequest("task-1"))
		done <- askResult{a, err}
	}()

	var qid string
	require.Eventually(t, func() bool {
		un := svc.Unresolved()
		if len(un) == 0 {
			return false
		}
		qid = un[0].QuestionID
		return true
	}, 2*time.Second, 5*time.Millisecond)

	yes := true
	want := []question.Answer{{Yes: &yes}}
	require.NoError(t, svc.AnswerTask("owner-1", qid, want))
	require.NoError(t, (<-done).err)

	un, err := repo.ListUnresolved(t.Context())
	require.NoError(t, err)
	require.Empty(t, un, "durable mirror agrees with the in-memory winner")
}
