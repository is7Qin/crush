package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent/task"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/stretchr/testify/require"
)

// toolCallStreamModel emits one unknown-tool call per stream and finishes
// with FinishReasonToolCalls, forcing the fantasy loop to request another
// assistant step. It counts stream invocations so tests can pin exactly
// how many steps a bounded run executed.
type toolCallStreamModel struct {
	calls atomic.Int32
}

func (m *toolCallStreamModel) Provider() string { return "fake" }
func (m *toolCallStreamModel) Model() string    { return "fake-model" }

func (m *toolCallStreamModel) Generate(_ context.Context, _ fantasy.Call) (*fantasy.Response, error) {
	return nil, errors.New("not implemented")
}

func (m *toolCallStreamModel) Stream(_ context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
	m.calls.Add(1)
	return func(yield func(fantasy.StreamPart) bool) {
		if !yield(fantasy.StreamPart{
			Type:          fantasy.StreamPartTypeToolCall,
			ID:            "tc-1",
			ToolCallName:  "view",
			ToolCallInput: `{"filePath":"missing.txt"}`,
		}) {
			return
		}
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls})
	}, nil
}

func (m *toolCallStreamModel) GenerateObject(_ context.Context, _ fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errors.New("not implemented")
}

func (m *toolCallStreamModel) StreamObject(_ context.Context, _ fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errors.New("not implemented")
}

// blockingModel signals entry, then blocks until its stream context is
// done and reports that context error as the stream failure. It is the
// honest stand-in for a provider request that observes cancellation and
// deadlines (real HTTP clients do).
type blockingModel struct {
	entered chan struct{}
	once    sync.Once
}

func newBlockingModel() *blockingModel {
	return &blockingModel{entered: make(chan struct{})}
}

func (m *blockingModel) Provider() string { return "fake" }
func (m *blockingModel) Model() string    { return "fake-model" }

func (m *blockingModel) Generate(_ context.Context, _ fantasy.Call) (*fantasy.Response, error) {
	return nil, errors.New("not implemented")
}

func (m *blockingModel) Stream(ctx context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
	return func(yield func(fantasy.StreamPart) bool) {
		m.once.Do(func() { close(m.entered) })
		<-ctx.Done()
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: ctx.Err()})
	}, nil
}

func (m *blockingModel) GenerateObject(_ context.Context, _ fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errors.New("not implemented")
}

func (m *blockingModel) StreamObject(_ context.Context, _ fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errors.New("not implemented")
}

// stepsChildAgent mirrors fakeChildAgent but carries a profile step limit
// through the real SessionAgentOptions seam. The small model is a plain
// text streamer: Run fires GenerateTitle on a fresh session and the title
// agent uses the small model, so a tool-calling small model would spin
// the title loop.
func stepsChildAgent(env fakeEnv, model fantasy.LanguageModel, maxSteps int) SessionAgent {
	large := Model{
		Model:      model,
		CatwalkCfg: catwalk.Model{ID: "mock-model", ContextWindow: 8192, DefaultMaxTokens: 128},
		ModelCfg:   config.SelectedModel{Provider: "mock", Model: "mock-model"},
	}
	small := Model{
		Model:      &finishStreamModel{text: "title"},
		CatwalkCfg: catwalk.Model{ID: "mock-model", ContextWindow: 8192, DefaultMaxTokens: 128},
		ModelCfg:   config.SelectedModel{Provider: "mock", Model: "mock-model"},
	}
	return NewSessionAgent(SessionAgentOptions{
		LargeModel:           large,
		SmallModel:           small,
		SystemPrompt:         "fake system prompt",
		IsSubAgent:           true,
		DisableAutoSummarize: true,
		IsYolo:               true,
		Sessions:             env.sessions,
		Messages:             env.messages,
		MaxSteps:             maxSteps,
	})
}

// policyToolEnv wires a coordinator to a real task manager whose child
// factory resolves the real profile from profileTestConfig and builds a
// fake-model child carrying that profile's max_steps. It mirrors
// production buildProfileAgent's policy plumbing minus the provider.
func policyToolEnv(
	t *testing.T,
	model func(prof config.ResolvedProfile) fantasy.LanguageModel,
) (*coordinator, *task.Manager, fakeEnv, chan task.Event) {
	t.Helper()
	env := testEnv(t)
	require.NoError(t, os.WriteFile(filepath.Join(env.workingDir, "crush.json"), []byte(profileTestConfig), 0o644))
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)
	cfg.SetupAgents()

	coord := &coordinator{
		cfg:         cfg,
		sessions:    env.sessions,
		messages:    env.messages,
		permissions: env.permissions,
		history:     env.history,
		filetracker: *env.filetracker,
	}
	mgr := task.New(t.Context(), task.Config{
		WorkspaceID: env.workingDir,
		Store:       task.NewSQLiteStore(env.conn),
	})
	events := make(chan task.Event, 64)
	mgr.Subscribe(func(ev task.Event) { events <- ev })
	t.Cleanup(func() {
		_ = mgr.Shutdown(context.WithoutCancel(t.Context()))
	})

	coord.tasks = mgr
	coord.newChildAgent = func(_ context.Context, name, _ string) (SessionAgent, config.ResolvedProfile, error) {
		prof, err := cfg.Config().ResolveAgentProfile(name)
		if err != nil {
			return nil, config.ResolvedProfile{}, err
		}
		return stepsChildAgent(env, model(prof), prof.MaxSteps), prof, nil
	}
	return coord, mgr, env, events
}
