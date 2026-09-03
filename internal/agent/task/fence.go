package task

import (
	"context"
	"sync"
)

// Fence is one task attempt's shared/exclusive admission gate. Every
// child tool call takes shared admission for the full invocation (see
// tools.Fence); terminalization takes exclusive admission before it
// changes task state, aggregates cost, or publishes terminal delivery.
// After fencing, shared admission always fails, so a late runner can
// only perform local cleanup and cannot invoke another tool.
type Fence struct {
	mu     sync.Mutex
	shared int
	fenced bool
	// idle is non-nil exactly while shared > 0: created on the 0->1
	// admission and closed once on the 1->0 drain.
	idle chan struct{}
}

// NewFence returns an open fence.
func NewFence() *Fence { return &Fence{} }

// AdmitShared takes shared admission for one tool invocation and
// returns its idempotent release. Once fenced, it fails with
// ErrRunFenced.
func (f *Fence) AdmitShared(context.Context) (func(), error) {
	f.mu.Lock()
	if f.fenced {
		f.mu.Unlock()
		return nil, ErrRunFenced
	}
	f.shared++
	if f.shared == 1 {
		f.idle = make(chan struct{})
	}
	f.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			f.mu.Lock()
			f.shared--
			if f.shared == 0 && f.idle != nil {
				close(f.idle)
				f.idle = nil
			}
			f.mu.Unlock()
		})
	}, nil
}

// Fence takes exclusive admission: it closes shared admission
// immediately and waits until every in-flight shared holder has
// released, or ctx expires. The fence stays closed regardless of the
// wait outcome; ctx expiry only reports that not every in-flight call
// drained in time.
func (f *Fence) Fence(ctx context.Context) error {
	f.mu.Lock()
	f.fenced = true
	idle := f.idle
	f.mu.Unlock()
	if idle == nil {
		return nil // no shared admission was in flight
	}
	select {
	case <-idle:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
