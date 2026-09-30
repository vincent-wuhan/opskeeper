## ADDED Requirements

### Requirement: Record the Paperclip integration feasibility decision
The project SHALL document whether and how OpsKeeper can integrate with Paperclip, including the selected integration shape, rejected alternatives, phased scope, upstream constraints, and implementation risks.

#### Scenario: A maintainer reviews the integration decision
- **WHEN** a maintainer reads the feasibility change
- **THEN** it identifies Paperclip's current plugin capabilities and deployment constraints
- **AND** explains why a read-first connector plugin is preferred over replacing AgentTeams or wrapping the entire OpsKeeper workflow as a Paperclip adapter
- **AND** records the Paperclip commit and license used for the assessment

### Requirement: Preserve OpsKeeper as the operational authority
A Paperclip integration SHALL NOT become the authoritative source for incident state, remediation proposals, approval, execution authorization, or recovery verification.

#### Scenario: Paperclip displays an incident
- **WHEN** a synced Paperclip issue shows incident state, RCA, evidence, or repair-preview information
- **THEN** that presentation is labeled as a mirror with the OpsKeeper source identity and synchronization time
- **AND** the authoritative state remains obtainable from OpsKeeper

#### Scenario: Paperclip submits an approval decision
- **WHEN** an approval bridge is explicitly enabled and an authorized Paperclip actor approves or rejects a proposal
- **THEN** the plugin submits an idempotent request bound to the exact OpsKeeper approval and proposal identity
- **AND** OpsKeeper performs authorization, dual-sign or tenant policy checks, execution, and audit recording

### Requirement: Start with a read-only connector phase
The first implementation phase SHALL expose only read-only synchronization and presentation capabilities, with mutating bridge features disabled by default.

#### Scenario: Read-only mode is enabled
- **WHEN** the connector is configured with read-only mode
- **THEN** it may list incidents, read timelines and summaries, and create Paperclip mirror records
- **AND** it rejects approval submission, incident resolution, silencing, repair execution, and other mutating calls

#### Scenario: OpsKeeper is unavailable
- **WHEN** the connector cannot reach OpsKeeper
- **THEN** Paperclip shows stale-state and last-success metadata instead of fabricating incident status
- **AND** availability of the connector does not affect the independently operating OpsKeeper workflow

### Requirement: Minimize connector privileges
The connector SHALL use a dedicated least-privilege OpsKeeper identity and SHALL NOT expose administrator credentials, generic shell tools, arbitrary MCP mutation, or cross-resource repair authority to Paperclip.

#### Scenario: Capabilities are reviewed
- **WHEN** the Paperclip plugin manifest and service-account scopes are inspected
- **THEN** they contain only the capabilities required by the enabled phase
- **AND** secrets remain server-side and are absent from UI state, logs, and issue comments

#### Scenario: An agent requests a mutating operation
- **WHEN** a Paperclip agent asks the connector to execute a repair or mutate an external resource
- **THEN** the connector refuses the direct operation
- **AND** any allowed follow-up is limited to creating a human-readable suggestion for the existing OpsKeeper proposal workflow
