package backend

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/charmbracelet/crush/internal/agent/task"
	"github.com/charmbracelet/crush/internal/agent/taskquestion"
	"github.com/charmbracelet/crush/internal/app"
	"github.com/charmbracelet/crush/internal/proto"
	"github.com/charmbracelet/crush/internal/question"
	"github.com/charmbracelet/crush/internal/session"
)

// Task and task-question control-plane errors. They are stable and
// machine-matchable: the HTTP layer maps each to its documented
// status/code pair and never classifies by prose.
var (
	// ErrClientSessionRequired names an attached client with no
	// current-session binding; task routes derive the trusted owner
	// from that binding.
	ErrClientSessionRequired = errors.New("client has no current session")
	// ErrTaskNotFound names an unknown task id.
	ErrTaskNotFound = errors.New("task not found")
	// ErrTaskForbidden names a task owned by another live session.
	ErrTaskForbidden = errors.New("task owner forbidden")
	// ErrTaskOwnerDeleted names a task whose immutable owner session
	// has been deleted; retained ids grant no access.
	ErrTaskOwnerDeleted = errors.New("task owner deleted")
	// ErrTaskAlreadyTerminal rejects cancelling a task that already
	// reached a terminal state.
	ErrTaskAlreadyTerminal = errors.New("task already terminal")
	// ErrEmptyPrompt rejects a child message with no prompt text.
	ErrEmptyPrompt = errors.New("empty prompt")
	// ErrTaskQuestionNotFound names an unknown task question id.
	ErrTaskQuestionNotFound = errors.New("task question not found")
	// ErrTaskQuestionResolved names a question another answer,
	// cancellation, timeout, or shutdown already resolved.
	ErrTaskQuestionResolved = errors.New("task question already resolved")
	// ErrInvalidTaskQuestionAnswer reports malformed answer data.
	ErrInvalidTaskQuestionAnswer = errors.New("invalid task question answer")
	// ErrTasksUnavailable reports a workspace whose task services are
	// not wired (synthetic harnesses); it is a server defect, never a
	// caller error.
	ErrTasksUnavailable = errors.New("task services are not available in this workspace")
)

// ClientCurrentSession derives the trusted caller identity for a task
// route: the client id must be a valid, non-retired client with a live
// stream attached to the workspace, and the returned session id is
// that client's current-session binding. Bodies never carry authority
// fields; this is the only source of task ownership over the wire.
func (b *Backend) ClientCurrentSession(workspaceID, clientID string) (string, error) {
	if _, err := validateClientID(clientID); err != nil {
		return "", err
	}
	b.mu.Lock()
	_, retired := b.retired[clientID]
	b.mu.Unlock()
	if retired {
		return "", ErrClientRetired
	}
	ws, ok := b.workspaces.Get(workspaceID)
	if !ok {
		return "", ErrWorkspaceNotFound
	}
	ws.clientsMu.Lock()
	cs, attached := ws.clients[clientID]
	sessionID := ""
	if attached && cs.streams > 0 {
		sessionID = cs.currentSessionID
	} else {
		attached = false
	}
	ws.clientsMu.Unlock()
	if !attached {
		return "", ErrClientNotAttached
	}
	if sessionID == "" {
		return "", ErrClientSessionRequired
	}
	// A binding to a since-deleted session is stale: the caller must
	// reselect. Deleted owners never act through retained bindings.
	if ws.App != nil && ws.Sessions != nil {
		if _, err := ws.Sessions.Get(context.Background(), sessionID); err != nil {
			return "", ErrClientSessionRequired
		}
	}
	return sessionID, nil
}

// ChildMessageInput is one owner-authorized direct child message. The
// attachments field is the JSON-array wire text of the request's
// attachment objects; identity is the trusted caller session, never a
// body field.
type ChildMessageInput struct {
	CallerSessionID string
	TaskID          string
	Prompt          string
	Attachments     string
}

// taskWorkspace resolves the workspace's task control bundle.
func taskWorkspaceOf(ws *Workspace) (TaskControl, error) {
	if ws.App == nil {
		return TaskControl{}, ErrTasksUnavailable
	}
	return TaskControlFromApp(ws.App)
}

// TaskControl bundles the durable task primitives one owner-authorized
// control surface drives: the task manager, the task-question service,
// and the session store used to classify deleted owners. The HTTP
// backend and the in-process local workspace run the identical package
// contract through these methods, so authority rules never fork.
type TaskControl struct {
	Mgr      *task.Manager
	Quest    taskquestion.TaskQuestionService
	Sessions session.Service
}

// TaskControlFromApp extracts the task control bundle from a running
// [app.App].
func TaskControlFromApp(a *app.App) (TaskControl, error) {
	if a == nil {
		return TaskControl{}, ErrTasksUnavailable
	}
	mgr, svc := a.Tasks(), a.TaskQuestions()
	if mgr == nil || svc == nil {
		return TaskControl{}, ErrTasksUnavailable
	}
	return TaskControl{Mgr: mgr, Quest: svc, Sessions: a.Sessions}, nil
}

// ownerLive reports whether a task/question owner session still
// exists. A nil session store (synthetic harnesses) reports every
// owner as deleted so retained ids never resolve as live.
func (tc TaskControl) ownerLive(ctx context.Context, sessionID string) bool {
	if tc.Sessions == nil || sessionID == "" {
		return false
	}
	if _, err := tc.Sessions.Get(ctx, sessionID); err != nil {
		return false
	}
	return true
}

// authorizedTask loads a task and enforces the trusted owner session,
// classifying unknown (ErrTaskNotFound), deleted-owner
// (ErrTaskOwnerDeleted), and foreign (ErrTaskForbidden) against
// durable state before any control runs.
func (tc TaskControl) authorizedTask(ctx context.Context, callerSessionID, taskID string) (*task.Task, error) {
	t, err := tc.Mgr.Status(ctx, callerSessionID, taskID)
	switch {
	case err == nil:
		return t, nil
	case errors.Is(err, task.ErrNotFound):
		return nil, ErrTaskNotFound
	case errors.Is(err, task.ErrNotOwner):
		// Re-read the durable record to distinguish a foreign live
		// owner from a deleted owner whose retained ids grant nothing.
		raw, gerr := tc.Mgr.Store().Get(ctx, taskID)
		if gerr != nil {
			return nil, ErrTaskNotFound
		}
		if !tc.ownerLive(ctx, raw.OwnerSessionID) {
			return nil, ErrTaskOwnerDeleted
		}
		return nil, ErrTaskForbidden
	default:
		return nil, err
	}
}

// List returns the caller's task snapshots. A non-empty
// parentSessionID filter must equal the caller identity.
func (tc TaskControl) List(ctx context.Context, callerSessionID, parentSessionID string) ([]proto.TaskSnapshot, error) {
	tasks, err := tc.Mgr.List(ctx, callerSessionID, parentSessionID)
	if err != nil {
		return nil, MapTaskControlError(err)
	}
	out := make([]proto.TaskSnapshot, len(tasks))
	for i, t := range tasks {
		out[i] = TaskToSnapshot(t)
	}
	return out, nil
}

// Get returns the caller-owned task snapshot.
func (tc TaskControl) Get(ctx context.Context, callerSessionID, taskID string) (proto.TaskSnapshot, error) {
	t, err := tc.authorizedTask(ctx, callerSessionID, taskID)
	if err != nil {
		return proto.TaskSnapshot{}, err
	}
	return TaskToSnapshot(t), nil
}

// Cancel cancels the caller's non-terminal attempt and reports the
// post-cancellation status. Cancelling a terminal task is
// ErrTaskAlreadyTerminal and creates no attempt.
func (tc TaskControl) Cancel(ctx context.Context, callerSessionID, taskID string) (proto.AgentCancelAccepted, error) {
	t, err := tc.authorizedTask(ctx, callerSessionID, taskID)
	if err != nil {
		return proto.AgentCancelAccepted{}, err
	}
	if t.Status.Terminal() {
		return proto.AgentCancelAccepted{}, ErrTaskAlreadyTerminal
	}
	if err := tc.Mgr.Cancel(ctx, callerSessionID, taskID); err != nil {
		return proto.AgentCancelAccepted{}, MapTaskControlError(err)
	}
	final, err := tc.Mgr.Status(ctx, callerSessionID, taskID)
	if err != nil {
		return proto.AgentCancelAccepted{}, MapTaskControlError(err)
	}
	return proto.AgentCancelAccepted{TaskID: final.ID, Status: string(final.Status)}, nil
}

// AppendChildMessage queues one direct user message for the task's
// child conversation. The manager allocates the FIFO sequence and,
// for a terminal task, claims the lowest queued message into a
// successor attempt, which the acceptance reports.
func (tc TaskControl) AppendChildMessage(ctx context.Context, in ChildMessageInput) (proto.ChildMessageAccepted, error) {
	if in.Prompt == "" {
		return proto.ChildMessageAccepted{}, ErrEmptyPrompt
	}
	if _, err := tc.authorizedTask(ctx, in.CallerSessionID, in.TaskID); err != nil {
		return proto.ChildMessageAccepted{}, err
	}
	accepted, err := tc.Mgr.AppendMessage(ctx, task.MessageRequest{
		OwnerSessionID: in.CallerSessionID,
		TaskID:         in.TaskID,
		Origin:         task.OriginUser,
		Prompt:         in.Prompt,
		Attachments:    in.Attachments,
	})
	if err != nil {
		return proto.ChildMessageAccepted{}, MapTaskControlError(err)
	}
	return proto.ChildMessageAccepted{
		TaskID:         accepted.TaskID,
		ChildSessionID: accepted.ChildSessionID,
		Sequence:       accepted.Sequence,
		AttemptTaskID:  accepted.AttemptTaskID,
		Status:         string(accepted.Status),
	}, nil
}

// Resync is the durable reconnect recovery read: the caller's task
// snapshots plus undelivered outbox and inbox rows plus the caller's
// unresolved task questions, all from durable stores so recovery works
// after every live pub/sub and SSE event was dropped. Hidden
// system-owned (agentic_fetch) payloads are omitted from every
// projection here; the filter runs on the decoded payload before the
// wire conversion, so the durable rows stay intact for internal
// delivery and diagnostics.
func (tc TaskControl) Resync(ctx context.Context, callerSessionID string) (proto.TaskResyncResponse, error) {
	tasks, err := tc.Mgr.List(ctx, callerSessionID, "")
	if err != nil {
		return proto.TaskResyncResponse{}, MapTaskControlError(err)
	}
	outbox, err := tc.Mgr.Outbox(ctx)
	if err != nil {
		return proto.TaskResyncResponse{}, err
	}
	inbox, err := tc.Mgr.Inbox(ctx, callerSessionID)
	if err != nil {
		return proto.TaskResyncResponse{}, err
	}
	questions, err := tc.Quest.PendingForOwner(ctx, callerSessionID)
	if err != nil {
		return proto.TaskResyncResponse{}, err
	}
	resp := proto.TaskResyncResponse{
		Tasks:  make([]proto.TaskSnapshot, len(tasks)),
		Outbox: make([]proto.OutboxEntry, 0, len(outbox)),
		Inbox:  make([]proto.InboxEntry, 0, len(inbox)),
	}
	for i, t := range tasks {
		resp.Tasks[i] = TaskToSnapshot(t)
	}
	for _, e := range outbox {
		t, err := e.Task()
		if err != nil {
			return proto.TaskResyncResponse{}, err
		}
		if t.IsHidden() || t.OwnerSessionID != callerSessionID {
			continue
		}
		resp.Outbox = append(resp.Outbox, OutboxEntryToWire(e))
	}
	for _, e := range inbox {
		env, err := e.Envelope()
		if err != nil {
			return proto.TaskResyncResponse{}, err
		}
		if env.Profile == task.HiddenProfile {
			continue
		}
		resp.Inbox = append(resp.Inbox, InboxEntryToWire(e))
	}
	resp.Questions = TaskQuestionsToWire(questions)
	return resp, nil
}

// PendingQuestions returns the caller's unresolved task questions,
// oldest first, for reconnect resync.
func (tc TaskControl) PendingQuestions(ctx context.Context, callerSessionID string) ([]proto.TaskQuestion, error) {
	questions, err := tc.Quest.PendingForOwner(ctx, callerSessionID)
	if err != nil {
		return nil, err
	}
	return TaskQuestionsToWire(questions), nil
}

// AnswerQuestion resolves the question with the caller's answers after
// verifying ownership. It reports whether this call won the
// conditional resolution; every error class leaves task state
// untouched.
func (tc TaskControl) AnswerQuestion(ctx context.Context, callerSessionID string, req proto.TaskQuestionAnswerRequest) (bool, error) {
	answers, err := taskQuestionAnswers(req)
	if err != nil {
		return false, err
	}
	return tc.questionOutcome(ctx, callerSessionID, req.QuestionID,
		tc.Quest.AnswerTask(callerSessionID, req.QuestionID, answers))
}

// CancelQuestion resolves the question as cancelled after verifying
// ownership.
func (tc TaskControl) CancelQuestion(ctx context.Context, callerSessionID, questionID string) (bool, error) {
	return tc.questionOutcome(ctx, callerSessionID, questionID,
		tc.Quest.CancelTask(callerSessionID, questionID))
}

// questionOutcome maps a question control call's service error onto
// the stable control-plane errors, distinguishing a deleted owner
// (410) from a live foreign owner (403).
func (tc TaskControl) questionOutcome(ctx context.Context, callerSessionID, questionID string, err error) (bool, error) {
	if err == nil {
		return true, nil
	}
	switch {
	case errors.Is(err, taskquestion.ErrNotFound):
		return false, ErrTaskQuestionNotFound
	case errors.Is(err, taskquestion.ErrAlreadyResolved):
		return false, ErrTaskQuestionResolved
	case errors.Is(err, taskquestion.ErrInvalidRequest):
		return false, ErrInvalidTaskQuestionAnswer
	case errors.Is(err, taskquestion.ErrNotOwner):
		// Only pending questions are controllable, so the tracked
		// record names the owner without leaking resolved state.
		if q, ok := tc.Quest.Pending(questionID); ok &&
			q.OwnerSessionID != callerSessionID &&
			!tc.ownerLive(ctx, q.OwnerSessionID) {
			return false, ErrTaskOwnerDeleted
		}
		return false, ErrTaskForbidden
	default:
		return false, err
	}
}

// -- Backend delegation --

// TaskList lists the caller's tasks within a workspace.
func (b *Backend) TaskList(ctx context.Context, workspaceID, callerSessionID, parentSessionID string) ([]proto.TaskSnapshot, error) {
	ws, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return nil, err
	}
	tc, err := taskWorkspaceOf(ws)
	if err != nil {
		return nil, err
	}
	return tc.List(ctx, callerSessionID, parentSessionID)
}

// TaskGet returns one caller-owned task snapshot.
func (b *Backend) TaskGet(ctx context.Context, workspaceID, callerSessionID, taskID string) (proto.TaskSnapshot, error) {
	ws, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return proto.TaskSnapshot{}, err
	}
	tc, err := taskWorkspaceOf(ws)
	if err != nil {
		return proto.TaskSnapshot{}, err
	}
	return tc.Get(ctx, callerSessionID, taskID)
}

// TaskCancel owner-authorizedly cancels the task's current
// non-terminal attempt.
func (b *Backend) TaskCancel(ctx context.Context, workspaceID, callerSessionID, taskID string) (proto.AgentCancelAccepted, error) {
	ws, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return proto.AgentCancelAccepted{}, err
	}
	tc, err := taskWorkspaceOf(ws)
	if err != nil {
		return proto.AgentCancelAccepted{}, err
	}
	return tc.Cancel(ctx, callerSessionID, taskID)
}

// TaskAppendChildMessage queues one direct user message for the
// task's child conversation.
func (b *Backend) TaskAppendChildMessage(ctx context.Context, workspaceID string, in ChildMessageInput) (proto.ChildMessageAccepted, error) {
	ws, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return proto.ChildMessageAccepted{}, err
	}
	tc, err := taskWorkspaceOf(ws)
	if err != nil {
		return proto.ChildMessageAccepted{}, err
	}
	return tc.AppendChildMessage(ctx, in)
}

// TasksResync runs the durable reconnect recovery read for a workspace.
func (b *Backend) TasksResync(ctx context.Context, workspaceID, callerSessionID string) (proto.TaskResyncResponse, error) {
	ws, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return proto.TaskResyncResponse{}, err
	}
	tc, err := taskWorkspaceOf(ws)
	if err != nil {
		return proto.TaskResyncResponse{}, err
	}
	return tc.Resync(ctx, callerSessionID)
}

// TaskQuestionsPending lists the caller's unresolved task questions.
func (b *Backend) TaskQuestionsPending(ctx context.Context, workspaceID, callerSessionID string) ([]proto.TaskQuestion, error) {
	ws, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return nil, err
	}
	tc, err := taskWorkspaceOf(ws)
	if err != nil {
		return nil, err
	}
	return tc.PendingQuestions(ctx, callerSessionID)
}

// AnswerTaskQuestion resolves a task question with caller ownership.
func (b *Backend) AnswerTaskQuestion(ctx context.Context, workspaceID, callerSessionID string, req proto.TaskQuestionAnswerRequest) (bool, error) {
	ws, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return false, err
	}
	tc, err := taskWorkspaceOf(ws)
	if err != nil {
		return false, err
	}
	return tc.AnswerQuestion(ctx, callerSessionID, req)
}

// CancelTaskQuestion cancels a task question with caller ownership.
func (b *Backend) CancelTaskQuestion(ctx context.Context, workspaceID, callerSessionID, questionID string) (bool, error) {
	ws, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return false, err
	}
	tc, err := taskWorkspaceOf(ws)
	if err != nil {
		return false, err
	}
	return tc.CancelQuestion(ctx, callerSessionID, questionID)
}

// taskQuestionAnswers converts and structurally validates the answer
// payload. Malformed data is rejected before any control reaches the
// service, so a bad body never alters task state.
func taskQuestionAnswers(req proto.TaskQuestionAnswerRequest) ([]question.Answer, error) {
	if req.QuestionID == "" {
		return nil, ErrInvalidTaskQuestionAnswer
	}
	answers := make([]question.Answer, len(req.Responses))
	for i, r := range req.Responses {
		if r.QuestionID == "" {
			return nil, ErrInvalidTaskQuestionAnswer
		}
		answers[i] = question.Answer{
			QuestionID:  r.QuestionID,
			SelectedIDs: r.SelectedIDs,
			FillInText:  r.FillInText,
			Yes:         r.Yes,
			Notes:       r.Notes,
		}
	}
	return answers, nil
}

// MapTaskControlError converts task manager sentinels into the stable
// backend errors above so callers never match on task internals.
func MapTaskControlError(err error) error {
	switch {
	case errors.Is(err, task.ErrNotFound):
		return ErrTaskNotFound
	case errors.Is(err, task.ErrNotOwner):
		return ErrTaskForbidden
	default:
		return err
	}
}

// TaskToSnapshot projects a task record onto the wire shape.
func TaskToSnapshot(t *task.Task) proto.TaskSnapshot {
	snap := proto.TaskSnapshot{
		ID:              t.ID,
		OwnerSessionID:  t.OwnerSessionID,
		ParentSessionID: t.ParentSessionID,
		ChildSessionID:  t.ChildSessionID,
		ParentMessageID: t.ParentMessageID,
		ToolCallID:      t.ToolCallID,
		Profile:         t.Profile,
		Provider:        t.Provider,
		Model:           t.Model,
		RunGeneration:   t.RunGeneration,
		Status:          string(t.Status),
		Result:          t.Result,
		Summary:         t.Summary,
		Error:           t.Err,
		Truncated:       t.ResultTruncated,
	}
	snap.CreatedAt, _ = proto.WireTime(t.CreatedAt)
	if s, ok := proto.WireTime(t.StartedAt); ok {
		snap.StartedAt = s
	}
	if c, ok := proto.WireTime(t.CompletedAt); ok {
		snap.CompletedAt = c
	}
	return snap
}

// TaskEventToWire projects one task lifecycle fact onto its SSE
// payload. At is the transition stamp: the task's persisted
// UpdatedAt, or now for a record that never reached a store.
func TaskEventToWire(ev task.Event) proto.AgentTaskEvent {
	t := ev.Task
	out := proto.AgentTaskEvent{Type: string(ev.Type)}
	if t == nil {
		return out
	}
	out = proto.AgentTaskEvent{
		Type:             string(ev.Type),
		TaskID:           t.ID,
		ParentSessionID:  t.ParentSessionID,
		ChildSessionID:   t.ChildSessionID,
		ParentMessageID:  t.ParentMessageID,
		ToolCallID:       t.ToolCallID,
		Profile:          t.Profile,
		ResolvedProvider: t.Provider,
		ResolvedModel:    t.Model,
		Status:           string(t.Status),
		Summary:          t.Summary,
		Error:            t.Err,
		RunGeneration:    t.RunGeneration,
	}
	at := t.UpdatedAt
	if at.IsZero() {
		at = time.Now().UTC()
	}
	out.At, _ = proto.WireTime(at)
	return out
}

// OutboxEntryToWire projects a durable outbox row onto its resync
// projection. The payload stays the stored JSON text.
func OutboxEntryToWire(e *task.OutboxEntry) proto.OutboxEntry {
	return proto.OutboxEntry{
		ID:            e.ID,
		TaskID:        e.TaskID,
		RunGeneration: e.RunGeneration,
		EventType:     string(e.EventType),
		Payload:       json.RawMessage(e.Payload),
		DeliveredAt:   wireTimePtr(e.DeliveredAt),
		CreatedAt:     mustWireTime(e.CreatedAt),
	}
}

// InboxEntryToWire projects a durable parent inbox row onto its resync
// projection.
func InboxEntryToWire(e *task.InboxEntry) proto.InboxEntry {
	return proto.InboxEntry{
		ID:                 e.ID,
		OwnerSessionID:     e.OwnerSessionID,
		TaskID:             e.TaskID,
		TerminalGeneration: e.TerminalGeneration,
		Payload:            json.RawMessage(e.Payload),
		DeliveredAt:        wireTimePtr(e.DeliveredAt),
		CreatedAt:          mustWireTime(e.CreatedAt),
	}
}

// TaskQuestionsToWire projects domain question records onto their
// transport form, preserving the oldest-first input order.
func TaskQuestionsToWire(qs []taskquestion.TaskQuestion) []proto.TaskQuestion {
	out := make([]proto.TaskQuestion, len(qs))
	for i, q := range qs {
		out[i] = TaskQuestionToWire(q)
	}
	return out
}

// TaskQuestionToWire projects one task-correlated question record.
func TaskQuestionToWire(q taskquestion.TaskQuestion) proto.TaskQuestion {
	wire := proto.TaskQuestion{
		QuestionID:     q.QuestionID,
		TaskID:         q.TaskID,
		OwnerSessionID: q.OwnerSessionID,
		ChildSessionID: q.ChildSessionID,
		RunGeneration:  q.RunGeneration,
		Batch: proto.QuestionRequest{
			ID:                 q.Batch.ID,
			SessionID:          q.Batch.SessionID,
			ToolCallID:         q.Batch.ToolCallID,
			Questions:          questionItemsToProto(q.Batch.Questions),
			ConfirmTitle:       q.Batch.ConfirmTitle,
			ConfirmDescription: q.Batch.ConfirmDescription,
		},
		Answers:    answersToWire(q.Answers),
		Resolution: string(q.Resolution),
	}
	wire.CreatedAt, _ = proto.WireTime(q.CreatedAt)
	wire.ResolvedAt = wireTimePtr(q.ResolvedAt)
	return wire
}

func answersToWire(as []question.Answer) []proto.TaskQuestionAnswer {
	if len(as) == 0 {
		return nil
	}
	out := make([]proto.TaskQuestionAnswer, len(as))
	for i, a := range as {
		out[i] = proto.TaskQuestionAnswer{
			QuestionID:  a.QuestionID,
			SelectedIDs: a.SelectedIDs,
			FillInText:  a.FillInText,
			Yes:         a.Yes,
			Notes:       a.Notes,
		}
	}
	return out
}

func questionItemsToProto(qs []question.Question) []proto.QuestionItem {
	if len(qs) == 0 {
		return nil
	}
	out := make([]proto.QuestionItem, len(qs))
	for i, q := range qs {
		choices := make([]proto.QuestionChoice, len(q.Choices))
		for j, c := range q.Choices {
			choices[j] = proto.QuestionChoice{
				ID:          c.ID,
				Label:       c.Label,
				Description: c.Description,
			}
		}
		out[i] = proto.QuestionItem{
			ID:          q.ID,
			Type:        string(q.Type),
			Label:       q.Label,
			Question:    q.Text,
			Description: q.Description,
			Choices:     choices,
		}
	}
	return out
}

// wireTimePtr formats t as a nullable wire timestamp: nil for the
// zero time.
func wireTimePtr(t time.Time) *string {
	s, ok := proto.WireTime(t)
	if !ok {
		return nil
	}
	return &s
}

// mustWireTime formats a store timestamp that is always set; the zero
// time degrades to the empty string rather than lying about a row.
func mustWireTime(t time.Time) string {
	s, _ := proto.WireTime(t)
	return s
}
