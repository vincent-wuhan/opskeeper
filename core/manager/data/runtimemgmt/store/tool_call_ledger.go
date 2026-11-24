// tool_call_ledger.go 是 biz/pigruntime.ToolCallLedger 的 gorm 适配：
// 起始行 Insert + 按 correlation id 的收尾 Update。端口由消费方声明
// （tool_call_ledger.go），本文件只做翻译——装配根经 NewBizToolCallLedger
// 窄化注入，测试用内存 fake（gateway_fail_closed_test.go）。
package store

import (
	"context"
	"errors"

	"gorm.io/gorm"

	bizpigruntime "github.com/vincent-wuhan/opskeeper/core/manager/biz/pigruntime"
	model "github.com/vincent-wuhan/opskeeper/core/manager/model/runtimemgmt"
)

// ToolCallLedgerStore 是 gorm 实现。
type ToolCallLedgerStore struct {
	db *gorm.DB
}

// NewToolCallLedgerStore 构造。
func NewToolCallLedgerStore(db *gorm.DB) *ToolCallLedgerStore {
	return &ToolCallLedgerStore{db: db}
}

// NewBizToolCallLedger 是装配根用的窄化构造。
func NewBizToolCallLedger(db *gorm.DB) bizpigruntime.ToolCallLedger {
	return NewToolCallLedgerStore(db)
}

var _ bizpigruntime.ToolCallLedger = (*ToolCallLedgerStore)(nil)

// RecordStart 落起始行并返回 correlation id（模型 BeforeCreate 补
// UUID）。Status 由 Decision 推导：blocked 裁决永不分发，起始行即
// 终态；allowed/proposal_required 起始为 dispatched，等 RecordEnd 回填。
func (s *ToolCallLedgerStore) RecordStart(ctx context.Context, start bizpigruntime.ToolCallStart) (string, error) {
	rec := model.ToolCallRecord{
		RuntimeID:        start.RuntimeID,
		TaskID:           start.TaskID,
		TenantID:         start.TenantID,
		IncidentID:       start.IncidentID,
		StableToolID:     start.StableToolID,
		ToolClass:        start.ToolClass,
		CallerRole:       start.CallerRole,
		CallerWorker:     start.CallerWorker,
		ReleaseDigest:    start.ReleaseDigest,
		CapabilityDigest: start.CapabilityDigest,
		ParametersSHA256: start.ParametersSHA256,
		TraceID:          start.TraceID,
		StartedAt:        start.StartedAt,
		Decision:         start.Decision,
		DenyReason:       start.DenyReason,
	}
	switch start.Decision {
	case model.ToolCallDecisionBlocked:
		rec.Status = model.ToolCallStatusBlocked
		ended := start.StartedAt
		rec.EndedAt = &ended
	default:
		rec.Status = model.ToolCallStatusDispatched
	}
	if err := rec.Validate(); err != nil {
		return "", err
	}
	if err := s.db.WithContext(ctx).Create(&rec).Error; err != nil {
		return "", err
	}
	return rec.ID, nil
}

// RecordEnd 按 correlation id 回填收尾事实。行不存在视为写失败
// （fail closed 语义由网关处理：起始行写不进就不分发）。
func (s *ToolCallLedgerStore) RecordEnd(ctx context.Context, end bizpigruntime.ToolCallEnd) error {
	if end.ID == "" {
		return errors.New("runtimemgmt: tool call end without correlation id")
	}
	updates := map[string]any{
		"status":        end.Status,
		"result_sha256": end.ResultSHA256,
		"error_message": end.ErrorMessage,
		"ended_at":      end.EndedAt,
	}
	result := s.db.WithContext(ctx).Model(&model.ToolCallRecord{}).
		Where("id = ?", end.ID).
		Updates(updates)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return errors.New("runtimemgmt: tool call record not found for end")
	}
	return nil
}

// MigrateToolCallTable 注册 PiG 调用表（pig-runtime-adapter 3.3）。
// 单独成 migrator 供 managerMigrators 计数闸门逐表族可读。
func MigrateToolCallTable(db *gorm.DB) error {
	return db.AutoMigrate(&model.ToolCallRecord{})
}
