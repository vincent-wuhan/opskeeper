package runtimemgmt

// FailureClass 是应用于终态 Failed 任务的闭集分类，供编排器自动重试
// 决策、Dashboard 失败视图以及失败 API（tasks.md §2.3）随 TaskResult 记录。
//
// 取值使用类型化字符串而非 iota，保证新增分类与磁盘既有记录兼容。
// 下方常量必须与 FailureClassValues 保持同步。
type FailureClass string

const (
	// FailureClassNoIdempotencyContract 表示任务标记为变更型，但首次认领前
	// 未提交 IdempotencyContract 指纹。规格场景“变更型任务无幂等契约”
	// 要求使用该分类，使重试保持终态。
	FailureClassNoIdempotencyContract FailureClass = "no_idempotency_contract"

	// FailureClassLeaseExpired 表示运行时在生成检查点前丢失准备或存活租约；
	// 恢复流程必须在重试前归类。
	FailureClassLeaseExpired FailureClass = "lease_expired"

	// FailureClassRuntimeLost 表示认领运行时在生成检查点前进入 recently_lost
	// 或 offline；恢复流程可按任务策略在其他运行时续跑。
	FailureClassRuntimeLost FailureClass = "runtime_lost"

	// FailureClassAdmissionDenied 表示认领前槽位准入拒绝排队。它与瞬时信号
	// ErrNoCapacity 不同，本身是终态分类。
	FailureClassAdmissionDenied FailureClass = "admission_denied"

	// FailureClassWorkspaceUnavailable 表示无法获取任务工作区，例如锁忙或
	// 根目录缺失；除非迁移工作区所有权，否则为终态。
	FailureClassWorkspaceUnavailable FailureClass = "workspace_unavailable"

	// FailureClassCredentialDenied 表示凭据代理拒绝发放请求范围，例如审计
	// 不完整、授权绑定已撤销任务等。
	FailureClassCredentialDenied FailureClass = "credential_denied"

	// FailureClassPluginIncompatible 表示固定版本的插件发布快照未通过兼容性
	// 或能力校验。
	FailureClassPluginIncompatible FailureClass = "plugin_incompatible"

	// FailureClassSideEffectDuplicate 表示即将产生重复副作用，任务在真正发出
	// 操作前被停止。
	FailureClassSideEffectDuplicate FailureClass = "side_effect_duplicate"

	// FailureClassWorkerError 是 Worker 错误未被更精确分类时的兜底，
	// Worker 的结构化错误保存在 TaskResult.ErrorMessage。
	FailureClassWorkerError FailureClass = "worker_error"

	// FailureClassCancelled 表示操作员取消使任务进入 Failed；与独立的
	// TaskStateCancelled 区分，让恢复流程仍可使用 Failed 终态语义。
	FailureClassCancelled FailureClass = "cancelled"

	// FailureClassInternal 表示控制面自身错误，例如数据库写入或调度器缺陷；
	// 需要通知值班，绝不自动重试。
	FailureClassInternal FailureClass = "internal"
)

// FailureClassValues 是闭集枚举，供校验使用。
var FailureClassValues = []FailureClass{
	FailureClassNoIdempotencyContract,
	FailureClassLeaseExpired,
	FailureClassRuntimeLost,
	FailureClassAdmissionDenied,
	FailureClassWorkspaceUnavailable,
	FailureClassCredentialDenied,
	FailureClassPluginIncompatible,
	FailureClassSideEffectDuplicate,
	FailureClassWorkerError,
	FailureClassCancelled,
	FailureClassInternal,
}

// IsFailureClass 判断 s 是否属于 FailureClassValues；失败 API 用它拒绝未知标签。
func IsFailureClass(s FailureClass) bool {
	for _, v := range FailureClassValues {
		if v == s {
			return true
		}
	}
	return false
}

// Validate 校验 fc 是否为识别的分类。空值或未知值返回
// ErrFailureClassInvalid，调用方可使用 errors.Is 分支处理。
func (fc FailureClass) Validate() error {
	if fc == "" {
		return ErrFailureClassInvalid
	}
	if !IsFailureClass(fc) {
		return ErrFailureClassInvalid
	}
	return nil
}

// IsRetryable 表示编排器是否可以对该分类尝试自动恢复。变更型任务仍需
// 认领前 IdempotencyContract；只读与一次性预览任务始终安全。
//
// 返回值是 v1 默认策略；新增分类必须在同一变更中明确其重试立场。
func (fc FailureClass) IsRetryable(taskSafety TaskSafety) bool {
	switch fc {
	case FailureClassNoIdempotencyContract,
		FailureClassAdmissionDenied,
		FailureClassCredentialDenied,
		FailureClassPluginIncompatible,
		FailureClassSideEffectDuplicate,
		FailureClassInternal:
		// 绝不自动重试：这些是策略拒绝或需要操作员处理的控制面缺陷。
		return false
	case FailureClassLeaseExpired, FailureClassRuntimeLost:
		// 分类本身可重试；变更型任务还必须由上游闸门校验幂等契约。
		return true
	case FailureClassWorkspaceUnavailable:
		// 仅安全级别允许时重试：只读可重新获取，变更型依赖工作区策略。
		return taskSafety == TaskSafetyReadOnly
	case FailureClassWorkerError:
		// Worker 错误对只读任务可重试；变更型任务依赖幂等契约。
		return taskSafety != TaskSafetyMutating
	case FailureClassCancelled:
		return false
	default:
		return false
	}
}
