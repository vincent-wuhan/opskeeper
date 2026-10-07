package runner

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/harness/axes"
	"github.com/vincent-wuhan/opskeeper/core/harness/judge"
	"github.com/vincent-wuhan/opskeeper/core/harness/schema"
)

// stubLoopOrchestrator is a tiny in-memory LoopOrchestrator for tests.
// It produces a deterministic 7-phase event timeline matching
// postmortem terminal state with 30ms per phase.
type stubLoopOrchestrator struct {
	finalPhase string
}

func (s *stubLoopOrchestrator) Run(ctx context.Context, opts LoopRunOptions) (*LoopRunResult, error) {
	now := time.Now().UTC()
	phases := []string{"detected", "correlated", "investigated", "critiqued", "approved", "recovered", "postmortem"}
	events := make([]LoopEventRecord, 0, len(phases)*2)
	durations := make(map[string]int64)
	t := now
	for _, p := range phases {
		events = append(events, LoopEventRecord{
			Phase:     p,
			EventType: "phase_entered",
			CreatedAt: t,
		})
		dur := int64(30)
		t = t.Add(time.Duration(dur) * time.Millisecond)
		durations[p] = dur
		events = append(events, LoopEventRecord{
			Phase:     p,
			EventType: "phase_contract_written",
			CreatedAt: t,
		})
	}
	final := s.finalPhase
	if final == "" {
		final = "postmortem"
	}
	return &LoopRunResult{
		IncidentID: opts.IncidentID,
		FinalPhase: final,
		LoopEvents: events,
		Durations:  durations,
	}, nil
}

func TestRunLoop_DryRun_Postmortem(t *testing.T) {
	ctx := context.Background()
	outDir := filepath.Join(t.TempDir(), "result")
	res, err := RunLoop(ctx, LoopOptions{
		CaseID:   "pg/long-running-tx",
		Mode:     ModeLoop,
		Env:      EnvStaging,
		TenantID: "T1",
		OutDir:   outDir,
		CasesDir: "../cases",
	}, LoopDeps{})
	if err != nil {
		t.Fatalf("RunLoop: %v", err)
	}
	if !res.Passed {
		t.Errorf("expected passed=true in dry-run, got false")
	}
	if res.FinalPhase != "postmortem" {
		t.Errorf("expected final phase postmortem, got %s", res.FinalPhase)
	}
	if res.Rubric.RCAAccuracy == nil || *res.Rubric.RCAAccuracy != 1.0 {
		t.Errorf("expected rca_accuracy=1.0, got %v", res.Rubric.RCAAccuracy)
	}
	if res.Rubric.RecoveryPassRate == nil || *res.Rubric.RecoveryPassRate != 1.0 {
		t.Errorf("expected recovery_pass_rate=1.0, got %v", res.Rubric.RecoveryPassRate)
	}
	// Verify the JSON file was written.
	outPath := filepath.Join(outDir, "pg-long-running-tx.json")
	if _, err := os.Stat(outPath); err != nil {
		t.Fatalf("expected output JSON at %s: %v", outPath, err)
	}
}

func TestRunLoop_OrchestratorStub(t *testing.T) {
	ctx := context.Background()
	outDir := filepath.Join(t.TempDir(), "result")
	res, err := RunLoop(ctx, LoopOptions{
		CaseID:   "redis/memory-burst",
		Mode:     ModeLoop,
		Env:      EnvStaging,
		TenantID: "T1",
		OutDir:   outDir,
		CasesDir: "../cases",
	}, LoopDeps{Orchestrator: &stubLoopOrchestrator{}})
	if err != nil {
		t.Fatalf("RunLoop: %v", err)
	}
	if !res.Passed {
		t.Errorf("expected passed=true")
	}
	if len(res.Phases) != 7 {
		t.Errorf("expected 7 phase records, got %d", len(res.Phases))
	}
}

func TestRunLoop_Rollback_NotPassed(t *testing.T) {
	ctx := context.Background()
	outDir := filepath.Join(t.TempDir(), "result")
	res, err := RunLoop(ctx, LoopOptions{
		CaseID:   "host/cpu-spike",
		Mode:     ModeLoop,
		OutDir:   outDir,
		CasesDir: "../cases",
	}, LoopDeps{Orchestrator: &stubLoopOrchestrator{finalPhase: "failed"}})
	if err != nil {
		t.Fatalf("RunLoop: %v", err)
	}
	if res.Passed {
		t.Errorf("expected passed=false when final phase=failed")
	}
	if res.Rubric.RecoveryPassRate == nil || *res.Rubric.RecoveryPassRate != 0.0 {
		t.Errorf("expected recovery_pass_rate=0.0, got %v", res.Rubric.RecoveryPassRate)
	}
}

func TestParseMode(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Mode
		err  bool
	}{
		{"", ModeTool, false},
		{"tool", ModeTool, false},
		{"loop", ModeLoop, false},
		{"chat", ModeChat, false},
		{"Loop", ModeLoop, false}, // case-insensitive
		{"unknown", "", true},
	} {
		got, err := ParseMode(tc.in)
		if (err != nil) != tc.err {
			t.Errorf("ParseMode(%q): err=%v want_err=%v", tc.in, err, tc.err)
			continue
		}
		if !tc.err && got != tc.want {
			t.Errorf("ParseMode(%q): got=%s want=%s", tc.in, got, tc.want)
		}
	}
}

func TestRunLoop_ChatMode_RubricShape(t *testing.T) {
	ctx := context.Background()
	outDir := filepath.Join(t.TempDir(), "result")
	res, err := RunLoop(ctx, LoopOptions{
		CaseID:   "pg/long-running-tx",
		Mode:     ModeChat,
		OutDir:   outDir,
		CasesDir: "../cases",
	}, LoopDeps{Orchestrator: &stubLoopOrchestrator{}})
	if err != nil {
		t.Fatalf("RunLoop: %v", err)
	}
	// Chat mode: kb_hit_rate / follow_up_depth are nil with reason
	// explaining "chat metrics not populated". RubricIncomplete is
	// true until chat path populates them.
	if res.Rubric.KBHitRateReason == "" {
		t.Errorf("expected kb_hit_rate reason for chat mode")
	}
	if res.Rubric.FollowUpDepthReason == "" {
		t.Errorf("expected follow_up_depth reason for chat mode")
	}
	if !res.Rubric.RubricIncomplete {
		t.Errorf("expected rubric_incomplete=true for chat mode (chat metrics stub)")
	}
}

func TestRunLoop_BackwardCompat_ToolModeRejected(t *testing.T) {
	_, err := RunLoop(context.Background(), LoopOptions{
		CaseID: "pg/long-running-tx",
		Mode:   ModeTool,
	}, LoopDeps{})
	if err == nil {
		t.Fatalf("expected error when ModeTool is passed to RunLoop")
	}
}

// TestRunLoop_WithCustomJudge verifies the Judge injection path.
func TestRunLoop_WithCustomJudge(t *testing.T) {
	ctx := context.Background()
	outDir := filepath.Join(t.TempDir(), "result")
	// HeuristicJudge is the only Judge we have; verify it integrates.
	res, err := RunLoop(ctx, LoopOptions{
		CaseID:   "pg/long-running-tx",
		Mode:     ModeLoop,
		TenantID: "T1",
		OutDir:   outDir,
		CasesDir: "../cases",
	}, LoopDeps{
		Orchestrator: &stubLoopOrchestrator{},
		Judge:        judge.NewHeuristicJudge(),
	})
	if err != nil {
		t.Fatalf("RunLoop: %v", err)
	}
	if res.JudgeScores.JudgesUsed == nil || len(res.JudgeScores.JudgesUsed) == 0 {
		t.Errorf("expected judge to populate JudgesUsed")
	}
}

func TestFileSafeCaseID(t *testing.T) {
	cases := map[string]string{
		"pg/long-running-tx": "pg-long-running-tx",
		"redis/memory-burst": "redis-memory-burst",
		"host/cpu-spike":     "host-cpu-spike",
	}
	for in, want := range cases {
		if got := fileSafeCaseID(in); got != want {
			t.Errorf("fileSafeCaseID(%q): got %q want %q", in, got, want)
		}
	}
}

// runner 的桥接同样必须把case 自报的阈值带过去（eval CLI 那条桥有对应的守卫）。
func TestCaseToJudgeCaseCarriesTheDeclaredThreshold(t *testing.T) {
	c := &schema.Case{
		ID:     "pg/lock-waits",
		Expect: schema.Expect{RootCauseLines: []string{"pg.lock_waits"}},
		Rubric: schema.Rubric{RCAAccuracy: 0.85},
	}
	if got := caseToJudgeCase(c).RCAThreshold; got != 0.85 {
		t.Errorf("RCAThreshold = %v, want 0.85", got)
	}
}

// 两条评分桥接必须与那一份派生逐案一致。
//
// runner 与 opskeeper-eval 用同一个 judge 打同一批 case。它们此前对同一个 case
// 得出不同的三轴——eval 那条有，runner 那条没有——而两个 harness 都把结果写进
// 同一张 leaderboard。逐案比对而不是抽查一个 case：派生规则的一处改动（多认一个
// 身份键、少拆一个分隔符）只会影响特定的 casefamily，抽查恰好会漏掉。
func TestTheBridgeCarriesExactlyWhatTheDerivationSays(t *testing.T) {
	cases, err := schema.NewLoader("../cases").LoadAll()
	if err != nil {
		t.Fatalf("load shipped cases: %v", err)
	}
	if len(cases) == 0 {
		t.Fatal("no shipped cases: the comparison would pass on an empty corpus")
	}
	for _, c := range cases {
		want := axes.Of(c)
		got := caseToJudgeCase(c)
		if !equalTokens(got.ExpectedLocus, want.Locus) {
			t.Errorf("%s: locus = %v, want %v", c.ID, got.ExpectedLocus, want.Locus)
		}
		if !equalTokens(got.ExpectedFaultType, want.FaultType) {
			t.Errorf("%s: fault type = %v, want %v", c.ID, got.ExpectedFaultType, want.FaultType)
		}
	}
}

func equalTokens(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// 这是搬动派生要换来的东西：runner 这条路径现在也能看见三轴。
//
// 一个结论完全正确、轨迹里一条观测都没有的回答，在只看结果维度的评分里与一次
// 真正的诊断无法区分——而它此前连被标记的机会都没有，因为这条路径算不出 reason
// 轴。断言的是标记本身，不是某个数字：数字会随权重变，标记是给人看的那个结论。
func TestARunScoredThroughThisBridgeIsFlaggedWhenTheTraceIsUngrounded(t *testing.T) {
	cases, err := schema.NewLoader("../cases").LoadAll()
	if err != nil {
		t.Fatalf("load shipped cases: %v", err)
	}
	if len(cases) == 0 {
		t.Fatal("no shipped cases")
	}
	c := cases[0]
	judgeCase := caseToJudgeCase(c)
	if len(judgeCase.ExpectedLocus) == 0 || len(judgeCase.ExpectedFaultType) == 0 {
		t.Fatalf("%s carries no axes through this bridge: %+v", c.ID, judgeCase)
	}
	// 结论完美、轨迹为空：只有 reason 轴能看见这件事。
	response := &judge.AgentResponse{
		RootCause:    append([]string(nil), c.Expect.RootCauseLines...),
		Remediations: append([]string(nil), c.Expect.RemediationOptions...),
	}
	score, err := judge.NewHeuristicJudge().Score(context.Background(), judgeCase, response)
	if err != nil {
		t.Fatalf("Score: %v", err)
	}
	if !score.Flagged {
		t.Errorf("%s: an answer with an empty trace was not flagged (reason=%v overall=%.2f)",
			c.ID, score.Dimensions[judge.DimensionReason], score.Overall)
	}
}
