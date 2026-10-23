# Design: datasource third-party backend — VictoriaMetrics

Decisions are recorded as numbered verdicts (D1–D7) rather than as a narrative,
because each one closes a question whose answer a later reader would otherwise
have to reconstruct. Where a decision discarded an alternative, the discarded
alternative is recorded with the reason it lost — a design document that only
lists winners cannot be used to argue that the losers were wrong.

Baseline for every file:line below is `main@483783b4`.

---

## D1 — Module form: `go.work` reference means a self-contained module

The task description asked for "独立 Go module，go.work 引用". `go.work` is a
**gitignored local file** (`.gitignore:40-41`); CI builds from a checkout that
has none (`.github/workflows/ci.yml`), and the release condition is that every
module builds and tests itself with `GOWORK=off` (`make module-standalone-check`,
driven by the `PIG_MODULES` list in `Makefile`).

So "go.work reference" is realised as: the module carries its own `replace`
directives, exactly as `core/pig/extensions/opskeeper-sre-observability` does,
the root `go.mod` requires and replaces it, and it joins `PIG_MODULES`.
`go.work` remains a developer convenience and is not checked in. **Acceptance is
`GOWORK=off` standalone build and test.**

*This is the only deviation from the literal wording of the task.* It is stated
in the pull request description.

**Precedent, now fixed.** This is the first Go module under `plugins/` — the
directory until now held JS/TS delivery packages and YAML manifests only. The
rule every later third-party package copies: a Go plugin package lives at
`plugins/<pkg>/`, is its own module, resolves the workspace through `replace`,
and joins `PIG_MODULES`. D1 is written down here for that reason.

## D2 — Dependency surface is the module boundary

The package may import, and only these:

| Import | For |
| --- | --- |
| `core/base/pkg/promquery` | the result type every PromQL consumer already speaks |
| `core/base/pkg/promauth` | TLS + per-request credential injection |
| `core/floor/kernel` | `Registrar`, `KindDatasource` |
| `core/floor/pluginmanifest` | the manifest type `Host.Activate` takes |
| `sdk` | `Decode` for the embedded `pig-ops.yaml` |

It may **not** import `core/manager` or any `core/pig*` module. A third-party
package that could reach the control plane's business modules would drag the
whole control-plane graph into anything that vendors it.

`toolcore.PromQuerier` therefore cannot be referenced by the package. It is
satisfied **structurally**: `Query(ctx, expr, ts) (*promquery.InstantResult, error)`
and `QueryRange(ctx, expr, start, end, step) (*promquery.InstantResult, error)`.
The named-interface assertions live in the root module, where `core/manager` is
legitimately importable.

### Rejected — a `datasource.Backends` fourth field

`Backends` has three fields, all first-party, and `datasource.Activate` only
ever recognises three plugin names (`datasource.go:113-145`, `manifestFor` at
`:181-202`). Adding a field, or a third-party slot, edits the first-party
activation flow and puts a third-party package inside a list that reads as
first-party. Both violate the red line for this slice.

## D3 — Single activation: assembly-time branch, compiled-in package

Three mechanisms were compared.

**Rejected — the disk-loaded manifest channel.**
`pluginhost.New(pluginsDir, …)` reads manifests from
`OPSKEEPER_MANAGER_PLUGINS_DIR` and `pluginhost.RegisterActivator`
(`core/manager/middleware/pluginhost/host.go:130`) is the channel designed for
exactly this. But that directory has **no deployment wiring**: the only
production reference is `cmd/opskeeper/main.go:1710` reading the env var, plus a
design document. Shipping it first would deliver a switch that nothing mounts —
configured, enabled, and dead. It is recorded as the target shape once the
manager plugin-directory deployment task lands.

**Rejected — dual registration under one signal.**
`Registry.Register` refuses a `(KindDatasource, "prom")` that is already taken
(`core/floor/kernel/registry.go:68`), so the second activation fails rather than
overwrites. That is the safety property we want; what is rejected is *relying on
it as the selector*, because it makes a configuration mistake a boot failure
rather than a reported choice.

**Adopted — an assembly-layer branch that chooses, with fail-closed as backup.**
When `prom.backend == "victoriametrics"`, `dsBackends.Prom` stays nil and the
assembly layer calls `ph.Kernel().Activate(ctx, vm.Manifest(), vm.New(…))`
itself, on the same host and through the same gate as the first-party package:

- `Host.Activate` admits **before** it registers
  (`core/floor/kernel/host.go:141-153`), including the check that the
  contribution's `PluginName()` equals the manifest name;
- the policy is still `aiopsdatasource.ManagerPolicy()`, so every boot runs the
  third-party manifest through real admission rather than only through tests;
- a `Register` that fails partway is rolled back by the host, so a refused
  package leaves no entries behind.

Mutual exclusion is guaranteed by the branch; if configuration is broken badly
enough that both are activated, the second `Register` returns `ErrDuplicate`
and startup fails. **A unit test asserts that red light directly.**

`Set{}.Prom()` resolves by signal name (`datasource.go:100-102`) and does not
change with the contributing package.

## D4 — Configuration surface

`core/floor/config.PromConfig` gains:

```
Backend string   // env: OPSKEEPER_PROM_BACKEND; default "prometheus"
                  // legal values: prometheus | victoriametrics
```

An illegal value logs and exits at startup (`log.Error` + `os.Exit(1)`, the
precedent at `main.go:1324-1328`). There is no silent fallback: a manager that
thinks it is querying VictoriaMetrics while querying Prometheus is worse than a
manager that refuses to start. `core/floor/config` has no `Validate` hook, so
validation happens in the assembly layer.

Scope of the switch, stated so it is not misread:

- `Backend` selects the **backend behind the `prom` signal only**. The overall
  on/off switch remains `cfg.Prom.Enabled`.
- remote_write (`push_prom_samples`, `promwrite`) is **not** switched. It keeps
  pointing at the first-party configuration even when `Backend =
  victoriametrics`.
- URL and TLS reuse `cfg.Prom.URL` / `QueryURL` / `TLS*`.
- Credentials are injected from the environment, three mechanisms, mutually
  exclusive with Bearer taking precedence: `OPSKEEPER_VM_BEARER_TOKEN`,
  `OPSKEEPER_VM_BASIC_USERNAME` / `OPSKEEPER_VM_BASIC_PASSWORD`,
  `OPSKEEPER_VM_TLS_CA_FILE`. They are never persisted and never logged.

The name `datasource.prom.backend` from the recovery plan is this field's
semantic name; the mapping is written into the field comment and the pull
request.

## D5 — The split-plane risk is made visible, not managed away

`promQueryClient` has **seven** consumers in `main.go`: 1349 (integrations
probe), 1369 (metric domain handler), 1671 (alert preview), 1718 (datasource),
2395 (report), 3598 (Monitor page `/v1/prometheus` proxy), 4061 (alert rule
pipeline). This slice switches **1718 only**. The other six keep talking to the
first-party Prometheus.

That is a deliberate, bounded first step, and it is the kind of thing that is
misread later as "VictoriaMetrics is wired". So:

1. Selecting `victoriametrics` logs, explicitly and by name, every surface that
   still points at the first-party Prometheus: the integrations probe, the metric
   domain handler, alert rules and their preview, the report collector, and the
   Monitor page proxy.
2. The `Backend` field comment and this document state the same boundary.
3. The root module asserts that the VictoriaMetrics client satisfies each of the
   narrow interfaces those consumers hold (`managerbizalert.PromQuerier`,
   `managerbizalert.PreviewPromQuerier`, `managerservermetric.PromQuerier`,
   `managerserverprom.PromQuerier`, `managerreportdata.PromQuerier`; the
   integrations probe goes through the existing `AdaptPromQuerier` closure).
   All five are `Query`/`QueryRange` subsets returning
   `*promquery.InstantResult`.

Those assertions are what make the eventual one-shot cutover a change of where
each of the seven values comes from, rather than a change to the plugin package.

## D6 — Manifest, translation, and compatibility

**Manifest.** `plugins/opskeeper-datasource-victoriametrics/pig-ops.yaml` is a
byte-for-byte copy of the manifest merged with the datasource manifest work.
The module `go:embed`s that file and decodes it with `sdk.Decode`. **There is no
second declaration of the manifest as a Go struct**: the file on disk is the only
source of truth, and a test asserts the embedded bytes equal the file's. The
original draft under `openspec/changes/plugin-architecture-recovery-plan/` is
left untouched as a plan archive.

**Translation.** VictoriaMetrics serves `/api/v1/query` and `/api/v1/query_range`
with the same parameters. Equivalence is proven by capturing the raw
`RequestURI` both clients emit against the same `httptest.Server` and comparing
them as strings. That is the test that keeps this claim honest; a test that
re-derives the expected parameters from the package's own helper would only prove
the helper equals itself.

**MetricsQL extensions.** v0.1 validates the expression against a whitelist of
standard PromQL function names. A MetricsQL-only function is refused with an
error containing `vm_unsupported_function`. Silently truncating the expression
would let a diagnostic report a number computed from a different question than
the one asked.

**Results.** `*promquery.InstantResult`, with `resultType` and `result` passed
through unchanged. No new result type is introduced. 4xx / 5xx / timeout are
mapped per class and never swallowed.

**Timeout.** The 30s ceiling lives in `core/manager`
(`toolcore.QueryPromqlCallTimeout`), which the package may not import. The
package therefore copies the constant locally with a comment naming its source,
and the root module asserts the two are equal — so a change on either side turns
the build red instead of quietly widening or narrowing the window.

**Roster.** `scripts/dcell`'s roster counts the five `plugins/pig-ops` node
packages. This directory is not in it and no change is needed.

## D7 — Four test layers, and where each one can fail

| Layer | Location | Asserts |
| --- | --- | --- |
| unit | inside the module, `GOWORK=off go test ./...` | parameter mapping byte-equality; MetricsQL refusal; result normalisation; 401 / 5xx / timeout error mapping; manifest decode + validate + embed identity |
| contract | inside the module, over one shared `testdata/` fixture set | both clients, same fake backend, same responses → deep-equal `*InstantResult`. CI-blocking |
| integration | root module, `//go:build integration`, testcontainers | single-node `victoriametrics/victoria-metrics`, five PromQL templates, absolute difference 0 against the equivalent Prometheus expression |
| failure | unit + one real 5xx in integration | 401 / 5xx / timeout surface errors rather than empty results |

**Fixtures live in the module's `testdata/`, not `core/harness/fixtures/datasource/vm/`.**
The plan put them in the harness assembly position; putting them there would
make the plugin module's contract test depend on the harness module, which is
the cross-module dependency this slice is trying not to introduce. The deviation
is stated in the pull request.

**The integration target is `vm-integration-check`, not a fold-in to
`integration-check`.** `integration-check` is bound to the MySQL DSN semantics and
a MySQL service; a VictoriaMetrics container is a different dependency. It gets
its own CI job, and an environment without Docker skips it — stated in the pull
request rather than reported as green.

**Regression (acceptance line 3).** Root-module tests in `cmd/opskeeper`:
with `Backend` unset or `prometheus`, `Live()` is exactly the baseline set
(`opskeeper-datasource-prom` and nothing else), `Set.Prom()` is the first-party
client, and the selection logic is extracted into a function that can be called
directly. With `victoriametrics`, `Live()` contains
`opskeeper-datasource-victoriametrics` and does not contain the first-party
package. Dual activation is refused with `ErrDuplicate`.

---

## Risk register

| Risk | Severity | Control |
| --- | --- | --- |
| First Go module under `plugins/`; a later package copies it without `replace` or outside `PIG_MODULES` | high | D1 writes the rule into this document; the module is in `PIG_MODULES`; `module-standalone-check` builds it with `GOWORK=off` |
| Double activation / signal mutual exclusion fails | medium | D3's `ErrDuplicate` unit test |
| MetricsQL differences silently change a diagnosis | medium | D6's function-name whitelist, the contract comparison, and integration ±0 |
| "Half the queries go to VM, half to Prom" misread as a completed cutover | medium | D5's explicit startup log, documented boundary, interface assertions |
| First-party Prometheus regression | medium | D7's `Live()` / `Set` bit-for-bit assertions |
| Contract drift (30s ceiling, observed fields) | low | D6's constant-equality assertion |

## Rollback

Single pull request, no data migration, default behaviour unchanged. `git revert`
is the rollback. In a live deployment, follow the four-step causal chain in
§3.7 of the recovery plan: switch the configuration back to `prometheus` →
unload → restart → verify `Set.Prom()` is the first-party client. This slice
delivers only as far as the configuration switch; the marketplace uninstall
channel is out of scope.

## Recorded for later decisions

1. **The disk-loaded manifest channel.** `OPSKEEPER_MANAGER_PLUGINS_DIR` has no
   deployment or release wiring. The target shape needs the deployment task
   first, and that task must also decide which policy governs the directory
   (currently `ManagerPolicy()` is reused).
2. **One-shot cutover of the whole Prom query surface.** D5's assertions fix the
   interface path; the cutover itself has to review each of the seven consumers
   for concrete-type dependence and degradation semantics.
3. **Counting third-party packages.** `dcell`'s roster covers `plugins/pig-ops`
   only; whether it grows a row belongs to the plugin-ecosystem task.
4. **Self-observability.** latency / error rate / point count are left as log
   fields here; wiring them into the host's metrics surface is a separate task.