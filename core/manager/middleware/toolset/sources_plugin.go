package toolset

import (
	"context"
	"log/slog"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/floor/kernel"
	"github.com/vincent-wuhan/opskeeper/core/floor/pluginmanifest"
)

// adapterPluginPrefix is the manifest-name prefix every first-party adapter
// family's plugin carries. It is a prefix rather than a literal so the
// family name is part of the plugin name and a rollback names one family.
const adapterPluginPrefix = "opskeeper-adapter-"

// familyPlugin is the microkernel contribution for one adapter family.
//
// A family contributes exactly one entry — its Source — under KindAdapter,
// keyed by the family name. The kernel does not know what an adapter is; it
// knows a plugin registered one entry of a kind, which is what makes an
// adapter family the same shape of thing as a datasource or a provider.
type familyPlugin struct{ src Source }

func (f familyPlugin) PluginName() string { return adapterPluginPrefix + string(f.src.Name) }

func (f familyPlugin) Register(r kernel.Registrar) error {
	return r.Register(kernel.KindAdapter, string(f.src.Name), f.src)
}

// adapterPolicy is the admission ceiling for a control-plane host that hosts
// adapter families.
//
// L2 and single-namespace: an adapter reads and may write OpsKeeper-managed
// state through the closed loop, but nothing here is cluster-scoped, and the
// blast radius a family may request stops at one namespace. The scopes are
// the read/write scopes the first-party families declare.
func adapterPolicy() kernel.Policy {
	return kernel.Policy{
		Target:         domain.TargetManager,
		MaxSafetyLevel: domain.SafetyL2,
		MaxBlastRadius: domain.RadiusSingleNS,
		GrantedScopes: domain.Scopes{
			domain.ScopeDBRead, domain.ScopeDBWrite,
			domain.ScopeK8sRead, domain.ScopeK8sExec,
			domain.ScopeMQRead, domain.ScopeMQWrite,
			domain.ScopeHostRead, domain.ScopeHostWrite,
		},
	}
}

// adapterManifest synthesizes the governance manifest of a first-party
// adapter family.
//
// The families are compiled in, so there is no pig-ops.yaml to load, but
// they are admitted through the same gate a third-party adapter package is
// — which is what keeps the third-party path exercised on every boot.
func adapterManifest(fam Family) pluginmanifest.Plugin {
	return pluginmanifest.Plugin{
		Manifest: domain.PluginManifest{
			APIVersion: domain.PluginAPIVersion,
			Kind:       domain.PluginKind,
			Metadata:   domain.PluginMeta{Name: adapterPluginPrefix + string(fam), Version: "1.0.0", Vendor: "opskeeper"},
			Spec: domain.PluginSpec{
				Targets:      domain.Targets{domain.TargetManager},
				SafetyLevel:  domain.SafetyL2,
				Capabilities: []domain.ToolClass{domain.ClassRead, domain.ClassWrite},
				RequiredScopes: domain.Scopes{
					domain.ScopeDBRead, domain.ScopeK8sRead, domain.ScopeMQRead, domain.ScopeHostRead,
				},
				Audit:    domain.AuditPolicy{Emits: true, Mutates: false},
				Approval: domain.ApprovalPolicy{Required: true, MaxBlastRadius: domain.RadiusSingleNS},
				Install:  domain.InstallPolicy{Strategy: domain.InstallRolling},
			},
		},
	}
}

// Sources is the single list of adapter families this build can register.
//
// It is derived from the microkernel registry rather than returned as a
// literal. Each first-party family is activated as a KindAdapter plugin into
// a fresh host, and the list is read back out — so the family set is a
// property of what the host admitted, not of a slice literal, and a
// third-party adapter family a package registers lands in the same list.
//
// Order is the order the families were contributed (the first-party order
// below), which is the order they are reported and, for the connected path,
// wired at boot.
func Sources() []Source {
	host := kernel.NewHost(kernel.NewRegistry(), adapterPolicy(), slog.New(slog.DiscardHandler))
	for _, src := range firstPartySources() {
		// A first-party family that fails admission is a bug in this file,
		// not a deployment condition: the policy above is written to admit
		// exactly these. Dropping it would silently shrink the build's
		// capability set, which is the failure this whole package exists to
		// prevent, so it is surfaced by omission from the returned list and
		// caught by the catalog test rather than swallowed here.
		if err := host.Activate(context.Background(), adapterManifest(src.Name), familyPlugin{src: src}); err != nil {
			continue
		}
	}

	// Read back in the order the families were contributed. The registry
	// orders entries by name; the family order is the first-party order, so
	// map by name and walk the first-party slice.
	byName := map[Family]Source{}
	for _, e := range host.Registry().All(kernel.KindAdapter) {
		if s, ok := e.Impl.(Source); ok {
			byName[s.Name] = s
		}
	}
	out := make([]Source, 0, len(byName))
	for _, src := range firstPartySources() {
		if s, ok := byName[src.Name]; ok {
			out = append(out, s)
		}
	}
	return out
}
