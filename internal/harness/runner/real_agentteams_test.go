package runner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testRealTraceID = "1234567890abcdef1234567890abcdef"

func validRealAgentTeamsBundle(t *testing.T) RealAgentTeamsEvidencePaths {
	t.Helper()
	dir := t.TempDir()
	start := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	phase := func(offset, duration time.Duration) realStatePhase {
		completed := start.Add(offset + duration)
		return realStatePhase{
			Status:      "completed",
			StartedAt:   start.Add(offset),
			CompletedAt: &completed,
		}
	}
	alert := phase(0, time.Second)
	alert.AssignedTo = "alerter"
	rca := phase(time.Second, time.Second)
	rca.AssignedTo = "investigator"
	critic := phase(2*time.Second, time.Second)
	critic.AssignedTo = "critic"
	review := phase(3*time.Second, time.Second)
	review.AssignedTo = "reviewer"
	hitl := phase(4*time.Second, time.Second)
	hitl.AssignedTo = "@admin:agentteams.local"
	hitl.ApprovalRequestID = "0ed74282-a074-4f20-9e0f-0b30f4eb5a34"
	repair := phase(5*time.Second, time.Second)
	repair.AssignedTo = "repairer"
	verification := phase(6*time.Second, time.Second)
	verification.AssignedTo = "verifier"
	postmortemPhase := phase(7*time.Second, time.Second)
	postmortemPhase.AssignedTo = "reporter"

	state := realStateEvidence{
		SchemaVersion: "v1",
		Tasks: map[string]realStateTaskEvidence{
			"task-host-cpu-spike-real": {
				IncidentID:   "host-cpu-spike-real",
				ProjectID:    "opskeeper-demo",
				MatrixRoomID: "!cpu:agentteams.local",
				Type:         "finite",
				AssignedTo:   "alerter",
				CreatedAt:    start,
				UpdatedAt:    start.Add(9 * time.Second),
				Status:       "completed",
				Phases: map[string]realStatePhase{
					"alert_dedup": alert, "rca": rca, "critic_audit": critic, "review": review,
					"hitl_approval": hitl, "repair_execution": repair,
					"verification": verification, "postmortem": postmortemPhase,
				},
				HITLDecisions: []realStateHITLDecision{{
					Admin:             "@admin:agentteams.local",
					Decision:          "approved",
					ApprovalRequestID: "0ed74282-a074-4f20-9e0f-0b30f4eb5a34",
					DecidedAt:         start.Add(5 * time.Second),
				}},
				AuditRefs: []realStateAuditRef{
					{AuditLogID: "audit-recovery", TraceID: testRealTraceID, EventID: "event-recovery"},
					{AuditLogID: "audit-verify", TraceID: testRealTraceID, EventID: "event-verify"},
				},
			},
		},
	}
	hitlEvidence := realHITLEvidence{
		SchemaVersion: realHITLSchemaVersion,
		ExecutionMode: realAgentTeamsExecutionMode,
		IncidentID:    "host-cpu-spike-real",
		TraceID:       testRealTraceID,
		RoomID:        "!cpu:agentteams.local",
		ProposalID:    "0ed74282-a074-4f20-9e0f-0b30f4eb5a34",
		PayloadHash:   "a665a45920422f9d417e4867efdc4fb8a04a1f3fff1fa07e998e86f7f7a27ae3",
		Action:        "kill_process",
		Resource:      "host:fixture",
		Parameters: realHITLParameters{
			Command: "kill_process", IncidentID: "host-cpu-spike-real",
			FixtureManifestID: "fixture-manifest-1", Reason: "terminate case-owned CPU processes",
		},
		MatrixEvent: realMatrixApprovalEvent{
			EventID: "$approval-event", Sender: "@admin:agentteams.local", Decision: "approve",
			Source: "human", ApprovedBy: "@admin:agentteams.local", DecidedAt: start.Add(5 * time.Second),
		},
	}
	mcpEvidence := realMCPEvidence{
		SchemaVersion: realMCPSchemaVersion,
		ExecutionMode: realAgentTeamsExecutionMode,
		IncidentID:    "host-cpu-spike-real",
		TraceID:       testRealTraceID,
		Calls: []realMCPCallEvidence{
			{
				Tool: "recovery.execute", Role: "repairer", Worker: "opskeeper-repairer", Status: "success",
				AuditLogID: "audit-recovery", ProposalID: hitlEvidence.ProposalID, PayloadHash: hitlEvidence.PayloadHash,
			},
			{
				Tool: "recovery.verify", Role: "verifier", Worker: "opskeeper-verifier", Status: "success", AuditLogID: "audit-verify",
			},
		},
	}
	fixture := func(stage string, capturedAt time.Time) realFixtureEvidence {
		evidence := realFixtureEvidence{
			SchemaVersion: realFixtureSchemaVersion, ExecutionMode: realAgentTeamsExecutionMode,
			IncidentID: "host-cpu-spike-real", TraceID: testRealTraceID, Stage: stage,
			FixtureManifestID: "fixture-manifest-1", CapturedAt: capturedAt,
			Status: "running", ProcessCount: 2,
			Metrics: []realFixtureMetric{{Name: "cpu_usage_percent", Values: []float64{96, 98, 97}, SampleSize: 3}},
		}
		if stage == "after" {
			evidence.Status = "terminated"
			evidence.Metrics = []realFixtureMetric{{Name: "cpu_usage_percent", Values: []float64{10, 12, 11}, SampleSize: 3}}
		}
		return evidence
	}
	postmortem := realPostmortemEvidence{
		SchemaVersion: realPostmortemSchemaVersion, ExecutionMode: realAgentTeamsExecutionMode,
		IncidentID: "host-cpu-spike-real", TraceID: testRealTraceID, GeneratedBy: "opskeeper-reporter",
		CompletedAt: start.Add(9 * time.Second), RootCause: "case-owned CPU processes saturated the host",
		Impact: "CPU was saturated", Resolution: "approved case-owned process termination",
		Verification: "CPU returned below 50% and processes were absent",
	}

	write := func(name string, value any) string {
		path := filepath.Join(dir, name)
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatalf("marshal %s: %v", name, err)
		}
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		return path
	}
	return RealAgentTeamsEvidencePaths{
		State: write("state.json", state), HITL: write("hitl.json", hitlEvidence),
		MCP: write("mcp.json", mcpEvidence), FixtureBefore: write("fixture-before.json", fixture("before", start)),
		FixtureAfter: write("fixture-after.json", fixture("after", start.Add(6*time.Second))),
		Postmortem:   write("postmortem.json", postmortem),
	}
}

func testRealRunnerOptions(t *testing.T, paths RealAgentTeamsEvidencePaths) LoopOptions {
	t.Helper()
	return LoopOptions{
		CaseID: "host/cpu-spike", Mode: ModeLoop, ExecutionMode: ExecutionModeRealAgentTeams,
		Env: EnvStaging, TenantID: "T1", IncidentID: "host-cpu-spike-real", TraceID: testRealTraceID,
		CasesDir: "../cases", OutDir: filepath.Join(t.TempDir(), "result"),
		RealAgentTeamsEvidence: paths,
	}
}

func TestRunLoop_RealAgentTeams_Completes(t *testing.T) {
	ctx := context.Background()
	opts := testRealRunnerOptions(t, validRealAgentTeamsBundle(t))
	result, err := RunLoop(ctx, opts, LoopDeps{})
	if err != nil {
		t.Fatalf("RunLoop real-agentteams: %v", err)
	}
	if result.ExecutionMode != ExecutionModeRealAgentTeams {
		t.Fatalf("execution_mode=%q", result.ExecutionMode)
	}
	if !result.Passed || result.FinalPhase != "postmortem" || len(result.Phases) != 8 {
		t.Fatalf("unexpected result: passed=%t final=%s phases=%d", result.Passed, result.FinalPhase, len(result.Phases))
	}
	if result.Metadata["trace_id"] != testRealTraceID || len(result.Metadata["evidence_sha256"].(map[string]string)) != 6 {
		t.Fatalf("trace/evidence metadata is incomplete: %#v", result.Metadata)
	}
	if strings.Contains(result.Rubric.RCAAccuracyReason, "dry-run") {
		t.Fatalf("real result must not carry a dry-run rubric reason: %q", result.Rubric.RCAAccuracyReason)
	}
}

func TestValidateRealAgentTeamsEvidence_RejectsInvalidBundles(t *testing.T) {
	ctx := context.Background()
	base := validRealAgentTeamsBundle(t)
	tests := []struct {
		name   string
		mutate func(paths *RealAgentTeamsEvidencePaths)
		want   string
	}{
		{
			name: "missing evidence path",
			mutate: func(paths *RealAgentTeamsEvidencePaths) {
				paths.MCP = ""
			},
			want: "MCP evidence path is required",
		},
		{
			name: "incident ID conflict",
			mutate: func(paths *RealAgentTeamsEvidencePaths) {
				mutateJSONFile(t, paths.HITL, "incident_id", "other-incident")
			},
			want: "incident_id/trace_id must equal harness input",
		},
		{
			name: "dry-run masquerading as real",
			mutate: func(paths *RealAgentTeamsEvidencePaths) {
				mutateJSONFile(t, paths.MCP, "execution_mode", "dry_run")
			},
			want: "execution must be real_agentteams",
		},
		{
			name: "missing phase",
			mutate: func(paths *RealAgentTeamsEvidencePaths) {
				mutateJSONFile(t, paths.State, "phases.review", nil)
			},
			want: "state phase review is missing",
		},
		{
			name: "cross-role recovery call",
			mutate: func(paths *RealAgentTeamsEvidencePaths) {
				mutateJSONFile(t, paths.MCP, "calls[0].role", "verifier")
			},
			want: "cross-role tool call",
		},
		{
			name: "fixture evidence carries PID",
			mutate: func(paths *RealAgentTeamsEvidencePaths) {
				mutateJSONFile(t, paths.FixtureBefore, "pid", 123)
			},
			want: `unknown field "pid"`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			paths := copyRealAgentTeamsBundle(t, base)
			test.mutate(&paths)
			_, err := ValidateRealAgentTeamsEvidence(ctx, "host-cpu-spike-real", testRealTraceID, paths)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v, want substring %q", err, test.want)
			}
		})
	}
}

func TestValidateRealAgentTeamsEvidence_RejectsMarkdownPostmortem(t *testing.T) {
	ctx := context.Background()
	paths := validRealAgentTeamsBundle(t)
	if err := os.WriteFile(paths.Postmortem, []byte("# postmortem\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := ValidateRealAgentTeamsEvidence(ctx, "host-cpu-spike-real", testRealTraceID, paths)
	if err == nil || !strings.Contains(err.Error(), "postmortem:") {
		t.Fatalf("error=%v, want JSON postmortem decode failure", err)
	}
}

func copyRealAgentTeamsBundle(t *testing.T, source RealAgentTeamsEvidencePaths) RealAgentTeamsEvidencePaths {
	t.Helper()
	dir := t.TempDir()
	destination := source
	for name, path := range map[string]string{
		"state": source.State, "hitl": source.HITL, "mcp": source.MCP,
		"fixture-before": source.FixtureBefore, "fixture-after": source.FixtureAfter,
		"postmortem": source.Postmortem,
	} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		nextPath := filepath.Join(dir, name+".json")
		if err := os.WriteFile(nextPath, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		switch name {
		case "state":
			destination.State = nextPath
		case "hitl":
			destination.HITL = nextPath
		case "mcp":
			destination.MCP = nextPath
		case "fixture-before":
			destination.FixtureBefore = nextPath
		case "fixture-after":
			destination.FixtureAfter = nextPath
		case "postmortem":
			destination.Postmortem = nextPath
		}
	}
	return destination
}

func mutateJSONFile(t *testing.T, path string, field string, value any) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	if field == "phases.review" {
		task := firstStateTask(t, document)
		phases := task["phases"].(map[string]any)
		delete(phases, "review")
		updated, err := json.Marshal(document)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, updated, 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	switch field {
	case "calls[0].role":
		document["calls"].([]any)[0].(map[string]any)["role"] = value
	default:
		document[field] = value
	}
	updated, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, updated, 0o600); err != nil {
		t.Fatal(err)
	}
}

func firstStateTask(t *testing.T, document map[string]any) map[string]any {
	t.Helper()
	tasks := document["tasks"].(map[string]any)
	for _, task := range tasks {
		return task.(map[string]any)
	}
	t.Fatal("state evidence has no task")
	return nil
}

func TestParseLoopExecutionMode(t *testing.T) {
	valid := map[string]LoopExecutionMode{
		"":                ExecutionModeDryRun,
		"dry-run":         ExecutionModeDryRun,
		"orchestrator":    ExecutionModeOrchestrator,
		"real-agentteams": ExecutionModeRealAgentTeams,
	}
	for input, want := range valid {
		got, err := ParseLoopExecutionMode(input)
		if err != nil || got != want {
			t.Fatalf("ParseLoopExecutionMode(%q)=%q,%v; want %q", input, got, err, want)
		}
	}
	if _, err := ParseLoopExecutionMode("controlled-demo"); err == nil {
		t.Fatal("expected controlled-demo to be rejected")
	}
}
