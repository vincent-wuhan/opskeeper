package leaderboard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func f64(v float64) *float64 { return &v }

// boardOf 造一张只有指定 case 的看板。
func boardOf(entries ...*LoopBoardEntry) *LoopBoard {
	return &LoopBoard{Entries: entries, GeneratedAt: time.Unix(0, 0).UTC(), RunDir: "t", RecoveryPassRateThreshold: 0.5}
}

func fullEntry(caseID string, rca, ap, rec, kb float64) *LoopBoardEntry {
	return &LoopBoardEntry{
		CaseID: caseID, Passed: true, Qualified: true,
		RCAAccuracy: f64(rca), ApprovalRate: f64(ap),
		RecoveryPassRate: f64(rec), KBHitRate: f64(kb),
	}
}

// 一个没测出任何指标的 case 必须被单独记下，而不是当成 0 分。
// 把它当成 0 分会让下一次任何非负分数都被判成 100% 回归。
func TestAnEntryWithNoMeasuredMetricIsNotScoredAsZero(t *testing.T) {
	e := &LoopBoardEntry{CaseID: "host/cpu-spike", RubricIncomplete: true}
	if score, ok := AggregateScore(e); ok {
		t.Fatalf("AggregateScore = %f, ok; want ok=false so the case is not locked as 0", score)
	}
	base, unmeasured := LockBaseline(boardOf(e), "tester")
	if len(base.Scores) != 0 {
		t.Fatalf("baseline scores = %v, want the unmeasured case left out entirely", base.Scores)
	}
	if len(unmeasured) != 1 || unmeasured[0] != "host/cpu-spike" {
		t.Fatalf("unmeasured = %v, want it named out loud", unmeasured)
	}
}

// 聚合只取已测量的那几个：只有 rca 的 case 不该因为另外三个没跑而看起来掉一半。
func TestAggregateSkipsUnmeasuredMetricsRatherThanScoringThemZero(t *testing.T) {
	e := &LoopBoardEntry{CaseID: "pg/lock-waits", RCAAccuracy: f64(0.8)}
	score, ok := AggregateScore(e)
	if !ok {
		t.Fatal("AggregateScore ok=false, want the one measured metric to count")
	}
	if score != 0.8 {
		t.Fatalf("score = %f, want 0.8", score)
	}
}

// 三个分类必须分开：它们都不是"没退步"，合成一句"无回归"就是谎报。
func TestTheThreeKindsOfUnjudgedCaseAreCountedSeparately(t *testing.T) {
	base := &Baseline{Scores: map[string]float64{"pg/old": 0.9, "k8s/gone": 0.7}}
	board := boardOf(
		fullEntry("pg/old", 0.9, 0.9, 0.9, 0.9),
		fullEntry("k8s/new", 0.9, 0.9, 0.9, 0.9),
		&LoopBoardEntry{CaseID: "host/none"},
	)
	rep := CheckBoard(board, base)
	if len(rep.Regressions) != 0 {
		t.Fatalf("regressions = %v, want none", rep.Regressions)
	}
	if len(rep.New) != 1 || rep.New[0] != "k8s/new" {
		t.Fatalf("New = %v, want the never-locked case", rep.New)
	}
	if len(rep.Missing) != 1 || rep.Missing[0] != "k8s/gone" {
		t.Fatalf("Missing = %v, want the case that was not run this time", rep.Missing)
	}
	if len(rep.Unmeasured) != 1 || rep.Unmeasured[0] != "host/none" {
		t.Fatalf("Unmeasured = %v, want the case with no score", rep.Unmeasured)
	}
	if rep.Unaccounted() != 3 {
		t.Fatalf("Unaccounted = %d, want 3", rep.Unaccounted())
	}
}

// 基线为 0 时 drop% 无定义。"无定义"绝不能读成"没有回归"。
func TestAZeroBaselineIsABlockerNotAnAbsence(t *testing.T) {
	r := CheckRegressionFor(&Baseline{}, "k8s/pod-oom", 0.0, 0.5)
	if r.Severity != SeverityBlock {
		t.Fatalf("severity = %q, want block: a zero baseline makes the drop undefined", r.Severity)
	}
}

func TestDropThresholdsLandOnTheDocumentedSeverities(t *testing.T) {
	cases := []struct {
		baseline, current float64
		want              Severity
	}{
		{0.80, 0.80, SeverityNone},
		{0.80, 0.79, SeverityNone},  // 1.25% drop
		{0.80, 0.76, SeverityWarn},  // 5.0% drop
		{0.80, 0.68, SeverityBlock}, // 15.0% drop
	}
	for _, c := range cases {
		got := CheckRegressionFor(&Baseline{}, "x", c.baseline, c.current).Severity
		if got != c.want {
			t.Errorf("baseline %.2f current %.2f: severity = %q, want %q", c.baseline, c.current, got, c.want)
		}
	}
}

// 一个没跑 recovery_pass_rate 的 LoopResult 不该让 leaderboard 崩掉。
// 这一条是本轮实测撞出来的：thresholdMarker 收的是值，调用点先解引用，
// 于是 formatFloatPtr 就在旁边一行对 nil 返回 "—"，而先执行的那个赢了。
func TestRenderingAnEntryWithoutARecoveryRateDoesNotPanic(t *testing.T) {
	b := boardOf(&LoopBoardEntry{CaseID: "host/cpu-spike", Passed: true, RCAAccuracy: f64(0.7)})
	md := b.Render()
	if md == "" {
		t.Fatal("Render returned nothing")
	}
}

// 基线文件不存在时，错误必须说"尚未建立"，让调用方非零退出。
// 把它读成"零回归"发生在最需要它说实话的时刻：一次刚引入回归的 CI。
func TestAMissingBaselineFileSaysItWasNeverEstablished(t *testing.T) {
	_, err := LoadBaseline(filepath.Join(t.TempDir(), "baseline.json"))
	if err == nil {
		t.Fatal("LoadBaseline on a missing file returned no error")
	}
	if got := err.Error(); !strings.Contains(got, "not established") {
		t.Fatalf("error = %q, want it to say the baseline was never established", got)
	}
}

func TestAnEmptyBaselineFileIsRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "baseline.json")
	if err := os.WriteFile(path, []byte(`{"scores":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadBaseline(path); err == nil {
		t.Fatal("LoadBaseline accepted a baseline holding no scores")
	}
}

func TestBaselineRoundTripsThroughDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "baseline.json")
	base, _ := LockBaseline(boardOf(fullEntry("pg/lock-waits", 0.9, 0.8, 1.0, 0.7)), "tester")
	if err := SaveBaseline(path, base); err != nil {
		t.Fatal(err)
	}
	back, err := LoadBaseline(path)
	if err != nil {
		t.Fatal(err)
	}
	if back.Scores["pg/lock-waits"] != base.Scores["pg/lock-waits"] {
		t.Fatalf("scores = %v, want %v", back.Scores, base.Scores)
	}
	if len(back.Metrics) != len(BoardMetrics) {
		t.Fatalf("metrics = %v, want the aggregation recorded in the file", back.Metrics)
	}
	if back.LockedBy != "tester" {
		t.Fatalf("locked_by = %q, want it recorded so a baseline says who set it", back.LockedBy)
	}
}
