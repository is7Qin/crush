package taskquestion

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// PendingForOwner is the reconnect resync read: durable pending rows
// for exactly one owner session, never another's.

func TestPendingForOwner_UsesDurableRepo(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	svc := NewService(Config{Repo: repo})

	done := make(chan askResult, 1)
	go func() {
		a, err := svc.AskTask(context.Background(), askRequest("task-1"))
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

	mine, err := svc.PendingForOwner(t.Context(), "owner-1")
	require.NoError(t, err)
	require.Len(t, mine, 1)
	require.Equal(t, qid, mine[0].QuestionID)
	foreign, err := svc.PendingForOwner(t.Context(), "someone-else")
	require.NoError(t, err)
	require.Empty(t, foreign)

	require.NoError(t, svc.CancelTask("owner-1", qid))
	<-done
	mine, err = svc.PendingForOwner(t.Context(), "owner-1")
	require.NoError(t, err)
	require.Empty(t, mine, "a resolved question leaves the resync set")
}

func TestPendingForOwner_InMemoryFilter(t *testing.T) {
	t.Parallel()
	svc := NewService(Config{})
	done := make(chan askResult, 1)
	go func() {
		a, err := svc.AskTask(context.Background(), askRequest("task-1"))
		done <- askResult{a, err}
	}()
	require.Eventually(t, func() bool { return len(svc.Unresolved()) == 1 },
		2*time.Second, 5*time.Millisecond)

	mine, err := svc.PendingForOwner(t.Context(), "owner-1")
	require.NoError(t, err)
	require.Len(t, mine, 1)
	foreign, err := svc.PendingForOwner(t.Context(), "intruder")
	require.NoError(t, err)
	require.Empty(t, foreign)
	require.NoError(t, svc.CancelTask("owner-1", mine[0].QuestionID))
	<-done
}
