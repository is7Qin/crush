package app

import (
	"testing"

	"github.com/charmbracelet/crush/internal/agent/task"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/stretchr/testify/require"
)

func TestTaskLimits(t *testing.T) {
	t.Parallel()

	require.Equal(t, task.Limits{RunningPerModel: config.DefaultRunningTasksPerModel},
		taskLimits(nil), "absent options still get the bounded model default")

	o := &config.Options{
		LiveTasksPerParent:    2,
		LiveTasksPerWorkspace: 5,
	}
	require.Equal(t, task.Limits{
		LiveTasksPerParent:    2,
		LiveTasksPerWorkspace: 5,
		RunningPerModel:       config.DefaultRunningTasksPerModel,
	}, taskLimits(o), "unset model cap falls back to 10")

	seven := 7
	o.RunningTasksPerModel = &seven
	require.Equal(t, task.Limits{
		LiveTasksPerParent:    2,
		LiveTasksPerWorkspace: 5,
		RunningPerModel:       7,
	}, taskLimits(o), "configured model cap is honored")
}
