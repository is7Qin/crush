package model

import (
	"context"
	"log/slog"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/crush/internal/proto"
	"github.com/charmbracelet/crush/internal/ui/dialog"
)

// subagentsFetchMsg carries one authorized Workspace.TaskList read back
// to the Update loop. Commands never touch model state directly.
type subagentsFetchMsg struct {
	tasks []proto.TaskSnapshot
	err   error
}

// openSubagentsDialog opens (or raises) the subagent picker and kicks
// off the task snapshot read. Listing is owner-scoped server-side, so
// no parent filter is passed.
func (m *UI) openSubagentsDialog() tea.Cmd {
	if m.dialog == nil {
		return nil
	}
	if m.dialog.ContainsDialog(dialog.SubagentsID) {
		// Bring to front and refresh the snapshots.
		m.dialog.BringToFront(dialog.SubagentsID)
		return m.fetchSubagentsCmd()
	}

	m.dialog.OpenDialog(dialog.NewSubagents(m.com))
	return m.fetchSubagentsCmd()
}

// fetchSubagentsCmd reads the current owner's task snapshots off the
// workspace and reports them back through subagentsFetchMsg.
func (m *UI) fetchSubagentsCmd() tea.Cmd {
	return func() tea.Msg {
		tasks, err := m.com.Workspace.TaskList(context.Background(), "")
		if err != nil {
			return subagentsFetchMsg{err: err}
		}
		return subagentsFetchMsg{tasks: tasks}
	}
}

// applySubagentsFetch routes a task snapshot read into the open
// Subagents dialog. It is a no-op once the dialog has been closed.
func (m *UI) applySubagentsFetch(msg subagentsFetchMsg) {
	if m.dialog == nil {
		return
	}
	subagents, ok := m.dialog.Dialog(dialog.SubagentsID).(*dialog.Subagents)
	if !ok || subagents == nil {
		return
	}
	if msg.err != nil {
		slog.Error("Failed to list subagent tasks", "error", msg.err)
		subagents.SetError(msg.err)
		return
	}
	subagents.SetTasks(msg.tasks)
}

// refreshSubagentsCmd re-reads task snapshots while the Subagents
// dialog is open so live task events surface in the list. It returns
// nil when the dialog is not visible.
func (m *UI) refreshSubagentsCmd() tea.Cmd {
	if m.dialog == nil || !m.dialog.ContainsDialog(dialog.SubagentsID) {
		return nil
	}
	return m.fetchSubagentsCmd()
}
