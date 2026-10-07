package incident

import (
	"testing"
	"time"
)

func TestComputeReportMeasuresFiveJudgeMetrics(t *testing.T) {
	base := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	events := []Event{
		event("alert", base, EventAlertReceived, "", true),
		event("root", base.Add(4*time.Minute), EventRootCause, "", true),
		event("approve", base.Add(6*time.Minute), EventApproved, "", true),
		event("action", base.Add(7*time.Minute), EventAction, "restart", true),
		event("recovery", base.Add(8*time.Minute), EventRecovery, "", true),
		event("close", base.Add(9*time.Minute), EventClosed, "", true),
	}
	events[4].RecoverySignal = true

	report, err := ComputeReport(events)
	if err != nil {
		t.Fatal(err)
	}
	if report.MeanLocalizationSeconds != 240 {
		t.Fatalf("localization seconds = %v, want 240", report.MeanLocalizationSeconds)
	}
	if report.WrongClosureCount != 0 {
		t.Fatalf("wrong closures = %d, want 0", report.WrongClosureCount)
	}
	if report.RepeatedActionCount != 0 {
		t.Fatalf("repeated actions = %d, want 0", report.RepeatedActionCount)
	}
	if report.ApprovedRecommendationCount != 1 || report.RecoveryConfirmedRecommendationCount != 1 {
		t.Fatalf("recommendation counts = %+v", report)
	}
	if report.RecommendationSuccessRate != 1 {
		t.Fatalf("recommendation success rate = %v, want 1", report.RecommendationSuccessRate)
	}
	if report.AuditEvidenceCompleteness != 1 {
		t.Fatalf("audit completeness = %v, want 1", report.AuditEvidenceCompleteness)
	}
}

func TestClosureWithoutNewRecoverySignalIsWrong(t *testing.T) {
	base := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	events := []Event{
		event("alert", base, EventAlertReceived, "", true),
		event("action", base.Add(time.Minute), EventAction, "kill", true),
		event("close", base.Add(2*time.Minute), EventClosed, "", true),
	}
	report, err := ComputeReport(events)
	if err != nil {
		t.Fatal(err)
	}
	if report.WrongClosureCount != 1 {
		t.Fatalf("wrong closures = %d, want 1", report.WrongClosureCount)
	}
	if report.RecommendationSuccessRate != 0 {
		t.Fatalf("empty approval denominator must yield zero, got %v", report.RecommendationSuccessRate)
	}
}

func TestComputeReportCountsRepeatAndIncompleteAudit(t *testing.T) {
	base := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	events := []Event{
		event("alert", base, EventAlertReceived, "", true),
		event("action-1", base.Add(time.Minute), EventAction, "restart", false),
		event("action-2", base.Add(2*time.Minute), EventAction, "restart", true),
	}
	report, err := ComputeReport(events)
	if err != nil {
		t.Fatal(err)
	}
	if report.RepeatedActionCount != 1 {
		t.Fatalf("repeated actions = %d, want 1", report.RepeatedActionCount)
	}
	if report.CompleteAuditEventCount != 2 || report.AuditRequiredEventCount != 3 {
		t.Fatalf("audit counts = complete %d / required %d, want 2 / 3", report.CompleteAuditEventCount, report.AuditRequiredEventCount)
	}
}

func event(id string, at time.Time, eventType string, action string, complete bool) Event {
	item := Event{
		ID: id, TenantID: "tenant", IncidentID: "incident", OccurredAt: at,
		Phase: eventType, EventType: eventType, ActorType: "agent", Actor: "worker",
		Status: "ok", ActionFingerprint: action,
	}
	if complete {
		item.TraceID = "trace-" + id
		item.EvidenceRef = "evidence-" + id
	}
	return item
}
