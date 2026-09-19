package dialog

import (
	"fmt"
	"image"
	"sort"
	"strings"
	"time"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/crush/internal/proto"
	"github.com/charmbracelet/crush/internal/ui/common"
	"github.com/charmbracelet/crush/internal/ui/list"
	"github.com/charmbracelet/crush/internal/ui/styles"
	"github.com/charmbracelet/crush/internal/ui/util"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/sahilm/fuzzy"
)

// SubagentsID is the identifier for the subagent task picker dialog.
const SubagentsID = "subagents"

// Subagents lists the current owner session's durable call_agent task
// snapshots. Selecting an entry switches the UI to that task's child
// session. Task data is pushed in from the main model (via SetTasks /
// SetError) after a Workspace.TaskList read; the dialog itself never
// performs I/O.
type Subagents struct {
	com   *common.Common
	help  help.Model
	list  *list.FilterableList
	input textinput.Model

	loading  bool
	fetchErr string

	// tasks holds the full ordered snapshot set. The filterable list
	// only shows a capped window of it while the query is empty; a
	// non-empty query searches everything in tasks.
	tasks []proto.TaskSnapshot

	bodyArea      image.Rectangle
	mouseScrolled bool
	lastClickTime time.Time
	lastClickID   string

	keyMap struct {
		Select   key.Binding
		Next     key.Binding
		Previous key.Binding
		UpDown   key.Binding
		Close    key.Binding
	}
}

var _ Dialog = (*Subagents)(nil)

// NewSubagents creates a Subagents dialog in its loading state.
func NewSubagents(com *common.Common) *Subagents {
	s := &Subagents{com: com, loading: true}

	h := help.New()
	h.Styles = com.Styles.DialogHelpStyles()
	s.help = h

	s.list = list.NewFilterableList()
	s.list.Focus()

	s.input = textinput.New()
	s.input.SetVirtualCursor(false)
	s.input.Placeholder = "Type to filter"
	s.input.SetStyles(com.Styles.TextInput)
	s.input.Focus()

	s.keyMap.Select = key.NewBinding(
		key.WithKeys("enter", "tab", "ctrl+y"),
		key.WithHelp("enter", "open child"),
	)
	s.keyMap.Next = key.NewBinding(
		key.WithKeys("down", "ctrl+n"),
		key.WithHelp("↓", "next item"),
	)
	s.keyMap.Previous = key.NewBinding(
		key.WithKeys("up", "ctrl+p"),
		key.WithHelp("↑", "previous item"),
	)
	s.keyMap.UpDown = key.NewBinding(
		key.WithKeys("up", "down"),
		key.WithHelp("↑/↓", "choose"),
	)
	s.keyMap.Close = CloseKey

	return s
}

// ID implements Dialog.
func (s *Subagents) ID() string {
	return SubagentsID
}

// SetTasks replaces the listed snapshots, keeping the selection on the
// same task id when it is still present after the refresh. Snapshots
// are ordered live-first with finished newest-first, and the finished
// section is capped while the filter query is empty (see
// applyTaskVisibility).
func (s *Subagents) SetTasks(tasks []proto.TaskSnapshot) {
	selectedID := ""
	if item, ok := s.list.SelectedItem().(*SubagentItem); ok && item != nil {
		selectedID = item.ID()
	}

	s.loading = false
	s.fetchErr = ""
	s.tasks = orderSubagentSnapshots(tasks)
	s.applyTaskVisibility()
	if index := s.indexOfTask(selectedID); index >= 0 {
		s.list.SetSelected(index)
	} else {
		s.list.SetSelected(0)
	}
	s.list.ScrollToSelected()
}

// applyTaskVisibility rebuilds the filterable items from the full
// ordered snapshot set for the current query. With an empty query the
// finished section is capped at maxFinishedSubagents with one summary
// row appended for the remainder; with a non-empty query every task
// is listed (no cap, no summary row) so filtering reaches all tasks.
func (s *Subagents) applyTaskVisibility() {
	query := s.input.Value()
	if query != "" {
		s.list.SetItems(subagentItems(s.com.Styles, s.tasks)...)
		s.list.SetFilter(query)
		return
	}
	visible := make([]proto.TaskSnapshot, 0, len(s.tasks))
	hidden := 0
	shownFinished := 0
	for _, snapshot := range s.tasks {
		if !isLiveSubagentStatus(snapshot.Status) {
			if shownFinished >= maxFinishedSubagents {
				hidden++
				continue
			}
			shownFinished++
		}
		visible = append(visible, snapshot)
	}
	items := subagentItems(s.com.Styles, visible)
	if hidden > 0 {
		items = append(items, &subagentsMoreItem{
			Versioned: list.NewVersioned(),
			count:     hidden,
			t:         s.com.Styles,
		})
	}
	// Always re-apply the current query (even when empty) so a stale
	// list-level filter can never hide rows after a refresh.
	s.list.SetItems(items...)
	s.list.SetFilter(query)
}

// SetError moves the dialog into its error state, shown until the next
// SetTasks.
func (s *Subagents) SetError(err error) {
	s.loading = false
	s.fetchErr = err.Error()
}

// indexOfTask returns the filtered-list position of the task with the
// given id, or -1 when it is not listed.
func (s *Subagents) indexOfTask(id string) int {
	if id == "" {
		return -1
	}
	for i, item := range s.list.FilteredItems() {
		if taskItem, ok := item.(*SubagentItem); ok && taskItem != nil && taskItem.ID() == id {
			return i
		}
	}
	return -1
}

// statusMessage returns the placeholder body line for the loading,
// error, and empty states, or "" when the task list should render. The
// result is always a single line, safe for the dialog frame.
func (s *Subagents) statusMessage() string {
	switch {
	case s.loading:
		return "Loading subagents…"
	case s.fetchErr != "":
		return "Failed to load subagents: " + strings.Join(strings.Fields(s.fetchErr), " ")
	case len(s.list.FilteredItems()) == 0 && s.input.Value() == "":
		return "No subagent tasks."
	case len(s.list.FilteredItems()) == 0:
		return "No subagent tasks match the filter."
	}
	return ""
}

// HandleMsg implements Dialog.
func (s *Subagents) HandleMsg(msg tea.Msg) Action {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch {
		case key.Matches(msg, s.keyMap.Close):
			return ActionClose{}
		case key.Matches(msg, s.keyMap.Previous):
			s.list.Focus()
			if s.list.IsSelectedFirst() {
				s.list.SelectLast()
			} else {
				s.list.SelectPrev()
			}
			s.list.ScrollToSelected()
		case key.Matches(msg, s.keyMap.Next):
			s.list.Focus()
			if s.list.IsSelectedLast() {
				s.list.SelectFirst()
			} else {
				s.list.SelectNext()
			}
			s.list.ScrollToSelected()
		case key.Matches(msg, s.keyMap.Select):
			item, ok := s.list.SelectedItem().(*SubagentItem)
			if !ok || item == nil {
				break
			}
			if item.snapshot.ChildSessionID == "" {
				return ActionCmd{util.ReportWarn("Subagent has no child session to open yet")}
			}
			return ActionSelectSubagent{ChildSessionID: item.snapshot.ChildSessionID}
		default:
			prevValue := s.input.Value()
			var cmd tea.Cmd
			s.input, cmd = s.input.Update(msg)
			if s.input.Value() != prevValue {
				s.applyTaskVisibility()
				s.list.ScrollToTop()
				s.list.SetSelected(0)
			}
			return ActionCmd{cmd}
		}
	case common.CoalescedWheelMsg:
		if image.Pt(msg.Mouse.X, msg.Mouse.Y).In(s.bodyArea) {
			s.list.ScrollBy(int(msg.DeltaY))
			s.mouseScrolled = true
		}
	case tea.MouseClickMsg:
		return s.handleMouseClick(msg)
	}
	return nil
}

func (s *Subagents) handleMouseClick(msg tea.MouseClickMsg) Action {
	if msg.Button != tea.MouseLeft {
		s.resetMouseClick()
		return nil
	}
	area := s.bodyArea
	area.Max.X = min(area.Max.X, area.Min.X+s.list.Width())
	point := image.Pt(msg.X, msg.Y)
	if !point.In(area) {
		s.resetMouseClick()
		return nil
	}
	index, _ := s.list.ItemIndexAtPosition(point.X-area.Min.X, point.Y-area.Min.Y)
	if index < 0 {
		s.resetMouseClick()
		return nil
	}
	item, ok := s.list.ItemAt(index).(*SubagentItem)
	if !ok || item == nil {
		s.resetMouseClick()
		return nil
	}
	now := time.Now()
	if s.lastClickID == item.ID() && now.Sub(s.lastClickTime) <= doubleClickThreshold {
		s.resetMouseClick()
		if item.snapshot.ChildSessionID == "" {
			return ActionCmd{util.ReportWarn("Subagent has no child session to open yet")}
		}
		return ActionSelectSubagent{ChildSessionID: item.snapshot.ChildSessionID}
	}
	s.lastClickTime = now
	s.lastClickID = item.ID()
	s.list.SetSelected(index)
	return nil
}

func (s *Subagents) resetMouseClick() {
	s.lastClickTime = time.Time{}
	s.lastClickID = ""
}

// Cursor returns the cursor position relative to the dialog.
func (s *Subagents) Cursor() *tea.Cursor {
	return InputCursor(s.com.Styles, s.input.Cursor())
}

// Draw implements [Dialog].
func (s *Subagents) Draw(scr uv.Screen, area uv.Rectangle) *tea.Cursor {
	t := s.com.Styles
	s.bodyArea = image.Rectangle{}
	width := max(0, min(defaultDialogMaxWidth, area.Dx()-t.Dialog.View.GetHorizontalBorderSize()))
	height := max(0, min(defaultDialogHeight, area.Dy()-t.Dialog.View.GetVerticalBorderSize()))
	innerWidth := width - t.Dialog.View.GetHorizontalFrameSize()
	s.input.SetWidth(dialogInputTextWidth(t, s.input, innerWidth))
	listHeight, listTotalHeight, listWidth := sizeDialogList(t, s.list, innerWidth, height)

	cur := s.Cursor()
	rc := NewRenderContext(t, width)
	rc.Title = "Subagents"
	rc.AddPart(t.Dialog.InputPrompt.Render(s.input.View()))

	var bodyView string
	if message := s.statusMessage(); message != "" {
		text := ansi.Truncate(message,
			max(0, innerWidth-t.Dialog.ListItem.InfoBlurred.GetHorizontalFrameSize()), "…")
		rc.AddPart(t.Dialog.List.Height(listHeight).Render(
			t.Dialog.ListItem.InfoBlurred.Render(text)))
	} else {
		// Hide the model column uniformly when it would crowd the
		// task title.
		applyInfoColumnVisibility(s.list.FilteredItems(), listWidth, sessionInfoMaxPercent)

		// Keep the selected entry visible unless the mouse is scrolling.
		start, end := s.list.VisibleItemIndices()
		if !s.mouseScrolled && (s.list.Selected() < start || s.list.Selected() > end) {
			s.list.ScrollToSelected()
		}

		bodyView = t.Dialog.List.Height(s.list.Height()).Render(s.list.Render())
		bodyView = joinScrollbar(t, bodyView, listHeight, listTotalHeight, listHeight, s.list.Offset())
		rc.AddPart(bodyView)
	}
	rc.Help = renderDialogHelp(t, &s.help, s, innerWidth)

	view := rc.Render()
	if bodyView != "" {
		s.bodyArea = dialogListBodyArea(area, view, bodyView, rc.Help, rc.ViewStyle, t.Dialog.List, innerWidth, listHeight)
	}

	DrawCenterCursor(scr, area, view, cur)
	return cur
}

// ShortHelp implements [help.KeyMap].
func (s *Subagents) ShortHelp() []key.Binding {
	return []key.Binding{
		s.keyMap.UpDown,
		s.keyMap.Select,
		s.keyMap.Close,
	}
}

// FullHelp implements [help.KeyMap].
func (s *Subagents) FullHelp() [][]key.Binding {
	m := [][]key.Binding{}
	slice := []key.Binding{
		s.keyMap.Select,
		s.keyMap.Next,
		s.keyMap.Previous,
		s.keyMap.Close,
	}
	for i := 0; i < len(slice); i += 4 {
		end := min(i+4, len(slice))
		m = append(m, slice[i:end])
	}
	return m
}

// SubagentItem wraps one [proto.TaskSnapshot] as a dialog list item.
type SubagentItem struct {
	*list.Versioned
	snapshot proto.TaskSnapshot
	t        *styles.Styles
	m        fuzzy.Match
	cache    map[int]string
	focused  bool
	hideInfo bool
}

var _ ListItem = (*SubagentItem)(nil)

// Finished implements list.Item. Subagent items are render-stable
// outside of explicit SetFocused / SetMatch calls.
func (s *SubagentItem) Finished() bool {
	return true
}

// ID returns the task id.
func (s *SubagentItem) ID() string {
	return s.snapshot.ID
}

// title returns the task summary, falling back to the task id.
func (s *SubagentItem) title() string {
	if s.snapshot.Summary != "" {
		return s.snapshot.Summary
	}
	return "task " + s.snapshot.ID
}

// Filter returns the status, profile, title, and model for fuzzy matching.
func (s *SubagentItem) Filter() string {
	return s.snapshot.Status + " " + s.snapshot.Profile + " " + s.title() + " " + s.snapshot.Model
}

// InfoText returns the resolved model for the secondary column. The
// status lives in the title so it is never hidden by the info-column
// squeeze.
func (s *SubagentItem) InfoText() string {
	return s.snapshot.Model
}

// SetHideInfo controls whether the model info column is shown.
func (s *SubagentItem) SetHideInfo(v bool) {
	if s.hideInfo == v {
		return
	}
	s.cache = nil
	s.hideInfo = v
	if s.Versioned != nil {
		s.Bump()
	}
}

// SetFocused sets the focus state of the item.
func (s *SubagentItem) SetFocused(focused bool) {
	if s.focused == focused {
		return
	}
	s.cache = nil
	s.focused = focused
	if s.Versioned != nil {
		s.Bump()
	}
}

// SetMatch sets the fuzzy match for the item.
func (s *SubagentItem) SetMatch(m fuzzy.Match) {
	if sameFuzzyMatch(s.m, m) {
		return
	}
	s.cache = nil
	s.m = m
	if s.Versioned != nil {
		s.Bump()
	}
}

// Render returns the string representation of the task item.
func (s *SubagentItem) Render(width int) string {
	info := s.InfoText()
	if s.hideInfo {
		info = ""
	}
	sty := ListItemStyles{
		ItemBlurred:     s.t.Dialog.NormalItem,
		ItemFocused:     s.t.Dialog.SelectedItem,
		InfoTextBlurred: s.t.Dialog.ListItem.InfoBlurred,
		InfoTextFocused: s.t.Dialog.ListItem.InfoFocused,
	}
	title := fmt.Sprintf("[%s] %s · %s", displayTaskStatus(s.snapshot.Status), s.snapshot.Profile, s.title())
	return renderItem(sty, title, info, s.focused, width, s.cache, &s.m)
}

func displayTaskStatus(status string) string {
	switch status {
	case "pending":
		return "queued"
	case "waiting_for_input":
		return "waiting for input"
	case "completed":
		return "completed"
	case "failed":
		return "failed"
	case "cancelled":
		return "cancelled"
	case "interrupted":
		return "interrupted"
	default:
		return status
	}
}

// subagentItems converts task snapshots into list items.
func subagentItems(t *styles.Styles, tasks []proto.TaskSnapshot) []list.FilterableItem {
	items := make([]list.FilterableItem, len(tasks))
	for i, snapshot := range tasks {
		items[i] = &SubagentItem{Versioned: list.NewVersioned(), snapshot: snapshot, t: t}
	}
	return items
}

// maxFinishedSubagents bounds the finished section while the filter
// query is empty, so a long-lived session never opens as a wall of
// finished tasks. Live tasks are never capped, and a non-empty query
// searches every task (see applyTaskVisibility).
const maxFinishedSubagents = 5

// isLiveSubagentStatus reports whether a task status counts as live
// (listed first, never capped).
func isLiveSubagentStatus(status string) bool {
	switch status {
	case "pending", "running", "waiting_for_input":
		return true
	}
	return false
}

// orderSubagentSnapshots returns live tasks first (in arrival order),
// then finished tasks newest-first by completion time (falling back
// to creation time). The sort is stable, so tasks without parseable
// timestamps keep their arrival order.
func orderSubagentSnapshots(tasks []proto.TaskSnapshot) []proto.TaskSnapshot {
	ordered := make([]proto.TaskSnapshot, len(tasks))
	copy(ordered, tasks)
	var live []proto.TaskSnapshot
	var finished []proto.TaskSnapshot
	for _, snapshot := range ordered {
		if isLiveSubagentStatus(snapshot.Status) {
			live = append(live, snapshot)
		} else {
			finished = append(finished, snapshot)
		}
	}
	sort.SliceStable(finished, func(i, j int) bool {
		return subagentRecency(finished[i]).After(subagentRecency(finished[j]))
	})
	return append(live, finished...)
}

// subagentRecency returns the timestamp that orders one finished
// task against another: completion time when known, otherwise
// creation time. Unparseable timestamps sort oldest.
func subagentRecency(snapshot proto.TaskSnapshot) time.Time {
	if at, err := proto.ParseWireTime(snapshot.CompletedAt); err == nil && !at.IsZero() {
		return at
	}
	if at, err := proto.ParseWireTime(snapshot.CreatedAt); err == nil {
		return at
	}
	return time.Time{}
}

// subagentsMoreItem is the single summary row appended after the
// capped finished section, e.g. "… 12 more finished (type to
// filter)". It is not a task: activating it is a no-op (the dialog
// only acts on *SubagentItem selections), and it never appears while
// a filter query is active.
type subagentsMoreItem struct {
	*list.Versioned
	count   int
	t       *styles.Styles
	focused bool
}

var _ list.FilterableItem = (*subagentsMoreItem)(nil)

// Finished implements list.Item. The summary row is render-stable
// outside of explicit SetFocused calls.
func (m *subagentsMoreItem) Finished() bool {
	return true
}

// Filter implements list.FilterableItem. The row is only ever listed
// with an empty query, so it matches nothing.
func (m *subagentsMoreItem) Filter() string {
	return ""
}

// text returns the summary line.
func (m *subagentsMoreItem) text() string {
	return fmt.Sprintf("… %d more finished (type to filter)", m.count)
}

// SetFocused sets the focus state of the item.
func (m *subagentsMoreItem) SetFocused(focused bool) {
	if m.focused == focused {
		return
	}
	m.focused = focused
	if m.Versioned != nil {
		m.Bump()
	}
}

// Render returns the string representation of the summary row.
func (m *subagentsMoreItem) Render(width int) string {
	sty := ListItemStyles{
		ItemBlurred:     m.t.Dialog.NormalItem,
		ItemFocused:     m.t.Dialog.SelectedItem,
		InfoTextBlurred: m.t.Dialog.ListItem.InfoBlurred,
		InfoTextFocused: m.t.Dialog.ListItem.InfoFocused,
	}
	return renderItem(sty, m.text(), "", m.focused, width, nil, nil)
}
