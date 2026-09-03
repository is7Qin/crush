package task

import "errors"

// Sentinel errors for the task core. Callers match with errors.Is.
var (
	// ErrNotFound names an unknown task id.
	ErrNotFound = errors.New("task not found")
	// ErrNotOwner rejects control operations whose trusted caller
	// session is not the task's immutable owner.
	ErrNotOwner = errors.New("caller session does not own the task")
	// ErrDelegation rejects task starts from child depth >= 1.
	ErrDelegation = errors.New("child sessions cannot delegate tasks")
	// ErrQuota rejects a start when live-task quota is exhausted.
	ErrQuota = errors.New("live task quota exhausted")
	// ErrResumeLive rejects resuming a task that is not terminal.
	ErrResumeLive = errors.New("cannot resume a task that is not terminal")
	// ErrInvalidTransition rejects a disallowed status transition.
	ErrInvalidTransition = errors.New("invalid task status transition")
	// ErrShuttingDown rejects new starts after Shutdown began.
	ErrShuttingDown = errors.New("task manager is shutting down")
	// ErrInvalidRequest reports a malformed start request.
	ErrInvalidRequest = errors.New("invalid task request")
	// ErrStepLimit is the stable terminal reason for a child run that
	// reached its profile's max_steps.
	ErrStepLimit = errors.New("task_step_limit")
	// ErrTimeout is the stable terminal reason for a child run that
	// exceeded its profile's max_duration. Cancellation still wins:
	// the manager terminalizes a canceled attempt as cancelled even
	// when the runner reports a timeout.
	ErrTimeout = errors.New("task_timeout")
	// ErrRunFenced rejects a tool admission from a late runner whose
	// attempt has already been fenced for terminalization.
	ErrRunFenced = errors.New("task run fenced: tool admission closed")
)
