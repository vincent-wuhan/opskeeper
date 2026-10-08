## ADDED Requirements

### Requirement: Pool metrics are dynamically manifest-labeled
The pool fixture service SHALL expose aggregate Prometheus metrics for every loaded fixture, and each series MUST carry a `pool_manifest_id` label.

#### Scenario: A new fixture is created
- **WHEN** a pool fixture is created while the metrics endpoint is already running
- **THEN** subsequent aggregate metric responses include series for that fixture without restarting or reconfiguring the endpoint

#### Scenario: Multiple fixtures exist
- **WHEN** aggregate metrics are requested for more than one fixture
- **THEN** active and capacity series remain distinguishable by `pool_manifest_id`

### Requirement: Demo exporter is manifest-agnostic
The public-demo pool metrics exporter SHALL obtain metrics from the authoritative pool-fixture aggregate endpoint and SHALL NOT require a manifest ID in its deployment configuration.

#### Scenario: Exporter starts before a fixture exists
- **WHEN** the exporter starts and no fixture is active
- **THEN** it remains healthy and emits no stale pool series

#### Scenario: Pool fixture becomes unavailable
- **WHEN** the exporter cannot reach the authoritative pool-fixture endpoint
- **THEN** it returns an upstream failure rather than fabricating pool values
