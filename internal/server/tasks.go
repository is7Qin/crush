package server

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/charmbracelet/crush/internal/backend"
	"github.com/charmbracelet/crush/internal/proto"
	"github.com/google/uuid"
)

// requireTaskClient resolves the trusted caller identity every task
// and task-question route needs: a client_id query parameter that
// names an attached, non-retired client, plus that client's
// current-session binding as the owner session. Request bodies and
// URLs carry lookup ids only, never authority fields.
//
// Status/code mapping is exact per the integration contract: missing
// or malformed ids are 401 client_required, retired or unattached
// clients are 403 client_unattached, and a client without a current
// session is 409 client_session_required.
func (c *controllerV1) requireTaskClient(w http.ResponseWriter, r *http.Request, workspaceID string) (string, bool) {
	cid := r.URL.Query().Get("client_id")
	if cid == "" {
		jsonErrorCode(w, http.StatusUnauthorized, "client_required", "client_id is required")
		return "", false
	}
	if _, err := uuid.Parse(cid); err != nil {
		jsonErrorCode(w, http.StatusUnauthorized, "client_required", "client_id is not a valid UUID")
		return "", false
	}
	sessionID, err := c.backend.ClientCurrentSession(workspaceID, cid)
	switch {
	case err == nil:
		return sessionID, true
	case errors.Is(err, backend.ErrWorkspaceNotFound):
		jsonErrorCode(w, http.StatusNotFound, "workspace_not_found", err.Error())
	case errors.Is(err, backend.ErrInvalidClientID):
		jsonErrorCode(w, http.StatusUnauthorized, "client_required", err.Error())
	case errors.Is(err, backend.ErrClientRetired), errors.Is(err, backend.ErrClientNotAttached):
		jsonErrorCode(w, http.StatusForbidden, "client_unattached", err.Error())
	case errors.Is(err, backend.ErrClientSessionRequired):
		jsonErrorCode(w, http.StatusConflict, "client_session_required", err.Error())
	default:
		c.handleTaskError(w, r, err)
	}
	return "", false
}

// handleTaskError maps stable backend task errors onto the exact
// status/code pairs from the integration contract.
func (c *controllerV1) handleTaskError(w http.ResponseWriter, r *http.Request, err error) {
	status, code := http.StatusInternalServerError, ""
	switch {
	case errors.Is(err, backend.ErrWorkspaceNotFound):
		status, code = http.StatusNotFound, "workspace_not_found"
	case errors.Is(err, backend.ErrTaskNotFound):
		status, code = http.StatusNotFound, "task_not_found"
	case errors.Is(err, backend.ErrTaskQuestionNotFound):
		status, code = http.StatusNotFound, "task_question_not_found"
	case errors.Is(err, backend.ErrTaskForbidden):
		status, code = http.StatusForbidden, "task_owner_forbidden"
	case errors.Is(err, backend.ErrTaskOwnerDeleted):
		status, code = http.StatusGone, "task_owner_deleted"
	case errors.Is(err, backend.ErrTaskAlreadyTerminal):
		status, code = http.StatusConflict, "task_already_terminal"
	case errors.Is(err, backend.ErrTaskQuestionResolved):
		status, code = http.StatusConflict, "task_question_already_resolved"
	case errors.Is(err, backend.ErrEmptyPrompt):
		status, code = http.StatusBadRequest, "empty_prompt"
	case errors.Is(err, backend.ErrInvalidTaskQuestionAnswer):
		status, code = http.StatusBadRequest, "invalid_task_question_answer"
	}
	c.server.logError(r, "Task control request failed", "error", err.Error())
	if code == "" {
		jsonError(w, status, err.Error())
		return
	}
	jsonErrorCode(w, status, code, err.Error())
}

// handleGetWorkspaceTasks lists the caller's tasks.
//
//	@Summary		List caller tasks
//	@Tags			tasks
//	@Produce		json
//	@Param			id				path		string	true	"Workspace ID"
//	@Param			client_id			query		string	true	"Attached client ID (UUID)"
//	@Param			parent_session_id	query		string	false	"Parent session filter (must equal the caller's current session)"
//	@Success		200				{object}	proto.TaskListResponse
//	@Router			/workspaces/{id}/tasks [get]
func (c *controllerV1) handleGetWorkspaceTasks(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	caller, ok := c.requireTaskClient(w, r, id)
	if !ok {
		return
	}
	parent := r.URL.Query().Get("parent_session_id")
	if parent != "" && parent != caller {
		c.handleTaskError(w, r, backend.ErrTaskForbidden)
		return
	}
	tasks, err := c.backend.TaskList(r.Context(), id, caller, parent)
	if err != nil {
		c.handleTaskError(w, r, err)
		return
	}
	jsonEncode(w, proto.TaskListResponse{Tasks: tasks})
}

// handleGetWorkspaceTask returns one caller-owned task snapshot.
//
//	@Summary		Get task
//	@Tags			tasks
//	@Produce		json
//	@Param			id			path	string	true	"Workspace ID"
//	@Param			tid			path	string	true	"Task ID"
//	@Param			client_id	query	string	true	"Attached client ID (UUID)"
//	@Success		200			{object}	proto.TaskSnapshot
//	@Router			/workspaces/{id}/tasks/{tid} [get]
func (c *controllerV1) handleGetWorkspaceTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	caller, ok := c.requireTaskClient(w, r, id)
	if !ok {
		return
	}
	snap, err := c.backend.TaskGet(r.Context(), id, caller, r.PathValue("tid"))
	if err != nil {
		c.handleTaskError(w, r, err)
		return
	}
	jsonEncode(w, snap)
}

// handleGetWorkspaceTaskOutput returns the bounded stored result of a
// caller-owned task.
//
//	@Summary		Get task output
//	@Tags			tasks
//	@Produce		json
//	@Param			id			path	string	true	"Workspace ID"
//	@Param			tid			path	string	true	"Task ID"
//	@Param			client_id	query	string	true	"Attached client ID (UUID)"
//	@Success		200			{object}	proto.TaskOutputResponse
//	@Router			/workspaces/{id}/tasks/{tid}/output [get]
func (c *controllerV1) handleGetWorkspaceTaskOutput(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	caller, ok := c.requireTaskClient(w, r, id)
	if !ok {
		return
	}
	snap, err := c.backend.TaskGet(r.Context(), id, caller, r.PathValue("tid"))
	if err != nil {
		c.handleTaskError(w, r, err)
		return
	}
	jsonEncode(w, proto.TaskOutputResponse{
		TaskID:    snap.ID,
		Status:    snap.Status,
		Result:    snap.Result,
		Summary:   snap.Summary,
		Error:     snap.Error,
		Truncated: snap.Truncated,
	})
}

// handlePostWorkspaceTaskCancel cancels the caller's non-terminal task
// attempt. Cancelling a terminal task is 409 task_already_terminal.
//
//	@Summary		Cancel task
//	@Tags			tasks
//	@Accept			json
//	@Produce		json
//	@Param			id			path	string	true	"Workspace ID"
//	@Param			tid			path	string	true	"Task ID"
//	@Param			client_id	query	string	true	"Attached client ID (UUID)"
//	@Success		200			{object}	proto.AgentCancelAccepted
//	@Router			/workspaces/{id}/tasks/{tid}/cancel [post]
func (c *controllerV1) handlePostWorkspaceTaskCancel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	caller, ok := c.requireTaskClient(w, r, id)
	if !ok {
		return
	}
	accepted, err := c.backend.TaskCancel(r.Context(), id, caller, r.PathValue("tid"))
	if err != nil {
		c.handleTaskError(w, r, err)
		return
	}
	jsonEncode(w, accepted)
}

// handlePostWorkspaceTaskMessages appends a direct user message to the
// task's child conversation. Messages are canonicalized to the task
// id in the path; the acceptance echoes the derived child session id
// and the FIFO sequence, and never accepts authority fields.
//
//	@Summary		Send direct child message
//	@Tags			tasks
//	@Accept			json
//	@Produce		json
//	@Param			id			path	string						true	"Workspace ID"
//	@Param			tid			path	string						true	"Task ID"
//	@Param			client_id	query	string						true	"Attached client ID (UUID)"
//	@Param			request		body	proto.ChildMessageRequest	true	"Child message"
//	@Success		202			{object}	proto.ChildMessageAccepted
//	@Router			/workspaces/{id}/tasks/{tid}/messages [post]
func (c *controllerV1) handlePostWorkspaceTaskMessages(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	caller, ok := c.requireTaskClient(w, r, id)
	if !ok {
		return
	}
	var req proto.ChildMessageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		c.server.logError(r, "Failed to decode request", "error", err)
		jsonError(w, http.StatusBadRequest, "failed to decode request")
		return
	}
	attachments := "[]"
	if len(req.Attachments) > 0 {
		raw, err := json.Marshal(req.Attachments)
		if err != nil {
			jsonError(w, http.StatusBadRequest, "failed to encode attachments")
			return
		}
		attachments = string(raw)
	}
	accepted, err := c.backend.TaskAppendChildMessage(r.Context(), id, backend.ChildMessageInput{
		CallerSessionID: caller,
		TaskID:          r.PathValue("tid"),
		Prompt:          req.Prompt,
		Attachments:     attachments,
	})
	if err != nil {
		c.handleTaskError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(accepted)
}

// handleGetWorkspaceTasksResync is the durable reconnect recovery
// read, scoped to the caller's current session.
//
//	@Summary		Resync tasks
//	@Tags			tasks
//	@Produce		json
//	@Param			id			path	string	true	"Workspace ID"
//	@Param			client_id	query	string	true	"Attached client ID (UUID)"
//	@Success		200			{object}	proto.TaskResyncResponse
//	@Router			/workspaces/{id}/tasks/resync [get]
func (c *controllerV1) handleGetWorkspaceTasksResync(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	caller, ok := c.requireTaskClient(w, r, id)
	if !ok {
		return
	}
	resync, err := c.backend.TasksResync(r.Context(), id, caller)
	if err != nil {
		c.handleTaskError(w, r, err)
		return
	}
	jsonEncode(w, resync)
}

// handleGetWorkspaceTaskQuestionsPending lists the caller's unresolved
// task questions.
//
//	@Summary		List pending task questions
//	@Tags			questions
//	@Produce		json
//	@Param			id			path	string	true	"Workspace ID"
//	@Param			client_id	query	string	true	"Attached client ID (UUID)"
//	@Success		200			{object}	proto.TaskQuestionListResponse
//	@Router			/workspaces/{id}/task-questions/pending [get]
func (c *controllerV1) handleGetWorkspaceTaskQuestionsPending(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	caller, ok := c.requireTaskClient(w, r, id)
	if !ok {
		return
	}
	questions, err := c.backend.TaskQuestionsPending(r.Context(), id, caller)
	if err != nil {
		c.handleTaskError(w, r, err)
		return
	}
	jsonEncode(w, proto.TaskQuestionListResponse{Questions: questions})
}

// handlePostWorkspaceTaskQuestionsAnswer resolves a task question with
// owner authorization. Foreign owner, unknown, resolved, deleted-owner,
// and malformed bodies use the contract's exact status/code pairs and
// never alter task state.
//
//	@Summary		Answer task question
//	@Tags			questions
//	@Accept			json
//	@Produce		json
//	@Param			id			path	string								true	"Workspace ID"
//	@Param			client_id	query	string								true	"Attached client ID (UUID)"
//	@Param			request		body	proto.TaskQuestionAnswerRequest		true	"Task question answer"
//	@Success		200			{object}	proto.TaskQuestionResolutionResponse
//	@Router			/workspaces/{id}/task-questions/answer [post]
func (c *controllerV1) handlePostWorkspaceTaskQuestionsAnswer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	caller, ok := c.requireTaskClient(w, r, id)
	if !ok {
		return
	}
	var req proto.TaskQuestionAnswerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		c.server.logError(r, "Failed to decode request", "error", err)
		jsonError(w, http.StatusBadRequest, "failed to decode request")
		return
	}
	resolved, err := c.backend.AnswerTaskQuestion(r.Context(), id, caller, req)
	if err != nil {
		c.handleTaskError(w, r, err)
		return
	}
	jsonEncode(w, proto.TaskQuestionResolutionResponse{Resolved: resolved})
}

// handlePostWorkspaceTaskQuestionsCancel resolves a task question as
// cancelled with owner authorization.
//
//	@Summary		Cancel task question
//	@Tags			questions
//	@Accept			json
//	@Produce		json
//	@Param			id			path	string								true	"Workspace ID"
//	@Param			client_id	query	string								true	"Attached client ID (UUID)"
//	@Param			request		body	proto.TaskQuestionCancelRequest	true	"Task question cancellation"
//	@Success		200			{object}	proto.TaskQuestionResolutionResponse
//	@Router			/workspaces/{id}/task-questions/cancel [post]
func (c *controllerV1) handlePostWorkspaceTaskQuestionsCancel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	caller, ok := c.requireTaskClient(w, r, id)
	if !ok {
		return
	}
	var req proto.TaskQuestionCancelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		c.server.logError(r, "Failed to decode request", "error", err)
		jsonError(w, http.StatusBadRequest, "failed to decode request")
		return
	}
	resolved, err := c.backend.CancelTaskQuestion(r.Context(), id, caller, req.QuestionID)
	if err != nil {
		c.handleTaskError(w, r, err)
		return
	}
	jsonEncode(w, proto.TaskQuestionResolutionResponse{Resolved: resolved})
}
