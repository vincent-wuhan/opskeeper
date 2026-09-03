package leaderboard

import (
	"math"
	"testing"
	"time"
)

func TestRecord_ValidatesInputs(t *testing.T) {
	lb := NewLeaderboard()
	if err := lb.Record(nil); err == nil {
		t.Errorf("expected error on nil entry")
	}
	e := &Entry{CaseID: "", Score: 0.5}
	if err := lb.Record(e); err == nil {
		t.Errorf("expected error on empty caseID")
	}
	e2 := &Entry{CaseID: "c1", Score: 1.5}
	if err := lb.Record(e2); err == nil {
		t.Errorf("expected error on out-of-range score")
	}
}

func TestRecord_EstablishesBaseline(t *testing.T) {
	lb := NewLeaderboard()
	if err := lb.Record(&Entry{CaseID: "pg/lock-waits", Score: 0.85}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	base, ok := lb.Baseline("pg/lock-waits")
	if !ok || base != 0.85 {
		t.Errorf("baseline = %f (ok=%v), want 0.85", base, ok)
	}
}

func TestRecord_AutoPromotesBaseline(t *testing.T) {
	lb := NewLeaderboard()
	lb.Record(&Entry{CaseID: "c1", Score: 0.7})
	lb.Record(&Entry{CaseID: "c1", Score: 0.85}) // higher → new baseline
	lb.Record(&Entry{CaseID: "c1", Score: 0.75}) // lower → baseline stays
	base, _ := lb.Baseline("c1")
	if base != 0.85 {
		t.Errorf("baseline = %f, want 0.85 (highest)", base)
	}
}

func TestSetBaseline_OverridesAuto(t *testing.T) {
	lb := NewLeaderboard()
	lb.Record(&Entry{CaseID: "c1", Score: 0.7})
	if err := lb.SetBaseline("c1", 0.5); err != nil {
		t.Fatalf("SetBaseline: %v", err)
	}
	base, _ := lb.Baseline("c1")
	if base != 0.5 {
		t.Errorf("baseline = %f, want 0.5 (manual override)", base)
	}
}

func TestSetBaseline_RejectsInvalid(t *testing.T) {
	lb := NewLeaderboard()
	if err := lb.SetBaseline("c1", 1.5); err == nil {
		t.Errorf("expected error on out-of-range score")
	}
}

func TestCheckRegression_NoBaseline(t *testing.T) {
	lb := NewLeaderboard()
	r := lb.CheckRegression("never-ran", 0.5)
	if r.Severity != SeverityNone {
		t.Errorf("Severity = %q, want none (no baseline)", r.Severity)
	}
	if r.Message == "" {
		t.Errorf("Message should explain missing baseline")
	}
}

func TestCheckRegression_NoDrop(t *testing.T) {
	lb := NewLeaderboard()
	lb.Record(&Entry{CaseID: "c1", Score: 0.85})
	r := lb.CheckRegression("c1", 0.90)
	if r.Severity != SeverityNone {
		t.Errorf("Severity = %q, want none (improved)", r.Severity)
	}
}

func TestCheckRegression_WarnAt5Percent(t *testing.T) {
	lb := NewLeaderboard()
	lb.Record(&Entry{CaseID: "c1", Score: 1.0})
	// 5% drop → warn (0.95 vs 1.0 = exactly 5%)
	r := lb.CheckRegression("c1", 0.95)
	if r.Severity != SeverityWarn {
		t.Errorf("Severity = %q, want warn at 5%% drop (got %+v)", r.Severity, r)
	}
}

func TestCheckRegression_BlockAt15Percent(t *testing.T) {
	lb := NewLeaderboard()
	lb.Record(&Entry{CaseID: "c1", Score: 1.0})
	// 15% drop → block
	r := lb.CheckRegression("c1", 0.85)
	if r.Severity != SeverityBlock {
		t.Errorf("Severity = %q, want block at 15%% drop", r.Severity)
	}
	if r.DropPercent < 14.9 || r.DropPercent > 15.1 {
		t.Errorf("DropPercent = %f, want ~15.0", r.DropPercent)
	}
}

func TestCheckRegression_Below5Percent_NoSeverity(t *testing.T) {
	lb := NewLeaderboard()
	lb.Record(&Entry{CaseID: "c1", Score: 1.0})
	r := lb.CheckRegression("c1", 0.96) // 4% drop
	if r.Severity != SeverityNone {
		t.Errorf("4%% drop should not warn, got %q", r.Severity)
	}
}

func TestHistory_PreservesOrder(t *testing.T) {
	lb := NewLeaderboard()
	now := time.Now()
	lb.Record(&Entry{CaseID: "c1", Score: 0.7, Timestamp: now})
	lb.Record(&Entry{CaseID: "c1", Score: 0.8, Timestamp: now.Add(time.Second)})
	lb.Record(&Entry{CaseID: "c1", Score: 0.75, Timestamp: now.Add(2 * time.Second)})
	h := lb.History("c1")
	if len(h) != 3 {
		t.Fatalf("len = %d, want 3", len(h))
	}
	if h[0].Score != 0.7 || h[1].Score != 0.8 || h[2].Score != 0.75 {
		t.Errorf("order broken: %v", []float64{h[0].Score, h[1].Score, h[2].Score})
	}
}

func TestLastEntry_ReturnsMostRecent(t *testing.T) {
	lb := NewLeaderboard()
	lb.Record(&Entry{CaseID: "c1", Score: 0.7})
	lb.Record(&Entry{CaseID: "c1", Score: 0.8})
	last, ok := lb.LastEntry("c1")
	if !ok || last.Score != 0.8 {
		t.Errorf("LastEntry score = %f (ok=%v), want 0.8", last.Score, ok)
	}
}

func TestFlaggedEntries_FiltersAndSorts(t *testing.T) {
	lb := NewLeaderboard()
	now := time.Now()
	lb.Record(&Entry{CaseID: "c1", Score: 0.5, Flagged: true, Timestamp: now})
	lb.Record(&Entry{CaseID: "c1", Score: 0.8, Flagged: false, Timestamp: now.Add(time.Second)})
	lb.Record(&Entry{CaseID: "c2", Score: 0.4, Flagged: true, Timestamp: now.Add(2 * time.Second)})
	flagged := lb.FlaggedEntries()
	if len(flagged) != 2 {
		t.Fatalf("len = %d, want 2 (flagged only)", len(flagged))
	}
	// Time desc: c2 (newer) before c1
	if flagged[0].CaseID != "c2" || flagged[1].CaseID != "c1" {
		t.Errorf("sort broken: %s before %s", flagged[0].CaseID, flagged[1].CaseID)
	}
}

func TestBlockers_ReturnsOnlyBlockSeverity(t *testing.T) {
	lb := NewLeaderboard()
	lb.Record(&Entry{CaseID: "c1", Score: 1.0})  // baseline
	lb.Record(&Entry{CaseID: "c1", Score: 0.8})  // 20% drop → block
	lb.Record(&Entry{CaseID: "c2", Score: 1.0})  // baseline
	lb.Record(&Entry{CaseID: "c2", Score: 0.92}) // 8% drop → warn
	blocks := lb.Blockers()
	if len(blocks) != 1 {
		t.Fatalf("len = %d, want 1", len(blocks))
	}
	if blocks[0].CaseID != "c1" {
		t.Errorf("blocker = %s, want c1", blocks[0].CaseID)
	}
}

func TestWarns_ReturnsOnlyWarnSeverity(t *testing.T) {
	lb := NewLeaderboard()
	lb.Record(&Entry{CaseID: "c1", Score: 1.0})
	lb.Record(&Entry{CaseID: "c1", Score: 0.92}) // warn
	warns := lb.Warns()
	if len(warns) != 1 {
		t.Fatalf("len = %d, want 1", len(warns))
	}
}

func TestSize(t *testing.T) {
	lb := NewLeaderboard()
	if lb.Size() != 0 {
		t.Errorf("empty lb Size = %d, want 0", lb.Size())
	}
	lb.Record(&Entry{CaseID: "c1", Score: 0.8})
	lb.Record(&Entry{CaseID: "c2", Score: 0.7})
	lb.Record(&Entry{CaseID: "c1", Score: 0.85}) // same case
	if lb.Size() != 2 {
		t.Errorf("Size = %d, want 2 (unique cases)", lb.Size())
	}
}

func TestRegressionMath(t *testing.T) {
	// 防止浮点误判：直接验证 (base - current) / base 计算
	cases := []struct {
		base, current, expectedPct float64
	}{
		{1.0, 0.95, 5.0},
		{1.0, 0.85, 15.0},
		{0.8, 0.76, 5.0},  // 0.04 / 0.8 = 5%
		{0.5, 0.25, 50.0}, // severe
	}
	for _, tc := range cases {
		got := (tc.base - tc.current) / tc.base * 100
		if math.Abs(got-tc.expectedPct) > 0.01 {
			t.Errorf("dropPct(%f, %f) = %f, want %f", tc.base, tc.current, got, tc.expectedPct)
		}
	}
}

func TestRecord_TimestampAutoFill(t *testing.T) {
	lb := NewLeaderboard()
	before := time.Now()
	if err := lb.Record(&Entry{CaseID: "c1", Score: 0.5}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	after := time.Now()
	e, _ := lb.LastEntry("c1")
	if e.Timestamp.Before(before) || e.Timestamp.After(after) {
		t.Errorf("Timestamp %v not in [%v, %v]", e.Timestamp, before, after)
	}
}
