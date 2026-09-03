package loop

import (
	"context"
	"errors"
	"testing"

	"github.com/vincent-wuhan/opskeeper/internal/pkg/tenantctx"
)

func TestMCPAuthorizer_ServiceIdentityAndToolPolicy(t *testing.T) {
	identity := tenantctx.AgentTeamsIdentity{
		TenantID: "tenant-a", Service: "agentteams", Worker: "opskeeper-investigator", Role: "investigator",
		AllowedTools: []string{"loop.correlate", "loop.investigate", "recovery.execute"},
	}
	claimed := MCPWorkerIdentity{
		TenantID: "tenant-a", Service: "agentteams", Worker: "opskeeper-investigator", Role: "investigator",
	}
	authorizer := NewMCPAuthorizer(nil)

	if err := authorizer.Authorize(context.Background(), tenantctx.Tenant{AgentTeams: &identity}, claimed, "loop.investigate"); err != nil {
		t.Fatalf("authorized tool rejected: %v", err)
	}
	err := authorizer.Authorize(context.Background(), tenantctx.Tenant{AgentTeams: &identity}, claimed, "recovery.execute")
	if !errors.Is(err, ErrMCPMutatingNotAllowed) {
		t.Fatalf("mutating tool error = %v, want %v", err, ErrMCPMutatingNotAllowed)
	}
}

func TestMCPAuthorizer_AllowsHigressSuperRoleInstanceIdentity(t *testing.T) {
	identity := tenantctx.AgentTeamsIdentity{
		TenantID:     "default",
		Service:      "agentteams",
		Worker:       "worker-lumos",
		Role:         "worker",
		AllowedTools: []string{"query_incidents", "query_promql", "recovery.execute"},
	}
	claimed := MCPWorkerIdentity{
		TenantID: "default", Service: "agentteams", Worker: "worker-lumos", Role: "worker",
	}
	authorizer := NewMCPAuthorizer(nil)

	if err := authorizer.Authorize(context.Background(), tenantctx.Tenant{AgentTeams: &identity}, claimed, "query_incidents"); err != nil {
		t.Fatalf("Higress worker identity rejected: %v", err)
	}
	if got := authorizer.FilterToolNames(tenantctx.Tenant{AgentTeams: &identity}, claimed, []string{"query_incidents", "recovery.execute"}); len(got) != 2 {
		t.Fatalf("Higress worker tools = %v, want all signed tools", got)
	}
}

func TestMCPAuthorizer_FilterToolNamesCoversWorkerRoles(t *testing.T) {
	tests := []struct {
		role string
		want []string
	}{
		{role: "alerter", want: []string{"loop.correlate"}},
		{role: "investigator", want: []string{"loop.correlate", "loop.investigate", "query_knowledge"}},
		{role: "critic", want: []string{"query_knowledge"}},
		{role: "reviewer", want: []string{"query_knowledge"}},
		{role: "repairer", want: []string{"recovery.execute", "query_knowledge"}},
		{role: "verifier", want: []string{"recovery.verify", "query_knowledge"}},
		{role: "reporter", want: []string{}},
	}
	all := []string{"loop.correlate", "loop.investigate", "recovery.execute", "recovery.verify", "host_restart_service", "query_knowledge"}
	for _, test := range tests {
		t.Run(test.role, func(t *testing.T) {
			identity := tenantctx.AgentTeamsIdentity{
				TenantID: "tenant-a", Service: "agentteams", Worker: "opskeeper-" + test.role, Role: test.role,
				AllowedTools: all,
			}
			claimed := MCPWorkerIdentity{
				TenantID: identity.TenantID, Service: identity.Service, Worker: identity.Worker, Role: identity.Role,
			}
			got := NewMCPAuthorizer(nil).FilterToolNames(
				tenantctx.Tenant{AgentTeams: &identity}, claimed, all,
			)
			if len(got) != len(test.want) {
				t.Fatalf("role %s tools = %v, want %v", test.role, got, test.want)
			}
			for index := range test.want {
				if got[index] != test.want[index] {
					t.Fatalf("role %s tools = %v, want %v", test.role, got, test.want)
				}
			}
		})
	}
}

func TestMCPAuthorizer_RequiresCompleteSignedIdentity(t *testing.T) {
	identity := tenantctx.AgentTeamsIdentity{TenantID: "tenant-a", Service: "agentteams", Role: "investigator"}
	err := NewMCPAuthorizer(nil).Authorize(
		context.Background(),
		tenantctx.Tenant{AgentTeams: &identity},
		MCPWorkerIdentity{TenantID: "tenant-a", Service: "agentteams", Role: "investigator"},
		"loop.investigate",
	)
	if !errors.Is(err, ErrMCPInvalidIdentityData) {
		t.Fatalf("error = %v, want %v", err, ErrMCPInvalidIdentityData)
	}
}

func TestMCPAuthorizer_RejectsIdentityAndServiceMismatch(t *testing.T) {
	identity := tenantctx.AgentTeamsIdentity{
		TenantID: "tenant-a", Service: "agentteams", Worker: "opskeeper-investigator", Role: "investigator",
		AllowedTools: []string{"loop.investigate"},
	}
	mismatchedTenant := MCPWorkerIdentity{
		TenantID: "tenant-b", Service: "agentteams", Worker: "opskeeper-investigator", Role: "investigator",
	}
	err := NewMCPAuthorizer(nil).Authorize(
		context.Background(), tenantctx.Tenant{AgentTeams: &identity}, mismatchedTenant, "loop.investigate",
	)
	if !errors.Is(err, ErrMCPIdentityMismatch) {
		t.Fatalf("tenant mismatch error = %v, want %v", err, ErrMCPIdentityMismatch)
	}

	identity.Service = "unknown-service"
	claimed := MCPWorkerIdentity{
		TenantID: "tenant-a", Service: "unknown-service", Worker: "opskeeper-investigator", Role: "investigator",
	}
	err = NewMCPAuthorizer(nil).Authorize(
		context.Background(), tenantctx.Tenant{AgentTeams: &identity}, claimed, "loop.investigate",
	)
	if !errors.Is(err, ErrMCPServiceNotAllowed) {
		t.Fatalf("unknown service error = %v, want %v", err, ErrMCPServiceNotAllowed)
	}
}

func TestMCPAuthorizer_RejectsUnboundWorkerRole(t *testing.T) {
	identity := tenantctx.AgentTeamsIdentity{
		TenantID: "tenant-a", Service: "agentteams", Worker: "generic", Role: "repairer",
		AllowedTools: []string{"recovery.execute"},
	}
	err := NewMCPAuthorizer(nil).Authorize(
		context.Background(),
		tenantctx.Tenant{AgentTeams: &identity},
		MCPWorkerIdentity{TenantID: "tenant-a", Service: "agentteams", Worker: "generic", Role: "repairer"},
		"recovery.execute",
	)
	if !errors.Is(err, ErrMCPInvalidIdentityData) {
		t.Fatalf("error = %v, want %v", err, ErrMCPInvalidIdentityData)
	}
}
