package alert

import (
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/vincent-wuhan/opskeeper/core/floor/prom"
	model "github.com/vincent-wuhan/opskeeper/core/manager/model/alert"
)

// The evaluator had a latency histogram and no counter, for the life of the
// deployment. prom.IncAlertEvalTick was declared, registered as
// opskeeper_alert_eval_ticks_total, exported on /metrics, and never called
// by anything -- so a dashboard that asked "are the alert rules still
// running" read a flat zero next to a healthy latency series, and the only
// honest reading of that pair was "the counter is broken" rather than
// "every rule is dead".
//
// The two series are moved from the one closure every Phase-A and Phase-B
// evaluator already runs, so they cannot disagree about which ticks
// happened. These two tests hold them to that.

// countedSeries is one labelled counter sample, flattened so a test can
// assert on the labels by name instead of by position.
type countedSeries struct {
	labels map[string]string
	value  float64
}

func gatherCounter(t *testing.T, registry *prometheus.Registry, name string) []countedSeries {
	t.Helper()
	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		out := make([]countedSeries, 0, len(family.Metric))
		for _, metric := range family.Metric {
			labels := map[string]string{}
			for _, pair := range metric.GetLabel() {
				labels[pair.GetName()] = pair.GetValue()
			}
			out = append(out, countedSeries{labels: labels, value: metric.GetCounter().GetValue()})
		}
		return out
	}
	t.Fatalf("%s is not on the registry: a metric nothing registers is a metric nothing exports", name)
	return nil
}

func TestAFinishedEvaluationIsBothTimedAndCounted(t *testing.T) {
	registry := prometheus.NewRegistry()
	prom.RegisterManagerMetrics(registry, nil)

	var evalErr error
	observeEval(model.RuleKindLogMatch, &evalErr)()

	ticks := gatherCounter(t, registry, "opskeeper_alert_eval_ticks_total")
	if len(ticks) != 1 {
		t.Fatalf("tick series = %d, want 1", len(ticks))
	}
	if got := ticks[0].value; got != 1 {
		t.Errorf("tick count = %v, want 1", got)
	}
	if got := ticks[0].labels["rule_kind"]; got != string(model.RuleKindLogMatch) {
		t.Errorf("rule_kind = %q, want %q: an alert storm is diagnosed by rule, and a counter that cannot "+
			"be split by rule only says that something happened", got, model.RuleKindLogMatch)
	}
	if got := ticks[0].labels["status"]; got != "ok" {
		t.Errorf("status = %q, want ok", got)
	}
}

// The status label is the reason the counter exists at all: a rule whose
// data source is down and a rule that matched nothing look identical on a
// latency histogram, and the operator needs to be told which.
func TestAFailedEvaluationIsCountedUnderItsOwnStatus(t *testing.T) {
	registry := prometheus.NewRegistry()
	prom.RegisterManagerMetrics(registry, nil)

	evalErr := errors.New("promql: query timed out")
	observeEval(model.RuleKindMetricBurnRate, &evalErr)()

	ticks := gatherCounter(t, registry, "opskeeper_alert_eval_ticks_total")
	if len(ticks) != 1 {
		t.Fatalf("tick series = %d, want 1", len(ticks))
	}
	if got := ticks[0].labels["status"]; got != "error" {
		t.Errorf("status = %q, want error: a broken data source and a quiet rule have to be told apart", got)
	}
	if got := ticks[0].value; got != 1 {
		t.Errorf("tick count = %v, want 1", got)
	}
}
