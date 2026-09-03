package task

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
)

func TestStatus_TerminalAndLive(t *testing.T) {
	t.Parallel()
	terminal := []Status{StatusCompleted, StatusFailed, StatusCancelled, StatusInterrupted}
	live := []Status{StatusPending, StatusRunning, StatusWaitingForInput}
	for _, s := range terminal {
		require.True(t, s.Terminal(), s)
		require.False(t, s.Live(), s)
	}
	for _, s := range live {
		require.False(t, s.Terminal(), s)
		require.True(t, s.Live(), s)
	}
}

func TestStatus_Transitions(t *testing.T) {
	t.Parallel()
	cases := []struct {
		from, to Status
		want     bool
	}{
		{StatusPending, StatusRunning, true},
		{StatusPending, StatusCancelled, true},
		{StatusPending, StatusFailed, true},
		{StatusPending, StatusInterrupted, true},
		{StatusPending, StatusCompleted, false},
		{StatusPending, StatusWaitingForInput, false},
		{StatusRunning, StatusWaitingForInput, true},
		{StatusRunning, StatusCompleted, true},
		{StatusRunning, StatusFailed, true},
		{StatusRunning, StatusCancelled, true},
		{StatusRunning, StatusInterrupted, true},
		{StatusRunning, StatusPending, false},
		{StatusWaitingForInput, StatusRunning, true},
		{StatusWaitingForInput, StatusFailed, true},
		{StatusWaitingForInput, StatusCancelled, true},
		{StatusWaitingForInput, StatusInterrupted, true},
		{StatusWaitingForInput, StatusCompleted, false},
		{StatusWaitingForInput, StatusPending, false},
	}
	for _, c := range cases {
		require.Equal(t, c.want, c.from.CanTransitionTo(c.to), "%s -> %s", c.from, c.to)
	}
}

func TestStatus_TerminalIsImmutable(t *testing.T) {
	t.Parallel()
	all := []Status{
		StatusPending, StatusRunning, StatusWaitingForInput,
		StatusCompleted, StatusFailed, StatusCancelled, StatusInterrupted,
	}
	for _, term := range []Status{StatusCompleted, StatusFailed, StatusCancelled, StatusInterrupted} {
		for _, to := range all {
			require.False(t, term.CanTransitionTo(to), "%s -> %s", term, to)
		}
	}
}

func TestTruncateResult(t *testing.T) {
	t.Parallel()
	ascii := strings.Repeat("a", MaxResultBytes)
	got, tr := TruncateResult(ascii)
	require.Equal(t, ascii, got)
	require.False(t, tr)

	short := strings.Repeat("a", MaxResultBytes-1)
	got, tr = TruncateResult(short)
	require.Equal(t, short, got)
	require.False(t, tr)

	// Two-byte runes straddling the limit: 32767 ASCII bytes plus two
	// 2-byte runes puts a rune boundary at 32767 and 32769.
	straddle := strings.Repeat("a", MaxResultBytes-1) + "éé"
	got, tr = TruncateResult(straddle)
	require.True(t, tr)
	require.True(t, utf8.ValidString(got))
	require.LessOrEqual(t, len(got), MaxResultBytes)
	require.Equal(t, MaxResultBytes-1, len(got))

	// A rune starting exactly at the limit must be dropped entirely.
	boundary := strings.Repeat("a", MaxResultBytes) + "é"
	got, tr = TruncateResult(boundary)
	require.True(t, tr)
	require.Equal(t, ascii, got)
}
