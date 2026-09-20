package agent

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/require"
)

// buildPromptMemoryFixture returns a message list exercising every
// preparePrompt branch: plain turns, tool calls with late
// (non-adjacent) results, an orphaned tool result, an orphaned tool
// call, media tool results, image and text history attachments,
// reasoning content, an empty cancelled assistant message, and a
// summary message.
func buildPromptMemoryFixture() []message.Message {
	return []message.Message{
		{
			ID:   "msg-user-hello",
			Role: message.User,
			Parts: []message.ContentPart{
				message.TextContent{Text: "hello"},
			},
		},
		{
			ID:   "msg-assistant-calls",
			Role: message.Assistant,
			Parts: []message.ContentPart{
				message.TextContent{Text: "let me check"},
				message.ReasoningContent{Thinking: "need to run two tools"},
				message.ToolCall{
					ID:       "call_A",
					Name:     "bash",
					Input:    `{"command":"date"}`,
					Finished: true,
				},
				message.ToolCall{
					ID:       "call_B",
					Name:     "view",
					Input:    `{"path":"/foo"}`,
					Finished: true,
				},
			},
		},
		{
			ID:   "msg-user-interleaved",
			Role: message.User,
			Parts: []message.ContentPart{
				message.TextContent{Text: "are we done?"},
			},
		},
		{
			ID:   "msg-tool-late-a",
			Role: message.Tool,
			Parts: []message.ContentPart{
				message.ToolResult{
					ToolCallID: "call_A",
					Name:       "bash",
					Content:    "Fri May 2 21:00:00 UTC 2026",
				},
			},
		},
		{
			ID:   "msg-tool-media-b",
			Role: message.Tool,
			Parts: []message.ContentPart{
				message.ToolResult{
					ToolCallID: "call_B",
					Name:       "view",
					Data:       base64.StdEncoding.EncodeToString([]byte("fake-png-data")),
					MIMEType:   "image/png",
				},
			},
		},
		{
			ID:   "msg-tool-orphaned-result",
			Role: message.Tool,
			Parts: []message.ContentPart{
				message.ToolResult{
					ToolCallID: "call_ghost",
					Name:       "bash",
					Content:    "output with no matching call",
				},
			},
		},
		{
			ID:   "msg-assistant-orphaned-call",
			Role: message.Assistant,
			Parts: []message.ContentPart{
				message.ToolCall{
					ID:       "call_orphaned",
					Name:     "agent",
					Input:    `{"prompt":"search"}`,
					Finished: true,
				},
			},
		},
		{
			ID:   "msg-assistant-cancelled",
			Role: message.Assistant,
		},
		{
			ID:   "msg-user-attachments",
			Role: message.User,
			Parts: []message.ContentPart{
				message.TextContent{Text: "see attached"},
				message.BinaryContent{Path: "notes.txt", MIMEType: "text/plain", Data: []byte("important notes")},
				message.BinaryContent{Path: "image.png", MIMEType: "image/png", Data: []byte("fake-image-data")},
			},
		},
		{
			ID:               "msg-assistant-summary",
			Role:             message.Assistant,
			IsSummaryMessage: true,
			Parts: []message.ContentPart{
				message.TextContent{Text: "Summary of prior work: checked the date."},
			},
		},
		{
			ID:   "msg-user-continue",
			Role: message.User,
			Parts: []message.ContentPart{
				message.TextContent{Text: "continue"},
			},
		},
	}
}

// promptMemoryAttachments are new-turn attachments: one image (lands
// in the files list) and one text file (folded into the prompt).
func promptMemoryAttachments() []message.Attachment {
	return []message.Attachment{
		{FileName: "screenshot.png", MimeType: "image/png", Content: []byte("fake-screenshot")},
		{FileName: "notes.txt", MimeType: "text/plain", Content: []byte("attached notes")},
	}
}

// serializePrompt renders the model-visible request deterministically
// for golden comparison. Pointer addresses in %#v output (e.g. error
// values) are normalized since they vary between runs.
func serializePrompt(t *testing.T, agent *sessionAgent, supportsImages bool) string {
	t.Helper()
	history, files := agent.preparePrompt(buildPromptMemoryFixture(), supportsImages, promptMemoryAttachments()...)
	var sb strings.Builder
	fmt.Fprintf(&sb, "history:\n%#v\nfiles:\n%#v\n", history, files)
	return pointerAddrPattern.ReplaceAllString(sb.String(), "0xPTR")
}

var pointerAddrPattern = regexp.MustCompile(`0x[0-9a-f]+`)

func TestPreparePrompt_ByteIdentical(t *testing.T) {
	t.Parallel()

	agent := &sessionAgent{}
	for _, supportsImages := range []bool{false, true} {
		name := "noimages"
		if supportsImages {
			name = "images"
		}
		history, _ := agent.preparePrompt(buildPromptMemoryFixture(), supportsImages, promptMemoryAttachments()...)
		requireToolCallAdjacency(t, history)
		goldenPath := filepath.Join("testdata", "prepare_prompt_"+name+".golden")
		got := serializePrompt(t, agent, supportsImages)
		if os.Getenv("UPDATE_GOLDEN") != "" {
			require.NoError(t, os.MkdirAll("testdata", 0o755))
			require.NoError(t, os.WriteFile(goldenPath, []byte(got), 0o644))
		}
		want, err := os.ReadFile(goldenPath)
		require.NoError(t, err, "golden file missing; run with UPDATE_GOLDEN=1 to create it")
		require.Equal(t, string(want), got, "model-visible prompt changed for supportsImages=%v", supportsImages)
	}
}

// buildPromptMemoryBenchHistory scales the fixture to a large
// history for allocation benchmarking. With stripMedia the media
// tool result is replaced by plain text, modeling the common
// text-only history on the media-workaround path.
func buildPromptMemoryBenchHistory(n int, stripMedia bool) []message.Message {
	base := buildPromptMemoryFixture()
	if stripMedia {
		base = append([]message.Message(nil), base...)
		for i, m := range base {
			if m.ID != "msg-tool-media-b" {
				continue
			}
			base[i].Parts = []message.ContentPart{
				message.ToolResult{
					ToolCallID: "call_B",
					Name:       "view",
					Content:    "file contents as text",
				},
			}
		}
	}
	msgs := make([]message.Message, 0, n)
	for len(msgs) < n {
		msgs = append(msgs, base...)
	}
	return msgs[:n]
}

func BenchmarkPreparePrompt(b *testing.B) {
	agent := &sessionAgent{}
	msgs := buildPromptMemoryBenchHistory(1500, false)
	attachments := promptMemoryAttachments()
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_, _ = agent.preparePrompt(msgs, true, attachments...)
	}
}

func BenchmarkWorkaroundNoMedia(b *testing.B) {
	agent := &sessionAgent{}
	msgs := buildPromptMemoryBenchHistory(1500, true)
	history, _ := agent.preparePrompt(msgs, true)
	model := Model{
		ModelCfg:   config.SelectedModel{Provider: "openai"},
		CatwalkCfg: catwalk.Model{SupportsImages: true},
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_ = agent.workaroundProviderMediaLimitations(history, model)
	}
}

func TestWorkaroundProviderMediaLimitations_NoMediaNoCopy(t *testing.T) {
	t.Parallel()

	agent := &sessionAgent{}
	messages := []fantasy.Message{
		fantasy.NewUserMessage("hello"),
		{
			Role: fantasy.MessageRoleTool,
			Content: []fantasy.MessagePart{
				fantasy.ToolResultPart{
					ToolCallID: "call_1",
					Output: fantasy.ToolResultOutputContentText{
						Text: "plain text result",
					},
				},
			},
		},
	}
	model := Model{
		ModelCfg:   config.SelectedModel{Provider: "openai"},
		CatwalkCfg: catwalk.Model{SupportsImages: true},
	}

	result := agent.workaroundProviderMediaLimitations(messages, model)

	require.Len(t, result, len(messages))
	require.True(t, &result[0] == &messages[0],
		"media workaround must not copy the message list when there is no media to convert")
}

// BenchmarkFallbackUsageEstimate measures the per-step usage
// estimate on a large prepared history. The WithClone variant
// replicates the pre-change behavior (full clone before estimating)
// so the saving from sharing the prepared slice is measurable.
func BenchmarkFallbackUsageEstimate(b *testing.B) {
	agent := &sessionAgent{}
	msgs := buildPromptMemoryBenchHistory(1500, true)
	history, _ := agent.preparePrompt(msgs, true)
	step := fantasy.StepResult{}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_, _ = fallbackStepUsage(history, step)
	}
}

func BenchmarkFallbackUsageEstimateWithClone(b *testing.B) {
	agent := &sessionAgent{}
	msgs := buildPromptMemoryBenchHistory(1500, true)
	history, _ := agent.preparePrompt(msgs, true)
	step := fantasy.StepResult{}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		cloned := make([]fantasy.Message, len(history))
		for i, msg := range history {
			cloned[i] = msg
			cloned[i].Content = append([]fantasy.MessagePart(nil), msg.Content...)
		}
		_, _ = fallbackStepUsage(cloned, step)
	}
}
