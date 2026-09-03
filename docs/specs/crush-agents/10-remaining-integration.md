# Crush Agents: Remaining Integration Contract

This document completes the implementation contract for the agent profile and
task work already present in this branch. It is self-contained and authoritative
for the remaining integration behavior. References to `00` through `09` are
context only; when this document gives a concrete field, state transition,
error, or acceptance rule, this document wins. It does not reopen the settled
decisions that the public delegation tool is `call_agent`, child depth is
exactly one, and child sessions retain ordinary capabilities subject to profile
policy. All child execution uses one asynchronous task model; there is no
foreground/background execution distinction.

The current branch has the profile resolver, profile-aware child construction,
the task manager, SQLite task and outbox storage, task control tools, and the
in-process task-question suspend/resume path. The work below is required before
the feature can be considered complete.

## Completion Boundary

The implementation is complete only when all of these paths are real, not just
represented by an in-memory package or an unused accessor:

```text
config profile policy
  -> runtime child limits and tool admission

call_agent
  -> one transaction for task/session binding
  -> task manager and execution fence
  -> terminal task + outbox + parent inbox

child question
  -> durable question row
  -> task waiting_for_input
  -> existing workspace/client answer path
  -> owner-authorized resume

task event
  -> app broker
  -> server envelope/SSE
  -> client decode
  -> workspace/UI correlation by task id + tool call id

primary and child mutating tool
  -> hook decision
  -> task-run shared admission
  -> workspace FIFO write lease when exclusive
  -> inner tool
```

The following are explicitly not acceptable completion substitutes:

- constructing `OutboxNotifier` without a production drain or resync caller;
- publishing a task event type that `server.wrapEvent` drops;
- persisting a question that no production endpoint or UI can answer;
- accepting a profile field that has no effect on the child runtime;
- creating a child session before task creation and leaving it behind when
  task admission fails;
- adding a lock only to `write.go` while `bash`, MCP, rename, and multiedit
  can still mutate the same workspace concurrently.

## Preserved Invariants

These invariants remain authoritative during the remaining work:

1. Model input never supplies caller session, parent session, child session,
   task id, run generation, workspace id, or authorization identity.
2. A task with trusted caller depth greater than zero is rejected before quota
   reservation and child creation. The child effective tool list contains no
   `call_agent`, task-control tool, or `agentic_fetch`.
3. `call_agent` is the only public user-created delegation path. The old
   `agent` name may be recognized only at historical permission/message
   boundaries; it must not be a second live implementation.
4. Every accepted child task is asynchronous and its lifetime is bound to the
   workspace context, not the initiating request. The initiating call always
   returns after durable task admission; a parent or user may cancel the task
   explicitly, but request cancellation alone does not cancel an accepted task.
5. Task terminalization is conditional and exactly once. Capacity, cost
   aggregation, outbox insertion, inbox insertion, and terminal events are
   owned by the winning terminal transition.
6. Task-question resolution is conditional on question id and run generation.
   The first answer, cancellation, timeout, or shutdown outcome wins.
7. A task waiting for input consumes live-task quota but not running-model
   capacity. Resume reacquires capacity through the same FIFO dispatcher.
8. Task result text is bounded to 32 KiB at a valid UTF-8 boundary. The
   bounded record states whether the full output is available through
   `agent_output`.
9. Prompt text, result text, credentials, and file contents are not logged by
   default. Task events carry metadata and correlation ids, not full output.
10. All public task, question, and inbox operations verify the trusted owner
     session. A deleted owner cannot use retained ids to access or control old
     task data.

## Unified Asynchronous Child Conversation

There is one child execution model. `call_agent` always creates a durable task
attempt, persists the initial child-session binding, and returns an acceptance
acknowledgement before the child reaches a terminal state. The request has no
`run_in_background` field, and there is no synchronous result-returning mode.

The public request shape is:

```go
type AgentCallRequest struct {
    Profile string `json:"profile"`
    Prompt string `json:"prompt"`
    Model string `json:"model,omitempty"`
    TaskID string `json:"task_id,omitempty"`
}
```

`TaskID` is used only for an explicit continuation. It must belong to the
trusted caller's parent session and name a terminal attempt. A continuation
reuses the retained child session, creates a new task attempt with
`run_generation + 1`, and re-resolves the current profile/model policy. It
never replays an interrupted provider request automatically. The response is:

```go
type AgentCallAccepted struct {
    TaskID string `json:"task_id"`
    ChildSessionID string `json:"child_session_id,omitempty"`
    Profile string `json:"profile"`
    Provider string `json:"resolved_provider"`
    Model string `json:"resolved_model"`
    Status string `json:"status"`
}
```

The task is a conversation address as well as a unit of execution. Its child
session is retained after a turn completes. Communication has three explicit
directions:

```text
main agent -> child: agent_message(task_id, prompt)
user       -> child: POST /v1/workspaces/{id}/tasks/{tid}/messages
child      -> main: durable task event + parent inbox terminal result
child question -> durable question row + owner-authorized task-question transport
```

`agent_message` is a primary-only task control tool that shares the same
backend mailbox operation as the user route. It is not available to children.
Its exact shape is:

```go
type AgentMessageRequest struct {
    TaskID string `json:"task_id"`
    Prompt string `json:"prompt"`
    Attachments []Attachment `json:"attachments,omitempty"`
}
type AgentMessageAccepted struct {
    TaskID string `json:"task_id"`
    ChildSessionID string `json:"child_session_id"`
    Sequence uint64 `json:"sequence"`
    AttemptTaskID string `json:"attempt_task_id,omitempty"`
    Status string `json:"status"`
}
```

`Attachment` is the existing wire object with `file_path`, `file_name`,
`mime_type`, and `content` fields. The tool input contains only a task lookup
id, prompt text, and optional attachments; identity is derived from trusted
main-session context. It returns `AgentMessageAccepted` after the mailbox row
commits.

### Child mailbox contract

Messages are canonicalized to `task_id`. The HTTP route uses `{tid}` as the
task id; `child_session_id` is never accepted from a user or model and is
returned only as response metadata.
The authenticated user client must be attached to the task's immutable owner
parent session. A parent agent uses that same owner identity. Foreign,
deleted-owner, unknown, or hidden `agentic_fetch` addresses are rejected.

The durable child mailbox is stored in `agent_task_messages`:

```sql
agent_task_messages (
    id TEXT PRIMARY KEY,
    child_session_id TEXT NOT NULL,
    task_id TEXT NOT NULL,
    owner_session_id TEXT NOT NULL,
    sequence INTEGER NOT NULL,
    origin TEXT NOT NULL,
    prompt TEXT NOT NULL,
    attachments TEXT NOT NULL DEFAULT '[]',
    state TEXT NOT NULL,
    reason TEXT NULL,
    created_at INTEGER NOT NULL,
    delivered_at INTEGER NULL,
    UNIQUE(child_session_id, sequence)
)
```

`origin` is `parent` or `user`; `state` is `queued`, `delivered`, or
`rejected`. The sequence is allocated transactionally per child session.
Delivery claims the lowest queued sequence and records `delivered_at` in the
same transaction that creates the attempt. Restart recovery leaves undelivered
rows queued. The dispatch repository exposes:

```text
AppendChildMessage(ctx, ChildMessage) (ChildMessage, error)
DispatchNextChildMessage(ctx, childSessionID) (TaskRecord, bool, error)
```

`ChildMessage` contains `id`, `child_session_id`, `task_id`,
`owner_session_id`, `sequence`, `origin`, `prompt`, `attachments`, `state`,
`reason`, `created_at`, and nullable `delivered_at`. `origin` is `parent` or
`user`; `state` is `queued`, `delivered`, or `rejected`; `reason` is nullable
and contains a stable rejection code. `attachments` is JSON text containing
objects with `file_path`, `file_name`, `mime_type`, and base64 `content`.
`DispatchNextChildMessage` uses one SQLite transaction to claim the lowest
queued sequence, mark it delivered, and either transition the bound pending
attempt to running or create its successor attempt. It records the claimed
message id on that attempt. No separate claim or delivery-mark operation is
available to callers.

Messages may be appended to tasks in `pending`, `running`,
`waiting_for_input`, or terminal states. Pending-task messages remain queued
until the initial attempt is dispatched. A message never answers a typed
question and never resumes a waiting attempt.

The initial `call_agent` prompt is sequence zero. User and parent messages use
the same sequence and are delivered FIFO. A child provider request or mutating
tool invocation is never interrupted by an incoming message. A message
received while a task is running waits for the next child turn boundary. A
message received while a task is waiting for a typed question is queued but
does not answer or resume that question.

When a child turn reaches a terminal state, the next queued message starts a
new task attempt against the same child session. The new attempt records the
previous task id in `resumes_task_id` and increments the child session's task
run generation. No two attempts for one child session execute concurrently.
Message delivery, typed-question resume, cancellation, and terminal fencing
are serialized by the task manager.

For a terminal task, the manager atomically claims the lowest queued message
for that child session and creates the next attempt. Multiple messages create
successive attempts in sequence order; only the first message is claimed by
the next attempt. Each attempt has at most one input message. For a pending,
running, or waiting task, `AttemptTaskID` in the acceptance response is empty.

Child responses are persisted in the child session and emitted as task events.
Terminal responses also create exactly one parent inbox item. The parent inbox
is an untrusted evidence channel and never starts a re-entrant parent run while
the parent is busy. The user can observe the same child conversation through
the child session and task event stream.

`agent_cancel` is the normal explicit cancellation path. Cancellation of the
HTTP request or parent turn that created a task does not cancel the accepted
child. Workspace shutdown cancels all child tasks.

### Direct-message wire contract

Add this existing-controller route:

```text
POST /v1/workspaces/{id}/tasks/{tid}/messages?client_id=<attached-client>
```

with exact request/response shapes:

```go
type ChildMessageRequest struct {
    Prompt string `json:"prompt"`
    Attachments []Attachment `json:"attachments,omitempty"`
}
type ChildMessageAccepted struct {
    TaskID string `json:"task_id"`
    ChildSessionID string `json:"child_session_id"`
    Sequence uint64 `json:"sequence"`
    AttemptTaskID string `json:"attempt_task_id"`
    Status string `json:"status"`
}
```

`ChildMessageRequest.Attachments` uses the existing attachment fields:
`file_path`, `file_name`, `mime_type`, and `content`.

Empty prompts are HTTP 400/`empty_prompt`. Missing or malformed client ids are
HTTP 401/`client_required`; retired or unattached clients are
HTTP 403/`client_unattached`; clients without a current session are
HTTP 409/`client_session_required`; foreign task ownership is
HTTP 403/`task_owner_forbidden`; a deleted task owner is HTTP
410/`task_owner_deleted`; unknown tasks and hidden internal tasks are HTTP
404/`task_not_found`; and accepted messages are HTTP 202. The body has no
owner, parent, child, workspace, or generation authority fields.

The primary-only `agent_cancel` tool has this shape:

```go
type AgentCancelRequest struct {
    TaskID string `json:"task_id"`
}
type AgentCancelAccepted struct {
    TaskID string `json:"task_id"`
    Status string `json:"status"`
}
```

It validates owner and task id, then conditionally cancels the current
non-terminal attempt. If that attempt is pending, its sequence-zero mailbox
message is marked `rejected` with reason `task_cancelled`; a running or waiting
attempt already has a delivered message. Messages with higher queued sequences
are unchanged and dispatch as successor attempts after the cancelled attempt
terminalizes. Cancellation of a running attempt
fences future tool admission, cancels the runner context, and terminalizes
exactly once. Cancellation of a waiting attempt resolves its pending question
as cancelled and terminalizes the attempt in the same transaction. Cancelling
a terminal task returns HTTP 409/`task_already_terminal` and creates no new
attempt.

## 1. Profile Runtime Policy

### Resolved profile

`ResolvedProfile` must carry the validated effective values for:

```text
name
description
model / fallback models
system prompt / prompt file
allowed ordinary tools and MCP tools
can_delegate
can_ask_questions
max_steps
max_duration
profile generation
```

Its concrete runtime shape is:

```go
type ResolvedProfile struct {
    Name             string
    Description      string
    Model            SelectedModel
    FallbackModels   []SelectedModel
    AllowedTools     []string
    AllowedMCP       map[string][]string
    CanDelegate      bool
    CanAskQuestions  bool
    MaxSteps         int
    MaxDuration      time.Duration
    Generation       uint64
}
```

The resolver precedence is, per scalar field, same-scope project profile,
same-scope global profile, built-in profile default. An explicitly present
empty list replaces the inherited list; an omitted field inherits it. `model`
is an exact selected model, while `models` is an ordered fallback list. The
resolver selects request model, then profile fallback entries, then parent
resolved model, then global large model. A request model that is unavailable
fails without fallback. A profile snapshot is immutable and its `Generation`
increments once per committed config reload.

The merge layer may use presence-aware fields, but the runtime must receive
concrete effective values. A field that is accepted by JSON/crushrc parsing and
validation must either affect runtime behavior or be removed from the public
profile schema.

### Runtime behavior

- `can_delegate=false` removes `call_agent` from a primary profile's effective
  tool set. At child depth, delegation is always denied regardless of this
  value.
- `can_ask_questions=false` removes `question` from the profile's effective
  tool set. This check applies to the task-aware child question adapter too;
  transport availability must never grant a tool excluded by the profile.
- `max_steps > 0` is passed to the child `SessionAgent` and limits the
  assistant/tool loop at the existing step boundary. Reaching the limit ends
  the task as `failed` with stable reason `task_step_limit`.
- `max_duration > 0` derives a child deadline from the task manager context.
  Expiry ends the task as `failed` with stable reason `task_timeout`, unless
  cancellation already won, in which case the task is `cancelled`.
- The recorded task metadata includes the profile generation and the resolved
  values used by that attempt. Config reload affects new tasks only.

The implementation must extend the existing `SessionAgentCall` or its current
loop seam minimally. It must not duplicate the agent loop in the coordinator.

### Acceptance tests

- A profile excluding `question` has no task-aware question tool.
- A profile with `max_steps=1` cannot execute a second assistant step.
- A profile with a short `max_duration` becomes `failed/task_timeout`.
- A cancellation racing a duration timeout produces exactly one terminal state.
- Config reload changes the generation used by a new task but not an active
  task.

## 2. Atomic Task and Child Session Creation

### Repository contract

The task dispatch repository owns one SQLite transaction over the existing DB
connection. `session.Service.CreateTaskSession` must not be used for a task
dispatch because it publishes a session event before the task binding commits.

The `task` package owns a narrow dispatch repository interface. Its SQLite
implementation receives the shared `*sql.DB`; callers never receive a raw
`*sql.Tx` and never issue a second write outside the repository transaction.
The interface is:

```text
CreatePendingTask(ctx, TaskRecord) (TaskRecord, error)
DispatchTask(ctx, DispatchRequest) (TaskRecord, error)
TerminalizeAndDeliver(ctx, Terminalization) (TaskRecord, won, error)
RecoverLiveTasks(ctx, reason) error
ListTasks(ctx, ownerSessionID, parentSessionID) ([]TaskRecord, error)
ListOutbox(ctx, ownerSessionID) ([]OutboxEntry, error)
ListInbox(ctx, ownerSessionID) ([]InboxEntry, error)
AckOutbox(ctx, ownerSessionID, ids) error
AckInbox(ctx, ownerSessionID, ids) error
```

`TaskRecord` contains task id, owner session id, parent session id, child
session id, parent message id, tool call id, profile, profile generation,
requested and resolved model, fallback models, prompt/tool fingerprints, run
generation, terminal generation, status, timestamps, bounded result metadata,
and usage. The corresponding SQLite columns use these exact types and
nullability: `id TEXT PRIMARY KEY`, `owner_session_id TEXT NOT NULL`,
`parent_session_id TEXT NULL after parent deletion`, `child_session_id TEXT
NOT NULL`, `parent_message_id TEXT NOT NULL`, `tool_call_id TEXT NOT
NULL`, `profile TEXT NOT NULL`, `profile_generation INTEGER NOT NULL`,
`requested_model TEXT NULL`, `resolved_provider TEXT NOT NULL`,
`resolved_model TEXT NOT NULL`, `fallback_models TEXT NOT NULL DEFAULT '[]'`,
`prompt_fingerprint TEXT NOT NULL`, `tool_fingerprint TEXT NOT NULL`,
`run_generation INTEGER NOT NULL`, `resumes_task_id TEXT NULL`,
`message_id TEXT NULL`, `terminal_generation INTEGER NULL`,
`cost_aggregated_generation INTEGER NULL`, `status TEXT NOT NULL`,
`prompt TEXT NOT NULL`, `result TEXT NOT NULL DEFAULT ''`, `summary TEXT NOT
NULL DEFAULT ''`, `error TEXT NOT NULL DEFAULT ''`,
`result_truncated INTEGER NOT NULL DEFAULT 0`, `created_at INTEGER NOT NULL`,
`started_at INTEGER NULL`, `completed_at INTEGER NULL`,
`updated_at INTEGER NOT NULL`, `prompt_tokens INTEGER NOT NULL DEFAULT 0`,
`completion_tokens INTEGER NOT NULL DEFAULT 0`, and `cost REAL NOT NULL
DEFAULT 0`. Timestamps are Unix nanoseconds; boolean values use SQLite integer
0/1; encoded arrays use JSON text.

The task table also contains `resumes_task_id TEXT NULL` and
`message_id TEXT NULL`. `resumes_task_id` references the immediately preceding
terminal attempt in the same child session. `message_id` identifies the
mailbox message that caused the attempt, when applicable. The child-session
repository maintains one current-attempt pointer. A continuation or queued
message dispatch validates that the predecessor is terminal, sets
`resumes_task_id`, sets `run_generation` to the predecessor generation plus
one, and preserves the unique `(child_session_id, run_generation)` pair.

The exact Go request shapes are:

```go
type TaskRecord struct {
    ID, OwnerSessionID string
    ParentSessionID *string
    ChildSessionID string
    ParentMessageID, ToolCallID, Profile string
    RequestedModel *string
    ResolvedProvider, ResolvedModel, PromptFingerprint, ToolFingerprint string
    FallbackModels []string
    ResumesTaskID, MessageID *string
    ProfileGeneration, RunGeneration uint64
    TerminalGeneration, CostAggregatedGeneration *uint64
    Status, Prompt, Result, Summary, Error string
    ResultTruncated bool
    CreatedAt, UpdatedAt time.Time
    StartedAt, CompletedAt *time.Time
    PromptTokens, CompletionTokens int64
    Cost float64
}
type DispatchRequest struct {
    TaskID, OwnerSessionID, ParentSessionID string
    ParentMessageID, ToolCallID, ChildTitle string
    ChildSessionID string
    RunGeneration uint64
}

The child session insert uses exactly these fields and defaults:
`id TEXT PRIMARY KEY`, `parent_session_id TEXT NULL`, `title TEXT NOT NULL`,
`message_count INTEGER NOT NULL DEFAULT 0`, `prompt_tokens INTEGER NOT NULL
DEFAULT 0`, `completion_tokens INTEGER NOT NULL DEFAULT 0`, and `cost REAL
NOT NULL DEFAULT 0`. The repository generates `ChildSessionID` during dispatch;
model input never supplies it.

type UsageDelta struct {
    PromptTokens, CompletionTokens int64
    Cost float64
}
type Terminalization struct {
    TaskID string
    RunGeneration, TerminalGeneration uint64
    Status, Reason, Result, Summary string
    ResultTruncated bool
    Usage UsageDelta
    OutboxPayload, InboxPayload []byte
}
type OutboxEntry struct {
    ID, TaskID, EventType, Payload string
    RunGeneration uint64
    DeliveredAt, CreatedAt *time.Time
}
type InboxEntry struct {
    ID, OwnerSessionID, TaskID, Payload string
    TerminalGeneration uint64
    DeliveredAt, CreatedAt *time.Time
}
```

`ParentSessionID`, `StartedAt`, `CompletedAt`,
`TerminalGeneration`, and `CostAggregatedGeneration` are nullable only where
the SQLite column is nullable. `DispatchRequest.ChildSessionID` is populated by
the admission transaction before a pending task is returned.
`Terminalization.Status` is one of `completed`, `failed`, `cancelled`, or
`interrupted`; `Reason` is the stable machine code for failures. `OutboxPayload`
and `InboxPayload` are already-encoded JSON bytes produced by the task layer.

`UsageDelta` contains `prompt_tokens`, `completion_tokens`, and `cost` numeric
fields to apply to the parent session. The parent update is conditional on
`cost_aggregated_generation IS NULL OR cost_aggregated_generation !=
run_generation`, then sets `cost_aggregated_generation = run_generation` in the
same transaction.

The parent cost update is guarded by
`(task_id, run_generation, cost_aggregated_generation)`. A successful first
terminalization increases parent cost by exactly the supplied usage delta;
repeated terminalization leaves parent cost unchanged. The transaction test
records the numeric parent cost before and after the first commit and after a
repeat, and asserts those exact values.

The repository implementation performs task SQL,
session SQL, outbox SQL, inbox SQL, and parent usage SQL on the same
transaction handle. Session event publication remains outside the transaction.

Pending admission creates and binds the child session in the same SQLite
transaction as the task row, inserts the initial `call_agent` prompt as mailbox
sequence zero with state `queued`, and emits the created event after commit.
When a slot is available, dispatch changes the bound pending task to `running`,
claims sequence zero, and writes the start outbox event. Only after commit are
session/task notifications published and the child goroutine launched.

If child-session creation or binding fails, the admission transaction rolls back
the task, child session, and mailbox row. The manager then records one terminal
failed task with reason `task_session_creation_failed` in a separate durable
failure transaction; no child session or mailbox row is visible, no runner is
launched, and quota/capacity are released exactly once. A committed pending
task always has its child session and sequence-zero mailbox row.

`call_agent` must stop creating the child session before calling task manager
admission. The manager/dispatch repository owns the binding.

The manager reserves live quota before `CreatePendingTask`, but it releases
that reservation if the repository returns an error. A committed pending task
always has its child session and sequence-zero mailbox row; a failed admission
transaction leaves neither visible.

### Transaction failure proof

The SQLite integration test creates a temporary trigger that rejects the inbox
insert for one terminalization. The test then asserts all of the following:

```text
task status is still live
task result/error is unchanged
parent cost is unchanged
no terminal outbox row exists
no inbox row exists
manager live quota is still reserved
```

After the trigger is removed, the same terminalization commits all rows and
releases quota once. A second terminalization is a `won=false` no-op.

### Acceptance tests

- A queued task has a bound child session and a queued sequence-zero mailbox row.
- A failed session insert leaves no visible child session and one failed task.
- A shutdown or persistence failure does not leave live quota reserved.
- Task and child session rows are both visible after a committed dispatch.
- The task start event cannot be observed before its task/session transaction
  commits.

## 3. Durable Terminal Delivery

### Schema

Add the durable `agent_task_inbox` table from `05-persistence-events.md` with
the unique key `(owner_session_id, task_id, terminal_generation)`. Extend task
metadata as needed to retain `parent_message_id`, `tool_call_id`, resolved
profile/model metadata, run generation, and terminal generation.

The exact delivery columns are:

```sql
agent_task_outbox (
    id TEXT PRIMARY KEY,
    task_id TEXT NOT NULL,
    run_generation INTEGER NOT NULL,
    event_type TEXT NOT NULL,
    payload TEXT NOT NULL,
    delivered_at INTEGER NULL,
    created_at INTEGER NOT NULL,
    UNIQUE(task_id, run_generation, event_type)
)

agent_task_inbox (
    id TEXT PRIMARY KEY,
    owner_session_id TEXT NOT NULL,
    task_id TEXT NOT NULL,
    terminal_generation INTEGER NOT NULL,
    payload TEXT NOT NULL,
    delivered_at INTEGER NULL,
    created_at INTEGER NOT NULL,
    UNIQUE(owner_session_id, task_id, terminal_generation)
)
```

The migration adds indexes on `(parent_session_id, status)`,
`(child_session_id)`, `(status, updated_at)`, and `(delivered_at, created_at)`.
The `parent_session_id` foreign key uses `ON DELETE SET NULL`. The
`child_session_id` column remains `TEXT NOT NULL` and is retained for the
child-session lifetime; it must not use `ON DELETE SET NULL`. The immutable
`owner_session_id` remains non-FK and is never cleared.

The terminal transition transaction must:

1. conditionally fence `(task_id, run_generation, status not terminal)`;
2. update terminal task state and bounded result metadata;
3. aggregate child usage into the parent once for that generation;
4. insert one outbox row for the terminal event;
5. insert one inbox row for the parent result envelope;
6. commit before publishing wake-up events.

The parent cost update is observable and numeric: record the parent cost before
the transaction, assert that the first successful terminalization increases it
by exactly `Terminalization.UsageDelta.cost`, then repeat the same
terminalization and assert the cost is unchanged. The SQL guard is the task
run-generation key: update only when
`cost_aggregated_generation IS NULL OR cost_aggregated_generation !=
run_generation`, and set `cost_aggregated_generation = run_generation` with the
cost increment.

The outbox and inbox rows use conflict-ignore or equivalent idempotence on
their documented unique keys. A database error rolls back the entire terminal
transaction; in-memory manager bookkeeping must not be released as though a
durable terminal transition succeeded.

The SQLite store must not perform a successful conditional update followed by a
required read that can make a committed terminal task look like a failed
transition. The write path returns the updated record or otherwise preserves
the winning transition result.

### Parent inbox drain

Add a workspace-scoped inbox service with these rules:

- drain is serialized per parent session;
- a parent must exist and be idle before delivery;
- a busy parent keeps the inbox item pending;
- a closed/deleted parent does not receive a re-entrant run; the inbox row
  remains retained and the task/output remains queryable through authorized
  resync;
- the internal message part is marked untrusted and includes task id, child
  session id, profile, status, summary, and bounded text or an output marker;
- `delivered_at` is written only after the internal message commit succeeds.

The model-facing serialization must clearly delimit the child result as
untrusted evidence. It cannot become a system instruction, user request, tool
authorization, or profile override.

### Resync contract

Expose typed workspace-level reads for:

```text
task snapshots by owner / parent
undelivered outbox rows by owner workspace
undelivered inbox rows by owner session
unresolved task questions by owner session
```

Reconnect must be able to recover after all live pub/sub/SSE events are dropped.
Live events are wake-up hints only.

### Acceptance tests

- Terminal task, outbox row, inbox row, and one cost aggregation commit
  atomically.
- Repeating terminal completion creates no second outbox/inbox/cost update.
- Dropping the event broker still permits task and inbox resync from SQLite.
- A busy parent does not receive a re-entrant `Run`.
- A deleted parent retains task and inbox data but cannot access it publicly.
- Full result retrieval remains available through owner-authorized
  `agent_output`.

## 4. Task-Question Production Transport

### Domain

Keep `internal/agent/taskquestion` as a sibling of the global
`internal/question` service. Its durable record has this exact shape:

```go
type TaskQuestionRecord struct {
    QuestionID, TaskID, OwnerSessionID, ChildSessionID string
    RunGeneration uint64
    Batch question.Request
    Answers []question.Answer
    Resolution string
    CreatedAt time.Time
    ResolvedAt *time.Time
}
```

`Batch` is encoded as JSON in the `batch TEXT NOT NULL` column. `Answers` is
encoded as JSON in `answers TEXT NOT NULL DEFAULT ''`; an empty value means no
answers. `Resolution` is one of `pending`, `answered`, `cancelled`,
`timed_out`, or `interrupted`. The database enforces one pending row per
`task_id` with a partial unique index.

The public wire `TaskQuestion` is a separate transport projection. It uses
RFC3339Nano UTC strings for timestamps, a nullable `resolved_at`, and the
following exact nested JSON shapes:

```go
type QuestionRequest struct {
    ID string `json:"id"`
    SessionID string `json:"session_id"`
    ToolCallID string `json:"tool_call_id"`
    Questions []Question `json:"questions"`
    ConfirmTitle string `json:"confirm_title,omitempty"`
    ConfirmDescription string `json:"confirm_description,omitempty"`
}
type Question struct {
    ID string `json:"id"`
    Type string `json:"type"`
    Label string `json:"label,omitempty"`
    Text string `json:"question"`
    Description string `json:"description,omitempty"`
    Choices []Choice `json:"choices,omitempty"`
}
type Choice struct {
    ID string `json:"id"`
    Label string `json:"label"`
    Description string `json:"description,omitempty"`
}
type QuestionAnswer struct {
    QuestionID string `json:"question_id"`
    SelectedIDs []string `json:"selected_ids,omitempty"`
    FillInText string `json:"fill_in_text,omitempty"`
    Yes *bool `json:"yes,omitempty"`
    Notes map[string]string `json:"notes,omitempty"`
}
```

No raw SQL row, repository type, or in-memory waiter is exposed on the wire.

The task-aware adapter receives identity only from trusted task execution
context. The model supplies only the existing `QuestionParams` payload.

The child suspend/resume behavior is uniform for every task. A question can be
answered through the workspace control path whether the task was just accepted
or is continuing an existing child session. A question call with no task transport fails promptly
with `question_transport_unavailable`; it never blocks indefinitely.

The production service uses an App-owned `TaskQuestionLifecycle` bridge with
these exact methods:

```go
type TaskQuestionLifecycle interface {
    BeginWait(ctx context.Context, q TaskQuestionRecord) error
    Resolve(ctx context.Context, q TaskQuestionRecord, u ResolutionUpdate) error
    Resume(ctx context.Context, q TaskQuestionRecord) error
}
type ResolutionUpdate struct {
    QuestionID string
    TaskID string
    OwnerSessionID string
    ChildSessionID string
    RunGeneration uint64
    Resolution string
    Answers []QuestionAnswer
    ResolvedAt time.Time
}
```

`BeginWait` is one SQLite transaction that inserts the pending question row and
changes the matching task attempt from `running` to `waiting_for_input`.
`Resolve` is one conditional SQLite transaction that changes the question row
from `pending` to its terminal resolution and applies the matching task
transition. For an answered resolution, the task remains
`waiting_for_input` until `Resume` reacquires model capacity and conditionally
changes it to `running`. Cancellation changes the task to `cancelled`; timeout
changes the task to `failed` with reason `task_question_timeout`; interruption
changes it to `interrupted`. `Resolve` checks task id, owner session id, child
session id, and run generation. The service wakes the runner only after
`Resolve` commits; `Resume` is valid only for an answered question whose task
is still `waiting_for_input`. If the task transition fails, the transaction
rolls back both question and task changes, returns `task_persistence_failed`,
and does not wake the runner.

The in-memory test implementation may use the existing `Suspend`/`Resume`
callbacks, but production App wiring must use this lifecycle bridge so a crash
cannot leave a durable pending question and a running task out of sync.

The durable question state machine is:

```text
pending -> answered | cancelled | timed_out | interrupted
```

On process restart, every durable `pending` question is resolved as
`interrupted` in the same recovery transaction that marks its task
`interrupted`. No durable question is reattached to a new in-memory waiter.
Reconnect can still read the resolved record for audit, but cannot answer it.

### Answer and cancellation API

Add these owner-authorized operations at the existing workspace routes:

```text
POST /v1/workspaces/{id}/task-questions/answer
POST /v1/workspaces/{id}/task-questions/cancel
GET  /v1/workspaces/{id}/task-questions/pending
```

The wire request/response fields are:

```text
TaskQuestionAnswerRequest {
    question_id
    responses: QuestionResponse[]
}
TaskQuestionCancelRequest { question_id }
TaskQuestionListResponse  { questions[] }
QuestionResponse          { question_id, selected_ids, fill_in_text, yes, notes }
TaskQuestion              { question_id, task_id, owner_session_id,
                            child_session_id, run_generation, batch,
                            answers, resolution, created_at, resolved_at }
```

The equivalent Go wire types are:

```go
type TaskQuestionAnswerRequest struct {
    QuestionID string `json:"question_id"`
    Responses []QuestionResponse `json:"responses"`
}
type TaskQuestionCancelRequest struct {
    QuestionID string `json:"question_id"`
}
type TaskQuestionListResponse struct {
    Questions []TaskQuestion `json:"questions"`
}
type TaskQuestion struct {
    QuestionID string `json:"question_id"`
    TaskID string `json:"task_id"`
    OwnerSessionID string `json:"owner_session_id"`
    ChildSessionID string `json:"child_session_id"`
    RunGeneration uint64 `json:"run_generation"`
    Batch QuestionRequest `json:"batch"`
    Answers []QuestionAnswer `json:"answers,omitempty"`
    Resolution string `json:"resolution"`
    CreatedAt string `json:"created_at"`
    ResolvedAt *string `json:"resolved_at"`
}
type QuestionResponse struct {
    QuestionID string `json:"question_id"`
    SelectedIDs []string `json:"selected_ids,omitempty"`
    FillInText string `json:"fill_in_text,omitempty"`
    Yes *bool `json:"yes,omitempty"`
    Notes map[string]string `json:"notes,omitempty"`
}
```

The exact JSON tags are `question_id`, `task_id`, `owner_session_id`,
`child_session_id`, `run_generation`, `batch`, `answers`, `resolution`,
`created_at`, and `resolved_at`; `resolved_at` is nullable.
`QuestionResponse` uses `question_id`, `selected_ids`, `fill_in_text`, `yes`,
and `notes`.

Every task and task-question route requires the existing `client_id` query
parameter. The server validates that the client is attached to the workspace
and derives `caller_session_id` from that client's current-session binding. A
request body or URL may contain only task/question lookup ids and answer data;
it must not contain `owner_session_id`, `parent_session_id`,
`child_session_id`, or authority identity. Missing, retired, unattached, or
sessionless clients use these exact status/code pairs: HTTP 401/
`client_required` for missing or malformed ids, HTTP 403/`client_unattached`
for retired or unattached ids, and HTTP 409/`client_session_required` for a
client without a current session. The service verifies question owner, task
owner, and run generation before resolving.

Resolution order is conditional and exactly once:

```text
owner answer       -> answered -> resume task
owner cancellation -> cancelled -> cancel task
runner context     -> cancelled -> cancel task
timeout            -> timed_out -> failed task_question_timeout
shutdown           -> interrupted -> interrupted task
```

The task transitions to `waiting_for_input` before the question request is
published. It releases model capacity while retaining live quota. Answering
leaves the task waiting until `Resume` reacquires capacity and conditionally
changes the matching task attempt to `running` before the runner continues. A
second unresolved question for one task attempt is rejected.

### Event and reconnect behavior

Question request and resolution notification payloads extend the existing
question envelope with task id, question id, child session id, run generation,
and the existing answer schema. Both are durable-resyncable. The question
dialog uses the existing UI question flow; it does not create a competing
question model.

The answer route calls `TaskQuestionService.AnswerTask` with the client’s
derived current session identity. The cancel route calls `CancelTask`. The
pending route returns only questions owned by that identity. Foreign owner,
unknown id, resolved id, deleted owner, and malformed answer responses map to
HTTP 403/`task_owner_forbidden`, HTTP 404/`task_question_not_found`, HTTP
409/`task_question_already_resolved`, HTTP 410/`task_owner_deleted`, and HTTP
400/`invalid_task_question_answer`, respectively; none alters task state.

### Acceptance tests

- Child asks, owner answers, child resumes and completes without the initiating
  request remaining open.
- A task question becomes `waiting_for_input`, owner answers by question id,
  and the same task attempt resumes without a second child run.
- Foreign owner answer/cancel is rejected without changing task state.
- Answer/cancel/timeout/shutdown races resolve once by question id and run
  generation.
- Dropped SSE followed by reconnect lists the unresolved question and permits
  its owner to answer it.

## 5. Task Events Through Workspace Transport

### Domain event

Define one task-owned event payload with exactly these fields:

```text
event type
task id
parent session id
child session id
parent message id
tool call id
profile
resolved provider/model
status
summary
error/reason
run generation
at
```

The wire representation is exactly:

```go
type AgentTaskEvent struct {
    Type             string `json:"type"`
    TaskID           string `json:"task_id"`
    ParentSessionID  string `json:"parent_session_id"`
    ChildSessionID   string `json:"child_session_id,omitempty"`
    ParentMessageID  string `json:"parent_message_id"`
    ToolCallID       string `json:"tool_call_id"`
    Profile          string `json:"profile"`
    ResolvedProvider string `json:"resolved_provider"`
    ResolvedModel    string `json:"resolved_model"`
    Status           string `json:"status"`
    Summary          string `json:"summary,omitempty"`
    Error            string `json:"error,omitempty"`
    RunGeneration    uint64 `json:"run_generation"`
    At               string `json:"at"`
}
```

`Type` is one of `created`, `started`, `waiting_for_input`, `resumed`,
`completed`, `failed`, `cancelled`, or `interrupted`; `Status` is one of
`pending`, `running`, `waiting_for_input`, `completed`, `failed`, `cancelled`,
or `interrupted`; `At` is RFC3339Nano UTC. The JSON decoder rejects missing
`type`, `task_id`, `parent_session_id`, `tool_call_id`, `profile`, `status`,
or `at`.

Every `created`, `started`, `waiting_for_input`, `resumed`, `completed`,
`failed`, `cancelled`, and `interrupted` event carries `task_id` and
`tool_call_id`. The event is a fact; consumers query current state for full
output.

### Existing transport integration

Update only the existing integration boundaries:

```text
internal/pubsub       event payload discriminator if needed
internal/proto        task event/question wire structs and JSON tags
internal/server/events.go
                       wrap task/question events instead of dropping them
internal/client        decode task/question payloads
internal/workspace     translate and resync task/question state
internal/backend       owner-authorized answer/cancel/list operations
internal/ui/model      route task question into existing question dialog
internal/ui/chat       correlate task tool updates by task id + tool call id
```

No new protocol framework or parallel event bus is allowed. Unknown event
types remain droppable with a diagnostic, but all task event types defined by
this contract must round-trip.

Add these task routes to the existing workspace controller:

```text
GET  /v1/workspaces/{id}/tasks
GET  /v1/workspaces/{id}/tasks/{tid}
GET  /v1/workspaces/{id}/tasks/{tid}/output
POST /v1/workspaces/{id}/tasks/{tid}/cancel
POST /v1/workspaces/{id}/tasks/{tid}/messages
GET  /v1/workspaces/{id}/tasks/resync
GET  /v1/workspaces/{id}/task-questions/pending
POST /v1/workspaces/{id}/task-questions/answer
POST /v1/workspaces/{id}/task-questions/cancel
```

The task route wire shapes are exactly:

```go
type TaskSnapshot struct {
    ID              string `json:"id"`
    OwnerSessionID  string `json:"owner_session_id"`
    ParentSessionID string `json:"parent_session_id,omitempty"`
    ChildSessionID  string `json:"child_session_id,omitempty"`
    ParentMessageID string `json:"parent_message_id"`
    ToolCallID      string `json:"tool_call_id"`
    Profile         string `json:"profile"`
    Provider        string `json:"resolved_provider"`
    Model           string `json:"resolved_model"`
    RunGeneration   uint64 `json:"run_generation"`
    Status          string `json:"status"`
    Result          string `json:"result,omitempty"`
    Summary         string `json:"summary,omitempty"`
    Error           string `json:"error,omitempty"`
    Truncated       bool   `json:"truncated"`
    CreatedAt       string `json:"created_at"`
    StartedAt       string `json:"started_at,omitempty"`
    CompletedAt     string `json:"completed_at,omitempty"`
}
type TaskListResponse struct { Tasks []TaskSnapshot `json:"tasks"` }
type TaskOutputResponse struct {
    TaskID string `json:"task_id"`
    Status string `json:"status"`
    Result string `json:"result,omitempty"`
    Summary string `json:"summary,omitempty"`
    Error string `json:"error,omitempty"`
    Truncated bool `json:"truncated"`
}
type TaskResyncResponse struct {
    Tasks []TaskSnapshot `json:"tasks"`
    Outbox []OutboxEntry `json:"outbox"`
    Inbox []InboxEntry `json:"inbox"`
    Questions []TaskQuestion `json:"questions"`
}
type OutboxEntry struct {
    ID string `json:"id"`
    TaskID string `json:"task_id"`
    RunGeneration uint64 `json:"run_generation"`
    EventType string `json:"event_type"`
    Payload json.RawMessage `json:"payload"`
    DeliveredAt *string `json:"delivered_at"`
    CreatedAt string `json:"created_at"`
}
type InboxEntry struct {
    ID string `json:"id"`
    OwnerSessionID string `json:"owner_session_id"`
    TaskID string `json:"task_id"`
    TerminalGeneration uint64 `json:"terminal_generation"`
    Payload json.RawMessage `json:"payload"`
    DeliveredAt *string `json:"delivered_at"`
    CreatedAt string `json:"created_at"`
}
```

All timestamp strings are RFC3339Nano UTC. `GET /tasks` returns
`TaskListResponse`; `GET /tasks/{tid}` returns `TaskSnapshot`;
`GET /tasks/{tid}/output` returns `TaskOutputResponse`. A reconnect client
uses `GET /v1/workspaces/{id}/tasks/resync?client_id=...` for
`TaskResyncResponse`; this route is the durable recovery path and returns only
rows owned by the client's current session.

New route status mapping is exact: missing or malformed `client_id` is HTTP
401/`client_required`; retired or unattached client is
HTTP 403/`client_unattached`; sessionless client is
HTTP 409/`client_session_required`; unknown task or question is HTTP 404;
foreign owner is HTTP 403/`task_owner_forbidden`; already-resolved question is
HTTP 409/`task_question_already_resolved`; deleted owner is
HTTP 410/`task_owner_deleted`; malformed answer data is
HTTP 400/`invalid_task_question_answer`. Successful reads, task cancellation,
and task-question answer/cancel writes use HTTP 200. A successfully accepted
child message uses HTTP 202. The response error body uses a typed error
envelope with a stable `code` field.

The task list/status/output/cancel handlers use the authenticated current
session as owner. `parent_session_id` is an optional query filter only when it
equals that identity. Output responses contain `task_id`, `status`, `result`,
`summary`, `error`, and `truncated`; they do not expose raw store objects or
another owner's rows.

The server/SSE acceptance procedure is executable:

1. Start an `httptest` server using `server.NewServer` and one test backend
   workspace with a deterministic client id and owner session.
2. Open `GET /v1/workspaces/{id}/events` and capture the JSON envelope.
3. Publish one real task event from the App task broker. Assert the envelope
   discriminator and every task field, including `task_id` and `tool_call_id`.
4. Decode the same bytes through the client protocol decoder and assert the
   translated workspace message retains both correlation ids.
5. Close the stream before publishing a terminal event. Call
   `GET /v1/workspaces/{id}/tasks` and the task output route after reconnect
   and assert the terminal task/result is returned from SQLite.
6. Publish a real task-question request, answer it with the named answer route,
   and assert the child task receives `resumed` rather than a second task run.

The UI model test feeds the translated question request into `Update`, checks
that the existing question dialog is keyed by `question_id`, then feeds a
matching resolution notification and checks only that dialog is reconciled.
No prose or visual snapshot is required for these machine-correlation tests.

Task control tools remain primary-only. The remote task control endpoints and
question endpoints enforce the same owner checks as the in-process services.

### Acceptance tests

- The executable SSE procedure above passes for created, started, waiting,
  resumed, and every terminal event type.
- The client decoder and workspace translator retain task id and tool call id.
- The named question routes reach the existing dialog and answer the correct
  task with owner authorization.
- A resolution notification reconciles only its matching question id.
- Dropping all live events still permits task, inbox, and question recovery
  through the named GET routes.

## 6. Workspace Write Lease and Execution Fence

### Lease

Add `internal/agent/tools/exclusive.go` with a process-local lease registry
keyed by workspace identity. Use standard-library synchronization only.

The lease key is the canonical workspace path returned by the backend workspace
resolver. The package exposes only these operations and errors:

```go
type Lease interface {
    AcquireShared(ctx context.Context, key string) (Release, error)
    AcquireExclusive(ctx context.Context, key string) (Release, error)
}
type Release func()

var (
    ErrLeaseCancelled = errors.New("workspace lease cancelled")
    ErrLeaseTimeout   = errors.New("workspace lease timed out")
)
```

All acquisitions enter one FIFO queue. A shared request may join only the
contiguous shared prefix before the first queued exclusive request. An
exclusive request waits for all earlier queue entries and all shared holders.
Cancellation or timeout removes that request atomically; later requests cannot
bypass it. Both operations return `ErrLeaseCancelled` for context cancellation
and `ErrLeaseTimeout` when their derived deadline expires. The wrapper derives
the deadline as the smaller of five minutes and the remaining task
`max_duration`; a primary call with no task deadline uses five minutes.
`Release` is safe to call once or multiple times and only the first call
changes lease state.

The lease is FIFO, cancellable, and bounded by five minutes or the smaller
remaining task deadline. It is held for one mutating tool invocation, not for
the full child lifetime. Release is deferred and idempotent.

Exclusive tools are identified conservatively:

```text
bash
edit
multiedit
write
lsp_rename
lsp_replace_symbol
all MCP tools
```

Read-only tools do not acquire the workspace lease. A detached child shell job
is rejected while a task-run fence is active; the child must wait for its
process to return.

### Execution fence

Each task attempt has a run generation and a shared/exclusive admission gate.
Every child tool call acquires shared admission for the full invocation.
Terminalization acquires exclusive admission before changing task state,
aggregating cost, or publishing terminal delivery. A late runner cannot admit
another tool after fencing and can only perform local cleanup.

Wrapper order is fixed:

```text
primary: hookedTool(exclusiveTool(inner))
child:   exclusiveTool(inner)
```

Hook denial or halt happens before lease acquisition. The wrapper must preserve
the existing tool name, input schema, permission behavior, and response.

### Acceptance tests

- Use a barrier-controlled fake inner mutating tool and two independent agent
  sessions sharing one workspace lease. Assert the second inner invocation
  cannot enter until the first releases, while a fake read-only tool does enter
  during the first lease.
- A waiting lease observes context cancellation and releases its queue entry.
- Lease timeout is distinguishable from task cancellation.
- A hook denial does not acquire the lease.
- Cancellation and terminalization release the lease exactly once.
- A late runner cannot invoke another tool or publish a second terminal event.
- Read-only work continues while another task holds the write lease.

## 7. Agentic Fetch and Legacy Boundaries

`agentic_fetch` remains a primary-only specialized capability, but its child
execution must submit a hidden system-owned task request through the same task
manager and execution fence. It must not create a child session directly.
The request records:

```text
origin=agentic_fetch
fixed internal web profile
owner session from trusted tool context
no public task-control exposure
no call_agent or agentic_fetch in its child tools
```

Hidden `agentic_fetch` tasks are excluded from all public task list, status,
output, cancel, resume, and question routes. They are readable only through an
internal diagnostics interface that requires the trusted originating workspace
and owner session. The public owner-scoped output route applies only to public
`call_agent` tasks.

The fixed profile name is `agentic_fetch_internal`. It is not user-configurable,
not listed in tool descriptions, and permits only the existing fetch/web tools;
it denies `call_agent`, `agentic_fetch`, task-control tools, `question`, and all
workspace-mutating tools. Its temporary fetch directory is owned by the hidden
task runner, exists for the full task attempt, and is removed after terminal
cleanup. Its terminal result is bounded and delivered through the same
outbox/inbox transaction, with origin metadata available only to internal
diagnostics. It uses the existing permission and hook policy for the
originating primary tool; it does not prompt the user for a second delegation
permission.

The acceptance harness injects a fake fetch runner and asserts: one hidden task
row, no direct session creation by `agentic_fetch`, no public child delegation
tools, one terminal outbox/inbox record, temporary directory cleanup after
terminalization, and public owner-scoped output retrieval returns not-found for
the hidden task. Internal diagnostics may retrieve it only with trusted
workspace and originating-session identity.

Existing `agent` permission entries are read as `call_agent` only at the
permission migration boundary. If both names exist, explicit `call_agent`
wins. New writes emit only `call_agent`. Existing historical tool-call
messages remain renderable as legacy data.

Acceptance tests cover specialized fetch routing, legacy permission loading,
new permission serialization, and historical `agent` message rendering.

## 8. Restart, Shutdown, and Error Semantics

At workspace startup, a recovery transaction marks all `pending`, `running`,
and `waiting_for_input` tasks as `interrupted`, with reason
`process_restart`, and writes their terminal outbox/inbox records. Crush does
not replay an unfinished provider turn. Explicit `call_agent(task_id=...)`
creates a new attempt with `run_generation + 1` against the retained child
session only after owner validation and current profile/model resolution. The
recovery transaction increments no run generation, emits exactly one terminal
delivery for each interrupted generation, and resolves each pending question
as `interrupted`; no interrupted waiter is reattached after restart.

Shutdown order is:

```text
stop new task admission
interrupt pending task questions
fence/cancel task runners
wait up to five seconds
mark unsettled runs interrupted
commit terminal delivery
flush messages
close generic services and DB
```

All error classes are stable and machine-matchable:

```text
unknown_profile
disabled_profile
unavailable_model
child_delegation_forbidden
task_capacity_exhausted
task_persistence_failed
task_session_creation_failed
task_timeout
task_cancelled
question_transport_unavailable
task_question_pending
task_question_timeout
task_question_already_resolved
task_owner_deleted
task_already_terminal
task_not_found
empty_prompt
client_required
client_unattached
client_session_required
task_owner_forbidden
```

No user-facing control path should classify errors by matching prose.

## 9. Implementation Order

After this spec passes review, implement in these dependency waves:

1. Profile runtime policy and explicit model/task request fields.
2. Atomic task/session dispatch and corrected terminal SQLite transaction.
3. Durable inbox, parent drain, restart recovery, and resync queries.
4. Production task-question backend/workspace transport and unified task tests.
5. Task/question server, proto, client, workspace, and UI event round trips.
6. Workspace lease and task-run execution fence wrappers.
7. `agentic_fetch`, legacy permission boundary, docs/schema examples.
8. Full focused/race suite, CGO-disabled build, full suite classification, and
   final review.

Waves may parallelize only when their files and contracts do not overlap. The
terminal transaction and execution fence are prerequisites for the transport
and child-question acceptance tests.

## 10. Required Verification Matrix

The implementation is not complete until these commands and focused scenarios
have evidence:

```text
go test ./internal/config ./internal/agent/task ./internal/agent/taskquestion ./internal/agent
go test ./internal/backend ./internal/proto ./internal/server ./internal/client ./internal/workspace ./internal/ui/model
go test -race -shuffle=on -count=1 ./internal/config ./internal/agent/... ./internal/backend ./internal/proto ./internal/server ./internal/client ./internal/workspace
CGO_ENABLED=0 GOEXPERIMENT=greenteagc go build ./...
go test ./...
```

Focused acceptance must include:

```text
profile policy enforcement and reload generation
unified asynchronous completion and explicit cancellation
queued/pending/running/waiting task control
atomic task/session failure rollback
terminal outbox/inbox/cost idempotence
restart recovery and reconnect resync
child question answer/cancel/timeout/shutdown
task event server/client/workspace round trip
owner authorization and deleted-owner denial
workspace lease serialization and cancellation
execution fence against late tool admission
agentic_fetch hidden task routing
legacy permission/message compatibility
```

Known Windows failures caused solely by missing POSIX `sh`, `bash`, or `sleep`
must be reported separately. They do not waive failures in the task,
question, persistence, transport, or profile packages.

The equivalent required commands are:

```powershell
$env:CGO_ENABLED = "0"
$env:GOEXPERIMENT = "greenteagc"
go build ./...
go test ./...
go test -race -shuffle=on -count=1 ./internal/config ./internal/agent/... ./internal/backend ./internal/proto ./internal/server ./internal/client ./internal/workspace
```

Each command is a pass only when it exits successfully. Each focused scenario
is a pass only when its named observable assertions pass. Missing POSIX tools
may be classified separately only for the shell/MCP tests that invoke them.
