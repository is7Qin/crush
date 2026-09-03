package task

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// questionOutcome builds the fence payload for row against t.
func questionOutcome(t *Task, id, resolution string) QuestionOutcome {
	return QuestionOutcome{
		QuestionID:     id,
		TaskID:         t.ID,
		OwnerSessionID: t.OwnerSessionID,
		ChildSessionID: t.ChildSessionID,
		RunGeneration:  t.RunGeneration,
		Resolution:     resolution,
		ResolvedAt:     time.Now().UTC(),
	}
}

// questionRow builds the wait payload for row against t.
func questionRow(t *Task, id string) QuestionRow {
	return QuestionRow{
		QuestionID:     id,
		TaskID:         t.ID,
		OwnerSessionID: t.OwnerSessionID,
		ChildSessionID: t.ChildSessionID,
		RunGeneration:  t.RunGeneration,
		Batch:          `{"questions":[{"type":"yes_no","question":"Proceed?"}]}`,
		CreatedAt:      time.Now().UTC(),
	}
}

// startRunning admits a task and waits until its runner is inside
// the run, returning the task snapshot and the gate that releases
// the runner.
func startRunning(t *testing.T, m *Manager, owner string) (*Task, chan Result) {
	t.Helper()
	gate := make(chan Result, 1)
	inside := make(chan struct{})
	started := make(chan *Task, 1)
	task, err := m.Start(context.Background(), StartRequest{
		CallerSessionID: owner,
		Prompt:          "work",
		Provider:        "p",
		Model:           "m",
		ChildSessionID:  uniqueChild(),
		Run: func(context.Context, *Handle) (Result, error) {
			inside <- struct{}{}
			return <-gate, nil
		},
	})
	require.NoError(t, err)
	select {
	case <-inside:
	case <-time.After(5 * time.Second):
		t.Fatal("runner never entered")
	}
	started <- task
	return <-started, gate
}

func TestManager_QuestionBeginWaitMovesTaskAndRecordsPending(t *testing.T) {
	t.Parallel()
	m := newTestManager(t, Limits{})
	rec := record(m)
	task, gate := startRunning(t, m, "owner")
	defer func() { gate <- Result{Text: "done"} }()

	require.NoError(t, m.BeginQuestionWait(context.Background(), questionRow(task, "q1")))
	waitStatus(t, m, "owner", task.ID, StatusWaitingForInput)
	status, err := m.store.QuestionStatus(context.Background(), "q1")
	require.NoError(t, err)
	require.Equal(t, "pending", status)
	require.Equal(t, 1, rec.count(EventWaitingForInput))

	// A second question cannot bind: the attempt no longer runs and
	// the task already has a pending row.
	require.Error(t, m.BeginQuestionWait(context.Background(), questionRow(task, "q2")))
	// Unknown and stale-generation attempts reject the bind too.
	require.ErrorIs(t, m.BeginQuestionWait(context.Background(), QuestionRow{
		QuestionID: "q9", TaskID: "nope", RunGeneration: 1, CreatedAt: time.Now(),
	}), ErrNotFound)
	stale := questionRow(task, "q10")
	stale.RunGeneration = task.RunGeneration + 1
	require.ErrorIs(t, m.BeginQuestionWait(context.Background(), stale), ErrNotFound)
}

func TestManager_QuestionAnswerKeepsTaskWaitingUntilResume(t *testing.T) {
	t.Parallel()
	m := newTestManager(t, Limits{})
	rec := record(m)
	task, gate := startRunning(t, m, "owner")
	defer func() { gate <- Result{Text: "done"} }()

	require.NoError(t, m.BeginQuestionWait(context.Background(), questionRow(task, "q1")))
	yes := "answered"
	require.NoError(t, m.AnswerQuestion(context.Background(), questionOutcome(task, "q1", yes)))

	// The answer commits while the task stays waiting: capacity is
	// only reacquired by Resume, after the service woke the runner.
	got := waitStatus(t, m, "owner", task.ID, StatusWaitingForInput)
	require.Equal(t, StatusWaitingForInput, got.Status)
	require.Zero(t, rec.count(EventResumed))
	status, err := m.store.QuestionStatus(context.Background(), "q1")
	require.NoError(t, err)
	require.Equal(t, yes, status)

	require.NoError(t, m.ResumeQuestion(context.Background(), task.ID, task.RunGeneration))
	waitStatus(t, m, "owner", task.ID, StatusRunning)
	require.Equal(t, 1, rec.count(EventResumed))

	// Resuming with a stale generation is refused without touching
	// the task.
	require.ErrorIs(t, m.ResumeQuestion(context.Background(), task.ID, task.RunGeneration+1),
		ErrInvalidTransition)
}

func TestManager_QuestionTerminalKindsCommitTogether(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		kind      string
		want      Status
		terminalU func() TerminalUpdate
	}{
		{"cancel", "cancelled", StatusCancelled, func() TerminalUpdate {
			return TerminalUpdate{Status: StatusCancelled, Summary: "task question cancelled"}
		}},
		{"timeout", "timed_out", StatusFailed, func() TerminalUpdate {
			return TerminalUpdate{
				Status: StatusFailed, Err: ReasonTaskQuestionTimeout,
				Summary: "task question timed out",
			}
		}},
		{"interrupt", "interrupted", StatusInterrupted, func() TerminalUpdate {
			return TerminalUpdate{Status: StatusInterrupted, Summary: "task question interrupted"}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Limits{})
			task, gate := startRunning(t, m, "owner")
			_ = gate // runner stays blocked; terminalization is the transition under test

			require.NoError(t, m.BeginQuestionWait(context.Background(), questionRow(task, "q1")))
			u := tc.terminalU()
			u.CompletedAt = time.Now()
			require.NoError(t, m.TerminalizeQuestion(context.Background(),
				questionOutcome(task, "q1", tc.kind), u))
			got := waitStatus(t, m, "owner", task.ID, tc.want)
			status, err := m.store.QuestionStatus(context.Background(), "q1")
			require.NoError(t, err)
			require.Equal(t, tc.kind, status, "question and task commit together")

			// The durable terminal row carries the timeout reason.
			if tc.name == "timeout" {
				require.Contains(t, got.Err, ReasonTaskQuestionTimeout)
			}

			// Repeating the resolve observes the committed outcome and
			// cannot re-terminalize.
			require.NoError(t, m.TerminalizeQuestion(context.Background(),
				questionOutcome(task, "q1", tc.kind), u))
			got = waitStatus(t, m, "owner", task.ID, tc.want)
			require.Equal(t, tc.want, got.Status)
			// A fresh question on the now-terminal attempt never binds.
			require.Error(t, m.BeginQuestionWait(context.Background(), questionRow(task, "q2")))
		})
	}
}

func TestManager_PlainTerminalizationSweepsPendingQuestion(t *testing.T) {
	t.Parallel()
	m := newTestManager(t, Limits{})
	task, gate := startRunning(t, m, "owner")
	require.NoError(t, m.BeginQuestionWait(context.Background(), questionRow(task, "q1")))

	// A terminalization without a question fence (a cancelled
	// runner settling) must still resolve the dangling pending row
	// as interrupted in the same durable transaction.
	gate <- Result{Text: "raced"}
	waitStatus(t, m, "owner", task.ID, StatusCompleted)
	status, err := m.store.QuestionStatus(context.Background(), "q1")
	require.NoError(t, err)
	require.Equal(t, "interrupted", status)
}

func TestManager_QuestionFenceMismatchChangesNothing(t *testing.T) {
	t.Parallel()
	m := newTestManager(t, Limits{})
	task, gate := startRunning(t, m, "owner")
	defer func() { gate <- Result{Text: "done"} }()

	require.NoError(t, m.BeginQuestionWait(context.Background(), questionRow(task, "q1")))

	// A resolution claiming a different owner cannot commit, and the
	// pending row and waiting task are untouched.
	rogue := questionOutcome(task, "q1", "answered")
	rogue.OwnerSessionID = "intruder"
	require.ErrorIs(t, m.AnswerQuestion(context.Background(), rogue), ErrQuestionStale)
	status, err := m.store.QuestionStatus(context.Background(), "q1")
	require.NoError(t, err)
	require.Equal(t, "pending", status)
	waitStatus(t, m, "owner", task.ID, StatusWaitingForInput)

	// An answered resolution for a still-running (never-waiting)
	// task is likewise rejected.
	never := questionOutcome(task, "q-nope", "answered")
	require.ErrorIs(t, m.AnswerQuestion(context.Background(), never), ErrNotFound)
}

func TestManager_QuestionBeginWaitFailureLeavesNoPendingRow(t *testing.T) {
	t.Parallel()
	// One running slot: the first task occupies it so the second
	// deterministically stays pending, where the wait fence must
	// reject the bind without leaving a question row behind.
	m := newTestManager(t, Limits{RunningPerModel: 1})
	first, gate := startRunning(t, m, "owner")
	defer func() { gate <- Result{Text: "done"} }()
	second, err := m.Start(context.Background(), StartRequest{
		CallerSessionID: "owner",
		Prompt:          "work",
		Provider:        "p",
		Model:           "m",
		ChildSessionID:  uniqueChild(),
		Run:             nopRun("done"),
	})
	require.NoError(t, err)
	waitStatus(t, m, "owner", first.ID, StatusRunning)
	require.Equal(t, StatusPending, second.Status)

	err = m.BeginQuestionWait(context.Background(), questionRow(second, "q1"))
	require.ErrorIs(t, err, ErrInvalidTransition)
	_, serr := m.store.QuestionStatus(context.Background(), "q1")
	require.ErrorIs(t, serr, ErrNotFound, "a failed wait binds no question row")
}
