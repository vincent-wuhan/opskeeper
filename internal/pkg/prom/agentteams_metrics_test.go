package prom

import (
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// drainCounterVec reads the current value of every series in a counter vec
// and returns the sum. Used to assert that Observe/Inc actually mutated
// the metric (vs. only registered it).
func drainCounterVec(t *testing.T, cv *prometheus.CounterVec) float64 {
	t.Helper()
	if cv == nil {
		t.Fatal("counter vec is nil")
	}
	ch := make(chan prometheus.Metric, 64)
	cv.Collect(ch)
	close(ch)
	total := 0.0
	for m := range ch {
		var pb dto.Metric
		if err := m.Write(&pb); err != nil {
			t.Errorf("metric write: %v", err)
			continue
		}
		if pb.Counter != nil {
			total += pb.Counter.GetValue()
		}
	}
	return total
}

func silentLoggerAgentTeams() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestRegisterAgentTeamsMetrics_Idempotent(t *testing.T) {
	reg := prometheus.NewRegistry()
	RegisterAgentTeamsMetrics(reg, silentLoggerAgentTeams())
	// Second registration must NOT panic (already-registered path).
	RegisterAgentTeamsMetrics(reg, silentLoggerAgentTeams())
	if AgentTeamsMCPCallTotal == nil {
		t.Fatal("AgentTeamsMCPCallTotal not initialized")
	}
	if AgentTeamsHigressResolveTotal == nil {
		t.Fatal("AgentTeamsHigressResolveTotal not initialized")
	}
	if LoopPhaseTotal == nil {
		t.Fatal("LoopPhaseTotal not initialized")
	}
}

func TestRegisterAgentTeamsMetrics_NilRegistry(t *testing.T) {
	// Must not panic; just early-return. Other tests in this package may
	// have already populated the package-globals, so we only assert that
	// this call returns cleanly without touching them.
	RegisterAgentTeamsMetrics(nil, silentLoggerAgentTeams())
}

func TestObserveAgentTeamsMCPCall_OK(t *testing.T) {
	reg := prometheus.NewRegistry()
	RegisterAgentTeamsMetrics(reg, silentLoggerAgentTeams())
	before := drainCounterVec(t, AgentTeamsMCPCallTotal)
	ObserveAgentTeamsMCPCall("loop.correlate", "investigator", 0.05, nil, false, false)
	ObserveAgentTeamsMCPCall("loop.investigate", "investigator", 1.2, nil, false, false)
	after := drainCounterVec(t, AgentTeamsMCPCallTotal)
	if delta := after - before; delta != 2 {
		t.Errorf("expected 2 increments, got delta=%v", delta)
	}
}

func TestObserveAgentTeamsMCPCall_DeniedVsAuthVsError(t *testing.T) {
	reg := prometheus.NewRegistry()
	RegisterAgentTeamsMetrics(reg, silentLoggerAgentTeams())
	ObserveAgentTeamsMCPCall("recovery.execute", "alerter", 0.01, nil, false, true)                  // denied
	ObserveAgentTeamsMCPCall("recovery.execute", "investigator", 0.01, nil, true, false)             // auth_failed
	ObserveAgentTeamsMCPCall("recovery.execute", "repairer", 0.01, errors.New("boom"), false, false) // error
	// Verify each result label got exactly one increment.
	got := map[string]float64{}
	ch := make(chan prometheus.Metric, 64)
	AgentTeamsMCPCallTotal.Collect(ch)
	close(ch)
	for m := range ch {
		var pb dto.Metric
		if err := m.Write(&pb); err != nil {
			continue
		}
		for _, lp := range pb.Label {
			if lp.GetName() == "result" {
				if pb.Counter != nil {
					got[lp.GetValue()] += pb.Counter.GetValue()
				}
			}
		}
	}
	for _, want := range []string{"denied", "auth_failed", "error"} {
		if got[want] < 1 {
			t.Errorf("expected at least 1 observation with result=%q, got %v", want, got)
		}
	}
}

func TestIncAgentTeamsHigressResolve(t *testing.T) {
	reg := prometheus.NewRegistry()
	RegisterAgentTeamsMetrics(reg, silentLoggerAgentTeams())
	IncAgentTeamsHigressResolve("ok")
	IncAgentTeamsHigressResolve("not_found")
	IncAgentTeamsHigressResolve("not_found")
	before := drainCounterVec(t, AgentTeamsHigressResolveTotal)
	IncAgentTeamsHigressResolve("ok")
	IncAgentTeamsHigressResolve("ok")
	IncAgentTeamsHigressResolve("network_error")
	after := drainCounterVec(t, AgentTeamsHigressResolveTotal)
	if delta := after - before; delta != 3 {
		t.Errorf("expected 3 increments, got delta=%v", delta)
	}
}

func TestObserveLoopPhase(t *testing.T) {
	reg := prometheus.NewRegistry()
	RegisterAgentTeamsMetrics(reg, silentLoggerAgentTeams())
	ObserveLoopPhase("investigator", 1.5, nil)
	ObserveLoopPhase("investigator", 0.5, errors.New("llm timeout"))
	ObserveLoopPhase("recovered", 2.0, errSeverityEscalated{})
	ObserveLoopPhase("recovered", 1.0, errRolledBack{})
	got := map[string]float64{}
	ch := make(chan prometheus.Metric, 64)
	LoopPhaseTotal.Collect(ch)
	close(ch)
	for m := range ch {
		var pb dto.Metric
		if err := m.Write(&pb); err != nil {
			continue
		}
		for _, lp := range pb.Label {
			if lp.GetName() == "result" && pb.Counter != nil {
				got[lp.GetValue()] += pb.Counter.GetValue()
			}
		}
	}
	for _, want := range []string{"ok", "failed", "severity_escalated", "rolled_back"} {
		if got[want] < 1 {
			t.Errorf("expected at least 1 with result=%q, got %v", want, got)
		}
	}
}

func TestIncLoopDBApprovedDecisionLookup(t *testing.T) {
	reg := prometheus.NewRegistry()
	RegisterAgentTeamsMetrics(reg, silentLoggerAgentTeams())
	before := drainCounterVec(t, LoopDBApprovedDecisionLookupTotal)
	IncLoopDBApprovedDecisionLookup("loaded")
	IncLoopDBApprovedDecisionLookup("not_found")
	IncLoopDBApprovedDecisionLookup("skipped")
	IncLoopDBApprovedDecisionLookup("db_error")
	after := drainCounterVec(t, LoopDBApprovedDecisionLookupTotal)
	if delta := after - before; delta != 4 {
		t.Errorf("expected 4 increments, got delta=%v", delta)
	}
}

func TestIsSeverityEscalatedAndRolledBack(t *testing.T) {
	if !IsSeverityEscalated(errSeverityEscalated{}) {
		t.Error("IsSeverityEscalated should match errSeverityEscalated")
	}
	if !IsRolledBack(errRolledBack{}) {
		t.Error("IsRolledBack should match errRolledBack")
	}
	if IsSeverityEscalated(errors.New("plain")) {
		t.Error("IsSeverityEscalated should not match unrelated error")
	}
}
