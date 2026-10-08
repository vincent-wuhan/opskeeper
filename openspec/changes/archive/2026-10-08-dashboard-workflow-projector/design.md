
# Dashboard Workflow Projector Design

## Contracts

AgentTeams Dashboard already accepts any Matrix `m.room.message` whose content contains an object at `agentteams.workflow`. It derives a live task by `runId`, merges its overall status into the task board, and renders `steps` and `subagents` in chat. Element renders the same event through its normal notice body and HTML fallback.

OpsKeeper therefore sends one new Matrix notice per meaningful transition. Every notice carries a full snapshot rather than an incremental delta, avoiding dependency on `m.replace` ordering or Dashboard history compaction.

## Stage Model

The stable stages are:

1. `alert` — alert confirmation
2. `investigate` — root-cause investigation
3. `review` — proposal and evidence review
4. `approval` — human approval gate
5. `repair` — approved repair
6. `verify` — independent recovery verification
7. `report` — postmortem and knowledge archive

Worker roles map one-to-one except `approval`, which is owned by Manager/admin. The first incident-like identifier in an admin request becomes the stable `runId`; generated task markers are mapped back to that run so Worker results can advance the correct stage.

## State and Safety

Projector state is atomically persisted beside the installed plugin with a bounded number of active runs. It never becomes an execution authority: duplicate transitions are ignored, missing state cannot block dispatch, and Matrix send failures only log warnings.

Projection emits only after:

- the Manager accepts a new incident request;
- a Manager message tool reports successful task dispatch;
- Manager consumes a matching first `OPSKEEPER_RESULT`;
- an admin sends an explicit approval/rejection instruction.

This preserves the existing one-dispatch-per-turn gate, role isolation, outbound safety, and HITL authority.

## Element and Dashboard Experience

Each event body contains a concise title, run ID, current stage, and stage list. The same event includes `agentteams.workflow`, so Element and Dashboard see one synchronized message. Dashboard can independently render the workflow card and advance the live task-board status.
