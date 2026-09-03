package tools

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"
)

// recordingTool blocks inside Run between its entry and release
// signals so tests can observe lease and admission ordering.
type recordingTool struct {
	name    string
	entered chan struct{}
	release chan struct{}
	calls   atomic.Int32
}

func newRecordingTool(name string) *recordingTool {
	return &recordingTool{
		name:    name,
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (r *recordingTool) Info() fantasy.ToolInfo { return fantasy.ToolInfo{Name: r.name} }

func (r *recordingTool) Run(_ context.Context, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
	if r.calls.Add(1) == 1 {
		close(r.entered)
	}
	<-r.release
	return fantasy.NewTextResponse("ok"), nil
}

func (r *recordingTool) ProviderOptions() fantasy.ProviderOptions     { return nil }
func (r *recordingTool) SetProviderOptions(_ fantasy.ProviderOptions) {}

func callAs(name string) fantasy.ToolCall {
	return fantasy.ToolCall{ID: "call-1", Name: name, Input: "{}"}
}

// orderedEvents is the shared event log both spies append to under one
// mutex, so acquisition ordering is observable without a data race.
type orderedEvents struct {
	mu     sync.Mutex
	events []string
}

func (o *orderedEvents) record(e string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.events = append(o.events, e)
}

func (o *orderedEvents) snapshot() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.events...)
}

// spyLease grants immediately and records the acquisition order.
type spyLease struct {
	events *orderedEvents
	err    error
}

func (s *spyLease) AcquireShared(ctx context.Context, key string) (Release, error) {
	return s.acquire(ctx, key, "shared")
}

func (s *spyLease) AcquireExclusive(ctx context.Context, key string) (Release, error) {
	return s.acquire(ctx, key, "exclusive")
}

func (s *spyLease) acquire(ctx context.Context, key, kind string) (Release, error) {
	select {
	case <-ctx.Done():
		return nil, leaseErr(ctx)
	default:
	}
	if s.err != nil {
		return nil, s.err
	}
	if s.events != nil {
		s.events.record("acquire:" + kind)
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			if s.events != nil {
				s.events.record("release:" + kind)
			}
		})
	}, nil
}

// spyFence records shared admissions and can be closed to reject them.
type spyFence struct {
	mu       sync.Mutex
	closed   bool
	admitted atomic.Int32
	events   *orderedEvents
}

func (f *spyFence) AdmitShared(context.Context) (func(), error) {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return nil, errors.New("task run fenced")
	}
	f.admitted.Add(1)
	f.mu.Unlock()
	if f.events != nil {
		f.events.record("admit")
	}
	return func() {
		if f.events != nil {
			f.events.record("admit-release")
		}
	}, nil
}

func (f *spyFence) close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
}

func TestExclusiveTool_SerializesWritersAcrossSessions(t *testing.T) {
	reg := NewLeaseRegistry()
	key := t.TempDir()

	writerA := newRecordingTool("write")
	writerB := newRecordingTool("write")
	t.Cleanup(func() {
		select {
		case <-writerA.release:
		default:
			close(writerA.release)
		}
	})
	toolA := NewExclusiveTool(writerA, key, reg)
	toolB := NewExclusiveTool(writerB, key, reg) // the second agent session

	go func() { _, _ = toolA.Run(context.Background(), callAs("write")) }()
	<-writerA.entered

	go func() { _, _ = toolB.Run(context.Background(), callAs("write")) }()
	require.Never(t, func() bool {
		select {
		case <-writerB.entered:
			return true
		default:
			return false
		}
	}, 300*time.Millisecond, 10*time.Millisecond,
		"the second inner invocation must not enter while the first holds the lease")

	close(writerA.release)
	select {
	case <-writerB.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the second writer never acquired the lease after the first released")
	}
	close(writerB.release)
}

func TestExclusiveTool_ReadToolsEnterUnderHeldLease(t *testing.T) {
	reg := NewLeaseRegistry()
	key := t.TempDir()

	writer := newRecordingTool("write")
	reader := newRecordingTool("view")
	t.Cleanup(func() {
		for _, ch := range []chan struct{}{writer.release, reader.release} {
			select {
			case <-ch:
			default:
				close(ch)
			}
		}
	})

	go func() { _, _ = NewExclusiveTool(writer, key, reg).Run(context.Background(), callAs("write")) }()
	<-writer.entered

	go func() { _, _ = NewExclusiveTool(reader, key, reg).Run(context.Background(), callAs("view")) }()
	select {
	case <-reader.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("a read-only tool must proceed while another session holds the write lease")
	}
	close(reader.release)
}

func TestExclusiveTool_WaitIsBoundedByContextDeadline(t *testing.T) {
	reg := NewLeaseRegistry()
	key := t.TempDir()

	held, err := reg.AcquireExclusive(context.Background(), key)
	require.NoError(t, err)
	t.Cleanup(held)

	writer := newRecordingTool("bash")
	tool := NewExclusiveTool(writer, key, reg)
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()

	_, err = tool.Run(ctx, callAs("bash"))
	require.ErrorIs(t, err, ErrLeaseTimeout)
	require.NotErrorIs(t, err, ErrLeaseCancelled, "timeout must be distinguishable from cancellation")
	require.Zero(t, writer.calls.Load(), "the inner tool never ran")
}

func TestExclusiveTool_FenceOrderingAndLateRejection(t *testing.T) {
	log := &orderedEvents{}
	lease := &spyLease{events: log}
	fence := &spyFence{events: log}
	ctx := WithTaskRunContext(context.Background(), TaskRunContext{Fence: fence})

	tool := newRecordingTool("write")
	t.Cleanup(func() {
		select {
		case <-tool.release:
		default:
			close(tool.release)
		}
	})
	wrapped := NewExclusiveTool(tool, "ws", lease)

	res := make(chan error, 1)
	go func() { _, err := wrapped.Run(ctx, callAs("write")); res <- err }()
	<-tool.entered
	require.Equal(t, []string{"admit", "acquire:exclusive"}, log.snapshot())
	close(tool.release)
	require.NoError(t, <-res)
	require.Equal(t, []string{"admit", "acquire:exclusive", "release:exclusive", "admit-release"}, log.snapshot(),
		"shared admission precedes the lease; both releases follow the call")

	// A fenced (terminalized) run rejects admission before touching
	// the lease registry or the inner tool.
	log.mu.Lock()
	log.events = log.events[:0]
	log.mu.Unlock()
	fence.close()
	_, err := wrapped.Run(ctx, callAs("write"))
	require.Error(t, err)
	require.Empty(t, log.snapshot(), "a fenced run acquires neither admission nor lease")
	require.Equal(t, int32(1), tool.calls.Load(), "the inner tool ran once, before fencing")
}

func TestExclusiveTool_NonExclusiveSkipsLease(t *testing.T) {
	log := &orderedEvents{}
	lease := &spyLease{events: log}
	viewer := newRecordingTool("view")
	t.Cleanup(func() {
		select {
		case <-viewer.release:
		default:
			close(viewer.release)
		}
	})
	wrapped := NewExclusiveTool(viewer, "ws", lease)

	res := make(chan error, 1)
	go func() { _, err := wrapped.Run(context.Background(), callAs("view")); res <- err }()
	<-viewer.entered
	close(viewer.release)
	require.NoError(t, <-res)
	require.Empty(t, log.snapshot(), "read-only tools do not acquire the workspace lease")
}

func TestNeedsWriteLease(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"bash", "edit", "multiedit", "write", "lsp_rename",
		"lsp_replace_symbol", "mcp_github_get_issue", "mcp_any"} {
		require.Truef(t, needsWriteLease(name), "%s must take the write lease", name)
	}
	for _, name := range []string{"view", "ls", "grep", "glob", "read_mcp_resource",
		"list_mcp_resources", "question", "call_agent", "agent_status", "lsp_symbols"} {
		require.Falsef(t, needsWriteLease(name), "%s must not take the write lease", name)
	}
}

func TestExclusiveTool_PreservesInfoAndProviderOptions(t *testing.T) {
	t.Parallel()
	inner := &optionTool{}
	wrapped := NewExclusiveTool(inner, "ws", &spyLease{})
	require.Equal(t, inner.Info(), wrapped.Info(), "name and schema must survive wrapping")
	require.Equal(t, inner.ProviderOptions(), wrapped.ProviderOptions())
	opts := fantasy.ProviderOptions{"test": optionData("x")}
	wrapped.SetProviderOptions(opts)
	require.Equal(t, opts, inner.got)
}

type optionData string

func (optionData) Options()                       {}
func (o optionData) MarshalJSON() ([]byte, error) { return json.Marshal(string(o)) }
func (o optionData) UnmarshalJSON([]byte) error   { return nil }

type optionTool struct{ got fantasy.ProviderOptions }

func (o *optionTool) Info() fantasy.ToolInfo { return fantasy.ToolInfo{Name: "view", Description: "d"} }
func (o *optionTool) Run(context.Context, fantasy.ToolCall) (fantasy.ToolResponse, error) {
	return fantasy.NewTextResponse("ok"), nil
}
func (o *optionTool) ProviderOptions() fantasy.ProviderOptions {
	return fantasy.ProviderOptions{"test": optionData("v")}
}
func (o *optionTool) SetProviderOptions(p fantasy.ProviderOptions) { o.got = p }

func TestLease_SharedJoinsOnlyContiguousPrefix(t *testing.T) {
	reg := NewLeaseRegistry()
	key := "ws-a"
	ctx := context.Background()

	s1, err := reg.AcquireShared(ctx, key)
	require.NoError(t, err)
	s2, err := reg.AcquireShared(ctx, key)
	require.NoError(t, err, "shared joins the running shared set")

	exDone := make(chan error, 1)
	exRel := make(chan Release, 1)
	go func() {
		rel, err := reg.AcquireExclusive(ctx, key)
		if err == nil {
			exRel <- rel
		}
		exDone <- err
	}()
	sq := reg.state(canonicalLeaseKey(key))
	require.Eventually(t, func() bool {
		sq.mu.Lock()
		defer sq.mu.Unlock()
		return len(sq.queue) == 1
	}, time.Second, 5*time.Millisecond)

	laterDone := make(chan error, 1)
	laterRel := make(chan Release, 1)
	go func() {
		rel, err := reg.AcquireShared(ctx, key)
		if err == nil {
			laterRel <- rel
		}
		laterDone <- err
	}()
	require.Eventually(t, func() bool {
		sq.mu.Lock()
		defer sq.mu.Unlock()
		return len(sq.queue) == 2
	}, time.Second, 5*time.Millisecond)
	select {
	case err := <-laterDone:
		t.Fatalf("shared request bypassed the queued exclusive: %v", err)
	case <-time.After(200 * time.Millisecond):
	}

	s1()
	s2()
	require.NoError(t, <-exDone, "exclusive waits for all earlier entries and shared holders")
	select {
	case err := <-laterDone:
		t.Fatalf("shared entered while the exclusive holds: %v", err)
	case <-time.After(200 * time.Millisecond):
	}

	(<-exRel)()
	require.NoError(t, <-laterDone, "the queued shared acquires once the exclusive releases")
	(<-laterRel)()
}

func TestLease_CancellationRemovesQueueEntryAtomically(t *testing.T) {
	reg := NewLeaseRegistry()
	key := "ws-b"
	held, err := reg.AcquireExclusive(context.Background(), key)
	require.NoError(t, err)

	sq := reg.state(canonicalLeaseKey(key))
	cctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { _, err := reg.AcquireShared(cctx, key); errCh <- err }()
	require.Eventually(t, func() bool {
		sq.mu.Lock()
		defer sq.mu.Unlock()
		return len(sq.queue) == 1
	}, time.Second, 5*time.Millisecond)

	cancel()
	require.ErrorIs(t, <-errCh, ErrLeaseCancelled)
	require.Eventually(t, func() bool {
		sq.mu.Lock()
		defer sq.mu.Unlock()
		return len(sq.queue) == 0
	}, time.Second, 5*time.Millisecond, "the cancelled request left the queue")

	held()
	next, err := reg.AcquireShared(context.Background(), key)
	require.NoError(t, err)
	next()
}

func TestLease_TimeoutDistinguishableFromCancel(t *testing.T) {
	reg := NewLeaseRegistry()
	key := "ws-c"
	held, err := reg.AcquireExclusive(context.Background(), key)
	require.NoError(t, err)
	t.Cleanup(held)

	tctx, tcancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer tcancel()
	_, err = reg.AcquireShared(tctx, key)
	require.ErrorIs(t, err, ErrLeaseTimeout)
	require.NotErrorIs(t, err, ErrLeaseCancelled)

	cctx, ccancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		ccancel()
	}()
	defer ccancel()
	_, err = reg.AcquireShared(cctx, key)
	require.ErrorIs(t, err, ErrLeaseCancelled)
	require.NotErrorIs(t, err, ErrLeaseTimeout)
}

func TestLease_ReleaseIsIdempotent(t *testing.T) {
	reg := NewLeaseRegistry()
	key := "ws-d"
	s1, err := reg.AcquireShared(context.Background(), key)
	require.NoError(t, err)
	s2, err := reg.AcquireShared(context.Background(), key)
	require.NoError(t, err)

	s1()
	s1()
	s1()
	sq := reg.state(canonicalLeaseKey(key))
	sq.mu.Lock()
	active := sq.active
	sq.mu.Unlock()
	require.Equal(t, 1, active, "only the first release changed lease state")

	s2()
	rel, err := reg.AcquireExclusive(context.Background(), key)
	require.NoError(t, err)
	rel()
}

func TestLeaseRegistry_CanonicalizesKey(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	reg := NewLeaseRegistry()
	held, err := reg.AcquireExclusive(context.Background(), dir)
	require.NoError(t, err)
	t.Cleanup(held)

	alias := dir + string(filepath.Separator) + "."
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	_, err = reg.AcquireExclusive(ctx, alias)
	require.ErrorIs(t, err, ErrLeaseTimeout, "an equivalent path spelling maps onto the same lease")
}

func TestBashTaskRunRejectsDetachedJobs(t *testing.T) {
	tool := newBashToolForTest(t.TempDir())
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")
	ctx = WithTaskRunContext(ctx, TaskRunContext{Fence: &spyFence{}})

	input, err := json.Marshal(BashParams{
		Description:     "detach",
		Command:         "echo done",
		RunInBackground: true,
	})
	require.NoError(t, err)
	resp, err := tool.Run(ctx, fantasy.ToolCall{ID: "c", Name: BashToolName, Input: string(input)})
	require.NoError(t, err)
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "run_in_background is not available")
}

func TestTaskRunContextRoundTrip(t *testing.T) {
	t.Parallel()
	_, ok := GetTaskRunContextFromContext(context.Background())
	require.False(t, ok, "a plain context is not a task run")
	tr := TaskRunContext{Fence: &spyFence{}}
	got, ok := GetTaskRunContextFromContext(WithTaskRunContext(context.Background(), tr))
	require.True(t, ok)
	require.Equal(t, tr.Fence, got.Fence)
}
