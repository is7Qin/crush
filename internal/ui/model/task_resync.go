package model

import (
	"encoding/json"
	"log/slog"
	"time"

	"github.com/charmbracelet/crush/internal/agent/task"
	"github.com/charmbracelet/crush/internal/agent/taskquestion"
	"github.com/charmbracelet/crush/internal/proto"
	"github.com/charmbracelet/crush/internal/question"
)

// applyTaskResync applies durable lifecycle facts and unresolved child
// questions to the same UI paths used by live workspace events. Inbox rows
// are intentionally not synthesized into messages: the server's inbox
// drainer owns their commit, and the surrounding session reload observes a
// message only after that commit succeeds.
func (m *UI) applyTaskResync(resync proto.TaskResyncResponse) {
	for _, entry := range resync.Outbox {
		var recovered task.Task
		if err := json.Unmarshal(entry.Payload, &recovered); err != nil {
			slog.Warn("Failed to decode task outbox resync entry", "entry_id", entry.ID, "error", err)
			continue
		}
		if recovered.IsHidden() {
			continue
		}
		m.handleTaskEvent(task.Event{
			Type: task.EventType(entry.EventType),
			Task: &recovered,
		})
	}

	for _, snapshot := range resync.Tasks {
		if snapshot.Profile == task.HiddenProfile {
			continue
		}
		m.handleTaskEvent(task.Event{
			Type: taskEventTypeForStatus(task.Status(snapshot.Status)),
			Task: taskFromSnapshot(snapshot),
		})
	}

	questions := make([]taskquestion.TaskQuestion, 0, len(resync.Questions))
	for _, wire := range resync.Questions {
		if wire.Resolution != "" && wire.Resolution != string(taskquestion.ResolutionPending) {
			continue
		}
		questions = append(questions, taskQuestionFromResyncWire(wire))
	}
	m.reconcileTaskQuestions(questions)
}

func (m *UI) reconcileTaskQuestions(questions []taskquestion.TaskQuestion) {
	pending := make(map[string]struct{}, len(questions))
	for _, q := range questions {
		pending[q.QuestionID] = struct{}{}
	}

	activeID := ""
	if m.activeTaskQuestion != nil {
		activeID = m.activeTaskQuestion.QuestionID
	}
	if activeID != "" {
		if _, ok := pending[activeID]; !ok {
			m.activeInline = nil
			m.activeQuestionKey = ""
			m.activeTaskQuestion = nil
			m.textarea.Focus()
			m.updateLayoutAndSize()
			activeID = ""
		}
	}

	m.pendingTaskQuestions = m.pendingTaskQuestions[:0]
	for _, q := range questions {
		if q.QuestionID != activeID {
			m.pendingTaskQuestions = append(m.pendingTaskQuestions, q)
		}
	}
	if m.activeInline == nil {
		m.promoteNextTaskQuestion()
	}
}

func taskEventTypeForStatus(status task.Status) task.EventType {
	switch status {
	case task.StatusPending:
		return task.EventCreated
	case task.StatusRunning:
		return task.EventStarted
	default:
		return task.EventType(status)
	}
}

func taskFromSnapshot(snapshot proto.TaskSnapshot) *task.Task {
	createdAt, _ := proto.ParseWireTime(snapshot.CreatedAt)
	startedAt, _ := proto.ParseWireTime(snapshot.StartedAt)
	completedAt, _ := proto.ParseWireTime(snapshot.CompletedAt)
	updatedAt := createdAt
	if !startedAt.IsZero() {
		updatedAt = startedAt
	}
	if !completedAt.IsZero() {
		updatedAt = completedAt
	}
	return &task.Task{
		ID:              snapshot.ID,
		OwnerSessionID:  snapshot.OwnerSessionID,
		ParentSessionID: snapshot.ParentSessionID,
		ChildSessionID:  snapshot.ChildSessionID,
		ParentMessageID: snapshot.ParentMessageID,
		ToolCallID:      snapshot.ToolCallID,
		Profile:         snapshot.Profile,
		Provider:        snapshot.Provider,
		Model:           snapshot.Model,
		RunGeneration:   snapshot.RunGeneration,
		Status:          task.Status(snapshot.Status),
		Result:          snapshot.Result,
		Summary:         snapshot.Summary,
		Err:             snapshot.Error,
		ResultTruncated: snapshot.Truncated,
		CreatedAt:       createdAt,
		StartedAt:       startedAt,
		CompletedAt:     completedAt,
		UpdatedAt:       updatedAt,
	}
}

func taskQuestionFromResyncWire(w proto.TaskQuestion) taskquestion.TaskQuestion {
	questions := make([]question.Question, len(w.Batch.Questions))
	for i, item := range w.Batch.Questions {
		choices := make([]question.Choice, len(item.Choices))
		for j, choice := range item.Choices {
			choices[j] = question.Choice{
				ID:          choice.ID,
				Label:       choice.Label,
				Description: choice.Description,
			}
		}
		questions[i] = question.Question{
			ID:          item.ID,
			Type:        question.Type(item.Type),
			Label:       item.Label,
			Text:        item.Question,
			Description: item.Description,
			Choices:     choices,
		}
	}
	answers := make([]question.Answer, len(w.Answers))
	for i, answer := range w.Answers {
		answers[i] = question.Answer{
			QuestionID:  answer.QuestionID,
			SelectedIDs: answer.SelectedIDs,
			FillInText:  answer.FillInText,
			Yes:         answer.Yes,
			Notes:       answer.Notes,
		}
	}
	createdAt, _ := proto.ParseWireTime(w.CreatedAt)
	var resolvedAt time.Time
	if w.ResolvedAt != nil {
		resolvedAt, _ = proto.ParseWireTime(*w.ResolvedAt)
	}
	return taskquestion.TaskQuestion{
		QuestionID:     w.QuestionID,
		TaskID:         w.TaskID,
		OwnerSessionID: w.OwnerSessionID,
		ChildSessionID: w.ChildSessionID,
		RunGeneration:  w.RunGeneration,
		Batch: question.Request{
			ID:                 w.Batch.ID,
			SessionID:          w.Batch.SessionID,
			ToolCallID:         w.Batch.ToolCallID,
			Questions:          questions,
			ConfirmTitle:       w.Batch.ConfirmTitle,
			ConfirmDescription: w.Batch.ConfirmDescription,
		},
		Answers:    answers,
		Resolution: taskquestion.Resolution(w.Resolution),
		CreatedAt:  createdAt,
		ResolvedAt: resolvedAt,
	}
}
