package backend

import (
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/agent/task"
	"github.com/stretchr/testify/require"
)

// TestTaskControl_ResyncSkipsConsumedReports is the resync half of
// the report-delivery read-then-deliver contract: a report the
// parent already pulled through agent_output is not a report it
// missed, so reconnect resync must not return it.
func TestTaskControl_ResyncSkipsConsumedReports(t *testing.T) {
	t.Parallel()
	tc, _ := newTaskControlForTest(t)
	ctx := t.Context()
	seedTask(t, tc, "t1", "owner", task.StatusRunning)

	_, won, err := tc.Mgr.Store().TerminalizeAndDeliver(ctx, "t1", 1, task.TerminalUpdate{
		Status: task.StatusCompleted, Result: "body", CompletedAt: time.Now(),
	})
	require.NoError(t, err)
	require.True(t, won)

	resync, err := tc.Resync(ctx, "owner")
	require.NoError(t, err)
	require.Len(t, resync.Inbox, 1, "an unread report still resyncs")

	_, _, err = tc.Mgr.Output(ctx, "owner", "t1")
	require.NoError(t, err)

	resync, err = tc.Resync(ctx, "owner")
	require.NoError(t, err)
	require.Empty(t, resync.Inbox, "a pulled report never resyncs")
}
