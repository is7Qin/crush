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

// taskResultWriter commits one drain window's untrusted child
// result envelopes into the parent session as a single internal
// message. The rendered batch delimits the child output as evidence
// with no instruction authority; full results stay behind
// owner-authorized agent_output, except for hidden-profile tasks,
// whose bounded result is their only delivery channel.
type taskResultWriter struct {
	messages message.Service
}

func (w taskResultWriter) WriteResults(ctx context.Context, ownerSessionID string, envs []task.TaskResultEnvelope, pending int) error {
	if len(envs) == 0 {
		return nil
	}
	_, err := w.messages.Create(ctx, ownerSessionID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: task.RenderBatch(envs, pending)}},
	})
	return err
}
