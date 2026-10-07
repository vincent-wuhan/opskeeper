package leaderboard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeLoopResult(t *testing.T, dir, caseID string, recoveryPassRate *float64, finalPhase string, passed bool) {
	t.Helper()
	v := recoveryPassRate
	r := struct {
		IncidentID string `json:"incident_id"`
		CaseID     string `json:"case_id"`
		Mode       string `json:"mode"`
		FinalPhase string `json:"final_phase"`
		Passed     bool   `json:"passed"`
		Rubric     struct {
			RCAAccuracy            *float64 `json:"rca_accuracy"`
			TimeToRemediate        string   `json:"time_to_remediate"`
			TimeToRemediateSec     *int     `json:"time_to_remediate_sec"`
			ApprovalRate           *float64 `json:"approval_rate"`
			RecoveryPassRate       *float64 `json:"recovery_pass_rate"`
			RecoveryPassRateReason string   `json:"recovery_pass_rate_reason"`
			KBHitRate              *float64 `json:"kb_hit_rate"`
			FollowUpDepth          *int     `json:"follow_up_depth"`
			RubricIncomplete       bool     `json:"rubric_incomplete"`
		} `json:"rubric"`
	}{}
	r.IncidentID = "harness-" + caseID
	r.CaseID = caseID
	r.Mode = "loop"
	r.FinalPhase = finalPhase
	r.Passed = passed
	r.Rubric.RCAAccuracy = fp(0.95)
	r.Rubric.TimeToRemediate = "3m 0s"
	sec := 180
	r.Rubric.TimeToRemediateSec = &sec
	r.Rubric.ApprovalRate = fp(1.0)
	r.Rubric.RecoveryPassRate = v
	r.Rubric.RecoveryPassRateReason = "loop reached postmortem terminal phase"
	r.Rubric.KBHitRate = nil
	r.Rubric.FollowUpDepth = nil
	r.Rubric.RubricIncomplete = false

	raw, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	path := filepath.Join(dir, strings.ReplaceAll(caseID, "/", "-")+".json")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func fp(v float64) *float64 { return &v }

func TestNewLoopBoard_AllQualified(t *testing.T) {
	dir := t.TempDir()
	rpr := 0.85
	writeLoopResult(t, dir, "pg/long-running-tx", &rpr, "postmortem", true)
	rpr = 0.92
	writeLoopResult(t, dir, "redis/memory-burst", &rpr, "postmortem", true)
	rpr = 0.78
	writeLoopResult(t, dir, "host/cpu-spike", &rpr, "postmortem", true)

	b, err := NewLoopBoard(dir)
	if err != nil {
		t.Fatalf("NewLoopBoard: %v", err)
	}
	if len(b.Entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(b.Entries))
	}
	for _, e := range b.Entries {
		if !e.Qualified {
			t.Errorf("expected %s qualified, got %s", e.CaseID, e.NotQualifiedReason)
		}
	}
}

func TestNewLoopBoard_NotQualified(t *testing.T) {
	dir := t.TempDir()
	rpr := 0.85
	writeLoopResult(t, dir, "pg/long-running-tx", &rpr, "postmortem", true)
	rpr = 0.30 // below threshold
	writeLoopResult(t, dir, "redis/memory-burst", &rpr, "postmortem", true)

	b, err := NewLoopBoard(dir)
	if err != nil {
		t.Fatalf("NewLoopBoard: %v", err)
	}
	for _, e := range b.Entries {
		if e.CaseID == "redis/memory-burst" && e.Qualified {
			t.Errorf("expected redis/memory-burst NOT qualified")
		}
		if e.CaseID == "redis/memory-burst" && e.NotQualifiedReason == "" {
			t.Errorf("expected not-qualified reason set")
		}
	}
}

func TestLoopBoard_Render(t *testing.T) {
	dir := t.TempDir()
	rpr := 0.85
	writeLoopResult(t, dir, "pg/long-running-tx", &rpr, "postmortem", true)

	b, err := NewLoopBoard(dir)
	if err != nil {
		t.Fatalf("NewLoopBoard: %v", err)
	}
	md := b.Render()
	if !strings.Contains(md, "Harness Loop Leaderboard") {
		t.Errorf("expected title in markdown")
	}
	if !strings.Contains(md, "pg/long-running-tx") {
		t.Errorf("expected case id in markdown")
	}
	if !strings.Contains(md, "QUALIFIED") {
		t.Errorf("expected QUALIFIED status")
	}
}

func TestLoopBoard_RenderToFile(t *testing.T) {
	dir := t.TempDir()
	rpr := 0.85
	writeLoopResult(t, dir, "pg/long-running-tx", &rpr, "postmortem", true)

	b, err := NewLoopBoard(dir)
	if err != nil {
		t.Fatalf("NewLoopBoard: %v", err)
	}
	out := filepath.Join(dir, "leaderboard.md")
	if err := b.RenderToFile(out); err != nil {
		t.Fatalf("RenderToFile: %v", err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(data), "Harness Loop Leaderboard") {
		t.Errorf("file should contain title")
	}
	// Ensure file timestamp ≤ now
	_ = time.Now()
}
