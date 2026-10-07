---
title: pg-pool-exhaustion:resize-pool:v1
kind: sop
tags: [database, postgresql, connection-pool, pool-fixture, resize, recovery, rollback]
applies_to: [manager]
---

# pg-pool-exhaustion:resize-pool:v1

Use this SOP only for the disposable `pg:pool-fixture` when an application-side
pool is exhausted. It must never be used against the shared PostgreSQL service.

## Entry criteria

- `incident_id` and `pool_manifest_id` are both present and bound to this fixture.
- `root_cause.confirmed` identifies `pg_pool_exhausted`.
- Active connections equal the current capacity and the failed probe returned
  `pool_exhausted`.
- The proposed action is `resize_pool`, the target is exactly `pg:pool-fixture`,
  and the requested capacity comes from the fixture manifest.
- `preview_run_id` and `preview_candidate_id` identify a `PASS` repair-preview candidate
  for this incident, and the compact baseline/A/B card has been shown to the approver.

## Review checklist

1. **SOP coverage**: all entry criteria above are true.
2. **No parallel operation**: incident detail shows no running or executed
   `resize_pool` for the same manifest. Only one approved proposal may be active.
3. **Rollback scope**: recovery is manifest-bound. A failed resize can return
   the disposable fixture to its previous capacity and cannot touch databases,
   schema, users, or the shared `opskeeper-postgres` service.
4. **Preview evidence**: the referenced candidate is `PASS`; any `FAIL` or
   `REJECTED_BY_PREVIEW` candidate, including `reset_pool`, is rejected before HITL.

Approval requires all four checks to pass. Any missing evidence is a rejection.

## Execution and verification

- Execute only through the approved, proposal-bound `recovery.execute` tool.
- Preserve `preview_run_id` and `preview_candidate_id` in the canonical recovery
  parameters; the executor rechecks preview eligibility before reserving the proposal.
- Verify a new successful pool probe and active connections below the new
  capacity before recording `recovery_signal.observed`.
- Close the incident only after the postmortem records the manifest, capacity
  transition, probe evidence, and rollback scope.
