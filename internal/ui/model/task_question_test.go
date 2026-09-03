package model

import (
	"sync"
	"testing"

	"github.com/charmbracelet/crush/internal/agent/task"
	"github.com/charmbracelet/crush/internal/agent/taskquestion"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/charmbracelet/crush/internal/question"
	"github.com/charmbracelet/crush/internal/ui/chat"
	"github.com/charmbracelet/crush/internal/ui/dialog"
	"github.com/charmbracelet/crush/internal/workspace"
	"github.com/stretchr/testify/require"
)

// taskQuestionWorkspace records task-question control calls so the
// test can assert the dialog resolves by question id, and keeps the
// primary single-slot answer path separate.
type taskQuestionWorkspace struct {
	workspace.Workspace

	mu        sync.Mutex
	answered  map[string][]question.Answer
	cancelled []string
}

func (w *taskQuestionWorkspace) Config() *config.Config { return nil }

func (w *taskQuestionWorkspace) TaskQuestionAnswer(questionID string, responses []question.Answer) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.answered == nil {
		w.answered = map[string][]question.Answer{}
	}
	w.answered[questionID] = responses
	return true
}

func (w *taskQuestionWorkspace) TaskQuestionCancel(questionID string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.cancelled = append(w.cancelled, questionID)
	return true
}

func (w *taskQuestionWorkspace) record(questionID string) []question.Answer {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.answered[questionID]
}

func (w *taskQuestionWorkspace) wasCancelled(questionID string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, id := range w.cancelled {
		if id == questionID {
			return true
		}
	}
	return false
}

func newTaskQuestionTestUI(t *testing.T) (*UI, *taskQuestionWorkspace) {
	t.Helper()
	u := newTestUI()
	fake := &taskQuestionWorkspace{}
	u.com.Workspace = fake
	return u, fake
}

func childQuestion(id, taskID string) taskquestion.TaskQuestion {
	return taskquestion.TaskQuestion{
		QuestionID:     id,
		TaskID:         taskID,
		OwnerSessionID: "owner",
		ChildSessionID: "child",
		RunGeneration:  1,
		Resolution:     taskquestion.ResolutionPending,
		Batch: question.Request{
			ID:         "batch-" + id,
			SessionID:  "child",
			ToolCallID: "tc-child",
			Questions: []question.Question{{
				ID:          "q-" + id,
				Type:        question.TypeYesNo,
				Text:        "Proceed?",
				Description: "Continue with the step?",
			}},
		},
	}
}

// TestUI_TaskQuestionDialog_KeyedByQuestionID is the spec section 5
// UI model acceptance: a translated child question request fed into
// Update opens the existing question dialog keyed by question_id, a
// resolution notification reconciles only its matching question, and
// submissions carry the right question id.
func TestUI_TaskQuestionDialog_KeyedByQuestionID(t *testing.T) {
	u, _ := newTaskQuestionTestUI(t)

	u.Update(pubsub.Event[taskquestion.TaskQuestion]{
		Type:    pubsub.CreatedEvent,
		Payload: childQuestion("qq-1", "t1"),
	})

	qf, ok := u.activeInline.(*dialog.QuestionForm)
	require.True(t, ok, "the existing question dialog must open")
	require.Equal(t, "batch-qq-1", qf.BatchID, "the form renders the child batch")
	require.Equal(t, "qq-1", u.activeQuestionKey, "the dialog is keyed by question_id")

	// A second child question queues: no competing model and no
	// displacement of the open form.
	u.Update(pubsub.Event[taskquestion.TaskQuestion]{
		Type:    pubsub.CreatedEvent,
		Payload: childQuestion("qq-2", "t2"),
	})
	require.Same(t, qf, u.activeInline, "the open form must not be replaced")
	require.Len(t, u.pendingTaskQuestions, 1)

	// A resolution for the queued question reconciles only the queue
	// entry; the open dialog keeps its identity.
	u.Update(pubsub.Event[taskquestion.Notification]{
		Type:    pubsub.CreatedEvent,
		Payload: taskquestion.Notification{QuestionID: "qq-2", TaskID: "t2", Resolution: taskquestion.ResolutionCancelled},
	})
	require.Empty(t, u.pendingTaskQuestions)
	require.Equal(t, "qq-1", u.activeQuestionKey)

	// A primary question notification must not dismiss a child form.
	u.Update(pubsub.Event[question.Notification]{
		Type:    pubsub.CreatedEvent,
		Payload: question.Notification{BatchID: "unrelated-batch"},
	})
	require.Same(t, qf, u.activeInline, "a foreign resolution must not reconcile the child dialog")

	// The matching question_id resolution dismisses exactly that
	// dialog and frees the editor.
	u.Update(pubsub.Event[taskquestion.Notification]{
		Type:    pubsub.CreatedEvent,
		Payload: taskquestion.Notification{QuestionID: "qq-1", TaskID: "t1", Resolution: taskquestion.ResolutionAnswered},
	})
	require.Nil(t, u.activeInline)
	require.Empty(t, u.activeQuestionKey)
	require.Nil(t, u.activeTaskQuestion)
}

// TestUI_TaskQuestionSubmitCarriesQuestionID proves the dialog's
// answer and cancel callbacks route through the task-question control
// path keyed by the question id, never the primary single-slot path.
func TestUI_TaskQuestionSubmitCarriesQuestionID(t *testing.T) {
	u, fake := newTaskQuestionTestUI(t)

	u.Update(pubsub.Event[taskquestion.TaskQuestion]{
		Type:    pubsub.CreatedEvent,
		Payload: childQuestion("qq-9", "t9"),
	})
	qf, ok := u.activeInline.(*dialog.QuestionForm)
	require.True(t, ok)

	yes := true
	qf.OnAnswer([]question.Answer{{QuestionID: "q-qq-9", Yes: &yes}})
	require.Equal(t, []question.Answer{{QuestionID: "q-qq-9", Yes: &yes}}, fake.record("qq-9"))

	// Cancel path.
	u.activeInline = nil
	u.activeQuestionKey = ""
	u.Update(pubsub.Event[taskquestion.TaskQuestion]{
		Type:    pubsub.CreatedEvent,
		Payload: childQuestion("qq-10", "t10"),
	})
	qf2 := u.activeInline.(*dialog.QuestionForm)
	qf2.OnCancel()
	require.True(t, fake.wasCancelled("qq-10"))
}

// TestUI_TaskQuestionPromotionAfterDismiss checks the queued child
// question resurfaces once the open form resolves.
func TestUI_TaskQuestionPromotionAfterDismiss(t *testing.T) {
	u, fake := newTaskQuestionTestUI(t)

	u.Update(pubsub.Event[taskquestion.TaskQuestion]{Type: pubsub.CreatedEvent, Payload: childQuestion("qq-a", "ta")})
	u.Update(pubsub.Event[taskquestion.TaskQuestion]{Type: pubsub.CreatedEvent, Payload: childQuestion("qq-b", "tb")})
	require.Equal(t, "qq-a", u.activeQuestionKey)

	// The user answers qq-a through the form callback; the form
	// completes and the queued question is promoted in Update.
	u.activeInline.(*dialog.QuestionForm).OnAnswer([]question.Answer{})
	u.activeInline = nil
	u.clearActiveQuestion()
	require.Equal(t, "qq-b", u.activeQuestionKey)
	require.NotNil(t, fake)

	u.Update(pubsub.Event[taskquestion.Notification]{
		Type:    pubsub.CreatedEvent,
		Payload: taskquestion.Notification{QuestionID: "qq-b", Resolution: taskquestion.ResolutionAnswered},
	})
	require.Nil(t, u.activeInline)
	require.Empty(t, u.activeQuestionKey)
}

// TestUI_PrimaryQuestionNotifiedReconcilesItsOwnBatch keeps the
// legacy flow honest: a primary batch notification keyed by batch id
// dismisses only the matching primary form.
func TestUI_PrimaryQuestionNotifiedReconcilesItsOwnBatch(t *testing.T) {
	u, _ := newTaskQuestionTestUI(t)

	u.Update(pubsub.Event[question.Request]{
		Type:    pubsub.CreatedEvent,
		Payload: question.Request{ID: "b-primary", Questions: []question.Question{{ID: "q1", Type: question.TypeFreeText, Text: "Q?", Description: "d"}}},
	})
	require.Equal(t, "b-primary", u.activeQuestionKey)

	u.Update(pubsub.Event[question.Notification]{
		Type:    pubsub.CreatedEvent,
		Payload: question.Notification{BatchID: "b-primary"},
	})
	require.Nil(t, u.activeInline)
	require.Empty(t, u.activeQuestionKey)
}

// TestUI_TaskEventsCorrelateAgentToolItem feeds task facts into
// Update and asserts the call_agent tool item is found by tool call
// id and records both correlation ids; mismatched or stale facts do
// not clobber newer state.
func TestUI_TaskEventsCorrelateAgentToolItem(t *testing.T) {
	u, _ := newTaskQuestionTestUI(t)

	toolCall := message.ToolCall{ID: "tc-corr", Name: "call_agent", Input: `{"profile":"coder","prompt":"go"}`}
	item := chat.NewToolMessageItem(u.com.Styles, "m1", toolCall, nil, false, "")
	u.chat.SetMessages(item)

	u.Update(pubsub.Event[task.Event]{
		Type: pubsub.UpdatedEvent,
		Payload: task.Event{Type: task.EventStarted, Task: &task.Task{
			ID: "tA", ToolCallID: "tc-corr", Status: task.StatusRunning, RunGeneration: 2,
		}},
	})

	tracker, ok := item.(chat.TaskStateTracker)
	require.True(t, ok)
	require.Equal(t, chat.TaskState{TaskID: "tA", Event: "started", Status: "running", Revision: 2}, tracker.TaskState())

	// A fact for another tool call must not touch this item.
	u.Update(pubsub.Event[task.Event]{
		Type: pubsub.UpdatedEvent,
		Payload: task.Event{Type: task.EventCompleted, Task: &task.Task{
			ID: "tB", ToolCallID: "tc-other", Status: task.StatusCompleted, RunGeneration: 1,
		}},
	})
	require.Equal(t, "tA", tracker.TaskState().TaskID)

	// A stale run generation for the same task is ignored.
	u.Update(pubsub.Event[task.Event]{
		Type: pubsub.UpdatedEvent,
		Payload: task.Event{Type: task.EventCreated, Task: &task.Task{
			ID: "tA", ToolCallID: "tc-corr", Status: task.StatusPending, RunGeneration: 1,
		}},
	})
	require.Equal(t, uint64(2), tracker.TaskState().Revision)
}
