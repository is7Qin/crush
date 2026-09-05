package dialog

import (
	"image"
	"strings"
	"time"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/crush/internal/ui/common"
	"github.com/charmbracelet/crush/internal/ui/list"
	"github.com/charmbracelet/crush/internal/ui/styles"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/sahilm/fuzzy"
)

// AgentsID is the identifier for the primary agent profile picker.
const AgentsID = "agents"

// AgentProfileOption is one selectable primary agent profile: the
// canonical name plus its human-readable description for the info
// column. The main model builds these from the config snapshot; the
// dialog never reads config or performs I/O itself.
type AgentProfileOption struct {
	Name        string
	Description string
}

// Agents lists the discoverable primary agent profiles and lets the
// user select one to switch the workspace's runtime primary agent to.
// The profile currently in use is marked "(current)". Profile data and
// the active profile are pushed in from the main model (SetProfiles /
// SetCurrent); the dialog itself never performs I/O.
type Agents struct {
	com   *common.Common
	help  help.Model
	list  *list.FilterableList
	input textinput.Model

	current string

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

var _ Dialog = (*Agents)(nil)

// NewAgents creates an Agents dialog with an empty profile list.
func NewAgents(com *common.Common) *Agents {
	a := &Agents{com: com}

	h := help.New()
	h.Styles = com.Styles.DialogHelpStyles()
	a.help = h

	a.list = list.NewFilterableList()
	a.list.Focus()

	a.input = textinput.New()
	a.input.SetVirtualCursor(false)
	a.input.Placeholder = "Type to filter"
	a.input.SetStyles(com.Styles.TextInput)
	a.input.Focus()

	a.keyMap.Select = key.NewBinding(
		key.WithKeys("enter", "tab", "ctrl+y"),
		key.WithHelp("enter", "switch agent"),
	)
	a.keyMap.Next = key.NewBinding(
		key.WithKeys("down", "ctrl+n"),
		key.WithHelp("↓", "next item"),
	)
	a.keyMap.Previous = key.NewBinding(
		key.WithKeys("up", "ctrl+p"),
		key.WithHelp("↑", "previous item"),
	)
	a.keyMap.UpDown = key.NewBinding(
		key.WithKeys("up", "down"),
		key.WithHelp("↑/↓", "choose"),
	)
	a.keyMap.Close = CloseKey

	return a
}

// ID implements Dialog.
func (a *Agents) ID() string {
	return AgentsID
}

// SetProfiles replaces the listed profiles, keeping the selection on
// the same profile name when it is still present after the refresh.
func (a *Agents) SetProfiles(profiles []AgentProfileOption) {
	selectedID := ""
	if item, ok := a.list.SelectedItem().(*AgentProfileItem); ok && item != nil {
		selectedID = item.ID()
	}

	a.list.SetItems(agentProfileItems(a.com.Styles, profiles, a.current)...)
	if query := a.input.Value(); query != "" {
		a.list.SetFilter(query)
	}
	if index := a.indexOfProfile(selectedID); index >= 0 {
		a.list.SetSelected(index)
	} else {
		a.list.SetSelected(0)
	}
	a.list.ScrollToSelected()
}

// SetCurrent marks the profile with the given name as the active
// primary agent. An unknown or empty name clears the marker.
func (a *Agents) SetCurrent(name string) {
	if a.current == name {
		return
	}
	a.current = name
	for _, item := range a.list.FilteredItems() {
		if profileItem, ok := item.(*AgentProfileItem); ok && profileItem != nil {
			profileItem.setCurrent(profileItem.ID() == name)
		}
	}
}

// indexOfProfile returns the filtered-list position of the profile with
// the given name, or -1 when it is not listed.
func (a *Agents) indexOfProfile(name string) int {
	if name == "" {
		return -1
	}
	for i, item := range a.list.FilteredItems() {
		if profileItem, ok := item.(*AgentProfileItem); ok && profileItem != nil && profileItem.ID() == name {
			return i
		}
	}
	return -1
}

// statusMessage returns the placeholder body line for the empty state,
// or "" when the profile list should render. The result is always a
// single line, safe for the dialog frame.
func (a *Agents) statusMessage() string {
	if len(a.list.FilteredItems()) == 0 && a.input.Value() == "" {
		return "No agent profiles."
	}
	return ""
}

// HandleMsg implements Dialog.
func (a *Agents) HandleMsg(msg tea.Msg) Action {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch {
		case key.Matches(msg, a.keyMap.Close):
			return ActionClose{}
		case key.Matches(msg, a.keyMap.Previous):
			a.list.Focus()
			if a.list.IsSelectedFirst() {
				a.list.SelectLast()
			} else {
				a.list.SelectPrev()
			}
			a.list.ScrollToSelected()
		case key.Matches(msg, a.keyMap.Next):
			a.list.Focus()
			if a.list.IsSelectedLast() {
				a.list.SelectFirst()
			} else {
				a.list.SelectNext()
			}
			a.list.ScrollToSelected()
		case key.Matches(msg, a.keyMap.Select):
			item, ok := a.list.SelectedItem().(*AgentProfileItem)
			if !ok || item == nil {
				break
			}
			return ActionSelectAgentProfile{Profile: item.ID()}
		default:
			prevValue := a.input.Value()
			var cmd tea.Cmd
			a.input, cmd = a.input.Update(msg)
			if a.input.Value() != prevValue {
				a.list.SetFilter(a.input.Value())
				a.list.ScrollToTop()
				a.list.SetSelected(0)
			}
			return ActionCmd{cmd}
		}
	case common.CoalescedWheelMsg:
		if image.Pt(msg.Mouse.X, msg.Mouse.Y).In(a.bodyArea) {
			a.list.ScrollBy(int(msg.DeltaY))
			a.mouseScrolled = true
		}
	case tea.MouseClickMsg:
		return a.handleMouseClick(msg)
	}
	return nil
}

func (a *Agents) handleMouseClick(msg tea.MouseClickMsg) Action {
	if msg.Button != tea.MouseLeft {
		a.resetMouseClick()
		return nil
	}
	area := a.bodyArea
	area.Max.X = min(area.Max.X, area.Min.X+a.list.Width())
	point := image.Pt(msg.X, msg.Y)
	if !point.In(area) {
		a.resetMouseClick()
		return nil
	}
	index, _ := a.list.ItemIndexAtPosition(point.X-area.Min.X, point.Y-area.Min.Y)
	if index < 0 {
		a.resetMouseClick()
		return nil
	}
	item, ok := a.list.ItemAt(index).(*AgentProfileItem)
	if !ok || item == nil {
		a.resetMouseClick()
		return nil
	}
	now := time.Now()
	if a.lastClickID == item.ID() && now.Sub(a.lastClickTime) <= doubleClickThreshold {
		a.resetMouseClick()
		return ActionSelectAgentProfile{Profile: item.ID()}
	}
	a.lastClickTime = now
	a.lastClickID = item.ID()
	a.list.SetSelected(index)
	return nil
}

func (a *Agents) resetMouseClick() {
	a.lastClickTime = time.Time{}
	a.lastClickID = ""
}

// Cursor returns the cursor position relative to the dialog.
func (a *Agents) Cursor() *tea.Cursor {
	return InputCursor(a.com.Styles, a.input.Cursor())
}

// Draw implements [Dialog].
func (a *Agents) Draw(scr uv.Screen, area uv.Rectangle) *tea.Cursor {
	t := a.com.Styles
	a.bodyArea = image.Rectangle{}
	width := max(0, min(defaultDialogMaxWidth, area.Dx()-t.Dialog.View.GetHorizontalBorderSize()))
	height := max(0, min(defaultDialogHeight, area.Dy()-t.Dialog.View.GetVerticalBorderSize()))
	innerWidth := width - t.Dialog.View.GetHorizontalFrameSize()
	a.input.SetWidth(dialogInputTextWidth(t, a.input, innerWidth))
	listHeight, listTotalHeight, listWidth := sizeDialogList(t, a.list, innerWidth, height)

	cur := a.Cursor()
	rc := NewRenderContext(t, width)
	rc.Title = "Switch Agent"
	rc.AddPart(t.Dialog.InputPrompt.Render(a.input.View()))

	var bodyView string
	if message := a.statusMessage(); message != "" {
		text := ansi.Truncate(message,
			max(0, innerWidth-t.Dialog.ListItem.InfoBlurred.GetHorizontalFrameSize()), "…")
		rc.AddPart(t.Dialog.List.Height(listHeight).Render(
			t.Dialog.ListItem.InfoBlurred.Render(text)))
	} else {
		// Hide the description column when it would crowd the name.
		applyInfoColumnVisibility(a.list.FilteredItems(), listWidth, sessionInfoMaxPercent)

		// Keep the selected entry visible unless the mouse is scrolling.
		start, end := a.list.VisibleItemIndices()
		if !a.mouseScrolled && (a.list.Selected() < start || a.list.Selected() > end) {
			a.list.ScrollToSelected()
		}

		bodyView = t.Dialog.List.Height(a.list.Height()).Render(a.list.Render())
		bodyView = joinScrollbar(t, bodyView, listHeight, listTotalHeight, listHeight, a.list.Offset())
		rc.AddPart(bodyView)
	}
	rc.Help = renderDialogHelp(t, &a.help, a, innerWidth)

	view := rc.Render()
	if bodyView != "" {
		a.bodyArea = dialogListBodyArea(area, view, bodyView, rc.Help, rc.ViewStyle, t.Dialog.List, innerWidth, listHeight)
	}

	DrawCenterCursor(scr, area, view, cur)
	return cur
}

// ShortHelp implements [help.KeyMap].
func (a *Agents) ShortHelp() []key.Binding {
	return []key.Binding{
		a.keyMap.UpDown,
		a.keyMap.Select,
		a.keyMap.Close,
	}
}

// FullHelp implements [help.KeyMap].
func (a *Agents) FullHelp() [][]key.Binding {
	m := [][]key.Binding{}
	slice := []key.Binding{
		a.keyMap.Select,
		a.keyMap.Next,
		a.keyMap.Previous,
		a.keyMap.Close,
	}
	for i := 0; i < len(slice); i += 4 {
		end := min(i+4, len(slice))
		m = append(m, slice[i:end])
	}
	return m
}

// AgentProfileItem wraps one [AgentProfileOption] as a dialog list
// item.
type AgentProfileItem struct {
	*list.Versioned
	name        string
	description string
	t           *styles.Styles
	m           fuzzy.Match
	cache       map[int]string
	focused     bool
	current     bool
	hideInfo    bool
}

var _ ListItem = (*AgentProfileItem)(nil)

// Finished implements list.Item. Profile items are render-stable
// outside of explicit SetFocused / SetMatch / setCurrent calls.
func (p *AgentProfileItem) Finished() bool {
	return true
}

// ID returns the canonical profile name.
func (p *AgentProfileItem) ID() string {
	return p.name
}

// Filter returns the name and description for fuzzy matching.
func (p *AgentProfileItem) Filter() string {
	return p.name + " " + p.description
}

// InfoText returns the profile description for the secondary column.
// The current marker lives in the title so it is never hidden by the
// info-column squeeze.
func (p *AgentProfileItem) InfoText() string {
	return p.description
}

// SetHideInfo controls whether the description info column is shown.
func (p *AgentProfileItem) SetHideInfo(v bool) {
	if p.hideInfo == v {
		return
	}
	p.cache = nil
	p.hideInfo = v
	if p.Versioned != nil {
		p.Bump()
	}
}

// SetFocused sets the focus state of the item.
func (p *AgentProfileItem) SetFocused(focused bool) {
	if p.focused == focused {
		return
	}
	p.cache = nil
	p.focused = focused
	if p.Versioned != nil {
		p.Bump()
	}
}

// setCurrent marks the item as the active primary agent profile.
func (p *AgentProfileItem) setCurrent(current bool) {
	if p.current == current {
		return
	}
	p.cache = nil
	p.current = current
	if p.Versioned != nil {
		p.Bump()
	}
}

// SetMatch sets the fuzzy match for the item.
func (p *AgentProfileItem) SetMatch(m fuzzy.Match) {
	if sameFuzzyMatch(p.m, m) {
		return
	}
	p.cache = nil
	p.m = m
	if p.Versioned != nil {
		p.Bump()
	}
}

// Render returns the string representation of the profile item.
func (p *AgentProfileItem) Render(width int) string {
	info := p.InfoText()
	if p.hideInfo {
		info = ""
	}
	sty := ListItemStyles{
		ItemBlurred:     p.t.Dialog.NormalItem,
		ItemFocused:     p.t.Dialog.SelectedItem,
		InfoTextBlurred: p.t.Dialog.ListItem.InfoBlurred,
		InfoTextFocused: p.t.Dialog.ListItem.InfoFocused,
	}
	title := p.name
	if p.current {
		title += " (current)"
	}
	return renderItem(sty, title, info, p.focused, width, p.cache, &p.m)
}

// agentProfileItems converts profile options into list items, marking
// the one matching current as active.
func agentProfileItems(t *styles.Styles, profiles []AgentProfileOption, current string) []list.FilterableItem {
	items := make([]list.FilterableItem, len(profiles))
	for i, profile := range profiles {
		items[i] = &AgentProfileItem{
			Versioned:   list.NewVersioned(),
			name:        profile.Name,
			description: strings.TrimSpace(profile.Description),
			t:           t,
			current:     profile.Name == current && current != "",
		}
	}
	return items
}
