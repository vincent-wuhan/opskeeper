package runtimemgmt

import (
	"strings"
	"testing"
	"time"
)

func validToolCallRecord() ToolCallRecord {
	return ToolCallRecord{
		ID:               "id-1",
		RuntimeID:        "rt-1",
		TaskID:           "task-1",
		TenantID:         "tenant-1",
		IncidentID:       "inc-1",
		StableToolID:     "opskeeper.host.status",
		ToolClass:        "read",
		ParametersSHA256: strings.Repeat("a", 64),
		StartedAt:        time.Now(),
		Status:           ToolCallStatusDispatched,
		Decision:         ToolCallDecisionAllowed,
	}
}

func TestToolCallRecordValidateAcceptsAllowed(t *testing.T) {
	if err := validToolCallRecord().Validate(); err != nil {
		t.Fatalf("Validate() error = %v, want nil", err)
	}
}

func TestToolCallRecordValidateAcceptsBlocked(t *testing.T) {
	rec := validToolCallRecord()
	rec.Status = ToolCallStatusBlocked
	rec.Decision = ToolCallDecisionBlocked
	rec.DenyReason = "cross_incident"
	if err := rec.Validate(); err != nil {
		t.Fatalf("Validate() error = %v, want nil", err)
	}
}

func TestToolCallRecordValidateAcceptsProposalRequired(t *testing.T) {
	rec := validToolCallRecord()
	rec.Decision = ToolCallDecisionProposalRequired
	rec.ToolClass = "write"
	if err := rec.Validate(); err != nil {
		t.Fatalf("Validate() error = %v, want nil", err)
	}
}

func TestToolCallRecordValidateRejectsInconsistent(t *testing.T) {
	cases := map[string]func(*ToolCallRecord){
		"missing id":     func(r *ToolCallRecord) { r.ID = "" },
		"missing runtime": func(r *ToolCallRecord) { r.RuntimeID = "" },
		"missing task":   func(r *ToolCallRecord) { r.TaskID = "" },
		"missing tenant": func(r *ToolCallRecord) { r.TenantID = "" },
		"missing tool":   func(r *ToolCallRecord) { r.StableToolID = "" },
		"missing class":  func(r *ToolCallRecord) { r.ToolClass = "" },
		"short hash":     func(r *ToolCallRecord) { r.ParametersSHA256 = "abc" },
		"zero started_at": func(r *ToolCallRecord) {
			r.StartedAt = time.Time{}
		},
		"unknown decision":    func(r *ToolCallRecord) { r.Decision = "maybe" },
		"allowed with reason": func(r *ToolCallRecord) { r.DenyReason = "x" },
		"blocked without reason": func(r *ToolCallRecord) {
			r.Decision = ToolCallDecisionBlocked
			r.DenyReason = ""
		},
		"unknown status": func(r *ToolCallRecord) { r.Status = "paused" },
		"blocked status with allowed decision": func(r *ToolCallRecord) {
			r.Status = ToolCallStatusBlocked
		},
		"ended before started": func(r *ToolCallRecord) {
			ended := r.StartedAt.Add(-time.Second)
			r.EndedAt = &ended
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			rec := validToolCallRecord()
			mutate(&rec)
			if err := rec.Validate(); err == nil {
				t.Fatalf("Validate() = nil for %s, want ErrToolCallInvalid", name)
			}
		})
	}
}
