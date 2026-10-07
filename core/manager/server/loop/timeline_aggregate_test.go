// Package loop — timeline_aggregate_test.go
//
// Day 11 tests for the trace-aggregation helpers. The
// SPA consumed the timeline endpoint with no aggregation before
// Day 11, so the test cases here focus on the new contract:
//
//  1. Empty event slice yields 7 forward phases (all "pending") and
//     a zero-value rubric.
//  2. A fully-walked PG-connection-pool incident produces 7 phases
//     in stable order with the rubric metrics matching the
//     loop-harness-rubric spec.
//  3. The chain footer reports coverage, current/final phase,
//     recovery signal, and closure flags.
//  4. A terminal-state event (failed) appears as a trailing row so
//     the renderer can show "loop closed by failure" instead of
//     hiding the failure inside the last forward phase.
package loop

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	loopbiz "github.com/vincent-wuhan/opskeeper/core/manager/biz/loop"
	loopmodel "github.com/vincent-wuhan/opskeeper/core/manager/model/loop"
)

func TestBuildTimelinePhases_Empty(t *testing.T) {
	t.Parallel()
	phases := BuildTimelinePhases(nil)
	if len(phases) != 7 {
		t.Fatalf("len(phases) = %d, want 7", len(phases))
	}
	for i, p := range phases {
		if p.Phase != forwardPhaseOrder[i] {
			t.Errorf("phase[%d] = %q, want %q", i, p.Phase, forwardPhaseOrder[i])
		}
		if p.Status != "pending" {
			t.Errorf("phase[%d] status = %q, want pending", i, p.Status)
		}
	}
}

func TestBuildTimelinePhases_FullWalk(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	events := fullPGPoolWalk(now)

	phases := BuildTimelinePhases(events)
	if len(phases) != 7 {
		t.Fatalf("len(phases) = %d, want 7", len(phases))
	}

	// Forward phase order is preserved.
	for i, p := range phases {
		if p.Phase != forwardPhaseOrder[i] {
			t.Errorf("phase[%d] = %q, want %q", i, p.Phase, forwardPhaseOrder[i])
		}
	}

	// Every phase has its declared role and skill version.
	wantRoles := []string{
		"opskeeper-alerter",
		"opskeeper-investigator",
		"opskeeper-investigator",
		"opskeeper-critic",
		"opskeeper-reviewer",
		"opskeeper-repairer",
		"opskeeper-postmortem",
	}
	for i, p := range phases {
		if p.WorkerRole != wantRoles[i] {
			t.Errorf("phase[%d] role = %q, want %q", i, p.WorkerRole, wantRoles[i])
		}
		if p.WorkerSkillVer == "" {
			t.Errorf("phase[%d] skill version empty", i)
		}
	}

	// Detected / investigated / recovered / postmortem must end in
	// success and surface a contract summary.
	statusByName := map[string]string{}
	summaryByName := map[string]string{}
	for _, p := range phases {
		statusByName[p.Phase] = p.Status
		summaryByName[p.Phase] = p.ContractSummary
	}
	mustSuccess := []string{
		string(loopbiz.PhaseDetected),
		string(loopbiz.PhaseInvestigated),
		string(loopbiz.PhaseApproved),
		string(loopbiz.PhaseRecovered),
		string(loopbiz.PhasePostmortem),
	}
	for _, name := range mustSuccess {
		if statusByName[name] != "success" {
			t.Errorf("phase %s status = %q, want success", name, statusByName[name])
		}
		if summaryByName[name] == "" {
			t.Errorf("phase %s summary empty", name)
		}
	}

	// The approved phase must surface a bound-target audit row
	// (this is what Element shows for "approval bound to target").
	approved := phases[4]
	if len(approved.Audit) == 0 {
		t.Fatalf("approved phase has no audit rows")
	}
	foundApproval := false
	for _, row := range approved.Audit {
		if row.Kind == "approval" {
			foundApproval = true
			if row.BoundTarget == "" {
				t.Errorf("approval audit row has empty bound_target")
			}
		}
	}
	if !foundApproval {
		t.Errorf("approved phase has no approval audit row")
	}

	// Recovered phase must surface an execution audit row.
	recovered := phases[5]
	foundExecution := false
	for _, row := range recovered.Audit {
		if row.Kind == "execution" {
			foundExecution = true
		}
	}
	if !foundExecution {
		t.Errorf("recovered phase has no execution audit row")
	}
	// ... and the tool it actually ran. The orchestrator writes
	// tool_replay onto the success-path contract event; if the aggregate
	// only mined phase_failed, a clean recovery would show no tool call
	// and read as "nothing was executed".
	foundReplay := false
	for _, tc := range recovered.ToolCalls {
		if tc.Name == "pg.resize_pool" {
			foundReplay = true
			if tc.Status != "success" {
				t.Errorf("replayed tool status = %q, want success", tc.Status)
			}
			if !strings.Contains(tc.Args, "120") {
				t.Errorf("replayed tool args = %q, want the executed arguments", tc.Args)
			}
		}
	}
	if !foundReplay {
		t.Errorf("recovered phase has no pg.resize_pool tool call; the successful repair would read as 'ran nothing'")
	}

	// Postmortem phase must surface a close audit row.
	postmortem := phases[6]
	foundClose := false
	for _, row := range postmortem.Audit {
		if row.Kind == "close" {
			foundClose = true
		}
	}
	if !foundClose {
		t.Errorf("postmortem phase has no close audit row")
	}
}

func TestBuildRubric_FullWalk(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	events := fullPGPoolWalk(now)

	rubric := BuildRubric(events)
	if rubric.EventCount != len(events) {
		t.Errorf("rubric.EventCount = %d, want %d", rubric.EventCount, len(events))
	}
	if rubric.RCAAccuracy < 0.99 {
		t.Errorf("rubric.RCAAccuracy = %f, want ~1.0", rubric.RCAAccuracy)
	}
	if rubric.TimeToRemediate == "—" {
		t.Errorf("rubric.TimeToRemediate = —, want a duration")
	}
	if rubric.ApprovalRate < 0.99 {
		t.Errorf("rubric.ApprovalRate = %f, want ~1.0", rubric.ApprovalRate)
	}
	if rubric.RecoveryPassRate < 0.99 {
		t.Errorf("rubric.RecoveryPassRate = %f, want ~1.0", rubric.RecoveryPassRate)
	}
	if !rubric.HasRecoverySignal {
		t.Errorf("rubric.HasRecoverySignal = false, want true")
	}
	if !rubric.HasClosure {
		t.Errorf("rubric.HasClosure = false, want true")
	}
}

func TestBuildChainMeta_FullWalk(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	events := fullPGPoolWalk(now)

	chain := BuildChainMeta(events, "INC-PG-POOL-001")
	if chain.PhasesExpected != 7 {
		t.Errorf("chain.PhasesExpected = %d, want 7", chain.PhasesExpected)
	}
	if chain.PhasesObserved < 5 {
		t.Errorf("chain.PhasesObserved = %d, want >= 5", chain.PhasesObserved)
	}
	if chain.CoveragePct < 0.7 {
		t.Errorf("chain.CoveragePct = %f, want >= 0.7", chain.CoveragePct)
	}
	if chain.FinalPhase != string(loopbiz.PhasePostmortem) {
		t.Errorf("chain.FinalPhase = %q, want %q", chain.FinalPhase, loopbiz.PhasePostmortem)
	}
	if !chain.RecoverySignal {
		t.Errorf("chain.RecoverySignal = false, want true")
	}
	if !chain.Closed {
		t.Errorf("chain.Closed = false, want true")
	}
	if chain.IncidentIDAlias != "INC-PG-POOL-001" {
		t.Errorf("chain.IncidentIDAlias = %q, want INC-PG-POOL-001", chain.IncidentIDAlias)
	}
}

func TestBuildTimelinePhases_TerminalFailure(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	events := []loopmodel.Event{
		{
			ID:             1,
			IncidentID:     "INC-X",
			Phase:          "detected",
			EventType:      loopmodel.EventTypePhaseEntered,
			CreatedAt:      now,
			IdempotencyKey: "detected:entered:1",
		},
		{
			ID:             2,
			IncidentID:     "INC-X",
			Phase:          "detected",
			EventType:      loopmodel.EventPhaseContractWritten,
			CreatedAt:      now.Add(10 * time.Second),
			Payload:        `{"summary":"alert dedup confirmed"}`,
			IdempotencyKey: "detected:contract:1",
		},
		{
			ID:             3,
			IncidentID:     "INC-X",
			Phase:          "failed",
			EventType:      loopmodel.EventPhaseFailed,
			CreatedAt:      now.Add(2 * time.Minute),
			Payload:        `{"error":"upstream MCP gateway down"}`,
			IdempotencyKey: "failed:phase_failed:1",
		},
	}

	phases := BuildTimelinePhases(events)
	last := phases[len(phases)-1]
	if last.Phase != "failed" {
		t.Errorf("last phase = %q, want failed", last.Phase)
	}
	if last.Status != "failed" {
		t.Errorf("last phase status = %q, want failed", last.Status)
	}
	if last.ContractSummary == "" {
		t.Errorf("last phase summary empty, want terminal-state message")
	}
}

// fullPGPoolWalk builds a synthetic but realistic event log for the
// PG connection-pool-exhaustion main scenario. Each event encodes
// the audit_kind + bound_target / execution identity that the
// Day 11 helpers need to surface in the timeline view.
func fullPGPoolWalk(start time.Time) []loopmodel.Event {
	return []loopmodel.Event{
		// detected (alerter)
		{
			ID: 1, IncidentID: "INC-PG-POOL-001", Phase: "detected",
			EventType: loopmodel.EventTypePhaseEntered, CreatedAt: start,
			IdempotencyKey: "detected:entered:1",
			TraceID:        "trace-inc-pg-pool-001-alerter",
		},
		{
			ID: 2, IncidentID: "INC-PG-POOL-001", Phase: "detected",
			EventType: loopmodel.EventPhaseContractWritten, CreatedAt: start.Add(8 * time.Second),
			Payload:        `{"summary":"alert dedup: 71 waiters, pg connections 116/120","audit_kind":"dispatch","actor":"opskeeper-alerter","actor_role":"opskeeper-alerter","bound_target":"incident:INC-PG-POOL-001","evidence_ref":"evidence/incidents/INC-PG-POOL-001/alert.json"}`,
			IdempotencyKey: "detected:contract:1",
			TraceID:        "trace-inc-pg-pool-001-alerter",
		},
		// correlated (investigator)
		{
			ID: 3, IncidentID: "INC-PG-POOL-001", Phase: "correlated",
			EventType: loopmodel.EventTypePhaseEntered, CreatedAt: start.Add(15 * time.Second),
			IdempotencyKey: "correlated:entered:1",
			TraceID:        "trace-inc-pg-pool-001-investigator",
		},
		{
			ID: 4, IncidentID: "INC-PG-POOL-001", Phase: "correlated",
			EventType: loopmodel.EventPhaseContractWritten, CreatedAt: start.Add(45 * time.Second),
			Payload:        `{"tool":"postgres.analyze_status","args":{"incident_id":"INC-PG-POOL-001"},"result":{"active":116,"waiters":71,"max_connections":120},"audit_kind":"dispatch","actor":"opskeeper-investigator","actor_role":"opskeeper-investigator","evidence_ref":"evidence/incidents/INC-PG-POOL-001/pg-stat-activity.json","knowledge_ref":"knowledge/pg-connection-pool-tuning"}`,
			IdempotencyKey: "correlated:contract:1",
			TraceID:        "trace-inc-pg-pool-001-investigator",
		},
		// investigated
		{
			ID: 5, IncidentID: "INC-PG-POOL-001", Phase: "investigated",
			EventType: loopmodel.EventTypePhaseEntered, CreatedAt: start.Add(time.Minute),
			IdempotencyKey: "investigated:entered:1",
			TraceID:        "trace-inc-pg-pool-001-investigator",
		},
		{
			ID: 6, IncidentID: "INC-PG-POOL-001", Phase: "investigated",
			EventType: loopmodel.EventPhaseContractWritten, CreatedAt: start.Add(2 * time.Minute),
			Payload:        `{"schema_version":"v1","root_cause_object":{"kind":"pool_capacity_exhausted","summary":"Application pool 90/90 saturated; pg 116/120; 2 idle-in-transaction workers holding budget"},"confidence":0.92,"remediation_options":[{"action":"resize_pool","target":"pg:pool-fixture","params":{"from":90,"to":120}}],"audit_kind":"dispatch","actor":"opskeeper-investigator","actor_role":"opskeeper-investigator","evidence_ref":"evidence/incidents/INC-PG-POOL-001/diagnosis.json"}`,
			IdempotencyKey: "investigated:contract:1",
			TraceID:        "trace-inc-pg-pool-001-investigator",
		},
		// critiqued
		{
			ID: 7, IncidentID: "INC-PG-POOL-001", Phase: "critiqued",
			EventType: loopmodel.EventTypePhaseEntered, CreatedAt: start.Add(2*time.Minute + 10*time.Second),
			IdempotencyKey: "critiqued:entered:1",
		},
		{
			ID: 8, IncidentID: "INC-PG-POOL-001", Phase: "critiqued",
			EventType: loopmodel.EventPhaseContractWritten, CreatedAt: start.Add(3 * time.Minute),
			Payload:        `{"replay":"success","depth":"success","consistency":"success","score":0.95,"audit_kind":"verification","actor":"opskeeper-critic","actor_role":"opskeeper-critic","note":"evidence chain covers capacity, waiters, probe failure, db counter-evidence"}`,
			IdempotencyKey: "critiqued:contract:1",
		},
		// approved (HITL-bound)
		{
			ID: 9, IncidentID: "INC-PG-POOL-001", Phase: "approved",
			EventType: loopmodel.EventTypePhaseEntered, CreatedAt: start.Add(3*time.Minute + 5*time.Second),
			IdempotencyKey: "approved:entered:1",
		},
		{
			ID: 10, IncidentID: "INC-PG-POOL-001", Phase: "approved",
			EventType: loopmodel.EventPhaseContractWritten, CreatedAt: start.Add(4 * time.Minute),
			Payload:        `{"decision":"approve","approver":"dba-oncall","signed_by":"opskeeper-observer","audit_kind":"approval","actor":"opskeeper-reviewer","actor_role":"opskeeper-reviewer","bound_target":"pg:pool-fixture","bound_params":"from=90 to=120","bound_scope":"pool_capacity_exhausted/INC-PG-POOL-001","fallback":"none","fallback_cause":"","evidence_ref":"evidence/incidents/INC-PG-POOL-001/approval.json"}`,
			IdempotencyKey: "approved:contract:1",
		},
		// recovered
		{
			ID: 11, IncidentID: "INC-PG-POOL-001", Phase: "recovered",
			EventType: loopmodel.EventTypePhaseEntered, CreatedAt: start.Add(4*time.Minute + 5*time.Second),
			IdempotencyKey: "recovered:entered:1",
		},
		{
			ID: 12, IncidentID: "INC-PG-POOL-001", Phase: "recovered",
			EventType: loopmodel.EventPhaseContractWritten, CreatedAt: start.Add(5 * time.Minute),
			Payload:        `{"schema_version":"v1","passed":true,"deltas":{"app_pool_waiters":-0.96,"app_pool_wait_latency_p95":-0.94,"pg_connections_active":-0.05},"recovery_signal":true,"sample_size":10,"tolerance":0.15,"audit_kind":"execution","actor":"opskeeper-repairer","actor_role":"opskeeper-repairer","action":"resize_pool","bound_target":"pg:pool-fixture","bound_params":"from=90 to=120","evidence_ref":"evidence/incidents/INC-PG-POOL-001/recovery-check.json","tool_replay":[{"Name":"pg.resize_pool","ArgsJSON":"{\"from\":90,\"to\":120}","ResultJSON":"{\"pool_size\":120}","Status":"success","LatencyMs":42}]}`,
			IdempotencyKey: "recovered:contract:1",
		},
		// postmortem
		{
			ID: 13, IncidentID: "INC-PG-POOL-001", Phase: "postmortem",
			EventType: loopmodel.EventTypePhaseEntered, CreatedAt: start.Add(5*time.Minute + 5*time.Second),
			IdempotencyKey: "postmortem:entered:1",
		},
		{
			ID: 14, IncidentID: "INC-PG-POOL-001", Phase: "postmortem",
			EventType: loopmodel.EventPhaseContractWritten, CreatedAt: start.Add(6 * time.Minute),
			Payload:        `{"summary":"application pool 90/90 saturated; pg 116/120; resize + recycle_idle restored service in ~5m","recommendation":"set pool auto-resize 1.25x","audit_kind":"close","actor":"opskeeper-postmortem","actor_role":"opskeeper-postmortem","evidence_ref":"evidence/incidents/INC-PG-POOL-001/postmortem.md"}`,
			IdempotencyKey: "postmortem:contract:1",
		},
	}
}

// TestTimelineResponse_PhasesFieldIsPopulated covers the wire-format
// requirement: the timeline handler must return a non-empty phases
// slice + non-nil rubric for any non-empty event set. This is the
// regression test for the aggregation gap (the old handler returned
// only the raw events).
func TestTimelineResponse_PhasesFieldIsPopulated(t *testing.T) {
	t.Parallel()
	h, _, events, _ := newTestHandler(t)
	events.events = fullPGPoolWalk(time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC))

	r := newRequest(http.MethodGet, "/v1/loops/INC-PG-POOL-001/timeline", nil)
	r = r.WithContext(adminCtx(r))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("incident_id", "INC-PG-POOL-001")
	r = r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))

	w := httptest.NewRecorder()
	h.timeline(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	var resp TimelineResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode err: %v", err)
	}
	if len(resp.Phases) != 7 {
		t.Errorf("len(resp.Phases) = %d, want 7", len(resp.Phases))
	}
	if resp.Rubric == nil {
		t.Errorf("resp.Rubric = nil, want populated")
	}
	if resp.Chain.PhasesExpected != 7 {
		t.Errorf("resp.Chain.PhasesExpected = %d, want 7", resp.Chain.PhasesExpected)
	}
}

// TestBuildRubric_RecoveryPassRateWeightedBySampleSize covers
// P0-2 from the 337 review: the rubric.RecoveryPassRate must
// reflect a 60s multi-sample observation window, not collapse each
// contract into a single +/-1 boolean. The PG-pool fixture carries
// sample_size=10; the synthetic below swaps it for sample_size=60
// and adds a second failure (sample_size=20) so the metric must
// reach 60/(60+20) = 0.75 instead of the 1/2 = 0.5 you'd get from
// counting contracts only.
func TestBuildRubric_RecoveryPassRateWeightedBySampleSize(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	events := []loopmodel.Event{
		{ID: 1, Phase: "recovered", EventType: loopmodel.EventPhaseContractWritten,
			CreatedAt: now,
			Payload:   `{"schema_version":"v1","passed":true,"recovery_signal":true,"sample_size":60,"tolerance":0.15}`},
		{ID: 2, Phase: "recovered", EventType: loopmodel.EventPhaseContractWritten,
			CreatedAt: now.Add(time.Minute),
			Payload:   `{"schema_version":"v1","passed":false,"recovery_signal":false,"sample_size":20,"tolerance":0.15}`},
	}
	rubric := BuildRubric(events)
	want := 60.0 / 80.0
	if rubric.RecoveryPassRate < want-0.01 || rubric.RecoveryPassRate > want+0.01 {
		t.Errorf("RecoveryPassRate = %f, want ~%f", rubric.RecoveryPassRate, want)
	}
}

// TestBuildRubric_RecoveryPassRateFallsBackToOneWhenSampleMissing
// covers the backwards-compat path: a recovered contract that does
// not carry sample_size must still count as one attempt, otherwise
// historical event logs (pre-D11) would render RecoveryPassRate=0
// and trip a false-negative in the rubric.
func TestBuildRubric_RecoveryPassRateFallsBackToOneWhenSampleMissing(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	events := []loopmodel.Event{
		{ID: 1, Phase: "recovered", EventType: loopmodel.EventPhaseContractWritten,
			CreatedAt: now,
			Payload:   `{"schema_version":"v1","passed":true,"recovery_signal":true,"tolerance":0.15}`},
	}
	rubric := BuildRubric(events)
	if rubric.RecoveryPassRate < 0.99 {
		t.Errorf("RecoveryPassRate = %f, want ~1.0 when no sample_size", rubric.RecoveryPassRate)
	}
}

// TestBuildChainMeta_TerminalStateOverridesClosed covers P0-3
// from the 337 review: a terminal-state event (failed / aborted)
// must override the postmortem-driven Closed=true signal so the
// timeline never advertises a closed-by-failure loop as a healthy
// close. The FinalPhase / CurrentPhase must also flip to the
// terminal name.
func TestBuildChainMeta_TerminalStateOverridesClosed(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	events := []loopmodel.Event{
		{ID: 1, Phase: "detected", EventType: loopmodel.EventTypePhaseEntered, CreatedAt: now},
		{ID: 2, Phase: "postmortem", EventType: loopmodel.EventPhaseContractWritten, CreatedAt: now.Add(time.Minute)},
		// A failed event arrives AFTER the postmortem (race /
		// late retry). The terminal state must still win.
		{ID: 3, Phase: "failed", EventType: loopmodel.EventPhaseFailed, CreatedAt: now.Add(2 * time.Minute)},
	}
	chain := BuildChainMeta(events, "INC-X")
	if chain.Closed {
		t.Errorf("chain.Closed = true, want false (terminal state wins)")
	}
	if chain.FinalPhase != "failed" {
		t.Errorf("chain.FinalPhase = %q, want failed", chain.FinalPhase)
	}
	if chain.CurrentPhase != "failed" {
		t.Errorf("chain.CurrentPhase = %q, want failed", chain.CurrentPhase)
	}
}

// TestBuildChainMeta_ClosedOnlyWhenPostmortemClean covers the
// positive path: a fully-walked loop without any terminal-state
// event must keep Closed=true (no false negatives from the new
// terminal-priority logic).
func TestBuildChainMeta_ClosedOnlyWhenPostmortemClean(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	events := []loopmodel.Event{
		{ID: 1, Phase: "detected", EventType: loopmodel.EventTypePhaseEntered, CreatedAt: now},
		{ID: 2, Phase: "postmortem", EventType: loopmodel.EventPhaseContractWritten, CreatedAt: now.Add(time.Minute)},
	}
	chain := BuildChainMeta(events, "INC-Y")
	if !chain.Closed {
		t.Errorf("chain.Closed = false, want true (clean postmortem)")
	}
	if chain.FinalPhase != "postmortem" {
		t.Errorf("chain.FinalPhase = %q, want postmortem", chain.FinalPhase)
	}
}

// TestParseAuditRow_BoundTargetRejectsOffWhitelist covers P0-4
// from the 337 review: an approval audit row whose bound_target
// does not match the approved prefix set must be flagged as
// REJECTED in the rendered row, and the FallbackCause slot must
// carry the reason. The original target stays visible for audit
// replay.
func TestParseAuditRow_BoundTargetRejectsOffWhitelist(t *testing.T) {
	t.Parallel()
	ev := loopmodel.Event{
		Phase:     string(loopbiz.PhaseApproved),
		EventType: loopmodel.EventPhaseContractWritten,
		Payload:   `{"decision":"approve","audit_kind":"approval","actor":"opskeeper-reviewer","actor_role":"opskeeper-reviewer","bound_target":"unknown-host:secret","fallback":"none","fallback_cause":""}`,
	}
	got, ok := parseAuditRow(ev.Payload, ev.Phase, ev)
	if !ok {
		t.Fatalf("parseAuditRow returned ok=false, want true")
	}
	if !strings.Contains(got.BoundTarget, "REJECTED:wrong_target") {
		t.Errorf("BoundTarget = %q, want substring REJECTED:wrong_target", got.BoundTarget)
	}
	if !strings.Contains(got.BoundTarget, "unknown-host:secret") {
		t.Errorf("BoundTarget must preserve original target for audit replay; got %q", got.BoundTarget)
	}
	if !strings.Contains(got.FallbackCause, "bound_target_rejected:wrong_target") {
		t.Errorf("FallbackCause = %q, want substring bound_target_rejected:wrong_target", got.FallbackCause)
	}
}

// TestParseAuditRow_BoundTargetRejectsEmpty covers the missing
// target path: an approval row with no bound_target must be
// flagged as REJECTED:missing so the reviewer can see the binding
// is incomplete.
func TestParseAuditRow_BoundTargetRejectsEmpty(t *testing.T) {
	t.Parallel()
	ev := loopmodel.Event{
		Phase:     string(loopbiz.PhaseApproved),
		EventType: loopmodel.EventPhaseContractWritten,
		Payload:   `{"decision":"approve","audit_kind":"approval","actor":"opskeeper-reviewer","actor_role":"opskeeper-reviewer","bound_target":"","fallback":"none","fallback_cause":""}`,
	}
	got, ok := parseAuditRow(ev.Payload, ev.Phase, ev)
	if !ok {
		t.Fatalf("parseAuditRow returned ok=false, want true")
	}
	if !strings.Contains(got.BoundTarget, "REJECTED:missing") {
		t.Errorf("BoundTarget = %q, want substring REJECTED:missing", got.BoundTarget)
	}
	if !strings.Contains(got.FallbackCause, "bound_target_rejected:missing") {
		t.Errorf("FallbackCause = %q, want substring bound_target_rejected:missing", got.FallbackCause)
	}
}

// TestParseAuditRow_BoundTargetAllowsWhitelistedPrefix covers the
// happy path: a target that matches one of the approved prefixes
// passes through unmodified and the fallback_cause slot is left
// untouched. This is the regression test that protects the
// existing PG-pool / incident / opskeeper targets from the new
// reject logic.
func TestParseAuditRow_BoundTargetAllowsWhitelistedPrefix(t *testing.T) {
	t.Parallel()
	for _, target := range []string{
		"pg:pool-fixture",
		"incident:INC-1",
		"opskeeper:audit",
		"app:checkout",
		"host:node-7",
	} {
		ev := loopmodel.Event{
			Phase:     string(loopbiz.PhaseApproved),
			EventType: loopmodel.EventPhaseContractWritten,
			Payload:   `{"decision":"approve","audit_kind":"approval","actor":"opskeeper-reviewer","actor_role":"opskeeper-reviewer","bound_target":"` + target + `","fallback":"none","fallback_cause":""}`,
		}
		got, ok := parseAuditRow(ev.Payload, ev.Phase, ev)
		if !ok {
			t.Fatalf("parseAuditRow returned ok=false for %s", target)
		}
		if strings.Contains(got.BoundTarget, "REJECTED") {
			t.Errorf("target %q should pass whitelist but BoundTarget = %q", target, got.BoundTarget)
		}
		if got.BoundTarget != target {
			t.Errorf("BoundTarget = %q, want %q", got.BoundTarget, target)
		}
	}
}

// TestParseAuditRow_FallbackWithoutCause covers P1 from the 337
// review: when fallback is set but fallback_cause is empty (or
// vice versa), the parser must surface the inconsistency with an
// explicit "<unset>:<reason>" marker instead of letting the row
// look well-formed.
func TestParseAuditRow_FallbackWithoutCause(t *testing.T) {
	t.Parallel()
	ev := loopmodel.Event{
		Phase:     string(loopbiz.PhaseApproved),
		EventType: loopmodel.EventPhaseContractWritten,
		Payload:   `{"decision":"approve","audit_kind":"approval","actor":"opskeeper-reviewer","actor_role":"opskeeper-reviewer","bound_target":"pg:pool-fixture","fallback":"manual_review","fallback_cause":""}`,
	}
	got, ok := parseAuditRow(ev.Payload, ev.Phase, ev)
	if !ok {
		t.Fatalf("parseAuditRow returned ok=false, want true")
	}
	if !strings.Contains(got.FallbackCause, "<unset>:fallback_without_cause") {
		t.Errorf("FallbackCause = %q, want substring <unset>:fallback_without_cause", got.FallbackCause)
	}
}

// TestParseToolReplay_ReadsTheSuccessPathEntries: the payload the
// orchestrator writes on phase_contract_written must yield a tool call.
// This is the read half of the pairing — the orchestrator's write half is
// covered in biz/loop.
func TestParseToolReplay_ReadsTheSuccessPathEntries(t *testing.T) {
	t.Parallel()
	raw := `{"tool_replay":[{"Name":"host.restart_service","ArgsJSON":"{\"unit\":\"nginx.service\"}","ResultJSON":"{}","Argv":["systemctl","restart","nginx.service"],"Status":"success","LatencyMs":17}]}`
	got := parseToolReplay(raw, "opskeeper-repairer")
	if len(got) != 1 {
		t.Fatalf("parseToolReplay = %+v, want one entry", got)
	}
	if got[0].Name != "host.restart_service" {
		t.Errorf("name = %q, want host.restart_service", got[0].Name)
	}
	if got[0].Actor != "opskeeper-repairer" {
		t.Errorf("actor = %q, want the phase role", got[0].Actor)
	}
	if got[0].LatencyMs != 17 {
		t.Errorf("latency = %d, want 17", got[0].LatencyMs)
	}
	// The executed vector must survive into the timeline: it is what a
	// reviewer reads and what the crystalliser promotes, and dropping it
	// here would leave the two of them guessing from ArgsJSON.
	if strings.Join(got[0].Argv, " ") != "systemctl restart nginx.service" {
		t.Errorf("argv = %v, want the executed vector surfaced, not dropped", got[0].Argv)
	}
}

// TestParseToolReplay_AbsentOrMalformedIsNoCall: a phase that ran nothing
// (or whose payload predates the field) must not synthesise a tool call.
// A zero entry here would be indistinguishable from a real invocation and
// would put "ran something" on every phase.
func TestParseToolReplay_AbsentOrMalformedIsNoCall(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"", "{}", `{"summary":"no replay here"}`, `{"tool_replay":"not-an-array"}`, `[1,2,3]`} {
		if got := parseToolReplay(raw, "opskeeper-repairer"); got != nil {
			t.Errorf("parseToolReplay(%q) = %+v, want nil", raw, got)
		}
	}
}
