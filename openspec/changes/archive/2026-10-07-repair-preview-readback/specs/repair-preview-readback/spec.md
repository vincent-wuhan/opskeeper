## ADDED Requirements

### Requirement: Persist incident-bound repair preview evidence
The system SHALL persist every repair preview run with tenant ID, business incident ID, preview branch identifier, seed fingerprint, workload fingerprint, isolation boundary, execution status, and one or more candidate evaluations.

#### Scenario: Preview run is auditable
- **WHEN** a repair preview run completes for an incident
- **THEN** Manager stores the run and candidate records bound to the same tenant and incident ID
- **AND** each candidate includes consistency, latency, throughput, write-impact, storage-impact, and decision evidence

### Requirement: Evaluate candidates in isolated controlled replay
The system SHALL execute baseline and candidate repairs in isolated preview-pg branches created from a deterministic seed and SHALL replay the same fixed workload for every branch.

#### Scenario: Runtime fault is reconstructed without overclaiming
- **WHEN** the preview evaluates connection-pool exhaustion or lock waiting
- **THEN** the runner reconstructs the condition with bounded controlled load
- **AND** persisted and displayed evidence states that original active sessions are not copied

#### Scenario: Candidate changes query behavior
- **WHEN** a candidate changes an index, SQL statement, or data value
- **THEN** the runner compares result checksums and records query latency, throughput, storage delta, and write impact against baseline

### Requirement: Gate risky proposals on preview outcome
The system SHALL allow a risky formal-change proposal to enter HITL only when it references a preview candidate whose consistency checks passed and whose required comparison metrics are complete.

#### Scenario: Passing candidate remains human-gated
- **WHEN** Candidate A passes fixed-load consistency and recovery checks
- **THEN** it becomes eligible for HITL
- **AND** Manager does not treat the preview result as human approval

#### Scenario: Failing candidate cannot reach formal change
- **WHEN** Candidate B loses data, fails a business probe, diverges from the baseline checksum, or exceeds the permitted write impact
- **THEN** Manager records it as rejected by preview
- **AND** the candidate cannot enter formal-change HITL

### Requirement: Project preview evidence into incident archive
The incident archive API SHALL return sanitized repair-preview summaries for the requested tenant and incident, including baseline/candidate metrics, decisions, workload fingerprints, and the controlled-load boundary.

#### Scenario: Plugin renders the comparison
- **WHEN** a user opens an incident that has repair preview evidence
- **THEN** the TeamHarness Archive view renders the baseline/candidate comparison and PASS/FAIL decisions
- **AND** retains the standalone preview page only as a secondary deep link

### Requirement: Present compact decision evidence at the approval gate
The system SHALL expose a bounded, incident-bound preview summary for the live approval gate that identifies baseline metrics, passing candidate eligibility, failing candidate rejection, replay fingerprint, and controlled-load boundary without requiring the full evaluation to execute during the presentation.

#### Scenario: Presenter reaches HITL
- **WHEN** a preview-bound incident is waiting for human approval
- **THEN** Manager can return the compact decision summary for Candidate A and Candidate B
- **AND** only the passing candidate is eligible for the formal-change approval request

#### Scenario: Full comparison is needed
- **WHEN** the incident is closed or a reviewer opens Archive
- **THEN** the complete baseline/candidate metrics and decisions remain available from the authoritative incident record

#### Scenario: Legacy archive remains usable
- **WHEN** a user opens an incident created before repair preview integration
- **THEN** the archive still loads and displays an explanatory empty preview section
- **AND** the incident is not marked incomplete solely because optional preview evidence is absent

### Requirement: Isolate and sanitize preview readback
The system SHALL enforce tenant isolation for preview records and SHALL NOT expose credentials, raw SQL parameters, production data, or unbounded raw run artifacts in archive responses.

#### Scenario: Tenant cross-access is denied
- **WHEN** a user requests another tenant's incident archive
- **THEN** Manager does not return that tenant's preview evidence

#### Scenario: Sensitive values remain private
- **WHEN** archive readback is generated
- **THEN** it contains only bounded sanitized metrics, hashes, fingerprints, decisions, and evidence references
