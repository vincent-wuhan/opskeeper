// Package scheduler 实现 missed-run 检测 + 周期巡检。
//
// 路径 A P1-1 阶段 1 任务 1.3 — missed-run-detector 模块。
package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/vincent-wuhan/opskeeper/internal/pkg/prom"
)

// AlertSink 告警写入接口。
type AlertSink interface {
	WriteMissedRunAlert(ctx context.Context, source string, missed []MissedRunInfo) error
}

// MissedRunInfo 单条 missed run 信息。
type MissedRunInfo struct {
	FlowID            string
	NodeID            string
	CronSpec          string
	ExpectedFireAt    time.Time
	MissedDurationSec int64
}

// Repo 数据访问接口。
type Repo interface {
	ListMissed(ctx context.Context, before time.Time) ([]MissedRunInfo, error)
	RecordMissedAudit(ctx context.Context, missed MissedRunInfo) error
}

// Detector missed-run 检测器。
type Detector struct {
	repo  Repo
	alert AlertSink
	log   *slog.Logger
	now   func() time.Time
}

// NewDetector 构造 missed-run 检测器。
func NewDetector(repo Repo, alert AlertSink, log *slog.Logger) *Detector {
	if log == nil {
		log = slog.Default()
	}
	return &Detector{repo: repo, alert: alert, log: log, now: time.Now}
}

// SetClock 注入时钟（仅测试用）。
func (d *Detector) SetClock(now func() time.Time) {
	if now != nil {
		d.now = now
	}
}

// RunOnBoot 启动时执行一次 missed-run 检测。
func (d *Detector) RunOnBoot(ctx context.Context) error {
	d.log.Info("missed-run-detector: scanning on boot")
	return d.scan(ctx)
}

func (d *Detector) scan(ctx context.Context) error {
	now := d.now().UTC()
	missed, err := d.repo.ListMissed(ctx, now)
	if err != nil {
		return fmt.Errorf("missed-run-detector: list missed: %w", err)
	}

	if len(missed) == 0 {
		d.log.Info("missed-run-detector: no missed runs detected")
		return nil
	}

	d.log.Warn("missed-run-detector: detected missed runs",
		slog.Int("count", len(missed)))

	for _, m := range missed {
		prom.IncSchedulerMissedRun(m.FlowID, m.NodeID)
		if err := d.repo.RecordMissedAudit(ctx, m); err != nil {
			d.log.Error("missed-run-detector: write audit failed",
				slog.String("flow_id", m.FlowID),
				slog.String("node_id", m.NodeID),
				slog.Any("err", err))
		}
	}

	if d.alert != nil {
		if err := d.alert.WriteMissedRunAlert(ctx, "scheduler-missed-run", missed); err != nil {
			return fmt.Errorf("missed-run-detector: write alert: %w", err)
		}
	}

	return nil
}
