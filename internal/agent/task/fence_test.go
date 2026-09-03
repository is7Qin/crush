package task

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestFence_FenceWaitsForSharedDrain(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := NewFence()
	release, err := f.AdmitShared(ctx)
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() { done <- f.Fence(ctx) }()
	select {
	case err := <-done:
		t.Fatalf("Fence returned while shared admission was held: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	release()
	require.NoError(t, <-done)
}

func TestFence_LateAdmissionRejected(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := NewFence()
	release, err := f.AdmitShared(ctx)
	require.NoError(t, err)
	release()
	require.NoError(t, f.Fence(ctx))

	_, err = f.AdmitShared(ctx)
	require.ErrorIs(t, err, ErrRunFenced)
}

func TestFence_FenceStaysClosedWhenWaitTimesOut(t *testing.T) {
	t.Parallel()
	f := NewFence()
	release, err := f.AdmitShared(context.Background())
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, f.Fence(ctx), context.DeadlineExceeded)

	// The admission closed regardless of the undrained holder: a late
	// runner cannot admit another tool after fencing.
	_, err = f.AdmitShared(context.Background())
	require.ErrorIs(t, err, ErrRunFenced)

	// The abandoned holder still drains cleanly exactly once.
	release()
	release()
	require.NoError(t, f.Fence(context.Background()))
}

func TestFence_ReleaseIsIdempotent(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := NewFence()
	r1, err := f.AdmitShared(ctx)
	require.NoError(t, err)
	r2, err := f.AdmitShared(ctx)
	require.NoError(t, err)

	// Dropping one admission and calling its release twice must leave
	// the other held admission blocking the drain.
	r1()
	r1() // second call changes nothing
	done := make(chan error, 1)
	go func() { done <- f.Fence(ctx) }()
	select {
	case err := <-done:
		t.Fatalf("fence drained while r2 still holds admission: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	r2()
	require.NoError(t, <-done)
}

func TestFence_ConcurrentAdmissionsAllAdmitted(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := NewFence()
	releases := make([]func(), 0, 8)
	for range 8 {
		release, err := f.AdmitShared(ctx)
		require.NoError(t, err)
		releases = append(releases, release)
	}
	for _, release := range releases {
		release()
	}
	require.NoError(t, f.Fence(ctx))
}

// TestFinishFencesAndDrainsLateRunner proves the terminalization
// seam end to end: exclusive admission closes before the store
// commits, the commit waits for the in-flight child tool call to
// release, late admissions are rejected, and the runner's own settle
// cannot publish a second terminal transition.
func TestFinishFencesAndDrainsLateRunner(t *testing.T) {
	m, _ := newTestManagerStore(t, Limits{})
	rec := record(m)

	type gate struct {
		fence   *Fence
		release func()
	}
	gateCh := make(chan gate, 1)
	runnerDone := make(chan struct{})
	defer func() {
		select {
		case <-runnerDone:
		default:
			close(runnerDone)
		}
	}()
	run := func(ctx context.Context, h *Handle) (Result, error) {
		// One held shared admission stands in for one in-flight
		// child tool invocation; the release is owned by the test.
		release, err := h.Fence().AdmitShared(ctx)
		if err != nil {
			return Result{}, err
		}
		gateCh <- gate{fence: h.Fence(), release: release}
		<-runnerDone
		return Result{Text: "late result"}, nil
	}
	task, err := m.Start(t.Context(), StartRequest{
		CallerSessionID: "owner", Profile: "coder", Provider: "p", Model: "m",
		ChildSessionID: uniqueChild(), ChildTitle: "child", Prompt: "work", Run: run,
	})
	require.NoError(t, err)
	g := <-gateCh

	m.mu.Lock()
	at := m.live[task.ID]
	m.mu.Unlock()
	require.NotNil(t, at)

	finished := make(chan struct{})
	go func() {
		m.finish(t.Context(), at, TerminalUpdate{
			Status: StatusFailed, Summary: "fence-win", CompletedAt: time.Now(),
		})
		close(finished)
	}()

	// Exclusive admission closes before any state change, so fresh
	// admissions are rejected while the commit still waits.
	require.Eventually(t, func() bool {
		_, err := g.fence.AdmitShared(context.Background())
		return errors.Is(err, ErrRunFenced)
	}, 5*time.Second, 5*time.Millisecond, "admission must close before the commit")
	select {
	case <-finished:
		t.Fatal("terminalization committed before the held admission drained")
	default:
	}

	// The in-flight call releases; only the fenced terminalization
	// proceeds (the runner itself is still parked in its "tool").
	g.release()
	g.release() // release is idempotent
	<-finished

	got := waitStatus(t, m, "owner", task.ID, StatusFailed)
	require.Equal(t, "fence-win", got.Summary)

	_, err = g.fence.AdmitShared(context.Background())
	require.ErrorIs(t, err, ErrRunFenced, "a late runner cannot invoke another tool")

	// The runner settles only after the terminal commit and its
	// second terminalization loses exactly-once.
	close(runnerDone)
	require.Eventually(t, func() bool {
		m.mu.Lock()
		_, live := m.live[task.ID]
		m.mu.Unlock()
		return !live
	}, 5*time.Second, 5*time.Millisecond, "the attempt must settle after the runner returns")
	require.Equal(t, 1, rec.count(EventFailed), "exactly one terminal event")
	got, err = m.Status(context.Background(), "owner", task.ID)
	require.NoError(t, err)
	require.Equal(t, "fence-win", got.Summary, "the late settle must not overwrite the terminal record")
}
