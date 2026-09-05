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
