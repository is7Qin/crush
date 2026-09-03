package task

import (
	"context"
	"time"
)

// ReasonProcessRestart is the stable interruption reason recorded by
// startup recovery for tasks left live by a previous process.
const ReasonProcessRestart = "process_restart"

// RecoverLiveTasks runs the startup recovery transaction over every
// task still marked pending, running, or waiting for input from a
// previous process: each is force-terminalized as interrupted with
// reason process_restart through the same durable terminal
// transaction (one outbox row, one inbox row, cost aggregation at
// most once per generation) that normal settlement uses. It
// increments no run generation, never replays a provider turn, and
// leaves undelivered child mailbox messages queued. Call it once at
// startup before any runner can settle the same tasks.
func (m *Manager) RecoverLiveTasks(ctx context.Context) ([]*Task, error) {
	live, err := m.store.ListLiveTasks(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	var recovered []*Task
	for _, t := range live {
		u := TerminalUpdate{
			Status:      StatusInterrupted,
			Summary:     ReasonProcessRestart,
			CompletedAt: now,
			// The live row is the only usage record a restart can
			// observe: carry its snapshot through the terminal
			// transaction so the parent cost aggregation sees the
			// usage the crashed process had already accumulated.
			Usage: UsageDelta{
				PromptTokens:     t.PromptTokens,
				CompletionTokens: t.CompletionTokens,
				Cost:             t.Cost,
			},
		}
		saved, won, err := m.store.TerminalizeAndDeliver(ctx, t.ID, t.RunGeneration, u)
		if err != nil {
			return recovered, err
		}
		if !won {
			// A runner in this process already settled the task (or
			// a concurrent terminalization won); the committed
			// records stand and recovery adds nothing.
			continue
		}
		recovered = append(recovered, saved)
	}
	return recovered, nil
}
