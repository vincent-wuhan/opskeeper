package runtimemgmt

import (
	"context"
	"time"

	model "github.com/vincent-wuhan/opskeeper/core/manager/model/runtimemgmt"
)

// TaskStore 是 biz 层依赖的最窄持久化端口（ADR D-1）。SQL 实现放
// data/runtimemgmt/store；本包（与 server 包）只见方法签名。
//
// 并发契约（ADR D-3，实现必须遵守，biz 测试钉死）：
//   - ClaimTask / StartTask / TransitionTask / WriteTerminal /
//     CloseAttemptAndRequeue / SupersedeAndRequeue 内的任务行更新必须是
//     单条条件 UPDATE（带 state 前置），RowsAffected==1 才算成功；否则
//     返回 model.ErrConflict 语义（此处用 errs.ErrConflict 由实现映射）。
//   - 多行写入（task + attempt + result 等）必须单事务，失败整组回滚，
//     不留半写状态。
type TaskStore interface {
	// ---- 读 ----

	// GetTask 按 (task_id, tenant_id) 取任务行；不存在返回可被
	// errors.Is 判定为 not-found 的错误。
	GetTask(ctx context.Context, taskID, tenantID string) (model.Task, error)
	// ListTasks 按租户列任务（state 为空 = 全部），CreatedAt 倒序。
	ListTasks(ctx context.Context, tenantID string, state model.TaskState, limit, offset int) ([]model.Task, error)
	// GetClaim 取任务当前认领行；未认领返回 not-found。
	GetClaim(ctx context.Context, taskID, tenantID string) (model.Claim, error)
	// GetLease 按 (task_id, generation) 取租约。
	GetLease(ctx context.Context, taskID, tenantID string, generation int64) (model.Lease, error)
	// GetAttempt 按 (task_id, attempt_number) 取尝试记录。
	GetAttempt(ctx context.Context, taskID, tenantID string, attemptNumber int) (model.AttemptRecord, error)
	// LatestAttemptNumber 返回任务最新尝试序号；无尝试返回 0。
	LatestAttemptNumber(ctx context.Context, taskID, tenantID string) (int, error)
	// GetIdempotencyContract 取任务幂等契约；无契约行返回
	// model.ErrIdempotencyRequired（认领闸门据此落终态）。
	GetIdempotencyContract(ctx context.Context, taskID, tenantID string) (model.IdempotencyContract, error)
	// CountInFlight 统计全租户 in-flight（claimed+preparing+running）。
	CountInFlight(ctx context.Context) (int, error)
	// ListStaleClaims 取 LeasedUntil <= now 的认领行（带 state 非终态
	// 条件），上限 limit。
	ListStaleClaims(ctx context.Context, now time.Time, limit int) ([]model.Claim, error)
	// ListCheckpoints 按 (task_id, attempt_number) 列检查点，
	// Sequence 升序。
	ListCheckpoints(ctx context.Context, taskID, tenantID string, attemptNumber int) ([]model.Checkpoint, error)

	// ---- 写 ----

	// CreateTaskWithAdmission 单事务写 task（queued）+ admission 决策行
	// +（可选）幂等契约行。决策 admitted 时任务行直接落 admitted
	// （入队内联评估，ADR D-4）。
	CreateTaskWithAdmission(ctx context.Context, task model.Task, decision model.AdmissionDecision, contract *model.IdempotencyContract) error
	// ClaimTask 单事务：条件 UPDATE 任务行 admitted→claimed（唯一裁决
	// 点，并发双认领恰一成功），upsert 认领行（新代际 = 旧+1，首认领
	// 为 1），写租约（PrepareDeadline=prepareDeadline）、写首个尝试行
	// （attempt=1，MaxAttempts 快照由实现读取默认）。
	ClaimTask(ctx context.Context, taskID, tenantID, runtimeID string, now, prepareDeadline time.Time) (model.Task, model.Claim, model.Lease, model.AttemptRecord, error)
	// StartTask 单事务：条件 UPDATE claimed→preparing→running（两笔
	// 迁移一次调用），写 liveness 租约期限；代际不匹配返回
	// model.ErrClaimLost。
	StartTask(ctx context.Context, taskID, tenantID, runtimeID string, generation int64, now, livenessDeadline time.Time) (model.Task, model.Lease, error)
	// SaveLease 写回滚动后的租约行（续约/检查点路径）。
	SaveLease(ctx context.Context, lease model.Lease) error
	// NextCheckpointSequence 返回 (task_id, attempt_number) 下一个
	// 检查点序号（当前最大 +1，首条为 1）。
	NextCheckpointSequence(ctx context.Context, taskID, tenantID string, attemptNumber int) (int, error)
	// AppendCheckpointWithLease 单事务：追加检查点行 + 写回滚动租约。
	// 检查点行是追加表，无代际条件（biz 已在调用前裁决代际）。
	AppendCheckpointWithLease(ctx context.Context, cp model.Checkpoint, lease model.Lease) error
	// WriteTerminal 单事务：条件 UPDATE 任务行→终态（from 任意非终态）
	// + 写 attempt 关闭行 + 写 result 行。已终态返回
	// model.ErrTerminalState。
	WriteTerminal(ctx context.Context, taskID, tenantID string, to model.TaskState, attempt model.AttemptRecord, result model.TaskResult, now time.Time) (model.Task, error)
	// CloseAttemptAndRequeue 单事务：关闭 attempt（outcome=failed）+
	// 条件 UPDATE 任务行→admitted（失败重试路径）。
	CloseAttemptAndRequeue(ctx context.Context, taskID, tenantID string, to model.TaskState, attempt model.AttemptRecord, now time.Time) (model.Task, error)
	// SupersedeAndRequeue 单事务：旧尝试标 superseded + 认领行代际
	// 推进（AdvanceTo 语义，新认领者由后续 claim 写）+ 条件 UPDATE
	// 任务行→admitted（陈旧恢复路径）。task.RuntimeID 清空。
	SupersedeAndRequeue(ctx context.Context, claim model.Claim, attempt model.AttemptRecord, now time.Time) (model.Task, error)
	// TransitionTask 单条条件 UPDATE 任务行 from→to（cancel 未认领
	// 路径等无子行的迁移）。
	TransitionTask(ctx context.Context, taskID, tenantID string, to model.TaskState, now time.Time) (model.Task, error)
}
