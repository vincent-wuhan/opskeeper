## Context

The final-demo environment runs an incident-owned PostgreSQL pool fixture and an authenticated Prometheus scraper. The current public exporter is an unversioned Python container whose `POOL_MANIFEST_ID` environment variable is manually replaced for each incident. The TeamHarness plugin is installed in Manager and standalone Worker runtimes, but Manager prompt and continuation registrations are not consistently filtered by role.

## Goals / Non-Goals

**Goals:**

- Make pool metric collection independent of a manually pinned manifest.
- Ensure only Manager runtimes execute Manager continuation and relay logic.
- Ensure Workers do not receive Manager-only prompt instructions.
- Suppress reasoning/thinking text in outbound replies.
- Bound runaway message generation and honor an admin STOP for an incident.
- Preserve OpsKeeper Manager as the authority for incident records, approval, recovery execution, permissions, and redaction.

**Non-Goals:**

- No PolarDB, HA, schema, or shared PostgreSQL/Redis topology change.
- No replacement of the AgentTeams Matrix transport or its room membership model.
- No signed dispatch envelope protocol rewrite in this hotfix.
- No change to HITL approval authority or backend recovery validation.

## Decisions

### Pool metrics use authoritative dynamic discovery

The pool fixture already owns all live manifests and exposes an authenticated aggregate `/metrics` endpoint. The Go endpoint will include `pool_manifest_id` on every series. The versioned demo exporter will proxy that endpoint with its bearer token instead of storing a manifest ID. This avoids a new list API, prevents stale pinning, and allows PromQL to select the exact current incident manifest.

Alternative rejected: recreating the exporter with a new environment variable for every incident. That is the current failure mode and leaves no durable artifact in the repository.

### Manager behavior is selected by explicit role

`_is_manager_agent` will treat explicit Worker/standalone roles as non-Manager even if a Manager runtime variable leaks into the environment. Manager prompt sections and the continuation/relay hook will be registered or executed only for Manager contexts. Worker contexts will not consume Manager pending markers.

Alternative rejected: relying on `AGENTTEAMS_MANAGER_RUNTIME` alone. That variable describes a runtime family and is not a safe role identity.

### Replies are sanitized at the middleware boundary

A QwenPaw `on_reply` middleware will remove thinking blocks and literal `<think>...</think>` spans from final text events. This covers direct room replies without altering model reasoning itself.

### Fail-safe controls are process-local and bounded

Each runtime gets a small process-local reply/message limiter and an incident STOP set. An admin STOP records the incident and skips further non-admin turns that mention it. Rate breaches raise a terminal boundary error instead of emitting another Matrix message. These controls are intentionally simple to deploy quickly and stop recursive feedback.

## Risks / Trade-offs

- [Prometheus retains recent stale series for a closed manifest] → PromQL must filter by the current `pool_manifest_id`, while old series naturally expire.
- [Aggregate metrics include multiple fixtures] → Every series is manifest-labeled, making incident selection explicit.
- [Role inference may encounter an unknown deployment] → Explicit `standalone`/worker roles take precedence; tests cover the public-demo Manager and Worker configurations.
- [A process-local STOP set does not survive process restart] → Restart remains an operator recovery action; STOP prevents runaway output during an active incident.
- [A low rate limit could interrupt a legitimate long transcript] → The default permits short bursts and is environment-configurable.

## Migration Plan

1. Update the fixture metrics and add the versioned exporter.
2. Tighten TeamHarness role routing, reply sanitization, rate limits, and STOP handling.
3. Run focused Go and Python tests.
4. Build locally on macOS, deploy only approved artifacts to the public host, and run a fresh-manifest E2E.
5. Roll back by restoring the prior TeamHarness package and pool fixture image; no database migration is required.

## Open Questions

- None blocking implementation. Deployment and public E2E still require explicit user approval.
