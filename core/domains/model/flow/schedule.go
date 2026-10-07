package flow

import "time"

// FlowScheduleNextFire persists the trigger.cron scheduler cursor so a
// manager restart can detect and report missed executions.
type FlowScheduleNextFire struct {
	FlowID          uint64     `gorm:"column:flow_id;primaryKey" json:"flow_id"`
	NodeID          string     `gorm:"column:node_id;primaryKey;size:64" json:"node_id"`
	CronSpec        string     `gorm:"column:cron_spec;size:64;not null" json:"cron_spec"`
	NextFireAt      time.Time  `gorm:"column:next_fire_at;not null;index:idx_flow_schedule_status,priority:1" json:"next_fire_at"`
	LastFireAt      *time.Time `gorm:"column:last_fire_at" json:"last_fire_at,omitempty"`
	LastHeartbeatAt time.Time  `gorm:"column:last_heartbeat_at;not null" json:"last_heartbeat_at"`
	Status          string     `gorm:"column:status;size:16;not null;default:enabled;index:idx_flow_schedule_status,priority:2" json:"status"`
	MissedCount     int        `gorm:"column:missed_count;not null;default:0" json:"missed_count"`
	CreatedAt       time.Time  `gorm:"column:created_at;autoCreateTime" json:"created_at"`
	UpdatedAt       time.Time  `gorm:"column:updated_at;autoUpdateTime" json:"updated_at"`
}

func (FlowScheduleNextFire) TableName() string { return "flow_schedule_next_fire" }

const (
	FlowScheduleStatusEnabled  = "enabled"
	FlowScheduleStatusDisabled = "disabled"
	FlowScheduleStatusMissed   = "missed"
)

// MissedRunAudit records one detected expected fire. The unique pair makes
// repeated boot scans idempotent.
type MissedRunAudit struct {
	ID                   uint64    `gorm:"column:id;primaryKey;autoIncrement" json:"id"`
	ScheduleID           string    `gorm:"column:schedule_id;size:128;not null;uniqueIndex:idx_missed_unique,priority:1" json:"schedule_id"`
	ExpectedFireAt       time.Time `gorm:"column:expected_fire_at;not null;uniqueIndex:idx_missed_unique,priority:2" json:"expected_fire_at"`
	MissedDurationSec    int64     `gorm:"column:missed_duration_sec;not null" json:"missed_duration_sec"`
	DetectedAt           time.Time `gorm:"column:detected_at;not null;index" json:"detected_at"`
	AlertGenerated       bool      `gorm:"column:alert_generated;not null;default:false" json:"alert_generated"`
	NotificationDedupKey string    `gorm:"column:notification_dedup_key;size:64;index" json:"notification_dedup_key,omitempty"`
	CreatedAt            time.Time `gorm:"column:created_at;autoCreateTime" json:"created_at"`
}

func (MissedRunAudit) TableName() string { return "scheduler_missed_run_audit" }
