package model

import (
	"context"
	"errors"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/history"
	"github.com/charmbracelet/crush/internal/proto"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/charmbracelet/crush/internal/ui/attachments"
	"github.com/charmbracelet/crush/internal/ui/dialog"
	"github.com/charmbracelet/crush/internal/workspace"
	"github.com/stretchr/testify/require"
)

// subagentsTestWorkspace is a fake workspace covering the reads the
// Subagents flow performs: an authorized task listing and the child
// session load.
type subagentsTestWorkspace struct {
	workspace.Workspace
	cfg         *config.Config
	tasks       []proto.TaskSnapshot
	listErr     error
	taskListCtx context.Context
	loadedID    string
}

func (w *subagentsTestWorkspace) Config() *config.Config { return w.cfg }

func (w *subagentsTestWorkspace) TaskList(ctx context.Context, parentSessionID string) ([]proto.TaskSnapshot, error) {
	w.taskListCtx = ctx
	if w.listErr != nil {
		return nil, w.listErr
	}
	return w.tasks, nil
}

func (w *subagentsTestWorkspace) GetSession(_ context.Context, id string) (session.Session, error) {
	w.loadedID = id
	return session.Session{ID: id}, nil
}

func (w *subagentsTestWorkspace) ListSessionHistory(context.Context, string) ([]history.File, error) {
	return nil, nil
}

func (w *subagentsTestWorkspace) FileTrackerListReadFiles(context.Context, string) ([]string, error) {
	return nil, nil
}

func (w *subagentsTestWorkspace) SetCurrentSession(context.Context, string) error {
	return nil
}

var _ workspace.Workspace = (*subagentsTestWorkspace)(nil)

func taskSnap(id, child string) proto.TaskSnapshot {
	return proto.TaskSnapshot{ID: id, ChildSessionID: child, Profile: "coder", Summary: id, Status: "running"}
}

func newTestUIWithWorkspace(ws workspace.Workspace) *UI {
	u := newTestUI()
	u.com.Workspace = ws
	u.dialog = dialog.NewOverlay()
	return u
}

// runCmd executes a returned batched command and collects every
// message produced.
func runCmd(t *testing.T, cmd tea.Cmd) []tea.Msg {
	t.Helper()

	if cmd == nil {
		return nil
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		var out []tea.Msg
		for _, sub := range batch {
			if m := sub(); m != nil {
				out = append(out, m)
			}
		}
		return out
	}
	if msg == nil {
		return nil
	}
	return []tea.Msg{msg}
}

func TestOpenSubagentsDialogListsViaAuthorizedTaskRead(t *testing.T) {
	ws := &subagentsTestWorkspace{tasks: []proto.TaskSnapshot{
		taskSnap("t1", "child-1"),
	}}
	u := newTestUIWithWorkspace(ws)

	cmd := u.openSubagentsDialog()
	require.True(t, u.dialog.ContainsDialog(dialog.SubagentsID))
	require.Nil(t, ws.taskListCtx, "opening must not perform the read inline in Update")

	var fetch subagentsFetchMsg
	for _, msg := range runCmd(t, cmd) {
		if f, ok := msg.(subagentsFetchMsg); ok {
			fetch = f
		}
	}
	require.NotNil(t, ws.taskListCtx, "the read must run inside the tea.Cmd with a context")
	require.NoError(t, fetch.err)
	require.Len(t, fetch.tasks, 1)

	// Re-opening from the palette raises and refreshes the same dialog.
	cmd2 := u.openSubagentsDialog()
	require.NotNil(t, cmd2)
}

func TestCtrlBOpensSubagentsForActiveSession(t *testing.T) {
	ws := &subagentsTestWorkspace{cfg: agentsTestConfig()}
	u := newTestUIWithWorkspace(ws)
	u.session = &session.Session{ID: "parent-1"}
	u.attachments = attachments.New(nil, attachments.Keymap{})
	u.focus = uiFocusMain
	u.keyMap = DefaultKeyMap()

	cmd := u.handleKeyPressMsg(tea.KeyPressMsg{Code: 'b', Mod: tea.ModCtrl})

	require.True(t, u.dialog.ContainsDialog(dialog.SubagentsID))
	require.NotNil(t, cmd)
}

func TestCtrlBDoesNotOpenSubagentsWithoutSession(t *testing.T) {
	u := newTestUIWithWorkspace(&subagentsTestWorkspace{cfg: agentsTestConfig()})
	u.attachments = attachments.New(nil, attachments.Keymap{})
	u.focus = uiFocusMain
	u.keyMap = DefaultKeyMap()

	u.handleKeyPressMsg(tea.KeyPressMsg{Code: 'b', Mod: tea.ModCtrl})

	require.False(t, u.dialog.ContainsDialog(dialog.SubagentsID))
}

func TestSubagentsFetchErrorIsRoutedToDialog(t *testing.T) {
	ws := &subagentsTestWorkspace{listErr: errors.New("boom")}
	u := newTestUIWithWorkspace(ws)
	u.openSubagentsDialog()

	var fetch subagentsFetchMsg
	for _, msg := range runCmd(t, u.fetchSubagentsCmd()) {
		if f, ok := msg.(subagentsFetchMsg); ok {
			fetch = f
		}
	}
	require.Error(t, fetch.err)

	require.NotPanics(t, func() { u.applySubagentsFetch(fetch) })

	// Applying a result with no open dialog is a no-op.
	u.dialog.CloseDialog(dialog.SubagentsID)
	require.NotPanics(t, func() { u.applySubagentsFetch(subagentsFetchMsg{tasks: ws.tasks}) })
}

func TestSubagentsSelectionSwitchesSessionThroughLoadSession(t *testing.T) {
	ws := &subagentsTestWorkspace{tasks: []proto.TaskSnapshot{
		taskSnap("t1", "child-1"),
		taskSnap("t2", "child-2"),
	}}
	u := newTestUIWithWorkspace(ws)
	u.session = &session.Session{ID: "parent-1"}
	u.openSubagentsDialog()
	u.applySubagentsFetch(subagentsFetchMsg{tasks: ws.tasks})

	cmd := u.handleDialogMsg(tea.KeyPressMsg{Code: tea.KeyEnter})

	require.False(t, u.dialog.ContainsDialog(dialog.SubagentsID),
		"selecting a subagent must close the dialog")
	var loaded loadSessionMsg
	for _, msg := range runCmd(t, cmd) {
		if l, ok := msg.(loadSessionMsg); ok {
			loaded = l
		}
	}
	require.NotNil(t, loaded.session)
	require.Equal(t, "child-1", loaded.session.ID)
	require.Equal(t, "child-1", ws.loadedID, "the switch must go through the workspace session load")
}

func TestRefreshSubagentsCmdOnlyWhenDialogOpen(t *testing.T) {
	u := newTestUIWithWorkspace(&subagentsTestWorkspace{})
	require.Nil(t, u.refreshSubagentsCmd())

	u.openSubagentsDialog()
	require.NotNil(t, u.refreshSubagentsCmd())
}

func returnToParentKey() tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: ']', Mod: tea.ModCtrl}
}

func helpAdvertisesParentSession(t *testing.T, u *UI) bool {
	t.Helper()
	for _, row := range u.FullHelp() {
		for _, b := range row {
			if b.Help().Desc == "parent session" {
				return true
			}
		}
	}
	return false
}

func TestReturnToParentLoadsParentSession(t *testing.T) {
	ws := &subagentsTestWorkspace{cfg: agentsTestConfig()}
	u := newTestUIWithWorkspace(ws)
	u.session = &session.Session{ID: "child-1", ParentSessionID: "parent-1"}
	u.attachments = attachments.New(nil, attachments.Keymap{})
	u.focus = uiFocusMain
	u.keyMap = DefaultKeyMap()

	cmd := u.handleKeyPressMsg(returnToParentKey())
	require.NotNil(t, cmd)

	var loaded loadSessionMsg
	for _, msg := range runCmd(t, cmd) {
		if l, ok := msg.(loadSessionMsg); ok {
			loaded = l
		}
	}
	require.NotNil(t, loaded.session)
	require.Equal(t, "parent-1", loaded.session.ID)
	require.Equal(t, "parent-1", ws.loadedID)
}

func TestReturnToParentAdvertisedOnlyWithParent(t *testing.T) {
	ws := &subagentsTestWorkspace{cfg: agentsTestConfig()}
	u := newTestUIWithWorkspace(ws)
	u.attachments = attachments.New(nil, attachments.Keymap{})
	u.focus = uiFocusMain
	u.keyMap = DefaultKeyMap()

	u.session = &session.Session{ID: "s1"}
	require.False(t, helpAdvertisesParentSession(t, u))

	u.session = &session.Session{ID: "child-1", ParentSessionID: "parent-1"}
	require.True(t, helpAdvertisesParentSession(t, u))
}

func TestReturnToParentWithoutParentLoadsNothing(t *testing.T) {
	ws := &subagentsTestWorkspace{cfg: agentsTestConfig()}
	u := newTestUIWithWorkspace(ws)
	u.session = &session.Session{ID: "s1"}
	u.attachments = attachments.New(nil, attachments.Keymap{})
	u.focus = uiFocusMain
	u.keyMap = DefaultKeyMap()

	cmd := u.handleKeyPressMsg(returnToParentKey())
	for _, msg := range runCmd(t, cmd) {
		_, isLoad := msg.(loadSessionMsg)
		require.False(t, isLoad, "no parent must not trigger a session load")
	}
	require.Empty(t, ws.loadedID)
}
