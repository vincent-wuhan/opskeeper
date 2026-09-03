package main

import (
	"context"
	"fmt"

	managerbizalert "github.com/vincent-wuhan/opskeeper/internal/manager/biz/alert"
	managerbizscheduler "github.com/vincent-wuhan/opskeeper/internal/manager/biz/scheduler"
)

// schedulerAlertSink adapts the scheduler consumer contract to the existing
// alert usecase without introducing a cross-domain import in biz packages.
type schedulerAlertSink struct {
	uc *managerbizalert.Usecase
}

func (s schedulerAlertSink) WriteMissedRunAlert(ctx context.Context, source string, missed []managerbizscheduler.MissedRunInfo) error {
	if s.uc == nil {
		return fmt.Errorf("scheduler alert sink: alert usecase not wired")
	}
	for _, item := range missed {
		if err := s.uc.RecordSchedulerMissedRun(ctx, source, managerbizalert.SchedulerMissedRun{
			FlowID:            item.FlowID,
			NodeID:            item.NodeID,
			CronSpec:          item.CronSpec,
			ExpectedFireAt:    item.ExpectedFireAt,
			MissedDurationSec: item.MissedDurationSec,
		}); err != nil {
			return fmt.Errorf("scheduler alert sink: flow %s node %s: %w", item.FlowID, item.NodeID, err)
		}
	}
	return nil
}
