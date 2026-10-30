## Why

`core/manager/biz/aiops/datasource` was reworked so an observability query
backend is a microkernel plugin rather than a field in an assembly struct, and
its package comment says the point was that "a fourth backend (VictoriaMetrics,
Elasticsearch) is a new plugin rather than a fourth field here". That claim has
never been paid off: the only three packages registered under
`kernel.KindDatasource` are the first-party ones, and nothing has yet demonstrated
that the third-party path works outside a test — no third-party package has ever
been admitted by a running manager.

The datasource recovery plan (§3 候选 1, §6 骨架) names VictoriaMetrics as the
first third-party candidate and defines its manifest, but explicitly refuses to
land it ("落地门槛：用户须单独开 change"). This change is that separate change.

## What Changes

- Add `plugins/opskeeper-datasource-victoriametrics/` as the first Go module
  under `plugins/`, carrying its own `go.mod` with local `replace` directives
  and entering the `PIG_MODULES` standalone-build list. This fixes the module
  precedent every later third-party package will copy.
- Add `internal/vmquery`, which turns a PromQL expression plus a time window
  into VictoriaMetrics' `/api/v1/query` and `/api/v1/query_range` parameters,
  byte-for-byte identical to what `core/base/pkg/promquery` sends, and refuses
  MetricsQL-only functions with a `vm_unsupported_function` error rather than
  letting them silently change the answer.
- Add `internal/vmclient`, a Prometheus-HTTP-API client whose results normalise
  to `*promquery.InstantResult`, so every existing PromQL consumer keeps working
  without a type change.
- Add `internal/contributor`, a `KindDatasource` contribution registering under
  the **existing** `prom` signal name — no new signal is introduced.
- Add `prometheus.backend` configuration (`OPSKEEPER_PROM_BACKEND`), validated at
  startup, that selects which one of the two `prom` clients the assembly layer
  activates. Both cannot be live: a second registration of `datasource/prom` is
  refused by the registry and startup fails closed.
- Ship the package's `pig-ops.yaml` as the single source of truth: it is
  `go:embed`-ed and decoded through `sdk.Decode`, never re-declared as a Go
  struct.
- Log, at startup and explicitly, which query surfaces still point at the
  first-party Prometheus when the VictoriaMetrics backend is selected.

## Capabilities

### New Capabilities

- `datasource-third-party-backend`: admission, single activation, PromQL
  compatibility, and observability of a third-party package registered under a
  signal the first-party packages already own.

### Modified Capabilities

- None. `datasource.go`'s activation flow and the kernel's closed kind set are
  unchanged; the third-party package reaches the registry through the same
  `Host.Activate` entry point the first-party ones use.

## Impact

- New module `plugins/opskeeper-datasource-victoriametrics` (its own version,
  its own manifest, its own tests), added to `PIG_MODULES` and therefore to
  `make module-standalone-check`.
- `core/floor/config`: one new `PromConfig.Backend` field, defaulted to
  `prometheus`. The default path is unchanged bit for bit.
- `cmd/opskeeper`: the backend-selection decision is extracted into a testable
  function and one call site switches. Six other `promQueryClient` consumers are
  deliberately untouched and are named in the startup log.
- No migration, no schema change, no behaviour change when `Backend` is unset.
  Rollback is `git revert` plus (in a live deployment) setting
  `OPSKEEPER_PROM_BACKEND=prometheus`.

## Out of Scope

- Elasticsearch / OpenSearch datasources (separate change).
- The disk-loaded third-party manifest channel
  (`OPSKEEPER_MANAGER_PLUGINS_DIR` + `RegisterActivator`). That directory has no
  deployment wiring today, so this slice preinstalls the package in the binary;
  see `design.md` for why the disk channel is the wrong first step.
- One-shot cutover of the whole Prom query surface. Only the datasource
  extension point switches here; the interface-satisfaction assertions added here
  are what make that later cutover a one-line change per consumer.
- remote_write / `push_prom_samples`: still targets the first-party
  configuration even when `Backend = victoriametrics`.
- Counting third-party packages in `scripts/dcell`'s roster.