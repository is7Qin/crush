package taskquestion

import (
	"context"
	"time"

	"github.com/charmbracelet/crush/internal/question"
)

// ResolutionUpdate is the payload of the one conditional question
// resolution.
type ResolutionUpdate struct {
	Resolution Resolution
	Answers    []question.Answer
	ResolvedAt time.Time
}

// Repository mirrors task questions in durable storage. The service
// is the single writer and serializes its calls, so implementations
// only need to be safe for concurrent reads. Resolve must be
// conditional on the stored row still being pending so the durable
// mirror agrees with the in-memory winner.
type Repository interface {
	// Save inserts or replaces the full record.
	Save(ctx context.Context, q TaskQuestion) error
	// Resolve applies u only while the stored row is still pending.
	// The bool reports whether this call won the transition.
	Resolve(ctx context.Context, questionID string, u ResolutionUpdate) (bool, error)
	// ListUnresolved returns copies of pending records, oldest first.
	ListUnresolved(ctx context.Context) ([]TaskQuestion, error)
	// ListUnresolvedForOwner returns copies of the owner's pending
	// records, oldest first, for owner-scoped reconnect resync.
	ListUnresolvedForOwner(ctx context.Context, ownerSessionID string) ([]TaskQuestion, error)
}
