package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/harness/axes"
	"github.com/vincent-wuhan/opskeeper/core/harness/judge"
	"github.com/vincent-wuhan/opskeeper/core/harness/schema"
)

const shippedCasesDir = "../../core/harness/cases"

// runAxesIn runs the subcommand into a temp file and decodes its JSON.
func runAxesIn(t *testing.T, f axesFlags) (axesReport, error) {
	t.Helper()
	f.jsonOut = true
	path := filepath.Join(t.TempDir(), "out.txt")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	runErr := runAxes(f, file)
	body, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	var report axesReport
	if len(body) > 0 {
		if err := json.Unmarshal(body, &report); err != nil {
			t.Fatalf("decode axes report: %v (raw=%s)", err, body)
		}
	}
	return report, runErr
}

// TestEveryShippedCaseDeclaresTheThreeAxes is the gate. The judge scores
// Localization × Identification × Reason, and a case that declares nothing for
// one of them produces no number for it — an axis a leaderboard then silently
// averages over nothing. Every shipped case has to be measurable, so a case
// added tomorrow with an id the derivation cannot read fails here.
func TestEveryShippedCaseDeclaresTheThreeAxes(t *testing.T) {
	report, err := runAxesIn(t, axesFlags{casesDir: shippedCasesDir})
	if err != nil {
		t.Fatalf("axes: %v", err)
	}
	if report.Total != 20 {
		t.Fatalf("total = %d, want the 20 shipped cases", report.Total)
	}
	for _, item := range report.Cases {
		if len(item.Locus) == 0 {
			t.Errorf("%s declares no locus, so localization cannot be measured", item.CaseID)
		}
		if len(item.FaultType) == 0 {
			t.Errorf("%s declares no fault type, so identification cannot be measured", item.CaseID)
		}
		if len(item.Evidence) == 0 {
			t.Errorf("%s declares no expected observations, so reason cannot be measured", item.CaseID)
		}
	}
	if report.Unmeasured != 0 {
		t.Errorf("%d cases are unmeasurable", report.Unmeasured)
	}
}

// TestAnUnreadableFaultSegmentFailsTheGate pins the failure mode the gate
// exists for: a case whose fault segment is too short to be a word yields no
// identification tokens, and a run of it would score an identification axis
// that does not exist.
func TestAnUnreadableFaultSegmentFailsTheGate(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "case.yaml", `id: pg/ab
description: a synthetic case whose fault segment is too short to be a word
severity: P2
prerequisites:
  - pg.cluster reachable
inject:
  - type: pg.inject_lock_chain
    duration: 180s
    params:
      table: orders
expect:
  time_to_detect: 60
  time_to_remediate: 120
  root_cause_lines:
    - pg.lock_waits
  remediation_options:
    - pg.kill_session
`)
	report, err := runAxesIn(t, axesFlags{casesDir: dir, failUnmeasured: true})
	if err == nil {
		t.Fatalf("gate accepted an unmeasurable case: %+v", report.Cases)
	}
	if report.Unmeasured != 1 {
		t.Errorf("unmeasured = %d, want 1", report.Unmeasured)
	}
}

func TestTheSummaryCarriesTheThreeAxes(t *testing.T) {
	two := 0.5
	score := &judge.Score{
		Overall: 0.9,
		Dimensions: map[string]float64{
			judge.DimensionLocalization:   1,
			judge.DimensionIdentification: 1,
			judge.DimensionReason:         two,
		},
	}
	summary := summarize(score)
	if summary.Localization == nil || summary.Identification == nil || summary.Reason == nil {
		t.Fatalf("summary dropped an axis: %+v", summary)
	}
	if *summary.Reason != two {
		t.Errorf("reason = %v, want %v", *summary.Reason, two)
	}
	// Absence is not zero: a case that declares no locus must not report a
	// localization of 0 in the artifact a person reads.
	if summary.RCAAccuracy != nil {
		t.Errorf("rca_accuracy invented: %v", *summary.RCAAccuracy)
	}
	bare := summarize(&judge.Score{Overall: 0.9, Dimensions: map[string]float64{}})
	if bare.Localization != nil || bare.Identification != nil || bare.Reason != nil {
		t.Errorf("unmeasured axes reported as numbers: %+v", bare)
	}
}

func TestJudgeCaseOfCarriesTheDerivedAxes(t *testing.T) {
	caseObj, err := schema.NewLoader(shippedCasesDir).LoadByID("pg/lock-waits")
	if err != nil {
		t.Fatalf("load pg/lock-waits: %v", err)
	}
	judgeCase := judgeCaseOf(caseObj)
	if len(judgeCase.ExpectedLocus) == 0 {
		t.Fatal("the judge case carries no locus")
	}
	if len(judgeCase.ExpectedFaultType) == 0 {
		t.Fatal("the judge case carries no fault type")
	}
}

// TestARealCaseScoresTheAxesAndFlagsAnUngroundedAnswer is the end-to-end
// statement of why the axes exist: a run of a shipped case whose conclusion is
// perfect and whose trace is empty is, by the outcome dimensions alone,
// indistinguishable from a real diagnosis — and is now flagged for review.
func TestARealCaseScoresTheAxesAndFlagsAnUngroundedAnswer(t *testing.T) {
	caseObj, err := schema.NewLoader(shippedCasesDir).LoadByID("pg/lock-waits")
	if err != nil {
		t.Fatalf("load pg/lock-waits: %v", err)
	}
	response := &judge.AgentResponse{
		RootCause:    append([]string(nil), caseObj.Expect.RootCauseLines...),
		Remediations: append([]string(nil), caseObj.Expect.RemediationOptions...),
	}
	score, err := judge.NewHeuristicJudge().Score(context.Background(), judgeCaseOf(caseObj), response)
	if err != nil {
		t.Fatalf("Score: %v", err)
	}
	if score.Dimensions[judge.DimensionIdentification] != 1.0 {
		t.Errorf("identification = %v, want 1.0 (the answer names the fault)",
			score.Dimensions[judge.DimensionIdentification])
	}
	if score.Dimensions[judge.DimensionReason] != 0.0 {
		t.Errorf("reason = %v, want 0.0 (no tool call made the observations)",
			score.Dimensions[judge.DimensionReason])
	}
	if score.Overall < judge.DiagnosticOutcomeFloor {
		t.Fatalf("fixture is not a good outcome: overall=%v", score.Overall)
	}
	if !score.Flagged {
		t.Error("a perfect conclusion with an empty trace was not flagged for review")
	}
}

// case 自报的 rca_accuracy 阈值必须过桥到达judge。
//
// rubric.rca_accuracy 在 30+ 个随仓库发布的 case.yaml 里都写了（0.8 / 0.85 /
// 0.9），schema 也校验它，但这条桥接此前不传它，于是每一个阈值都是一句没有
// 执行者的声明：judge 收不到，也就无从比对，而 case 之间的门槛差异对评分
// 完全不可见。
func TestJudgeCaseOfCarriesTheDeclaredThreshold(t *testing.T) {
	caseObj, err := schema.NewLoader(shippedCasesDir).LoadByID("pg/lock-waits")
	if err != nil {
		t.Fatalf("load pg/lock-waits: %v", err)
	}
	if caseObj.Rubric.RCAAccuracy <= 0 {
		t.Fatalf("fixture assumption broken: the shipped case declares no threshold (%v)",
			caseObj.Rubric.RCAAccuracy)
	}
	if got := judgeCaseOf(caseObj).RCAThreshold; got != caseObj.Rubric.RCAAccuracy {
		t.Errorf("RCAThreshold = %v, want the case's declared %v", got, caseObj.Rubric.RCAAccuracy)
	}
}

// eval 这条桥接同样逐案对齐那一份派生——与 runner 侧那条守卫成对。
//
// 两个 harness 共用一个 judge、一张 leaderboard，而它们此前对同一个 case 得出
// 不同的三轴。守卫放在两边而不是一处，是因为「不一致」这件事只能在各自的桥接上
// 观察：从库那一侧看，两条桥接都调用了 axes.Of，从任一 bridge 那一侧也看不到
// 另一条桥接。
func TestTheEvalBridgeCarriesExactlyWhatTheDerivationSays(t *testing.T) {
	cases, err := schema.NewLoader(shippedCasesDir).LoadAll()
	if err != nil {
		t.Fatalf("load shipped cases: %v", err)
	}
	if len(cases) == 0 {
		t.Fatal("no shipped cases: the comparison would pass on an empty corpus")
	}
	for _, c := range cases {
		want := axes.Of(c)
		got := judgeCaseOf(c)
		if strings.Join(got.ExpectedLocus, ",") != strings.Join(want.Locus, ",") {
			t.Errorf("%s: locus = %v, want %v", c.ID, got.ExpectedLocus, want.Locus)
		}
		if strings.Join(got.ExpectedFaultType, ",") != strings.Join(want.FaultType, ",") {
			t.Errorf("%s: fault type = %v, want %v", c.ID, got.ExpectedFaultType, want.FaultType)
		}
	}
}
