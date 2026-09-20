package model

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/charmbracelet/crush/internal/ui/chat"
	"github.com/charmbracelet/crush/internal/workspace"
	"github.com/stretchr/testify/require"
)

// bodyWindowWorkspace is a minimal workspace stub serving messages
// from memory so released bodies can be reloaded without SQLite.
type bodyWindowWorkspace struct {
	workspace.Workspace
	cfg  *config.Config
	msgs map[string]message.Message
}

func (w *bodyWindowWorkspace) Config() *config.Config { return w.cfg }

func (w *bodyWindowWorkspace) WorkingDir() string { return "/tmp/crush-test" }

func (w *bodyWindowWorkspace) GetMessage(_ context.Context, _, id string) (message.Message, error) {
	msg, ok := w.msgs[id]
	if !ok {
		return message.Message{}, errors.New("message not found")
	}
	return msg, nil
}

// bodyWindowTestSetup builds a UI over count synthetic messages with
// distinct sizable bodies and loads them as a session.
func bodyWindowTestSetup(t *testing.T, count int) (*UI, []message.Message) {
	t.Helper()

	fake := &bodyWindowWorkspace{
		cfg:  &config.Config{},
		msgs: make(map[string]message.Message),
	}
	u := newTestUI()
	u.com.Workspace = fake
	u.session = &session.Session{ID: "body-window"}
	u.chat.SetSize(80, 24)

	msgs := make([]message.Message, 0, count)
	pad := strings.Repeat("x", 2048)
	for i := range count {
		id := fmt.Sprintf("msg-%04d", i)
		var msg message.Message
		if i%2 == 0 {
			msg = message.Message{
				ID:        id,
				SessionID: "body-window",
				Role:      message.User,
				Parts: []message.ContentPart{
					message.TextContent{Text: fmt.Sprintf("user body %d %s", i, pad)},
					message.Finish{Reason: message.FinishReasonMaxTokens, Time: 1},
				},
				CreatedAt: int64(i),
			}
		} else {
			msg = message.Message{
				ID:        id,
				SessionID: "body-window",
				Role:      message.Assistant,
				Parts: []message.ContentPart{
					message.TextContent{Text: fmt.Sprintf("assistant body %d %s", i, pad)},
					message.Finish{Reason: message.FinishReasonMaxTokens, Time: 1},
				},
				CreatedAt: int64(i),
			}
		}
		fake.msgs[id] = msg
		msgs = append(msgs, msg)
	}

	require.Nil(t, u.setSessionMessages(msgs))
	return u, msgs
}

// firstReleasable returns the first releasable item in list order.
func firstReleasable(u *UI) chat.Releasable {
	for i := range u.chat.Len() {
		if releasable, ok := u.chat.list.ItemAt(i).(chat.Releasable); ok {
			return releasable
		}
	}
	return nil
}

// TestChatBodyWindow_BoundsRetention asserts that loading a long
// history keeps decoded bodies only for the viewport window: the
// first message must be released while the loaded count stays far
// below the total.
func TestChatBodyWindow_BoundsRetention(t *testing.T) {
	t.Parallel()

	const count = 300
	u, _ := bodyWindowTestSetup(t, count)

	loaded := u.chat.LoadedBodyCount()
	require.Less(t, loaded, count/2,
		"loaded bodies %d must stay far below total %d", loaded, count)

	first := firstReleasable(u)
	require.NotNil(t, first, "expected releasable items")
	require.False(t, first.BodyLoaded(), "oldest item must be released after load")
}

// TestChatBodyWindow_ScrollBackRendersCorrectContent asserts that
// scrolling to released history reloads the right content and the
// bound still holds afterward.
func TestChatBodyWindow_ScrollBackRendersCorrectContent(t *testing.T) {
	t.Parallel()

	const count = 300
	u, _ := bodyWindowTestSetup(t, count)

	u.chat.ScrollToTop()

	loaded := u.chat.LoadedBodyCount()
	require.Less(t, loaded, count/2,
		"loaded bodies %d must stay bounded after scroll", loaded, count)

	top := u.chat.list.ItemAt(0)
	require.NotNil(t, top)
	rendered := top.(chat.MessageItem).RawRender(u.chat.list.Width())
	require.Contains(t, rendered, "user body 0",
		"scroll-back must render the reloaded content")

	u.chat.ScrollToBottom()
	bottom := u.chat.list.ItemAt(u.chat.Len() - 1)
	require.NotNil(t, bottom)
	rendered = bottom.(chat.MessageItem).RawRender(u.chat.list.Width())
	require.NotContains(t, rendered, "user body 0",
		"bottom item must not render the top message content")
}

// TestChatBodyWindow_ToolRoundTrip asserts that a released tool item
// reloads both its call input and its result content on demand.
func TestChatBodyWindow_ToolRoundTrip(t *testing.T) {
	t.Parallel()

	fake := &bodyWindowWorkspace{
		cfg:  &config.Config{},
		msgs: make(map[string]message.Message),
	}
	u := newTestUI()
	u.com.Workspace = fake
	u.session = &session.Session{ID: "body-window-tools"}
	u.chat.SetSize(80, 24)

	msgs := make([]message.Message, 0, 120)
	pad := strings.Repeat("y", 1024)
	for i := range 100 {
		id := fmt.Sprintf("fill-%04d", i)
		msg := message.Message{
			ID:        id,
			SessionID: "body-window-tools",
			Role:      message.User,
			Parts: []message.ContentPart{
				message.TextContent{Text: fmt.Sprintf("fill %d %s", i, pad)},
				message.Finish{Reason: message.FinishReasonMaxTokens, Time: 1},
			},
			CreatedAt: int64(i),
		}
		fake.msgs[id] = msg
		msgs = append(msgs, msg)
	}
	assistantMsg := message.Message{
		ID:        "assistant-tools",
		SessionID: "body-window-tools",
		Role:      message.Assistant,
		Parts: []message.ContentPart{
			message.TextContent{Text: "running tool"},
			message.ToolCall{ID: "tc-1", Name: "bash", Input: `{"command":"echo hi"}`, Finished: true},
			message.Finish{Reason: message.FinishReasonMaxTokens, Time: 1},
		},
		CreatedAt: 100,
	}
	toolMsg := message.Message{
		ID:        "tool-msg-1",
		SessionID: "body-window-tools",
		Role:      message.Tool,
		Parts: []message.ContentPart{
			message.ToolResult{ToolCallID: "tc-1", Name: "bash", Content: "tool output hi"},
			message.Finish{Reason: message.FinishReasonMaxTokens, Time: 1},
		},
		CreatedAt: 101,
	}
	fake.msgs[assistantMsg.ID] = assistantMsg
	fake.msgs[toolMsg.ID] = toolMsg
	msgs = append(msgs, assistantMsg, toolMsg)
	for i := range 60 {
		id := fmt.Sprintf("tail-%04d", i)
		msg := message.Message{
			ID:        id,
			SessionID: "body-window-tools",
			Role:      message.User,
			Parts: []message.ContentPart{
				message.TextContent{Text: fmt.Sprintf("tail %d %s", i, pad)},
				message.Finish{Reason: message.FinishReasonMaxTokens, Time: 1},
			},
			CreatedAt: int64(200 + i),
		}
		fake.msgs[id] = msg
		msgs = append(msgs, msg)
	}

	require.Nil(t, u.setSessionMessages(msgs))

	// Look up by index without touching MessageItem: that accessor
	// ensures the body, which would defeat the assertion.
	idx, ok := u.chat.idInxMap["tc-1"]
	require.True(t, ok, "tool item must be indexed")
	toolItem, ok := u.chat.list.ItemAt(idx).(chat.MessageItem)
	require.True(t, ok, "tool slot must hold a message item")
	releasable, ok := toolItem.(chat.Releasable)
	require.True(t, ok, "tool item must be releasable")
	require.False(t, releasable.BodyLoaded(), "far tool item must be released")

	rendered := toolItem.RawRender(u.chat.list.Width())
	require.Contains(t, rendered, "hi",
		"reloaded tool item must render its result content")
}
