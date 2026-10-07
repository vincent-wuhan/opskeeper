## ADDED Requirements

### Requirement: Present incidents through a command-first architecture
OpsKeeper incident-facing surfaces SHALL orient operators around current incident identity, business impact, authoritative stage, owner, blocking reason, freshness, elapsed time, and prioritized next action before presenting administrative capabilities.

#### Scenario: Operator opens an active incident
- **WHEN** an active incident is selected
- **THEN** the first screen identifies the incident, impact, stage, owner, duration, freshness, blocker, and one prioritized next action
- **AND** plugin administration and integration self-check remain available from secondary diagnostics

#### Scenario: Authoritative stage data is absent
- **WHEN** stage data is missing, malformed, expired, or cannot be mapped
- **THEN** the UI displays an unknown or stale state
- **AND** it does not infer running or completed progress

### Requirement: Normalize an authoritative presentation projection
OpsKeeper SHALL provide a presentation-only incident-command projection that normalizes stage, substate, owner, generalized impact, freshness, observation time, source reference, stage timeline, evidence completeness, and next action for incident-facing surfaces.

#### Scenario: Projection receives authoritative transition data
- **WHEN** an authoritative event or task maps to a command stage
- **THEN** the projection preserves stage, status, owner, timing, outcome, and source reference
- **AND** it does not create execution authority

#### Scenario: Public demo and plugin receive equivalent source data
- **WHEN** both surfaces normalize equivalent authoritative inputs
- **THEN** they produce equivalent stage, owner, freshness, next action, impact, and completeness projections
- **AND** visual chrome may remain surface-specific

### Requirement: Map stages only from authoritative sources
OpsKeeper SHALL maintain an explicit mapping among command stages, Manager loop events, RCA/task phases, and public-demo states. Command stage identifiers SHALL use the authoritative Manager loop phases `detected`, `correlated`, `investigated`, `critiqued`, `approved`, `recovered`, and `postmortem`; worker roles SHALL be owner labels rather than stage identifiers. Unmapped or unverifiable states SHALL remain unknown or partial.

#### Scenario: Repair waits for a human decision
- **WHEN** repair has not started and approval is required
- **THEN** the `approved` stage identifies the `awaiting_human` substate and human owner
- **AND** it does not label the Worker as executing repair

#### Scenario: A source event name changes
- **WHEN** an authoritative event or task kind no longer matches the normative mapping matrix
- **THEN** the affected stage becomes unknown or stale
- **AND** a focused mapping test fails

### Requirement: Render a decision-oriented command timeline
Incident-facing surfaces SHALL render the seven OpsKeeper stages with status, substate, owner, duration, compact outcome, blocking reason, evidence link, and source reference where available.

#### Scenario: Operator waits for precise approval
- **WHEN** the incident reaches the human approval transition
- **THEN** the approval state is visually dominant
- **AND** the interface names the waiting owner and exact decision required
- **AND** completed stages remain inspectable without exposing every raw payload initially

#### Scenario: A stage transition completes
- **WHEN** an authoritative transition is recorded
- **THEN** the timeline shows compact outcome and duration
- **AND** future stages remain visually distinct without implying speculative progress

### Requirement: Keep decision evidence one action away
OpsKeeper SHALL provide an evidence view that groups incident facts, causal chain, affected scope, monitoring evidence, candidate comparison, rollback plan, approval record, execution identity, and verification result by supported decision.

#### Scenario: Operator inspects a repair proposal
- **WHEN** the evidence view opens from the repair or approval state
- **THEN** it presents candidate comparison, controlled workload boundary, target/workload identity, rollback plan, and verification criteria
- **AND** raw payloads remain secondary disclosures

#### Scenario: Legacy archive evidence is incomplete
- **WHEN** optional newer evidence fields are absent on a legacy incident
- **THEN** the UI shows an explicit legacy-not-applicable or partial state
- **AND** it does not mark the incident failed solely for that absence

### Requirement: Present precise approval without granting authority
The approval surface SHALL expose candidate identity, execution identity, target fingerprint, workload fingerprint, impact scope, parameters, expiry, authoritative server time, rollback plan, preview eligibility, verification criteria, and authoritative approval status before an approval action appears visually actionable.

#### Scenario: Operator supplies an ambiguous affirmative
- **WHEN** approval input lacks incident or candidate context
- **THEN** the UI identifies the missing precise instruction
- **AND** it does not imply that Manager authority has been granted or bypassed

#### Scenario: All authoritative approval facts are present
- **WHEN** the checklist is complete
- **THEN** the operator can inspect each fact and reach the existing precise approval channel or command
- **AND** Manager remains the sole validator and recorder of the authoritative decision

### Requirement: Replay closed incidents without rewriting history
Archive SHALL support selecting a closed incident and replaying its timeline, decision-time evidence, selected and rejected candidates, rollback outcome, verification result, completeness, and bounded historical similarities without changing authoritative incident history.

#### Scenario: Reviewer opens a closed incident
- **WHEN** a closed incident has complete evidence
- **THEN** archive presents replay and decision summary before dense tables
- **AND** individual transitions link to their source evidence

#### Scenario: Archive contains post-incident enrichment
- **WHEN** knowledge or similarity data was created after closure
- **THEN** replay identifies it as post-incident enrichment
- **AND** it is not presented as evidence that was available at decision time

#### Scenario: Reviewer compares similar incidents
- **WHEN** bounded similarity data is available
- **THEN** archive exposes comparison provenance and limits
- **AND** it does not create a drill unless all controlled-drill identities and safeguards are supported

### Requirement: Preserve accessibility, responsiveness, and host integration
Incident Command UI SHALL keep status, timeline, evidence, disclosure, and approval controls accessible and responsive. TeamHarness SHALL use OpsKeeper-scoped semantic tokens that remain compatible with AgentTeams host theme variables.

#### Scenario: Keyboard-only operator reviews an incident
- **WHEN** the surface is used without a pointer device
- **THEN** timeline nodes, evidence disclosures, and checklist controls are reachable and operable
- **AND** the evidence drawer supports focus management and `Escape` dismissal

#### Scenario: Plugin runs under a light host theme
- **WHEN** TeamHarness renders inside AgentTeams
- **THEN** OpsKeeper-scoped tokens map to host-compatible contrast and foreground values
- **AND** the plugin does not impose a global OpsKeeper page theme

### Requirement: Preserve operational and runtime authority
Incident Command UI SHALL be presentation-only and MUST NOT execute repairs, bypass approval, mutate safety state, change runtime desired state, or replace AgentTeams Dashboard navigation.

#### Scenario: Projection fails
- **WHEN** normalization or projection read fails
- **THEN** the surface shows retry or stale-data state
- **AND** existing Manager, Worker, safety, approval, repair, verification, and runtime paths continue unchanged

#### Scenario: Runtime information explains an incident blocker
- **WHEN** authoritative runtime/task readback is available
- **THEN** command or system status can show health, claim, lease, checkpoint, recovery, or version-drift context
- **AND** detailed runtime administration remains outside primary incident navigation
