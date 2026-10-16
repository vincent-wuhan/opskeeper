# Tasks: pig-runtime-adapter

## 1. Architecture and Contracts

- [x] 1.1 Define the `AgentRuntimeAdapter` port, runtime descriptor, lifecycle event, health, cancellation, capability, and terminal-result contracts.
- [x] 1.2 Map the adapter port to ChatRuntime and AgentTeams semantics without changing their current behavior.
- [x] 1.3 Define PiG release identity, compatibility, capability manifest, sandbox profile, and immutable execution-snapshot records.
- [x] 1.4 Add configuration for an opt-in, pinned PiG executable or Piglet artifact with ambient discovery disabled.

## 2. PiG Runtime Adapter

- [ ] 2.1 Implement a PiG runtime driver that starts, probes, streams events, cancels, and terminates an isolated PiG process.
- [ ] 2.2 Normalize PiG registration and lifecycle observations into OpsKeeper runtime health and task events.
- [ ] 2.3 Add capability admission that reconciles declared, observed, and allowlisted tools before exposing them to a role.
- [ ] 2.4 Enforce no ambient extension discovery, no provider override, no host write, and no direct production credential access in the first slice.
- [ ] 2.5 Add deterministic fake-PiG integration fixtures for launch, registration, heartbeat, progress, cancellation, crash, and completion.

## 3. Governed Tool Gateway Integration

- [ ] 3.1 Translate PiG tool identities to stable OpsKeeper tool IDs and read/write/destructive classes.
- [ ] 3.2 Enforce tenant, incident, role, resource, capability, rate-limit, and read-only policy before dispatch.
- [ ] 3.3 Persist tool start/end, caller identity, release/capability digest, parameters hash, result state, error, and trace correlation.
- [ ] 3.4 Add tests proving undeclared, duplicate, drifted, write, destructive, cross-incident, and cross-resource tools fail closed.
- [ ] 3.5 Add proposal-only behavior for repair candidates while preserving the existing approval and exact-payload execution path.

## 4. Pi/PiG Ecosystem Inventory and Resources

- [ ] 4.1 Produce a Phase 0 inventory template and reviewed candidate list for operations-relevant Pi/PiG packages.
- [ ] 4.2 Implement immutable package/release records with source, version, digest, license, dependencies, capability manifest, and verification state.
- [ ] 4.3 Import allowlisted non-executable skills, prompts, themes, and agent declarations while skipping hooks, MCP, and executable extensions by default.
- [ ] 4.4 Handle duplicate names, provenance visibility, tenant enablement, rollback, and deterministic reload.
- [ ] 4.5 Document rejected packages and required remediation before executable admission.

## 5. Runtime Management Interop

- [ ] 5.1 Keep adapter contracts compatible with the planned runtime inventory, heartbeat, task claim, lease, workspace, credential, and event models.
- [ ] 5.2 Add a migration seam so PiG release snapshots and capability digests can move into the shared plugin rollout model without a schema fork.
- [ ] 5.3 Ensure Manager loss cancels or safely terminates unclaimed work and never retries a side-effect-capable operation without an idempotency contract.

## 6. Operations UI and Observability

- [ ] 6.1 Expose PiG runtime kind, release digest, health, capabilities, active task count, and last heartbeat in runtime inventory.
- [ ] 6.2 Emit append-only registration, capability, drift, tool-call, cancellation, rollback, and terminal events with incident/task/trace correlation.
- [ ] 6.3 Add configuration validation, artifact verification, startup, and rollout failure diagnostics.

## 7. Verification and Promotion Gates

- [ ] 7.1 Add unit and integration tests for adapter lifecycle, capability admission, policy enforcement, and event completeness.
- [ ] 7.2 Add E2E validation for one investigator-style incident using only read-only PiG tools.
- [ ] 7.3 Add process isolation, timeout, heartbeat loss, crash, cancellation, restart, and no-replay tests.
- [ ] 7.4 Add redaction, secret non-disclosure, audit bypass, cross-tenant, and sandbox constraint tests.
- [ ] 7.5 Record Phase 2 promotion evidence and define Phase 3 executable-extension acceptance criteria.

## 8. Documentation and OPC Handoff

- [ ] 8.1 Document target architecture, runtime selection, package admission, and rollback strategy.
- [ ] 8.2 Add operator runbooks for PiG registration, incident execution, drift, cancellation, and rollback.
- [ ] 8.3 Publish the Pi/PiG adoption roadmap, promotion gates, and OPC squad ownership matrix.
