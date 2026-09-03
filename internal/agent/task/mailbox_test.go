package task

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/stretchr/testify/require"
)

// childSeer reports whether a child session row is visible to
// readers, per store implementation.
type childSeer func(ctx context.Context, id string) bool

// eachDispatchStore runs fn against both Store implementations so
// the mailbox contract is proven identically in memory and SQLite.
func eachDispatchStore(t *testing.T, fn func(t *testing.T, s Store, childExists childSeer)) {
	t.Helper()

	t.Run("memory", func(t *testing.T) {
		t.Parallel()
		s := NewMemoryStore()
		fn(t, s, func(_ context.Context, id string) bool { return s.ChildSessionExists(id) })
	})
	t.Run("sqlite", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		conn, err := db.Connect(t.Context(), dir)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, db.Release(dir)) })
		s := NewSQLiteStore(conn)
		fn(t, s, func(ctx context.Context, id string) bool {
			var n int
			require.NoError(t, conn.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM sessions WHERE id = ?`, id).Scan(&n))
			return n > 0
		})
	})
}

func admissionFor(id, child, prompt string) Admission {
	now := time.Now()
	return Admission{
		Task: &Task{
			ID:              id,
			OwnerSessionID:  "owner",
			ParentSessionID: "parent",
			ChildSessionID:  child,
			ParentMessageID: "msg-" + id,
			ToolCallID:      "tc-" + id,
			Profile:         "coder",
			Provider:        "p",
			Model:           "m",
			RunGeneration:   1,
			Prompt:          prompt,
			Status:          StatusPending,
			CreatedAt:       now,
			UpdatedAt:       now,
		},
		Child: ChildSpec{ID: child, Title: "child session", ParentID: "parent", New: true},
	}
}

func requireMessages(t *testing.T, s Store, child string, want ...ChildMessage) {
	t.Helper()
	got, err := s.ListChildMessages(t.Context(), child)
	require.NoError(t, err)
	require.Len(t, got, len(want))
	for i, w := range want {
		require.Equal(t, w.Sequence, got[i].Sequence, "row %d sequence", i)
		require.Equal(t, w.State, got[i].State, "row %d state", i)
		require.Equal(t, w.Prompt, got[i].Prompt, "row %d prompt", i)
		if w.Reason != "" {
			require.Equal(t, w.Reason, got[i].Reason, "row %d reason", i)
		}
		if w.TaskID != "" {
			require.Equal(t, w.TaskID, got[i].TaskID, "row %d task", i)
		}
	}
}

func appendMsg(s Store, taskID, prompt string) ChildMessage {
	msg, err := s.AppendChildMessage(context.Background(), ChildMessage{
		TaskID: taskID, Origin: OriginUser, Prompt: prompt,
	})
	if err != nil {
		panic("appendMsg: " + err.Error())
	}
	return msg
}

func TestDispatchStore_AdmissionBindsChildAndQueuesSequenceZero(t *testing.T) {
	eachDispatchStore(t, func(t *testing.T, s Store, childExists childSeer) {
		ctx := t.Context()
		saved, err := s.CreatePendingTask(ctx, admissionFor("t1", "child-1", "initial"))
		require.NoError(t, err)
		require.Equal(t, "child-1", saved.ChildSessionID, "a committed pending task owns its child session")
		require.Equal(t, StatusPending, saved.Status)
		require.Equal(t, uint64(1), saved.RunGeneration)

		got, err := s.Get(ctx, "t1")
		require.NoError(t, err)
		require.Equal(t, "child-1", got.ChildSessionID)
		require.Equal(t, "msg-t1", got.ParentMessageID)
		require.Equal(t, "tc-t1", got.ToolCallID)
		require.True(t, childExists(ctx, "child-1"), "the sessions row committed with the task")

		requireMessages(t, s, "child-1", ChildMessage{
			Sequence: 0, State: MessageQueued, Prompt: "initial", TaskID: "t1",
		})
	})
}

func TestDispatchStore_FailedAdmissionLeavesNothingVisible(t *testing.T) {
	eachDispatchStore(t, func(t *testing.T, s Store, childExists childSeer) {
		ctx := t.Context()
		_, err := s.CreatePendingTask(ctx, admissionFor("t1", "child-1", "first"))
		require.NoError(t, err)

		// A second admission reusing the task id fails at the task
		// insert after the child session was created in the same
		// transaction; everything must roll back.
		dup := admissionFor("t1", "child-2", "collide")
		_, err = s.CreatePendingTask(ctx, dup)
		require.Error(t, err)

		_, err = s.Get(ctx, "t1")
		require.NoError(t, err, "the original task is untouched")
		require.False(t, childExists(ctx, "child-2"), "rolled-back admission leaves no child session")
		msgs, err := s.ListChildMessages(ctx, "child-2")
		require.NoError(t, err)
		require.Empty(t, msgs, "rolled-back admission leaves no mailbox row")
	})
}

func TestDispatchStore_AppendAllocatesFIFOSequences(t *testing.T) {
	eachDispatchStore(t, func(t *testing.T, s Store, _ childSeer) {
		ctx := t.Context()
		_, err := s.CreatePendingTask(ctx, admissionFor("t1", "child-1", "seq zero"))
		require.NoError(t, err)

		for i, prompt := range []string{"one", "two", "three"} {
			msg, err := s.AppendChildMessage(ctx, ChildMessage{
				TaskID: "t1", Origin: OriginParent, Prompt: prompt,
			})
			require.NoError(t, err)
			require.Equal(t, uint64(i+1), msg.Sequence)
			require.Equal(t, "child-1", msg.ChildSessionID, "child binding derives from the task")
			require.Equal(t, "owner", msg.OwnerSessionID)
			require.Equal(t, MessageQueued, msg.State)
		}

		_, err = s.AppendChildMessage(ctx, ChildMessage{TaskID: "nope", Prompt: "x"})
		require.ErrorIs(t, err, ErrNotFound)
		_, err = s.AppendChildMessage(ctx, ChildMessage{TaskID: "t1", Prompt: ""})
		require.ErrorIs(t, err, ErrInvalidRequest)
	})
}

func TestDispatchStore_DispatchPromotesPendingWithSequenceZero(t *testing.T) {
	eachDispatchStore(t, func(t *testing.T, s Store, _ childSeer) {
		ctx := t.Context()
		_, err := s.CreatePendingTask(ctx, admissionFor("t1", "child-1", "initial"))
		require.NoError(t, err)

		t2, found, err := s.DispatchNextChildMessage(ctx, "child-1")
		require.NoError(t, err)
		require.True(t, found)
		require.Equal(t, "t1", t2.ID, "the bound pending attempt is promoted, not replaced")
		require.Equal(t, StatusRunning, t2.Status)
		require.False(t, t2.StartedAt.IsZero())
		require.NotEmpty(t, t2.MessageID, "the attempt records the claimed message")

		msgs, err := s.ListChildMessages(ctx, "child-1")
		require.NoError(t, err)
		require.Equal(t, MessageDelivered, msgs[0].State)
		require.False(t, msgs[0].DeliveredAt.IsZero())
		require.Equal(t, t2.ID, msgs[0].TaskID)

		// The start fact is durable in the outbox.
		out, err := s.ListOutbox(ctx)
		require.NoError(t, err)
		require.Len(t, out, 1)
		require.Equal(t, EventStarted, out[0].EventType)
		require.Equal(t, "t1", out[0].TaskID)

		// A second dispatch with an empty mailbox and a running
		// attempt claims nothing.
		_, found, err = s.DispatchNextChildMessage(ctx, "child-1")
		require.NoError(t, err)
		require.False(t, found)
	})
}

func TestDispatchStore_RunningAttemptBlocksDelivery(t *testing.T) {
	eachDispatchStore(t, func(t *testing.T, s Store, _ childSeer) {
		ctx := t.Context()
		_, err := s.CreatePendingTask(ctx, admissionFor("t1", "child-1", "initial"))
		require.NoError(t, err)
		_, found, err := s.DispatchNextChildMessage(ctx, "child-1")
		require.NoError(t, err)
		require.True(t, found)

		msg := appendMsg(s, "t1", "while running")
		require.Equal(t, uint64(1), msg.Sequence)

		next, found, err := s.DispatchNextChildMessage(ctx, "child-1")
		require.NoError(t, err)
		require.False(t, found, "no concurrent attempts for one child session")
		require.Nil(t, next)

		// The message stays queued for the next turn boundary.
		msgs, err := s.ListChildMessages(ctx, "child-1")
		require.NoError(t, err)
		require.Equal(t, MessageQueued, msgs[1].State)

		// A waiting attempt is the same: a message never resumes it.
		cur, err := s.Get(ctx, "t1")
		require.NoError(t, err)
		cur.Status = StatusWaitingForInput
		require.NoError(t, s.Save(ctx, cur))
		_, found, err = s.DispatchNextChildMessage(ctx, "child-1")
		require.NoError(t, err)
		require.False(t, found)
	})
}

func TestDispatchStore_TerminalDispatchCreatesSuccessorLineage(t *testing.T) {
	eachDispatchStore(t, func(t *testing.T, s Store, _ childSeer) {
		ctx := t.Context()
		_, err := s.CreatePendingTask(ctx, admissionFor("t1", "child-1", "initial"))
		require.NoError(t, err)
		_, found, err := s.DispatchNextChildMessage(ctx, "child-1")
		require.NoError(t, err)
		require.True(t, found)

		msg := appendMsg(s, "t1", "next turn")

		_, won, err := s.TerminalizeAndDeliver(ctx, "t1", 1, TerminalUpdate{
			Status: StatusCompleted, Result: "first answer", CompletedAt: time.Now(),
		})
		require.NoError(t, err)
		require.True(t, won)

		succ, found, err := s.DispatchNextChildMessage(ctx, "child-1")
		require.NoError(t, err)
		require.True(t, found)
		require.NotEqual(t, "t1", succ.ID, "the successor is a new task attempt")
		require.Equal(t, StatusRunning, succ.Status)
		require.Equal(t, uint64(2), succ.RunGeneration, "generation increments per child turn")
		require.Equal(t, "t1", succ.ResumesTaskID, "lineage names the predecessor")
		require.Equal(t, msg.ID, succ.MessageID)
		require.Equal(t, "next turn", succ.Prompt, "the claimed message is the attempt input")
		require.Equal(t, "child-1", succ.ChildSessionID, "the child session is retained")
		require.Equal(t, "coder", succ.Profile, "policy metadata carries forward")
		require.Equal(t, "tc-t1", succ.ToolCallID)

		// The predecessor stays exactly as terminalized.
		first, err := s.Get(ctx, "t1")
		require.NoError(t, err)
		require.Equal(t, "first answer", first.Result)
	})
}

func TestDispatchStore_ContinuationValidatesAndBindsLineage(t *testing.T) {
	eachDispatchStore(t, func(t *testing.T, s Store, _ childSeer) {
		ctx := t.Context()
		_, err := s.CreatePendingTask(ctx, admissionFor("t1", "child-1", "initial"))
		require.NoError(t, err)
		_, found, err := s.DispatchNextChildMessage(ctx, "child-1")
		require.NoError(t, err)
		require.True(t, found)

		// A live attempt cannot be continued.
		cont := Admission{Task: &Task{
			ID: "t2", OwnerSessionID: "owner", ParentSessionID: "parent",
			Prompt: "continue", Status: StatusPending, RunGeneration: 1,
			ResumesTaskID: "t1", CreatedAt: time.Now(), UpdatedAt: time.Now(),
		}}
		_, err = s.CreatePendingTask(ctx, cont)
		require.ErrorIs(t, err, ErrResumeLive)

		// Unknown and foreign predecessors are rejected.
		cont.Task.ResumesTaskID = "nope"
		_, err = s.CreatePendingTask(ctx, cont)
		require.ErrorIs(t, err, ErrNotFound)
		cont.Task.ResumesTaskID = "t1"
		cont.Task.OwnerSessionID = "intruder"
		_, err = s.CreatePendingTask(ctx, cont)
		require.ErrorIs(t, err, ErrNotOwner)

		// The owner's continuation binds the retained child session
		// with run_generation + 1.
		cont.Task.ID = "t2"
		cont.Task.OwnerSessionID = "owner"
		_, won, err := s.TerminalizeAndDeliver(ctx, "t1", 1, TerminalUpdate{
			Status: StatusCompleted, CompletedAt: time.Now(),
		})
		require.NoError(t, err)
		require.True(t, won)

		saved, err := s.CreatePendingTask(ctx, cont)
		require.NoError(t, err)
		require.Equal(t, "child-1", saved.ChildSessionID)
		require.Equal(t, uint64(2), saved.RunGeneration)
		require.Equal(t, "t1", saved.ResumesTaskID)
		require.Equal(t, StatusPending, saved.Status)

		// The continuation's prompt is queued as its own mailbox row.
		msgs, err := s.ListChildMessages(ctx, "child-1")
		require.NoError(t, err)
		require.Len(t, msgs, 2, "seq zero plus the continuation row")
		require.Equal(t, "t2", msgs[1].TaskID)
	})
}

func TestDispatchStore_CancelPendingRejectsAdmissionMessage(t *testing.T) {
	eachDispatchStore(t, func(t *testing.T, s Store, _ childSeer) {
		ctx := t.Context()
		_, err := s.CreatePendingTask(ctx, admissionFor("t1", "child-1", "initial"))
		require.NoError(t, err)
		later := appendMsg(s, "t1", "queued behind cancel")

		u := TerminalUpdate{
			Status: StatusCancelled, Summary: "cancelled before start", CompletedAt: time.Now(),
		}
		t1, won, err := s.CancelPendingIfLive(ctx, "t1", u, ReasonTaskCancelled)
		require.NoError(t, err)
		require.True(t, won)
		require.Equal(t, StatusCancelled, t1.Status)

		// A second cancel loses the pending-fence.
		_, won, err = s.CancelPendingIfLive(ctx, "t1", u, ReasonTaskCancelled)
		require.NoError(t, err)
		require.False(t, won)

		msgs, err := s.ListChildMessages(ctx, "child-1")
		require.NoError(t, err)
		require.Equal(t, MessageRejected, msgs[0].State, "the undelivered admission message is rejected")
		require.Equal(t, ReasonTaskCancelled, msgs[0].Reason)
		require.True(t, msgs[0].DeliveredAt.IsZero())
		require.Equal(t, MessageQueued, msgs[1].State, "higher sequences are unchanged")

		// The higher message dispatches as a successor attempt.
		succ, found, err := s.DispatchNextChildMessage(ctx, "child-1")
		require.NoError(t, err)
		require.True(t, found)
		require.Equal(t, uint64(2), succ.RunGeneration)
		require.Equal(t, later.ID, succ.MessageID)
		require.Equal(t, "queued behind cancel", succ.Prompt)
	})
}

func TestDispatchStore_DispatchUnknownChildIsNoOp(t *testing.T) {
	eachDispatchStore(t, func(t *testing.T, s Store, _ childSeer) {
		_, found, err := s.DispatchNextChildMessage(t.Context(), "nobody")
		require.NoError(t, err)
		require.False(t, found)
	})
}

func TestSQLiteStore_MessageSequenceIsUniquePerChild(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	conn, err := db.Connect(t.Context(), dir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Release(dir)) })
	store := NewSQLiteStore(conn)

	_, err = store.CreatePendingTask(t.Context(), admissionFor("t1", "child-1", "initial"))
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), `INSERT INTO agent_task_messages
		(id, child_session_id, task_id, owner_session_id, sequence, origin, prompt, state, created_at)
		VALUES ('dupe', 'child-1', 't1', 'owner', 0, 'user', 'clash', 'queued', 1)`)
	require.Error(t, err, "UNIQUE(child_session_id, sequence) rejects a collided sequence")
}

func TestSQLiteStore_AdmissionRollbackKeepsSessionRowAbsent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	conn, err := db.Connect(t.Context(), dir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Release(dir)) })
	store := NewSQLiteStore(conn)
	ctx := t.Context()

	// Force the mailbox insert to fail for this child only, after
	// the task and sessions rows were written in the transaction.
	_, err = conn.ExecContext(ctx, `CREATE TRIGGER reject_mailbox
		BEFORE INSERT ON agent_task_messages
		WHEN NEW.child_session_id = 'child-boom'
		BEGIN SELECT RAISE(ABORT, 'no mailbox for you'); END`)
	require.NoError(t, err)

	_, err = store.CreatePendingTask(ctx, admissionFor("t1", "child-boom", "initial"))
	require.Error(t, err)

	n, err := queryCount(ctx, conn, `SELECT COUNT(*) FROM agent_tasks WHERE id = 't1'`)
	require.NoError(t, err)
	require.Zero(t, n, "the task row rolled back")
	n, err = queryCount(ctx, conn, `SELECT COUNT(*) FROM sessions WHERE id = 'child-boom'`)
	require.NoError(t, err)
	require.Zero(t, n, "the child session row rolled back with the task")
	n, err = queryCount(ctx, conn, `SELECT COUNT(*) FROM agent_task_messages WHERE child_session_id = 'child-boom'`)
	require.NoError(t, err)
	require.Zero(t, n, "no mailbox row survived the rollback")

	_, err = conn.ExecContext(ctx, `DROP TRIGGER reject_mailbox`)
	require.NoError(t, err)
	_, err = store.CreatePendingTask(ctx, admissionFor("t1", "child-boom", "initial"))
	require.NoError(t, err, "admission succeeds once the failure is removed")
}

func queryCount(ctx context.Context, conn *sql.DB, q string, args ...any) (int, error) {
	var n int
	err := conn.QueryRowContext(ctx, q, args...).Scan(&n)
	return n, err
}
