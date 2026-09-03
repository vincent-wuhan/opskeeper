package auth

import (
	"testing"
	"time"
)

func TestSignAgentTeamsServiceAllowsKnowledgeLookup(t *testing.T) {
	signer := NewSigner("test-secret", time.Minute, time.Minute)
	tests := []struct {
		role string
	}{
		{role: "investigator"},
		{role: "critic"},
		{role: "reviewer"},
		{role: "repairer"},
		{role: "verifier"},
	}
	for _, test := range tests {
		t.Run(test.role, func(t *testing.T) {
			service := AgentTeamsServiceClaims{
				TenantID: "tenant-a", Service: AgentTeamsServiceName,
				Worker: AgentTeamsWorkerForRole(test.role), Role: test.role,
				AllowedTools: []string{"query_knowledge"},
			}
			if _, err := signer.SignAgentTeamsService(service, time.Minute); err != nil {
				t.Fatalf("SignAgentTeamsService() error = %v", err)
			}
		})
	}
}

func TestSignAgentTeamsServiceRejectsAlerterAndReporterKnowledgeLookup(t *testing.T) {
	signer := NewSigner("test-secret", time.Minute, time.Minute)
	service := AgentTeamsServiceClaims{
		TenantID: "tenant-a", Service: AgentTeamsServiceName,
		Worker: AgentTeamsWorkerForRole("alerter"), Role: "alerter",
		AllowedTools: []string{"query_knowledge"},
	}
	if _, err := signer.SignAgentTeamsService(service, time.Minute); err == nil {
		t.Fatal("SignAgentTeamsService() succeeded for alerter knowledge lookup")
	}

	service.Worker = AgentTeamsWorkerForRole("reporter")
	service.Role = "reporter"
	if _, err := signer.SignAgentTeamsService(service, time.Minute); err == nil {
		t.Fatal("SignAgentTeamsService() succeeded for reporter knowledge lookup")
	}
}

func TestSignAgentTeamsServiceRejectsUnboundWorkerRole(t *testing.T) {
	signer := NewSigner("test-secret", time.Minute, time.Minute)
	service := AgentTeamsServiceClaims{
		TenantID: "tenant-a", Service: AgentTeamsServiceName,
		Worker: "generic", Role: "repairer",
		AllowedTools: []string{"recovery.execute"},
	}
	if _, err := signer.SignAgentTeamsService(service, time.Minute); err == nil {
		t.Fatal("SignAgentTeamsService() succeeded for unbound worker/role identity")
	}
}

func TestAgentTeamsWorkerPermissionsMatrixIsCanonical(t *testing.T) {
	rows := AgentTeamsWorkerPermissions()
	if len(rows) != 7 {
		t.Fatalf("matrix has %d rows, want 7 (alerter/investigator/critic/reviewer/repairer/verifier/reporter)", len(rows))
	}
	wantOrder := []string{"alerter", "investigator", "critic", "reviewer", "repairer", "verifier", "reporter"}
	for i, row := range rows {
		if row.Role != wantOrder[i] {
			t.Errorf("row[%d].Role = %q, want %q (matrix order must be stable for JSON diff)", i, row.Role, wantOrder[i])
		}
		if row.Worker != AgentTeamsWorkerForRole(row.Role) {
			t.Errorf("row[%d].Worker = %q, want %q", i, row.Worker, AgentTeamsWorkerForRole(row.Role))
		}
		if row.Rationale == "" {
			t.Errorf("row[%d].Rationale empty; every role must justify why it has/does not have tools", i)
		}
	}
	// Exactly two narrowly scoped mutating roles.
	mutating := 0
	for _, row := range rows {
		if row.Mutating {
			mutating++
			if row.Role != "repairer" && row.Role != "reporter" {
				t.Errorf("role %q unexpectedly marked mutating", row.Role)
			}
		}
	}
	if mutating != 2 {
		t.Errorf("mutating role count = %d, want 2 (repairer, reporter)", mutating)
	}
	// Alerter remains detection/audit-only. Reporter remains write-only.
	for _, role := range []string{"alerter"} {
		if AgentTeamsRoleAllows(role, "query_knowledge") {
			t.Errorf("role %q allowed query_knowledge; spec says it must be denied", role)
		}
		if AgentTeamsRoleAllows(role, "loop.investigate") {
			t.Errorf("role %q allowed loop.investigate; spec says it must be denied", role)
		}
	}
	if AgentTeamsRoleAllows("reporter", "query_knowledge") {
		t.Error("reporter allowed query_knowledge; spec says it must be denied")
	}
	if !AgentTeamsRoleAllows("reporter", "knowledge.write") {
		t.Error("reporter denied knowledge.write; postmortem persistence requires it")
	}
	for _, tool := range []string{
		"query_promql", "query_incidents", "get_incident_detail", "get_host_load",
		"get_host_processes", "analyze_database_status",
	} {
		if !AgentTeamsRoleAllows("investigator", tool) {
			t.Errorf("investigator denied read-only diagnostic tool %q", tool)
		}
	}
}

func TestIncidentRecordIsRestrictedToStageRoles(t *testing.T) {
	allowed := map[string]string{
		"alerter":      "alert.received",
		"investigator": "root_cause.confirmed",
		"reviewer":     "recommendation.approved",
		"repairer":     "action.executed",
		"verifier":     "recovery_signal.observed",
		"reporter":     "incident.closed",
	}
	for role := range allowed {
		if !AgentTeamsRoleAllows(role, "incident.record") {
			t.Errorf("role %q denied incident.record", role)
		}
	}
	if AgentTeamsRoleAllows("critic", "incident.record") {
		t.Error("critic unexpectedly allowed incident.record")
	}
}
