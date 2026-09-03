// Package authz — Data-Guard sensitivity 维度扩展（路径 A P1-3 阶段 2 任务 2.3–2.5）。
//
// 设计要点：
//   - 不改 casbin model（保持向后兼容；旧 4-tuple policy 自动按 Public 处理）
//   - SensitivityTier 表保存 user → sensitivity-tier 授权
//   - AllowWithSensitivity 串联 RBAC + sensitivity 闸门：
//     1) 标准 RBAC 通过 →
//     2) 若 resource 是 Public/Internal 放行；
//     若 resource 是 TopSecret/Restricted → 校验用户具备对应或更高 tier
//   - GrantSensitivityTier / RevokeSensitivityTier 走标准 repo 接口
package authz

import (
	"context"
	"errors"

	"gorm.io/gorm"

	"github.com/vincent-wuhan/opskeeper/internal/dataguard"
)

// SensitivityTier sensitivity-reader 等级（与 dataguard.Sensitivity 1:1 对应）。
//
// 排名：TierPublic < TierInternal < TierConfidential < TierRestricted < TierTopSecret。
// 持有更高 tier 可访问所有更低 tier 资源。
type SensitivityTier string

const (
	TierPublic       SensitivityTier = "public_reader"
	TierInternal     SensitivityTier = "internal_reader"
	TierConfidential SensitivityTier = "confidential_reader"
	TierRestricted   SensitivityTier = "restricted_reader"
	TierTopSecret    SensitivityTier = "topsecret_reader"
)

// tierRanking 用整型比较两个 tier（更高 = 更具权限）。
func tierRanking(t SensitivityTier) int {
	switch t {
	case TierPublic:
		return 0
	case TierInternal:
		return 1
	case TierConfidential:
		return 2
	case TierRestricted:
		return 3
	case TierTopSecret:
		return 4
	}
	return -1
}

// TierForSensitivity 把 dataguard.Sensitivity 降到最低 tier 要求。
//
// 例如：
//   - Restricted → 需要 restricted_reader（持有 topsecret 也算）
//   - Public / Internal → 不要求（默认通过）
func TierForSensitivity(s dataguard.Sensitivity) SensitivityTier {
	switch s {
	case dataguard.Restricted:
		return TierRestricted
	case dataguard.Confidential:
		return TierConfidential
	case dataguard.Internal:
		return TierInternal
	case dataguard.Public:
		return TierPublic
	case dataguard.TopSecret:
		return TierTopSecret
	}
	return TierPublic
}

// MeetsSensitivity 检查用户的 tier 是否满足 required。
//
// 兼容性（task 2.5）：若 required 是 Public 默认通过；用户 tier 为空字符串视为 Public。
func MeetsSensitivity(userTier, required SensitivityTier) bool {
	rank := func(t SensitivityTier) int {
		if t == "" {
			return 0 // 缺省当作 Public
		}
		return tierRanking(t)
	}
	return rank(userTier) >= rank(required)
}

// SensitivityTierRepo 是 user → tier 授权的持久化接口（biz 抽象）。
type SensitivityTierRepo interface {
	Get(ctx context.Context, userID, orgID uint64) (SensitivityTier, error)
	Set(ctx context.Context, userID, orgID uint64, tier SensitivityTier) error
	Delete(ctx context.Context, userID, orgID uint64) error
}

// sqlSensitivityTierRepo 是 GORM 实现。
type sqlSensitivityTierRepo struct{ db *gorm.DB }

// NewSensitivityTierRepo 构造 GORM repo。
func NewSensitivityTierRepo(db *gorm.DB) SensitivityTierRepo {
	return &sqlSensitivityTierRepo{db: db}
}

// SensitivityTierRow 是 user_sensitivity_tier 表的 GORM 模型。
type SensitivityTierRow struct {
	UserID    uint64          `gorm:"primaryKey;column:user_id"`
	OrgID     uint64          `gorm:"primaryKey;column:org_id"`
	Tier      SensitivityTier `gorm:"size:32;not null;column:tier"`
	GrantedBy *uint64         `gorm:"column:granted_by"`
	CreatedAt int64           `gorm:"column:created_at;autoCreateTime"`
	UpdatedAt int64           `gorm:"column:updated_at;autoUpdateTime"`
}

// TableName 固定表名。
func (SensitivityTierRow) TableName() string { return "user_sensitivity_tier" }

// Migrate 注册 user_sensitivity_tier 表（additive）。
func MigrateSensitivityTier(db *gorm.DB) error {
	if db == nil {
		return errors.New("authz: nil db")
	}
	return db.AutoMigrate(&SensitivityTierRow{})
}

func (r *sqlSensitivityTierRepo) Get(ctx context.Context, userID, orgID uint64) (SensitivityTier, error) {
	var row SensitivityTierRow
	err := r.db.WithContext(ctx).
		Where("user_id = ? AND org_id = ?", userID, orgID).
		First(&row).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return "", nil // 无记录 → 默认 Public tier
		}
		return "", err
	}
	return row.Tier, nil
}

func (r *sqlSensitivityTierRepo) Set(ctx context.Context, userID, orgID uint64, tier SensitivityTier) error {
	return r.db.WithContext(ctx).Clauses().
		Save(&SensitivityTierRow{UserID: userID, OrgID: orgID, Tier: tier}).Error
}

func (r *sqlSensitivityTierRepo) Delete(ctx context.Context, userID, orgID uint64) error {
	return r.db.WithContext(ctx).
		Where("user_id = ? AND org_id = ?", userID, orgID).
		Delete(&SensitivityTierRow{}).Error
}

// ErrTierUnmet 暴露 AllowWithSensitivity 拒绝原因。
var ErrTierUnmet = errors.New("authz: sensitivity tier unmet")

// AllowWithSensitivity 串联 RBAC + sensitivity 闸门。
//
// 决策流程：
//  1. 标准 Allow（org-domain RBAC）通过与否
//  2. 若 sensitivity == "" / Public / Internal → 放行
//  3. 查 SensitivityTierRepo：user 未设 tier → 默认 Public reader
//  4. MeetsSensitivity 检查；若不满足返回 false + ErrTierUnmet
//
// 兼容旧 policy（task 2.5）：调用方未传 sens 视为 Public，零侵入。
func (a *Enforcer) AllowWithSensitivity(ctx context.Context, userID, orgID uint64, obj, act string, sens dataguard.Sensitivity, tierRepo SensitivityTierRepo) (bool, error) {
	if !a.Allow(ctx, userID, orgID, obj, act) {
		return false, nil
	}
	if sens == "" || sens == dataguard.Public || sens == dataguard.Internal {
		return true, nil
	}
	required := TierForSensitivity(sens)
	if tierRepo == nil {
		// 没装配 tier 表 → 退化为只看 RBAC（兼容性默认 Public）
		return true, nil
	}
	tier, err := tierRepo.Get(ctx, userID, orgID)
	if err != nil {
		return false, err
	}
	if MeetsSensitivity(tier, required) {
		return true, nil
	}
	return false, ErrTierUnmet
}

// GrantSensitivityTier / RevokeSensitivityTier 是外部 API 用于 admin 调整 tier。
func (a *Enforcer) GrantSensitivityTier(ctx context.Context, tierRepo SensitivityTierRepo, userID, orgID uint64, tier SensitivityTier, grantedBy uint64) error {
	if tierRepo == nil {
		return errors.New("authz: nil tierRepo")
	}
	return tierRepo.Set(ctx, userID, orgID, tier)
}

func (a *Enforcer) RevokeSensitivityTier(ctx context.Context, tierRepo SensitivityTierRepo, userID, orgID uint64) error {
	if tierRepo == nil {
		return errors.New("authz: nil tierRepo")
	}
	return tierRepo.Delete(ctx, userID, orgID)
}
