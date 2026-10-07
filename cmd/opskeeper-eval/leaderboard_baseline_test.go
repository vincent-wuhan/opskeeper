package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeLoopResult 造一份 LoopResult JSON，四个指标缺的那些留 nil。
func writeLoopResult(t *testing.T, dir, caseID string, scores map[string]float64) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	rubric := map[string]any{}
	for _, key := range []string{"rca_accuracy", "approval_rate", "recovery_pass_rate", "kb_hit_rate"} {
		if v, ok := scores[key]; ok {
			rubric[key] = v
		}
	}
	body := map[string]any{
		"incident_id": "i-" + caseID,
		"case_id":     caseID,
		"mode":        "loop",
		"final_phase": "recovered",
		"passed":      true,
		"rubric":      rubric,
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(dir, caseID+".json")
	if err := os.WriteFile(name, raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

func leaderboardArgs(dir, outDir, baseline string, extra ...string) []string {
	return append([]string{
		"--dir", dir, "--out-dir", outDir, "--baseline-file", baseline,
	}, extra...)
}

// 锁基线再原样对照，必须干净通过。走的是完整命令路径，不是库函数。
func TestLockingThenCheckingAnUnchangedBoardPasses(t *testing.T) {
	root := t.TempDir()
	dir, base := filepath.Join(root, "loop"), filepath.Join(root, "baseline.json")
	writeLoopResult(t, dir, "pg-lock-waits", map[string]float64{
		"rca_accuracy": 0.9, "approval_rate": 0.8, "recovery_pass_rate": 1.0, "kb_hit_rate": 0.7,
	})
	if err := cmdLeaderboard(context.Background(), leaderboardArgs(dir, root, base, "--lock-baseline")); err != nil {
		t.Fatalf("lock: %v", err)
	}
	if _, err := os.Stat(base); err != nil {
		t.Fatalf("baseline file not written: %v", err)
	}
	if err := cmdLeaderboard(context.Background(), leaderboardArgs(dir, root, base, "--check-regression")); err != nil {
		t.Fatalf("check on an unchanged board: %v", err)
	}
}

// 掉 25% 必须非零退出——这是这条命令存在的理由。
func TestABlockingDropFailsTheCommand(t *testing.T) {
	root := t.TempDir()
	dir, base := filepath.Join(root, "loop"), filepath.Join(root, "baseline.json")
	good := map[string]float64{"rca_accuracy": 0.8, "approval_rate": 0.8, "recovery_pass_rate": 0.8, "kb_hit_rate": 0.8}
	writeLoopResult(t, dir, "k8s-pod-oom", good)
	if err := cmdLeaderboard(context.Background(), leaderboardArgs(dir, root, base, "--lock-baseline")); err != nil {
		t.Fatal(err)
	}
	bad := map[string]float64{"rca_accuracy": 0.6, "approval_rate": 0.6, "recovery_pass_rate": 0.6, "kb_hit_rate": 0.6}
	writeLoopResult(t, dir, "k8s-pod-oom", bad)
	if err := cmdLeaderboard(context.Background(), leaderboardArgs(dir, root, base, "--check-regression")); err == nil {
		t.Fatal("check on a 25% drop returned nil, want a non-zero exit")
	}
	// 6% 的下降是 warn：默认放过，--fail-on-warn 才拦。
	mild := map[string]float64{"rca_accuracy": 0.75, "approval_rate": 0.75, "recovery_pass_rate": 0.75, "kb_hit_rate": 0.75}
	writeLoopResult(t, dir, "k8s-pod-oom", mild)
	if err := cmdLeaderboard(context.Background(), leaderboardArgs(dir, root, base, "--check-regression")); err != nil {
		t.Fatalf("a 6%% drop failed without --fail-on-warn: %v", err)
	}
	if err := cmdLeaderboard(context.Background(),
		leaderboardArgs(dir, root, base, "--check-regression", "--fail-on-warn")); err == nil {
		t.Fatal("a 6% drop passed with --fail-on-warn")
	}
}

// 没有基线就检查，必须非零退出并说清为什么。
// 把它当成"零回归"发生在最需要它说实话的时刻：一次刚引入回归的 CI。
func TestCheckingWithoutABaselineFailsInsteadOfReportingNoRegression(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "loop")
	base := filepath.Join(root, "never-locked.json")
	writeLoopResult(t, dir, "pg-lock-waits", map[string]float64{"rca_accuracy": 0.9})
	err := cmdLeaderboard(context.Background(), leaderboardArgs(dir, root, base, "--check-regression"))
	if err == nil {
		t.Fatal("check with no baseline returned nil, want a non-zero exit")
	}
	if got := err.Error(); !strings.Contains(got, "not established") {
		t.Fatalf("error = %q, want it to say the baseline was never established", got)
	}
}

// 基线里有、这次没跑的 case 必须被说出来。它不是回归，但把它算成"没退步"
// 就等于让一次漏跑通过了一次回归检查。
func TestACaseMissingFromThisRunIsNotCountedAsNoRegression(t *testing.T) {
	root := t.TempDir()
	dir, base := filepath.Join(root, "loop"), filepath.Join(root, "baseline.json")
	vals := map[string]float64{"rca_accuracy": 0.9, "approval_rate": 0.9, "recovery_pass_rate": 0.9, "kb_hit_rate": 0.9}
	writeLoopResult(t, dir, "pg-lock-waits", vals)
	writeLoopResult(t, dir, "k8s-pod-oom", vals)
	if err := cmdLeaderboard(context.Background(), leaderboardArgs(dir, root, base, "--lock-baseline")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "k8s-pod-oom.json")); err != nil {
		t.Fatal(err)
	}
	if err := cmdLeaderboard(context.Background(), leaderboardArgs(dir, root, base, "--check-regression")); err != nil {
		t.Fatalf("a dropped case must not by itself fail the check: %v", err)
	}
	var loaded struct {
		Scores map[string]float64 `json:"scores"`
	}
	raw, err := os.ReadFile(base)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &loaded); err != nil {
		t.Fatal(err)
	}
	if _, ok := loaded.Scores["k8s-pod-oom"]; !ok {
		t.Fatal("baseline lost the case; the missing one would then be silently untracked")
	}
}
