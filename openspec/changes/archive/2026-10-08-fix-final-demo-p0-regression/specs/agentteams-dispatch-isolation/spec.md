## ADDED Requirements

### Requirement: Manager continuation is role-isolated
The TeamHarness continuation and result-relay logic SHALL execute only in a Manager runtime, and an explicit Worker or standalone role MUST be treated as non-Manager.

#### Scenario: Worker sees a Manager dispatch marker
- **WHEN** a standalone Worker receives text containing a pending Manager task marker
- **THEN** it does not consume the marker, relay a result, or enter Manager continuation logic

### Requirement: Role prompts do not cross contexts
Manager-specific prompt content SHALL NOT be injected into standalone Worker contexts, and Worker prompt content SHALL NOT be injected into Manager contexts.

#### Scenario: Standalone worker builds its system prompt
- **WHEN** TeamHarness registers prompt sections for `opskeeper-repairer`
- **THEN** Manager-only coordination instructions are absent from that Worker prompt

### Requirement: Matrix replies exclude reasoning content
Outbound AgentTeams replies MUST NOT expose model reasoning or thinking content.

#### Scenario: Final reply contains a thinking span
- **WHEN** a model reply includes `<think>private reasoning</think>` and a public result line
- **THEN** only the public result line is emitted

### Requirement: Runaway messages are bounded
TeamHarness SHALL enforce a configurable per-agent outbound reply/message burst limit and terminate rather than continue emitting when the limit is exceeded.

#### Scenario: Repeated replies exceed the configured boundary
- **WHEN** an agent exceeds the configured outbound burst limit
- **THEN** no additional Matrix message is emitted for that boundary and a terminal rate-limit error is raised

### Requirement: Admin incident STOP is fail-safe
An administrator SHALL be able to stop an incident by ID, and subsequent non-admin messages that reference that incident MUST skip agent execution.

#### Scenario: Admin issues STOP
- **WHEN** an admin sends `ADMIN STOP <incident-id>`
- **THEN** the incident is recorded as stopped in that runtime before the model runs

#### Scenario: A stopped worker receives another incident message
- **WHEN** a non-admin message references a stopped incident
- **THEN** the agent turn is skipped without dispatch, room access, or status forwarding
