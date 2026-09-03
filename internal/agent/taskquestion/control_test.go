package taskquestion

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/question"
	"github.com/stretchr/testify/require"
)

func TestAnswerTask_AuthorizesOwnerOnly(t *testing.T) {
	t.Parallel()
	h := newHarness()
	ch, qid := h.ask(t, askRequest("task-1"))

	require.ErrorIs(t, h.svc.AnswerTask("intruder", qid, answer("q", true)), ErrNotOwner)
	require.ErrorIs(t, h.svc.AnswerTask("owner-1", "nope", nil), ErrNotFound)

	_, ok := h.svc.Pending(qid)
	require.True(t, ok, "rejected attempts must not resolve")

	require.NoError(t, h.svc.AnswerTask("owner-1", qid, answer("q", true)))
	require.NoError(t, (<-ch).err)
}

func TestAnswerTask_DuplicateLosesSecond(t *testing.T) {
	t.Parallel()
	h := newHarness()
	ch, qid := h.ask(t, askRequest("task-1"))

	first := answer("q", true)
	require.NoError(t, h.svc.AnswerTask("owner-1", qid, first))
	require.ErrorIs(t, h.svc.AnswerTask("owner-1", qid, answer("q", false)), ErrAlreadyResolved)

	got := <-ch
	require.Equal(t, first, got.answers, "first committed answer wins")
}

func TestCancelTask_WakesRunnerWithCancelled(t *testing.T) {
	t.Parallel()
	h := newHarness()
	ch, qid := h.ask(t, askRequest("task-1"))

	require.ErrorIs(t, h.svc.CancelTask("intruder", qid), ErrNotOwner)
	require.NoError(t, h.svc.CancelTask("owner-1", qid))
	require.ErrorIs(t, h.svc.AnswerTask("owner-1", qid, answer("q", true)), ErrAlreadyResolved)

	got := <-ch
	require.ErrorIs(t, got.err, question.ErrCancelled)
	require.Nil(t, got.answers)
}

func TestShutdown_InterruptsWaitersAndRejectsNewAsks(t *testing.T) {
	t.Parallel()
	h := newHarness()
	ch, qid := h.ask(t, askRequest("task-1"))

	h.svc.Shutdown()
	h.svc.Shutdown() // idempotent

	got := <-ch
	require.ErrorIs(t, got.err, ErrShuttingDown)
	_, err := h.svc.AskTask(t.Context(), askRequest("task-2"))
	require.ErrorIs(t, err, ErrShuttingDown)
	require.ErrorIs(t, h.svc.AnswerTask("owner-1", qid, answer("q", true)), ErrAlreadyResolved)
}

func TestAnswerCancelRace_ExactlyOneWins(t *testing.T) {
	t.Parallel()
	h := newHarness()
	ch, qid := h.ask(t, askRequest("task-1"))

	const racers = 8
	errs := make([]error, racers)
	var wg sync.WaitGroup
	for i := range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if i%2 == 0 {
				errs[i] = h.svc.AnswerTask("owner-1", qid, answer("q", true))
			} else {
				errs[i] = h.svc.CancelTask("owner-1", qid)
			}
		}()
	}
	wg.Wait()

	var wins int
	for _, err := range errs {
		if err == nil {
			wins++
			continue
		}
		require.ErrorIs(t, err, ErrAlreadyResolved)
	}
	require.Equal(t, 1, wins, "the first committed resolution wins exactly once")

	got := <-ch
	if got.err != nil {
		require.ErrorIs(t, got.err, question.ErrCancelled)
	} else {
		require.Len(t, got.answers, 1)
		require.Equal(t, qid, <-h.resumed)
	}
}

// fakeRepo records durable mirror traffic for seam tests.
type fakeRepo struct {
	mu       sync.Mutex
	saved    []TaskQuestion
	resolved map[string]ResolutionUpdate
	saveErr  error
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{resolved: map[string]ResolutionUpdate{}}
}

func (r *fakeRepo) Save(_ context.Context, q TaskQuestion) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.saveErr != nil {
		return r.saveErr
	}
	r.saved = append(r.saved, q)
	return nil
}

func (r *fakeRepo) Resolve(_ context.Context, id string, u ResolutionUpdate) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, done := r.resolved[id]; done {
		return false, nil
	}
	r.resolved[id] = u
	return true, nil
}

// ListUnresolved mirrors the SQLite repository's pending-only view
// over the saved records.
func (r *fakeRepo) ListUnresolved(context.Context) ([]TaskQuestion, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []TaskQuestion
	for _, q := range r.saved {
		if _, done := r.resolved[q.QuestionID]; !done {
			out = append(out, q)
		}
	}
	return out, nil
}

// ListUnresolvedForOwner filters the pending view to one owner,
// mirroring the SQLite repository's owner-scoped resync read.
func (r *fakeRepo) ListUnresolvedForOwner(ctx context.Context, ownerSessionID string) ([]TaskQuestion, error) {
	all, err := r.ListUnresolved(ctx)
	if err != nil {
		return nil, err
	}
	var out []TaskQuestion
	for _, q := range all {
		if q.OwnerSessionID == ownerSessionID {
			out = append(out, q)
		}
	}
	return out, nil
}

func TestServiceMirrorsLifecycleToRepository(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	svc := NewService(Config{Repo: repo})

	done := make(chan askResult, 1)
	go func() {
		a, err := svc.AskTask(context.Background(), askRequest("task-1"))
		done <- askResult{a, err}
	}()

	var qid string
	require.Eventually(t, func() bool {
		un := svc.Unresolved()
		if len(un) == 0 {
			return false
		}
		qid = un[0].QuestionID
		return true
	}, 2*time.Second, 5*time.Millisecond)

	want := answer("q", true)
	require.NoError(t, svc.AnswerTask("owner-1", qid, want))
	require.NoError(t, (<-done).err)

	repo.mu.Lock()
	defer repo.mu.Unlock()
	require.Len(t, repo.saved, 1)
	require.Equal(t, ResolutionPending, repo.saved[0].Resolution, "registration mirrors a pending row")
	got := repo.resolved[qid]
	require.Equal(t, ResolutionAnswered, got.Resolution, "resolution mirrors conditionally")
	require.Equal(t, want, got.Answers)
}

func TestService_SaveFailureForgetsWaiter(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	repo.saveErr = errors.New("disk on fire")
	svc := NewService(Config{Repo: repo})

	_, err := svc.AskTask(t.Context(), askRequest("task-1"))
	require.ErrorContains(t, err, "disk on fire")
	require.Empty(t, svc.Unresolved(), "failed durable write must leave no tracked waiter")
}

func TestInterruptStale_ResolvedOnceAndSkipsLiveWaiters(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := newFakeRepo()
	// A previous process died with this question unanswered.
	require.NoError(t, repo.Save(ctx, TaskQuestion{
		QuestionID: "stale", TaskID: "t-old", OwnerSessionID: "owner",
		Resolution: ResolutionPending,
	}))
	svc := NewService(Config{Repo: repo})

	// A live waiter's question must survive the sweep.
	done := make(chan askResult, 1)
	go func() {
		a, err := svc.AskTask(ctx, askRequest("task-1"))
		done <- askResult{a, err}
	}()
	var liveID string
	require.Eventually(t, func() bool {
		un := svc.Unresolved()
		if len(un) == 0 {
			return false
		}
		liveID = un[0].QuestionID
		return true
	}, 2*time.Second, 5*time.Millisecond)

	n, err := svc.InterruptStale(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, n, "only the durable pending row from the dead process is swept")
	repo.mu.Lock()
	sweep := repo.resolved["stale"]
	_, liveTouched := repo.resolved[liveID]
	repo.mu.Unlock()
	require.Equal(t, ResolutionInterrupted, sweep.Resolution)
	require.False(t, liveTouched, "a live waiter is never interrupted by recovery")

	// A second pass adds nothing: the conditional resolve already lost.
	n, err = svc.InterruptStale(ctx)
	require.NoError(t, err)
	require.Zero(t, n)

	require.NoError(t, svc.AnswerTask("owner-1", liveID, answer("q", true)))
	require.NoError(t, (<-done).err)
}
