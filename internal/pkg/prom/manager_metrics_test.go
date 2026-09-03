package prom

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

func TestRegisterManagerMetricsIncludesInvestigatorDuration(t *testing.T) {
	registry := prometheus.NewRegistry()
	RegisterManagerMetrics(registry, nil)
	InvestigatorDuration.WithLabelValues("ready").Observe(65)
	InvestigatorDuration.WithLabelValues("failed").Observe(5)

	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, family := range families {
		if family.GetName() != "opskeeper_investigator_duration_seconds" {
			continue
		}
		if len(family.Metric) != 2 {
			t.Fatalf("metric count = %d, want 2", len(family.Metric))
		}
		if got := len(family.Metric[0].GetHistogram().Bucket); got != len(investigatorDurationBuckets) {
			t.Fatalf("bucket count = %d, want %d", got, len(investigatorDurationBuckets))
		}
		return
	}
	t.Fatal("opskeeper_investigator_duration_seconds not registered")
}

func TestRegisterManagerMetricsIncludesSchedulerMissedRun(t *testing.T) {
	registry := prometheus.NewRegistry()
	RegisterManagerMetrics(registry, nil)
	IncSchedulerMissedRun("42", "cron-1")

	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, family := range families {
		if family.GetName() != "scheduler_missed_run_total" {
			continue
		}
		if len(family.Metric) != 1 || family.Metric[0].GetCounter().GetValue() != 1 {
			t.Fatalf("scheduler metric = %#v", family.Metric)
		}
		return
	}
	t.Fatal("scheduler_missed_run_total not registered")
}
