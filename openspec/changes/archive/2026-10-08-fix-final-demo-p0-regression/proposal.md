## Why

The public final-demo regression exposed two P0 blockers: the pool metrics exporter is pinned to a previous fixture manifest, and AgentTeams role contexts contaminate one another, producing duplicate execution and a Matrix message storm. These defects prevent a trustworthy end-to-end incident demonstration.

## What Changes

- Replace manifest-pinned demo metrics with a versioned exporter that discovers every fixture from the authoritative pool-fixture service and emits `pool_manifest_id`-labeled series.
- Isolate Manager and Worker instructions and continuation handling by runtime role.
- Prevent workers from consuming Manager dispatch state or handling messages addressed to another role.
- Strip reasoning/thinking content before replies reach Matrix.
- Add per-agent message-rate protection and a reliable admin incident STOP circuit breaker.
- Add focused Go and Python tests for dynamic metrics, role routing, reply sanitization, rate limiting, and STOP behavior.

## Capabilities

### New Capabilities

- `pool-metrics-discovery`: Dynamically expose metrics for every pool fixture without redeploying or manually pinning a manifest.
- `agentteams-dispatch-isolation`: Keep Manager and Worker contexts separated, make stage continuation idempotent, and provide fail-safe message controls.

### Modified Capabilities

- None. This repository has no existing OpenSpec capability specs.

## Impact

- `cmd/pool-fixture` and its Go tests.
- Public-demo monitoring scripts under `deploy/demo/monitoring/pgpool-fixture`.
- `plugins/opskeeper-teamharness/adapters/qwenpaw/plugin.py` and related Python tests.
- TeamHarness package version metadata and generated release artifacts during the later build phase.
- No shared PostgreSQL or Redis schema changes; pool fixtures remain incident-owned and disposable.
