package dialog

import (
	"image"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/crush/internal/ui/common"
	"github.com/charmbracelet/crush/internal/ui/styles"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
)

func newAgentsDialogForTest(t *testing.T) *Agents {
	t.Helper()

	sty := styles.CharmtonePantera()
	return NewAgents(&common.Common{Styles: &sty})
}

func agentProfiles() []AgentProfileOption {
	return []AgentProfileOption{
		{Name: "coder", Description: "The everyday coding agent"},
		{Name: "task", Description: "Delegates child tasks"},
		{Name: "reviewer", Description: "Reviews finished work"},
	}
}

func TestAgentsEnterSelectsProfile(t *testing.T) {
	t.Parallel()

	dialog := newAgentsDialogForTest(t)
	dialog.SetProfiles(agentProfiles())

	action := dialog.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})

	selected, ok := action.(ActionSelectAgentProfile)
	require.True(t, ok, "enter must select the highlighted profile")
	require.Equal(t, "coder", selected.Profile)
}

func TestAgentsCurrentMarkerFollowsSetCurrent(t *testing.T) {
	t.Parallel()

	dialog := newAgentsDialogForTest(t)
	dialog.SetProfiles(agentProfiles())
	dialog.SetCurrent("task")

	items := dialog.list.FilteredItems()
	require.Len(t, items, 3)
	for _, item := range items {
		profileItem := item.(*AgentProfileItem)
		require.Equal(t, profileItem.ID() == "task", profileItem.current,
			"only the active profile carries the current marker")
	}
	require.Contains(t, ansi.Strip(items[1].Render(60)), "(current)")
	require.NotContains(t, ansi.Strip(items[0].Render(60)), "(current)")

	// A refresh re-applies the marker to the rebuilt items.
	dialog.SetProfiles(agentProfiles())
	require.True(t, dialog.list.FilteredItems()[1].(*AgentProfileItem).current)

	// Clearing falls back to no marker anywhere.
	dialog.SetCurrent("")
	for _, item := range dialog.list.FilteredItems() {
		require.False(t, item.(*AgentProfileItem).current)
	}
}

func TestAgentsFilterNarrowsByNameAndDescription(t *testing.T) {
	t.Parallel()

	dialog := newAgentsDialogForTest(t)
	dialog.SetProfiles(agentProfiles())

	// Filter by a word that only occurs in the description column.
	for _, r := range "Reviews" {
		require.IsType(t, ActionCmd{}, dialog.HandleMsg(tea.KeyPressMsg{Code: r, Text: string(r)}))
	}

	items := dialog.list.FilteredItems()
	require.Len(t, items, 1)
	require.Equal(t, "reviewer", items[0].(*AgentProfileItem).ID())

	// Selecting while filtered acts on the visible row only.
	action := dialog.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	selected, ok := action.(ActionSelectAgentProfile)
	require.True(t, ok)
	require.Equal(t, "reviewer", selected.Profile)
}

func TestAgentsSetProfilesKeepsSelection(t *testing.T) {
	t.Parallel()

	dialog := newAgentsDialogForTest(t)
	dialog.SetProfiles(agentProfiles())
	require.Nil(t, dialog.HandleMsg(tea.KeyPressMsg{Code: tea.KeyDown}))
	require.Equal(t, "task", dialog.list.SelectedItem().(*AgentProfileItem).ID())

	// A refresh that drops the selected row falls back to the top.
	dialog.SetProfiles([]AgentProfileOption{{Name: "coder"}})
	action := dialog.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Equal(t, ActionSelectAgentProfile{Profile: "coder"}, action)
}

func TestAgentsEmptyState(t *testing.T) {
	t.Parallel()

	dialog := newAgentsDialogForTest(t)
	require.Equal(t, "No agent profiles.", dialog.statusMessage())

	dialog.SetProfiles(agentProfiles())
	require.Empty(t, dialog.statusMessage())
}

func TestAgentsDoubleClickSelectsLikeEnter(t *testing.T) {
	t.Parallel()

	dialog := newAgentsDialogForTest(t)
	dialog.SetProfiles(agentProfiles())
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
	selected, ok := doubleClickAction.(ActionSelectAgentProfile)
	require.True(t, ok)
	require.Equal(t, "task", selected.Profile)
}

func TestAgentsDoubleClickExpires(t *testing.T) {
	t.Parallel()

	dialog := newAgentsDialogForTest(t)
	dialog.SetProfiles(agentProfiles())
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
	require.Equal(t, "task", dialog.list.SelectedItem().(*AgentProfileItem).ID())
}
