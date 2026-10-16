package runtimemgmt

// TaskSafety 描述任务的副作用边界，是自动重试、准入和配额决策的输入，
// 定义来自 design.md「Durable task model」与规格要求“变更型任务自动
// 重试前必须声明幂等”。
//
// v1 只有三种取值：
//   - ReadOnly：探测、诊断、盘点，始终可安全重试。
//   - Disposable：preview-pg、临时运行器等一次性预览运行时；副作用限于
//     运行时内，重试时可丢弃。
//   - Mutating：运行外持久副作用；首次认领前必须已有 IdempotencyContract
//     指纹，才有自动重试资格。
type TaskSafety string

const (
	TaskSafetyReadOnly   TaskSafety = "read_only"
	TaskSafetyDisposable TaskSafety = "disposable"
	TaskSafetyMutating   TaskSafety = "mutating"
)

// TaskSafetyValues 是闭集枚举，供校验使用。
var TaskSafetyValues = []TaskSafety{
	TaskSafetyReadOnly,
	TaskSafetyDisposable,
	TaskSafetyMutating,
}

// IsTaskSafety 判断 s 是否属于 TaskSafetyValues；空值与未知值返回 false。
func IsTaskSafety(s TaskSafety) bool {
	for _, v := range TaskSafetyValues {
		if v == s {
			return true
		}
	}
	return false
}

// RequiresIdempotencyContract 表示该安全级别是否必须在认领前具备幂等契约
// 才能安全自动重试。只读与一次性任务不强制要求，仅受失败分类闸门约束。
func (s TaskSafety) RequiresIdempotencyContract() bool {
	return s == TaskSafetyMutating
}
