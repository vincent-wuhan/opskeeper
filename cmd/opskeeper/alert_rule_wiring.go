// The alert-rule adapter that used to be a domain.
//
// It lived in core/manager/service/aiopsconfig, a domain of one package and
// one hundred and ten lines whose only caller was this composition root —
// the ledger's own rule (decisions 254 and 257) says a translation belongs at
// the assembly root, because putting one inside a domain teaches that domain
// about the other one. Decision 259 moved it here, and the domain it was
// holding open is gone: 57 domains became 56 and 24 declared edges became 22,
// and neither number moved because anything was deleted from the product.
//
// The vocabulary on both sides is still two parallel copies, and that is
// deliberate: biz/aiops/alertconfig refuses to depend on service-layer DTOs
// (see the note on AlertRulePort), so the copy is the price of that layering
// rule and the price is paid here, in one place, where it can be guarded.
// alert_rule_wiring_test.go is that guard: it proves every field of the
// source side arrives on the other, so a field added to one struct and
// forgotten here is a red test rather than a silently zero value.

package main

import (
	"context"
	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/configchange"

	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/alertconfig"
	managersvcalert "github.com/vincent-wuhan/opskeeper/core/manager/service/alert"
)

type alertRuleService interface {
	PreviewRule(ctx context.Context, caller managersvcalert.Caller, in managersvcalert.RuleInput, lookbackSeconds int) (*managersvcalert.PreviewResult, error)
	CreateRule(ctx context.Context, caller managersvcalert.Caller, in managersvcalert.RuleInput) (*managersvcalert.Rule, error)
}

func newAlertRuleManager(alertSvc alertRuleService) configchange.ConfigManager {
	if alertSvc == nil {
		return alertconfig.NewAlertRuleManager(nil)
	}
	return alertconfig.NewAlertRuleManager(alertRulePort{alert: alertSvc})
}

type alertRulePort struct {
	alert alertRuleService
}

func (p alertRulePort) PreviewRule(ctx context.Context, caller configchange.ConfigCaller, in alertconfig.RuleInput, lookbackSeconds int) (*alertconfig.PreviewResult, error) {
	res, err := p.alert.PreviewRule(ctx, toAlertServiceCaller(caller), toAlertServiceRuleInput(in), lookbackSeconds)
	if err != nil {
		return nil, err
	}
	return fromAlertServicePreview(res), nil
}

func (p alertRulePort) CreateRule(ctx context.Context, caller configchange.ConfigCaller, in alertconfig.RuleInput) (*alertconfig.Rule, error) {
	rule, err := p.alert.CreateRule(ctx, toAlertServiceCaller(caller), toAlertServiceRuleInput(in))
	if err != nil {
		return nil, err
	}
	return &alertconfig.Rule{
		ID:   rule.ID,
		Kind: rule.Kind,
		Name: rule.Name,
	}, nil
}

func toAlertServiceCaller(c configchange.ConfigCaller) managersvcalert.Caller {
	return managersvcalert.Caller{UserID: c.UserID, Role: c.Role}
}

func toAlertServiceRuleInput(in alertconfig.RuleInput) managersvcalert.RuleInput {
	conds := make([]managersvcalert.RuleCondition, 0, len(in.Conditions))
	for _, c := range in.Conditions {
		conds = append(conds, managersvcalert.RuleCondition{
			Metric:     c.Metric,
			Operator:   c.Operator,
			Threshold:  c.Threshold,
			Window:     c.Window,
			For:        c.For,
			Aggregator: c.Aggregator,
		})
	}
	return managersvcalert.RuleInput{
		RuleKey:             in.RuleKey,
		Kind:                in.Kind,
		Name:                in.Name,
		ScopeType:           in.ScopeType,
		JoinMode:            in.JoinMode,
		Severity:            in.Severity,
		Enabled:             in.Enabled,
		Conditions:          conds,
		Spec:                in.Spec,
		Labels:              in.Labels,
		RunbookURL:          in.RunbookURL,
		NotifyChannelIDs:    in.NotifyChannelIDs,
		NotifyWindowSeconds: in.NotifyWindowSeconds,
		NotifyMinFires:      in.NotifyMinFires,
	}
}

func fromAlertServicePreview(in *managersvcalert.PreviewResult) *alertconfig.PreviewResult {
	if in == nil {
		return nil
	}
	out := &alertconfig.PreviewResult{
		FireCount:     in.FireCount,
		FirstFireAt:   in.FirstFireAt,
		LastFireAt:    in.LastFireAt,
		Threshold:     in.Threshold,
		Unit:          in.Unit,
		SkippedReason: in.SkippedReason,
	}
	out.Samples = make([]alertconfig.PreviewSample, 0, len(in.Samples))
	for _, sample := range in.Samples {
		out.Samples = append(out.Samples, alertconfig.PreviewSample{
			Timestamp: sample.Timestamp,
			Labels:    sample.Labels,
			Value:     sample.Value,
			Summary:   sample.Summary,
		})
	}
	out.Series = make([]alertconfig.PreviewSeriesPoint, 0, len(in.Series))
	for _, point := range in.Series {
		out.Series = append(out.Series, alertconfig.PreviewSeriesPoint{
			Timestamp: point.Timestamp,
			Value:     point.Value,
		})
	}
	return out
}
