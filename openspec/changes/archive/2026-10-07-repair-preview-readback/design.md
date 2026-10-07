# Repair Preview Readback Design

## Context

The current `/preview/` page is generated outside the incident evidence chain. It usefully shows baseline versus two candidates, but its index-repair/data-repair example is not the PG connection-pool main scenario, and the plugin can only link to it. Meanwhile, Manager already owns `incident_timeline`, archive aggregation, approval records, and tenant isolation; TeamHarness already has a read-only Archive projection.

The final demo must show one measured candidate reaching HITL and another rejected by preview validation without changing AgentTeams or Dashboard host code.

## Goals / Non-Goals

**Goals:**

- Make preview evaluation authoritative, durable, incident-bound, and auditable.
- Compare baseline and candidates on result consistency, query latency, throughput, write impact, storage delta, and business-probe success.
- Fit the existing PG pool-exhaustion demo while preserving the judge-required boundary language.
- Make HITL gate behavior explicit: only a PASS candidate may be proposed; humans still make the approval.
- Show the full comparison in both the standalone preview page and TeamHarness Archive.
- Keep the live final demo concise: show a compact incident-bound preview readback at the
  HITL gate, while the complete baseline/candidate table remains available in Archive and
  the standalone preview report remains a secondary deep link.

**Non-Goals:**

- Do not introduce PolarDB HA in this change.
- Do not modify AgentTeams or Dashboard host source.
- Do not expose raw SQL parameters, credentials, full diagnostic payloads, or production data.
- Do not represent preview-pg as a copy of active production sessions.

## Decisions

### 1. Store structured evaluation records, not only timeline text

Create a normalized preview run/candidate model owned by Manager. Each candidate row references the incident, tenant, preview run, branch/seed fingerprint, workload fingerprint, and sanitized metrics. Also append compact `repair_preview.*` events to `incident_timeline` for chronological replay.

This supports archive queries and candidate comparisons without parsing evidence strings. The timeline event remains an audit pointer, not the payload authority.

### 2. Use deterministic isolated preview branches

Each run resets isolated preview-pg databases or schemas from the same versioned fixture seed and executes one candidate per branch. The runner records the seed and workload fingerprints. It compares candidate output checksums against baseline, measures latency/TPS and write/storage deltas, and captures controlled business-probe results.

For the main connection-pool scenario, the preview workload reconstructs saturation with bounded concurrent clients and probes. The UI and API must state that this is a controlled reconstruction, not a copy of original active sessions.

### 3. Align candidates with the final main scenario

Candidate A applies the proposed bounded pool-capacity adjustment and must preserve result consistency while improving latency/throughput and recovery under the fixed workload. Candidate B applies a risky reset/session-clear strategy and is rejected when business probes fail, checksums diverge, or writes are lost.

The existing index/data-repair example may remain as documentation or a secondary scenario, but final scoring evidence uses these pool-scenario candidates.

### 4. Make preview a gate, not an auto-approval

The proposal builder accepts only candidates with `PASS`, complete baseline comparison, and non-missing consistency metrics. Candidate B receives `REJECTED_BY_PREVIEW` and cannot enter formal-change HITL. Candidate A remains pending human approval; rejection/approval continues to use the existing approval flow.

### 5. Keep the plugin read-only

Manager extends `/v1/incidents/{incident_id}/archive` with a sanitized `repair_previews` section. TeamHarness normalizes that response and renders:

- workload/seed/isolation metadata;
- baseline and candidate metric table;
- consistency, write-impact, and business-probe outcomes;
- PASS → HITL eligibility or FAIL → rejected conclusion;
- explicit controlled-load boundary text;
- optional link to the standalone preview report.

The plugin never writes preview facts and does not become the authority.

### 6. Separate live demo presentation from full evaluation execution

The final demo does not wait for the complete baseline/A/B replay to run in front of the
audience. The preview run is executed before the rehearsal with the same deterministic
workload and bound to the selected incident. During the live flow, Manager presents the
sanitized readback at the approval gate:

1. Baseline saturation metrics.
2. Candidate A PASS and therefore HITL eligibility.
3. Candidate B FAIL and therefore no HITL eligibility.
4. The controlled-load boundary and replay fingerprint.

The presenter then approves only Candidate A. After recovery and incident closure, Archive
shows the complete comparison table and evidence chain. This distinction prevents conflating
decision evidence with post-change recovery verification.

## Risks / Trade-offs

- [Preview noise causes unstable PASS/FAIL] → Use versioned fixed workload, warmup, repeated samples, median/p95 values, and fingerprinted seeds; record sample counts and confidence bounds.
- [A risky candidate damages the preview branch] → Isolate each candidate, use disposable preview storage, and never target the control-plane or production database.
- [Overclaiming database-branch fidelity] → Persist and render the boundary statement; mark runtime faults as reconstructed controlled workloads.
- [Archive becomes too dense for a live demo] → Return bounded summaries by default and keep raw run artifacts behind evidence references.
- [Legacy incidents have no preview data] → Archive must render an explanatory empty state and must not mark those incidents incomplete solely for lacking optional preview evidence.

## Migration Plan

1. Add backward-compatible preview tables and indexes.
2. Deploy the deterministic runner and versioned workload fixtures.
3. Seed or execute one final-demo run and bind it to the selected incident ID.
4. Deploy Manager and TeamHarness together.
5. Verify an old incident still loads and the selected final-demo incident shows both candidates.

Rollback removes the plugin version and leaves Manager extensions unused; existing incident archive and approval flows remain compatible.
