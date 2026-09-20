package model

import (
	"strconv"
	"strings"
	"testing"
	"time"

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
// itself is bounded, geometry warms in time-budgeted steps, and once
// warm the scrollbar geometry is served from the memo with no renders.
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

	// Drive the scheduled warm chain to completion. Every step
	// advances by at least one item and the total converges to the
	// exact height; cheap items may all warm in one budget window.
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
		require.Greater(t, totalCountingHits(counted)-stepBefore, 0,
			"warm step %d must make progress", steps)
		require.LessOrEqual(t, totalCountingHits(counted)-stepBefore, n,
			"warm step %d must stay within one budget window", steps)
		if done {
			break
		}
		require.LessOrEqual(t, steps, n, "warming must converge")
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

// slowMessageItem simulates a costly glamour or chroma render so a
// test can prove a warm step stops on elapsed time, not on a fixed
// item count. Each render sleeps longer than the whole warm budget.
type slowMessageItem struct {
	id         string
	lines      int
	renderHits int
	delay      time.Duration
}

func (m *slowMessageItem) ID() string { return m.id }
func (m *slowMessageItem) Render(int) string {
	m.renderHits++
	time.Sleep(m.delay)
	lines := make([]string, m.lines)
	for i := range lines {
		lines[i] = m.id + ":" + strconv.Itoa(i)
	}
	return strings.Join(lines, "\n")
}
func (m *slowMessageItem) RawRender(width int) string { return m.Render(width) }
func (m *slowMessageItem) Version() uint64            { return 0 }
func (m *slowMessageItem) Finished() bool             { return true }

var _ chat.MessageItem = (*slowMessageItem)(nil)

// TestChatWarmStep_TimeBudgeted proves one WarmStep is bounded by
// render work, not by a fixed item count: with per-item costs above
// the budget it renders only the first item, yet driving the chain
// still converges to the exact total height.
func TestChatWarmStep_TimeBudgeted(t *testing.T) {
	t.Parallel()

	u := newTestUI()
	// Enough items that the viewport (20 lines) cannot have
	// rendered them all before warming starts; otherwise the
	// budget step would serve cache hits and prove nothing.
	const n = 40
	items := make([]chat.MessageItem, 0, n)
	slow := make([]*slowMessageItem, 0, n)
	for i := range n {
		it := &slowMessageItem{id: "s" + strconv.Itoa(i), lines: 2, delay: 20 * time.Millisecond}
		slow = append(slow, it)
		items = append(items, it)
	}
	u.chat.SetMessages(items...)
	u.updateLayoutAndSize()

	cmd := u.chat.ScrollBy(-5)
	require.NotNil(t, cmd, "scroll must start warming")
	require.True(t, u.chat.scrollWarming)

	// One step must stop after the budget even though items remain.
	before := 0
	for _, it := range slow {
		before += it.renderHits
	}
	start := time.Now()
	c, done := u.chat.WarmStep(u.chat.resizeSettleSeq)
	elapsed := time.Since(start)
	_ = c
	stepRenders := 0
	for _, it := range slow {
		stepRenders += it.renderHits
	}
	stepRenders -= before
	require.False(t, done, "one step must not finish six slow items")
	require.LessOrEqual(t, stepRenders, 2, "slow items must stop on time, not on a fixed count")
	require.Less(t, elapsed, 500*time.Millisecond, "one step must stay near the budget")

	// The full chain still converges to the exact geometry.
	want := 0
	for _, it := range slow {
		want += it.lines
	}
	want += u.chat.list.Gap() * (n - 1)
	steps := 1
	for u.chat.scrollWarming {
		c, done := u.chat.WarmStep(u.chat.resizeSettleSeq)
		_ = c
		steps++
		if done {
			break
		}
		require.LessOrEqual(t, steps, n+1, "warming must converge")
	}
	require.True(t, u.chat.list.TotalHeightReady())
	require.Equal(t, want, u.chat.list.TotalHeight(), "warmed geometry must be exact")
}
