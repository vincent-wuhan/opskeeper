package loop

import (
	"context"
	"errors"
)

type ResourceRoutingInvestigatorToolset struct {
	postgresql InvestigatorToolset
	generic    InvestigatorToolset
}

func NewResourceRoutingInvestigatorToolset(postgresql, generic InvestigatorToolset) (*ResourceRoutingInvestigatorToolset, error) {
	if postgresql == nil {
		return nil, errors.New("loop: PostgreSQL investigator toolset is required")
	}
	if generic == nil {
		return nil, errors.New("loop: generic investigator toolset is required")
	}
	return &ResourceRoutingInvestigatorToolset{postgresql: postgresql, generic: generic}, nil
}

func (t *ResourceRoutingInvestigatorToolset) Investigate(ctx context.Context, resourceType, alertID string, timeWindow TimeWindow) ([]EvidenceItem, error) {
	return t.delegate(resourceType).Investigate(ctx, resourceType, alertID, timeWindow)
}

func (t *ResourceRoutingInvestigatorToolset) ListRemediations(ctx context.Context, resourceType string) ([]RemediationOption, error) {
	return t.delegate(resourceType).ListRemediations(ctx, resourceType)
}

func (t *ResourceRoutingInvestigatorToolset) ListRemediationsWithEvidence(ctx context.Context, resourceType, alertID string, evidence []EvidenceItem) ([]RemediationOption, error) {
	delegate := t.delegate(resourceType)
	if evidenceAware, ok := delegate.(EvidenceAwareInvestigatorToolset); ok {
		return evidenceAware.ListRemediationsWithEvidence(ctx, resourceType, alertID, evidence)
	}
	return delegate.ListRemediations(ctx, resourceType)
}

func (t *ResourceRoutingInvestigatorToolset) delegate(resourceType string) InvestigatorToolset {
	if resourceType == "pg" || resourceType == "postgres" || resourceType == "postgresql" {
		return t.postgresql
	}
	return t.generic
}
