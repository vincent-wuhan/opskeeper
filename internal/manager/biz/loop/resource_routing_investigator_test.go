package loop

import (
	"context"
	"errors"
	"testing"
)

type routingInvestigatorToolset struct {
	name        string
	evidence    []EvidenceItem
	remediation []RemediationOption
	err         error
}

func (t routingInvestigatorToolset) Investigate(context.Context, string, string, TimeWindow) ([]EvidenceItem, error) {
	return t.evidence, t.err
}

func (t routingInvestigatorToolset) ListRemediations(context.Context, string) ([]RemediationOption, error) {
	return t.remediation, t.err
}

func TestResourceRoutingInvestigatorRoutesByResourceType(t *testing.T) {
	pgEvidence := []EvidenceItem{{Tool: "pg_stat_activity"}}
	hostEvidence := []EvidenceItem{{Tool: "resource_alert"}}
	pg := routingInvestigatorToolset{name: "pg", evidence: pgEvidence, remediation: []RemediationOption{{Action: "pg.terminate_long_tx"}}}
	generic := routingInvestigatorToolset{name: "generic", evidence: hostEvidence, remediation: []RemediationOption{{Action: "host.garbage_collect"}}}
	toolset, err := NewResourceRoutingInvestigatorToolset(pg, generic)
	if err != nil {
		t.Fatalf("NewResourceRoutingInvestigatorToolset: %v", err)
	}

	tests := []struct {
		resourceType string
		wantEvidence string
		wantAction   string
	}{
		{resourceType: "pg", wantEvidence: "pg_stat_activity", wantAction: "pg.terminate_long_tx"},
		{resourceType: "postgres", wantEvidence: "pg_stat_activity", wantAction: "pg.terminate_long_tx"},
		{resourceType: "postgresql", wantEvidence: "pg_stat_activity", wantAction: "pg.terminate_long_tx"},
		{resourceType: "host", wantEvidence: "resource_alert", wantAction: "host.garbage_collect"},
		{resourceType: "redis", wantEvidence: "resource_alert", wantAction: "host.garbage_collect"},
	}

	for _, test := range tests {
		t.Run(test.resourceType, func(t *testing.T) {
			evidence, err := toolset.Investigate(context.Background(), test.resourceType, "alert-1", TimeWindow{})
			if err != nil {
				t.Fatalf("Investigate: %v", err)
			}
			if len(evidence) != 1 || evidence[0].Tool != test.wantEvidence {
				t.Fatalf("evidence = %+v, want tool %s", evidence, test.wantEvidence)
			}
			remediations, err := toolset.ListRemediations(context.Background(), test.resourceType)
			if err != nil {
				t.Fatalf("ListRemediations: %v", err)
			}
			if len(remediations) != 1 || remediations[0].Action != test.wantAction {
				t.Fatalf("remediations = %+v, want action %s", remediations, test.wantAction)
			}
		})
	}
}

func TestResourceRoutingInvestigatorPropagatesErrors(t *testing.T) {
	toolset, err := NewResourceRoutingInvestigatorToolset(
		routingInvestigatorToolset{err: errors.New("pg unavailable")},
		routingInvestigatorToolset{},
	)
	if err != nil {
		t.Fatalf("NewResourceRoutingInvestigatorToolset: %v", err)
	}
	if _, err := toolset.Investigate(context.Background(), "pg", "alert-1", TimeWindow{}); err == nil {
		t.Fatal("Investigate must propagate postgres toolset error")
	}
	if _, err := toolset.ListRemediations(context.Background(), "postgres"); err == nil {
		t.Fatal("ListRemediations must propagate postgres toolset error")
	}
}

func TestResourceRoutingInvestigatorRequiresToolsets(t *testing.T) {
	if _, err := NewResourceRoutingInvestigatorToolset(nil, routingInvestigatorToolset{}); err == nil {
		t.Fatal("postgres toolset must be required")
	}
	if _, err := NewResourceRoutingInvestigatorToolset(routingInvestigatorToolset{}, nil); err == nil {
		t.Fatal("generic toolset must be required")
	}
}
