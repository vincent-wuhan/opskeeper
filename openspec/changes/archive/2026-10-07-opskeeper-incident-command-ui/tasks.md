# Tasks

## 1. Discovery, Contract, and Design Foundation

- [x] 1.1 Inventory live-demo, Manager incident, RCA task, approval, repair-preview, verification, archive, runtime, and plugin data dependencies.
- [x] 1.2 Build the normative stage-mapping matrix using Manager loop phase IDs (`detected`, `correlated`, `investigated`, `critiqued`, `approved`, `recovered`, `postmortem`) and exact event/task names plus source identifiers.
- [x] 1.3 Define `IncidentCommandView`, stage substates, generalized impact, freshness, completeness, source references, and next-action priority rules.
- [x] 1.4 Define projection behavior for missing, partial, stale, malformed, and legacy data.
- [x] 1.5 Audit current labels, tab IDs, status colors, emoji, inline styles, keyboard behavior, focus behavior, and responsive breakpoints.
- [x] 1.6 Define scoped semantic tokens, status iconography, focus states, contrast requirements, and data-density rules.
- [x] 1.7 Add compatibility aliases for all existing TeamHarness tab IDs and deep-link states.
- [x] 1.8 Confirm UI copy uses the approved generic operations-platform wording and does not reference another product.

## 2. P0 Incident-First Orientation

- [x] 2.1 Rename and reorder primary TeamHarness tabs to incident command, evidence approval, archive replay, and system status.
- [x] 2.2 Move plugin management and integration self-check into secondary diagnostics without removing capability.
- [x] 2.3 Implement the Incident Command Bar with incident identity, impact, stage, owner, elapsed time, freshness, and expiry boundary.
- [x] 2.4 Implement the prioritized next-action card for wait, inspect, approve, reject, verify, archive, and retry states.
- [x] 2.5 Add the same command summary to the public live page after successful injection.
- [x] 2.6 Preserve contextual external links and make them secondary to the incident summary.
- [x] 2.7 Add focused tests for tab aliases, projection, command summary, next-action copy, empty/stale state, and legacy input.

## 3. P1 Command Timeline and Decision Evidence

- [x] 3.1 Implement the seven-stage timeline with status, substate, owner, duration, outcome, blocking reason, and source reference.
- [x] 3.2 Implement the grouped evidence drawer with focus trapping, `Escape` dismissal, and keyboard-operable disclosures.
- [x] 3.3 Project A/B candidate comparison, controlled workload boundary, target/workload identity, and preview eligibility.
- [x] 3.4 Project rollback plan, verification criteria, approval record, execution identity, and audit trail.
- [x] 3.5 Implement the precise-approval checklist, exact approval instruction/channel, and missing-context warning.
- [x] 3.6 Ensure the UI cannot execute repair, bypass approval, imply granted authority, or mutate safety state.
- [x] 3.7 Add keyboard, focus, responsive, loading, retry, stale, empty, and failure-state tests.

## 4. P1.5 Shared Projection

- [x] 4.1 Extract shared projection, stage mapping, freshness, completeness, and formatting helpers.
- [x] 4.2 Adapt TeamHarness and the public live demo to shared helpers while preserving host/demo visual differences.
- [x] 4.3 Add cross-surface golden tests for stage, owner, freshness, next action, impact, and completeness.

## 5. P2 Archive Replay and Knowledge

- [x] 5.1 Design and implement replay from authoritative archived transitions.
- [x] 5.2 Group replay evidence by decision and retain links to source events.
- [x] 5.3 Distinguish decision-time evidence from post-incident enrichment and current knowledge references.
- [x] 5.4 Add evidence completeness and bounded historical-comparison summaries.
- [x] 5.5 Preserve explicit `legacy_not_applicable` states for optional newer fields.
- [x] 5.6 Add a visually secondary controlled-drill action only for scenarios with complete scenario, manifest, target, workload, and safety identity support.

## 6. P3 Runtime-Aware Command

- [x] 6.1 Define read-only projections from authoritative runtime inventory, task, health, and plugin-readback data.
- [x] 6.2 Show runtime and task blockers as incident/system-status explanations with claim, lease, checkpoint, recovery, and drift context.
- [x] 6.3 Keep detailed node, rollout, disk, credential, and plugin administration outside primary incident navigation.
- [x] 6.4 Add tests proving runtime data gaps produce unknown/stale states rather than inferred failures.

## 7. OPC Squad Delivery and Governance

- [x] 7.1 Create role-scoped OPC execution packets for contract, command UI, live demo, evidence/approval, archive, runtime liaison, design/accessibility, and verification.
- [x] 7.2 Define each packet's scope, owner, authoritative sources, changed files, tests, forbidden mutations, and handoff artifact.
- [x] 7.3 Establish review gates for projection contract, safety copy, accessibility, runtime boundary, and archive replay.
- [x] 7.4 Maintain a dependency board separating P-1/P0/P1/P2/P3/P4 work and preventing duplicate parallel edits.
- [x] 7.5 Record decisions, open questions, and cross-change dependencies in the change handoff notes.

## 8. Validation and Completion

- [x] 8.1 Validate strict OpenSpec change artifacts.
- [x] 8.2 Run focused TeamHarness tests/build.
- [x] 8.3 Run public live-demo tests/build and one end-to-end smoke flow.
- [x] 8.4 Validate Chinese/English copy, color contrast, tabular readability, keyboard paths, and narrow-screen behavior.
- [x] 8.5 Review final surfaces against incident-command positioning and remove residual generic-administrative framing.
- [x] 8.6 Verify no UI path mutates approval, safety, repair, runtime, or authoritative incident state.
