// Package datasource turns OpsKeeper's observability query backends into
// microkernel plugins.
//
// Before this package, the three query backends were three concrete clients
// constructed inline in main() and handed to the tool registry as three
// separate struct fields — PromQuerier, LogQuerier, TraceQuerier. Adding a
// fourth backend of any signal (VictoriaMetrics behind the Prometheus
// protocol, Elasticsearch behind LogQL, a self-hosted store) meant a fourth
// field, a fourth nil check, and a fourth edit to the assembly layer.
//
// Now each backend is a plugin: it registers one entry in the kernel under
// its signal name, and the registry is the only thing that knows how to turn
// "metrics" into a querier. The tool registry asks the Set for a querier and
// gets whatever the admitted plugin provided, or nil — which is the same
// graceful degradation the three separate fields gave, expressed once.
//
// The three first-party backends are preinstalled plugins: they are compiled
// into the binary rather than shipped as a tarball, but they are admitted
// through the same gate a third-party package is, so the third-party path is
// exercised on every boot rather than only in a test.
package datasource

import (
	"context"
	"log/slog"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/floor/kernel"
	"github.com/vincent-wuhan/opskeeper/core/floor/pluginmanifest"
	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/toolcore"
)

// Signal names. One backend per signal registers under the signal's name.
const (
	NameProm  = "prom"
	NameLoki  = "loki"
	NameTempo = "tempo"
)

// Plugin names. A contribution's PluginName is the manifest name of the
// package it belongs to, and it is the key a rollback removes.
const (
	PluginProm  = "opskeeper-datasource-prom"
	PluginLoki  = "opskeeper-datasource-loki"
	PluginTempo = "opskeeper-datasource-tempo"
)

// Prom is the first-party metrics datasource: the Prometheus HTTP client,
// registered as a plugin contribution.
type Prom struct{ Q toolcore.PromQuerier }

func (Prom) PluginName() string { return PluginProm }

func (d Prom) Register(r kernel.Registrar) error {
	return r.Register(kernel.KindDatasource, NameProm, d.Q)
}

// Logs is the first-party logs datasource: the Loki client.
type Logs struct{ Q toolcore.LogQuerier }

func (Logs) PluginName() string { return PluginLoki }

func (d Logs) Register(r kernel.Registrar) error {
	return r.Register(kernel.KindDatasource, NameLoki, d.Q)
}

// Traces is the first-party traces datasource: the Tempo client.
type Traces struct{ Q toolcore.TraceQuerier }

func (Traces) PluginName() string { return PluginTempo }

func (d Traces) Register(r kernel.Registrar) error {
	return r.Register(kernel.KindDatasource, NameTempo, d.Q)
}

// Set is the resolved view of the datasources a host admitted: one querier
// per signal, nil when no plugin provided one. It is what the tool registry
// consumes, and it is deliberately the same shape the three separate fields
// had, so nothing downstream changes.
type Set struct {
	prom  toolcore.PromQuerier
	logs  toolcore.LogQuerier
	trace toolcore.TraceQuerier
}

// Prom returns the metrics querier, or nil when none is active.
func (s Set) Prom() toolcore.PromQuerier { return s.prom }

// Logs returns the logs querier, or nil when none is active.
func (s Set) Logs() toolcore.LogQuerier { return s.logs }

// Traces returns the traces querier, or nil when none is active.
func (s Set) Traces() toolcore.TraceQuerier { return s.trace }

// Resolve reads the three queriers out of a registry. A signal with no
// admitted plugin resolves to nil, which the tool registry reads as "do not
// register this tool" — the same degradation the nil checks in main() gave.
func Resolve(reg *kernel.Registry) Set {
	var s Set
	if e, ok := reg.Lookup(kernel.KindDatasource, NameProm); ok {
		s.prom, _ = e.Impl.(toolcore.PromQuerier)
	}
	if e, ok := reg.Lookup(kernel.KindDatasource, NameLoki); ok {
		s.logs, _ = e.Impl.(toolcore.LogQuerier)
	}
	if e, ok := reg.Lookup(kernel.KindDatasource, NameTempo); ok {
		s.trace, _ = e.Impl.(toolcore.TraceQuerier)
	}
	return s
}

// Backends is the set of first-party backends to activate. A nil field is a
// backend the deployment did not configure; it is not activated, and its
// signal resolves to nil.
type Backends struct {
	Prom   toolcore.PromQuerier
	Logs   toolcore.LogQuerier
	Traces toolcore.TraceQuerier
}

// Activate admits and activates whichever backends are configured, into a
// host the caller owns, and returns the resolved Set.
//
// It is the one place the assembly layer needs: hand it the constructed
// clients (or nils), get back the queriers the tool registry consumes. The
// host is returned so a caller can report what is live and roll a backend
// back by name.
func Activate(host *kernel.Host, b Backends) (Set, error) {
	if b.Prom != nil {
		if err := host.Activate(context.Background(), manifestFor(PluginProm, NameProm), Prom{Q: b.Prom}); err != nil {
			return Set{}, err
		}
	}
	if b.Logs != nil {
		if err := host.Activate(context.Background(), manifestFor(PluginLoki, NameLoki), Logs{Q: b.Logs}); err != nil {
			return Set{}, err
		}
	}
	if b.Traces != nil {
		if err := host.Activate(context.Background(), manifestFor(PluginTempo, NameTempo), Traces{Q: b.Traces}); err != nil {
			return Set{}, err
		}
	}
	return Resolve(host.Registry()), nil
}

// ManagerPolicy is the admission ceiling for a control-plane host that hosts
// datasource plugins. It is exported so the assembly layer and its tests
// build the same host.
//
// The ceiling is L2 because a control-plane datasource may write OpsKeeper
// state (a saved query, a cached result) even though the three first-party
// backends are L1. The granted scopes are the read scopes a datasource
// declares.
func ManagerPolicy() kernel.Policy {
	return kernel.Policy{
		Target:         domain.TargetManager,
		MaxSafetyLevel: domain.SafetyL2,
		MaxBlastRadius: domain.RadiusSingleNS,
		GrantedScopes: domain.Scopes{
			domain.ScopeMetricsRO,
			domain.ScopeDBRead,
			domain.ScopeAlertRO,
			domain.ScopeTopologyRO,
		},
	}
}

// NewHost builds a control-plane host with the datasource admission policy.
func NewHost(log *slog.Logger) *kernel.Host {
	return kernel.NewHost(kernel.NewRegistry(), ManagerPolicy(), log)
}

// manifestFor synthesizes the governance manifest of a preinstalled
// datasource plugin.
//
// These packages are compiled in, so there is no pig-ops.yaml on disk to
// load — but they are still admitted through the same gate a third-party
// package is, which is what keeps the third-party path exercised. The
// manifest is minimal and read-only on purpose: a datasource reads.
func manifestFor(plugin, signal string) pluginmanifest.Plugin {
	return pluginmanifest.Plugin{
		Manifest: domain.PluginManifest{
			APIVersion: domain.PluginAPIVersion,
			Kind:       domain.PluginKind,
			Metadata: domain.PluginMeta{
				Name:    plugin,
				Version: "1.0.0",
				Vendor:  "opskeeper",
			},
			Spec: domain.PluginSpec{
				Targets:        domain.Targets{domain.TargetManager},
				SafetyLevel:    domain.SafetyL1,
				Capabilities:   []domain.ToolClass{domain.ClassRead},
				RequiredScopes: domain.Scopes{domain.ScopeMetricsRO},
				Audit:          domain.AuditPolicy{Emits: true, Mutates: false},
				Approval:       domain.ApprovalPolicy{Required: false},
				Install:        domain.InstallPolicy{Strategy: domain.InstallRolling},
			},
		},
	}
}

// Signal reports the signal a plugin name serves, for diagnostics.
func Signal(plugin string) (string, bool) {
	switch plugin {
	case PluginProm:
		return NameProm, true
	case PluginLoki:
		return NameLoki, true
	case PluginTempo:
		return NameTempo, true
	default:
		return "", false
	}
}
