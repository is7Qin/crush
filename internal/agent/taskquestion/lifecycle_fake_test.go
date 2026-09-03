package taskquestion

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fakeLifecycle records the ordered seam traffic so tests can pin
// persistence ordering (BeginWait before publish, Resolve before
// wake, Resume after Resolve) and exactly-once resolution.
type fakeLifecycle struct {
	mu       sync.Mutex
	order    []string // "begin:<id>" | "resolve:<id>:<kind>" | "resume:<id>"
	resolved map[string]ResolutionUpdate
	beginErr error
	resErr   error
	resumeEr error
}

func newFakeLifecycle() *fakeLifecycle {
	return &fakeLifecycle{resolved: map[string]ResolutionUpdate{}}
}

func (l *fakeLifecycle) record(step string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.order = append(l.order, step)
}

func (l *fakeLifecycle) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.order...)
}

func (l *fakeLifecycle) count(prefix string) int {
	n := 0
	for _, s := range l.snapshot() {
		if strings.HasPrefix(s, prefix) {
			n++
		}
	}
	return n
}

func (l *fakeLifecycle) BeginWait(_ context.Context, q TaskQuestion) error {
	l.record("begin:" + q.QuestionID)
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.beginErr
}

func (l *fakeLifecycle) Resolve(_ context.Context, q TaskQuestion, u ResolutionUpdate) error {
	l.record("resolve:" + q.QuestionID + ":" + string(u.Resolution))
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.resErr != nil {
		return l.resErr
	}
	l.resolved[q.QuestionID] = u
	return nil
}

func (l *fakeLifecycle) Resume(_ context.Context, q TaskQuestion) error {
	l.record("resume:" + q.QuestionID)
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.resumeEr
}

// newLifecycleService wires a service to a fresh fake lifecycle.
func newLifecycleService() (*taskQuestionService, *fakeLifecycle) {
	life := newFakeLifecycle()
	svc := NewService(Config{Lifecycle: life})
	return svc, life
}

// askLifecycle starts AskTask and returns once the batch has been
// published (which implies BeginWait committed) and the question is
// tracked.
func askLifecycle(t *testing.T, svc *taskQuestionService, req TaskQuestionRequest) (<-chan askResult, string) {
	t.Helper()
	events := svc.Subscribe(t.Context())
	ch := make(chan askResult, 1)
	go func() {
		a, err := svc.AskTask(context.Background(), req)
		ch <- askResult{answers: a, err: err}
	}()
	select {
	case <-events:
	case <-time.After(2 * time.Second):
		t.Fatal("AskTask never published a committed question")
	}
	var qid string
	require.Eventually(t, func() bool {
		un := svc.Unresolved()
		if len(un) == 0 {
			return false
		}
		qid = un[0].QuestionID
		return true
	}, 2*time.Second, 5*time.Millisecond)
	return ch, qid
}
