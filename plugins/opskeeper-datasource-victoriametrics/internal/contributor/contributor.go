// Package contributor is the VictoriaMetrics package's registration.
//
// It is the smallest possible contribution: one kind, one signal name, one
// value. The signal name is `prom` — the same name the first-party Prometheus
// package registers under — and that is deliberate rather than convenient.
// OpsKeeper asks "who answers metrics?" by name, so a new backend that
// introduced its own name would need a second signal in the closed set, a
// second entry in every capability listing, and a way for an operator to
// choose between them. Reusing the name makes the backend swap a configuration
// decision, which is what it is.
//
// The cost of reusing the name is that two packages can now want the same
// entry, and the registry refuses the second one (ErrDuplicate) rather than
// letting it overwrite the first. That refusal is the fail-closed property the
// whole single-activation story rests on, so it is asserted directly rather
// than assumed.
package contributor

import (
	"github.com/vincent-wuhan/opskeeper/core/floor/kernel"

	"github.com/vincent-wuhan/opskeeper/plugins/opskeeper-datasource-victoriametrics/internal/vmclient"
)

// PluginName is the package's name as it appears in the manifest. The host
// checks it against the manifest's own name before registering anything, so a
// mismatch fails before an entry exists rather than after.
const PluginName = "opskeeper-datasource-victoriametrics"

// SignalProm is the signal this package serves. It is the first-party
// constant's value, written out rather than imported: importing
// core/manager/biz/aiops/datasource to obtain a string would put the control
// plane in this module's dependency graph, which is the one thing this module
// must never do. A test in the root module asserts the two agree.
const SignalProm = "prom"

// VictoriaMetrics is the contribution: a PromQL querier registered under the
// `prom` signal.
type VictoriaMetrics struct {
	// Q is the querier this package provides. It is typed by the method set
	// it has, not by an OpsKeeper interface — see the package comment on why.
	Q *vmclient.Client
}

// PluginName reports the package this contribution belongs to.
func (VictoriaMetrics) PluginName() string { return PluginName }

// Register adds the querier to the registry under the `prom` signal.
func (c VictoriaMetrics) Register(r kernel.Registrar) error {
	return r.Register(kernel.KindDatasource, SignalProm, c.Q)
}
