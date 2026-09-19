package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/stretchr/testify/require"
)

// TestBuildOpenaiProviderUsesResponsesAPI ensures a provider
// declared as type "openai" routes every model, including ones
// fantasy does not recognise, to the Responses API.
func TestBuildOpenaiProviderUsesResponsesAPI(t *testing.T) {
	t.Parallel()

	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":     "resp_test",
			"object": "response",
			"model":  "muse-spark-1.3-contributor",
			"output": []any{
				map[string]any{
					"type":   "message",
					"id":     "msg_1",
					"role":   "assistant",
					"status": "completed",
					"content": []any{
						map[string]any{
							"type": "output_text",
							"text": "hi",
						},
					},
				},
			},
			"status": "completed",
			"usage": map[string]any{
				"input_tokens":  1,
				"output_tokens": 1,
				"total_tokens":  2,
			},
		})
	}))
	t.Cleanup(server.Close)

	env := testEnv(t)
	coord := newTestCoordinator(t, env, "test-openai", config.ProviderConfig{ID: "test-openai"})

	provider, err := coord.buildOpenaiProvider(server.URL, "test-key", nil)
	require.NoError(t, err)
	model, err := provider.LanguageModel(context.Background(), "muse-spark-1.3-contributor")
	require.NoError(t, err)

	_, genErr := model.Generate(context.Background(), fantasy.Call{
		Prompt: fantasy.Prompt{
			{
				Role:    fantasy.MessageRoleUser,
				Content: []fantasy.MessagePart{fantasy.TextPart{Text: "hi"}},
			},
		},
	})
	require.True(t, strings.HasSuffix(gotPath, "/responses"), "expected /responses, got %q", gotPath)
	require.NoError(t, genErr)
}
