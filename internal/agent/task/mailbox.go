package task

import "time"

// MessageOrigin names who appended a mailbox message. Trusted
// context derives it: a call_agent admission or an agent-parent
// message is OriginParent, the user-facing route is OriginUser.
type MessageOrigin string

const (
	OriginParent MessageOrigin = "parent"
	OriginUser   MessageOrigin = "user"
)

// MessageState is the mailbox delivery state. Rows start queued;
// dispatch claims the lowest queued sequence to delivered; a pending
// attempt cancelled before its message was ever delivered rejects
// that message with a stable reason.
type MessageState string

const (
	MessageQueued    MessageState = "queued"
	MessageDelivered MessageState = "delivered"
	MessageRejected  MessageState = "rejected"
)

// Stable mailbox rejection codes. Rejection reasons are machine
// codes, never prose.
const (
	// ReasonTaskCancelled rejects the undelivered sequence-zero
	// message of a pending attempt that was cancelled before it
	// ever ran.
	ReasonTaskCancelled = "task_cancelled"
)

// ChildMessage is one durable mailbox row on a child session. The
// dispatch repository allocates Sequence transactionally per child
// session (the initial call_agent prompt is sequence zero) and
// stamps DeliveredAt when an attempt claims the row.
type ChildMessage struct {
	ID             string        `json:"id"`
	ChildSessionID string        `json:"child_session_id"`
	TaskID         string        `json:"task_id"`
	OwnerSessionID string        `json:"owner_session_id"`
	Sequence       uint64        `json:"sequence"`
	Origin         MessageOrigin `json:"origin"`
	Prompt         string        `json:"prompt"`
	// Attachments is JSON array text of the wire attachment
	// objects; "[]" when empty.
	Attachments string       `json:"attachments"`
	State       MessageState `json:"state"`
	// Reason is the stable rejection code, empty unless State is
	// MessageRejected.
	Reason      string    `json:"reason,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	DeliveredAt time.Time `json:"delivered_at,omitempty"`
}

func (m *ChildMessage) clone() *ChildMessage {
	c := *m
	return &c
}

// ChildSpec describes the child session an admission binds. For a
// fresh delegation New is true and the dispatch transaction inserts
// the sessions row itself; a continuation reuses the retained child
// session, so ID is taken from the predecessor by the repository and
// the spec's ID is ignored.
type ChildSpec struct {
	ID       string
	Title    string
	ParentID string
	New      bool
}

// Admission is the input of CreatePendingTask: a fully populated
// pending record plus its child-session binding. The repository
// allocates the sequence-zero (or next) mailbox row for Task.Prompt
// inside the same transaction, validates any continuation lineage,
// and fills Task.ChildSessionID and Task.RunGeneration for
// continuations.
type Admission struct {
	Task  *Task
	Child ChildSpec
}
