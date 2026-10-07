# Design: opskeeper-incident-command-ui

## Positioning

OpsKeeper is an incident command and evidence-audit surface for AI-assisted operations. Its primary object is an incident moving through a governed loop from impact to verified recovery and reusable knowledge, not a dashboard, node, plugin, or workflow definition.

The interface has four persistent levels:

1. **Command** — what is affected, where the incident is, who owns the current step, and what happens next.
2. **Evidence** — the minimum authoritative material needed to trust the current state or decision.
3. **Authorization** — precise human approval, target identity, rollback, execution identity, and safeguards.
4. **Archive** — replayable history, decision-time evidence, similarities, and reusable operational knowledge.

Generic runtime administration remains contextual. It explains blockers after authoritative readback exists; it does not compete with the incident loop as the first impression.

## Presentation Contract

All incident-facing surfaces consume the same normalized projection. This is a UI contract, not an execution authority.

```ts
type StageId =
  | 'detected'
  | 'correlated'
  | 'investigated'
  | 'critiqued'
  | 'approved'
  | 'recovered'
  | 'postmortem';

type StageStatus =
  | 'pending'
  | 'running'
  | 'blocked'
  | 'completed'
  | 'failed'
  | 'unknown';

type ObservationFreshness = 'fresh' | 'stale' | 'unknown';

type Completeness = 'complete' | 'partial' | 'missing' | 'legacy_not_applicable';

type IncidentCommandView = {
  incidentId: string;
  scenario?: string;
  stage?: StageId;
  stageStatus: StageStatus;
  stageSubstate?: 'awaiting_human' | 'approved' | 'executing' | 'verifying';
  freshness: ObservationFreshness;
  observedAt?: string;
  serverNow?: string;
  sourceEventId?: string;
  sourceTaskId?: string;
  owner?: {
    kind: 'manager' | 'worker' | 'human' | 'verifier' | 'system';
    role?: string;
    label: string;
  };
  businessImpact: {
    level: 'unknown' | 'normal' | 'degraded' | 'severe';
    affectedScopes: Array<{ name: string; state: 'healthy' | 'degraded' | 'unknown' }>;
    indicators: Array<{ name: string; value?: string; state: 'healthy' | 'degraded' | 'unknown' }>;
  };
  nextAction?: {
    kind: 'wait' | 'inspect-evidence' | 'approve' | 'reject' | 'verify' | 'archive' | 'retry';
    priority: number;
    label: string;
    detail?: string;
    disabledReason?: string;
  };
  stageTimeline: Array<{
    stage: StageId;
    status: StageStatus;
    ownerLabel?: string;
    startedAt?: string;
    durationMs?: number;
    outcome?: string;
    blockingReason?: string;
    evidenceRefs: string[];
    sourceEventId?: string;
    sourceTaskId?: string;
  }>;
  evidenceCompleteness: {
    incident: Completeness;
    cause: Completeness;
    preview: Completeness;
    authorization: Completeness;
    execution: Completeness;
    verification: Completeness;
  };
};
```

Projection rules:

- A stage without an authoritative source is `unknown`, not `running`.
- An observation past its allowed age is `stale`.
- Expiry uses authoritative server time rather than client clock skew.
- UI animation may indicate refresh activity, never inferred incident progress.
- Every displayed stage fact retains a source event/task reference when available.

## Stage Mapping

The change will maintain one mapping matrix before timeline implementation:

| Command stage | Manager loop event | Loop/task phase | Public demo state |
|---|---|---|---|
| detected | `phase_entered:detected` and related detection events | alerter detection | `starting`, `awaiting_alert` |
| correlated | `phase_entered:correlated` / `phase_contract_written:correlated` | investigator correlation | `alert_correlated` |
| investigated | `phase_entered:investigated` / `phase_contract_written:investigated` | investigator diagnosis | `diagnosis_dispatched` |
| critiqued | `phase_entered:critiqued` / `phase_contract_written:critiqued` | critic review | diagnosis-to-preview transition |
| approved | `phase_entered:approved`, `phase_paused:approved`, and approval audit | reviewer proposal and HITL approval | `preview_ready`, `awaiting_approval` |
| recovered | `phase_entered:recovered` / `phase_contract_written:recovered`, execution and verification audit | repairer execution plus independent verifier | `repair_dispatched`, `verifying`, `recovered` |
| postmortem | `phase_entered:postmortem` / `phase_contract_written:postmortem` | postmortem reporter | `closed` |

The command stage IDs are the authoritative Manager loop phase IDs. Role names such as alerter, investigator, critic, reviewer, repairer, verifier, and postmortem reporter are owner labels, not stage IDs. The matrix is normative for implementation, but each source column must list exact event/task names during contract discovery. If a mapping cannot be proven, the corresponding UI state stays unknown or partial.

## TeamHarness Information Architecture

Primary tabs are `事故指挥`, `证据审批`, `复盘档案`, and `系统状态`. Secondary diagnostics retain plugin installation and integration self-check.

Compatibility aliases map:

- `diagnostics` → `incident-command`;
- `integration` → `incident-command` with diagnostics drawer;
- `archive` → `archive-replay`;
- `runtime` → `system-status`;
- `plugins` → `incident-command` with diagnostics drawer.

## Command and Evidence Layout

The first screen follows this reading order: incident identity and severity, business impact and duration, authoritative current stage and owner, one prioritized next action, compact seven-stage timeline, then contextual external links.

The evidence drawer groups material by decision:

- **Why this incident** — alert snapshot and affected probes.
- **Why this cause** — causal chain and corroborating signals.
- **Why this repair** — A/B preview, workload boundary, and rejected candidates.
- **Why it is safe** — target fingerprint, expiry, rollback, approval, and execution identity.
- **Why it worked** — post-change metrics and independent verification.

Raw payloads are secondary disclosures. The first evidence layer remains decision-oriented.

## Precise Approval

The approval checklist displays incident and candidate identity, execution identity, target and workload fingerprints, impact scope and parameters, expiry and server time, rollback plan, preview eligibility, verification criteria, and authoritative approval status.

The checklist can prefill or copy the exact approval instruction and can link to the existing approval channel. It cannot execute, bypass, or visually grant Manager authority. A bare affirmative remains insufficient and is shown as missing precise context.

## Archive Replay

Archive replay uses the same stage projection fixed at closure. It separates evidence available at decision time from post-incident enrichment, current similarity comparisons, and optional future drill actions.

Legacy incidents retain their original decision context. Missing optional newer fields render as `legacy_not_applicable`, not failure.

## Visual System

Semantic tokens precede component rewriting:

- impact/failure — red family;
- waiting/HITL — amber family;
- active system/agent work — cyan family;
- verified recovery — green family;
- evidence/history — neutral surfaces;
- unknown/stale — explicit neutral-plus-border treatment.

Tokens are OpsKeeper-scoped. The public page may use a dark command-console brand theme. TeamHarness maps tokens to host variables and preserves host-managed contrast and density.

Accessibility and responsiveness requirements include accessible status text, keyboard-focusable timeline nodes, focus-managed evidence dismissal, explicit disabled reasons, visible loading/stale/empty/retry/failure states, and narrow-screen stacking in reading order.

## Runtime Evolution Boundary

The runtime-management change remains a separate authority track. Incident Command UI consumes only authoritative Manager readback for runtime health and capacity, task claim/lease/checkpoint/recovery state, plugin version/capability drift, and workspace or preview fixture availability. These facts appear as incident blockers or system-status evidence. Detailed fleet, rollout, disk, credential, and plugin administration remain secondary administrative surfaces outside this change.

## OPC Squad Execution Model

The delivery README is the packet governance board. It records authoritative inputs, delivery scope, forbidden mutations, validation handoffs, the P-1 contract-freeze dependency gate, review criteria, and final validation evidence.

| Packet | Primary scope | Handoff |
|---|---|---|
| Contract owner | shared projection, exact phase mapping, fixtures | typed model and golden tests |
| Command UI owner | TeamHarness IA, command bar, next action, timeline | component tests/build |
| Live demo owner | demo adapter and public command components | typecheck/build/browser smoke |
| Evidence/approval owner | evidence drawer, candidates, precise checklist | safety-copy and focus tests |
| Archive owner | authoritative replay and legacy projection | replay and completeness tests |
| Runtime liaison | read-only blocker projection | runtime mapping and gap tests |
| Design/a11y owner | tokens, focus, contrast, responsive behavior | accessibility review |
| Verification owner | full matrix, boundary scan, final review | validation references |

The contract packet is the dependency gate. Parallel UI packets start only after phase IDs, source mappings, and golden fixtures freeze. No packet may change authoritative APIs or add mutation behavior. Shared-contract changes after freeze require the contract reviewer plus both surface owners; otherwise packet edit scopes remain disjoint.

## Rollout

1. **P-1 — Contract and design foundation:** inventory sources, define the mapping matrix, projection helpers, unknown/stale behavior, tokens, and compatibility aliases.
2. **P0 — Incident-first orientation:** rename tabs, move diagnostics secondary, add command bar and next action, and preserve existing capabilities.
3. **P1 — Decision depth:** timeline, evidence drawer, candidate comparison, rollback, precise approval, and accessibility/error-state tests.
4. **P1.5 — Shared projection:** extract shared helpers and add cross-surface golden tests.
5. **P2 — Replay and reuse:** archive replay, decision-time evidence, completeness, historical comparison, and gated drills.
6. **P3 — Runtime-aware command:** project authoritative runtime/task blockers while keeping runtime administration secondary.
7. **P4 — Knowledge and drill loop:** connect similarities to reusable knowledge and gate drill creation.

## Risks / Trade-offs

- **UI invents progress** → mapping matrix and source references are prerequisites.
- **Two surfaces diverge again** → shared projection helpers and golden tests precede visual polish.
- **Approval appears executable** → checklist is presentation-only and names the authoritative approval channel.
- **Evidence overwhelms operators** → first layer summarizes decisions; raw payloads remain secondary.
- **Plugin fights host theme** → scoped tokens map to host variables.
- **Runtime scope expands into generic administration** → runtime facts enter as incident explanations only.
- **Live demo regresses** → P0 remains presentation-only and preserves routes, recovery links, and existing APIs.
