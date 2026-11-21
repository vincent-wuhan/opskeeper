// capacity.go 是 biz/runtimemgmt.RuntimeCapacitySource 的 SQL 实现：
// 读 runtimemgmt_runtimes 注册行，投影健康（惰性推导）+ 槽位三值。
// 只读端口——本文件不含任何写路径（biz 端口只有一个方法，编译期窄
// 接口即只读性硬保证，与观察者端口同构）。
package store

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"

	bizruntimemgmt "github.com/vincent-wuhan/opskeeper/core/manager/biz/runtimemgmt"
	model "github.com/vincent-wuhan/opskeeper/core/manager/model/runtimemgmt"
)

// CapacitySource 是 gorm 实现。
type CapacitySource struct {
	db *gorm.DB
}

// NewCapacitySource 构造。
func NewCapacitySource(db *gorm.DB) *CapacitySource { return &CapacitySource{db: db} }

// NewBizCapacitySource 是装配根用的窄化构造。
func NewBizCapacitySource(db *gorm.DB) bizruntimemgmt.RuntimeCapacitySource {
	return NewCapacitySource(db)
}

var _ bizruntimemgmt.RuntimeCapacitySource = (*CapacitySource)(nil)

// Capacity 按 runtimeID 投影。未注册返回 (zero, false, nil)——认领闸门
// 按 runtime_not_ready 拒绝，不猜测（ADR 硬约束）。
func (s *CapacitySource) Capacity(ctx context.Context, runtimeID string, now time.Time) (bizruntimemgmt.RuntimeCapacity, bool, error) {
	var rec model.RuntimeRecord
	err := s.db.WithContext(ctx).Where("runtime_id = ?", runtimeID).First(&rec).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return bizruntimemgmt.RuntimeCapacity{}, false, nil
	}
	if err != nil {
		return bizruntimemgmt.RuntimeCapacity{}, false, err
	}
	return bizruntimemgmt.RuntimeCapacity{
		Health:      rec.EvaluateHealth(now),
		TotalSlots:  rec.TotalSlots,
		FreeSlots:   rec.FreeSlots,
		ActiveTasks: rec.ActiveTasks,
	}, true, nil
}
