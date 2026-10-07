# opskeeper-incident-command-ui Delivery Governance

OpsKeeper evolves from a general operations console into an incident-command and evidence-audit surface for a governed operations loop. This change is presentation-only: it explains authoritative state, decisions, and blockers; it does not become the execution authority.

## Delivery packets

| Packet | Authoritative inputs | Delivery scope | Forbidden mutations | Validation handoff |
|---|---|---|---|---|
| Projection contract | Manager loop state/timeline, repair-preview readback, audit events, public demo scenario | `shared/incident-command/*`, exact seven-phase mapping, fixtures, freshness, completeness, priority, and demo normalization | Never invent phases, running status, sources, approval, recovery, or rollback results | Typed contract, golden fixtures, and `incident-command-contract.test.js` |
| Command IA | Frozen shared projection and existing Manager incident reads | TeamHarness incident-first tabs, compatibility aliases, command bar, next-action card, incident list, and read API wrappers | No repair, approval, safety, plugin, runtime, or authoritative incident writes | `incident-command.test.js`, tab/API/projection checks, production build |
| Live demo | Existing public scenario API, business snapshot APIs, and shared demo adapter | Public live-page command bar, seven-stage timeline, prioritized next action, and contextual secondary links | Incident components add no execution action; existing one-click scenario injection remains the only demo control | Site typecheck/build and browser smoke |
| Evidence and approval | Manager loop events, repair-preview summary, approval/execution/verification audit readback | Grouped evidence drawer, candidate comparison, controlled workload boundary, rollback and verification facts, precise checklist | No approval, rejection, repair, safety bypass, execution, or implied granted authority | Focused evidence, focus-trap, authorization-copy, partial-read, and failure tests |
| Archive replay | Archived incident transitions, decision-time evidence, enrichment records, and bounded similarity readback | Closed-incident replay, source-linked evidence, completeness, rejected-candidate visibility, and controlled-drill eligibility | Never rewrite archive history or infer selected candidate, approval, recovery, or rollback outcome from incomplete data | Archive replay and legacy-completeness tests |
| Runtime liaison | Authoritative runtime inventory, task, health, capacity, and plugin readback | Incident/system-status blocker explanations, claim/lease/checkpoint/recovery/drift context, and fixture availability | No node, rollout, disk, credential, plugin, or runtime desired-state mutation | Runtime mapping tests plus unknown/stale-gap tests |
| Design and accessibility | OpsKeeper semantic tokens, host theme variables, localized copy, and interaction matrix | Focus-visible controls, keyboard path, modal focus cycling, responsive layout, readable status text, and contrast-safe tokens | No global host-theme takeover or decorative-only status indicators | Component accessibility tests, contrast review, desktop/390 px smoke |
| Verification owner | Frozen contract, both deployed surfaces, OpenSpec delta, and review checklist | Full test matrix, mutation-boundary scan, positioning review, smoke evidence, and final independent review | No product changes solely to make validation pass | Validation references below and final review packet |

## Dependency board

| Gate | Required before | Deliverables and boundaries |
|---|---|---|
| P-1 contract freeze | All parallel UI packets | Exact Manager phase IDs, event/task mappings, fixtures, status/freshness/completeness rules, aliases, and tokens are frozen first |
| P0 orientation | P1 decision depth | Command summary, single prioritized action, tabs, and secondary diagnostics land before deeper evidence |
| P1 decision depth | P1.5 parity | Timeline, evidence, and approval may render only facts available from authoritative reads |
| P1.5 parity | P2 replay and P3 runtime | TeamHarness and public demo consume the same adapter and golden fixtures before historical/runtime expansion |
| P2 replay | P4 reuse/drill | Archive selection, decision-time evidence, rejected candidates, completeness, and rollback boundaries are authoritative before reuse |
| P3 runtime awareness | Future runtime-control integration | Only authoritative readback may explain blockers; administrative control remains a separate contextual surface |
| P4 knowledge/drill | Separate approval | Drill visibility requires complete scenario, manifest, target, workload, and safety identity support |

The contract owner is the only packet allowed to change shared mapping semantics before P0. After freeze, shared-contract changes require a new review gate and both surface owners' acknowledgment. The TeamHarness shell, public live page, evidence components, archive normalizers, and runtime projection are otherwise disjoint edit scopes to prevent duplicate parallel edits.

## Review gates

1. **Projection authority:** exact `detected`, `correlated`, `investigated`, `critiqued`, `approved`, `recovered`, and `postmortem` IDs; missing, malformed, stale, and legacy inputs remain unknown/stale/legacy rather than inferred.
2. **Command orientation:** one current incident, one deterministic next action, visible owner/freshness/source boundary, and business evidence after command.
3. **Safety copy:** PASS means preview eligibility only; precise approval facts and the authoritative channel are present; no UI wording grants authority.
4. **Archive integrity:** decision-time evidence is separate from post-incident enrichment; selection and rollback are read from authoritative records only.
5. **Runtime boundary:** readback gaps remain unknown/stale and detailed administration stays outside incident navigation.
6. **Accessibility:** keyboard-only traversal, focus visibility and cycling, `Escape` dismissal, responsive 390 px layout, tabular values, and contrast meet the scoped checks.
7. **Surface parity:** both surfaces pass the same golden Manager/demo projections while preserving allowed visual differences.
8. **Positioning and brand:** copy centers the governed incident loop and generic operations-platform wording; no competing product name is introduced.

## Validation record

The final validation matrix passed on 2026-10-01:

```bash
openspec validate opskeeper-incident-command-ui --type change --strict --no-interactive
cd plugins/opskeeper-teamharness/dashboard && npm test && npm run build
cd ../../../site && npm run typecheck && npm run build
```

| Gate | Result |
|---|---|
| Strict OpenSpec change validation | Pass |
| TeamHarness contract/UI/runtime/archive tests | 97/97 pass |
| TeamHarness production build | Pass |
| Public site TypeScript check | Pass |
| Public site production build | Pass |
| Authority-boundary scan | No mutating call in incident-command components; existing demo scenario start remains the only public demo control |
| Brand scan | No competing product name in the change or incident surfaces |

The final browser smoke covered the public live page at 1440×1000 and 390×844: real scenario injection/readback, command summary before business cards, single prioritized next action, seven-stage timeline, closed command state, contextual links, keyboard Tab/Shift+Tab order, visible focus state, no critical overflow, bilingual copy, tabular time values, and contrast-safe command text. Three screenshots and machine-readable pass results were retained in the local SDD evidence directory; no secret, token, or runtime credential appears in the evidence.

The contrast review found the original `ink-500` helper copy at approximately 3.25:1 against the command surface. The final components use `ink-400`, approximately 5.04:1, for those normal-size labels while retaining the command hierarchy.

## Decision and handoff log

- **Authoritative stages:** only the seven Manager loop phase IDs are stage identifiers; role names and workflow states are labels or mappings.
- **Unknown boundaries:** absent, malformed, stale, or unmappable readback remains `unknown`/`stale`; the UI never infers running or detected state.
- **Approval authority:** preview `PASS` and checklist completeness convey eligibility only; Manager remains the sole validator and recorder.
- **Host integration:** TeamHarness uses OpsKeeper-scoped tokens mapped to host variables instead of replacing the host theme.
- **Open questions:** none block this UI change. Controlled drill execution and runtime desired-state administration remain future, separately governed capabilities.
- **Cross-change dependency:** runtime-control work must expose authoritative readback first and must not reuse incident components as mutation controls.

## Deferred dependencies

- P4 drill execution remains gated and is not part of this UI change.
- Future runtime-control work must not reuse incident components to expose desired-state mutation.
- Any new Manager field requires a contract fixture and both TeamHarness/public parity tests before display.
