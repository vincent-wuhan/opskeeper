# Tasks: runtime-management-gap

## 1. Runtime Inventory and Readback

- [ ] 1.1 Define runtime identity, health, capability, capacity, version, artifact-digest, and deployment-location persistence models.
- [ ] 1.2 Add manager APIs for runtime registration, heartbeat, command acknowledgement, inventory, and health transition.
- [ ] 1.3 Implement read-only adapters for Edge Agent, Manager Chat Worker, AgentTeams Worker, TeamHarness/plugin manager, and preview-pg.
- [ ] 1.4 Make every runtime report version and artifact digest from manifest/release metadata and reject hard-coded authority.
- [ ] 1.5 Add heartbeat liveness grace, `starting`, `degraded`, `recently_lost`, and `offline` transitions.
- [ ] 1.6 Add focused model/API/adapter tests and a read-only runtime inventory view.

## 2. Durable Task Lifecycle

- [x] 2.1 Define task state, idempotency, admission, claim, lease, checkpoint, attempt, failure classification, and terminal-result models.
  - 已随 PR #98 落入 `core/manager/model/runtimemgmt/`（task{,_state,_idempotency,_admission,_claim,_lease,_checkpoint,_attempt,_result,_safety}.go + failure_class.go + errors.go，均带测试）。本勾选为 2026-11-01 读数纠偏（此前 change 显示 0/41 但 2.1 实际已交付）。
- [ ] 2.2 Implement slot-before-claim admission using server limits, runtime health, free slots, and local capacity.
- [ ] 2.3 Add claim, prepare-lease renewal, start, checkpoint, progress, result, cancel, stale-recovery, and failure APIs.
- [ ] 2.4 Integrate one read-only diagnostic worker and repair-preview execution with the shared task lifecycle.
- [ ] 2.5 Integrate AgentTeams/TeamHarness task reporting while preserving the existing incident-phase ledger as evidence.
- [ ] 2.6 Add restart/failover tests proving no duplicate read-only or disposable-preview work.

## 3. Workspace and Disk Governance

- [ ] 3.1 Define per-task `workdir/output/logs/config/temp` layout, claim lock, GC metadata, retention class, and evidence policy.
- [ ] 3.2 Implement task workspace creation, ownership validation, lock hold/release, and fail-safe reset.
- [ ] 3.3 Add bounded disk scans and per-task/incident/workspace/runtime/node/fleet usage reporting.
- [ ] 3.4 Implement policy-driven reclaim for regenerable artifacts, temporary data, stale preview environments, and expired workspaces.
- [ ] 3.5 Add quota/TTL admission checks and crash/recovery/GC tests that preserve evidence.

## 4. Task Credential Broker

- [ ] 4.1 Define task token, external credential grant, scope, expiry, renewal, revocation, and destruction models.
- [ ] 4.2 Mint task-bound credentials at claim time and revoke them at terminal state, lease expiry, cancellation, or runtime loss.
- [ ] 4.3 Broker or narrowly scope cloud, database, plugin API, and SSH credentials instead of exporting broad long-lived environment secrets where feasible.
- [ ] 4.4 Audit credential issuance, use, renewal, denial, revocation, and destruction.
- [ ] 4.5 Add centralized output/error sanitization for task results, logs, evidence, telemetry, and UI.
- [ ] 4.6 Add token expiry, revocation, cross-task denial, audit, and redaction tests.

## 5. Plugin and Worker Rollout

- [ ] 5.1 Define immutable plugin release, artifact digest, signature/verification state, compatibility matrix, and capability manifest.
- [ ] 5.2 Add per-node/per-runtime desired version and observed readback state.
- [ ] 5.3 Implement plugin reconciliation on Node Runtime Agent with version, digest, capability, health, and compatibility readback.
- [ ] 5.4 Add canary, ordered batch, health gate, pause/resume, failure isolation, and rollback APIs.
- [ ] 5.5 Pin the plugin release snapshot used by each running task.
- [ ] 5.6 Add rollback and mixed-version compatibility tests.

## 6. Runtime Event Timeline and Operations UI

- [ ] 6.1 Define append-only runtime event schema and correlation IDs.
- [ ] 6.2 Emit registration, health, drift, task, workspace, credential, plugin, rollout, and rollback events.
- [ ] 6.3 Add timeline query APIs filtered by runtime, task, incident, plugin, approval, and trace.
- [ ] 6.4 Add runtime inventory, task recovery, capacity, disk, credential, rollout, and event-timeline UI.
- [ ] 6.5 Add retention, authorization, and event-completeness tests.

## 7. End-to-End Verification

- [ ] 7.1 Verify read-only inventory across all first-slice runtimes.
- [ ] 7.2 Simulate Manager, worker, edge, plugin, and preview-runner restarts and verify recovery semantics.
- [ ] 7.3 Simulate network partition and verify fail-closed new mutating work with bounded local autonomy.
- [ ] 7.4 Verify canary failure pauses rollout and automatically restores the previous healthy release.
- [ ] 7.5 Verify disk GC preserves evidence and only reclaims policy-approved artifacts.
- [ ] 7.6 Verify task credentials expire, revoke, audit, and redact as required.
- [ ] 7.7 Run security, migration, Go, Python, and end-to-end regression suites.
