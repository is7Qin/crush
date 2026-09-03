package task

import (
	"context"
	"sort"
	"sync"
	"time"
)

// Store persists task records. The manager is the single writer and
// serializes its calls, so implementations only need to be safe for
// concurrent reads. SQLiteStore (store_sqlite.go) implements this
// contract over the shared SQLite database; TerminalizeAndDeliver
// maps to one transaction whose conditional UPDATE decides the
// winner.
//
// The dispatch and terminal methods are the durable-delivery contract
// from docs/specs/crush-agents/10-remaining-integration.md: each is
// one SQLite transaction over the shared connection. Callers never
// receive a raw *sql.Tx and never issue a second write outside the
// repository transaction. A committed pending task always has its
// child session and its admission mailbox row; a failed admission
// leaves neither visible. A committed terminalization always has its
// outbox row, inbox row, and (first time for that generation) parent
// cost aggregation; a failed terminal transaction leaves none.
type Store interface {
	// Save inserts or replaces the full record.
	Save(ctx context.Context, t *Task) error
	// Get returns a copy of the stored task or ErrNotFound.
	Get(ctx context.Context, id string) (*Task, error)
	// ListByOwner returns copies of the owner's tasks, oldest first.
	ListByOwner(ctx context.Context, ownerSessionID string) ([]*Task, error)
	// ListLiveTasks returns copies of every non-terminal task,
	// oldest first, for startup recovery.
	ListLiveTasks(ctx context.Context) ([]*Task, error)
	// TerminalizeAndDeliver atomically applies u to the task while
	// the stored record is not terminal and still at runGeneration,
	// stores u.Usage as the attempt's usage snapshot, aggregates the
	// usage into the parent session exactly once per run generation,
	// and inserts one outbox row for the terminal event plus one
	// inbox row for the parent result envelope. It reports the
	// committed record and whether this call won the transition.
	// The same transaction resolves any still-pending task question
	// for the attempt as interrupted (and applies u.Question's
	// fenced resolution when set), so a terminalized attempt never
	// leaves a durable pending question behind. Any error rolls the
	// whole transaction back; a repeat call for
	// an already-terminal task never re-applies and never
	// duplicates.
	TerminalizeAndDeliver(ctx context.Context, id string, runGeneration uint64, u TerminalUpdate) (*Task, bool, error)
	// CreatePendingTask atomically admits one attempt: it persists
	// the pending task, binds the child session (inserting the
	// sessions row when Child.New), and queues the admission prompt
	// as the child's next mailbox sequence. For a continuation it
	// validates that the predecessor is the owner's and terminal,
	// then fills ChildSessionID and RunGeneration. Any failure rolls
	// back the task, child session, and mailbox row together.
	CreatePendingTask(ctx context.Context, adm Admission) (*Task, error)
	// AppendChildMessage queues msg under the addressed task's
	// child session with a transactionally allocated sequence.
	// Messages may be appended while the task is pending, running,
	// waiting for input, or terminal.
	AppendChildMessage(ctx context.Context, msg ChildMessage) (ChildMessage, error)
	// DispatchNextChildMessage claims the child session's lowest
	// queued message, marks it delivered, and either transitions
	// the bound pending attempt to running or creates its successor
	// attempt (run_generation + 1, resumes_task_id, message_id). It
	// writes the start outbox row in the same transaction. Nothing
	// is claimed while the current attempt is running or waiting
	// for input, so no two attempts for one child session execute
	// concurrently. It reports the dispatched record and whether a
	// message was claimed.
	DispatchNextChildMessage(ctx context.Context, childSessionID string) (*Task, bool, error)
	// CancelPendingIfLive terminalizes a pending attempt, rejects
	// its undelivered admission message with the given reason, and
	// performs the full terminal delivery (task update, bounded
	// result metadata, outbox row, inbox row) in one transaction. A
	// non-pending task loses the race and is returned unchanged.
	CancelPendingIfLive(ctx context.Context, id string, u TerminalUpdate, reason string) (*Task, bool, error)
	// ListChildMessages returns the child session's mailbox rows in
	// sequence order.
	ListChildMessages(ctx context.Context, childSessionID string) ([]ChildMessage, error)
	// ListOutbox returns copies of the undelivered entries, oldest
	// first.
	ListOutbox(ctx context.Context) ([]*OutboxEntry, error)
	// AckOutbox stamps the given entries delivered at deliveredAt.
	// Unknown ids are ignored.
	AckOutbox(ctx context.Context, ids []string, deliveredAt time.Time) error
	// ListInbox returns copies of the owner's undelivered inbox
	// rows, oldest first. An empty ownerSessionID lists every
	// undelivered row across owners.
	ListInbox(ctx context.Context, ownerSessionID string) ([]*InboxEntry, error)
	// AckInbox stamps the given rows delivered at deliveredAt.
	// Unknown ids are ignored.
	AckInbox(ctx context.Context, ids []string, deliveredAt time.Time) error
	// BeginQuestionWait inserts row's pending question row and
	// transitions its task attempt running -> waiting_for_input in
	// one transaction, fenced on task id, run generation, owner
	// session, and child session. Any failure rolls back both
	// changes; it reports the committed task record. A second
	// pending question for one task is rejected by storage.
	BeginQuestionWait(ctx context.Context, row QuestionRow) (*Task, error)
	// ResolveQuestionAnswered transitions the question row pending
	// -> out.Resolution and verifies the attempt is still
	// waiting_for_input for the same identity fence, in one
	// transaction. A lost fence returns ErrQuestionStale with
	// nothing changed.
	ResolveQuestionAnswered(ctx context.Context, out QuestionOutcome) error
	// QuestionStatus returns the stored resolution name of a
	// question row ('pending' until resolved). Unknown ids return
	// ErrNotFound.
	QuestionStatus(ctx context.Context, questionID string) (string, error)
}

// outboxKey is the store-level uniqueness constraint for outbox
// entries: one record per task attempt and lifecycle fact.
type outboxKey struct {
	taskID        string
	runGeneration uint64
	eventType     EventType
}

func outboxKeyOf(e *OutboxEntry) outboxKey {
	return outboxKey{taskID: e.TaskID, runGeneration: e.RunGeneration, eventType: e.EventType}
}

// inboxKey is the store-level uniqueness constraint for inbox rows:
// one parent delivery per task terminalization.
type inboxKey struct {
	ownerSessionID     string
	taskID             string
	terminalGeneration uint64
}

// MemoryStore is the in-memory Store implementation. Its child
// session registry and mailbox mirror the SQLite dispatch
// transactions under the same lock, so both stores implement one
// contract.
type MemoryStore struct {
	mu    sync.RWMutex
	tasks map[string]*Task

	outbox     []*OutboxEntry
	outboxSeen map[outboxKey]bool

	inbox     []*InboxEntry
	inboxSeen map[inboxKey]bool

	// parentUsage mirrors the SQLite parent-session cost
	// aggregation: one cumulative delta per parent session, applied
	// exactly once per task run generation.
	parentUsage map[string]UsageDelta

	// children maps child session id to its admission binding.
	children map[string]memoryChild
	// mailbox holds every appended message, ordered per child by
	// sequence.
	mailbox map[string][]*ChildMessage

	// questions mirrors the agent_task_questions rows the
	// task-question transactions touch, keyed by question id, so
	// both stores implement one contract. Its methods live in
	// store_memory_question.go.
	questions map[string]*memoryQuestion
}

// memoryChild records a child session inserted by an admission.
type memoryChild struct {
	parentSessionID string
	title           string
}

// NewMemoryStore returns an empty MemoryStore.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		tasks:       map[string]*Task{},
		outboxSeen:  map[outboxKey]bool{},
		inboxSeen:   map[inboxKey]bool{},
		parentUsage: map[string]UsageDelta{},
		children:    map[string]memoryChild{},
		mailbox:     map[string][]*ChildMessage{},
		questions:   map[string]*memoryQuestion{},
	}
}

// Save inserts or replaces a copy of t.
func (s *MemoryStore) Save(_ context.Context, t *Task) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tasks[t.ID] = t.clone()
	return nil
}

// Get returns a copy of the stored task or ErrNotFound.
func (s *MemoryStore) Get(_ context.Context, id string) (*Task, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.tasks[id]
	if !ok {
		return nil, ErrNotFound
	}
	return t.clone(), nil
}

// ListByOwner returns copies of the owner's tasks, oldest first.
func (s *MemoryStore) ListByOwner(_ context.Context, ownerSessionID string) ([]*Task, error) {
	s.mu.RLock()
	var out []*Task
	for _, t := range s.tasks {
		if t.OwnerSessionID == ownerSessionID {
			out = append(out, t.clone())
		}
	}
	s.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

// ListLiveTasks returns copies of every non-terminal task, oldest
// first.
func (s *MemoryStore) ListLiveTasks(_ context.Context) ([]*Task, error) {
	s.mu.RLock()
	var out []*Task
	for _, t := range s.tasks {
		if !t.Status.Terminal() {
			out = append(out, t.clone())
		}
	}
	s.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

// TerminalizeAndDeliver applies the full terminal transaction under
// the store lock: every validation (including the optional question
// fence) runs before any mutation, so a failed or lost call leaves
// no outbox, inbox, parent usage, or question change behind,
// mirroring the SQLite transaction.
func (s *MemoryStore) TerminalizeAndDeliver(_ context.Context, id string, runGeneration uint64, u TerminalUpdate) (*Task, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[id]
	if !ok {
		return nil, false, ErrNotFound
	}
	if t.Status.Terminal() || t.RunGeneration != runGeneration {
		return t.clone(), false, nil
	}
	if u.Question != nil {
		if err := s.fenceQuestionLocked(t, *u.Question); err != nil {
			return t.clone(), false, err
		}
	}
	s.deliverLocked(t, u)
	return t.clone(), true, nil
}

// deliverLocked applies a won terminal transition and its durable
// delivery: task state, usage snapshot, one-time parent aggregation,
// outbox row, and inbox row. Callers must hold s.mu and must have
// verified the transition wins.
func (s *MemoryStore) deliverLocked(t *Task, u TerminalUpdate) {
	t.Status = u.Status
	t.Result = u.Result
	t.Summary = u.Summary
	t.Err = u.Err
	t.ResultTruncated = u.ResultTruncated
	t.CompletedAt = u.CompletedAt
	t.UpdatedAt = u.CompletedAt
	t.TerminalGeneration = t.RunGeneration
	t.PromptTokens = u.Usage.PromptTokens
	t.CompletionTokens = u.Usage.CompletionTokens
	t.Cost = u.Usage.Cost
	if t.ParentSessionID != "" && t.CostAggregatedGeneration != t.RunGeneration {
		p := s.parentUsage[t.ParentSessionID]
		p.PromptTokens += u.Usage.PromptTokens
		p.CompletionTokens += u.Usage.CompletionTokens
		p.Cost += u.Usage.Cost
		s.parentUsage[t.ParentSessionID] = p
		t.CostAggregatedGeneration = t.RunGeneration
	}
	s.appendOutboxUnlocked(terminalOutboxEntry(t))
	s.appendInboxUnlocked(inboxEntryFor(t))
	s.resolveQuestionsLocked(t, u)
}

// ParentUsage returns the cumulative usage aggregated into a parent
// session by terminal deliveries.
func (s *MemoryStore) ParentUsage(sessionID string) UsageDelta {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.parentUsage[sessionID]
}

// appendInboxUnlocked records an inbox row unless the same
// (owner, task, terminal generation) row exists. Callers must hold
// s.mu.
func (s *MemoryStore) appendInboxUnlocked(e *InboxEntry) {
	key := inboxKey{ownerSessionID: e.OwnerSessionID, taskID: e.TaskID, terminalGeneration: e.TerminalGeneration}
	if s.inboxSeen[key] {
		return
	}
	s.inboxSeen[key] = true
	c := *e
	s.inbox = append(s.inbox, &c)
}

// ListInbox returns copies of the undelivered inbox rows, oldest
// first. An empty ownerSessionID lists every owner's rows.
func (s *MemoryStore) ListInbox(_ context.Context, ownerSessionID string) ([]*InboxEntry, error) {
	s.mu.RLock()
	var out []*InboxEntry
	for _, e := range s.inbox {
		if e.DeliveredAt.IsZero() && (ownerSessionID == "" || e.OwnerSessionID == ownerSessionID) {
			c := *e
			out = append(out, &c)
		}
	}
	s.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

// AckInbox stamps the given rows delivered.
func (s *MemoryStore) AckInbox(_ context.Context, ids []string, deliveredAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	for _, e := range s.inbox {
		if want[e.ID] {
			e.DeliveredAt = deliveredAt
		}
	}
	return nil
}

// appendOutboxUnlocked records an outbox entry unless the same
// (task, run_generation, event_type) row exists. The payload must
// already be encoded; the append itself cannot fail. Callers must
// hold s.mu.
func (s *MemoryStore) appendOutboxUnlocked(e *OutboxEntry) {
	key := outboxKeyOf(e)
	if s.outboxSeen[key] {
		return
	}
	s.outboxSeen[key] = true
	c := *e
	s.outbox = append(s.outbox, &c)
}

// ListOutbox returns copies of the undelivered entries, oldest first.
func (s *MemoryStore) ListOutbox(_ context.Context) ([]*OutboxEntry, error) {
	s.mu.RLock()
	var out []*OutboxEntry
	for _, e := range s.outbox {
		if e.DeliveredAt.IsZero() {
			c := *e
			out = append(out, &c)
		}
	}
	s.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

// AckOutbox stamps the given entries delivered.
func (s *MemoryStore) AckOutbox(_ context.Context, ids []string, deliveredAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	for _, e := range s.outbox {
		if want[e.ID] {
			e.DeliveredAt = deliveredAt
		}
	}
	return nil
}
