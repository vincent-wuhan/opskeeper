// worker.go 是 §2.4 worker 接入的通用执行循环（ADR D-6）。
//
// 分层分工：本包（biz）持有端口与循环——TaskHandler 是任务执行端口的
// 窄接口，WorkerRuntime 是 poll→claim→start→Execute→result/failure 的
// 通用编排；具体 handler（只读诊断、repair-preview、AgentTeams 桥）
// 全部住在装配根，跨域依赖（demo / agentteams.state）不越过决策 449 的
// 域边界。
//
// 租约纪律（ADR R2）：全部期限由 server 端（usecase）计算并返回，worker
// 只消费——不推算到期时刻、不本地造租约。循环在每次检查点后由
// usecase.Checkpoint 顺带滚动 liveness（同一存储动作），长时间无检查点
// 的 handler 由「租约到期 → stale-recovery 接管」兜底，循环不另起
// 续租定时器。
//
// 零 goroutine 纪律：本文件不 start 任何 goroutine；循环由装配根经
// Run(ctx) 驱动（errgroup 或 go 均由装配根决定），biz 保持与首切片
// 相同的可测形状。
package runtimemgmt

import (
	"context"
	"errors"
	"fmt"
	"time"

	model "github.com/vincent-wuhan/opskeeper/core/manager/model/runtimemgmt"
)

// TaskHandler 是任务执行端口（ADR D-6）。装配根为每个 kind 注册一个
// 实现；WorkerRuntime 按 Matches 路由。biz 不感知具体 kind——kind 是
// 生产者的路由键，控制面只消费。
type TaskHandler interface {
	// Matches 判定本 handler 是否负责该 kind。注册表是首个命中即用；
	// 两个 handler 同 kind 是装配错误，wiring 测试断言互斥。
	Matches(kind string) bool
	// Execute 执行任务。resume 非 nil 表示从检查点续跑（上次尝试的
	// 最新恢复指针，generation 已对当前认领校验）——handler 必须从
	// ResumeHint/PayloadRef 续跑而不是重跑；mutating handler 必须先
	// 比对幂等契约再产生副作用（ADR D-5，本签名的调用契约）。
	// 进度经 rt.Report 上报（检查点 + 滚租约一体）；Execute 返回 nil
	// 由循环写 succeeded 终态，返回错误由循环分类写 failure。
	Execute(ctx context.Context, task model.Task, resume *model.Checkpoint, rt Reporter) error
}

// Reporter 是 handler 回报进度/检查点的窄端口。WorkerRuntime 实现它，
// 转发到 usecase.Checkpoint（检查点 + liveness 滚动是同一存储动作）。
// handler 不直接触 usecase：签名保持在 Execute 入参里，handler 无从
// 绕过代际校验。
type Reporter interface {
	// Report 追加一条检查点（phase 标签 + 恢复指针）并滚动存活租约。
	// 代际漂移（认领被接管/恢复）返回 model.ErrClaimLost——handler 收
	// 到后应立即放弃执行（后续 result/failure 也会被拒）。
	Report(ctx context.Context, phase, resumeHint, payloadRef string) error
}

// WorkerConfig 是循环的运行参数。全部字段有默认；装配根从 env 读取
// 一次（不在循环里解析 env）。
type WorkerConfig struct {
	// RuntimeID 是认领者身份。认领/租约/终态写入全部以此署名。
	RuntimeID string
	// TenantID 是轮询租户（v1 单租户轮询：任务表按租户过滤）。
	TenantID string
	// PollInterval 是无任务可认领时的等待步长。<=0 取 DefaultPollInterval。
	PollInterval time.Duration
	// PollLimit 是每次轮询拉取的 admitted 上限。
	PollLimit int
}

// DefaultPollInterval 是 v1 轮询步长。秒级轮询与 AgentTeams 桥同一
// 取舍：控制面任务的到达率低，亚秒轮询只增加空转。
const DefaultPollInterval = 5 * time.Second

// WorkerRuntime 是通用执行循环（ADR D-6）。Poll→Claim→Start→Execute
// （handler 经 Reporter 报检查点）→result/failure。并发双认领由
// usecase.Claim 的条件 UPDATE 裁决（D-3）：败者收到 ErrNoCapacity/
// ErrConflict/ErrTerminalState 语义，循环跳过该任务继续轮询。
type WorkerRuntime struct {
	usecase  *Usecase
	handlers []TaskHandler
	cfg      WorkerConfig
}

// NewWorkerRuntime 构造。handlers 顺序即路由顺序（首个 Matches 命中）。
func NewWorkerRuntime(usecase *Usecase, cfg WorkerConfig, handlers ...TaskHandler) *WorkerRuntime {
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = DefaultPollInterval
	}
	if cfg.PollLimit <= 0 {
		cfg.PollLimit = 10
	}
	return &WorkerRuntime{usecase: usecase, handlers: cfg.sanitize(handlers), cfg: cfg}
}

func (c WorkerConfig) sanitize(handlers []TaskHandler) []TaskHandler {
	out := make([]TaskHandler, 0, len(handlers))
	for _, h := range handlers {
		if h != nil {
			out = append(out, h)
		}
	}
	return out
}

// handlerFor 返回首个 Matches 的 handler；nil 表示本 worker 不认识该
// kind——跳过（别的 runtime 可能认识；kind 是路由键不是错误）。
func (w *WorkerRuntime) handlerFor(kind string) TaskHandler {
	for _, h := range w.handlers {
		if h.Matches(kind) {
			return h
		}
	}
	return nil
}

// Run 是循环主体：阻塞直到 ctx 取消。每一轮先处理已认领待续跑的任务
// （恢复指针已在认领时校验），再轮询新 admitted 任务认领执行。
// 单任务失败不终止循环——分类上报后继续下一轮（一个坏任务拖垮整个
// worker 比一次可分类失败更糟）。
func (w *WorkerRuntime) Run(ctx context.Context) error {
	if w == nil || w.usecase == nil {
		return errors.New("runtimemgmt worker: not wired")
	}
	ticker := time.NewTicker(w.cfg.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			w.pollOnce(ctx)
		}
	}
}

// PollOnce 是单轮轮询：拉 admitted → 逐个认领执行。导出给装配根的
// 手动触发（AgentTeams 桥与测试共用同一执行面；bridge 不是 WorkerRuntime
// 的第二种循环，见 wiring 侧头注）。
func (w *WorkerRuntime) PollOnce(ctx context.Context) int {
	if w == nil || w.usecase == nil {
		return 0
	}
	tasks, err := w.usecase.ListTasks(ctx, w.cfg.TenantID, model.TaskStateAdmitted, w.cfg.PollLimit, 0)
	if err != nil {
		return 0
	}
	claimed := 0
	for _, task := range tasks {
		handler := w.handlerFor(task.Kind)
		if handler == nil {
			continue // 本 worker 不认识：留给其他 runtime
		}
		if w.executeOne(ctx, task, handler) {
			claimed++
		}
	}
	return claimed
}

func (w *WorkerRuntime) pollOnce(ctx context.Context) { w.PollOnce(ctx) }

// executeOne 认领并执行单个任务。返回 true 表示发生了一次执行（成功或
// 失败分类均算；claim 被抢/不认识/闸门拒绝返回 false）。
func (w *WorkerRuntime) executeOne(ctx context.Context, task model.Task, handler TaskHandler) bool {
	res, err := w.usecase.Claim(ctx, task.TaskID, task.TenantID, w.cfg.RuntimeID)
	if err != nil {
		// ErrIdempotencyRequired 时 usecase 已把任务落终态（durable 闸门）；
		// 其余（无容量/冲突/终态）都是「这一轮没有它」——跳过。
		return false
	}
	_, lease, err := w.usecase.Start(ctx, task.TaskID, task.TenantID, w.cfg.RuntimeID, res.Claim.Generation)
	if err != nil {
		// 认领成功但 start 失败：按 runtime_lost 分类上报（准备租约窗内
		// 没能进入 running），清掉这次尝试。
		_, _, _ = w.usecase.ReportFailure(ctx, task.TaskID, task.TenantID, w.cfg.RuntimeID, FailureInput{
			Generation:    res.Claim.Generation,
			AttemptNumber: res.Attempt.AttemptNumber,
			FailureClass:  model.FailureClassRuntimeLost,
			FailureReason: "worker failed to start after claim: " + err.Error(),
		})
		return true
	}
	// 检查点续跑指针：当前尝试的最新检查点（同一代际）。首次执行为 nil。
	resume := w.resumeFor(ctx, task, res.Attempt.AttemptNumber, res.Claim.Generation)
	rt := &claimReporter{w: w, task: task, generation: res.Claim.Generation, attempt: res.Attempt.AttemptNumber}
	execErr := handler.Execute(ctx, task, resume, rt)
	if execErr == nil {
		_, _, rerr := w.usecase.ReportResult(ctx, task.TaskID, task.TenantID, w.cfg.RuntimeID, ResultInput{
			Generation:      res.Claim.Generation,
			AttemptNumber:   res.Attempt.AttemptNumber,
			SanitizerStatus: model.SanitizerStatusSkipped,
			SanitizerNotes:  "in-process worker result; output artifacts are worker-managed files, no credentials path",
			OutputRef:       rt.outputRef,
			OutputSize:      rt.outputSize,
			OutputDigest:    rt.outputDigest,
		})
		if rerr != nil {
			// 终态写入被拒（代际被接管/终态竞态）：本认领已作废，
			// 真实状态由接管方持有。这是正常竞态，不是 worker 缺陷。
			_ = lease
			return true
		}
		return true
	}
	// Execute 失败：按 sentinel 分类（handler 用 TaskFailure 包装出
	// 精确分类；裸错误归 worker_error——对只读/一次性可重试，变更型
	// 需幂等契约，与 IsRetryable 的安全级别语义一致）。
	class := classifyExecError(execErr)
	_, _, _ = w.usecase.ReportFailure(ctx, task.TaskID, task.TenantID, w.cfg.RuntimeID, FailureInput{
		Generation:    res.Claim.Generation,
		AttemptNumber: res.Attempt.AttemptNumber,
		FailureClass:  class,
		FailureReason: execErr.Error(),
	})
	return true
}

// resumeFor 读上一尝试的最新检查点作为恢复指针。跨 attempt 的续跑
// （失败重试 / 租约过期 → stale-recovery → 回 admitted → 再认领）在
// 这里重建：新 attempt 的 AttemptNumber 已 +1，检查点行属于旧尝试、
// 旧代际——但 ResumeHint 对 handler 仍有效（spec「Worker restarts
// after checkpoint」的循环侧落点；IsForCurrentClaim 的代际闸门用于
// 同一尝试内的写入校验，续跑指针刻意不按新代际过滤，否则一次接管
// 之后的重跑永远从零开始）。
func (w *WorkerRuntime) resumeFor(ctx context.Context, task model.Task, attemptNumber int, generation int64) *model.Checkpoint {
	prev := attemptNumber - 1
	if prev < 1 {
		return nil // 首次执行：无恢复指针
	}
	cps, err := w.usecase.ListCheckpoints(ctx, task.TaskID, task.TenantID, prev)
	if err != nil {
		return nil
	}
	return model.ResumeFor(cps)
}

// classifyExecError 把 Execute 的错误映射进失败分类闭集。handler 用
// TaskFailure{Class} 包装给出精确分类；裸错误按 worker_error 兜底。
func classifyExecError(err error) model.FailureClass {
	var tf TaskFailure
	if errors.As(err, &tf) {
		return tf.Class
	}
	return model.FailureClassWorkerError
}

// TaskFailure 是 handler 声明精确失败分类的包装错误。分类必须在
// FailureClassValues 闭集内（ReportFailure 会校验）；不认识分类的
// handler 用普通错误即可（兜底 worker_error）。
type TaskFailure struct {
	Class model.FailureClass
	Err   error
}

func (e TaskFailure) Error() string {
	if e.Class == "" {
		e.Class = model.FailureClassWorkerError
	}
	if e.Err != nil {
		return string(e.Class) + ": " + e.Err.Error()
	}
	return string(e.Class)
}

func (e TaskFailure) Unwrap() error { return e.Err }

// claimReporter 是 Reporter 的循环侧实现：转发到 usecase.Checkpoint，
// 并记录 handler 声明的输出三元组（OutputRef/Size/Digest）——handler
// 在 Report 时携带产出物引用，循环在终态写入时回填 TaskResult。
type claimReporter struct {
	w            *WorkerRuntime
	task         model.Task
	generation   int64
	attempt      int
	outputRef    string
	outputSize   int64
	outputDigest string
}

// Report 追加检查点并滚动 liveness 租约。ErrClaimLost 透传给 handler
// （应立即放弃）；ErrLeaseExpired 同理（stale-recovery 即将接管）。
func (r *claimReporter) Report(ctx context.Context, phase, resumeHint, payloadRef string) error {
	_, _, err := r.w.usecase.Checkpoint(ctx, r.task.TaskID, r.task.TenantID, r.w.cfg.RuntimeID, CheckpointInput{
		Generation:    r.generation,
		AttemptNumber: r.attempt,
		Phase:         phase,
		ResumeHint:    resumeHint,
		PayloadRef:    payloadRef,
	})
	return err
}

// RecordOutput 是 handler 声明终态产出物的入口：写 <task-id>.json 一类
// 的输出 worker 在文件落盘后调用，循环把它如实回填 TaskResult 的
// OutputRef/OutputSize/OutputDigest。
func (r *claimReporter) RecordOutput(ref string, size int64, digest string) {
	r.outputRef = ref
	r.outputSize = size
	r.outputDigest = digest
}

// CompiledOutputReporter 是需要回填输出三元组的 handler 视图。Reporter
// 的窄面只有 Report；诊断 worker 需要 RecordOutput——装配根以类型断言
// 升级（biz 不为此扩宽端口：RecordOutput 是装配根 handler 的约定，
// 不是生命周期的约定）。
type CompiledOutputReporter interface {
	Reporter
	RecordOutput(ref string, size int64, digest string)
}

// FormatWorkerID 生成 v1 的进程内 worker runtime 身份。诊断 worker 与
// repair-preview handler 共用 manager 进程内的一个循环（一个 WorkerRuntime，
// 多个 handler），身份形如 manager-worker/<name>。
func FormatWorkerID(name string) string {
	if name == "" {
		name = "default"
	}
	return fmt.Sprintf("manager-worker/%s", name)
}
