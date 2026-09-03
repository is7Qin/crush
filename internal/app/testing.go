package app

import (
	"context"
	"log/slog"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/crush/internal/agent/notify"
	"github.com/charmbracelet/crush/internal/agent/task"
	"github.com/charmbracelet/crush/internal/agent/taskquestion"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/charmbracelet/crush/internal/question"
)

// NewForTest constructs a minimal [App] suitable for in-process tests
// that need a working event broker and permission service without
// booting a real config, database, LSP, MCP, or agent coordinator.
//
// The returned App has:
//
//   - A live `events` broker that [App.SendEvent] publishes to and
//     [App.Events] subscribes from.
//   - A real [permission.Service] whose request and notification
//     brokers are fanned into the events broker, so subscribers to
//     [App.Events] observe the same permission events the production
//     wiring would deliver to SSE clients.
//   - An [App.agentNotifications] broker.
//
// The caller owns lifetime: cancel ctx (or call [App.Shutdown]) to
// tear down the fan-in goroutines and the events broker.
func NewForTest(ctx context.Context) *App {
	app := &App{
		Permissions:        permission.NewPermissionService("", false, nil),
		Questions:          question.NewService(),
		globalCtx:          ctx,
		events:             pubsub.NewBroker[tea.Msg](),
		serviceEventsWG:    &sync.WaitGroup{},
		tuiWG:              &sync.WaitGroup{},
		agentNotifications: pubsub.NewBroker[notify.Notification](),
		runCompletions:     pubsub.NewBroker[notify.RunComplete](),
		taskEvents:         pubsub.NewBroker[task.Event](),
	}
	// A task manager over the in-memory store and the in-memory
	// task-question service give test apps the same task transport
	// surface production wires: lifecycle events bridge onto
	// taskEvents and terminal events drain the durable outbox (no
	// inbox is wired here), while both brokers fan into the shared
	// events stream exactly like [App.setupEvents].
	app.taskManager = task.New(ctx, task.Config{
		WorkspaceID: "test",
		Store:       task.NewMemoryStore(),
	})
	app.taskManager.Subscribe(app.handleTaskEvent)
	app.taskOutbox = task.NewOutboxNotifier(app.taskManager, app.taskEvents)
	app.taskQuestions = taskquestion.NewService(taskquestion.Config{})

	eventsCtx, cancel := context.WithCancel(ctx)
	app.eventsCtx = eventsCtx
	app.subscribeMustDeliver(eventsCtx, "permissions",
		app.Permissions.Subscribe)
	app.subscribeMustDeliver(eventsCtx, "permissions-notifications",
		app.Permissions.SubscribeNotifications)
	app.subscribeMustDeliver(eventsCtx, "question-batches",
		app.Questions.Subscribe)
	app.subscribeMustDeliver(eventsCtx, "question-notifications",
		app.Questions.SubscribeNotifications)
	app.subscribe(eventsCtx, "agent-notifications",
		app.agentNotifications.Subscribe)
	app.subscribe(eventsCtx, "run-completions",
		app.runCompletions.Subscribe)
	app.subscribeMustDeliver(eventsCtx, "task-events",
		app.taskEvents.Subscribe)
	app.subscribeMustDeliver(eventsCtx, "taskquestion-batches",
		app.taskQuestions.Subscribe)
	app.subscribeMustDeliver(eventsCtx, "taskquestion-notifications",
		app.taskQuestions.SubscribeNotifications)
	// The parent-idle inbox watcher rides the same lifetime as the
	// fan-in above. No taskInbox is wired here, so it stays a no-op
	// until a test injects one onto app.taskInbox.
	app.watchParentIdle()
	app.cleanupFuncs = append(app.cleanupFuncs, func(context.Context) error {
		cancel()
		app.serviceEventsWG.Wait()
		app.events.Shutdown()
		return nil
	})
	return app
}

// ShutdownForTest tears down the App's task pipeline, event broker,
// and fan-in goroutines. It is safe to call multiple times.
//
// Use this in tests instead of [App.Shutdown], which drives a full
// production shutdown path (database release, LSP teardown, MCP
// shutdown) that synthetic test apps cannot satisfy.
func (app *App) ShutdownForTest() {
	// Mirror the production teardown order from [App.Shutdown]:
	// interrupt pending task questions, then settle the task manager,
	// so cancellation and terminalization events still reach the event
	// brokers before the cleanup funcs below cancel the fan-in and shut
	// the brokers down.
	if app.taskQuestions != nil {
		app.taskQuestions.Shutdown()
	}
	if app.taskManager != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := app.taskManager.Shutdown(shutdownCtx); err != nil {
			slog.Error("Task manager shutdown did not settle all tasks", "error", err)
		}
	}
	for _, cleanup := range app.cleanupFuncs {
		if cleanup != nil {
			_ = cleanup(context.Background())
		}
	}
	app.cleanupFuncs = nil
}
