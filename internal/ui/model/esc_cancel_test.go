package model

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"
)

// escKey is the raw key press the terminal delivers for ESC. It matches
// the Chat.Cancel binding ("esc", "alt+esc").
func escKey() tea.Msg {
	return tea.KeyPressMsg{Code: tea.KeyEscape}
}

// pressEsc drives one ESC through Update the way the runtime would,
// except the arming timer command is dropped: executing it would block
// two wall-clock seconds (tea.Tick) and expiry is driven explicitly by
// delivering cancelTimerExpiredMsg instead.
func pressEsc(m *UI) tea.Cmd {
	_, cmd := m.Update(escKey())
	return cmd
}

// TestEscTwiceCancelsWhenReadyCacheStale pins the redundant ready gate:
// a turn is in flight (memoized busy true) but no probe has landed the
// readiness yet (construction seeded false, e.g. enter outran the boot
// probe). Both presses must still reach the cancel flow.
func TestEscTwiceCancelsWhenReadyCacheStale(t *testing.T) {
	pinTTLs(t)

	ws := &countingWorkspace{ready: true, agentBusy: true}
	m := newBusyUI(ws)
	warmCaches(m, true)
	m.agentReady = false // stale: turn running, readiness not landed.
	ws.resetCounters()

	pressEsc(m)
	require.True(t, m.isCanceling, "first ESC must arm even with stale readiness")

	_, cmd := m.Update(escKey())
	runCmds(m, cmd)
	require.False(t, m.isCanceling, "second ESC must disarm")
	require.Equal(t, 1, ws.cancelCalls, "second ESC must cancel the agent")
}

// TestEscTwiceCancelsDuringRetryBackoffWithStaleIdleCache pins ESC
// routing through a retry backoff whose only traffic is retry notices:
// the notices deliberately refresh nothing, so the busy cache may hold
// a stale idle value while the pinned notice proves a turn is in
// flight. Both presses must still reach the cancel flow.
func TestEscTwiceCancelsDuringRetryBackoffWithStaleIdleCache(t *testing.T) {
	pinTTLs(t)

	ws := &countingWorkspace{ready: true, agentBusy: true}
	m := newBusyUI(ws)
	warmCaches(m, false) // stale idle: no probe landed since the run started.
	m.agentReady = true
	m.retryNotice = true // pinned by TypeAgentRetrying: a turn is in flight.
	ws.resetCounters()

	pressEsc(m)
	require.True(t, m.isCanceling, "first ESC must arm while a retry notice is pinned")

	_, cmd := m.Update(escKey())
	runCmds(m, cmd)
	require.Equal(t, 1, ws.cancelCalls, "second ESC must cancel the backed-off turn")
}

// TestEscCancelClearsRetryNotice pins that cancelling a backed-off turn
// retires its notice: the run is dead, so the countdown must stop
// instead of ticking against a turn that no longer exists.
func TestEscCancelClearsRetryNotice(t *testing.T) {
	pinTTLs(t)

	ws := &countingWorkspace{ready: true, agentBusy: true}
	m := newBusyUI(ws)
	warmCaches(m, true)
	m.retryNotice = true
	ws.resetCounters()

	pressEsc(m)
	_, cmd := m.Update(escKey())
	runCmds(m, cmd)
	require.Equal(t, 1, ws.cancelCalls)
	require.False(t, m.retryNotice, "cancelling must retire the retry notice")
	require.Nil(t, m.applyRetryTick(), "a retired notice must stop the tick loop")
}

// TestEscArmingLifecycle pins the pre-existing successful paths end to
// end: first ESC arms, expiry disarms, second ESC within the window
// cancels, and an idle ESC does nothing at all.
func TestEscArmingLifecycle(t *testing.T) {
	pinTTLs(t)

	ws := &countingWorkspace{ready: true, agentBusy: true}
	m := newBusyUI(ws)
	warmCaches(m, true)
	ws.resetCounters()

	pressEsc(m)
	require.True(t, m.isCanceling, "first ESC must arm cancellation")

	_, cmd := m.Update(cancelTimerExpiredMsg{})
	runCmds(m, cmd)
	require.False(t, m.isCanceling, "expiry must disarm cancellation")
	require.Zero(t, ws.cancelCalls, "expiry must not cancel")

	pressEsc(m)
	_, cmd = m.Update(escKey())
	runCmds(m, cmd)
	require.Equal(t, 1, ws.cancelCalls, "second ESC within the window must cancel")
}

// TestIdleEscDoesNothing pins that the routing fix does not make ESC
// cancel-happy: with no turn in flight (idle cache, no notice) ESC must
// neither arm nor cancel.
func TestIdleEscDoesNothing(t *testing.T) {
	pinTTLs(t)

	ws := &countingWorkspace{ready: true}
	m := newBusyUI(ws)
	warmCaches(m, false)
	ws.resetCounters()

	_, cmd := m.Update(escKey())
	runCmds(m, cmd)
	require.False(t, m.isCanceling, "idle ESC must not arm cancellation")
	require.Zero(t, ws.cancelCalls, "idle ESC must not cancel")
}
