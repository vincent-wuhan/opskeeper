package datasource

import (
	"context"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/base/pkg/logquery"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/promquery"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/tracequery"
	"github.com/vincent-wuhan/opskeeper/core/floor/kernel"
)

// ── fakes ───────────────────────────────────────────────────────────────────

type fakeProm struct{ tag string }

func (f fakeProm) QueryRange(context.Context, string, time.Time, time.Time, time.Duration) (*promquery.InstantResult, error) {
	return nil, nil
}
func (f fakeProm) Query(context.Context, string, time.Time) (*promquery.InstantResult, error) {
	return nil, nil
}

type fakeLogs struct{ tag string }

func (f fakeLogs) QueryRange(context.Context, logquery.QueryRangeOptions) (*logquery.QueryRangeResult, error) {
	return nil, nil
}

type fakeTraces struct{ tag string }

func (f fakeTraces) SearchTraces(context.Context, tracequery.SearchOptions) (*tracequery.SearchResult, error) {
	return nil, nil
}

// ── tests ───────────────────────────────────────────────────────────────────

func TestActivateResolvesEveryConfiguredBackend(t *testing.T) {
	host := NewHost(nil)
	set, err := Activate(host, Backends{
		Prom:   fakeProm{tag: "p"},
		Logs:   fakeLogs{tag: "l"},
		Traces: fakeTraces{tag: "t"},
	})
	if err != nil {
		t.Fatalf("activate: %v", err)
	}
	if set.Prom() == nil {
		t.Fatal("metrics querier must resolve after activation")
	}
	if set.Logs() == nil {
		t.Fatal("logs querier must resolve after activation")
	}
	if set.Traces() == nil {
		t.Fatal("traces querier must resolve after activation")
	}
	if got := len(host.Live()); got != 3 {
		t.Fatalf("live plugins = %d; want 3", got)
	}
}

func TestUnconfiguredBackendResolvesNil(t *testing.T) {
	host := NewHost(nil)
	// Only metrics is configured, the way a deployment with no Loki URL is.
	set, err := Activate(host, Backends{Prom: fakeProm{tag: "p"}})
	if err != nil {
		t.Fatalf("activate: %v", err)
	}
	if set.Prom() == nil {
		t.Fatal("metrics must resolve")
	}
	// The absent signals must be nil, not a zero-value querier: the tool
	// registry reads nil as "do not register this tool", which is the same
	// degradation the three separate nil checks in main() gave.
	if set.Logs() != nil {
		t.Fatal("logs must be nil when no logs backend is configured")
	}
	if set.Traces() != nil {
		t.Fatal("traces must be nil when no traces backend is configured")
	}
}

func TestResolveOnEmptyRegistryIsAllNil(t *testing.T) {
	set := Resolve(kernel.NewRegistry())
	if set.Prom() != nil || set.Logs() != nil || set.Traces() != nil {
		t.Fatal("an empty registry must resolve to nil for every signal")
	}
}

func TestEveryBackendGoesThroughAdmission(t *testing.T) {
	// A host whose policy does not grant the datasource scope must refuse
	// the backend rather than register it. This is what makes the
	// preinstalled path the same path a third-party package takes.
	pol := ManagerPolicy()
	pol.GrantedScopes = nil // grant nothing
	host := kernel.NewHost(kernel.NewRegistry(), pol, nil)

	_, err := Activate(host, Backends{Prom: fakeProm{}})
	if err == nil {
		t.Fatal("a host that granted no scope must refuse the datasource plugin")
	}
	if host.Registry().Len() != 0 {
		t.Fatalf("registry has %d entries after a refused activation; want 0", host.Registry().Len())
	}
}

func TestDeactivateRollsBackByPluginName(t *testing.T) {
	host := NewHost(nil)
	if _, err := Activate(host, Backends{Prom: fakeProm{}, Logs: fakeLogs{}}); err != nil {
		t.Fatal(err)
	}
	if err := host.Deactivate(context.Background(), PluginProm); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	// Prom is gone, Loki stays: a rollback removes exactly one package.
	set := Resolve(host.Registry())
	if set.Prom() != nil {
		t.Fatal("prom must be gone after its rollback")
	}
	if set.Logs() == nil {
		t.Fatal("loki must survive prom's rollback")
	}
}

func TestSignalLookup(t *testing.T) {
	if s, ok := Signal(PluginLoki); !ok || s != NameLoki {
		t.Fatalf("Signal(loki) = %q,%v; want loki,true", s, ok)
	}
	if _, ok := Signal("not-a-plugin"); ok {
		t.Fatal("an unknown plugin name must not resolve to a signal")
	}
}
