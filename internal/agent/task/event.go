package task

import "sync"

// EventType names a task lifecycle fact. Events are facts, not
// commands: consumers that need full output query task state.
type EventType string

const (
	EventCreated         EventType = "created"
	EventStarted         EventType = "started"
	EventWaitingForInput EventType = "waiting_for_input"
	EventResumed         EventType = "resumed"
	EventCompleted       EventType = "completed"
	EventFailed          EventType = "failed"
	EventCancelled       EventType = "cancelled"
	EventInterrupted     EventType = "interrupted"
)

// Event carries a snapshot copy of the task at the moment of the
// transition.
type Event struct {
	Type EventType
	Task *Task
}

// eventForTransition maps a completed transition to its event type.
// Entry into running is Started from pending and Resumed from
// waiting_for_input.
func eventForTransition(t *Task, from Status) Event {
	if t.Status == StatusRunning {
		if from == StatusWaitingForInput {
			return Event{Type: EventResumed, Task: t}
		}
		return Event{Type: EventStarted, Task: t}
	}
	return Event{Type: EventType(t.Status), Task: t}
}

// broker is a minimal synchronous callback fan-out. Callbacks run
// outside the manager lock and must not block; later stages may bridge
// them to pub/sub.
type broker struct {
	mu     sync.Mutex
	subs   map[uint64]func(Event)
	nextID uint64
}

func newBroker() *broker {
	return &broker{subs: map[uint64]func(Event){}}
}

func (b *broker) subscribe(fn func(Event)) func() {
	b.mu.Lock()
	id := b.nextID
	b.nextID++
	b.subs[id] = fn
	b.mu.Unlock()
	return func() {
		b.mu.Lock()
		delete(b.subs, id)
		b.mu.Unlock()
	}
}

func (b *broker) publish(events ...Event) {
	if len(events) == 0 {
		return
	}
	b.mu.Lock()
	fns := make([]func(Event), 0, len(b.subs))
	for _, fn := range b.subs {
		fns = append(fns, fn)
	}
	b.mu.Unlock()
	for _, ev := range events {
		for _, fn := range fns {
			fn(ev)
		}
	}
}
