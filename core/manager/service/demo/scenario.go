package demo

import (
	"context"
	"encoding/json"
	"errors"

	bizdemo "github.com/vincent-wuhan/opskeeper/core/manager/biz/demo"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/errs"
)

type StartScenarioInput = bizdemo.StartScenarioInput
type ScenarioStatus = bizdemo.ScenarioStatus
type PreviewDecisionSummary = bizdemo.PreviewDecisionSummary
type ApproveScenarioInput = bizdemo.ApproveScenarioInput

type Service struct{ usecase *bizdemo.Usecase }

func NewService(usecase *bizdemo.Usecase) *Service { return &Service{usecase: usecase} }

func (s *Service) Start(ctx context.Context, tenantID uint64, input StartScenarioInput) (*ScenarioStatus, error) {
	return s.usecase.Start(ctx, tenantID, input)
}

func (s *Service) Get(ctx context.Context, tenantID uint64, scenarioID, key string) (*ScenarioStatus, error) {
	return s.usecase.Get(ctx, tenantID, scenarioID, key)
}

func (s *Service) AdvanceWorkflow(
	ctx context.Context, tenantID uint64, scenarioID, key, stage string,
) (*ScenarioStatus, error) {
	return s.usecase.AdvanceWorkflow(ctx, tenantID, scenarioID, key, stage)
}

func (s *Service) Approve(ctx context.Context, tenantID, incidentID uint64, input ApproveScenarioInput) (*ScenarioStatus, error) {
	return s.usecase.Approve(ctx, tenantID, incidentID, input.ApproverID)
}

func (s *Service) BusinessSnapshot(ctx context.Context, tenantID uint64, scenarioID, key, section string) (json.RawMessage, error) {
	return s.usecase.BusinessSnapshot(ctx, tenantID, scenarioID, key, section)
}

func (s *Service) BusinessSnapshotBaseline(ctx context.Context, section string) (json.RawMessage, error) {
	return s.usecase.BusinessSnapshotBaseline(ctx, section)
}

func MapError(err error) (int, string, string) {
	var fixtureErr *bizdemo.FixtureError
	if errors.As(err, &fixtureErr) {
		if fixtureErr.Code == "pool_exhausted" {
			return 503, "business query unavailable", "pool_exhausted"
		}
		return 503, "demo fixture unavailable", "demo_fixture_unavailable"
	}
	status := errs.HTTPStatus(err)
	switch status {
	case 400:
		return status, "invalid scenario request", "invalid_request"
	case 401:
		return status, "unauthorized", "unauthorized"
	case 403, 409:
		return status, "scenario conflicts with existing run", "scenario_conflict"
	case 404:
		return status, "scenario not found", "not_found"
	case 501:
		return status, "scenario not ready", "not_ready"
	default:
		return 500, "scenario operation failed", "scenario_failed"
	}
}
