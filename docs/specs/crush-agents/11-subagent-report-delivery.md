# Crush Agents: Subagent Report Delivery Contract

This document defines how a completed child task's report reaches the parent
session. It is self-contained and authoritative for that behavior. It does not
reopen settled decisions: delegation is `call_agent`, child depth is exactly one,
children retain ordinary capabilities subject to profile policy, and every child
runs under one asynchronous task model.

The problem this closes is interference, not latency. Background children are
the point — the parent keeps working while they run. What costs the parent real
work is the *callback*: the same report delivered twice, a report whose line
numbers belong to a revision the parent has already moved past, and a delivery
shape that forces a fresh turn per result before the parent has finished the
atomic edit it was in the middle of.

## Observed Failure Modes

Each mode below is caused by a specific mechanism in the current branch, listed
so the fix can be verified against the mechanism rather than the symptom.

1. **Re-delivery after the parent already read the report.** `agent_output`
   (`internal/agent/agent_task_tools.go`) reads the task record through
   `TaskController.Output` and writes nothing. The inbox drainer
   (`internal/agent/task/inbox.go`) suppresses only rows it has already
   delivered (`delivered_at`). A parent that pulled the full text itself still
   receives the pushed copy, so the same long text is paid for twice in context.
   Six pending tasks deliver six copies.

2. **No revision anchor.** `TaskResultEnvelope`
   (`internal/agent/task/inbox.go`) carries identity, status, summary, and the
   bounded result, but nothing identifying the file revision the child read.
   `filetracker` records `(session_id, path, read_at)` only
   (`internal/db/sql/read_files.sql`). A child reporting "line 567 is broken"
   cannot be checked against the parent's current file, so a stale report looks
   like a live defect and costs a byte-level audit to refute.

3. **A turn boundary per result.** The app wires a continuation into the
   drainer (`internal/app/task_inbox.go`), and `drainOwner` invokes it once per
   delivered row. Every delivered report therefore starts a parent turn, even
   when the parent is mid-edit. Each re-entry forces the parent to re-confirm
   the current file state, and line-addressed edits are exactly what a
   mid-edit interruption corrupts.

4. **Full text inside the delivery.** `taskResultWriter.WriteResult` writes
   `env.Render()`, which inlines the `result:` body (bounded by
   `task.MaxResultBytes`, still large). Reports compete for window space with
   the spec and code evidence the parent actually needs resident.

5. **No triage without reading.** The envelope has no severity, no counts, and
   no blocking flag, so deciding whether a report needs action requires reading
   it in full.

## Delivery Contract

### Report envelope

`TaskResultEnvelope` gains the following fields. Existing identity and status
fields are unchanged.

```text
task_id            string     // existing
run_generation     uint64     // existing; pins the attempt
status             Status     // existing (completed|failed|cancelled|interrupted)
summary            string     // existing; one-line outcome
verdict            enum       // PASS | FAIL | N/A  — did the child's own goal hold
counts             object     // { blocking, notable, info, confirmed, refuted }
findings           []Finding  // see below; bounded, may be empty
blind_spots        []string   // <=5 short lines: what the child could not check
artifact_anchor    Anchor     // the revision the child actually read
stale              bool       // current content no longer matches the anchor
output_ref         string     // task_id; the full text is pull-only
result             string     // retained for agent_output only, never pushed
```

```text
Finding:
  severity    enum        // blocking | notable | info
  claim       string      // one sentence
  evidence    []Location  // { file, line, line_end }
  confidence  enum        // high | medium | low

Anchor:
  path        string
  sha256_8    string      // first 8 hex chars of the content hash
  lines       int         // line count of the anchored revision
```

Rules:

- `Render()` (the pushed form) MUST emit the compact envelope only: identity,
  status, verdict, counts, findings, blind spots, anchor, stale, and
  `output_ref`. It MUST NOT emit `result`. The full text stays behind
  `agent_output`.
- `Render()` output SHOULD stay at or under 2 KB for a report with 5 findings.
  Findings beyond the first 10 MUST be dropped from the pushed form and their
  count reported in `counts`, never silently omitted.
- The anchor is **best-effort and MUST NOT be fabricated**. A child that never
  read file content (pure grep, reasoning from memory) reports no anchor, and
  `Render()` says so explicitly. An absent anchor is not an error.

### Anchor production

`read_files` gains `content_sha256_8` and `lines`. `filetracker.RecordRead`
takes the served content, and the `view` tool records what it actually served.
The anchor published in a report is the anchor of the file the child's evidence
points at, taken from that session's read receipts.

`stale` is computed at delivery time: the file's current hash is recomputed and
compared with the anchor. Mismatch sets `stale: true`. `stale` is `false` when
the anchor is absent — absence is reported by anchor presence, never by `stale`.

### Transmission rules

1. **At most one delivery per report.** A task whose result has been read
   through `agent_output` is marked consumed, and the drainer skips consumed
   rows. Consumption is recorded against the task's inbox row, so a delivery
   that has already happened and a read that has already happened are both
   terminal.

2. **Summary pushed, body pulled.** Rule: the pushed form is the compact
   envelope; the body is available only through `agent_output`.

3. **Every report carries its anchor.** Rule: `artifact_anchor` is present
   whenever the child read content; `stale` is computed per rule above.

4. **Delivery does not create a turn by default.** Auto-continuation is opt-in
   and off by default. The default surface is a cheap pending count that the
   parent reads when it chooses to drain. The count MUST be obtainable without
   loading report bodies.

5. **Coalesce within a window.** Pending reports for one owner that share an
   anchor (or that completed within the same drain window) are delivered as one
   message with one shared anchor header, and at most one continuation runs for
   the batch when auto-continuation is enabled.

6. **Triage without reading.** `counts` and the `blocking` flag are present in
   the pushed form so the parent can decide whether to pull the body.

7. **Already-covered marking.** When a finding's evidence anchor matches the
   only anchor already present in the parent's window, the pushed form marks
   that finding as `already_present` rather than presenting it as new.

## Delivery State Machine

```text
child terminalizes
  -> durable terminal row (task + outbox + parent inbox)      [existing]
  -> report is PENDING for its owner

parent pulls body via agent_output
  -> report becomes CONSUMED                                   [rule 1]
  -> no push for a consumed report

drain owner (explicit, or auto when enabled)
  -> gate: parent session exists and is idle                   [existing]
  -> select PENDING reports only                               [rule 1]
  -> compute stale per anchor                                  [rule 3]
  -> coalesce into one batch message                           [rule 5]
  -> write compact envelopes                                   [rule 2]
  -> ack delivered rows                                        [existing]
  -> at most one continuation for the batch (if enabled)       [rules 4, 5]
```

Invariants:

- A consumed report is never pushed. A delivered report is never pushed again.
- Reading a report never mutates task state beyond the consumption mark; it is
  not a transition of the task's own lifecycle.
- Delivery never blocks on the child, and never reorders a parent turn that is
  already in flight. Report delivery is not a scheduling primitive.
- The parent remains the only authority on whether a finding is acted upon. The
  envelope frames findings as untrusted evidence with no instruction authority,
  exactly as the current `Render()` does; that framing is retained.

## Acceptance Criteria

Each criterion is stated so it can be tested against the mechanism.

1. Read-then-deliver is silent. A parent that reads a task's output through
   `agent_output` receives no pushed copy of that report, and the drainer
   reports zero deliveries for it.
2. Unread reports still deliver exactly once, and a second drain delivers
   nothing.
3. A report whose anchored file was modified after the child read it is
   delivered with `stale: true`; an unmodified file yields `stale: false`; a
   report with no anchor reports no anchor and `stale: false`.
4. Default configuration produces no parent turn from delivery. With
   auto-continuation enabled, one drain of N pending reports produces exactly
   one continuation, not N.
5. The pushed form contains no `result` body, contains `counts` and per-finding
   severity, and stays within the 2 KB budget for a 5-finding report.
6. Two reports sharing an anchor are delivered as one message with a single
   anchor header.
7. A finding whose anchor matches the parent's already-present anchor is marked
   `already_present`.
8. `agent_output` still returns the full body, and a report read before delivery
   is byte-identical to what `agent_output` would have returned.

## Explicit Non-Goals

- No change to the child execution model, task lifecycle, quotas, or the
  `call_agent` acceptance contract.
- No reduction in child capability or in the parent's ability to pull full text.
- No structured-output parsing of free-form model prose. If findings cannot be
  produced structurally, the pushed form reports `counts: unknown` and the
  finding list is empty; it never guesses findings from prose.
- No attempt to make a report "correct". Anchoring reports staleness; it does
  not prevent a concurrent writer from invalidating a read.
- No change to the global context-file list, OMO memory wiring, or retry
  behavior.

## Open Decision: Findings Production

Structured findings must come from somewhere. Two candidate mechanisms, decided
at implementation time and recorded here as the one open fork:

- **Tool (preferred).** A `report_findings` tool the child calls before
  finishing, mirroring how `call_agent` already returns machine-readable
  acceptance metadata (`AgentCallAccepted`). Deterministic shape, no parsing.
- **File.** The child writes its report to a file; the harness records the path
  and hash and the parent reads it on demand. Cheaper, but loses the structured
  counts that rule 6 depends on.

If neither is implemented, rules 6 and 7 degrade to `counts: unknown` and no
findings, and the remaining rules (1-5, 8) still must hold.

## Completion Boundary

The contract is complete only when these paths are real, not represented by an
unused accessor or an in-memory-only store:

```text
view tool read
  -> read_files row with content_sha256_8 + lines
  -> report anchor resolves to the revision the child read

child terminalizes
  -> durable terminal row (task + outbox + parent inbox)
  -> report is PENDING

agent_output
  -> task result returned
  -> matching inbox row marked consumed
  -> later drain skips it

drain
  -> PENDING rows only
  -> stale computed against current content
  -> one compact batch message
  -> at most one continuation when enabled
```

Not acceptable as completion substitutes:

- marking consumption in memory so a restart re-delivers;
- computing `stale` from a hash taken at delivery time rather than at read time;
- shipping the compact envelope while `WriteResult` still writes the body;
- leaving auto-continuation on by default;
- a pending count that requires loading report bodies to compute.
