// Package runtimemgmt 保存运行时管理控制面的持久化实体，对应
// OpenSpec 变更 `runtime-management-gap` 的 §2「Durable Task Lifecycle」。
//
// §2.1 列出组成持久化任务记录及其配套契约的十个领域概念：
//
//	Task                — 中央持久化记录（task.go）
//	TaskState           — 任务状态机（task_state.go）
//	IdempotencyContract — 首次认领前提交的指纹（task_idempotency.go）
//	AdmissionDecision   — 认领前槽位准入快照（task_admission.go）
//	Claim               — 运行时认领代际与所有权（task_claim.go）
//	Lease               — 准备租约与存活租约期限（task_lease.go）
//	Checkpoint          — 恢复指针与载荷引用（task_checkpoint.go）
//	AttemptRecord       — 单次执行尝试的追加记录（task_attempt.go）
//	FailureClass        — 终态失败分类（failure_class.go）
//	TaskResult          — 终态结果与脱敏状态（task_result.go）
//
// 本包严格增量交付：只新增 `runtimemgmt_*` 表族和匹配的 Go 类型，
// 不修改、不替代既有 chat_task_executions、approvals、loop_event_log。
// 七阶段事件循环仍由编排器持有，运行时控制面只通过既有公开 API 查询。
//
// 多租户约束：Task、Claim、Lease、Checkpoint、AttemptRecord 与
// TaskResult 均携带 TenantID，查询必须按租户过滤。
// (tenant_id, task_id) 与 (tenant_id, runtime_id) 复合索引用于租户内操作。
package runtimemgmt

import "errors"

// 包级哨兵错误。导出后，准入、认领、租约恢复等调用方可使用 errors.Is
// 分支处理，测试也能避免依赖错误字符串。
var (
	// ErrInvalidTransition 表示请求的任务状态迁移不在 ValidTransitions；
	// 调用方必须视为永久失败，不得自动重试。
	ErrInvalidTransition = errors.New("runtimemgmt: invalid task state transition")

	// ErrUnknownState 表示遇到未识别状态，例如存储中的未映射历史状态。
	ErrUnknownState = errors.New("runtimemgmt: unknown task state")

	// ErrIdempotencyRequired 表示变更型任务在 Claim / Start 前尚未持久化
	// 幂等契约指纹，对应规格要求“变更型任务自动重试前必须声明幂等”。
	ErrIdempotencyRequired = errors.New("runtimemgmt: mutating task requires idempotency contract")

	// ErrLeaseExpired 表示在续约、检查点或结果调用前租约已经到期。
	ErrLeaseExpired = errors.New("runtimemgmt: lease expired")

	// ErrNoCapacity 表示服务端并发上限或运行时上报空闲槽位耗尽，
	// 对应 spec.md 的“无可用容量”场景。
	ErrNoCapacity = errors.New("runtimemgmt: no capacity for slot-before-claim admission")

	// ErrClaimLost 表示任务认领代际已超过调用方视图；租约恢复后新认领者接管。
	ErrClaimLost = errors.New("runtimemgmt: claim generation has advanced")

	// ErrTerminalState 表示检查点、进度、租约续期等非终态写入命中已终态任务。
	ErrTerminalState = errors.New("runtimemgmt: task already in a terminal state")

	// ErrResultNotReady 表示任务尚未进入终态就请求终态结果。
	ErrResultNotReady = errors.New("runtimemgmt: task result not ready")

	// ErrSanitizerMissing 表示持久化终态结果前缺少脱敏决策；
	// 必须已脱敏，或显式记录 sanitized_skipped 及理由。
	ErrSanitizerMissing = errors.New("runtimemgmt: terminal result requires sanitizer status decision")

	// ErrFailureClassInvalid 表示失败分类不在 FailureClass* 闭集内。
	ErrFailureClassInvalid = errors.New("runtimemgmt: failure class is not in the taxonomy")
)
