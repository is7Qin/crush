package app

import (
	"testing"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/stretchr/testify/require"
)

// TestTaskAutoContinue_DefaultOff pins the delivery default:
// report delivery never starts a parent turn unless the user opts
// in with task_auto_continue.
func TestTaskAutoContinue_DefaultOff(t *testing.T) {
	t.Parallel()
	require.False(t, taskAutoContinue(nil))
	require.False(t, taskAutoContinue(&config.Options{}))
	require.True(t, taskAutoContinue(&config.Options{TaskAutoContinue: true}))
}
