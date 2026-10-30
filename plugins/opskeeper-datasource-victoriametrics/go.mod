// Module opskeeper-datasource-victoriametrics is the first third-party
// datasource package OpsKeeper ships: a VictoriaMetrics backend registered
// under the `prom` signal.
//
// It is a separate module, and that is the point. A third-party package that
// lived in the control plane's module graph could import the control plane's
// business modules, and every vendor of OpsKeeper would inherit them. The
// boundary this module draws is narrow on purpose:
//
//   - core/base/pkg/promquery — the result type PromQL consumers already speak.
//     Returning it unchanged is what lets query_promql and its siblings keep
//     working without a type change.
//   - core/base/pkg/promauth — TLS and per-request credential injection.
//   - core/floor/kernel — the closed extension-point set. KindDatasource is
//     where a metrics backend goes; no new kind is needed and none is added.
//   - sdk — Decode for the pig-ops.yaml this module embeds.
//
// What it must NOT import is the part that matters: core/manager and every
// core/pig* module. A package that could reach the registry, the broker or the
// gate could answer its own questions, and a package that can answer its own
// questions is a second policy engine nobody reviews. The consequence is that
// toolcore.PromQuerier is satisfied structurally rather than by assertion — the
// method set is the contract, and the assertions live in the root module where
// core/manager is legitimately importable.
//
// The replace block is how a workspace-free build resolves its siblings.
// go.work is gitignored, CI checks out without one, and a release builds each
// module with GOWORK=off, so a module that only resolves inside a workspace
// does not build where it has to build. Precedent: the PiG extension modules
// under core/pig/extensions.
module github.com/vincent-wuhan/opskeeper/plugins/opskeeper-datasource-victoriametrics

go 1.26.0

require (
	github.com/vincent-wuhan/opskeeper/core/base v0.0.0
	github.com/vincent-wuhan/opskeeper/core/floor v0.0.0
	github.com/vincent-wuhan/opskeeper/sdk v0.0.0
)

require (
	github.com/vincent-wuhan/opskeeper/core v0.0.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

replace (
	github.com/vincent-wuhan/opskeeper/core => ../../core
	github.com/vincent-wuhan/opskeeper/core/base => ../../core/base
	github.com/vincent-wuhan/opskeeper/core/edge => ../../core/edge
	github.com/vincent-wuhan/opskeeper/core/floor => ../../core/floor
	github.com/vincent-wuhan/opskeeper/sdk => ../../sdk
)
