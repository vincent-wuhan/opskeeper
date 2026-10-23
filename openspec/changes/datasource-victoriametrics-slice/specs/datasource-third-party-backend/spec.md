# Specification: Third-Party Datasource Backend

## Purpose

Admit, activate, and observe a datasource package that OpsKeeper does not
vendor, on a signal the first-party packages already own, without widening the
first-party activation flow and without letting a query answer change shape
because the backend behind it changed.

## ADDED Requirements

### Requirement: A third-party datasource package is an admitted package
OpsKeeper SHALL register a third-party datasource package only through
`kernel.Host.Activate`, under the control-plane datasource admission policy, so
that every boot admits the package rather than only a test.

#### Scenario: Package passes admission
- **WHEN** the VictoriaMetrics package's embedded manifest is activated on a control-plane host
- **THEN** admission runs against `ManagerPolicy()` and the contribution registers under `kernel.KindDatasource`

#### Scenario: Package is out of scope for the host
- **WHEN** the manifest declares a scope the host's policy does not grant
- **THEN** admission refuses the package and nothing is registered

#### Scenario: Contribution and manifest disagree
- **WHEN** a contribution's package name differs from its manifest name
- **THEN** activation is refused before any registration happens

### Requirement: The `prom` signal has exactly one backend at a time
OpsKeeper SHALL select one backend for the `prom` signal at configuration time
and SHALL refuse to start if two packages claim the same signal.

#### Scenario: First-party default
- **WHEN** `prom.backend` is unset or is `prometheus`
- **THEN** `opskeeper-datasource-prom` is the only package registered for `prom`, and `Set.Prom()` is the first-party client

#### Scenario: Third-party selected
- **WHEN** `prom.backend` is `victoriametrics`
- **THEN** `opskeeper-datasource-victoriametrics` is registered for `prom` and `opskeeper-datasource-prom` is not

#### Scenario: Both are activated
- **WHEN** two packages register the `prom` signal on one host
- **THEN** the second registration is refused with `ErrDuplicate` and the process does not start

#### Scenario: Illegal backend value
- **WHEN** `prom.backend` is a value outside `{prometheus, victoriametrics}`
- **THEN** startup logs the value and exits non-zero without falling back to a default

### Requirement: A third-party package is its own module
OpsKeeper SHALL build each Go plugin package as an independent module that
resolves its workspace siblings through `replace` directives and is verified by
the standalone module build with the workspace disabled.

#### Scenario: Standalone build
- **WHEN** `make module-standalone-check` runs with `GOWORK=off` in the package directory
- **THEN** the package builds and its tests pass without any workspace file

#### Scenario: Package pulls in the control plane
- **WHEN** the package imports a business module of the control plane or a PiG module
- **THEN** this is a boundary violation and is refused in review

### Requirement: The package's declared scope is the whole of its reach
A third-party datasource package SHALL declare exactly the scopes it needs, and
OpsKeeper SHALL grant no scope beyond those declarations.

#### Scenario: Read-only metric package
- **WHEN** the VictoriaMetrics package is admitted
- **THEN** its declared scope set is `observability.read` and its safety level is L1

### Requirement: PromQL means the same thing on either backend
OpsKeeper SHALL send byte-identical query parameters to the VictoriaMetrics
HTTP API as to the first-party Prometheus client for the same expression and
time window.

#### Scenario: Same expression, both backends
- **WHEN** the same instant or range query is issued to both clients against a recording backend
- **THEN** the raw request URIs are identical and the normalised results are deeply equal

#### Scenario: Range query
- **WHEN** a range query with a given start, end and step is issued
- **THEN** both clients emit the same `query`, `start`, `end` and `step` parameters

### Requirement: A MetricsQL-only function is refused, not approximated
OpsKeeper SHALL refuse a query expression that uses a function outside the
standard PromQL set rather than evaluating a different question than the one
asked.

#### Scenario: MetricsQL-only function
- **WHEN** an expression uses a function that standard PromQL does not define
- **THEN** the call fails and the error contains `vm_unsupported_function`

#### Scenario: Standard function
- **WHEN** an expression uses only standard PromQL functions
- **THEN** the call is forwarded unchanged

### Requirement: Upstream failures surface as failures
OpsKeeper SHALL map a 4xx, a 5xx and a timeout to distinct errors and SHALL NOT
return an empty or partial result in place of a failed query.

#### Scenario: Unauthorised
- **WHEN** the backend answers 401
- **THEN** the call fails with an error naming the status

#### Scenario: Backend error
- **WHEN** the backend answers 5xx
- **THEN** the call fails with an error naming the status

#### Scenario: Timeout
- **WHEN** the backend does not answer within the query ceiling
- **THEN** the call fails with a timeout error rather than an empty result

### Requirement: Credentials reach the package and nothing else
OpsKeeper SHALL inject datasource credentials per request over an authenticated
transport and SHALL NOT persist them in configuration, in the manifest, or in
logs.

#### Scenario: Bearer credential
- **WHEN** a bearer token is configured
- **THEN** every request carries it and the token appears in no log line

#### Scenario: Precedence
- **WHEN** both a bearer token and basic credentials are configured
- **THEN** only the bearer token is sent

### Requirement: Selecting a third-party backend names the surfaces it did not change
OpsKeeper SHALL state, at startup, which query surfaces still use the first-party
backend when a third-party backend has been selected for one of them.

#### Scenario: Third-party selected for the datasource extension point
- **WHEN** `prom.backend` is `victoriametrics`
- **THEN** startup logs which surfaces — the integrations probe, the metric domain handler, alert rules and their preview, the report collector, and the Monitor page proxy — still use the first-party Prometheus client

#### Scenario: First-party selected
- **WHEN** `prom.backend` is unset or `prometheus`
- **THEN** no such split-plane log is emitted and startup output is unchanged from before this capability existed

### Requirement: The declared boundary is observable from the code
OpsKeeper SHALL hold compile-time assertions that the third-party datasource
client satisfies each narrow query interface the control plane consumes, so
that a later one-shot cutover is a change of wiring rather than of the package.

#### Scenario: Interface satisfaction
- **WHEN** the package's client is assigned to each of the control plane's PromQL query interfaces
- **THEN** the assignment compiles

### Requirement: The manifest on disk is the manifest in the binary
OpsKeeper SHALL decode the package's embedded `pig-ops.yaml` rather than
declaring the same manifest again in code.

#### Scenario: Embedded bytes and file bytes agree
- **WHEN** the embedded manifest is compared to the file in the package
- **THEN** the bytes are identical