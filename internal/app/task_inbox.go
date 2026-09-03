package app

import (
	"context"

	"github.com/charmbracelet/crush/internal/agent/task"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
)

// taskParentGate adapts the session and coordinator services to the
// inbox drain gate: a parent may receive a completion report only
// when its session exists and no agent turn is in flight, so a drain
// never starts a re-entrant run and a deleted parent's rows stay
// retained.
type taskParentGate struct {
	sessions session.Service
	app      *App
}

func (g taskParentGate) ParentReady(ctx context.Context, sessionID string) (bool, error) {
	if _, err := g.sessions.Get(ctx, sessionID); err != nil {
		// Deleted or unreadable parent: retain, never re-enter.
		return false, nil
	}
	if g.app.AgentCoordinator != nil && g.app.AgentCoordinator.IsSessionBusy(sessionID) {
		return false, nil
	}
	return true, nil
}

// taskResultWriter commits one untrusted child result envelope into
// the parent session as an internal message. The rendered envelope
// delimits the child output as evidence with no instruction
// authority; the full result stays behind owner-authorized
// agent_output.
type taskResultWriter struct {
	messages message.Service
}

func (w taskResultWriter) WriteResult(ctx context.Context, ownerSessionID string, env task.TaskResultEnvelope) error {
	_, err := w.messages.Create(ctx, ownerSessionID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: env.Render()}},
	})
	return err
}
