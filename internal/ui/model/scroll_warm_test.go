package model

import (
	"strconv"
	"strings"
	"testing"

	"github.com/charmbracelet/crush/internal/ui/chat"
	"github.com/stretchr/testify/require"
)

// countingMessageItem is a chat.MessageItem that counts Render calls so
// tests can bound how much work a scroll frame performs.
type countingMessageItem struct {
	id         string
	lines      int
	renderHits int
}

func (m *countingMessageItem) ID() string { return m.id }
func (m *countingMessageItem) Render(int) string {
	m.renderHits++
	lines := make([]string, m.lines)
	for i := range lines {
		lines[i] = m.id + ":" + strconv.Itoa(i)
	}
	return strings.Join(lines, "\n")
}
func (m *countingMessageItem) RawRender(width int) string { return m.Render(width) }
func (m *countingMessageItem) Version() uint64            { return 0 }
func (m *countingMessageItem) Finished() bool             { return true }

var _ chat.MessageItem = (*countingMessageItem)(nil)

func totalCountingHits(items []*countingMessageItem) int {
	n := 0
	for _, it := range items {
		n += it.renderHits
	}
	return n
}

// TestChatScroll_WarmsGeometryIncrementally proves a scroll that reveals
// the scrollbar never renders every item in one frame: the scroll frame
// itself is bounded, geometry warms in warmBatchSize steps, and once warm
// the scrollbar geometry is served from the memo with no further renders.
func TestChatScroll_WarmsGeometryIncrementally(t *testing.T) {
	t.Parallel()

	u := newTestUI()
	const n = 300
	items := make([]chat.MessageItem, 0, n)
	counted := make([]*countingMessageItem, 0, n)
	for i := range n {
		it := &countingMessageItem{id: "m" + strconv.Itoa(i), lines: 3}
		counted = append(counted, it)
		items = append(items, it)
	}
	u.chat.SetMessages(items...)
	u.updateLayoutAndSize()
	require.False(t, u.chat.list.TotalHeightReady(), "fresh session must not have geometry ready")

	// The scroll reveals the scrollbar and must schedule work (hide
	// timer plus the first warm step) instead of scanning inline.
	cmd := u.chat.ScrollBy(-5)
	require.NotNil(t, cmd, "scroll must schedule hide and warm commands")
	require.True(t, u.chat.scrollWarming, "scroll must start incremental warming")
	require.True(t, u.chat.RenderState().Warming, "warming must be part of the frame key")

	// The frame that follows the scroll returns immediately: bounded
	// work, far below a full scan, with no scrollbar yet.
	before := totalCountingHits(counted)
	_ = renderToBuffer(t, u.chat, 80, 20)
	require.Less(t, totalCountingHits(counted)-before, n, "scroll frame must not render every item")

	// A second scroll while warming restarts the hide timer but must
	// not schedule a duplicate warm.
	seq := u.chat.resizeSettleSeq
	next := u.chat.warmNext
	more := u.chat.ScrollBy(-5)
	require.NotNil(t, more, "scroll during warming must still arm the hide timer")
	require.Equal(t, seq, u.chat.resizeSettleSeq, "scroll during warming must not start a second warm")
	require.Equal(t, next, u.chat.warmNext, "scroll during warming must not reset warm progress")

	// Drive the scheduled warm chain to completion. Every step is
	// bounded to one batch; the total converges to the exact height.
	want := 0
	for _, it := range counted {
		want += it.lines
	}
	want += u.chat.list.Gap() * (n - 1)
	steps := 0
	for u.chat.scrollWarming {
		stepBefore := totalCountingHits(counted)
		c, done := u.chat.WarmStep(u.chat.resizeSettleSeq)
		_ = c
		steps++
		require.LessOrEqual(t, totalCountingHits(counted)-stepBefore, warmBatchSize,
			"warm step %d must render at most one batch", steps)
		if done {
			break
		}
		require.Less(t, steps, n/warmBatchSize+10, "warming must converge")
	}
	require.True(t, u.chat.list.TotalHeightReady())
	require.False(t, u.chat.RenderState().Warming)
	require.Equal(t, want, u.chat.list.TotalHeight(), "warmed geometry must be exact")

	// Steady state: geometry queries and draws are served from the
	// memo with no further renders, and the scrollbar is back.
	steady := totalCountingHits(counted)
	require.Equal(t, want, u.chat.list.TotalHeight())
	require.True(t, u.chat.list.Overflows(u.chat.list.Height()))
	require.Equal(t, steady, totalCountingHits(counted), "ready geometry must not re-render")
	_ = renderToBuffer(t, u.chat, 80, 20)
	require.Equal(t, steady, totalCountingHits(counted), "steady draw must not re-render")
}
