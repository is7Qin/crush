package task

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// startHidden admits a hidden agentic_fetch-shaped task and returns
// its record. The runner completes immediately with the given text.
func startHidden(t *testing.T, m *Manager, owner, child, text string) *Task {
	t.Helper()
	rec := record(m)
	tk, err := m.Start(t.Context(), StartRequest{
		CallerSessionID: owner, Prompt: "fetch something", Provider: "p", Model: "m",
		ChildSessionID: child, Profile: HiddenProfile, Run: nopRun(text),
	})
	require.NoError(t, err)
	waitStatusViaRecorder(t, rec, tk.ID, StatusCompleted)
	return tk
}

// waitStatusViaRecorder waits for a terminal event of the id. Hidden
// tasks are invisible to Status, so event observation is the join.
func waitStatusViaRecorder(t *testing.T, r *recorder, id string, want Status) {
	t.Helper()
	require.Eventually(t, func() bool {
		for _, e := range r.taskEvents(id) {
			if e.Task.Status == want && want.Terminal() {
				return true
			}
		}
		return false
	}, 5*time.Second, 5*time.Millisecond, "task %s never reached %s", id, want)
}

func TestHiddenTask_PublicControlPlaneDenies(t *testing.T) {
	t.Parallel()
	m := newTestManager(t, Limits{})
	child := uniqueChild()
	tk := startHidden(t, m, "owner", child, "analysis result")

	// Every public control path reports the hidden task as not found,
	// even to its owner.
	_, err := m.Status(t.Context(), "owner", tk.ID)
	require.ErrorIs(t, err, ErrNotFound)
	_, _, err = m.Output(t.Context(), "owner", tk.ID)
	require.ErrorIs(t, err, ErrNotFound)
	require.ErrorIs(t, m.Cancel(t.Context(), "owner", tk.ID), ErrNotFound)
	_, err = m.AppendMessage(t.Context(), MessageRequest{
		OwnerSessionID: "owner", TaskID: tk.ID, Origin: OriginUser, Prompt: "more",
	})
	require.ErrorIs(t, err, ErrNotFound)

	// The public list is hidden-free; a public sibling stays visible.
	_, err = m.Start(t.Context(), StartRequest{
		CallerSessionID: "owner", Prompt: "public", Provider: "p", Model: "m",
		ChildSessionID: uniqueChild(), Run: nopRun("pub"),
	})
	require.NoError(t, err)
	mine, err := m.List(t.Context(), "owner", "")
	require.NoError(t, err)
	for _, got := range mine {
		require.False(t, got.IsHidden(), "List must never return hidden tasks")
	}
	require.Len(t, mine, 1)
}

func TestHiddenTask_InternalDiagnosticAccess(t *testing.T) {
	t.Parallel()
	m := newTestManager(t, Limits{})
	tk := startHidden(t, m, "owner", uniqueChild(), "diagnostic text")

	// The trusted workspace plus the owning session reads it.
	got, err := m.DiagnosticTask(t.Context(), "ws", "owner", tk.ID)
	require.NoError(t, err)
	require.Equal(t, HiddenProfile, got.Profile)
	require.Equal(t, "diagnostic text", got.Result)
	require.Equal(t, StatusCompleted, got.Status)

	// A mismatched workspace cannot, even with the right owner.
	_, err = m.DiagnosticTask(t.Context(), "other-ws", "owner", tk.ID)
	require.ErrorIs(t, err, ErrNotFound)

	// A foreign owner cannot, even in the right workspace.
	_, err = m.DiagnosticTask(t.Context(), "ws", "intruder", tk.ID)
	require.ErrorIs(t, err, ErrNotOwner)
}

func TestHiddenTask_TerminalDeliveryStillFlowsToParentInbox(t *testing.T) {
	t.Parallel()
	m := newTestManager(t, Limits{})
	tk := startHidden(t, m, "owner", uniqueChild(), "fetched answer")

	inbox, err := m.Inbox(t.Context(), "owner")
	require.NoError(t, err)
	require.Len(t, inbox, 1, "the hidden task's terminal result is the parent's reply channel")
	env, err := inbox[0].Envelope()
	require.NoError(t, err)
	require.Equal(t, tk.ID, env.TaskID)
	require.Equal(t, HiddenProfile, env.Profile)
	require.Equal(t, "fetched answer", env.Result)
}

func TestHiddenTask_NotContinuable(t *testing.T) {
	t.Parallel()
	m := newTestManager(t, Limits{})
	tk := startHidden(t, m, "owner", uniqueChild(), "done")

	_, err := m.Start(t.Context(), StartRequest{
		CallerSessionID: "owner", Prompt: "continue the hidden child", Provider: "p", Model: "m",
		ResumesTaskID: tk.ID, Run: nopRun("never"),
	})
	require.ErrorIs(t, err, ErrNotFound)
}

func TestHiddenProfile_RenderHidesOutputRetrieval(t *testing.T) {
	t.Parallel()
	hidden := TaskResultEnvelope{Profile: HiddenProfile, ResultTruncated: true}.Render()
	require.NotContains(t, hidden, "agent_output")
	require.Contains(t, hidden, "no further output is retrievable")

	pub := TaskResultEnvelope{Profile: "coder", ResultTruncated: true}.Render()
	require.Contains(t, pub, "agent_output")
}
