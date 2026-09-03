package task

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/charmbracelet/crush/internal/pubsub"
)

// OutboxEntry is one durable lifecycle record. Stores enforce at most
// one entry per (TaskID, RunGeneration, EventType), so a replayed
// terminalization can never duplicate a row.
type OutboxEntry struct {
	ID            string    `json:"id"`
	TaskID        string    `json:"task_id"`
	RunGeneration uint64    `json:"run_generation"`
	EventType     EventType `json:"event_type"`
	// Payload is the JSON snapshot of the task at terminalization.
	Payload     string    `json:"payload"`
	CreatedAt   time.Time `json:"created_at"`
	DeliveredAt time.Time `json:"delivered_at,omitempty"`
}

// Task decodes the entry payload back into the task snapshot.
func (e *OutboxEntry) Task() (*Task, error) {
	var t Task
	if err := json.Unmarshal([]byte(e.Payload), &t); err != nil {
		return nil, fmt.Errorf("decode outbox entry %s: %w", e.ID, err)
	}
	return &t, nil
}

// Outbox returns the durable undelivered lifecycle entries, oldest
// first. The outbox, not pub/sub, is the completion record of truth.
func (m *Manager) Outbox(ctx context.Context) ([]*OutboxEntry, error) {
	return m.store.ListOutbox(ctx)
}

// AckOutbox marks the given entries delivered so they leave the
// pending set. Unknown ids are ignored.
func (m *Manager) AckOutbox(ctx context.Context, ids ...string) error {
	if len(ids) == 0 {
		return nil
	}
	return m.store.AckOutbox(ctx, ids, time.Now())
}

// OutboxNotifier is the workspace-scoped completion/result
// notification adapter. It replays a session's durable terminal
// entries onto a pub/sub broker and acks them, so the primary session
// can consume completions that a dropped SSE or pub/sub delivery lost
// without re-entering an agent run.
type OutboxNotifier struct {
	manager *Manager
	events  pubsub.Publisher[Event]
}

// NewOutboxNotifier returns a notifier that replays m's outbox onto
// pub. pub is a wake-up hint channel; the task rows stay the source
// of truth.
func NewOutboxNotifier(m *Manager, pub pubsub.Publisher[Event]) *OutboxNotifier {
	return &OutboxNotifier{manager: m, events: pub}
}

// Drain republishes ownerSessionID's undelivered terminal entries as
// UpdatedEvent hints and acks them. An empty ownerSessionID replays
// every undelivered entry. Delivery is at-least-once: a crash between
// publish and ack replays the entry on the next drain, which is safe
// because events are facts, not commands.
func (n *OutboxNotifier) Drain(ctx context.Context, ownerSessionID string) error {
	return n.drain(ctx, ownerSessionID, Event{})
}

// DrainExcept is Drain for one owner's entries except the one
// recording except's transition. A caller that live-publishes a
// terminal event and then drains has not proven any consumer received
// it: the triggering row must stay pending so a client whose stream
// dropped recovers the fact through resync. It leaves the pending set
// on that owner's next terminal drain or on the startup drain.
func (n *OutboxNotifier) DrainExcept(ctx context.Context, ownerSessionID string, except Event) error {
	return n.drain(ctx, ownerSessionID, except)
}

func (n *OutboxNotifier) drain(ctx context.Context, ownerSessionID string, except Event) error {
	entries, err := n.manager.Outbox(ctx)
	if err != nil {
		return err
	}
	var acked []string
	for _, e := range entries {
		t, err := e.Task()
		if err != nil {
			return err
		}
		if ownerSessionID != "" && t.OwnerSessionID != ownerSessionID {
			continue
		}
		if except.Task != nil && e.TaskID == except.Task.ID &&
			e.RunGeneration == except.Task.RunGeneration &&
			e.EventType == except.Type {
			continue
		}
		// Hidden system-owned (agentic_fetch) facts have no live
		// consumer: every public stream gates them. The replay is a
		// wake-up hint for those consumers, so it is skipped while
		// the row is still acked, keeping the pending set bounded
		// and the durable record intact for resync and diagnostics.
		if t.IsHidden() {
			acked = append(acked, e.ID)
			continue
		}
		n.events.PublishMustDeliver(ctx, pubsub.UpdatedEvent, Event{Type: e.EventType, Task: t})
		acked = append(acked, e.ID)
	}
	return n.manager.AckOutbox(ctx, acked...)
}
