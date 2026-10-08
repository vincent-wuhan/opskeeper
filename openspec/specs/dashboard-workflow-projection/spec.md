# dashboard-workflow-projection Specification

## Purpose
TBD - created by archiving change dashboard-workflow-projector. Update Purpose after archive.
## Requirements
### Requirement: Project OpsKeeper Transitions

OpsKeeper TeamHarness SHALL emit a Matrix `m.room.message` containing both a human-readable body and an `agentteams.workflow` object when Manager accepts an incident request, successfully dispatches a Worker task, consumes a matching Worker result, or records an explicit admin approval decision.

#### Scenario: Dashboard Live Task

- **WHEN** Manager emits a workflow event for incident `opskeeper-demo`
- **THEN** the event contains stable `runId="opskeeper-demo"`
- **AND** Dashboard can merge it into the live task board without host changes
- **AND** Element can display the same notice body

### Requirement: Preserve Incident Authority

Workflow projection SHALL NOT execute tasks, bypass HITL, mutate AgentTeams/Dashboard host state, or become the source of truth for incident progression.

#### Scenario: Matrix Failure

- **WHEN** the Matrix workflow send fails
- **THEN** OpsKeeper logs the failure
- **AND** continues the existing dispatch/result safety path

### Requirement: Bound Duplicate Projections

The projector SHALL suppress unchanged snapshots and duplicate transitions, and persist only a bounded incident/task index.

#### Scenario: Duplicate Worker Result

- **WHEN** the same task result is consumed more than once
- **THEN** only the first matching stage transition emits a workflow event

