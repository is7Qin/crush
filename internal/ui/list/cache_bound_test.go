package list

import (
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// retainedBytes sums the rendered text held by the memo. With the
// lines slice gone each entry holds a single copy of its output.
func retainedBytes(l *List) int {
	n := 0
	for _, e := range l.cache {
		n += len(e.content)
	}
	return n
}

// longList builds a list of n finished (freezable) multi-line items.
func longList(n int) ([]Item, []*multiLineItem) {
	items := make([]Item, n)
	typed := make([]*multiLineItem, n)
	for i := range n {
		it := newMultiLineItem("item-"+strconv.Itoa(i), 5)
		typed[i] = it
		items[i] = it
	}
	return items, typed
}

// TestList_CacheBoundedWhileScrolling scrolls a long list end to end
// and asserts the memo never grows with the item count: entry count
// stays within the LRU cap and retained bytes stay within one copy
// per capped entry.
func TestList_CacheBoundedWhileScrolling(t *testing.T) {
	t.Parallel()

	const n = 300
	items, _ := longList(n)
	l := NewList(items...)
	l.SetSize(40, 10)

	maxItemBytes := 0
	for i := range n {
		if b := len(items[i].Render(40)); b > maxItemBytes {
			maxItemBytes = b
		}
	}

	for i := range n {
		l.ScrollToIndex(i)
		_ = l.Render()
		require.LessOrEqual(t, len(l.cache), maxRenderCacheEntries,
			"cache must stay capped while scrolling at index %d", i)
		require.LessOrEqual(t, retainedBytes(l), maxRenderCacheEntries*maxItemBytes,
			"retained bytes must stay bounded at index %d", i)
	}
}

// TestList_EvictedItemRerendersIdentically scrolls a frozen item out
// of the working set until its memo entry is evicted, scrolls back,
// and asserts the output is byte-identical (frozen-item stability).
func TestList_EvictedItemRerendersIdentically(t *testing.T) {
	t.Parallel()

	const n = 3 * maxRenderCacheEntries
	items, _ := longList(n)
	l := NewList(items...)
	l.SetSize(40, 10)

	l.ScrollToTop()
	first := l.Render()
	heightBefore := l.TotalHeight()
	overflowsBefore := l.Overflows(10)

	// Scroll to the bottom so the top items' entries are evicted.
	l.ScrollToBottom()
	_ = l.Render()
	require.LessOrEqual(t, len(l.cache), maxRenderCacheEntries)

	// Scroll back; evicted items must re-render identically.
	l.ScrollToTop()
	require.Equal(t, first, l.Render(),
		"evicted items must re-render byte-identical output")
	require.Equal(t, heightBefore, l.TotalHeight(),
		"scrollbar geometry must survive eviction")
	require.Equal(t, overflowsBefore, l.Overflows(10))
}

// TestList_TotalHeightStableAcrossEviction asserts TotalHeight walks
// the whole item set correctly even when the memo cannot hold it all.
func TestList_TotalHeightStableAcrossEviction(t *testing.T) {
	t.Parallel()

	const n = 4 * maxRenderCacheEntries
	items, _ := longList(n)
	l := NewList(items...)
	l.SetSize(40, 10)

	want := 0
	for _, it := range items {
		// Each item renders 5 lines at any width.
		want += len(strings.Split(it.Render(40), "\n"))
	}
	require.Equal(t, want, l.TotalHeight())
	require.Equal(t, want, l.TotalHeight(), "second call is served from the total cache")

	// Warm far-ahead entries to churn the memo, then confirm the
	// total is unchanged.
	l.ScrollToBottom()
	_ = l.Render()
	require.Equal(t, want, l.TotalHeight())
	require.LessOrEqual(t, len(l.cache), maxRenderCacheEntries)
}
