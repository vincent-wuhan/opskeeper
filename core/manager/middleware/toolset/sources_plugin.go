package toolset

import (
	"context"
	"log/slog"
	"sort"

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

// adapterPolicy is the adapter host's default admission ceiling: the policy
// written for the first-party families.
//
// It is a thin wrapper over AdapterPolicy so the ceiling and the granted set
// have one definition, and so a test or a host that needs the same ceiling
// with a different grant set builds it the same way.
func adapterPolicy() kernel.Policy {
	return AdapterPolicy(defaultAdapterScopes())
}

// AdapterPolicy builds the admission ceiling for a control-plane host that
// hosts adapter families, with granted as its granted scope set.
//
// L2 and single-namespace are fixed and not a parameter: an adapter reads and
// may write OpsKeeper-managed state through the closed loop, but nothing an
// adapter does is cluster-scoped, and the blast radius a family may request
// stops at one namespace.
//
// granted is a host-side assemblable value, not a package-side claimable one.
// This function reads it and hands it to the gate; no plugin can widen it by
// declaring more, and Satisfies stays fail-closed on an unknown scope. Widening
// it is therefore always a host-code (assembly-layer) diff — one that a
// reviewer can see — never a plugin's self-declaration. The default grant set
// for the first-party families is defaultAdapterScopes().
func AdapterPolicy(granted domain.Scopes) kernel.Policy {
	return kernel.Policy{
		Target:         domain.TargetManager,
		MaxSafetyLevel: domain.SafetyL2,
		MaxBlastRadius: domain.RadiusSingleNS,
		GrantedScopes:  granted,
	}
}

// defaultAdapterScopes is the scope set the adapter host grants by default:
// the eight scopes the first-party families declare, in their existing order,
// plus topology.read and cloud.read for the families an out-of-tree package
// contributes.
//
// The two additions are read-only. topology.read is the topology view an
// adapter-backed investigator reads to place a fault; cloud.read is the cloud
// vendor's open-API read permission and is deliberately a separate scope from
// host.read (see domain.ScopeCloudRead). There is no cloud.write here: cloud
// writes are decided in the write-operation slice, not by a default grant.
func defaultAdapterScopes() domain.Scopes {
	return domain.Scopes{
		domain.ScopeDBRead, domain.ScopeDBWrite,
		domain.ScopeK8sRead, domain.ScopeK8sExec,
		domain.ScopeMQRead, domain.ScopeMQWrite,
		domain.ScopeHostRead, domain.ScopeHostWrite,
		domain.ScopeTopologyRO,
		domain.ScopeCloudRead,
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
// property of what the host admitted, not of a slice literal.
//
// Order is contract, not accident:
//   - the first-party families come first, in firstPartySources() order, as a
//     strict prefix. This is the boot-wiring order and the order the
//     capability vocabulary is reported in, so it must not move.
//   - any further families follow the first-party block, ordered by family
//     name lexicographically. The sort is explicit rather than inherited from
//     the registry's internal ordering, and it does not depend on Go package
//     init order, so two calls with the same input return the same list.
//
// On the default path — no out-of-tree contributions — the second group is
// empty and this returns exactly the eight first-party families in their
// first-party order. Sources() does not expose a way for a package to add to
// the list: a third-party family reaches it through the assembly layer that
// hosts the kernel, which is a separate, later decision.
func Sources() []Source {
	return sourcesFrom(nil)
}

// sourcesFrom returns Sources() plus the families a caller contributes.
//
// The parameter is the out-of-tree contribution point: every entry goes through
// the same host.Activate + adapterManifest gate the first-party families go
// through — same admission, same duplicate semantics, one registry, no parallel
// registration channel. That is the whole point of the parameter: a contributed
// family is indistinguishable from a first-party one by the time it is read back
// out, which is what makes the third-party path exercised by the same code.
//
// It is unexported because no caller outside this package exists yet. S0 fixes
// the path; the exported entry point lands with the first real candidate
// package, so that the API it freezes is one somebody has actually used.
func sourcesFrom(thirdParty []Source) []Source {
	host := kernel.NewHost(kernel.NewRegistry(), adapterPolicy(), slog.New(slog.DiscardHandler))

	firstParty := firstPartySources()
	for _, src := range firstParty {
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

	// A contributed family takes the same gate. A name that collides with a
	// family already in the registry fails activation here (ErrDuplicate), so
	// the collision is dropped rather than shadowing the family that won it —
	// the same refusal the first-party loop already makes.
	contributed := make([]Source, 0, len(thirdParty))
	for _, src := range thirdParty {
		if err := host.Activate(context.Background(), adapterManifest(src.Name), familyPlugin{src: src}); err != nil {
			continue
		}
		contributed = append(contributed, src)
	}

	byName := map[Family]Source{}
	for _, e := range host.Registry().All(kernel.KindAdapter) {
		if s, ok := e.Impl.(Source); ok {
			byName[s.Name] = s
		}
	}

	out := make([]Source, 0, len(firstParty)+len(contributed))
	owned := make(map[Family]bool, len(firstParty))
	for _, src := range firstParty {
		owned[src.Name] = true
		if s, ok := byName[src.Name]; ok {
			out = append(out, s)
		}
	}
	rest := make([]Source, 0, len(contributed))
	for _, src := range contributed {
		if owned[src.Name] {
			continue
		}
		if s, ok := byName[src.Name]; ok {
			rest = append(rest, s)
		}
	}
	sort.Slice(rest, func(i, j int) bool { return rest[i].Name < rest[j].Name })
	return append(out, rest...)
}
