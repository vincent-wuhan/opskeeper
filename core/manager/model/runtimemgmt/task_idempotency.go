package runtimemgmt

import (
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/google/uuid"
)

// IdempotencyContract 是授权变更型任务自动重试的认领前指纹。它必须在
// 首次认领前持久化，让恢复流程无需重启后询问外部系统即可判断该副作用
// 是否已经发生。
//
// 该契约来自规格要求“变更型任务自动重试前必须声明幂等”。指纹覆盖任务
// 身份、载荷摘要、目标系统与范围，使两个请求在同一记录上看似相同，
// 仍可解析为一次尝试。
//
// 只读与一次性预览任务可以携带契约，但不强制；安全级别才是约束规则。
type IdempotencyContract struct {
	// TaskID 是所属持久化任务；每个任务一条记录。契约在准入时设置，
	// 首次认领后不可变。
	TaskID string `gorm:"primaryKey;size:64"`

	// TenantID 是所属租户；查询必须按它过滤。
	TenantID string `gorm:"index;size:64;not null"`

	// Fingerprint 是契约体（PayloadDigest + TargetSystem + Scope）
	// 序列化后的 SHA-256 十六进制摘要；重试时编排器重新计算以检测漂移。
	Fingerprint string `gorm:"size:64;not null"`

	// PayloadDigest 标识生产者提供的载荷字节，是 64 位十六进制 SHA-256。
	PayloadDigest string `gorm:"size:64;not null"`

	// TargetSystem 表示目标系统，例如 preview-pg、pg:primary、shell:agent。
	// 指纹将范围绑定到系统，范围变化会导致摘要不匹配。
	TargetSystem string `gorm:"size:64;not null"`

	// Scope 将请求绑定到精确目标身份，例如表、资源 ID 或命令；
	// 相同字符串参与指纹计算。
	Scope string `gorm:"size:128;not null"`

	// ExternalRequestID 是生产者可选外部请求 ID，例如事务 ID或告警指纹；
	// 便于人工关联，但不参与指纹。
	ExternalRequestID string `gorm:"size:128"`

	// IssuedAt 是生产者提交契约的时间；仅为墙钟时间，不作为重试预算解释。
	IssuedAt time.Time `gorm:"index;not null"`

	// IssuedBy 表示生产者来源，例如 agent、chat、automation；
	// 自由短字符串，仅用于审计。
	IssuedBy string `gorm:"size:64;not null;default:''"`
}

// TableName 固定表名。迁移必须保持不变；自动重试闸门按
// (task_id, fingerprint) 匹配。
func (IdempotencyContract) TableName() string { return "runtimemgmt_idempotency_contracts" }

// NewIdempotencyContract 构建并计算契约指纹，算法为
// sha256(payloadDigest + "|" + targetSystem + "|" + scope)。
// 调用方必须原样持久化返回值，不做规范化，保证重试可直接比较。
func NewIdempotencyContract(
	taskID, tenantID, payloadDigest, targetSystem, scope, externalRequestID, issuedBy string,
	issuedAt time.Time,
) (IdempotencyContract, error) {
	if taskID == "" || tenantID == "" || payloadDigest == "" || targetSystem == "" || scope == "" {
		return IdempotencyContract{}, ErrIdempotencyRequired
	}
	sum := sha256.Sum256([]byte(payloadDigest + "|" + targetSystem + "|" + scope))
	return IdempotencyContract{
		TaskID:            taskID,
		TenantID:          tenantID,
		Fingerprint:       hex.EncodeToString(sum[:]),
		PayloadDigest:     payloadDigest,
		TargetSystem:      targetSystem,
		Scope:             scope,
		ExternalRequestID: externalRequestID,
		IssuedAt:          issuedAt.UTC(),
		IssuedBy:          issuedBy,
	}, nil
}

// Matches 判断已保存指纹是否等于重算值。本函数不重算；编排器重新哈希后
// 调用比较，可发现契约记录被旁路修改。
func (c IdempotencyContract) Matches(recomputed string) bool {
	if c.Fingerprint == "" || recomputed == "" {
		return false
	}
	return c.Fingerprint == recomputed
}

// RecomputeFingerprint 是重试器与测试共用的标准哈希函数；
// 独立函数便于测试固定拼接顺序。
func RecomputeFingerprint(payloadDigest, targetSystem, scope string) string {
	sum := sha256.Sum256([]byte(payloadDigest + "|" + targetSystem + "|" + scope))
	return hex.EncodeToString(sum[:])
}

// NewTaskID 是标准持久化任务 ID 生成器。UUID v4 可跨语言往返，
// Manager 与 Edge 调用方无需协调即可生成。
func NewTaskID() string {
	return uuid.NewString()
}
