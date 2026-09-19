package list

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestList_GeometrySurvivesTextEviction proves the heights memo
// decouples geometry from rendered text: after the bounded text
// cache evicts measured entries, TotalHeight, Overflows, ScrollBy,
// Offset, and VisibleItemIndices answer from memory without
// re-rendering.
func TestList_GeometrySurvivesTextEviction(t *testing.T) {
	t.Parallel()

	const n = 3 * maxRenderCacheEntries
	items := make([]Item, n)
	tracked := make([]*trackedItem, n)
	for i := range n {
		it := newTrackedItem(strconv.Itoa(i), "body-"+strconv.Itoa(i), true)
		tracked[i] = it
		items[i] = it
	}
	l := NewList(items...)
	l.SetSize(40, 10)

	require.False(t, l.TotalHeightReady(), "fresh list must not report ready")
	want := l.TotalHeight()
	require.True(t, l.TotalHeightReady())
	require.Equal(t, n, totalRenderHits(tracked), "first TotalHeight measures every item once")

	// Churn the bounded text memo so early entries are evicted while
	// their heights stay memoized.
	l.ScrollToBottom()
	_ = l.Render()
	require.LessOrEqual(t, len(l.cache), maxRenderCacheEntries)
	require.Equal(t, n, len(l.heights), "heights memo must hold every measured item")

	before := totalRenderHits(tracked)
	require.Equal(t, want, l.TotalHeight())
	require.Equal(t, before, totalRenderHits(tracked), "TotalHeight must not re-render after text eviction")
	require.True(t, l.Overflows(10))
	require.Equal(t, before, totalRenderHits(tracked), "Overflows must not re-render after text eviction")
	l.ScrollBy(-5)
	l.ScrollBy(5)
	require.Equal(t, before, totalRenderHits(tracked), "ScrollBy must not re-render measured items")
	_ = l.Offset()
	require.Equal(t, before, totalRenderHits(tracked), "Offset must not re-render measured items")
	_, _ = l.VisibleItemIndices()
	require.Equal(t, before, totalRenderHits(tracked), "VisibleItemIndices must not re-render measured items")

	// A new unmeasured item still invalidates readiness.
	l.AppendItems(newTrackedItem("new", "newcomer", true))
	require.False(t, l.TotalHeightReady(), "appended item must invalidate readiness")
	require.Equal(t, want+1, l.TotalHeight())
}

// TestList_TotalHeightReadyTracksMutations pins the readiness flag to
// the same invalidation points as the cached total: width changes,
// item swaps, and removals all report not-ready until recomputed.
func TestList_TotalHeightReadyTracksMutations(t *testing.T) {
	t.Parallel()

	a := newTrackedItem("a", "alpha", true)
	b := newTrackedItem("b", "bravo", true)
	l := NewList(a, b)
	l.SetSize(40, 10)

	require.False(t, l.TotalHeightReady())
	_ = l.TotalHeight()
	require.True(t, l.TotalHeightReady())

	l.SetSize(80, 10)
	require.False(t, l.TotalHeightReady(), "width change must invalidate readiness")
	_ = l.TotalHeight()
	require.True(t, l.TotalHeightReady())

	l.RemoveItem(0)
	require.False(t, l.TotalHeightReady(), "removal must invalidate readiness")
}
