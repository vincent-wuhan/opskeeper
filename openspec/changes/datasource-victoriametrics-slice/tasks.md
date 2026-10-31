# Tasks: datasource-victoriametrics-slice

## 1. OpenSpec change

- [x] 1.1 Write proposal.md, design.md (decisions D1–D7, rejected candidates, risk register, rollback) and specs/datasource-third-party-backend/spec.md.
- [x] 1.2 Record the `go.work` → `replace` + `PIG_MODULES` deviation and the fixture-location deviation from the recovery plan.

## 2. Plugin module

- [x] 2.1 Create `plugins/opskeeper-datasource-victoriametrics/` as its own module with local `replace` directives.
- [x] 2.2 Copy `pig-ops.yaml` byte-for-byte; embed it and decode through `sdk.Decode`; assert embedded bytes equal file bytes.
- [x] 2.3 `internal/vmquery`: instant and range parameter translation plus the MetricsQL function whitelist (`vm_unsupported_function`).
- [x] 2.4 `internal/vmclient`: HTTP client returning `*promquery.InstantResult`; 4xx / 5xx / timeout mapped per class.
- [x] 2.5 `internal/contributor`: `KindDatasource` contribution on the existing `prom` signal.
- [x] 2.6 Module root: `New(...)` and `Manifest()` for the assembly layer.

## 3. Configuration and assembly

- [x] 3.1 `PromConfig.Backend` with env `OPSKEEPER_PROM_BACKEND`, default `prometheus`; illegal value fails startup.
- [x] 3.2 Environment-injected VM credentials: `OPSKEEPER_VM_BEARER_TOKEN`, `OPSKEEPER_VM_BASIC_USERNAME` / `..._PASSWORD`, `OPSKEEPER_VM_TLS_CA_FILE`; Bearer wins; nothing persisted or logged.
- [x] 3.3 Root `go.mod` require + replace; `PIG_MODULES` gains the package.
- [x] 3.4 Extract the backend selection into a testable function; switch only the datasource extension point.
- [x] 3.5 Log, by name, the surfaces still on the first-party Prometheus when `victoriametrics` is selected.

## 4. Tests

- [x] 4.1 Unit: byte-equal request URIs against a recording `httptest.Server`; MetricsQL refusal; result normalisation; 401 / 5xx / timeout; manifest decode / validate / embed identity.
- [x] 4.2 Contract: one shared `testdata/` fixture set, both clients against the same fake backend, deep-equal results.
- [x] 4.3 Integration: `//go:build integration`, testcontainers single-node VictoriaMetrics, five PromQL templates, absolute difference 0; `make vm-integration-check` in its own CI job.
- [x] 4.4 Failure: 401 / 5xx / timeout do not return empty results.
- [x] 4.5 Regression in `cmd/opskeeper`: default path `Live()` / `Set.Prom()` bit-for-bit; `victoriametrics` path swaps the package; dual activation fails with `ErrDuplicate`.
- [x] 4.6 Root-module assertions: the VM client satisfies all five narrow PromQL interfaces; the 30s ceiling constant equals `toolcore.QueryPromqlCallTimeout`.
- [x] 4.7 Root-module assertion: `vm.Manifest()` admits under `aiopsdatasource.ManagerPolicy()`.

## 5. Gates

- [x] 5.1 `make pi-plugin-guard && make module-check && go test ./scripts/ledgercheck`
- [x] 5.2 `go vet ./...`
- [x] 5.3 `GOWORK=off go build ./... && go test ./...` inside the new module
- [x] 5.4 `make vm-integration-check` (Docker available)