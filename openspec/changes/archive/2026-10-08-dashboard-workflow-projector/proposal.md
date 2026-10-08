# Why

OpsKeeper stages currently live in Matrix text and the TeamHarness plugin views, while AgentTeams Dashboard has an existing real-time `agentteams.workflow` task-board channel. Without projection, Element can show the conversation but the Dashboard task board does not automatically advance through OpsKeeper stages.

# What Changes

Add a plugin-native workflow projector in `opskeeper-teamharness`. Manager will emit a complete, human-readable Matrix notice containing the standard `agentteams.workflow` payload at these authoritative transitions:

- incident request accepted;
- Worker task dispatched successfully;
- matching `OPSKEEPER_RESULT` consumed;
- admin approval or rejection recorded.

The payload uses one stable incident-derived `runId`, fixed OpsKeeper stage steps, and per-Worker subagent statuses. Projection is best-effort: Matrix or persistence failures are logged but never block incident execution or safety controls.

# Capabilities

## New Capabilities

- `dashboard-workflow-projection`: Project OpsKeeper's authoritative stage transitions into the existing AgentTeams Dashboard workflow/task-board protocol without modifying AgentTeams, Dashboard, or shared storage schemas.

## Modified Capabilities

- None.

# Impact

- `plugins/opskeeper-teamharness/adapters/qwenpaw/plugin.py`
- Focused Manager-gate Python tests and a new projector test module.
- TeamHarness package/version metadata and generated release artifacts.
- No AgentTeams Controller, Dashboard host, PostgreSQL, Redis, or MinIO schema changes.
