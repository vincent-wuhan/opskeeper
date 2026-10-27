package toolset

// This file is the reference implementation of a third-party adapter family's
// contribution to the adapter host — 参考实现，非产品包. It exists so a package
// author has a compiling example to copy, and so the shape the host admits is
// asserted by a test rather than described in a comment.
//
// Two rules are load-bearing here and are stated in code rather than prose:
//
//   - The contribution declares kernel.Contribution and nothing else. It does
//     not touch toolset's internals, it does not reach into a registry, and it
//     does not import any first-party adapter package: a family brings its own
//     backend. The only shared vocabulary is the scope it asks for.
//   - The scope it asks for is what makes it admissible. cloud.read is in the
//     adapter host's default grant set, so this manifest passes Admit with no
//     host change. A family that asked for a scope outside that set would be
//     refused, and widening the set is a host-code diff a reviewer sees — not
//     something this package can do to itself.

import (
	"context"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/floor/kernel"
	"github.com/vincent-wuhan/opskeeper/core/floor/pluginmanifest"
	middlewareregistry "github.com/vincent-wuhan/opskeeper/core/manager/middleware/registry"
)

// exampleOps is a stand-in for a real family package. Its name must not be a
// prefix of, nor prefixed by, any first-party family name: family names are
// the prefix of every tool name in that family, so a prefix relationship would
// silently change what ParseFamily resolves for existing tools.
type exampleOps struct{ src Source }

const (
	exampleOpsPlugin = "opskeeper-adapter-exampleops"
	exampleOpsFamily = Family("exampleops")
	exampleOpsScope  = domain.ScopeCloudRead
	exampleOpsDSNEnv = "OPSKEEPER_LOOP_EXAMPLEOPS_DSN"
)

// PluginName is the contribution's manifest name. A rollback removes the
// family by this name, so it must identify the package, not the family it
// happens to register today.
func (exampleOps) PluginName() string { return exampleOpsPlugin }

// Register contributes exactly one entry: the family under KindAdapter. The
// kernel knows a plugin registered one entry of a kind, which is what makes an
// adapter family the same shape of thing as a datasource or a provider.
func (e exampleOps) Register(r kernel.Registrar) error {
	return r.Register(kernel.KindAdapter, string(e.src.Name), e.src)
}

// source is what the family contributes: the tools to register, and the
// environment variable the closed loop reads its DSN from.
func (e exampleOps) source() Source {
	return Source{
		Name: e.src.Name,
		Env:  e.src.Env,
		Wire: func(_ context.Context, reg *middlewareregistry.Registry, dsn string) (func(), error) {
			// A real family constructs its client here, connects it when dsn
			// is non-empty, and returns a closer. This reference registers
			// nothing, so it has nothing to close.
			_ = reg
			_ = dsn
			return func() {}, nil
		},
	}
}

// manifest is the minimum a family package must declare to be admitted by the
// adapter host. It is a reader: one read-only scope, no write capability, no
// approval requirement, and no blast radius beyond this plugin itself.
func (e exampleOps) manifest() pluginmanifest.Plugin {
	return pluginmanifest.Plugin{
		Manifest: domain.PluginManifest{
			APIVersion: domain.PluginAPIVersion,
			Kind:       domain.PluginKind,
			Metadata: domain.PluginMeta{
				Name:    e.PluginName(),
				Version: "0.1.0",
				Vendor:  "example",
			},
			Spec: domain.PluginSpec{
				Targets:        domain.Targets{domain.TargetManager},
				SafetyLevel:    domain.SafetyL1,
				Capabilities:   []domain.ToolClass{domain.ClassRead},
				RequiredScopes: domain.Scopes{exampleOpsScope},
				Audit:          domain.AuditPolicy{Emits: true, Mutates: false},
				Approval:       domain.ApprovalPolicy{Required: false, MaxBlastRadius: domain.RadiusNone},
				Install:        domain.InstallPolicy{Strategy: domain.InstallRolling},
			},
		},
	}
}

func exampleOpsContribution() exampleOps {
	return exampleOps{src: Source{Name: exampleOpsFamily, Env: exampleOpsDSNEnv}}
}

func TestTheReferenceContributionIsAdmittedAndRegisters(t *testing.T) {
	// A family's whole shape, end to end, on the host that will run it. If the
	// adapter host ever stops admitting a plain read-only cloud reader, this
	// fails before any candidate package does.
	c := exampleOpsContribution()

	host := kernel.NewHost(kernel.NewRegistry(), adapterPolicy(), nil)
	if err := host.Activate(context.Background(), c.manifest(), c); err != nil {
		t.Fatalf("the reference contribution is refused by the adapter host: %v", err)
	}

	e, ok := host.Registry().Lookup(kernel.KindAdapter, string(c.src.Name))
	if !ok {
		t.Fatal("the reference contribution registered nothing")
	}
	if e.Plugin != exampleOpsPlugin {
		t.Errorf("entry plugin = %q; want %q", e.Plugin, exampleOpsPlugin)
	}
	if _, ok := e.Impl.(Source); !ok {
		t.Error("the registered impl must be a Source")
	}

	// It rolls back by name, so a family that is misbehaving can be withdrawn
	// without restarting the host.
	if err := host.Deactivate(context.Background(), exampleOpsPlugin); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	if _, ok := host.Registry().Lookup(kernel.KindAdapter, string(c.src.Name)); ok {
		t.Error("the entry survived deactivation")
	}
}

func TestTheReferenceFamilyNameIsUsableAlongsideTheFirstPartyOnes(t *testing.T) {
	assertNoFamilyNameCollision(t, exampleOpsFamily)
}

func TestTheReferenceContributionNeedsNoHostChangeToBeAdmitted(t *testing.T) {
	// The point of cloud.read being in the default grant set: a package that
	// only reads cloud state is a contribution, not a host modification.
	c := exampleOpsContribution()
	host := kernel.NewHost(kernel.NewRegistry(), adapterPolicy(), nil)
	if err := host.Admit(c.manifest()); err != nil {
		t.Fatalf("Admit failed under the default adapter policy: %v", err)
	}
	if !c.manifest().Manifest.Spec.RequiredScopes.Has(exampleOpsScope) {
		t.Fatal("the reference manifest must declare the scope it needs")
	}
}
