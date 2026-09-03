package app

import (
	"context"
	"log/slog"

	"github.com/charmbracelet/crush/internal/agent/task"
	"github.com/charmbracelet/crush/internal/pubsub"
)

// handleTaskEvent is the manager lifecycle bridge registered by New
// and NewForTest. Every event is republished onto the task events
// broker as a wake-up hint; the durable task row stays the source of
// truth. Hidden system-owned (agentic_fetch) facts are the exception:
// the broker feeds every attached client's live stream, so the bridge
// gates them out at the source instead of only at the wire boundary
// (server.wrapEvent stays the authoritative drop for any other
// producer, e.g. the outbox replay below).
//
// A committed terminalization additionally drains the owner's durable
// mailbox off the manager's synchronous publish path. The outbox
// drain republishes the owner's already-superseded lifecycle records
// as events (duplicates are safe: events are facts, not commands) and
// acks them, so the resync replay set stays bounded; the triggering
// row itself stays pending because this callback's live publish does
// not prove any consumer received it, and a client whose stream
// dropped must still recover it through resync. The inbox drain hands
// the completion report to an idle parent immediately; a parent still
// running the turn that spawned the child keeps its row pending for
// the next drain, which the parent's own terminal RunComplete
// triggers (see watchParentIdle). Both drains run on the workspace
// lifetime context, never a request context, so accepted durable work
// is not cancelled by the caller whose turn triggered the transition.
// The nil guards mirror NewForTest's partial wiring, which registers
// no inbox.
func (app *App) handleTaskEvent(ev task.Event) {
	if !ev.Task.IsHidden() {
		app.taskEvents.PublishMustDeliver(app.globalCtx, pubsub.UpdatedEvent, ev)
	}
	if ev.Task == nil || !task.Status(ev.Type).Terminal() {
		return
	}
	owner := ev.Task.OwnerSessionID
	go func() {
		ctx := context.WithoutCancel(app.globalCtx)
		if app.taskOutbox != nil {
			if err := app.taskOutbox.DrainExcept(ctx, owner, ev); err != nil {
				slog.Warn("Failed to drain task outbox",
					"owner_session_id", owner, "error", err)
			}
		}
		if app.taskInbox != nil {
			if _, err := app.taskInbox.Drain(ctx, owner); err != nil {
				slog.Warn("Failed to drain task inbox",
					"owner_session_id", owner, "error", err)
			}
		}
	}()
}

// watchParentIdle closes the one gap the terminal-event drain cannot
// cover: a child that terminalizes while its parent is mid-turn keeps
// its inbox row pending, and no further task event may ever arrive to
// retry it. The parent's own terminal RunComplete is the authoritative
// idle signal, so each such event re-attempts that session's drain.
// The event is only a wake-up hint: the per-parent drain stays
// serialized, re-checks ParentReady (the coordinator releases the busy
// entry before publishing), and never starts a run, so duplicate or
// spurious wake-ups deliver nothing twice and undelivered rows simply
// survive for the next signal. Registered after the event fan-in so it
// shares the eventsCtx lifetime and teardown.
func (app *App) watchParentIdle() {
	ctx := app.eventsCtx
	app.serviceEventsWG.Go(func() {
		events := app.runCompletions.Subscribe(ctx)
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-events:
				if !ok {
					return
				}
				sessionID := ev.Payload.SessionID
				if app.taskInbox == nil || sessionID == "" {
					continue
				}
				go func() {
					dctx := context.WithoutCancel(app.globalCtx)
					if _, err := app.taskInbox.Drain(dctx, sessionID); err != nil {
						slog.Warn("Failed to drain task inbox after parent run completion",
							"owner_session_id", sessionID, "error", err)
					}
				}()
			}
		}
	})
}
