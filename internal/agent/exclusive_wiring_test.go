package agent

import (
	"context"
	"reflect"
	"sync"
	"testing"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/stretchr/testify/require"
)

// spyLease records lease traffic for wiring assertions.
type spyLease struct {
	mu     sync.Mutex
	events []string
}

func (s *spyLease) record(e string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, e)
}

func (s *spyLease) AcquireShared(_ context.Context, key string) (tools.Release, error) {
	s.record("shared:" + key)
	return func() { s.record("shared-release") }, nil
}

func (s *spyLease) AcquireExclusive(_ context.Context, key string) (tools.Release, error) {
	s.record("exclusive:" + key)
	return func() { s.record("exclusive-release") }, nil
}

func (s *spyLease) calls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.events...)
}

func TestExclusiveWrapOrder(t *testing.T) {
	t.Parallel()
	inputs := []fantasy.AgentTool{&fakeTool{name: "bash"}, &fakeTool{name: "view"}}
	wrapped := tools.WrapToolsExclusive(inputs, "ws", &spyLease{})
	for i, tool := range wrapped {
		require.Equal(t, reflect.TypeOf(tool).String(), "*tools.exclusiveTool",
			"tool %d should be an exclusive wrapper", i)
		require.Equal(t, inputs[i].Info().Name, tool.Info().Name, "names must survive")
	}

	runner := newRunner(t, `exit 0`)
	primary := wrapToolsWithHooks(wrapped, runner, false)
	for _, tool := range primary {
		h, ok := tool.(*hookedTool)
		require.True(t, ok, "primary tools are hookedTool(exclusiveTool(inner))")
		require.Equal(t, "*tools.exclusiveTool", reflect.TypeOf(h.inner).String())
	}

	// Children get exclusiveTool(inner) with no hook interception.
	child := wrapToolsWithHooks(wrapped, runner, true)
	for _, tool := range child {
		require.Equal(t, "*tools.exclusiveTool", reflect.TypeOf(tool).String(),
			"child tools are exclusiveTool(inner)")
	}
}

// TestHookDenialDoesNotAcquireLease proves the completion-boundary
// order hook decision -> lease -> inner tool: a denied call never
// reaches the workspace lease.
func TestHookDenialDoesNotAcquireLease(t *testing.T) {
	lease := &spyLease{}
	inner := &fakeTool{name: "bash", resp: fantasy.NewTextResponse("ok")}
	wrapped := tools.WrapToolsExclusive([]fantasy.AgentTool{inner}, "ws", lease)
	tool := wrapToolsWithHooks(wrapped, newRunner(t, `echo "no" >&2; exit 2`), false)[0]

	resp, err := tool.Run(t.Context(), fantasy.ToolCall{ID: "c1", Name: "bash", Input: "{}"})
	require.NoError(t, err)
	require.True(t, resp.IsError, "the hook denial surfaces as a tool error")
	require.False(t, inner.called)
	require.Empty(t, lease.calls(), "hook denial must not acquire the lease")
}

func TestHookAllowAcquiresLeaseOnceAndReleases(t *testing.T) {
	lease := &spyLease{}
	inner := &fakeTool{name: "bash", resp: fantasy.NewTextResponse("ok")}
	wrapped := tools.WrapToolsExclusive([]fantasy.AgentTool{inner}, "ws", lease)
	tool := wrapToolsWithHooks(wrapped, newRunner(t, `echo '{"decision":"allow"}'`), false)[0]

	_, err := tool.Run(t.Context(), fantasy.ToolCall{ID: "c2", Name: "bash", Input: "{}"})
	require.NoError(t, err)
	require.True(t, inner.called)
	require.Equal(t, []string{"exclusive:ws", "exclusive-release"}, lease.calls())

	// Read-only tools never touch the lease even on the allowed path.
	readLease := &spyLease{}
	viewer := &fakeTool{name: "view", resp: fantasy.NewTextResponse("ok")}
	read := wrapToolsWithHooks(
		tools.WrapToolsExclusive([]fantasy.AgentTool{viewer}, "ws", readLease),
		newRunner(t, `exit 0`), false)[0]
	_, err = read.Run(t.Context(), fantasy.ToolCall{ID: "c3", Name: "view", Input: "{}"})
	require.NoError(t, err)
	require.Empty(t, readLease.calls())
}
