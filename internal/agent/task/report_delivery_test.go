package task

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// terminalInboxAnchor seeds s with one terminalized task carrying
// the given anchor and returns its pending inbox row.
func terminalInboxAnchor(t *testing.T, s *MemoryStore, id, child, owner string, anchor *ReportAnchor) *InboxEntry {
	t.Helper()
	ctx := t.Context()
	_, err := s.CreatePendingTask(ctx, admissionForWith(id, child, owner))
	require.NoError(t, err)
	_, found, err := s.DispatchNextChildMessage(ctx, child)
	require.NoError(t, err)
	require.True(t, found)
	_, won, err := s.TerminalizeAndDeliver(ctx, id, 1, TerminalUpdate{
		Status: StatusCompleted, Result: "answer", CompletedAt: time.Now(), Anchor: anchor,
	})
	require.NoError(t, err)
	require.True(t, won)
	entries, err := s.ListInbox(ctx, owner)
	require.NoError(t, err)
	for _, e := range entries {
		if e.TaskID == id {
			return e
		}
	}
	t.Fatalf("no inbox row for %s", id)
	return nil
}

// anchoredFile writes content to a temp file and returns its anchor
// as the view tool would record it.
func anchoredFile(t *testing.T, content string) (string, *ReportAnchor) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "anchored.txt")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	sha, lines := anchoredHash(content)
	return path, &ReportAnchor{Path: path, SHA256_8: sha, Lines: lines}
}

// anchoredHash computes the anchor of content with the same
// algorithm the view tool records and the drainer checks, so the
// test pins the shared contract rather than one side of it.
func anchoredHash(content string) (string, int) {
	sum := sha256.Sum256([]byte(content))
	sha := hex.EncodeToString(sum[:])[:8]
	if content == "" {
		return sha, 0
	}
	lines := strings.Count(content, "\n")
	if !strings.HasSuffix(content, "\n") {
		lines++
	}
	return sha, lines
}

func readyGate() *fakeGate {
	g := &fakeGate{}
	g.ready.Store(true)
	return g
}

// TestReportDelivery_ReadThenDeliverSilent is acceptance criterion 1:
// a parent that pulls a report through agent_output (Manager.Output)
// receives no pushed copy, the drainer reports zero deliveries, and
// the pending set is empty.
func TestReportDelivery_ReadThenDeliverSilent(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	m, s := newTestManagerStore(t, Limits{})
	task := completeTask(t, m, "owner", "secret body")

	pulled, _, err := m.Output(ctx, "owner", task.ID)
	require.NoError(t, err)
	require.Equal(t, "secret body", pulled.Text)

	writer := &fakeWriter{}
	n, err := NewInboxDrainer(s, readyGate(), writer).Drain(ctx, "owner")
	require.NoError(t, err)
	require.Zero(t, n, "a pulled report must not push")
	require.Empty(t, writer.snapshot())

	pending, err := s.ListInbox(ctx, "owner")
	require.NoError(t, err)
	require.Empty(t, pending)
	count, err := m.PendingCount(ctx, "owner")
	require.NoError(t, err)
	require.Zero(t, count)

	n, err = NewInboxDrainer(s, readyGate(), writer).Drain(ctx, "owner")
	require.NoError(t, err)
	require.Zero(t, n)
}

// TestReportDelivery_UnreadDeliversExactlyOnce is acceptance
// criterion 2: an unread report delivers once, and a second drain
// delivers nothing.
func TestReportDelivery_UnreadDeliversExactlyOnce(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := NewMemoryStore()
	terminalInbox(t, s, "t1", "child-1", "owner")
	writer := &fakeWriter{}

	n, err := NewInboxDrainer(s, readyGate(), writer).Drain(ctx, "owner")
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.Equal(t, 1, writer.batches, "one drain window is one message")
	require.Len(t, writer.snapshot(), 1)

	n, err = NewInboxDrainer(s, readyGate(), writer).Drain(ctx, "owner")
	require.NoError(t, err)
	require.Zero(t, n, "a delivered report is never pushed again")
	require.Len(t, writer.snapshot(), 1)
}

// TestReportDelivery_StaleComputedAtDelivery is acceptance criterion
// 3: a modified anchored file delivers stale:true, an unmodified
// file stale:false, a report with no anchor reports no anchor and
// stale:false, and an unreadable anchored path does not fail
// delivery.
func TestReportDelivery_StaleComputedAtDelivery(t *testing.T) {
	t.Parallel()
	t.Run("unmodified", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		s := NewMemoryStore()
		_, anchor := anchoredFile(t, "line one\nline two\n")
		terminalInboxAnchor(t, s, "t1", "child-1", "owner", anchor)
		writer := &fakeWriter{}
		n, err := NewInboxDrainer(s, readyGate(), writer).Drain(ctx, "owner")
		require.NoError(t, err)
		require.Equal(t, 1, n)
		env := writer.snapshot()[0]
		require.False(t, env.Stale)
		require.Contains(t, env.Render(), "stale: false")
	})
	t.Run("modified", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		s := NewMemoryStore()
		path, anchor := anchoredFile(t, "line one\nline two\n")
		terminalInboxAnchor(t, s, "t1", "child-1", "owner", anchor)
		require.NoError(t, os.WriteFile(path, []byte("line one\nline two\nline three\n"), 0o644))
		writer := &fakeWriter{}
		n, err := NewInboxDrainer(s, readyGate(), writer).Drain(ctx, "owner")
		require.NoError(t, err)
		require.Equal(t, 1, n)
		env := writer.snapshot()[0]
		require.True(t, env.Stale, "the anchored revision no longer matches")
		require.Contains(t, env.Render(), "stale: true")
	})
	t.Run("no anchor", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		s := NewMemoryStore()
		terminalInboxAnchor(t, s, "t1", "child-1", "owner", nil)
		writer := &fakeWriter{}
		n, err := NewInboxDrainer(s, readyGate(), writer).Drain(ctx, "owner")
		require.NoError(t, err)
		require.Equal(t, 1, n)
		env := writer.snapshot()[0]
		require.False(t, env.Stale, "absence is expressed by anchor presence, never by stale")
		require.Contains(t, env.Render(), "anchor: none")
		require.Contains(t, env.Render(), "stale: false")
	})
	t.Run("unreadable", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		s := NewMemoryStore()
		dir := t.TempDir()
		terminalInboxAnchor(t, s, "t1", "child-1", "owner",
			&ReportAnchor{Path: dir, SHA256_8: "deadbeef", Lines: 3})
		writer := &fakeWriter{}
		n, err := NewInboxDrainer(s, readyGate(), writer).Drain(ctx, "owner")
		require.NoError(t, err, "a stat or read failure never fails the delivery")
		require.Equal(t, 1, n)
		require.False(t, writer.snapshot()[0].Stale)
	})
}

// TestReportDelivery_ContinuationOncePerBatch is acceptance criterion
// 4: no continuation is configured by default so delivery starts no
// turn, and with auto-continuation enabled one drain of N pending
// reports produces exactly one continuation, not N.
func TestReportDelivery_ContinuationOncePerBatch(t *testing.T) {
	t.Parallel()
	t.Run("default starts no turn", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		s := NewMemoryStore()
		for i, child := range []string{"child-1", "child-2", "child-3"} {
			terminalInbox(t, s, string(rune('t'+i)), child, "owner")
		}
		writer := &fakeWriter{}
		n, err := NewInboxDrainer(s, readyGate(), writer).Drain(ctx, "owner")
		require.NoError(t, err)
		require.Equal(t, 3, n)
		require.Equal(t, 1, writer.batches)
	})
	t.Run("enabled continues exactly once", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		s := NewMemoryStore()
		for i, child := range []string{"child-1", "child-2", "child-3"} {
			terminalInbox(t, s, string(rune('t'+i)), child, "owner")
		}
		writer := &fakeWriter{}
		var continued atomic.Int64
		d := NewInboxDrainer(s, readyGate(), writer, func(_ context.Context, _ string) error {
			continued.Add(1)
			return nil
		})
		n, err := d.Drain(ctx, "owner")
		require.NoError(t, err)
		require.Equal(t, 3, n)
		require.Equal(t, int64(1), continued.Load(), "at most one continuation for the batch")
	})
	t.Run("empty drain continues never", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		s := NewMemoryStore()
		writer := &fakeWriter{}
		var continued atomic.Int64
		d := NewInboxDrainer(s, readyGate(), writer, func(_ context.Context, _ string) error {
			continued.Add(1)
			return nil
		})
		n, err := d.Drain(ctx, "owner")
		require.NoError(t, err)
		require.Zero(t, n)
		require.Zero(t, continued.Load())
	})
}

// fiveFindingEnvelope builds an ordinary 5-finding report for the
// pushed-form budget check.
func fiveFindingEnvelope() TaskResultEnvelope {
	env := TaskResultEnvelope{
		TaskID: "t1", ChildSessionID: "c1", Profile: "coder",
		RunGeneration: 1, Status: StatusCompleted, Summary: "done",
		Verdict:   VerdictUnknown,
		Counts:    FindingCounts{Blocking: 1, Notable: 2, Info: 2},
		OutputRef: "t1",
		ArtifactAnchor: &ReportAnchor{
			Path: "/repo/main.go", SHA256_8: "abcdef12", Lines: 400,
		},
		BlindSpots: []string{"did not check the vendored tree", "no network access to the registry"},
	}
	for i := range 5 {
		env.Findings = append(env.Findings, Finding{
			Severity:   SeverityNotable,
			Claim:      "finding claim number",
			Evidence:   []FindingLocation{{File: "main.go", Line: 10 + i, LineEnd: 12 + i}},
			Confidence: ConfidenceHigh,
		})
	}
	return env
}

// TestReportDelivery_PushedFormCompact is acceptance criterion 5:
// the pushed form of an ordinary task carries no result body,
// carries counts (including blocking and omitted), per-finding
// severity and confidence, verdict, and blind spots, stays within
// the 2 KiB budget for a 5-finding report, and still carries the
// bounded body for hidden-profile reports.
func TestReportDelivery_PushedFormCompact(t *testing.T) {
	t.Parallel()
	rendered := fiveFindingEnvelope().Render()
	require.NotContains(t, rendered, "result:", "the pushed form never carries the body")
	require.Contains(t, rendered, "verdict: unknown")
	require.Contains(t, rendered, "counts: blocking=1 notable=2 info=2 omitted=0")
	require.Contains(t, rendered, "notable|high")
	require.Contains(t, rendered, "blind_spots:")
	require.Contains(t, rendered, "anchor: path=/repo/main.go sha256_8=abcdef12 lines=400")
	require.Contains(t, rendered, "output_ref: t1")
	require.LessOrEqual(t, len(rendered), MaxPushedBytes)

	// Findings past the cap are dropped and counted, not silent.
	many := fiveFindingEnvelope()
	for range 7 {
		many.Findings = append(many.Findings, Finding{
			Severity: SeverityInfo, Claim: "extra", Confidence: ConfidenceLow,
		})
	}
	capped := many.Render()
	require.Contains(t, capped, "omitted=2")
	require.Equal(t, 10, strings.Count(capped, "- ["), "only the first 10 findings render")
	require.LessOrEqual(t, len(capped), MaxPushedBytes)

	// Hidden-profile reports still carry their bounded result body.
	hidden := TaskResultEnvelope{
		Profile: "agentic_fetch_internal", TaskID: "h1", Status: StatusCompleted,
		Result: "fetched body", OutputRef: "h1",
	}
	hiddenRendered := hidden.Render()
	require.Contains(t, hiddenRendered, "fetched body")
	require.NotContains(t, hiddenRendered, "agent_output")
}

// TestReportDelivery_CoalescedBatch is acceptance criterion 6: two
// reports sharing an anchor deliver as one message with a single
// anchor header, and two reports with different anchors completing
// in the same drain window also deliver as one message carrying one
// header per distinct anchor.
func TestReportDelivery_CoalescedBatch(t *testing.T) {
	t.Parallel()
	t.Run("same anchor", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		s := NewMemoryStore()
		_, anchor := anchoredFile(t, "shared\n")
		terminalInboxAnchor(t, s, "t1", "child-1", "owner", anchor)
		terminalInboxAnchor(t, s, "t2", "child-2", "owner",
			&ReportAnchor{Path: anchor.Path, SHA256_8: anchor.SHA256_8, Lines: 999})
		writer := &fakeWriter{}
		n, err := NewInboxDrainer(s, readyGate(), writer).Drain(ctx, "owner")
		require.NoError(t, err)
		require.Equal(t, 2, n)
		require.Equal(t, 1, writer.batches, "one drain window is ONE message")
		batch := RenderBatch(writer.snapshot(), 0)
		headers, _, _ := strings.Cut(batch, "<untrusted-agent-result>")
		require.Equal(t, 1, strings.Count(headers, "anchor: path="),
			"lines does not participate in anchor equality")
	})
	t.Run("distinct anchors", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		s := NewMemoryStore()
		_, first := anchoredFile(t, "first\n")
		secondPath := first.Path + ".other"
		require.NoError(t, os.WriteFile(secondPath, []byte("second\n"), 0o644))
		sha, lines := anchoredHash("second\n")
		terminalInboxAnchor(t, s, "t1", "child-1", "owner", first)
		terminalInboxAnchor(t, s, "t2", "child-2", "owner",
			&ReportAnchor{Path: secondPath, SHA256_8: sha, Lines: lines})
		writer := &fakeWriter{}
		n, err := NewInboxDrainer(s, readyGate(), writer).Drain(ctx, "owner")
		require.NoError(t, err)
		require.Equal(t, 2, n)
		require.Equal(t, 1, writer.batches)
		batch := RenderBatch(writer.snapshot(), 0)
		headers, _, _ := strings.Cut(batch, "<untrusted-agent-result>")
		require.Equal(t, 2, strings.Count(headers, "anchor: path="),
			"one shared header per distinct anchor")
	})
}

// markBlocking rewrites one pending inbox row's stored envelope so
// it carries counts.blocking = 1, pinning drain selection without a
// structured findings producer.
func markBlocking(t *testing.T, s *MemoryStore, taskID string) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.inbox {
		if e.TaskID != taskID {
			continue
		}
		var env TaskResultEnvelope
		require.NoError(t, json.Unmarshal([]byte(e.Payload), &env))
		env.Counts.Blocking = 1
		raw, err := json.Marshal(env)
		require.NoError(t, err)
		e.Payload = string(raw)
		return
	}
	t.Fatalf("no inbox row for %s", taskID)
}

// TestReportDelivery_BatchCapFiveBlockingFirst pins the batch bound:
// one drain delivers at most MaxBatchReports reports with blocking
// reports first regardless of pending order, the remainder stays
// pending behind one trailing line, and a second drain delivers it.
func TestReportDelivery_BatchCapFiveBlockingFirst(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := NewMemoryStore()
	ids := make([]string, 0, MaxBatchReports+2)
	for i := range MaxBatchReports + 2 {
		id := fmt.Sprintf("b%d", i)
		terminalInbox(t, s, id, fmt.Sprintf("child-%d", i), "owner")
		ids = append(ids, id)
	}
	// The blocking report is last pending; it must still ship first.
	markBlocking(t, s, ids[len(ids)-1])

	writer := &fakeWriter{}
	n, err := NewInboxDrainer(s, readyGate(), writer).Drain(ctx, "owner")
	require.NoError(t, err)
	require.Equal(t, MaxBatchReports, n)
	require.Equal(t, 1, writer.batches, "one drain window is ONE message")
	got := writer.snapshot()
	require.Len(t, got, MaxBatchReports)
	require.Equal(t, ids[len(ids)-1], got[0].TaskID,
		"a blocking report is never the one left behind")

	batch := RenderBatch(got, len(ids)-MaxBatchReports)
	require.Contains(t, batch, "<untrusted-agent-results count=5>")
	require.Equal(t, MaxBatchReports, strings.Count(batch, "<untrusted-agent-result>"))
	require.Contains(t, batch, "… 2 more results pending (drain to fetch)")

	pending, err := s.ListInbox(ctx, "owner")
	require.NoError(t, err)
	require.Len(t, pending, len(ids)-MaxBatchReports,
		"the remainder stays pending for the next drain")

	n, err = NewInboxDrainer(s, readyGate(), writer).Drain(ctx, "owner")
	require.NoError(t, err)
	require.Equal(t, len(ids)-MaxBatchReports, n)
	rest, err := s.ListInbox(ctx, "owner")
	require.NoError(t, err)
	require.Empty(t, rest)
}

// TestReportDelivery_PendingCount is acceptance criterion 7: the
// pending count excludes consumed and delivered reports.
func TestReportDelivery_PendingCount(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := NewMemoryStore()
	terminalInbox(t, s, "t1", "child-1", "owner")
	terminalInbox(t, s, "t2", "child-2", "owner")
	terminalInbox(t, s, "t3", "child-3", "owner")

	entries, err := s.ListInbox(ctx, "owner")
	require.NoError(t, err)
	require.Len(t, entries, 3)
	acked := false
	for _, e := range entries {
		if e.TaskID == "t1" {
			require.NoError(t, s.AckInbox(ctx, []string{e.ID}, time.Now()))
			acked = true
		}
	}
	require.True(t, acked)
	require.NoError(t, s.MarkInboxConsumed(ctx, "owner", "t2"))

	n, err := s.CountPendingInbox(ctx, "owner")
	require.NoError(t, err)
	require.Equal(t, 1, n, "delivered and consumed rows leave the pending count")
	pending, err := s.ListInbox(ctx, "owner")
	require.NoError(t, err)
	require.Len(t, pending, 1)
	require.Equal(t, "t3", pending[0].TaskID)
}

// TestReportDelivery_PendingCountSQLite pins the SQL half of
// criterion 7: the count query never selects payloads and skips
// consumed rows on the durable store.
func TestReportDelivery_PendingCountSQLite(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := connectStore(t, t.TempDir())
	for _, id := range []string{"t1", "t2", "t3"} {
		require.NoError(t, store.Save(ctx, &Task{
			ID: id, OwnerSessionID: "o", Status: StatusRunning, RunGeneration: 1,
		}))
		_, won, err := store.TerminalizeAndDeliver(ctx, id, 1, TerminalUpdate{
			Status: StatusCompleted, Result: "r" + id, CompletedAt: time.Now(),
		})
		require.NoError(t, err)
		require.True(t, won)
	}
	entries, err := store.ListInbox(ctx, "o")
	require.NoError(t, err)
	require.Len(t, entries, 3)
	acked := false
	for _, e := range entries {
		if e.TaskID == "t1" {
			require.NoError(t, store.AckInbox(ctx, []string{e.ID}, time.Now()))
			acked = true
		}
	}
	require.True(t, acked)
	require.NoError(t, store.MarkInboxConsumed(ctx, "o", "t2"))

	n, err := store.CountPendingInbox(ctx, "o")
	require.NoError(t, err)
	require.Equal(t, 1, n, "delivered and consumed rows leave the pending count")
	pending, err := store.ListInbox(ctx, "o")
	require.NoError(t, err)
	require.Len(t, pending, 1)
	require.Equal(t, "t3", pending[0].TaskID)
}

// TestReportDelivery_PushDoesNotMutateBody is acceptance criterion
// 8: the push path never mutates the stored report body. The task
// record read before delivery is byte-identical to the agent_output
// read after delivery, and the stored inbox payload is untouched by
// the drain that computed staleness on in-memory copies.
func TestReportDelivery_PushDoesNotMutateBody(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	m, s := newTestManagerStore(t, Limits{})
	task := completeTask(t, m, "owner", "frozen body")

	snapBefore, err := m.Status(ctx, "owner", task.ID)
	require.NoError(t, err)
	require.Equal(t, "frozen body", snapBefore.Result)
	payloadBefore := s.inboxPayload(task.ID)
	require.NotEmpty(t, payloadBefore)

	writer := &fakeWriter{}
	n, err := NewInboxDrainer(s, readyGate(), writer).Drain(ctx, "owner")
	require.NoError(t, err)
	require.Equal(t, 1, n)

	after, _, err := m.Output(ctx, "owner", task.ID)
	require.NoError(t, err)
	require.Equal(t, snapBefore.Result, after.Text, "read before delivery is byte-identical to read after")
	snapAfter, err := m.Status(ctx, "owner", task.ID)
	require.NoError(t, err)
	require.Equal(t, snapBefore.Result, snapAfter.Result)
	require.Equal(t, payloadBefore, s.inboxPayload(task.ID),
		"the push path computes staleness on copies, never on stored rows")
}

// inboxPayload returns the stored payload of one task's inbox row.
func (s *MemoryStore) inboxPayload(taskID string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, e := range s.inbox {
		if e.TaskID == taskID {
			return e.Payload
		}
	}
	return ""
}

// TestReportDelivery_AnchorFlowsToEnvelope pins the completion
// boundary: the anchor carried by the terminal update lands in the
// stored envelope the drain delivers.
func TestReportDelivery_AnchorFlowsToEnvelope(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := NewMemoryStore()
	_, anchor := anchoredFile(t, "pinned\n")
	row := terminalInboxAnchor(t, s, "t1", "child-1", "owner", anchor)
	env, err := row.Envelope()
	require.NoError(t, err)
	require.NotNil(t, env.ArtifactAnchor)
	require.Equal(t, anchor.Path, env.ArtifactAnchor.Path)
	require.Equal(t, anchor.SHA256_8, env.ArtifactAnchor.SHA256_8)
	require.Equal(t, anchor.Lines, env.ArtifactAnchor.Lines)
	require.Equal(t, "t1", env.OutputRef)
	require.Equal(t, VerdictUnknown, env.Verdict)
	require.False(t, env.Stale)

	// The durable sqlite store carries the same envelope.
	sqlite := connectStore(t, t.TempDir())
	require.NoError(t, sqlite.Save(ctx, &Task{
		ID: "t1", OwnerSessionID: "o", Status: StatusRunning, RunGeneration: 1,
	}))
	_, won, err := sqlite.TerminalizeAndDeliver(ctx, "t1", 1, TerminalUpdate{
		Status: StatusCompleted, Result: "r", CompletedAt: time.Now(), Anchor: anchor,
	})
	require.NoError(t, err)
	require.True(t, won)
	rows, err := sqlite.ListInbox(ctx, "o")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	stored, err := rows[0].Envelope()
	require.NoError(t, err)
	require.Equal(t, anchor.SHA256_8, stored.ArtifactAnchor.SHA256_8)
}
