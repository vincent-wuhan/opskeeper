package main

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/base/pkg/promquery"
	"github.com/vincent-wuhan/opskeeper/core/floor/config"
	"github.com/vincent-wuhan/opskeeper/core/floor/kernel"
	"github.com/vincent-wuhan/opskeeper/core/floor/pluginmanifest"
	aiopsdatasource "github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/datasource"

	vmplugin "github.com/vincent-wuhan/opskeeper/plugins/opskeeper-datasource-victoriametrics"
)

// The default path has to be the baseline. Not "equivalent to" — identical: the
// same plugins live, the same one does not, and nothing about the first-party
// Prometheus path was touched to get there. A test that only checked "the
// plugin list is non-empty" would pass on a wiring that quietly stopped
// activating first-party Prometheus.
func TestTheDefaultPlanActivatesNoThirdPartyPackage(t *testing.T) {
	for _, backend := range []string{"", config.PromBackendPrometheus} {
		plan, err := planPromBackend(backend)
		if err != nil {
			t.Fatalf("planPromBackend(%q): %v", backend, err)
		}
		if plan.ThirdParty != "" {
			t.Errorf("planPromBackend(%q) names third-party package %q; the default "+
				"path must be the one that ran before this field existed", backend, plan.ThirdParty)
		}
		if plan.Backend != config.PromBackendPrometheus {
			t.Errorf("planPromBackend(%q).Backend = %q, want %q",
				backend, plan.Backend, config.PromBackendPrometheus)
		}
	}
}

func TestTheVictoriaMetricsPlanNamesThePackage(t *testing.T) {
	plan, err := planPromBackend(config.PromBackendVictoriaMetrics)
	if err != nil {
		t.Fatalf("planPromBackend: %v", err)
	}
	if plan.ThirdParty != vmplugin.PluginName {
		t.Errorf("ThirdParty = %q, want %q", plan.ThirdParty, vmplugin.PluginName)
	}
}

// An unrecognised backend stops the manager. Silently using Prometheus for a
// deployment that asked for VictoriaMetrics is the worst outcome available:
// every dashboard works, every number comes from the wrong server, and nothing
// says so.
func TestAnUnknownBackendIsRefusedRatherThanFallenBackTo(t *testing.T) {
	for _, backend := range []string{"vm", "VictoriaMetrics", "elastic", "es", "prometheus-sd"} {
		plan, err := planPromBackend(backend)
		if err == nil {
			t.Errorf("planPromBackend(%q) = %+v, want an error", backend, plan)
			continue
		}
		if !strings.Contains(err.Error(), backend) {
			t.Errorf("error %q does not name the offending value %q", err, backend)
		}
	}
}

// A whitespace-padded value is trimmed rather than rejected: an env var written
// with a trailing newline in a YAML block scalar is a typo in the deployment
// file, not a different backend.
func TestSurroundingWhitespaceIsNotADifferentBackend(t *testing.T) {
	plan, err := planPromBackend("  victoriametrics\n")
	if err != nil {
		t.Fatalf("planPromBackend: %v", err)
	}
	if plan.ThirdParty != vmplugin.PluginName {
		t.Errorf("ThirdParty = %q, want %q", plan.ThirdParty, vmplugin.PluginName)
	}
}

// The residual-surface list is the only place an operator is told which parts of
// the control plane did not follow the switch. It is asserted here so that a
// surface added to the split without being listed is caught by review rather
// than by a user filing the bug.
func TestTheResidualSurfaceListNamesTheSixReadersThatDidNotSwitch(t *testing.T) {
	got := promResidualSurfaceLog()
	if len(got) != 6 {
		t.Errorf("residual surfaces = %d, want 6: %v", len(got), got)
	}
	for _, want := range []string{
		"integrations probe", "metric-domain", "alert preview",
		"report", "Monitor page proxy", "alert-rule",
	} {
		if !containsFold(got, want) {
			t.Errorf("residual surface list is missing %q: %v", want, got)
		}
	}
	// A sorted list is a stable log line; an unsorted one changes order every
	// time somebody adds an entry, and a log that reorders itself is a log
	// nobody greps.
	sorted := append([]string(nil), got...)
	sort.Strings(sorted)
	if !reflect.DeepEqual(got, sorted) {
		t.Errorf("promResidualSurfaceLog() is not sorted: %v", got)
	}
}

// The manifest the package ships admits under the real control-plane policy.
// The plugin module cannot import core/manager to say so — that is the boundary
// it exists to hold — which makes this the authoritative version of the check
// its restated copy approximates.
func TestThePackageManifestAdmitsUnderManagerPolicy(t *testing.T) {
	p := mustVMManifest(t)
	if err := aiopsdatasource.NewHost(nil).Admit(p); err != nil {
		t.Fatalf("Admit under ManagerPolicy(): %v", err)
	}
}

// The whole single-activation story in one test: the default leaves the
// first-party plugin exactly as it was, the third-party selection replaces it
// rather than joining it, and asking for both is refused by the registry.
//
// "Refused rather than joining" is the property. A host that kept both would
// look healthy — one package is live, the inventory is populated — and would
// answer queries from whichever registered first, which is a failure nobody
// sees until the numbers are wrong.
func TestTheTwoBackendsAreMutuallyExclusiveInOneHost(t *testing.T) {
	ctx := t.Context()

	t.Run("default activates first-party only", func(t *testing.T) {
		host := aiopsdatasource.NewHost(nil)
		set, err := aiopsdatasource.Activate(host, aiopsdatasource.Backends{Prom: stubProm{}})
		if err != nil {
			t.Fatalf("Activate: %v", err)
		}
		if live := host.Live(); !reflect.DeepEqual(live, []string{aiopsdatasource.PluginProm}) {
			t.Errorf("Live() = %v, want exactly [%s]", live, aiopsdatasource.PluginProm)
		}
		if set.Prom() == nil {
			t.Error("Set.Prom() is nil; the default path lost its metrics querier")
		}
	})

	t.Run("victoriametrics replaces first-party", func(t *testing.T) {
		host := aiopsdatasource.NewHost(nil)
		// The third-party package is admitted into the same host first and
		// the first-party backend is left nil — which is what main() does.
		if err := host.Activate(ctx, mustVMManifest(t), vmplugin.Contribution(nil)); err != nil {
			t.Fatalf("activate victoriametrics: %v", err)
		}
		set, err := aiopsdatasource.Activate(host, aiopsdatasource.Backends{})
		if err != nil {
			t.Fatalf("Activate: %v", err)
		}
		live := host.Live()
		if !reflect.DeepEqual(live, []string{vmplugin.PluginName}) {
			t.Errorf("Live() = %v, want exactly [%s]", live, vmplugin.PluginName)
		}
		for _, name := range live {
			if name == aiopsdatasource.PluginProm {
				t.Error("the first-party prometheus plugin is live alongside victoriametrics")
			}
		}
		// The plugin registers a *vmclient.Client, not a *promquery.Client.
		// Resolve() does a runtime assertion, so this is where a structural
		// mismatch would surface: a nil interface here means the tool registry
		// silently stopped registering query_promql.
		if set.Prom() == nil {
			t.Error("Set.Prom() is nil; the victoriametrics querier did not satisfy " +
				"toolcore.PromQuerier, so query_promql would not be registered")
		}
	})

	t.Run("both at once is refused", func(t *testing.T) {
		host := aiopsdatasource.NewHost(nil)
		if err := host.Activate(ctx, mustVMManifest(t), vmplugin.Contribution(nil)); err != nil {
			t.Fatalf("activate victoriametrics: %v", err)
		}
		_, err := aiopsdatasource.Activate(host, aiopsdatasource.Backends{Prom: stubProm{}})
		if err == nil {
			t.Fatal("activating both backends succeeded; one signal has one backend")
		}
		if !errors.Is(err, kernel.ErrDuplicate) {
			t.Errorf("err = %v, want it to wrap kernel.ErrDuplicate", err)
		}
	})
}

func mustVMManifest(t *testing.T) pluginmanifest.Plugin {
	t.Helper()
	p, err := vmplugin.Manifest()
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	return p
}

// stubProm stands in for the first-party *promquery.Client. It is a value
// rather than a nil interface so the "backend was configured" branch is
// genuinely taken — a typed-nil would take it too, and that is the trap the
// main.go comment describes.
type stubProm struct{}

func (stubProm) Query(context.Context, string, time.Time) (*promquery.InstantResult, error) {
	return nil, nil
}

func (stubProm) QueryRange(context.Context, string, time.Time, time.Time, time.Duration) (*promquery.InstantResult, error) {
	return nil, nil
}

func containsFold(hay []string, needle string) bool {
	for _, s := range hay {
		if strings.Contains(strings.ToLower(s), strings.ToLower(needle)) {
			return true
		}
	}
	return false
}
