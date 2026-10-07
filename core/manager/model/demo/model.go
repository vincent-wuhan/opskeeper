package demo

import "time"

const (
	ScenarioStatusStarting         = "starting"
	ScenarioStatusAwaitingAlert    = "awaiting_alert"
	ScenarioStatusAlertCorrelated  = "alert_correlated"
	ScenarioStatusDiagnosisSent    = "diagnosis_dispatched"
	ScenarioStatusPreviewReady     = "preview_ready"
	ScenarioStatusAwaitingApproval = "awaiting_approval"
	ScenarioStatusRepairDispatched = "repair_dispatched"
	ScenarioStatusVerifying        = "verifying"
	ScenarioStatusRecovered        = "recovered"
	ScenarioStatusClosed           = "closed"
	ScenarioStatusStartFailed      = "start_failed"
)

type ScenarioRun struct {
	ID                uint64    `gorm:"column:id;primaryKey;autoIncrement" json:"id"`
	TenantID          uint64    `gorm:"column:tenant_id;not null;uniqueIndex:uniq_demo_scenario_idempotency,priority:1" json:"tenant_id"`
	ScenarioID        string    `gorm:"column:scenario_id;size:64;not null;uniqueIndex:uniq_demo_scenario_idempotency,priority:2" json:"scenario_id"`
	IdempotencyKey    string    `gorm:"column:idempotency_key;size:128;not null;uniqueIndex:uniq_demo_scenario_idempotency,priority:3" json:"idempotency_key"`
	IncidentID        uint64    `gorm:"column:incident_id;not null;index" json:"incident_id"`
	PoolManifestID    string    `gorm:"column:pool_manifest_id;size:128;not null;default:''" json:"pool_manifest_id"`
	Target            string    `gorm:"column:target;size:128;not null" json:"target"`
	TargetFingerprint string    `gorm:"column:target_fingerprint;size:160;not null" json:"target_fingerprint"`
	AlertFingerprint  string    `gorm:"column:alert_fingerprint;size:160;not null;default:''" json:"alert_fingerprint"`
	Status            string    `gorm:"column:status;size:32;not null" json:"status"`
	ExpiresAt         time.Time `gorm:"column:expires_at;not null" json:"expires_at"`
	CreatedAt         time.Time `gorm:"column:created_at;autoCreateTime" json:"created_at"`
	UpdatedAt         time.Time `gorm:"column:updated_at;autoUpdateTime" json:"updated_at"`
}

func (ScenarioRun) TableName() string { return "demo_scenario_runs" }

func IsKnownStatus(status string) bool {
	switch status {
	case ScenarioStatusStarting, ScenarioStatusAwaitingAlert, ScenarioStatusAlertCorrelated,
		ScenarioStatusDiagnosisSent, ScenarioStatusPreviewReady, ScenarioStatusAwaitingApproval,
		ScenarioStatusRepairDispatched, ScenarioStatusVerifying, ScenarioStatusRecovered,
		ScenarioStatusClosed, ScenarioStatusStartFailed:
		return true
	default:
		return false
	}
}
