package task

import (
	"log/slog"
	"time"
)

// scheduleTerminalRetryLocked arms the one bounded retry chain for a
// failed terminalization. Only the first failure arms a chain; later
// failures are covered by the one already running. Once the manager
// is closed Shutdown's interruptRemaining owns reconciliation and no
// chain is armed. Callers must hold m.mu.
func (m *Manager) scheduleTerminalRetryLocked(at *attemptState, u TerminalUpdate) {
	if m.closed || at.terminalRetryArmed {
		return
	}
	at.terminalRetryArmed = true
	go m.retryTerminalize(at, u)
}

// retryTerminalize re-applies a terminal update whose store
// transaction failed. It reruns only the durable terminal
// transaction, never the runner: exactly-once outbox/inbox delivery,
// the generation fence, and late-runner fencing all hold across
// repeated calls because the conditional update commits at most once.
// The attempt count and exponential backoff are the deliberate
// bound: a store still failing after the budget leaves the attempt
// live (quota and slot held, no speculative release) so Shutdown's
// interruptRemaining and startup RecoverLiveTasks reconcile it
// durably. A lost race (won=false, no error) also ends the chain:
// another caller committed the terminalization.
func (m *Manager) retryTerminalize(at *attemptState, u TerminalUpdate) {
	delay := m.terminalRetryBaseDelay
	for range m.terminalRetryAttempts {
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-m.ctx.Done():
			timer.Stop()
			return
		}
		delay = min(delay*2, m.terminalRetryMaxDelay)
		if _, _, err := m.finish(m.ctx, at, u); err == nil {
			return
		}
	}
	slog.Error("Terminalization retry budget exhausted; shutdown recovery will reconcile",
		"task_id", at.id, "attempts", m.terminalRetryAttempts)
}
