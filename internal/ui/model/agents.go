package model

import (
	"context"
	"fmt"
	"log/slog"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/crush/internal/ui/dialog"
	"github.com/charmbracelet/crush/internal/ui/util"
)

// currentAgentMsg carries the workspace's active primary profile name
// back to the Update loop. The fetch runs off-thread (a synchronous
// HTTP round-trip in client/server mode); commands never touch model
// state directly.
type currentAgentMsg struct {
	profile string
}

// switchAgentMsg reports the outcome of a SetPrimaryAgent call back to
// the Update loop. On failure the dialog and the previous profile stay
// exactly as they were; the coordinator keeps the old agent active.
type switchAgentMsg struct {
	profile string
	err     error
}

// openAgentsDialog opens (or raises) the Switch Agent dialog. The
// profile list is projected from the in-memory config snapshot (no
// I/O) and the active profile is fetched via a tea.Cmd.
func (m *UI) openAgentsDialog() tea.Cmd {
	if m.dialog == nil {
		return nil
	}
	if m.dialog.ContainsDialog(dialog.AgentsID) {
		m.dialog.BringToFront(dialog.AgentsID)
		return m.fetchCurrentAgentCmd()
	}

	agents := dialog.NewAgents(m.com)
	agents.SetProfiles(m.agentProfileOptions())
	m.dialog.OpenDialog(agents)
	return m.fetchCurrentAgentCmd()
}

// agentProfileOptions lists the discoverable profiles with their
// resolved descriptions. All reads are pure in-memory projections of
// the config snapshot, safe to run on the Update goroutine.
func (m *UI) agentProfileOptions() []dialog.AgentProfileOption {
	cfg := m.com.Config()
	if cfg == nil {
		return nil
	}
	names := cfg.DiscoverableAgentProfiles()
	options := make([]dialog.AgentProfileOption, len(names))
	for i, name := range names {
		description := ""
		if profile, err := cfg.ResolvePrimaryAgentProfile(name); err == nil {
			description = profile.Agent.Description
		}
		options[i] = dialog.AgentProfileOption{Name: name, Description: description}
	}
	return options
}

// fetchCurrentAgentCmd reads the active primary profile off the
// workspace and reports it back through currentAgentMsg.
func (m *UI) fetchCurrentAgentCmd() tea.Cmd {
	if m.com == nil || m.com.Workspace == nil {
		return nil
	}
	ws := m.com.Workspace
	return func() tea.Msg {
		return currentAgentMsg{profile: ws.PrimaryAgent()}
	}
}

// applyCurrentAgent marks the fetched profile as active in the open
// Agents dialog. It is a no-op once the dialog has been closed.
func (m *UI) applyCurrentAgent(msg currentAgentMsg) {
	if m.dialog == nil {
		return
	}
	agents, ok := m.dialog.Dialog(dialog.AgentsID).(*dialog.Agents)
	if !ok || agents == nil {
		return
	}
	agents.SetCurrent(msg.profile)
}

// switchPrimaryAgentCmd asks the workspace to move the runtime primary
// agent to the named profile. The result lands as switchAgentMsg; the
// current session and messages are never touched by this path.
func (m *UI) switchPrimaryAgentCmd(profile string) tea.Cmd {
	ws := m.com.Workspace
	return func() tea.Msg {
		if err := ws.SetPrimaryAgent(context.Background(), profile); err != nil {
			slog.Error("Failed to switch primary agent", "profile", profile, "error", err)
			return switchAgentMsg{profile: profile, err: err}
		}
		return switchAgentMsg{profile: profile}
	}
}

// applySwitchAgent routes a switch outcome: on success the dialog
// closes and the memoized agent info is re-probed off-thread; on
// failure (busy, unknown/disabled profile, unreachable server) the
// dialog, session, and old profile all stay in place and the error is
// surfaced as a warning.
func (m *UI) applySwitchAgent(msg switchAgentMsg) []tea.Cmd {
	if msg.err != nil {
		return []tea.Cmd{util.ReportError(fmt.Errorf("failed to switch agent: %w", msg.err))}
	}

	m.dialog.CloseDialog(dialog.AgentsID)
	// The replacement agent may run a different model: invalidate the
	// memoized ready/model state and re-probe so the sidebar refreshes.
	m.invalidateBusyCaches()
	var cmds []tea.Cmd
	if cmd := m.dispatchBusyRefresh(); cmd != nil {
		cmds = append(cmds, cmd)
	}
	return append(cmds, util.CmdHandler(util.NewInfoMsg("Primary agent switched to "+msg.profile)))
}
