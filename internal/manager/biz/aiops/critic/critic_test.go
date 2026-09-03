package critic

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSpawner is a deterministic Spawner for tests. It returns a
// canned transcript per call; tests configure the queue to script
// the critic's behavior across rounds.
type fakeSpawner struct {
	mu          sync.Mutex
	transcripts []string
	calls       int
	spawnErr    error
	waitErr     error
	waitDelay   time.Duration
}

func (f *fakeSpawner) SpawnWorker(ctx context.Context, req SpawnRequest) (string, error) {
	if f.spawnErr != nil {
		return "", f.spawnErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return fmt.Sprintf("worker-%d", f.calls), nil
}

func (f *fakeSpawner) GetWorkerResult(ctx context.Context, workerID string, timeout time.Duration) (string, error) {
	if f.waitErr != nil {
		return "", f.waitErr
	}
	if f.waitDelay > 0 {
		time.Sleep(f.waitDelay)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	// The caller (Loop.runOneRound) is single-threaded per round, so
	// we pop the head of the queue each call. If a test forgets to
	// queue a transcript, return an empty one (which makes ParseAudit
	// fail — the test will see the error).
	if len(f.transcripts) == 0 {
		return "", nil
	}
	t := f.transcripts[0]
	f.transcripts = f.transcripts[1:]
	return t, nil
}

func newValidReport() *PrimaryReport {
	return &PrimaryReport{
		SessionID:  "sess-1",
		IncidentID: "inc-1",
		Severity:   "critical",
		RootCause:  "nginx config change introduced bad upstream",
		CausalChain: []string{
			"config push (commit abc123) → nginx reload → 502",
		},
		Symptom:    "service returning 502",
		Confidence: 0.85,
		ToolCalls: []ToolCallStub{
			{Tool: "query_change_events", ResultSummary: "found commit abc123"},
			{Tool: "query_logql", ResultSummary: "502 spike post-reload"},
		},
	}
}

func validAuditJSON() string {
	return `{"issues":[],"needs_correction":false,"summary":"audit pass"}`
}

func auditWithIssueJSON(issueType IssueType, severity IssueSeverity) string {
	return fmt.Sprintf(`{"issues":[{"type":%q,"location":"root_cause","severity":%q,"evidence":"x","suggestion":"y"}],"needs_correction":true,"summary":"one issue"}`, issueType, severity)
}

func TestShouldTrigger(t *testing.T) {
	cases := []struct {
		severity string
		want     bool
	}{
		{"critical", true},
		{"CRITICAL", true},
		{"P0", true},
		{"p1", true},
		{"warning", false},
		{"info", false},
		{"", false},
	}
	for _, c := range cases {
		t.Run(c.severity, func(t *testing.T) {
			if got := ShouldTrigger(c.severity); got != c.want {
				t.Errorf("ShouldTrigger(%q) = %v, want %v", c.severity, got, c.want)
			}
		})
	}
}

func TestParseAudit_Valid(t *testing.T) {
	transcript := "Some thinking...\n\n" + validAuditJSON() + "\nDone."
	a, err := ParseAudit(transcript)
	if err != nil {
		t.Fatalf("ParseAudit: %v", err)
	}
	if a.NeedsCorrection {
		t.Errorf("NeedsCorrection = true, want false")
	}
	if a.Summary != "audit pass" {
		t.Errorf("Summary = %q, want %q", a.Summary, "audit pass")
	}
}

func TestParseAudit_Malformed(t *testing.T) {
	if _, err := ParseAudit("no json here"); err == nil {
		t.Errorf("expected error for malformed input")
	}
}

func TestParseAudit_MultipleJSONTakesLast(t *testing.T) {
	// The critic may emit a thinking block that itself contains a JSON
	// example. The last valid audit wins.
	transcript := "First attempt:\n" + auditWithIssueJSON(IssueUnevidencedClaim, SeverityMajor) +
		"\nRevised:\n" + validAuditJSON()
	a, err := ParseAudit(transcript)
	if err != nil {
		t.Fatalf("ParseAudit: %v", err)
	}
	if a.NeedsCorrection {
		t.Errorf("expected last valid audit to pass; got needs_correction=true")
	}
}

func TestCriticAudit_HasCriticalIssue(t *testing.T) {
	a := &CriticAudit{
		Issues: []Issue{
			{Type: IssueMissedTool, Severity: SeverityMajor},
			{Type: IssueBrokenChain, Severity: SeverityCritical},
		},
	}
	if !a.HasCriticalIssue() {
		t.Errorf("expected HasCriticalIssue=true")
	}
	a2 := &CriticAudit{
		Issues: []Issue{{Type: IssueMissedTool, Severity: SeverityMajor}},
	}
	if a2.HasCriticalIssue() {
		t.Errorf("expected HasCriticalIssue=false")
	}
}

func TestLoop_SkipsBelowCritical(t *testing.T) {
	sp := &fakeSpawner{}
	l := New(sp)
	report := newValidReport()
	report.Severity = "warning"

	res, err := l.Run(context.Background(), report)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sp.calls != 0 {
		t.Errorf("expected zero spawns for low severity, got %d", sp.calls)
	}
	if res.Audit == nil || !strings.Contains(res.Audit.Summary, "skipped") {
		t.Errorf("expected skip summary, got %+v", res.Audit)
	}
}

func TestLoop_FirstRoundPass(t *testing.T) {
	sp := &fakeSpawner{transcripts: []string{validAuditJSON()}}
	l := New(sp)
	report := newValidReport()

	res, err := l.Run(context.Background(), report)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sp.calls != 1 {
		t.Errorf("expected 1 spawn (first round pass), got %d", sp.calls)
	}
	if res.Rounds != 1 {
		t.Errorf("Rounds = %d, want 1", res.Rounds)
	}
	if res.GaveUp {
		t.Errorf("unexpected gave_up")
	}
	if res.Audit.NeedsCorrection {
		t.Errorf("audit should not need correction")
	}
}

func TestLoop_CriticalIssueStopsEvenIfNeedsCorrectionFalse(t *testing.T) {
	// The critic returns no needs_correction but raises a critical
	// issue (e.g. "evidence is missing for the only causal hop").
	// The loop must surface the issue and NOT mark the report as final.
	transcript := `{"issues":[{"type":"broken_chain","location":"chain[0]","severity":"critical","evidence":"x","suggestion":"y"}],"needs_correction":false,"summary":"silent critical"}`
	sp := &fakeSpawner{transcripts: []string{transcript, transcript}}
	l := New(sp)
	report := newValidReport()

	res, err := l.Run(context.Background(), report)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Audit.HasCriticalIssue() {
		t.Errorf("expected critical issue")
	}
	// First round: critical issue found → second round would correct,
	// but MaxRounds=2 only allows 1 correction. With no corrector
	// wired, it gives up.
	if res.Rounds != 2 {
		t.Errorf("Rounds = %d, want 2 (1 audit + 1 correction attempt)", res.Rounds)
	}
	if !res.GaveUp {
		t.Errorf("expected gave_up=true (no corrector wired)")
	}
}

func TestLoop_MaxRoundsGivesUp(t *testing.T) {
	// 3 rounds all return needs_correction=true. MaxRounds=2 → 2
	// audits, then gave_up.
	sp := &fakeSpawner{
		transcripts: []string{
			auditWithIssueJSON(IssueMissedTool, SeverityMajor),
			auditWithIssueJSON(IssueMissedTool, SeverityMajor),
			auditWithIssueJSON(IssueMissedTool, SeverityMajor), // never used
		},
	}
	l := New(sp)
	l.MaxRounds = 2

	res, err := l.Run(context.Background(), newValidReport())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sp.calls != 2 {
		t.Errorf("expected 2 spawns, got %d", sp.calls)
	}
	if !res.GaveUp {
		t.Errorf("expected gave_up=true after MaxRounds")
	}
	if res.Rounds != 2 {
		t.Errorf("Rounds = %d, want 2", res.Rounds)
	}
}

func TestLoop_SpawnErrorAborts(t *testing.T) {
	sp := &fakeSpawner{spawnErr: fmt.Errorf("spawn boom")}
	l := New(sp)

	res, err := l.Run(context.Background(), newValidReport())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(res.AbortReason, "spawn boom") {
		t.Errorf("AbortReason = %q, want spawn boom", res.AbortReason)
	}
}

func TestLoop_CorrectionWired(t *testing.T) {
	// When CorrectFunc is wired, the loop audits → requests
	// correction → re-audits. The corrected report overrides
	// res.FinalReport.
	prev := CorrectFunc
	defer func() { CorrectFunc = prev }()

	corrected := newValidReport()
	corrected.RootCause = "corrected root cause"
	corrections := 0
	CorrectFunc = func(ctx context.Context, r *PrimaryReport, a *CriticAudit) (*PrimaryReport, error) {
		corrections++
		return corrected, nil
	}

	// Round 1 finds issue, round 2 finds issue still (limit 2 →
	// gives up). Both rounds call the corrector.
	sp := &fakeSpawner{
		transcripts: []string{
			auditWithIssueJSON(IssueMissedTool, SeverityMajor),
			auditWithIssueJSON(IssueMissedTool, SeverityMajor),
		},
	}
	l := New(sp)
	l.MaxRounds = 2

	res, err := l.Run(context.Background(), newValidReport())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if corrections != 1 {
		t.Errorf("expected 1 correction call, got %d", corrections)
	}
	if !res.Corrected {
		t.Errorf("expected Corrected=true")
	}
	if res.FinalReport.RootCause != "corrected root cause" {
		t.Errorf("FinalReport.RootCause = %q, want corrected", res.FinalReport.RootCause)
	}
}

func TestBuildCriticPrompt_IncludesReport(t *testing.T) {
	report := newValidReport()
	p := buildCriticPrompt(report, 1)
	if !strings.Contains(p, "round 1") {
		t.Errorf("prompt missing round number")
	}
	if !strings.Contains(p, "nginx config change") {
		t.Errorf("prompt missing report content")
	}
}

func TestBuildCriticPrompt_ReAuditNote(t *testing.T) {
	r1 := buildCriticPrompt(newValidReport(), 1)
	r2 := buildCriticPrompt(newValidReport(), 2)
	if !strings.Contains(r2, "re-audit") {
		t.Errorf("round 2 prompt missing re-audit instruction")
	}
	if strings.Contains(r1, "re-audit") {
		t.Errorf("round 1 prompt should not mention re-audit")
	}
}
