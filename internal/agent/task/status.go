// Package task implements the durable agent task core: task records,
// lifecycle state transitions, capacity queueing, cancellation, and
// owner-session authorization. The manager is independent of providers
// and the coordinator; MemoryStore keeps the core usable without a
// database and SQLiteStore persists records in the shared SQLite
// database.
package task

import "slices"

// Status is the lifecycle state of an agent task.
type Status string

const (
	StatusPending         Status = "pending"
	StatusRunning         Status = "running"
	StatusWaitingForInput Status = "waiting_for_input"
	StatusCompleted       Status = "completed"
	StatusFailed          Status = "failed"
	StatusCancelled       Status = "cancelled"
	StatusInterrupted     Status = "interrupted"
)

// Terminal reports whether s is a terminal state. Terminal states are
// immutable: no transition may leave one.
func (s Status) Terminal() bool {
	switch s {
	case StatusCompleted, StatusFailed, StatusCancelled, StatusInterrupted:
		return true
	default:
		return false
	}
}

// Live reports whether s still occupies live-task quota. Tasks waiting
// for input are live but do not occupy a running-model slot.
func (s Status) Live() bool { return !s.Terminal() }

// allowedTransitions mirrors the state machine in
// docs/specs/crush-agents/04-background-lifecycle.md.
var allowedTransitions = map[Status][]Status{
	StatusPending:         {StatusRunning, StatusCancelled, StatusFailed, StatusInterrupted},
	StatusRunning:         {StatusWaitingForInput, StatusCompleted, StatusFailed, StatusCancelled, StatusInterrupted},
	StatusWaitingForInput: {StatusRunning, StatusFailed, StatusCancelled, StatusInterrupted},
}

// CanTransitionTo reports whether from -> to is an allowed transition.
// Transitions out of terminal states are never allowed.
func (s Status) CanTransitionTo(to Status) bool {
	return slices.Contains(allowedTransitions[s], to)
}
