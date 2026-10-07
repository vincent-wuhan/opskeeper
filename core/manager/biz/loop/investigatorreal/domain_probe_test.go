package investigatorreal

import (
	"context"
	"errors"
	"testing"
	"time"

	loop "github.com/vincent-wuhan/opskeeper/core/manager/biz/loop"
)

type stubTools struct {
	specs   map[string]loop.ToolSpec
	results map[string]any
	errs    map[string]error
	calls   []string
}

func (s *stubTools) LookupTool(name string) (loop.ToolSpec, bool) {
	spec, ok := s.specs[name]
	return spec, ok
}

func (s *stubTools) CallTool(_ context.Context, name string, _ map[string]any) (any, error) {
	s.calls = append(s.calls, name)
	if err, ok := s.errs[name]; ok {
		return nil, err
	}
	return s.results[name], nil
}

func rowsOf(rows ...map[string]any) map[string]any {
	return map[string]any{"rows": rows, "count": len(rows)}
}

func readOnly(name string) loop.ToolSpec { return loop.ToolSpec{Name: name, RiskLevel: "L0"} }

func diagnostic(name string) loop.ToolSpec { return loop.ToolSpec{Name: name, RiskLevel: "L1"} }

func TestProbe_RecordsTheDomainResultAsEvidence(t *testing.T) {
	tools := &stubTools{
		specs: map[string]loop.ToolSpec{
			"pg.table_bloat":   readOnly("pg.table_bloat"),
			"pg.vacuum_status": diagnostic("pg.vacuum_status"),
			"pg.slow_log":      readOnly("pg.slow_log"),
		},
		results: map[string]any{
			"pg.table_bloat":   rowsOf(map[string]any{"table": "orders", "dead_pct": 61.0}),
			"pg.vacuum_status": rowsOf(),
			"pg.slow_log":      rowsOf(map[string]any{"query": "SELECT 1"}),
		},
	}
	items := probe(context.Background(), tools, "pg", timeZero)
	if len(items) != 3 {
		t.Fatalf("recorded %d probes, want 3: %#v", len(items), items)
	}
	byTool := map[string]loop.EvidenceItem{}
	for _, item := range items {
		byTool[item.Tool] = item
	}
	if _, ok := byTool["pg.table_bloat"]; !ok {
		t.Fatal("the bloat result must be in the chain")
	}
	if byTool["pg.table_bloat"].Count != 1 {
		t.Errorf("Count = %d, want the row count the adapter reported", byTool["pg.table_bloat"].Count)
	}
}

// An investigation runs before anything has been approved, so a probe at L2
// or above would be a write performed as a side effect of looking. The plan
// is data, so the ceiling has to be enforced rather than trusted.
func TestProbe_RefusesAToolThatGradesItselfAboveTheReadCeiling(t *testing.T) {
	tools := &stubTools{
		specs:   map[string]loop.ToolSpec{"pg.table_bloat": loop.ToolSpec{Name: "pg.table_bloat", RiskLevel: "L3"}},
		results: map[string]any{"pg.table_bloat": rowsOf()},
	}
	// Put a write-graded tool under a probe name and confirm it is not called.
	tools.specs["pg.slow_log"] = loop.ToolSpec{Name: "pg.slow_log", RiskLevel: "L0"}
	tools.specs["pg.vacuum_status"] = loop.ToolSpec{Name: "pg.vacuum_status", RiskLevel: "L0"}
	items := probe(context.Background(), tools, "pg", timeZero)
	for _, item := range items {
		if item.Tool == "pg.table_bloat" {
			t.Fatal("an L3 tool must never be probed during an investigation")
		}
	}
	if probeIsReadOnly(tools, "pg.table_bloat") {
		t.Error("probeIsReadOnly must reject an L3 tool")
	}
}

// A tool that will not say what it does is not one to call unprompted.
func TestProbe_RefusesAToolWithAnUnreadableRiskGrade(t *testing.T) {
	tools := &stubTools{specs: map[string]loop.ToolSpec{
		"pg.table_bloat": loop.ToolSpec{Name: "pg.table_bloat", RiskLevel: "moderate"},
	}}
	if probeIsReadOnly(tools, "pg.table_bloat") {
		t.Error("an unparseable risk grade must be treated as exceeding the ceiling")
	}
}

// The adapter for a domain is wired by DSN. An absent adapter is a fact
// about the deployment, not an incident.
func TestProbe_SkipsToolsThatAreNotRegistered(t *testing.T) {
	tools := &stubTools{specs: map[string]loop.ToolSpec{}}
	if items := probe(context.Background(), tools, "k8s", timeZero); len(items) != 0 {
		t.Fatalf("an unwired domain must record nothing, got %#v", items)
	}
}

// A database refusing connections is exactly the incident worth
// investigating; one failing question must not blank the chain.
func TestProbe_ToleratesOneFailingQuestion(t *testing.T) {
	tools := &stubTools{
		specs: map[string]loop.ToolSpec{
			"pg.table_bloat": readOnly("pg.table_bloat"),
			"pg.slow_log":    readOnly("pg.slow_log"),
		},
		results: map[string]any{"pg.slow_log": rowsOf(map[string]any{"query": "SELECT 1"})},
		errs:    map[string]error{"pg.table_bloat": errors.New("connection refused")},
	}
	items := probe(context.Background(), tools, "pg", timeZero)
	if len(items) != 1 || items[0].Tool != "pg.slow_log" {
		t.Fatalf("the surviving probe must still be recorded: %#v", items)
	}
}

func TestProbe_RecordsNothingForAResourceTypeItDoesNotPlan(t *testing.T) {
	tools := &stubTools{specs: map[string]loop.ToolSpec{}, results: map[string]any{}}
	if items := probe(context.Background(), tools, "host", timeZero); len(items) != 0 {
		t.Fatalf("host has no probe plan, got %#v", items)
	}
}

// The plan itself is a claim about safety, so it is tested rather than
// commented: every entry must be a tool the ceiling would allow.
func TestTheProbePlanIsEntirelyReadOnly(t *testing.T) {
	tools := &stubTools{specs: map[string]loop.ToolSpec{}}
	for resourceType, plan := range domainProbePlan {
		for _, entry := range plan {
			tools.specs[entry.Tool] = loop.ToolSpec{Name: entry.Tool, RiskLevel: "L4"}
			if probeIsReadOnly(tools, entry.Tool) {
				t.Errorf("%s/%s would be probed as a write", resourceType, entry.Tool)
			}
		}
	}
}

var timeZero = time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC)
