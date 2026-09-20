package tools

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestExclusiveTool_BashBypassesHeldWriteLease pins the structural fix:
// a bash invocation must proceed while another session holds the
// workspace write lease, so a long-running command never starves
// legitimate concurrency into a spurious lease timeout.
func TestExclusiveTool_BashBypassesHeldWriteLease(t *testing.T) {
	reg := NewLeaseRegistry()
	key := t.TempDir()

	held, err := reg.AcquireExclusive(context.Background(), key)
	require.NoError(t, err)
	t.Cleanup(held)

	inner := newRecordingTool("bash")
	t.Cleanup(func() {
		select {
		case <-inner.release:
		default:
			close(inner.release)
		}
	})
	tool := NewExclusiveTool(inner, key, reg)

	res := make(chan error, 1)
	go func() {
		_, err := tool.Run(context.Background(), callAs("bash"))
		res <- err
	}()

	select {
	case <-inner.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("bash must not wait for the workspace write lease")
	}
	close(inner.release)
	require.NoError(t, <-res)
}
