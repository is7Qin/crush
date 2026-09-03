package app

import (
	"context"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/agent/task"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestShutdownForTest_SettlesTaskManagerBeforeEventTeardown locks the
// review finding that ShutdownForTest skipped taskManager.Shutdown: a
// runner blocked until cancellation must be cancelled and settled, and
// its terminal event published onto the task stream, while the event
// brokers are still alive; after teardown the manager must refuse new
// admissions. Reverting the settle step leaves the attempt live and
// the post-shutdown Start succeeds, failing both assertions.
func TestShutdownForTest_SettlesTaskManagerBeforeEventTeardown(t *testing.T) {
	t.Parallel()

	app := NewForTest(context.Background())

	// Subscribe before teardown so the terminal event is buffered at
	// publish time and the read below cannot race the broker shutdown.
	// t.Context() releases the subscription when the test ends.
	watched := app.taskEvents.Subscribe(t.Context())

	dispatched := make(chan struct{})
	rec, err := app.Tasks().Start(context.Background(), task.StartRequest{
		CallerSessionID: "owner",
		Prompt:          "work",
		Provider:        "p",
		Model:           "m",
		ChildSessionID:  uuid.NewString(),
		Run: func(ctx context.Context, _ *task.Handle) (task.Result, error) {
			close(dispatched)
			<-ctx.Done()
			return task.Result{}, ctx.Err()
		},
	})
	require.NoError(t, err)

	select {
	case <-dispatched:
	case <-time.After(10 * time.Second):
		t.Fatal("task runner was never dispatched")
	}

	app.ShutdownForTest()

	// The manager settled the blocked attempt during ShutdownForTest:
	// its terminal event is already on the task stream.
	deadline := time.After(5 * time.Second)
	for settled := false; !settled; {
		select {
		case ev := <-watched:
			settled = ev.Payload.Task != nil &&
				ev.Payload.Task.ID == rec.ID &&
				task.Status(ev.Payload.Type).Terminal()
		case <-deadline:
			t.Fatal("no terminal task event before the brokers were torn down")
		}
	}

	// The manager is closed: no new admissions after teardown.
	_, err = app.Tasks().Start(context.Background(), task.StartRequest{
		CallerSessionID: "owner",
		Prompt:          "work",
		Provider:        "p",
		Model:           "m",
		ChildSessionID:  uuid.NewString(),
		Run: func(context.Context, *task.Handle) (task.Result, error) {
			return task.Result{}, nil
		},
	})
	require.ErrorIs(t, err, task.ErrShuttingDown)

	// Safe to call again (idempotent settle).
	require.NotPanics(t, app.ShutdownForTest)
}
