package main

import (
	"context"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/base/pkg/promquery"
	managerintegration "github.com/vincent-wuhan/opskeeper/core/domains/server/integration"
	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/toolcore"
	managerbizalert "github.com/vincent-wuhan/opskeeper/core/manager/biz/alert"
	"github.com/vincent-wuhan/opskeeper/core/manager/biz/loop/investigatorreal"
	managerreportstore "github.com/vincent-wuhan/opskeeper/core/manager/data/report/store"

	vmplugin "github.com/vincent-wuhan/opskeeper/plugins/opskeeper-datasource-victoriametrics"
)

// The control plane declares its metrics dependencies as narrow interfaces, one
// per consumer, and a concrete client satisfies them structurally rather than by
// declaring so. Structural satisfaction is checked at runtime, by an assertion
// at the assignment site — which is why these assertions are written down here
// instead.
//
// The consequence of a mismatch is silent. `Set.Prom()` resolves through a type
// assertion that yields a nil interface, the tool registry reads that as "this
// deployment has no metrics backend", and query_promql stops being registered.
// Nothing errors. The dashboard shows no data and the logs say the tool was
// deliberately not registered.
//
// The plugin module cannot make these assertions itself: importing core/manager
// from a third-party package is the boundary it exists to hold. So the check
// lives here, where core/manager is legitimately importable — and it runs
// against the concrete type the plugin actually ships, reached through its
// exported constructor rather than named, because internal/vmclient is not
// importable from here either.
//
// The assertion is written against `any` rather than as `var _ Iface = (*T)(nil)`
// for that reason: T is unnameable outside the module, so the declaration form
// cannot be written here at all. Reaching for the constructor is what makes the
// value the real one — a hand-written stand-in with a matching method set would
// pass a compile-time declaration just as happily.
func TestTheVictoriaMetricsClientSatisfiesEveryPromQuerierTheControlPlaneDeclares(t *testing.T) {
	var c any = vmplugin.New(vmplugin.Config{BaseURL: "http://victoria-metrics:8428"})

	_, toolcoreOK := c.(toolcore.PromQuerier)
	_, alertRuleOK := c.(managerbizalert.PromQuerier)
	_, alertPreviewOK := c.(managerbizalert.PreviewPromQuerier)
	_, reportOK := c.(managerreportstore.PromQuerier)
	_, investigatorOK := c.(investigatorreal.MetricQuerier)

	for _, tc := range []struct {
		what string
		ok   bool
	}{
		{"toolcore.PromQuerier (query_promql registration)", toolcoreOK},
		{"alert.PromQuerier (alert-rule pipeline)", alertRuleOK},
		{"alert.PreviewPromQuerier (alert preview)", alertPreviewOK},
		{"report/store.PromQuerier (report facts)", reportOK},
		{"investigatorreal.MetricQuerier (loop investigator)", investigatorOK},
	} {
		if !tc.ok {
			t.Errorf("the plugin's client does not satisfy %s", tc.what)
		}
	}
}

// One consumer is not in the list above: the integrations first-contact probe
// takes a func-shaped adapter whose interface returns `any`, so a client
// reaches it through a closure rather than an interface. Building that closure
// from the plugin's client is the assertion — an adapter wired to a hard-coded
// Prometheus client would compile just as well, and be just as wrong once
// someone switched the backend.
func TestTheIntegrationsProbeCanBeDrivenByTheVictoriaMetricsClient(t *testing.T) {
	c := vmplugin.New(vmplugin.Config{BaseURL: "http://127.0.0.1:1"})

	adapter := managerintegration.AdaptPromQuerier(
		func(ctx context.Context, expr string, ts time.Time) error {
			_, err := c.Query(ctx, expr, ts)
			return err
		})

	if adapter == nil {
		t.Fatal("AdaptPromQuerier returned nil")
	}
	// An unreachable backend has to surface as an error from the closure. An
	// adapter that swallowed it would make the probe report "Prometheus OK"
	// for a VictoriaMetrics deployment whose server is down.
	if _, err := adapter.Query(t.Context(), "up", time.Time{}); err == nil {
		t.Error("the probe reported success against a dead VictoriaMetrics backend")
	}
}

// The timeout is copied into the plugin module rather than imported, because
// importing toolcore from a third-party package would cross the boundary. A copy
// is only safe while it stays equal, and nothing inside the plugin module could
// notice the moment it stopped being.
func TestTheCopiedTimeoutIsTheSameCeilingTheFirstPartyClientUses(t *testing.T) {
	if got := vmplugin.QueryPromqlCallTimeout(); got != toolcore.QueryPromqlCallTimeout {
		t.Errorf("plugin timeout = %s, toolcore.QueryPromqlCallTimeout = %s; "+
			"the copy in the plugin module has drifted", got, toolcore.QueryPromqlCallTimeout)
	}
}

// Both clients answer with the same type, which is the whole compatibility
// argument in one assertion. A plugin that returned a VictoriaMetrics-shaped
// result would satisfy nothing above, and this states the reason why: the type
// crossing the boundary is core/base's, not the plugin's.
func TestThePluginAnswersInTheFirstPartyResultType(t *testing.T) {
	c := vmplugin.New(vmplugin.Config{BaseURL: "http://127.0.0.1:1"})

	// A compile-time statement that the plugin's result type is core/base's.
	// The call errors — nothing listens on port 1 — and the important part is
	// that the signature is *promquery.InstantResult at all.
	res, err := c.Query(t.Context(), "up", time.Time{})
	var _ *promquery.InstantResult = res
	if err == nil {
		t.Error("Query against a dead backend returned no error")
	}
}
