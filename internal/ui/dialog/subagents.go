package dialog

import (
	"fmt"
	"image"
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
// same task id when it is still present after the refresh.
func (s *Subagents) SetTasks(tasks []proto.TaskSnapshot) {
	selectedID := ""
	if item, ok := s.list.SelectedItem().(*SubagentItem); ok && item != nil {
		selectedID = item.ID()
	}

	s.loading = false
	s.fetchErr = ""
	s.list.SetItems(subagentItems(s.com.Styles, tasks)...)
	if query := s.input.Value(); query != "" {
		s.list.SetFilter(query)
	}
	if index := s.indexOfTask(selectedID); index >= 0 {
		s.list.SetSelected(index)
	} else {
		s.list.SetSelected(0)
	}
	s.list.ScrollToSelected()
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
				s.list.SetFilter(s.input.Value())
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

// Filter returns the profile, title, and model for fuzzy matching.
func (s *SubagentItem) Filter() string {
	return s.snapshot.Profile + " " + s.title() + " " + s.snapshot.Model
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
	title := fmt.Sprintf("[%s] %s · %s", s.snapshot.Status, s.snapshot.Profile, s.title())
	return renderItem(sty, title, info, s.focused, width, s.cache, &s.m)
}

// subagentItems converts task snapshots into list items.
func subagentItems(t *styles.Styles, tasks []proto.TaskSnapshot) []list.FilterableItem {
	items := make([]list.FilterableItem, len(tasks))
	for i, snapshot := range tasks {
		items[i] = &SubagentItem{Versioned: list.NewVersioned(), snapshot: snapshot, t: t}
	}
	return items
}
