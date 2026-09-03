package store

import (
	"time"

	"github.com/vincent-wuhan/opskeeper/internal/dataguard"
)

// LabelSource 标签来源（自动 / 人工 / 继承）。
type LabelSource string

const (
	SourceHeuristic LabelSource = "heuristic" // 自动启发式打标
	SourceManual    LabelSource = "manual"    // 人工打标
	SourceInherited LabelSource = "inherited" // 继承打标
	SourceOverride  LabelSource = "override"  // 子资源 override
)

// DataSensitivityLabel 资源敏感度标签记录。
//
// 路径 A P1-3 阶段 1 任务 1.1 — data_sensitivity_label 表 GORM 模型。
//
// 表结构：
//   - PK: (resource_type, resource_id)
//   - 索引: (sensitivity), (label_source, confidence)
type DataSensitivityLabel struct {
	ResourceType   string    `gorm:"primaryKey;size:64;not null" json:"resource_type"`
	ResourceID     string    `gorm:"primaryKey;size:128;not null" json:"resource_id"`
	Sensitivity    string    `gorm:"size:16;not null;index" json:"sensitivity"`
	ComplianceTags string    `gorm:"type:json" json:"compliance_tags"` // JSON array of framework names
	LabelSource    string    `gorm:"size:16;not null;index" json:"label_source"`
	Confidence     float64   `gorm:"not null;default:0.5" json:"confidence"`
	LabeledBy      string    `gorm:"size:128" json:"labeled_by,omitempty"` // user/heuristic-id
	Notes          string    `gorm:"type:text" json:"notes,omitempty"`
	CreatedAt      time.Time `gorm:"not null" json:"created_at"`
	UpdatedAt      time.Time `gorm:"not null" json:"updated_at"`
}

// TableName GORM 表名。
func (DataSensitivityLabel) TableName() string {
	return "data_sensitivity_label"
}

// ToSensitivity 转换为 dataguard.Sensitivity。
func (l *DataSensitivityLabel) ToSensitivity() (dataguard.Sensitivity, error) {
	return dataguard.Parse(l.Sensitivity)
}
