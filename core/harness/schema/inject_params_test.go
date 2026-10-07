package schema

import (
	"os"
	"path/filepath"
	"testing"
)

// writeAndLoad writes a case file into a temp dir and loads it back.
func writeAndLoad(t *testing.T, body string) *Case {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "case.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	caseObj, err := NewLoader(dir).LoadByID("pg/lock-waits")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return caseObj
}

// TestInjectParametersSurviveLoading is the regression for a silent data loss:
// the loader extracted only each step's type, so Params was always nil and
// every declared parameter — the table, the namespace, the key — was dropped
// before anything could use it.
func TestInjectParametersSurviveLoading(t *testing.T) {
	caseObj := writeAndLoad(t, `id: pg/lock-waits
description: a synthetic case for the inject parameter parser
severity: P2
prerequisites:
  - pg.cluster reachable
inject:
  - type: pg.inject_lock_chain
    duration: 180s
    params:
      table: orders
      sessions: 5
      wait_event: "Lock:transactionid"
      tables: [orders, order_items]
      dry_run: true
expect:
  time_to_detect: 60
  time_to_remediate: 120
  root_cause_lines:
    - pg.lock_waits
  remediation_options:
    - pg.kill_session
`)
	if len(caseObj.Inject) != 1 {
		t.Fatalf("steps = %d, want 1", len(caseObj.Inject))
	}
	step := caseObj.Inject[0]
	if step.Duration != "180s" {
		t.Errorf("duration = %q, want 180s", step.Duration)
	}
	if step.Params["table"] != "orders" {
		t.Errorf("table = %v, want orders", step.Params["table"])
	}
	if step.Params["sessions"] != 5 {
		t.Errorf("sessions = %v (%T), want the int 5", step.Params["sessions"], step.Params["sessions"])
	}
	if step.Params["wait_event"] != "Lock:transactionid" {
		t.Errorf("wait_event = %v, want the quoted value with its colon intact", step.Params["wait_event"])
	}
	tables, ok := step.Params["tables"].([]interface{})
	if !ok || len(tables) != 2 || tables[0] != "orders" {
		t.Errorf("tables = %v, want the inline list", step.Params["tables"])
	}
	if step.Params["dry_run"] != "true" {
		t.Errorf("dry_run = %v, want the string true", step.Params["dry_run"])
	}
}

// TestAParameterOfTheNextStepIsNotSwallowedByTheFirst pins the block boundary:
// an inject list has one params block per step, and a parser that ran to the
// end of the list would hand step 1 the parameters of step 2.
func TestAParameterOfTheNextStepIsNotSwallowedByTheFirst(t *testing.T) {
	caseObj := writeAndLoad(t, `id: pg/lock-waits
description: two inject steps with their own parameters
severity: P2
prerequisites:
  - pg.cluster reachable
inject:
  - type: pg.inject_lock_chain
    duration: 180s
    params:
      table: orders
  - type: pg.inject_replica_lag
    duration: 600s
    params:
      target_lag_mb: 500
expect:
  time_to_detect: 60
  time_to_remediate: 120
  root_cause_lines:
    - pg.lock_waits
  remediation_options:
    - pg.kill_session
`)
	if len(caseObj.Inject) != 2 {
		t.Fatalf("steps = %d, want 2", len(caseObj.Inject))
	}
	if _, present := caseObj.Inject[0].Params["target_lag_mb"]; present {
		t.Errorf("the first step absorbed the second step's parameter: %v", caseObj.Inject[0].Params)
	}
	if caseObj.Inject[1].Params["target_lag_mb"] != 500 {
		t.Errorf("second step params = %v, want target_lag_mb=500", caseObj.Inject[1].Params)
	}
}

// TestANestedStructureIsNotFlattenedIntoAParameter: the parser models one
// level. A nested map used to be flattened, which invented parameters at the
// wrong level; now the nested head is skipped and its children stay out.
func TestANestedStructureIsNotFlattenedIntoAParameter(t *testing.T) {
	caseObj := writeAndLoad(t, `id: pg/lock-waits
description: a nested parameter structure the parser does not model
severity: P2
prerequisites:
  - pg.cluster reachable
inject:
  - type: k8s.inject_memory_pressure
    duration: 300s
    params:
      limits:
        memory_mb: 512
      namespace: test
expect:
  time_to_detect: 60
  time_to_remediate: 120
  root_cause_lines:
    - pg.lock_waits
  remediation_options:
    - pg.kill_session
`)
	step := caseObj.Inject[0]
	if _, present := step.Params["memory_mb"]; present {
		t.Errorf("a nested value was flattened into the parameter level: %v", step.Params)
	}
	if _, present := step.Params["limits"]; present {
		t.Errorf("the nested head was recorded with an empty value: %v", step.Params)
	}
	if step.Params["namespace"] != "test" {
		t.Errorf("sibling parameter lost: %v", step.Params)
	}
}

// TestTheShippedCorpusLoadsItsParameters: every case in the corpus declares a
// duration and at least one parameter, and the loader now reads them. This is
// what makes the eval CLI's localization axis possible at all.
func TestTheShippedCorpusLoadsItsParameters(t *testing.T) {
	cases, err := NewLoader(filepath.Join("..", "cases")).LoadAll()
	if err != nil {
		t.Fatalf("load corpus: %v", err)
	}
	if len(cases) != 20 {
		t.Fatalf("loaded %d cases, want 20", len(cases))
	}
	for _, caseObj := range cases {
		for index, step := range caseObj.Inject {
			if step.Duration == "" {
				t.Errorf("%s inject step %d has no duration", caseObj.ID, index)
			}
			if len(step.Params) == 0 {
				t.Errorf("%s inject step %d has no parameters", caseObj.ID, index)
			}
		}
	}
}

func TestTheLockWaitsCaseNamesItsTable(t *testing.T) {
	caseObj, err := NewLoader(filepath.Join("..", "cases")).LoadByID("pg/lock-waits")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := caseObj.Inject[0].Params["table"]; got != "orders" {
		t.Errorf("table = %v, want orders (the case file says so)", got)
	}
}
