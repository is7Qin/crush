package dialog

import (
	"errors"
	"image"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/crush/internal/proto"
	"github.com/charmbracelet/crush/internal/ui/common"
	"github.com/charmbracelet/crush/internal/ui/styles"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/stretchr/testify/require"
)

func newSubagentsDialogForTest(t *testing.T) *Subagents {
	t.Helper()

	sty := styles.CharmtonePantera()
	return NewSubagents(&common.Common{Styles: &sty})
}

func taskSnapshot(id, child, profile, summary, status, model string) proto.TaskSnapshot {
	return proto.TaskSnapshot{
		ID:             id,
		ChildSessionID: child,
		Profile:        profile,
		Summary:        summary,
		Status:         status,
		Model:          model,
	}
}

func TestSubagentsEnterSelectsChildSession(t *testing.T) {
	t.Parallel()

	dialog := newSubagentsDialogForTest(t)
	dialog.SetTasks([]proto.TaskSnapshot{
		taskSnapshot("t1", "child-1", "coder", "Fix the bug", "running", "model-a"),
		taskSnapshot("t2", "child-2", "task", "Explore repo", "completed", "model-b"),
	})

	action := dialog.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})

	selected, ok := action.(ActionSelectSubagent)
	require.True(t, ok, "enter must select the highlighted task")
	require.Equal(t, "child-1", selected.ChildSessionID)
}

func TestSubagentsSelectionRetainedAcrossSetTasks(t *testing.T) {
	t.Parallel()

	dialog := newSubagentsDialogForTest(t)
	dialog.SetTasks([]proto.TaskSnapshot{
		taskSnapshot("t1", "child-1", "coder", "First", "running", ""),
		taskSnapshot("t2", "child-2", "task", "Second", "running", ""),
	})
	require.Nil(t, dialog.HandleMsg(tea.KeyPressMsg{Code: tea.KeyDown}))
	require.Equal(t, "t2", dialog.list.SelectedItem().(*SubagentItem).ID())

	// A refresh that reorders the rows must keep the cursor on t2.
	dialog.SetTasks([]proto.TaskSnapshot{
		taskSnapshot("t2", "child-2", "task", "Second", "completed", ""),
		taskSnapshot("t1", "child-1", "coder", "First", "running", ""),
	})

	action := dialog.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	selected, ok := action.(ActionSelectSubagent)
	require.True(t, ok)
	require.Equal(t, "child-2", selected.ChildSessionID)
}

func TestSubagentsEnterWithoutChildSessionWarns(t *testing.T) {
	t.Parallel()

	dialog := newSubagentsDialogForTest(t)
	dialog.SetTasks([]proto.TaskSnapshot{
		taskSnapshot("t1", "", "coder", "Pending task", "pending", ""),
	})

	action := dialog.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})

	cmdAction, ok := action.(ActionCmd)
	require.True(t, ok, "a task without a child session must not switch")
	require.NotNil(t, cmdAction.Cmd)
}

func TestSubagentsDoubleClickOpensEntryLikeEnter(t *testing.T) {
	t.Parallel()

	dialog := newSubagentsDialogForTest(t)
	dialog.SetTasks([]proto.TaskSnapshot{
		taskSnapshot("t1", "child-1", "coder", "First", "running", ""),
		taskSnapshot("t2", "child-2", "task", "Second", "running", ""),
	})
	scr := uv.NewScreenBuffer(80, 30)
	dialog.Draw(scr, image.Rect(0, 0, 80, 30))
	require.False(t, dialog.bodyArea.Empty())
	click := tea.MouseClickMsg(tea.Mouse{
		X:      dialog.bodyArea.Min.X,
		Y:      dialog.bodyArea.Min.Y + 1,
		Button: tea.MouseLeft,
	})

	require.Nil(t, dialog.HandleMsg(click))
	enterAction := dialog.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	doubleClickAction := dialog.HandleMsg(click)

	require.Equal(t, enterAction, doubleClickAction)
	selected, ok := doubleClickAction.(ActionSelectSubagent)
	require.True(t, ok)
	require.Equal(t, "child-2", selected.ChildSessionID)
}

func TestSubagentsDoubleClickExpires(t *testing.T) {
	t.Parallel()

	dialog := newSubagentsDialogForTest(t)
	dialog.SetTasks([]proto.TaskSnapshot{
		taskSnapshot("t1", "child-1", "coder", "First", "running", ""),
		taskSnapshot("t2", "child-2", "task", "Second", "running", ""),
	})
	scr := uv.NewScreenBuffer(80, 30)
	dialog.Draw(scr, image.Rect(0, 0, 80, 30))
	click := tea.MouseClickMsg(tea.Mouse{
		X:      dialog.bodyArea.Min.X,
		Y:      dialog.bodyArea.Min.Y + 1,
		Button: tea.MouseLeft,
	})

	require.Nil(t, dialog.HandleMsg(click))
	dialog.lastClickTime = time.Now().Add(-doubleClickThreshold - time.Millisecond)
	require.Nil(t, dialog.HandleMsg(click))
	require.Equal(t, "t2", dialog.list.SelectedItem().(*SubagentItem).ID())
}

var errTestFetch = errors.New("server said:\nmultiline boom")

func TestSubagentsStatusStates(t *testing.T) {
	t.Parallel()

	dialog := newSubagentsDialogForTest(t)
	require.Equal(t, "Loading subagents…", dialog.statusMessage())

	dialog.SetTasks(nil)
	require.Equal(t, "No subagent tasks.", dialog.statusMessage())

	dialog.SetError(errTestFetch)
	require.Contains(t, dialog.statusMessage(), "Failed to load subagents")
	require.NotContains(t, dialog.statusMessage(), "\n")

	// A successful read clears the error state.
	dialog.SetTasks([]proto.TaskSnapshot{
		taskSnapshot("t1", "child-1", "coder", "First", "running", ""),
	})
	require.Empty(t, dialog.statusMessage())
}

func TestSubagentsSetTasksShowsAllWhenQueryEmpty(t *testing.T) {
	t.Parallel()

	dialog := newSubagentsDialogForTest(t)
	tasks := []proto.TaskSnapshot{
		taskSnapshot("t1", "child-1", "coder", "First", "running", "model-a"),
		taskSnapshot("t2", "child-2", "task", "Second", "completed", "model-b"),
	}
	dialog.SetTasks(tasks)
	require.Empty(t, dialog.input.Value())
	require.Len(t, dialog.list.FilteredItems(), 2)

	// A stale list-level filter must not survive a refresh with no query.
	dialog.list.SetFilter("zzz-stale-filter")
	require.Empty(t, dialog.list.FilteredItems())

	dialog.SetTasks(tasks)
	require.Empty(t, dialog.input.Value())
	require.Len(t, dialog.list.FilteredItems(), 2)
	require.Empty(t, dialog.statusMessage())
}

func TestSubagentsFilterMatchesStatus(t *testing.T) {
	t.Parallel()

	dialog := newSubagentsDialogForTest(t)
	dialog.SetTasks([]proto.TaskSnapshot{
		taskSnapshot("t1", "child-1", "coder", "First", "running", "model-a"),
		taskSnapshot("t2", "child-2", "task", "Second", "completed", "model-b"),
	})

	running := &SubagentItem{snapshot: taskSnapshot("t1", "child-1", "coder", "First", "running", "model-a")}
	require.Contains(t, running.Filter(), "running")

	dialog.list.SetFilter("running")
	items := dialog.list.FilteredItems()
	require.Len(t, items, 1)
	require.Equal(t, "t1", items[0].(*SubagentItem).ID())
}

func TestSubagentsNoMatchMessage(t *testing.T) {
	t.Parallel()

	dialog := newSubagentsDialogForTest(t)
	dialog.SetTasks([]proto.TaskSnapshot{
		taskSnapshot("t1", "child-1", "coder", "First", "running", ""),
	})
	dialog.input.SetValue("zzz-no-such-task")
	dialog.list.SetFilter("zzz-no-such-task")
	require.Equal(t, "No subagent tasks match the filter.", dialog.statusMessage())
}

func TestSubagentsRenderShowsStatus(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	item := &SubagentItem{snapshot: taskSnapshot("t1", "child-1", "coder", "First", "running", "model-a"), t: &sty}
	require.Contains(t, item.Render(80), "[running]")
}

func finishedSnapshot(id string, hour int) proto.TaskSnapshot {
	snap := taskSnapshot(id, "child-"+id, "task", "Finished "+id, "completed", "")
	snap.CreatedAt = time.Date(2026, 9, 19, hour, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
	snap.CompletedAt = snap.CreatedAt
	return snap
}

func visibleTaskIDs(dialog *Subagents) []string {
	ids := []string{}
	for _, item := range dialog.list.FilteredItems() {
		if taskItem, ok := item.(*SubagentItem); ok && taskItem != nil {
			ids = append(ids, taskItem.ID())
		}
	}
	return ids
}

func visibleSummary(t *testing.T, dialog *Subagents) *subagentsMoreItem {
	t.Helper()
	for _, item := range dialog.list.FilteredItems() {
		if more, ok := item.(*subagentsMoreItem); ok {
			return more
		}
	}
	return nil
}

func TestSubagentsLiveBeforeFinished(t *testing.T) {
	t.Parallel()

	dialog := newSubagentsDialogForTest(t)
	dialog.SetTasks([]proto.TaskSnapshot{
		taskSnapshot("f1", "child-f1", "task", "Old finished", "completed", ""),
		taskSnapshot("l1", "child-l1", "coder", "Live one", "running", ""),
		taskSnapshot("f2", "child-f2", "task", "Old finished two", "failed", ""),
		taskSnapshot("l2", "child-l2", "coder", "Live two", "pending", ""),
		taskSnapshot("l3", "child-l3", "coder", "Live three", "waiting_for_input", ""),
	})

	require.Equal(t, []string{"l1", "l2", "l3", "f1", "f2"}, visibleTaskIDs(dialog))
}

func TestSubagentsFinishedNewestFirst(t *testing.T) {
	t.Parallel()

	dialog := newSubagentsDialogForTest(t)
	dialog.SetTasks([]proto.TaskSnapshot{
		finishedSnapshot("old", 10),
		finishedSnapshot("new", 12),
		finishedSnapshot("mid", 11),
	})

	require.Equal(t, []string{"new", "mid", "old"}, visibleTaskIDs(dialog))
}

func TestSubagentsFinishedCapAndSummary(t *testing.T) {
	t.Parallel()

	dialog := newSubagentsDialogForTest(t)
	tasks := []proto.TaskSnapshot{
		taskSnapshot("live", "child-live", "coder", "Live task", "running", ""),
	}
	for _, id := range []string{"f1", "f2", "f3", "f4", "f5", "f6", "f7"} {
		hour := int(id[1] - '0')
		tasks = append(tasks, finishedSnapshot(id, hour))
	}
	dialog.SetTasks(tasks)

	require.Equal(t, []string{"live", "f7", "f6", "f5", "f4", "f3"}, visibleTaskIDs(dialog))
	require.Len(t, dialog.list.FilteredItems(), 7)

	more := visibleSummary(t, dialog)
	require.NotNil(t, more, "a summary row must follow the capped finished section")
	require.Equal(t, 2, more.count)
	require.Contains(t, more.Render(80), "… 2 more finished (type to filter)")
	require.Empty(t, dialog.statusMessage())
}

func TestSubagentsLiveNeverCapped(t *testing.T) {
	t.Parallel()

	dialog := newSubagentsDialogForTest(t)
	tasks := []proto.TaskSnapshot{}
	for i := 1; i <= 8; i++ {
		id := "live-" + string(rune('0'+i))
		tasks = append(tasks, taskSnapshot(id, "child-"+id, "coder", "Live "+id, "running", ""))
	}
	tasks = append(tasks, finishedSnapshot("f1", 10))
	dialog.SetTasks(tasks)

	require.Len(t, visibleTaskIDs(dialog), 9)
	require.Nil(t, visibleSummary(t, dialog))
}

func TestSubagentsFilterBypassesCap(t *testing.T) {
	t.Parallel()

	dialog := newSubagentsDialogForTest(t)
	tasks := []proto.TaskSnapshot{}
	for i := 1; i <= 7; i++ {
		id := "f" + string(rune('0'+i))
		snap := finishedSnapshot(id, i)
		snap.Summary = "Routine work " + id
		tasks = append(tasks, snap)
	}
	// The oldest finished task is capped out of the default view; give
	// it a unique summary so only the filter can reach it.
	tasks[0].Summary = "quux hidden token"
	dialog.SetTasks(tasks)
	require.NotContains(t, visibleTaskIDs(dialog), "f1")
	require.NotNil(t, visibleSummary(t, dialog))

	for _, r := range "quux" {
		dialog.HandleMsg(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	require.Equal(t, "quux", dialog.input.Value())
	require.Equal(t, []string{"f1"}, visibleTaskIDs(dialog))
	require.Nil(t, visibleSummary(t, dialog), "the summary row must disappear while filtering")
	require.Empty(t, dialog.statusMessage())
}

func TestSubagentsRefreshWhileFilteringSearchesAll(t *testing.T) {
	t.Parallel()

	dialog := newSubagentsDialogForTest(t)
	dialog.SetTasks([]proto.TaskSnapshot{finishedSnapshot("f1", 1)})
	dialog.input.SetValue("zzz-no-such-task")
	dialog.SetTasks([]proto.TaskSnapshot{
		finishedSnapshot("f1", 1),
		taskSnapshot("match", "child-match", "coder", "zzz-no-such-task unique", "completed", ""),
	})

	require.Equal(t, []string{"match"}, visibleTaskIDs(dialog))
	require.Nil(t, visibleSummary(t, dialog))
}

func TestSubagentsCappedSelectionFallsBackToTop(t *testing.T) {
	t.Parallel()

	dialog := newSubagentsDialogForTest(t)
	dialog.SetTasks([]proto.TaskSnapshot{finishedSnapshot("only", 10)})
	require.Equal(t, "only", dialog.list.SelectedItem().(*SubagentItem).ID())

	// The previously selected task falls outside the finished cap
	// after the refresh, so the cursor returns to the top entry.
	tasks := []proto.TaskSnapshot{
		taskSnapshot("live", "child-live", "coder", "Live task", "running", ""),
	}
	for _, id := range []string{"f1", "f2", "f3", "f4", "f5", "f6"} {
		tasks = append(tasks, finishedSnapshot(id, int(id[1]-'0')))
	}
	dialog.SetTasks(tasks)
	require.Equal(t, "live", dialog.list.SelectedItem().(*SubagentItem).ID())
}
