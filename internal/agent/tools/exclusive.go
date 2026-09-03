package tools

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"charm.land/fantasy"
)

// Lease bounds workspace mutation to one writer at a time, with
// concurrent readers allowed. Implementations are process-local, keyed
// by canonical workspace path, and queue every acquisition through one
// FIFO order: a shared request joins only the contiguous shared prefix
// ahead of the first queued exclusive request; an exclusive request
// waits for all earlier entries and all shared holders. A cancelled or
// timed-out request removes its queue entry atomically, so later
// requests cannot bypass it.
type Lease interface {
	AcquireShared(ctx context.Context, key string) (Release, error)
	AcquireExclusive(ctx context.Context, key string) (Release, error)
}

// Release gives back one granted lease. It is safe to call once or
// multiple times; only the first call changes lease state.
type Release func()

var (
	// ErrLeaseCancelled reports context cancellation while waiting.
	ErrLeaseCancelled = errors.New("workspace lease cancelled")
	// ErrLeaseTimeout reports expiry of the derived lease deadline.
	ErrLeaseTimeout = errors.New("workspace lease timed out")
)

// maxLeaseWait bounds a lease wait with no task deadline. The wrapper
// shrinks it to the remaining task max_duration when the run context
// carries one.
const maxLeaseWait = 5 * time.Minute

// Fence is the per-task-run admission seam implemented by the task
// manager's execution fence: every child tool call takes shared
// admission for the full invocation, and terminalization takes
// exclusive admission before committing the terminal transition. Once
// fenced, shared admission fails and the late runner can only perform
// local cleanup.
type Fence interface {
	AdmitShared(ctx context.Context) (func(), error)
}

// taskRunContextKey is the context key for the trusted task-run
// execution identity of a child tool call.
type taskRunContextKey struct{}

// TaskRunContext carries what the workspace wrapper needs from the
// task boundary it runs under. The coordinator stamps it onto the
// manager-supplied run context, never from tool arguments, so a forged
// call cannot escape the execution fence.
type TaskRunContext struct {
	// Fence gates this attempt's tool admission.
	Fence Fence
}

// WithTaskRunContext returns a context carrying the trusted task-run
// identity tr.
func WithTaskRunContext(ctx context.Context, tr TaskRunContext) context.Context {
	return context.WithValue(ctx, taskRunContextKey{}, tr)
}

// GetTaskRunContextFromContext retrieves the task-run identity. A
// missing value means the call is not running inside a task attempt,
// so there is no execution fence and detached shell jobs are allowed.
func GetTaskRunContextFromContext(ctx context.Context) (TaskRunContext, bool) {
	tr, ok := ctx.Value(taskRunContextKey{}).(TaskRunContext)
	return tr, ok
}

// exclusiveToolNames is the conservative set of ordinary tools that
// mutate the workspace and therefore take the exclusive write lease.
// Every mcp_ tool is exclusive too; see needsWriteLease.
var exclusiveToolNames = map[string]bool{
	BashToolName:          true,
	EditToolName:          true,
	MultiEditToolName:     true,
	WriteToolName:         true,
	RenameToolName:        true,
	ReplaceSymbolToolName: true,
}

const mcpToolPrefix = "mcp_"

// needsWriteLease reports whether the named tool must hold the
// workspace write lease. Identification is conservative by name: any
// MCP tool is treated as mutating because its side effects are opaque.
func needsWriteLease(name string) bool {
	return exclusiveToolNames[name] || strings.HasPrefix(name, mcpToolPrefix)
}

// exclusiveTool wraps an inner tool with task-run shared admission and
// (for conservative mutating tools) the workspace write lease. It
// preserves the inner tool's name, schema, provider options, and
// response.
type exclusiveTool struct {
	inner      fantasy.AgentTool
	key        string
	writeLease bool
	leases     Lease
}

// NewExclusiveTool wraps inner for the workspace identified by
// workspaceKey, acquiring leases from leases.
func NewExclusiveTool(inner fantasy.AgentTool, workspaceKey string, leases Lease) fantasy.AgentTool {
	return &exclusiveTool{
		inner:      inner,
		key:        workspaceKey,
		writeLease: needsWriteLease(inner.Info().Name),
		leases:     leases,
	}
}

func (e *exclusiveTool) Info() fantasy.ToolInfo { return e.inner.Info() }

func (e *exclusiveTool) ProviderOptions() fantasy.ProviderOptions {
	return e.inner.ProviderOptions()
}

func (e *exclusiveTool) SetProviderOptions(opts fantasy.ProviderOptions) {
	e.inner.SetProviderOptions(opts)
}

func (e *exclusiveTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	if tr, ok := GetTaskRunContextFromContext(ctx); ok && tr.Fence != nil {
		release, err := tr.Fence.AdmitShared(ctx)
		if err != nil {
			return fantasy.ToolResponse{}, fmt.Errorf("%s: %w", call.Name, err)
		}
		defer release()
	}
	if e.writeLease {
		release, err := acquireWriteLease(ctx, e.leases, e.key)
		if err != nil {
			return fantasy.ToolResponse{}, fmt.Errorf("%s: %w", call.Name, err)
		}
		defer release()
	}
	return e.inner.Run(ctx, call)
}

// acquireWriteLease takes the exclusive workspace lease bounded by the
// smaller of maxLeaseWait and the remaining deadline on ctx (the task
// max_duration carried by the run context).
func acquireWriteLease(ctx context.Context, leases Lease, key string) (Release, error) {
	timeout := maxLeaseWait
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining < timeout {
			timeout = remaining
		}
	}
	actx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return leases.AcquireExclusive(actx, key)
}

// WrapToolsExclusive wraps every tool with the task-run admission and
// workspace write lease. Hook wrapping happens outside this call so a
// hook denial is decided before any lease is acquired.
func WrapToolsExclusive(list []fantasy.AgentTool, workspaceKey string, leases Lease) []fantasy.AgentTool {
	out := make([]fantasy.AgentTool, len(list))
	for i, tool := range list {
		out[i] = NewExclusiveTool(tool, workspaceKey, leases)
	}
	return out
}

// leaseWaiter is one queued acquisition request.
type leaseWaiter struct {
	exclusive bool
	ready     chan struct{} // closed under state.mu once granted
}

// leaseState is the FIFO lease bookkeeping for one workspace key.
type leaseState struct {
	mu     sync.Mutex
	active int // shared holders, or 1 while an exclusive is held
	excl   bool
	queue  []*leaseWaiter
}

// LeaseRegistry is a process-local lease registry keyed by canonical
// workspace path. It is safe for concurrent use.
type LeaseRegistry struct {
	mu    sync.Mutex
	byKey map[string]*leaseState
}

var processLeases = &LeaseRegistry{byKey: map[string]*leaseState{}}

// WorkspaceLeases returns the process-local lease registry that all
// tool wiring in this process shares.
func WorkspaceLeases() Lease { return processLeases }

// NewLeaseRegistry returns an isolated registry, for tests and callers
// that must not share the process-wide one.
func NewLeaseRegistry() *LeaseRegistry { return &LeaseRegistry{byKey: map[string]*leaseState{}} }

// state returns the canonicalized key's lease state. Canonicalization
// mirrors the backend workspace resolver: filepath.Abs then
// filepath.EvalSymlinks with a cleaned-abs fallback.
func (r *LeaseRegistry) state(key string) *leaseState {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.byKey[key]; ok {
		return s
	}
	s := &leaseState{}
	r.byKey[key] = s
	return s
}

func (r *LeaseRegistry) AcquireShared(ctx context.Context, key string) (Release, error) {
	return r.acquire(ctx, canonicalLeaseKey(key), false)
}

func (r *LeaseRegistry) AcquireExclusive(ctx context.Context, key string) (Release, error) {
	return r.acquire(ctx, canonicalLeaseKey(key), true)
}

func (r *LeaseRegistry) acquire(ctx context.Context, key string, exclusive bool) (Release, error) {
	s := r.state(key)
	w := &leaseWaiter{exclusive: exclusive, ready: make(chan struct{})}
	s.mu.Lock()
	s.queue = append(s.queue, w)
	s.pumpLocked()
	granted := isClosedLocked(w.ready)
	s.mu.Unlock()
	if !granted {
		select {
		case <-w.ready:
		case <-ctx.Done():
			s.mu.Lock()
			if !isClosedLocked(w.ready) {
				s.removeLocked(w)
				s.pumpLocked()
				s.mu.Unlock()
				return nil, leaseErr(ctx)
			}
			// Granted concurrently with cancellation: hand the
			// grant straight back and report the cancellation.
			s.releaseLocked(w)
			s.pumpLocked()
			s.mu.Unlock()
			return nil, leaseErr(ctx)
		}
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			s.releaseLocked(w)
			s.pumpLocked()
			s.mu.Unlock()
		})
	}, nil
}

// pumpLocked grants queue entries from the head for as long as the
// current state allows. Callers hold s.mu.
func (s *leaseState) pumpLocked() {
	for len(s.queue) > 0 {
		w := s.queue[0]
		if w.exclusive {
			if s.active != 0 {
				return // waits for all holders and earlier entries
			}
			s.queue = s.queue[1:]
			s.active = 1
			s.excl = true
			close(w.ready)
			continue
		}
		if s.excl {
			return // an exclusive holder owns the workspace
		}
		s.queue = s.queue[1:]
		s.active++
		close(w.ready)
	}
}

// releaseLocked returns one grant to the pool. Callers hold s.mu.
func (s *leaseState) releaseLocked(w *leaseWaiter) {
	s.active--
	if w.exclusive {
		s.excl = false
	}
}

// removeLocked drops an ungranted entry. Callers hold s.mu.
func (s *leaseState) removeLocked(w *leaseWaiter) {
	for i, e := range s.queue {
		if e == w {
			s.queue = append(s.queue[:i:i], s.queue[i+1:]...)
			return
		}
	}
}

func isClosedLocked(ch chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func leaseErr(ctx context.Context) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return ErrLeaseTimeout
	}
	return ErrLeaseCancelled
}

// canonicalLeaseKey resolves a workspace path to the same canonical
// form the backend workspace resolver produces.
func canonicalLeaseKey(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	return abs
}
