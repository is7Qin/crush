package list

import (
	"strings"
	"time"
)

// maxRenderCacheEntries bounds the F6 list-level render memo so
// resident memory stays proportional to the viewport, not to the
// total history length. A typical terminal shows well under 20
// items; 64 covers the visible window plus several viewports of
// scroll-back/forward without re-rendering, while keeping retained
// text a small constant. Evicted entries re-render on demand and
// are byte-identical because the (width, version) key still
// governs validity.
const maxRenderCacheEntries = 64

// List represents a list of items that can be lazily rendered. A list is
// always rendered like a chat conversation where items are stacked vertically
// from top to bottom.
type List struct {
	// Viewport size
	width, height int

	// Items in the list
	items []Item

	// Gap between items (0 or less means no gap)
	gap int

	// show list in reverse order
	reverse bool

	// Focus and selection state
	focused     bool
	selectedIdx int // The current selected index -1 means no selection

	// offsetIdx is the index of the first visible item in the viewport.
	offsetIdx int
	// offsetLine is the number of lines of the item at offsetIdx that are
	// scrolled out of view (above the viewport).
	// It must always be >= 0.
	offsetLine int

	// renderCallbacks is a list of callbacks to apply when rendering items.
	renderCallbacks []func(idx, selectedIdx int, item Item) Item

	// totalHeightCache is a cached value of the total rendered height of
	// all items. It is invalidated whenever the item set changes or the
	// viewport width changes (which can alter per-item line counts).
	totalHeightCache int
	totalHeightValid bool

	// cache is the F6 list-level render memo, keyed by item pointer.
	// Each entry stores one copy of the rendered content plus the
	// height and the keys that govern invalidation (width and
	// version). The line slice is derived lazily at draw time so
	// off-screen entries never retain a second copy of the text.
	// The frozen flag mirrors §4.5.1: once a Finished() item is
	// rendered, subsequent draws return the stored output verbatim
	// without calling back into Render. The memo is an LRU capped
	// at maxRenderCacheEntries; eviction re-renders byte-identical
	// output on next use. Recency is tracked by cacheSeq, bumped
	// on every entry use.
	cache    map[Item]*listCacheEntry
	cacheSeq uint64

	// heights is the non-evicted per-item geometry memo, keyed by
	// item pointer. Each entry is a few bytes (width, version,
	// height) with no rendered text, so it stays proportional to
	// the item count even on very long histories. Geometry readers
	// consult it first: once an item is measured, later queries
	// answer from memory even after the bounded text cache above
	// evicts the entry, and a full TotalHeight never re-renders
	// just to re-learn heights. Entries are pruned only when
	// their item leaves the list (RemoveItem, SetItems) or when
	// all geometry is dropped (width change).
	heights map[Item]heightEntry

	// freezeSuppressed marks items the list must not freeze on the
	// next render even when their Finished() reports true. This is
	// the §4.5.1 selection-drag escape hatch (option (a)): items
	// inside an active selection range render as live items so that
	// per-line highlight overlays land on the latest content. Cleared
	// on EndSelectionDrag.
	freezeSuppressed map[Item]struct{}
}

// listCacheEntry is the per-item entry in the list-level render memo.
type listCacheEntry struct {
	width    int
	version  uint64
	frozen   bool
	lastUsed uint64
	content  string
	height   int
}

// heightEntry is one row of the non-evicted geometry memo: the keys
// that govern validity (width, version) plus the measured height.
// No rendered text is retained.
type heightEntry struct {
	width   int
	version uint64
	height  int
}

// NewList creates a new lazy-loaded list.
func NewList(items ...Item) *List {
	l := new(List)
	l.items = items
	l.selectedIdx = -1
	l.cache = make(map[Item]*listCacheEntry)
	l.heights = make(map[Item]heightEntry)
	l.freezeSuppressed = make(map[Item]struct{})
	return l
}

// RenderCallback defines a function that can modify an item before it is
// rendered.
type RenderCallback func(idx, selectedIdx int, item Item) Item

// RegisterRenderCallback registers a callback to be called when rendering
// items. This can be used to modify items before they are rendered.
func (l *List) RegisterRenderCallback(cb RenderCallback) {
	l.renderCallbacks = append(l.renderCallbacks, cb)
}

// SetSize sets the size of the list viewport. A width change drops the
// entire render cache because every entry's wrapped output depends on
// width; a height-only change is a no-op for the cache.
func (l *List) SetSize(width, height int) {
	if l.width != width {
		l.invalidateAll()
	}
	l.width = width
	l.height = height
}

// SetGap sets the gap between items.
func (l *List) SetGap(gap int) {
	l.gap = gap
}

// Gap returns the gap between items.
func (l *List) Gap() int {
	return l.gap
}

// AtBottom returns whether the list is showing the last item at the bottom.
func (l *List) AtBottom() bool {
	if len(l.items) == 0 {
		return true
	}

	// Calculate the height from offsetIdx to the end. The comparison is
	// against the visible height (totalHeight minus the lines of the first
	// item that are scrolled out of view), otherwise a first item taller
	// than the viewport reports "not at bottom" while it is in fact
	// pinned there.
	var totalHeight int
	for idx := l.offsetIdx; idx < len(l.items); idx++ {
		if totalHeight-l.offsetLine > l.height {
			// No need to calculate further, we're already past the viewport height
			return false
		}
		itemHeight := l.heightAt(idx)
		if l.gap > 0 && idx > l.offsetIdx {
			itemHeight += l.gap
		}
		totalHeight += itemHeight
	}

	return totalHeight-l.offsetLine <= l.height
}

// SetReverse shows the list in reverse order.
func (l *List) SetReverse(reverse bool) {
	l.reverse = reverse
}

// Width returns the width of the list viewport.
func (l *List) Width() int {
	return l.width
}

// Height returns the height of the list viewport.
func (l *List) Height() int {
	return l.height
}

// Len returns the number of items in the list.
func (l *List) Len() int {
	return len(l.items)
}

// TotalHeightReady reports whether TotalHeight can be served from
// the memo without rendering any item. Callers that must not block
// the frame (e.g. a draw triggered by scrolling) check this first
// and defer the scan — via incremental Prewarm — while it is false.
func (l *List) TotalHeightReady() bool {
	return l.totalHeightValid
}

// TotalHeight returns the total height of all items in the list.
// The result is cached and only recomputed when the item set or
// viewport width changes. Items measured before answer from the
// non-evicted heights memo, so a recompute after eviction only
// renders items whose height is genuinely unknown.
func (l *List) TotalHeight() int {
	if l.totalHeightValid {
		return l.totalHeightCache
	}
	total := 0
	for idx := range l.items {
		total += l.heightAt(idx)
		if l.gap > 0 && idx < len(l.items)-1 {
			total += l.gap
		}
	}
	l.totalHeightCache = total
	l.totalHeightValid = true
	return total
}

// Prewarm renders items in the range [from, from+batch) into the width
// cache and returns the next index to warm (len(items) when done). It lets
// a caller populate the per-item render and heights memos incrementally
// across frames so a later TotalHeight is instant instead of rendering
// everything at once. Rendering is otherwise identical to what
// TotalHeight would do.
func (l *List) Prewarm(from, batch int) int {
	if from < 0 {
		from = 0
	}
	end := min(from+batch, len(l.items))
	for idx := from; idx < end; idx++ {
		l.renderItemEntry(idx)
	}
	return end
}

// PrewarmBudget renders items starting at from until budget elapses
// and returns the next index to warm (len(items) when done). At
// least one item renders per call so warming always converges, but
// no call does unbounded work: costly items (glamour, chroma)
// consume the budget after one or two renders while cheap items
// warm many per step. Callers spread geometry measurement across
// frames without dropping frames on slow items.
func (l *List) PrewarmBudget(from int, budget time.Duration) int {
	if from < 0 {
		from = 0
	}
	if from >= len(l.items) {
		return len(l.items)
	}
	start := time.Now()
	idx := from
	for idx < len(l.items) {
		l.renderItemEntry(idx)
		idx++
		if time.Since(start) >= budget {
			break
		}
	}
	return idx
}

// Overflows reports whether the items' total height exceeds the given
// viewport height. It walks from the bottom and stops as soon as the
// threshold is crossed, so for content taller than the viewport (the
// common case) it renders only a viewport's worth of items rather than
// all of them — much cheaper than TotalHeight when only the boolean is
// needed (e.g. deciding whether a scrollbar is required).
func (l *List) Overflows(height int) bool {
	total := 0
	for idx := len(l.items) - 1; idx >= 0; idx-- {
		total += l.heightAt(idx)
		if l.gap > 0 && idx < len(l.items)-1 {
			total += l.gap
		}
		if total > height {
			return true
		}
	}
	return false
}

// ItemsVersion folds every item's version into one number. It changes
// whenever any item in this list mutates in a way that affects its rendered
// output, since that is exactly the contract [Versioned.Bump] carries.
//
// It reads one field per item and renders nothing, so it is cheap enough to
// call once per frame. Callers that memoize a whole rendered frame fold it
// into their cache key to pick up item mutations they do not otherwise know
// about.
func (l *List) ItemsVersion() uint64 {
	var v uint64
	for _, item := range l.items {
		v = v*31 + item.Version()
	}
	return v
}

// ScrollPosition returns the index of the first visible item and the line
// offset into it. Unlike Offset it is O(1) and does not render items.
func (l *List) ScrollPosition() (offsetIdx, offsetLine int) {
	return l.offsetIdx, l.offsetLine
}

// Offset returns the current scroll offset in lines from the top.
func (l *List) Offset() int {
	offset := 0
	for idx := 0; idx < l.offsetIdx; idx++ {
		offset += l.heightAt(idx)
		if l.gap > 0 && idx < len(l.items)-1 {
			offset += l.gap
		}
	}
	offset += l.offsetLine
	return offset
}

// lastOffsetItem returns the index and line offsets of the last item that can
// be partially visible in the viewport.
func (l *List) lastOffsetItem() (int, int, int) {
	var totalHeight int
	var idx int
	for idx = len(l.items) - 1; idx >= 0; idx-- {
		itemHeight := l.heightAt(idx)
		if l.gap > 0 && idx < len(l.items)-1 {
			itemHeight += l.gap
		}
		totalHeight += itemHeight
		if totalHeight > l.height {
			break
		}
	}

	// Calculate line offset within the item
	lineOffset := max(totalHeight-l.height, 0)
	idx = max(idx, 0)

	return idx, lineOffset, totalHeight
}

// heightAt returns the height of the item at the given index for
// geometry queries (TotalHeight, Overflows, Offset, scrolling).
// Render callbacks run exactly as in renderItemEntry so per-frame
// state (focus, highlight) is discovered the same way; the
// post-callback (width, version) is then checked against the
// non-evicted heights memo. Hits return without touching rendered
// text, so geometry stays render-free after the bounded text cache
// evicts the entry. Misses render once via renderItemEntry, which
// populates both memos.
func (l *List) heightAt(idx int) int {
	if idx < 0 || idx >= len(l.items) {
		return 0
	}
	rawItem := l.items[idx]
	item := rawItem
	if len(l.renderCallbacks) > 0 {
		for _, cb := range l.renderCallbacks {
			if it := cb(idx, l.selectedIdx, item); it != nil {
				item = it
			}
		}
	}
	version := rawItem.Version()
	if h, ok := l.heights[rawItem]; ok && h.width == l.width && h.version == version {
		return h.height
	}
	entry := l.renderItemEntry(idx)
	if entry == nil {
		return 0
	}
	return entry.height
}

// renderItemEntry returns the cache entry for the given index, populating
// the cache on miss. The result must not be retained past the next
// invalidation (SetSize width change, SetItems, etc.).
//
// Render callbacks always run, even for frozen entries: callbacks
// are how the list discovers per-frame state changes (selection,
// highlight range) and they bump the item's version when those
// changes affect the rendered output. A frozen item whose callback
// run is a no-op (same focus, same highlight) keeps its stored
// version and the cache hit is preserved on the post-callback
// version check.
func (l *List) renderItemEntry(idx int) *listCacheEntry {
	if idx < 0 || idx >= len(l.items) {
		return nil
	}

	rawItem := l.items[idx]
	entry := l.cache[rawItem]

	// Run render callbacks. Callbacks may mutate the item (focus,
	// highlight) which in turn bumps its version when state actually
	// changes. We capture the post-callback version below.
	item := rawItem
	if len(l.renderCallbacks) > 0 {
		for _, cb := range l.renderCallbacks {
			if it := cb(idx, l.selectedIdx, item); it != nil {
				item = it
			}
		}
	}

	version := rawItem.Version()
	if entry != nil && entry.width == l.width && entry.version == version {
		// Cache hit — frozen or unfrozen, the entry content is
		// still correct because no version bump landed since the
		// last render. Selection-drag suppression turns this into
		// a miss only if the entry is frozen.
		if !entry.frozen {
			l.cacheSeq++
			entry.lastUsed = l.cacheSeq
			return entry
		}
		if _, suppressed := l.freezeSuppressed[rawItem]; !suppressed {
			l.cacheSeq++
			entry.lastUsed = l.cacheSeq
			return entry
		}
	}

	rendered := item.Render(l.width)
	rendered = strings.TrimRight(rendered, "\n")
	height := len(strings.Split(rendered, "\n"))

	// Re-read the version after Render so that any version bumps
	// caused by Render itself (e.g. an item that mutates internal
	// state during rendering) are captured. Without this we would
	// freeze a stale entry under the post-render version.
	finalVersion := rawItem.Version()

	frozen := false
	if rawItem.Finished() {
		if _, suppressed := l.freezeSuppressed[rawItem]; !suppressed {
			frozen = true
		}
	}

	if entry == nil {
		entry = &listCacheEntry{}
		l.cache[rawItem] = entry
	}
	// If the item's rendered height changed, the cached total height is
	// no longer valid and must be recomputed on the next TotalHeight call.
	if entry.height != height {
		l.totalHeightValid = false
	}
	entry.width = l.width
	entry.version = finalVersion
	entry.frozen = frozen
	entry.content = rendered
	entry.height = height
	l.cacheSeq++
	entry.lastUsed = l.cacheSeq
	l.evictOldest()
	// Record the geometry alongside the text so later queries
	// answer from the non-evicted heights memo after this entry
	// is evicted. The map is created lazily for zero-value Lists.
	if l.heights == nil {
		l.heights = make(map[Item]heightEntry)
	}
	l.heights[rawItem] = heightEntry{width: l.width, version: finalVersion, height: height}
	return entry
}

// evictOldest drops least-recently-used entries until the memo is
// back within maxRenderCacheEntries. Frozen entries are evictable:
// re-rendering one later reproduces the same bytes under the same
// (width, version) key.
func (l *List) evictOldest() {
	for len(l.cache) > maxRenderCacheEntries {
		var oldest Item
		var oldestUsed uint64
		first := true
		for k, e := range l.cache {
			if first || e.lastUsed < oldestUsed {
				oldest = k
				oldestUsed = e.lastUsed
				first = false
			}
		}
		delete(l.cache, oldest)
	}
}

// invalidateAll drops every cache entry. Called on width changes.
func (l *List) invalidateAll() {
	for k := range l.cache {
		delete(l.cache, k)
	}
	for k := range l.heights {
		delete(l.heights, k)
	}
	l.totalHeightValid = false
}

// Invalidate drops the cache entry for the given item, forcing a
// re-render on the next getItem call. No-op if the item is not in
// the cache.
func (l *List) Invalidate(item Item) {
	delete(l.cache, item)
	delete(l.heights, item)
}

// InvalidateFrozen drops the frozen flag (and stored content) for the
// given item. Equivalent to Invalidate but exposed under the F6
// frozen-items vocabulary so external callers can express intent.
func (l *List) InvalidateFrozen(item Item) {
	delete(l.cache, item)
	delete(l.heights, item)
}

// retainCacheFor drops every cache entry whose key is not in the given
// item set. Used by SetItems to keep entries for stable items while
// dropping entries for removed ones.
func (l *List) retainCacheFor(items []Item) {
	if len(l.cache) == 0 && len(l.heights) == 0 {
		return
	}
	keep := make(map[Item]struct{}, len(items))
	for _, it := range items {
		keep[it] = struct{}{}
	}
	for k := range l.cache {
		if _, ok := keep[k]; !ok {
			delete(l.cache, k)
		}
	}
	for k := range l.heights {
		if _, ok := keep[k]; !ok {
			delete(l.heights, k)
		}
	}
}

// BeginSelectionDrag marks the items in the inclusive [startIdx, endIdx]
// range as un-freezable for the duration of an active selection drag.
// Frozen entries inside the range are dropped so the next render
// reflects live selection-overlay output. The corresponding
// EndSelectionDrag clears the suppression set and lets items
// re-freeze on their next render. Indices outside the items slice
// are clipped silently.
func (l *List) BeginSelectionDrag(startIdx, endIdx int) {
	if len(l.items) == 0 {
		return
	}
	if startIdx > endIdx {
		startIdx, endIdx = endIdx, startIdx
	}
	startIdx = max(startIdx, 0)
	endIdx = min(endIdx, len(l.items)-1)
	for i := startIdx; i <= endIdx; i++ {
		it := l.items[i]
		l.freezeSuppressed[it] = struct{}{}
		// Drop any cached frozen entry so the next render rebuilds
		// it as a live (un-frozen) entry that picks up the
		// selection overlay.
		if entry, ok := l.cache[it]; ok && entry.frozen {
			delete(l.cache, it)
		}
	}
}

// EndSelectionDrag clears the selection-drag freeze suppression. Items
// inside the previous range will re-freeze on their next render once
// their Finished() reports true again.
func (l *List) EndSelectionDrag() {
	for k := range l.freezeSuppressed {
		delete(l.freezeSuppressed, k)
		// Drop the cache entry so the next render produces a clean
		// (un-highlighted) frozen entry.
		delete(l.cache, k)
	}
}

// ScrollToIndex scrolls the list to the given item index.
func (l *List) ScrollToIndex(index int) {
	if index < 0 {
		index = 0
	}
	if index >= len(l.items) {
		index = len(l.items) - 1
	}
	l.offsetIdx = index
	l.offsetLine = 0
}

// ScrollBy scrolls the list by the given number of lines.
func (l *List) ScrollBy(lines int) {
	if len(l.items) == 0 || lines == 0 {
		return
	}

	if l.reverse {
		lines = -lines
	}

	if lines > 0 {
		if l.AtBottom() {
			// Already at bottom
			return
		}

		// Scroll down
		l.offsetLine += lines
		currentHeight := l.heightAt(l.offsetIdx)
		for l.offsetLine >= currentHeight {
			l.offsetLine -= currentHeight
			if l.gap > 0 {
				l.offsetLine = max(0, l.offsetLine-l.gap)
			}

			// Move to next item
			l.offsetIdx++
			if l.offsetIdx > len(l.items)-1 {
				// Reached bottom
				l.ScrollToBottom()
				return
			}
			currentHeight = l.heightAt(l.offsetIdx)
		}

		lastOffsetIdx, lastOffsetLine, _ := l.lastOffsetItem()
		if l.offsetIdx > lastOffsetIdx || (l.offsetIdx == lastOffsetIdx && l.offsetLine > lastOffsetLine) {
			// Clamp to bottom
			l.offsetIdx = lastOffsetIdx
			l.offsetLine = lastOffsetLine
		}
	} else if lines < 0 {
		// Scroll up
		l.offsetLine += lines // lines is negative
		for l.offsetLine < 0 {
			// Move to previous item
			l.offsetIdx--
			if l.offsetIdx < 0 {
				// Reached top
				l.ScrollToTop()
				break
			}
			totalHeight := l.heightAt(l.offsetIdx)
			if l.gap > 0 {
				totalHeight += l.gap
			}
			l.offsetLine += totalHeight
		}
	}
}

// VisibleItemIndices finds the range of items that are visible in the viewport.
// This is used for checking if selected item is in view.
func (l *List) VisibleItemIndices() (startIdx, endIdx int) {
	if len(l.items) == 0 {
		return 0, 0
	}

	startIdx = l.offsetIdx
	currentIdx := startIdx
	visibleHeight := -l.offsetLine

	for currentIdx < len(l.items) {
		itemHeight := l.heightAt(currentIdx)
		visibleHeight += itemHeight
		if l.gap > 0 {
			visibleHeight += l.gap
		}

		if visibleHeight >= l.height {
			break
		}
		currentIdx++
	}

	endIdx = currentIdx
	if endIdx >= len(l.items) {
		endIdx = len(l.items) - 1
	}

	return startIdx, endIdx
}

// Render renders the list and returns the visible lines.
//
// F7: per-item slicing is bounded by the remaining viewport budget so
// per-frame work is O(viewport) rather than O(total item heights).
// We never append beyond l.height lines to the output buffer; the
// final trim is therefore unnecessary. Reverse mode applies the same
// final reversal as before, which is byte-identical because the
// pre-F7 trim happened at the tail of the joined buffer (the same
// lines we now drop implicitly per item).
func (l *List) Render() string {
	if len(l.items) == 0 {
		return ""
	}

	budget := max(l.height, 0)
	lines := make([]string, 0, budget)
	currentIdx := l.offsetIdx
	currentOffset := l.offsetLine

	for currentIdx < len(l.items) {
		remaining := budget - len(lines)
		if remaining <= 0 {
			break
		}

		entry := l.renderItemEntry(currentIdx)
		if entry == nil {
			break
		}
		// Split the single retained copy lazily; only visible
		// items pay for this per frame.
		itemLines := strings.Split(entry.content, "\n")
		itemHeight := len(itemLines)

		if currentOffset >= 0 && currentOffset < itemHeight {
			// Append only the visible slice that fits in the
			// remaining viewport budget. Anything past the
			// budget would be discarded by the pre-F7 tail
			// trim, so skipping the append here is
			// byte-identical and bounded.
			visible := itemLines[currentOffset:]
			if len(visible) > remaining {
				visible = visible[:remaining]
			}
			lines = append(lines, visible...)

			// Gap rows after the item, capped to the
			// remaining budget so a 30k-line item with a
			// trailing gap can't push past the viewport.
			if l.gap > 0 {
				gapBudget := min(budget-len(lines), l.gap)
				for range gapBudget {
					lines = append(lines, "")
				}
			}
		} else {
			// offsetLine starts inside the gap.
			gapOffset := currentOffset - itemHeight
			gapRemaining := l.gap - gapOffset
			if gapRemaining > 0 {
				gapBudget := min(budget-len(lines), gapRemaining)
				for range gapBudget {
					lines = append(lines, "")
				}
			}
		}

		currentIdx++
		currentOffset = 0 // Reset offset for subsequent items.
	}

	l.height = budget

	if l.reverse {
		// Reverse the lines so the list renders bottom-to-top.
		for i, j := 0, len(lines)-1; i < j; i, j = i+1, j-1 {
			lines[i], lines[j] = lines[j], lines[i]
		}
	}

	return strings.Join(lines, "\n")
}

// PrependItems prepends items to the list.
func (l *List) PrependItems(items ...Item) {
	l.items = append(items, l.items...)

	// Keep view position relative to the content that was visible
	l.offsetIdx += len(items)

	// Update selection index if valid
	if l.selectedIdx != -1 {
		l.selectedIdx += len(items)
	}
	l.totalHeightValid = false
}

// SetItems sets the items in the list. Cache entries for items that
// remain after the swap are preserved; entries for removed items are
// dropped.
func (l *List) SetItems(items ...Item) {
	l.items = items
	l.selectedIdx = min(l.selectedIdx, len(l.items)-1)
	l.offsetIdx = min(l.offsetIdx, len(l.items)-1)
	l.offsetLine = 0
	l.retainCacheFor(items)
	l.totalHeightValid = false
}

// AppendItems appends items to the list.
func (l *List) AppendItems(items ...Item) {
	l.items = append(l.items, items...)
	l.totalHeightValid = false
}

// RemoveItem removes the item at the given index from the list.
func (l *List) RemoveItem(idx int) {
	if idx < 0 || idx >= len(l.items) {
		return
	}

	removed := l.items[idx]

	// Remove the item
	l.items = append(l.items[:idx], l.items[idx+1:]...)

	// Drop the cache entry for the removed item; entries for stable
	// items stay valid because they are keyed by pointer, not index.
	delete(l.cache, removed)
	delete(l.heights, removed)
	delete(l.freezeSuppressed, removed)

	// Adjust selection if needed
	if l.selectedIdx == idx {
		l.selectedIdx = -1
	} else if l.selectedIdx > idx {
		l.selectedIdx--
	}

	// Adjust offset if needed
	if l.offsetIdx > idx {
		l.offsetIdx--
	} else if l.offsetIdx == idx && l.offsetIdx >= len(l.items) {
		l.offsetIdx = max(0, len(l.items)-1)
		l.offsetLine = 0
	}
	l.totalHeightValid = false
}

// Focused returns whether the list is focused.
func (l *List) Focused() bool {
	return l.focused
}

// Focus sets the focus state of the list.
func (l *List) Focus() {
	l.focused = true
}

// Blur removes the focus state from the list.
func (l *List) Blur() {
	l.focused = false
}

// ScrollToTop scrolls the list to the top.
func (l *List) ScrollToTop() {
	l.offsetIdx = 0
	l.offsetLine = 0
}

// ScrollToBottom scrolls the list to the bottom.
func (l *List) ScrollToBottom() {
	if len(l.items) == 0 {
		return
	}

	lastOffsetIdx, lastOffsetLine, _ := l.lastOffsetItem()
	l.offsetIdx = lastOffsetIdx
	l.offsetLine = lastOffsetLine
}

// ScrollToSelected scrolls the list to the selected item.
func (l *List) ScrollToSelected() {
	if l.selectedIdx < 0 || l.selectedIdx >= len(l.items) {
		return
	}

	// The list may not have been sized yet when the caller sets up its
	// selection, e.g. a dialog constructor that runs before the first
	// Draw. With no viewport height there is no visibility window to fit
	// the selection into, so pin the selected item to the top of the
	// viewport; the first render then shows it instead of computing a
	// bogus offset that skips past it entirely.
	if l.height <= 0 {
		l.offsetIdx = l.selectedIdx
		l.offsetLine = 0
		return
	}

	startIdx, endIdx := l.VisibleItemIndices()
	if l.selectedIdx < startIdx {
		// Selected item is above the visible range
		l.offsetIdx = l.selectedIdx
		l.offsetLine = 0
	} else if l.selectedIdx > endIdx {
		// Selected item is below the visible range
		// Scroll so that the selected item is at the bottom
		var totalHeight int
		for i := l.selectedIdx; i >= 0; i-- {
			totalHeight += l.heightAt(i)
			if l.gap > 0 && i < l.selectedIdx {
				totalHeight += l.gap
			}
			if totalHeight >= l.height {
				l.offsetIdx = i
				l.offsetLine = totalHeight - l.height
				break
			}
		}
		if totalHeight < l.height {
			// All items fit in the viewport
			l.ScrollToTop()
		}
	}
}

// SelectedItemInView returns whether the selected item is currently in view.
func (l *List) SelectedItemInView() bool {
	if l.selectedIdx < 0 || l.selectedIdx >= len(l.items) {
		return false
	}
	startIdx, endIdx := l.VisibleItemIndices()
	return l.selectedIdx >= startIdx && l.selectedIdx <= endIdx
}

// SetSelected sets the selected item index in the list.
// It returns -1 if the index is out of bounds.
func (l *List) SetSelected(index int) {
	if index < 0 || index >= len(l.items) {
		l.selectedIdx = -1
	} else {
		l.selectedIdx = index
	}
}

// Selected returns the index of the currently selected item. It returns -1 if
// no item is selected.
func (l *List) Selected() int {
	return l.selectedIdx
}

// IsSelectedFirst returns whether the first item is selected.
func (l *List) IsSelectedFirst() bool {
	return l.selectedIdx == 0
}

// IsSelectedLast returns whether the last item is selected.
func (l *List) IsSelectedLast() bool {
	return l.selectedIdx == len(l.items)-1
}

// SelectPrev selects the visually previous item (moves toward visual top).
// It returns whether the selection changed.
func (l *List) SelectPrev() bool {
	if l.reverse {
		// In reverse, visual up = higher index
		if l.selectedIdx < len(l.items)-1 {
			l.selectedIdx++
			return true
		}
	} else {
		// Normal: visual up = lower index
		if l.selectedIdx > 0 {
			l.selectedIdx--
			return true
		}
	}
	return false
}

// SelectNext selects the next item in the list.
// It returns whether the selection changed.
func (l *List) SelectNext() bool {
	if l.reverse {
		// In reverse, visual down = lower index
		if l.selectedIdx > 0 {
			l.selectedIdx--
			return true
		}
	} else {
		// Normal: visual down = higher index
		if l.selectedIdx < len(l.items)-1 {
			l.selectedIdx++
			return true
		}
	}
	return false
}

// SelectFirst selects the first item in the list.
// It returns whether the selection changed.
func (l *List) SelectFirst() bool {
	if len(l.items) == 0 {
		return false
	}
	l.selectedIdx = 0
	return true
}

// SelectLast selects the last item in the list (highest index).
// It returns whether the selection changed.
func (l *List) SelectLast() bool {
	if len(l.items) == 0 {
		return false
	}
	l.selectedIdx = len(l.items) - 1
	return true
}

// WrapToStart wraps selection to the visual start (for circular navigation).
// In normal mode, this is index 0. In reverse mode, this is the highest index.
func (l *List) WrapToStart() bool {
	if len(l.items) == 0 {
		return false
	}
	if l.reverse {
		l.selectedIdx = len(l.items) - 1
	} else {
		l.selectedIdx = 0
	}
	return true
}

// WrapToEnd wraps selection to the visual end (for circular navigation).
// In normal mode, this is the highest index. In reverse mode, this is index 0.
func (l *List) WrapToEnd() bool {
	if len(l.items) == 0 {
		return false
	}
	if l.reverse {
		l.selectedIdx = 0
	} else {
		l.selectedIdx = len(l.items) - 1
	}
	return true
}

// SelectedItem returns the currently selected item. It may be nil if no item
// is selected.
func (l *List) SelectedItem() Item {
	if l.selectedIdx < 0 || l.selectedIdx >= len(l.items) {
		return nil
	}
	return l.items[l.selectedIdx]
}

// SelectFirstInView selects the first item currently in view.
func (l *List) SelectFirstInView() {
	startIdx, _ := l.VisibleItemIndices()
	l.selectedIdx = startIdx
}

// SelectLastInView selects the last item currently in view.
func (l *List) SelectLastInView() {
	_, endIdx := l.VisibleItemIndices()
	l.selectedIdx = endIdx
}

// ItemAt returns the item at the given index.
func (l *List) ItemAt(index int) Item {
	if index < 0 || index >= len(l.items) {
		return nil
	}
	return l.items[index]
}

// ItemIndexAtPosition returns the item at the given viewport-relative y
// coordinate. Returns the item index and the y offset within that item. It
// returns -1, -1 if no item is found.
func (l *List) ItemIndexAtPosition(x, y int) (itemIdx int, itemY int) {
	return l.findItemAtY(x, y)
}

// findItemAtY finds the item at the given viewport y coordinate.
// Returns the item index and the y offset within that item. It returns -1, -1
// if no item is found.
func (l *List) findItemAtY(_, y int) (itemIdx int, itemY int) {
	if y < 0 || y >= l.height {
		return -1, -1
	}

	// Walk through visible items to find which one contains this y
	currentIdx := l.offsetIdx
	currentLine := -l.offsetLine // Negative because offsetLine is how many lines are hidden

	for currentIdx < len(l.items) && currentLine < l.height {
		itemHeight := l.heightAt(currentIdx)
		itemEndLine := currentLine + itemHeight

		// Check if y is within this item's visible range
		if y >= currentLine && y < itemEndLine {
			// Found the item, calculate itemY (offset within the item)
			itemY = y - currentLine
			return currentIdx, itemY
		}

		// Move to next item
		currentLine = itemEndLine
		if l.gap > 0 {
			currentLine += l.gap
		}
		currentIdx++
	}

	return -1, -1
}
