package main

// The fakes behind the roadmap delivery check.
//
// They exist for one reason and it is worth writing down: a check that asks
// "is this tool in the bag a deployment serves" is only worth anything if
// the bag it inspects is built the way the binary builds it. The binary
// passes a tunnel client, an edge catalog, three query clients and an alert
// usecase into the registry constructor, and every tool gated on one of them
// is absent when those are nil. A test that passed nils would therefore be
// checking a bag holding a seventh of the tools — and the seventh is the
// part that needs no wiring, which is exactly the part that cannot rot.
//
// So these are fakes, not nils, and each one is deliberately the smallest
// thing that satisfies the port. None of them is ever called: the check asks
// whether a tool is *registered*, and a tool that is registered is one the
// model is offered. What happens when the model calls it is a different
// question with a different set of tests behind it.

import (
	"context"
	"time"

	auditport "github.com/vincent-wuhan/opskeeper/core/base/pkg/audit"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/logquery"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/promquery"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/tracequery"
	"github.com/vincent-wuhan/opskeeper/core/domain"
	managerbizaiopstoolsconfigchange "github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/configchange"
	alertbiz "github.com/vincent-wuhan/opskeeper/core/manager/biz/alert"
	alertmodel "github.com/vincent-wuhan/opskeeper/core/manager/model/alert"
)

type stubCaller struct{}

func (stubCaller) Call(context.Context, uint64, string, []byte) ([]byte, error) {
	return []byte("{}"), nil
}

type stubEdgeCatalog struct{}

func (stubEdgeCatalog) ListCatalog(context.Context, domain.EdgeFilter) ([]domain.EdgePresence, error) {
	return nil, nil
}
func (stubEdgeCatalog) Presence(context.Context, uint64) (domain.EdgePresence, bool, error) {
	return domain.EdgePresence{}, false, nil
}
func (stubEdgeCatalog) PresenceByName(context.Context, string) (domain.EdgePresence, bool, error) {
	return domain.EdgePresence{}, false, nil
}
func (stubEdgeCatalog) PluginHealth(uint64) []domain.PluginHealth { return nil }

type stubPromQuerier struct{}

func (stubPromQuerier) QueryRange(context.Context, string, time.Time, time.Time, time.Duration) (*promquery.InstantResult, error) {
	return nil, nil
}
func (stubPromQuerier) Query(context.Context, string, time.Time) (*promquery.InstantResult, error) {
	return nil, nil
}

type stubLogQuerier struct{}

func (stubLogQuerier) QueryRange(context.Context, logquery.QueryRangeOptions) (*logquery.QueryRangeResult, error) {
	return nil, nil
}

type stubTraceQuerier struct{}

func (stubTraceQuerier) SearchTraces(context.Context, tracequery.SearchOptions) (*tracequery.SearchResult, error) {
	return nil, nil
}

type stubAlertUsecase struct{}

func (stubAlertUsecase) GetIncident(context.Context, uint64) (*alertmodel.Incident, error) {
	return nil, nil
}
func (stubAlertUsecase) ListIncidents(context.Context, alertbiz.IncidentFilter) ([]*alertmodel.Incident, error) {
	return nil, nil
}
func (stubAlertUsecase) ListEvents(context.Context, uint64, int) ([]*alertmodel.Event, error) {
	return nil, nil
}
func (stubAlertUsecase) ListRules(context.Context, string) ([]*alertmodel.Rule, error) {
	return nil, nil
}

// stubAuditLister and stubConfigManager exist because of A.2 and D.4.
//
// Both tools gate on a setter that lives in the assembly root rather than in
// the constructor, so leaving them nil here would drop query_change_events
// and apply_config_change from the bag — and then this check would report,
// honestly but uselessly, that the product does not serve two capabilities
// it does serve. A gate that fails for the gate's own reason teaches nobody
// anything.
type stubAuditLister struct{}

func (stubAuditLister) ListChanges(context.Context, time.Time, time.Time, string, string, int) ([]auditport.ChangeRow, error) {
	return nil, nil
}

type stubEdgeChangeLister struct{}

func (stubEdgeChangeLister) ListChangeWindow(context.Context, time.Time, time.Time, string, int) ([]domain.ChangeEvent, error) {
	return nil, nil
}

type stubConfigManager struct{}

func (stubConfigManager) DraftAlertRuleConfig(context.Context, managerbizaiopstoolsconfigchange.ConfigCaller, managerbizaiopstoolsconfigchange.AlertRuleConfigArgs) (*managerbizaiopstoolsconfigchange.ConfigDraft, error) {
	return nil, nil
}
func (stubConfigManager) ApplyAlertRuleConfig(context.Context, managerbizaiopstoolsconfigchange.ConfigCaller, managerbizaiopstoolsconfigchange.AlertRuleApplyArgs) (*managerbizaiopstoolsconfigchange.ConfigApplyResult, error) {
	return nil, nil
}
