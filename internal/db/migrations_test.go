package db

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestConnect_AppliesAgentTaskMigrations confirms Connect runs the
// agent task schema automatically: the task table, the durable
// outbox table, and the child mailbox table exist without any
// manual migration step.
func TestConnect_AppliesAgentTaskMigrations(t *testing.T) {
	t.Cleanup(ResetPool)

	dataDir := t.TempDir()
	conn, err := Connect(context.Background(), dataDir)
	require.NoError(t, err)

	for _, table := range []string{"agent_tasks", "agent_task_outbox", "agent_task_messages"} {
		var name string
		err := conn.QueryRowContext(context.Background(),
			`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, table,
		).Scan(&name)
		require.NoErrorf(t, err, "table %q missing after migrations", table)
		require.Equal(t, table, name)
	}

	// The dispatch columns and the child-generation uniqueness
	// index from the mailbox migration are present.
	for _, column := range []string{
		"parent_message_id", "tool_call_id", "profile_generation",
		"requested_model", "prompt_fingerprint", "tool_fingerprint",
		"message_id", "terminal_generation", "cost_aggregated_generation",
		"updated_at",
	} {
		var notnull int
		err := conn.QueryRowContext(context.Background(),
			`SELECT COUNT(*) FROM pragma_table_info('agent_tasks') WHERE name = ?`, column,
		).Scan(&notnull)
		require.NoError(t, err)
		require.Equalf(t, 1, notnull, "agent_tasks column %q missing", column)
	}
	var indexed int
	err = conn.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = 'idx_agent_tasks_child_generation'`,
	).Scan(&indexed)
	require.NoError(t, err)
	require.Equal(t, 1, indexed, "child generation uniqueness index missing")

	require.NoError(t, Release(dataDir))
}
