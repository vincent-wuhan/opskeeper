// Package store 保存运行时管理域的持久化层。首切片只有一张表：
// runtimemgmt_runtimes（§1 Runtime Inventory and Readback）；§2.2+§2.3
// 切片追加任务生命周期八表（ ADR D-1），全部纯增量。八表中的
// runtimemgmt_task_results 同时是 pig-runtime-adapter 2.2 生命周期归一层
// 落库的那张（PiG 终态写 TaskResult）。
package store

import (
	"gorm.io/gorm"

	model "github.com/vincent-wuhan/opskeeper/core/manager/model/runtimemgmt"
)

// Migrate 注册 runtime 域全部模型。纯增量：只新建 runtimemgmt_* 表族，
// 不触碰既有表族；AutoMigrate additive 不删列，回滚即 drop 这些表。
//
// 八张任务表是 §2.1 已落模型（PR #98）的首次迁移注册：
//   - runtimemgmt_tasks               中央任务行（状态机权威）
//   - runtimemgmt_idempotency_contracts  变更型自动重试的认领前指纹
//   - runtimemgmt_admission_decisions    认领前槽位准入快照
//   - runtimemgmt_claims              认领代际与所有权（一行每任务）
//   - runtimemgmt_leases              准备/存活租约期限（按代际）
//   - runtimemgmt_checkpoints         恢复指针（追加表）
//   - runtimemgmt_attempt_records     单次尝试（追加表）
//   - runtimemgmt_task_results        终态结果与脱敏裁决
//
// TEXT 列无 DB 默认值（MySQL 1101 陷阱，首切片先例）：模型的 TEXT 字段
// （FailureReason/ErrorMessage/SanitizerNotes/ResumeHint 等）在代码侧
// 初始化，AutoMigrate 不为 TEXT 列生成 DEFAULT 子句。
func Migrate(db *gorm.DB) error {
	if err := db.AutoMigrate(&model.RuntimeRecord{}); err != nil {
		return err
	}
	return MigrateTaskTables(db)
}

// MigrateTaskTables 注册 §2 任务生命周期八表。单独成 migrator 是为了
// managerMigrators 计数闸门（migrations_mysql_test.go 的 want=26）能
// 逐表族可读：25 是 runtimemgmt_runtimes（§1 首切片），26 是本函数
// （§2.2+§2.3）。两个 migrator 都幂等（AutoMigrate additive），先 Runtimes
// 后 Tasks 的顺序只影响日志可读性，不影响正确性。
func MigrateTaskTables(db *gorm.DB) error {
	return db.AutoMigrate(
		&model.Task{},
		&model.IdempotencyContract{},
		&model.AdmissionDecision{},
		&model.Claim{},
		&model.Lease{},
		&model.Checkpoint{},
		&model.AttemptRecord{},
		&model.TaskResult{},
	)
}
