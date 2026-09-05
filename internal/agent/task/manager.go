package task

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/google/uuid"
)

// DefaultRunningPerModel bounds concurrently running attempts per
// (provider, model) capacity key when the configured value is not
// positive. Model capacity is always finite so the FIFO queue keeps
// its backpressure; live-task quotas are the configurable limits.
const DefaultRunningPerModel = 10

// Limits bounds live task quota and running-model capacity.
// Non-positive live limits mean unlimited; a non-positive
// RunningPerModel falls back to DefaultRunningPerModel.
type Limits struct {
	LiveTasksPerParent    int
	LiveTasksPerWorkspace int
	RunningPerModel       int
}

func (l *Limits) applyDefaults() {
	if l.LiveTasksPerParent <= 0 {
		l.LiveTasksPerParent = math.MaxInt
	}
	if l.LiveTasksPerWorkspace <= 0 {
		l.LiveTasksPerWorkspace = math.MaxInt
	}
	if l.RunningPerModel <= 0 {
		l.RunningPerModel = DefaultRunningPerModel
	}
}

// Config wires a Manager. Store defaults to MemoryStore.
type Config struct {
	WorkspaceID string
	Limits      Limits
	Store       Store
}

// Runner executes one task attempt against the child session. It
// must return promptly when ctx is cancelled. The Handle lets the
// runner move the attempt between running and waiting_for_input and
// reports the attempt's mailbox prompt.
type Runner func(ctx context.Context, h *Handle) (Result, error)

// StartRequest describes one task attempt. CallerSessionID and
// CallerDepth are trusted identity supplied by the tool boundary,
// never by task arguments. For a fresh delegation ChildSessionID
// names the deterministic child session id the admission
// transaction inserts; for a continuation (ResumesTaskID set) it is
// ignored and the retained child session is bound by the store.
type StartRequest struct {
	CallerSessionID   string
	CallerDepth       int
	ParentSessionID   string
	ChildSessionID    string
	ChildTitle        string
	ParentMessageID   string
	ToolCallID        string
	Profile           string
	ProfileGeneration uint64
	RequestedModel    string
	FallbackModels    []string
	PromptFingerprint string
	ToolFingerprint   string
	Provider          string
	Model             string
	Prompt            string
	ResumesTaskID     string
	Run               Runner
}

// Manager owns task state, capacity queueing, mailbox dispatch,
// cancellation, and terminalization for one workspace. Message
// delivery, typed-question resume, cancellation, and terminal
// fencing all flow through the manager lock, so no two attempts for
// one child session ever execute concurrently. It is safe for
// concurrent use.
type Manager struct {
	ctx         context.Context
	workspaceID string
	limits      Limits
	store       Store
	events      *broker

	mu           sync.Mutex
	closed       bool
	liveTotal    int
	liveByParent map[string]int
	running      map[CapacityKey]int
	queue        map[CapacityKey][]*queueItem
	live         map[string]*attemptState
	// children keeps one runner binding per retained child session
	// so queued messages and continuations can start successor
	// attempts long after the admission call returned.
	children map[string]*childState
	// queuedMsgs counts appended (non-admission) mailbox messages
	// per child session, the in-memory hint for follow-up dispatch.
	queuedMsgs map[string]int
	// Bounded retry budget for terminalization transactions that
	// fail transiently. When the budget expires the attempt stays
	// in m.live so Shutdown's interruptRemaining and startup
	// RecoverLiveTasks remain the durable reconcilers. Tests shrink
	// the budget to run fast.
	terminalRetryAttempts  int
	terminalRetryBaseDelay time.Duration
	terminalRetryMaxDelay  time.Duration
}

// New returns a Manager whose task lifetime is bound to ctx (the
// workspace context), not to any caller context.
func New(ctx context.Context, cfg Config) *Manager {
	cfg.Limits.applyDefaults()
	store := cfg.Store
	if store == nil {
		store = NewMemoryStore()
	}
	return &Manager{
		ctx:          ctx,
		workspaceID:  cfg.WorkspaceID,
		limits:       cfg.Limits,
		store:        store,
		events:       newBroker(),
		liveByParent: map[string]int{},
		running:      map[CapacityKey]int{},
		queue:        map[CapacityKey][]*queueItem{},
		live:         map[string]*attemptState{},
		children:     map[string]*childState{},
		queuedMsgs:   map[string]int{},

		terminalRetryAttempts:  8,
		terminalRetryBaseDelay: 250 * time.Millisecond,
		terminalRetryMaxDelay:  4 * time.Second,
	}
}

// Subscribe registers a lifecycle event callback and returns its
// unsubscribe function. Callbacks run synchronously outside the
// manager lock and must not block.
func (m *Manager) Subscribe(fn func(Event)) func() {
	return m.events.subscribe(fn)
}

// Store returns the manager's durable task store, the source of
// truth for owner-scoped task, outbox, and inbox resync reads.
func (m *Manager) Store() Store { return m.store }

// Start durably admits one task attempt and returns the acceptance
// snapshot without waiting for the child. Admission is atomic: the
// store creates the pending task, the child-session binding (for a
// fresh delegation), and the sequence-zero mailbox row in one
// transaction, or none of them. The live-quota reservation taken
// before the transaction is released if it fails.
func (m *Manager) Start(ctx context.Context, req StartRequest) (*Task, error) {
	switch {
	case req.CallerDepth != 0:
		return nil, ErrDelegation
	case req.CallerSessionID == "" || req.Prompt == "" || req.Run == nil:
		return nil, fmt.Errorf("%w: caller session, prompt, and runner are required", ErrInvalidRequest)
	}

	t, _, err := m.create(ctx, req)
	if err != nil {
		return nil, err
	}
	return t, nil
}

// create validates, reserves quota, atomically admits the pending
// record with its child session and mailbox row, and enqueues the
// attempt for capacity. Events are published after the lock is
// released.
func (m *Manager) create(ctx context.Context, req StartRequest) (*Task, *attemptState, error) {
	parent := req.ParentSessionID
	if parent == "" {
		parent = req.CallerSessionID
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, nil, ErrShuttingDown
	}
	if !m.reserveQuotaLocked(parent) {
		return nil, nil, ErrQuota
	}
	now := time.Now()
	tpl := &Task{
		ID:                uuid.NewString(),
		OwnerSessionID:    req.CallerSessionID,
		ParentSessionID:   parent,
		ChildSessionID:    req.ChildSessionID,
		ParentMessageID:   req.ParentMessageID,
		ToolCallID:        req.ToolCallID,
		Profile:           req.Profile,
		ProfileGeneration: req.ProfileGeneration,
		RequestedModel:    req.RequestedModel,
		FallbackModels:    req.FallbackModels,
		PromptFingerprint: req.PromptFingerprint,
		ToolFingerprint:   req.ToolFingerprint,
		Provider:          req.Provider,
		Model:             req.Model,
		RunGeneration:     1,
		ResumesTaskID:     req.ResumesTaskID,
		Prompt:            req.Prompt,
		Status:            StatusPending,
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	if req.ResumesTaskID != "" {
		tpl.RunGeneration = 0 // the repository derives it from the lineage
	}
	adm := Admission{
		Task: tpl,
		Child: ChildSpec{
			ID:       req.ChildSessionID,
			Title:    req.ChildTitle,
			ParentID: parent,
			New:      req.ResumesTaskID == "",
		},
	}
	saved, err := m.store.CreatePendingTask(ctx, adm)
	if err != nil {
		m.releaseQuotaLocked(parent)
		return nil, nil, err
	}

	child := m.bindChildLocked(saved, parent, req.Run)
	at := m.newAttemptLocked(saved, child)
	m.live[at.id] = at
	at.child.wish = m.queueLocked(child, &queueItem{child: child, at: at})
	// Dispatch runs on the workspace context, never the caller's:
	// the admission committed, so a request cancelled mid-Start must
	// not strand the freshly accepted attempt in pending.
	events := append([]Event{{Type: EventCreated, Task: saved.clone()}}, m.pumpLocked(m.ctx, child.key)...)
	m.mu.Unlock()
	m.events.publish(events...)
	m.mu.Lock()
	return saved, at, nil
}

// reserveQuotaLocked takes one live-task slot for parent, reporting
// whether quota allowed it. Callers must hold m.mu.
func (m *Manager) reserveQuotaLocked(parent string) bool {
	if m.liveByParent[parent] >= m.limits.LiveTasksPerParent ||
		m.liveTotal >= m.limits.LiveTasksPerWorkspace {
		return false
	}
	m.liveByParent[parent]++
	m.liveTotal++
	return true
}

// releaseQuotaLocked frees one live-task slot. Callers must hold
// m.mu.
func (m *Manager) releaseQuotaLocked(parent string) {
	m.liveTotal--
	if n := m.liveByParent[parent]; n <= 1 {
		delete(m.liveByParent, parent)
	} else {
		m.liveByParent[parent] = n - 1
	}
}

// runAttempt executes one dispatched attempt to settlement. The
// running slot was already claimed by the dispatcher before this
// goroutine started.
func (m *Manager) runAttempt(at *attemptState) {
	defer at.closeDone()
	if !m.stillLive(at.id) {
		return
	}
	res, err := at.child.run(at.runCtx, at.handle)
	m.settle(at, res, err)
}

// settle maps the runner outcome to a terminal state: cancellation
// wins over errors, which win over success. The runner's usage
// snapshot rides into the terminal transaction, which aggregates it
// into the parent exactly once for the attempt's run generation.
func (m *Manager) settle(at *attemptState, res Result, runErr error) {
	u := TerminalUpdate{CompletedAt: time.Now(), Summary: res.Summary, Usage: res.Usage}
	u.Result, u.ResultTruncated = TruncateResult(res.Text)
	switch {
	case at.cancelRequested.Load():
		u.Status = StatusCancelled
	case runErr != nil:
		u.Status = StatusFailed
		u.Err = runErr.Error()
	default:
		u.Status = StatusCompleted
	}
	m.finish(m.ctx, at, u)
}
