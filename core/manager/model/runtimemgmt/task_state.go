package runtimemgmt

// TaskState 是单个任务记录的持久化阶段。状态机采用 OpenSpec design.md
// 「Durable task model」定义的 v1 集合：
//
//	queued -> admitted -> claimed -> preparing -> running
//	  -> succeeded | failed | cancelled
//
// 常量使用类型化字符串而非数字 iota，保证未来枚举重排在存储与 API 中
// 仍向后兼容。
type TaskState string

const (
	// TaskStateQueued 是记录初始状态：生产者已持久化请求，准入尚未执行。
	TaskStateQueued TaskState = "queued"

	// TaskStateAdmitted 表示认领前槽位准入通过且运行时选择器满足，
	// 之后允许认领。
	TaskStateAdmitted TaskState = "admitted"

	// TaskStateClaimed 表示特定运行时已通过 Claim 记录持有任务，
	// 准备租约开始计时。
	TaskStateClaimed TaskState = "claimed"

	// TaskStatePreparing 是运行时解析输入、工作区、插件与凭据的短窗口，
	// 由短准备租约约束；之后只能进入 Running 或 Failed。
	TaskStatePreparing TaskState = "preparing"

	// TaskStateRunning 是稳定执行态；存活租约必须通过检查点续约。
	TaskStateRunning TaskState = "running"

	// TaskStateSucceeded 是终态；TaskResult 记录保存脱敏后的结果。
	TaskStateSucceeded TaskState = "succeeded"

	// TaskStateFailed 是终态；TaskResult 记录失败分类与原因。
	// 变更型任务的自动重试由 IdempotencyContract 门控。
	TaskStateFailed TaskState = "failed"

	// TaskStateCancelled 是操作员取消后的终态，并在节点或 Worker 重启后保留。
	TaskStateCancelled TaskState = "cancelled"
)

// AllTaskStates 是用于成员校验的闭集，例如扫描与修复任务中的校验。
// 新增状态时必须同时更新这里与上方常量。
var AllTaskStates = []TaskState{
	TaskStateQueued,
	TaskStateAdmitted,
	TaskStateClaimed,
	TaskStatePreparing,
	TaskStateRunning,
	TaskStateSucceeded,
	TaskStateFailed,
	TaskStateCancelled,
}

// ValidTransitions 是权威邻接表。键必须是非终态，值是允许的下一状态；
// 终态没有出边。
//
// Paths encoded:
//
//	queued     -> admitted, failed, cancelled
//	admitted   -> claimed, failed, cancelled
//	claimed    -> preparing, failed, cancelled
//	preparing  -> running,  failed, cancelled
//	running    -> succeeded, failed, cancelled
//
// 映射刻意显式，不引入闭包或兜底路径。陈旧认领恢复使用恢复时观察到的
// From 状态，而不是从终态回退。
var ValidTransitions = map[TaskState]map[TaskState]struct{}{
	TaskStateQueued: {
		TaskStateAdmitted:  {},
		TaskStateFailed:    {},
		TaskStateCancelled: {},
	},
	TaskStateAdmitted: {
		TaskStateClaimed:   {},
		TaskStateFailed:    {},
		TaskStateCancelled: {},
	},
	TaskStateClaimed: {
		TaskStatePreparing: {},
		TaskStateFailed:    {},
		TaskStateCancelled: {},
	},
	TaskStatePreparing: {
		TaskStateRunning:   {},
		TaskStateFailed:    {},
		TaskStateCancelled: {},
	},
	TaskStateRunning: {
		TaskStateSucceeded: {},
		TaskStateFailed:    {},
		TaskStateCancelled: {},
	},
}

// IsValidTransition 判断 from -> to 是否是 ValidTransitions 允许的边。
// 未知 from 返回 false；调用方应返回 ErrInvalidTransition，而不是记录后静默继续。
func IsValidTransition(from, to TaskState) bool {
	if _, ok := ValidTransitions[from]; !ok {
		return false
	}
	_, ok := ValidTransitions[from][to]
	return ok
}

// IsTerminal 判断 s 是否为 succeeded、failed 或 cancelled。
// 只读路径用它决定展示恢复入口或停止轮询。
func IsTerminal(s TaskState) bool {
	switch s {
	case TaskStateSucceeded, TaskStateFailed, TaskStateCancelled:
		return true
	default:
		return false
	}
}

// Normalize 将未知或空状态映射为安全哨兵行为：无副作用地返回
// ErrUnknownState。历史重放等必须继续的调用方应跳过该行并记录观察结果。
func Normalize(s string) (TaskState, error) {
	switch TaskState(s) {
	case TaskStateQueued,
		TaskStateAdmitted,
		TaskStateClaimed,
		TaskStatePreparing,
		TaskStateRunning,
		TaskStateSucceeded,
		TaskStateFailed,
		TaskStateCancelled:
		return TaskState(s), nil
	default:
		return "", ErrUnknownState
	}
}
