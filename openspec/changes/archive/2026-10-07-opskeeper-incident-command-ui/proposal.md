# Proposal: opskeeper-incident-command-ui

## Why

OpsKeeper's differentiator is the auditable incident loop: real business degradation, multi-agent diagnosis, controlled repair preview, precise human approval, narrowly authorized repair, independent verification, and replayable archive evidence. The current public demo and TeamHarness plugin expose these capabilities, but their information architecture remains feature-oriented and uses two separate stage vocabularies. This makes the product feel like a generic operations console and obscures its decision-safety value.

OpsKeeper should orient every incident-facing surface around three questions:

1. What is happening to the business?
2. Which actor or safeguard is currently blocking progress?
3. What evidence justifies the next action?

This change establishes an incident-command presentation model, reorganizes TeamHarness around the incident workflow, upgrades the public live demo, and creates a staged path toward runtime-aware incident command without changing Manager execution authority.

## What Changes

### 1. Establish an authoritative presentation contract

Create a shared, presentation-only `IncidentCommandView` projection used by the TeamHarness plugin and public live demo. The projection will explicitly carry incident identity, scenario, generalized business impact, authoritative current stage, freshness and observation time, owner, blocking reason, prioritized next action, per-stage timeline facts, decision-category evidence completeness, and approval, rollback, and verification safeguards.

The contract will distinguish `fresh`, `stale`, and `unknown` observations. Missing authoritative data will never be projected as running progress.

### 2. Define an explicit stage mapping matrix

Map Manager loop events, RCA/task phases, live-demo scenario states, and the seven authoritative OpsKeeper command stages without inferring progress. The stage IDs will match Manager loop phases (`detected`, `correlated`, `investigated`, `critiqued`, `approved`, `recovered`, `postmortem`); worker roles remain owner labels rather than stage IDs. The matrix will identify the source event/task for every displayed stage, duration, owner, outcome, and blocking reason.

The repair stage will support `awaiting_human`, `approved`, and `executing` sub-states so a human approval block is not mislabeled as Worker execution.

### 3. Reorganize TeamHarness around incident command

Change the primary plugin navigation to `事故指挥`, `证据审批`, `复盘档案`, and `系统状态`. Plugin management and integration self-check will move to secondary diagnostics. Existing tab IDs will receive compatibility aliases so old deep links remain useful.

### 4. Add command orientation to the live demo

After scenario injection, the public live page will show a persistent command summary containing incident identity, business impact, current stage, owner, elapsed time, expiry boundary, and the single next action. External dashboards and rooms will remain contextual recovery links rather than the primary orientation.

### 5. Build decision-depth surfaces

Implement a seven-stage command timeline, grouped evidence drawer, A/B preview comparison, rollback plan, and precise-approval checklist. Evidence will be grouped by the decision it supports rather than by source system. The approval checklist will remain presentation-only and will expose the exact approval channel or command; it will never imply that reviewing the checklist grants authority.

### 6. Make archive replay the learning surface

Archive will lead with replay and closure summary before dense tables. Replay will distinguish evidence visible at decision time from post-incident enrichment. Legacy incidents without newer optional fields will receive explicit compatibility states rather than false failure states.

### 7. Establish a scoped visual and accessibility system

Define OpsKeeper-scoped semantic tokens for status, impact, surfaces, focus, spacing, and data density before the main component rewrite. The public demo may use a command-console brand treatment; the TeamHarness plugin will map scoped tokens to the AgentTeams host theme. Status icons, keyboard operation, focus traps, narrow-screen layouts, and stale/empty/error states are first-class requirements.

### 8. Stage runtime-aware evolution

Runtime inventory, task claims, leases, checkpoints, worker recovery, and plugin drift will initially remain outside primary incident navigation. Once the runtime-management change provides authoritative readback, those facts will explain incident blockers and system status. OpsKeeper will not become a general node, plugin, disk, or workflow administration console in this change.

### 9. Establish the OPC squad delivery track

Create role-scoped execution packets and review gates for the OPC squad so contract, UI, safety, archive, runtime, and validation work can proceed in bounded increments. Each packet will define owner, scope, authoritative data source, tests, and handoff artifact to avoid parallel UI work creating a second execution authority.

## Impact

- `site/app/live-incident/page.tsx`
- `site/lib/demo-types.ts`
- `site/components/demo/*`
- `plugins/opskeeper-teamharness/dashboard/src/extensions/tabs.js`
- `plugins/opskeeper-teamharness/dashboard/src/extensions/unified-route.jsx`
- diagnostic, archive, runtime, and integration extension components
- shared projection/token helpers and focused tests
- OPC squad execution and review documentation

## Non-Goals

- No change to Manager authority, Worker roles, approval semantics, safety gates, or repair execution.
- No new authoritative workflow-state database contract.
- No replacement for AgentTeams Dashboard navigation or host theming.
- No generic monitoring, topology, device, service-desk, or broad IT administration surface.
- No uncontrolled drill creation from archived incidents.
- No rebranding or affiliation with another product.
