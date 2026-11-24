// Package runtimemgmt（biz 层）持有 §2「Durable Task Lifecycle」的状态机
// 编排（runtime-management-gap §2.2+§2.3，ADR 方案 A / D-1）。
//
// 分层分工：model/runtimemgmt 持行对象与状态机邻接表；本包持用例——
// 准入评估、认领、租约滚动、检查点追加、终态写入与陈旧恢复；data 层的
// TaskStore 持 SQL（本包只见窄端口）；server 层做 decode/audit/respond。
//
// 并发纪律（ADR D-3，本刀最高风险点）：认领、租约滚动、终态写入全部经
// TaskStore 的单条条件 UPDATE（affected rows==1 才算成功）完成。本包绝不
// read-modify-write 任务行；所有“读出来的行”只用于判定，写回一律带状态
// 前置条件，由存储层在同一语句里裁决。旧代际的 checkpoint/progress/
// result 一律 model.ErrClaimLost（spec「Worker restarts after checkpoint」
// 场景靠它成立）。
//
// 域边界（决策 449 延续）：本包不 import edge/chatruntime/agentteams/
// pluginhost/demo 五个被观察域；跨域读 RuntimeRecord 只经
// RuntimeCapacitySource 端口（data 层实现），装配根注入。
package runtimemgmt

import (
	"context"
	"errors"
	"fmt"
	"time"

	model "github.com/vincent-wuhan/opskeeper/core/manager/model/runtimemgmt"
)

// Clock 是可注入时钟端口（ADR D-1）。测试用固定时刻钉租约与超时语义；
// 生产实现是 time.Now 的包装。全部期限 server 端计算并返回，worker 只
// 消费不推算（ADR R2）。
type Clock interface {
	Now() time.Time
}

// RuntimeCapacitySource 是准入与认领的运行时侧只读端口：按 runtimeID
// 投影健康 + 槽位。实现放 data 层（读 runtimemgmt_runtimes），本包不持
// gorm 句柄。
type RuntimeCapacitySource interface {
	// Capacity 返回指定 runtime 的健康推导与槽位投影。runtime 未注册时
	// 第二返回值为 false（认领闸门按 runtime_not_ready 拒绝，不猜测）。
	Capacity(ctx context.Context, runtimeID string, now time.Time) (RuntimeCapacity, bool, error)
}

// RuntimeCapacity 是 RuntimeRecord 的准入投影切片——biz 只需要健康三值
// 与槽位三值，不需要注册表的版本/摘要列。
type RuntimeCapacity struct {
	Health      model.RuntimeHealth
	TotalSlots  int
	FreeSlots   int
	ActiveTasks int
}

// AdmissionConfig 是 §2.2 准入闸门的 server 侧配置。
type AdmissionConfig struct {
	// ServerLimit 是全租户 in-flight（claimed+preparing+running）并发上限。
	// env：OPSKEEPER_RUNTIME_TASK_SERVER_LIMIT，默认 DefaultServerLimit。
	ServerLimit int
	// PrepareLeaseTTL 是认领时 PrepareDeadline 的窗口。
	PrepareLeaseTTL time.Duration
	// LivenessLeaseTTL 是进入 running 后 LivenessDeadline 的窗口，
	// 也是续约/检查点滚动的步长。
	LivenessLeaseTTL time.Duration
}

// DefaultServerLimit 与 §2.2 的默认并发上限。默认部署（未设 env）用
// 这个值——一个控制面默认放 64 个并发任务，与五类首切片 runtime 的
// 自报容量同量级。
const DefaultServerLimit = 64

// DefaultPrepareLeaseTTL / DefaultLivenessLeaseTTL 是 v1 的租约窗口。
// prepare 是短窗（解析输入/工作区/插件/凭据），liveness 按 5 分钟滚动、
// 由检查点或显式续约续——窗口值是 server 侧常量，ADR 把 env 覆盖留给
// 后续切片（与首切片心跳窗口同判）。
const (
	DefaultPrepareLeaseTTL  = 2 * time.Minute
	DefaultLivenessLeaseTTL = 5 * time.Minute
)

// WithDefaults 填充零值字段。ServerLimit<=0 取默认；TTL 为零取默认。
func (c AdmissionConfig) WithDefaults() AdmissionConfig {
	if c.ServerLimit <= 0 {
		c.ServerLimit = DefaultServerLimit
	}
	if c.PrepareLeaseTTL <= 0 {
		c.PrepareLeaseTTL = DefaultPrepareLeaseTTL
	}
	if c.LivenessLeaseTTL <= 0 {
		c.LivenessLeaseTTL = DefaultLivenessLeaseTTL
	}
	return c
}

// RuntimeReadyForAdmission 判定健康是否可认领：offline 与
// recently_lost 拒绝；starting/online/degraded 放行（degraded 是“活着
// 但有异常观察”，spec 要求接受存活观察——容量闸门只看生死，不看完美）。
func RuntimeReadyForAdmission(h model.RuntimeHealth) bool {
	switch h {
	case model.RuntimeHealthOffline, model.RuntimeHealthRecentlyLost:
		return false
	default:
		return true
	}
}

// ProjectedFreeSlots 是槽位兜底投影（ADR D-4）：TotalSlots>0 而
// FreeSlots=0 时，用心跳上报的 ActiveTasks<TotalSlots 投影——自报
// FreeSlots 缺席但占用数如实上报的 runtime 不至于被饿死。
func ProjectedFreeSlots(c RuntimeCapacity) int {
	if c.FreeSlots > 0 {
		return c.FreeSlots
	}
	if c.TotalSlots > 0 && c.ActiveTasks < c.TotalSlots {
		return c.TotalSlots - c.ActiveTasks
	}
	return 0
}

// ---- 用例结果形状 ----

// EnqueueResult 是入队结果。Admitted=false 表示任务留在 queued 且已写
// rejected 决策行（spec「No capacity is available」：不建 claim，任务
// 不消失，下次评估 upsert 覆盖）。
type EnqueueResult struct {
	Task        model.Task
	Admitted    bool
	Admission   model.AdmissionDecision
	Idempotency *model.IdempotencyContract
}

// ClaimResult 是认领结果：新代际认领行 + 首个尝试记录 + 准备租约。
type ClaimResult struct {
	Task    model.Task
	Claim   model.Claim
	Lease   model.Lease
	Attempt model.AttemptRecord
}

// CheckpointInput 是检查点/进度写入的入参。Progress 是 Checkpoint 的
// 轻量别名（phase 标签必填、resume hint 可空），同一存储动作。
type CheckpointInput struct {
	Generation    int64
	AttemptNumber int
	Phase         string
	ResumeHint    string
	PayloadRef    string
}

// ResultInput 是终态结果写入的入参。
type ResultInput struct {
	Generation      int64
	AttemptNumber   int
	Status          model.TaskState // succeeded（/failure、/cancel 走各自用例）
	OutputRef       string
	OutputSize      int64
	OutputDigest    string
	SanitizerStatus model.SanitizerStatus
	SanitizerNotes  string
}

// FailureInput 是失败上报的入参。
type FailureInput struct {
	Generation    int64
	AttemptNumber int
	FailureClass  model.FailureClass
	FailureReason string
}

// RecoverResult 是陈旧恢复一次扫描的结果。
type RecoverResult struct {
	// Recovered 是被新代际接管（任务回 admitted，旧尝试标 superseded）
	// 的 task id 列表。
	Recovered []string
	// Terminated 是按分类不可重试而落终态 failed 的 task id 列表。
	Terminated []string
	// Scanned 是本次扫到的陈旧认领行数。
	Scanned int
}

// ---- Usecase ----

// Usecase 是任务生命周期状态机的唯一入口。全部方法幂等或条件化：
// 失败分支不落半写状态（store 的事务边界保证一组行同生同灭）。
type Usecase struct {
	store TaskStore
	cap   RuntimeCapacitySource
	clock Clock
	cfg   AdmissionConfig
}

// NewUsecase 构造。cfg 零值字段由 WithDefaults 填默认。
func NewUsecase(store TaskStore, cap RuntimeCapacitySource, clock Clock, cfg AdmissionConfig) *Usecase {
	if clock == nil {
		clock = realClock{}
	}
	return &Usecase{store: store, cap: cap, clock: clock, cfg: cfg.WithDefaults()}
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now().UTC() }

// ---- 入队 + 准入（§2.2）----

// Enqueue 入队并内联评估一次准入（ADR D-4：入队时一次 + 认领时再校验，
// 零后台 sweeper）。mutating 任务必须携带幂等契约字段，否则
// errs 边界上的 ErrIdempotencyRequired 由调用面映射 400（spec 场景
// 「A mutating task has no idempotency contract」的第一道闸）。
func (u *Usecase) Enqueue(ctx context.Context, req EnqueueRequest) (EnqueueResult, error) {
	now := u.clock.Now()
	task, err := model.NewTask(
		req.TenantID, req.IncidentID, req.Kind, "", req.Payload,
		req.Safety, req.Selector, req.TraceID, req.Priority, now,
	)
	if err != nil {
		return EnqueueResult{}, fmt.Errorf("runtimemgmt: build task: %w", err)
	}
	// 幂等契约（可选携带）：mutating 必须携带——API 边 fail-fast。
	var contract *model.IdempotencyContract
	if req.Safety.RequiresIdempotencyContract() {
		if req.IdempotencyTargetSystem == "" || req.IdempotencyScope == "" {
			return EnqueueResult{}, model.ErrIdempotencyRequired
		}
	}
	if req.IdempotencyTargetSystem != "" && req.IdempotencyScope != "" {
		c, cerr := model.NewIdempotencyContract(
			task.TaskID, task.TenantID, task.PayloadDigest,
			req.IdempotencyTargetSystem, req.IdempotencyScope,
			req.IdempotencyExternalID, req.IssuedBy, now,
		)
		if cerr != nil {
			return EnqueueResult{}, fmt.Errorf("runtimemgmt: build idempotency contract: %w", cerr)
		}
		contract = &c
	}
	decision, admitted := u.evaluateAdmission(ctx, task, now)
	if err := u.store.CreateTaskWithAdmission(ctx, task, decision, contract); err != nil {
		return EnqueueResult{}, err
	}
	if admitted {
		// store 侧按决策把首行直接落 admitted（省一笔迁移）；返回值
		// 同步这一状态，调用方（server/测试）不需要再读一次库。
		task.State = model.TaskStateAdmitted
		return EnqueueResult{Task: task, Admitted: true, Admission: decision, Idempotency: contract}, nil
	}
	// 任务已持久化（queued + rejected 决策行）；返回给调用方的是
	// “入队成功但未获准”，不是错误——spec 的拒绝语义是留队等重评。
	return EnqueueResult{Task: task, Admitted: false, Admission: decision}, nil
}

// EnqueueRequest 是入队入参。Payload 是载荷字节（哈希后不回存）。
type EnqueueRequest struct {
	TenantID                string
	IncidentID              string
	Kind                    string
	Payload                 []byte
	Safety                  model.TaskSafety
	Selector                string
	TraceID                 string
	Priority                int
	IdempotencyTargetSystem string
	IdempotencyScope        string
	IdempotencyExternalID   string
	IssuedBy                string
}

// evaluateAdmission 计算一次准入决策：server limit ∩ runtime 健康 ∩
// 槽位。runtime 侧只在请求携带 selector（= runtimeID 的 v1 形状）时
// 评估；无 selector 的任务按 server limit 单闸评估——认领时还有第二道
// 闸（ClaimRevalidate）。
func (u *Usecase) evaluateAdmission(ctx context.Context, task model.Task, now time.Time) (model.AdmissionDecision, bool) {
	decision := model.AdmissionDecision{
		TaskID:          task.TaskID,
		TenantID:        task.TenantID,
		RuntimeSelector: task.DesiredRuntimeSelector,
		DecidedAt:       now,
		DecidedBy:       "scheduler.v1",
		ServerLimit:     u.cfg.ServerLimit,
	}
	inFlight, err := u.store.CountInFlight(ctx)
	if err != nil || inFlight >= u.cfg.ServerLimit {
		decision.Outcome = model.AdmissionOutcomeRejected
		decision.Reason = model.AdmissionReasonNoCapacity
		return decision, false
	}
	if task.DesiredRuntimeSelector == "" {
		decision.Outcome = model.AdmissionOutcomeAdmitted
		return decision, true
	}
	capacity, ok, cerr := u.cap.Capacity(ctx, task.DesiredRuntimeSelector, now)
	if cerr != nil || !ok || !RuntimeReadyForAdmission(capacity.Health) {
		decision.Outcome = model.AdmissionOutcomeRejected
		decision.Reason = model.AdmissionReasonRuntimeNotReady
		return decision, false
	}
	free := ProjectedFreeSlots(capacity)
	if free <= 0 {
		decision.Outcome = model.AdmissionOutcomeRejected
		decision.Reason = model.AdmissionReasonNoCapacity
		return decision, false
	}
	decision.RuntimeLimit = free
	decision.Outcome = model.AdmissionOutcomeAdmitted
	return decision, true
}

// ---- 认领（§2.3 / D-3 并发正确性）----

// Claim 让 runtime 认领一个 admitted 任务。store 侧是单条条件 UPDATE
// （state='admitted' 前置），并发双认领恰一个成功。认领内联重评准入
// （懒重评）：runtime 不健康或零槽时拒绝，任务留 admitted。
//
// mutating 任务在此执行 durable 闸门：无契约行 → 终态 failed +
// no_idempotency_contract（spec 场景原文），零重试。
func (u *Usecase) Claim(ctx context.Context, taskID, tenantID, runtimeID string) (ClaimResult, error) {
	now := u.clock.Now()
	// 懒重评：认领者必须是已注册、健康、有槽的 runtime。
	capacity, ok, err := u.cap.Capacity(ctx, runtimeID, now)
	if err != nil {
		return ClaimResult{}, err
	}
	if !ok || !RuntimeReadyForAdmission(capacity.Health) {
		return ClaimResult{}, fmt.Errorf("%w: runtime %s not ready for claim", model.ErrNoCapacity, runtimeID)
	}
	if ProjectedFreeSlots(capacity) <= 0 {
		return ClaimResult{}, fmt.Errorf("%w: runtime %s reports no free slots", model.ErrNoCapacity, runtimeID)
	}
	inFlight, err := u.store.CountInFlight(ctx)
	if err != nil {
		return ClaimResult{}, err
	}
	if inFlight >= u.cfg.ServerLimit {
		return ClaimResult{}, fmt.Errorf("%w: server in-flight limit %d reached", model.ErrNoCapacity, u.cfg.ServerLimit)
	}

	// mutating durable 闸门：终态 no_idempotency_contract。
	task, err := u.store.GetTask(ctx, taskID, tenantID)
	if err != nil {
		return ClaimResult{}, err
	}
	if task.Safety.RequiresIdempotencyContract() {
		contract, cerr := u.store.GetIdempotencyContract(ctx, taskID, tenantID)
		if cerr != nil || contract.Fingerprint == "" {
			if cerr != nil && !errors.Is(cerr, model.ErrIdempotencyRequired) {
				return ClaimResult{}, cerr
			}
			if tErr := u.terminateNoContract(ctx, task, now); tErr != nil {
				return ClaimResult{}, tErr
			}
			return ClaimResult{}, model.ErrIdempotencyRequired
		}
	}

	claimed, claim, lease, attempt, err := u.store.ClaimTask(ctx, taskID, tenantID, runtimeID, now,
		now.Add(u.cfg.PrepareLeaseTTL))
	if err != nil {
		return ClaimResult{}, err
	}
	return ClaimResult{Task: claimed, Claim: claim, Lease: lease, Attempt: attempt}, nil
}

// terminateNoContract 把 mutating 无契约任务落终态 failed +
// no_idempotency_contract（含 attempt/result 行）。
func (u *Usecase) terminateNoContract(ctx context.Context, task model.Task, now time.Time) error {
	attempt := model.AttemptRecord{
		TaskID: task.TaskID, AttemptNumber: 1, TenantID: task.TenantID,
		MaxAttempts: 1, StartedAt: now, EndedAt: &now,
		Outcome:       model.AttemptOutcomeFailed,
		FailureClass:  model.FailureClassNoIdempotencyContract,
		FailureReason: "mutating task claimed without a persisted idempotency contract",
		RuntimeID:     task.RuntimeID,
	}
	result := model.TaskResult{
		TaskID: task.TaskID, AttemptNumber: 1, TenantID: task.TenantID,
		Status: model.TaskStateFailed, EndedAt: now,
		FailureClass:    model.FailureClassNoIdempotencyContract,
		ErrorMessage:    "mutating task requires an idempotency contract before the first claim",
		SanitizerStatus: model.SanitizerStatusSkipped,
		SanitizerNotes:  "control-plane classification row; carries no worker output",
	}
	_, err := u.store.WriteTerminal(ctx, task.TaskID, task.TenantID, model.TaskStateFailed, attempt, result, now)
	return err
}

// ---- 启动（claimed→preparing→running 一次调用两笔迁移，ADR D-2）----

// Start 把 claimed 任务推进到 running：条件 UPDATE 先置 preparing（写
// Lease.PrepareDeadline 已在认领时完成、LivenessDeadline 在此设置）再置
// running。generation 不匹配返回 ErrClaimLost。
func (u *Usecase) Start(ctx context.Context, taskID, tenantID, runtimeID string, generation int64) (model.Task, model.Lease, error) {
	now := u.clock.Now()
	liveness := now.Add(u.cfg.LivenessLeaseTTL)
	return u.store.StartTask(ctx, taskID, tenantID, runtimeID, generation, now, liveness)
}

// ---- 续约 / 检查点 / 进度（同一存储动作：滚 liveness + 条件追加）----

// RenewLease 滚动存活租约。过期租约不复活——返回 ErrLeaseExpired，
// 由恢复流程接管（模型语义，Lease.ExtendLiveness）。终态任务残留的
// 认领行不构成续约资格：ErrTerminalState（验收项「终态后写入」）。
func (u *Usecase) RenewLease(ctx context.Context, taskID, tenantID, runtimeID string, generation int64) (model.Lease, error) {
	now := u.clock.Now()
	task, err := u.store.GetTask(ctx, taskID, tenantID)
	if err != nil {
		return model.Lease{}, err
	}
	if model.IsTerminal(task.State) {
		return model.Lease{}, model.ErrTerminalState
	}
	lease, err := u.store.GetLease(ctx, taskID, tenantID, generation)
	if err != nil {
		return lease, err
	}
	claim, err := u.store.GetClaim(ctx, taskID, tenantID)
	if err != nil {
		return lease, err
	}
	if !claimBelongsTo(claim, runtimeID, generation) {
		return lease, model.ErrClaimLost
	}
	extended, err := lease.ExtendLiveness(now, u.cfg.LivenessLeaseTTL)
	if err != nil {
		return lease, err
	}
	if err := u.store.SaveLease(ctx, extended); err != nil {
		return extended, err
	}
	return extended, nil
}

// Checkpoint 追加一条检查点并滚动存活租约。旧代际 → ErrClaimLost；
// 终态任务 → ErrTerminalState（认领行残留不构成写入资格）；检查点行
// 本身是证据（route-audit 无行裁决的依据）。
func (u *Usecase) Checkpoint(ctx context.Context, taskID, tenantID, runtimeID string, in CheckpointInput) (model.Checkpoint, model.Lease, error) {
	now := u.clock.Now()
	task, err := u.store.GetTask(ctx, taskID, tenantID)
	if err != nil {
		return model.Checkpoint{}, model.Lease{}, err
	}
	if model.IsTerminal(task.State) {
		return model.Checkpoint{}, model.Lease{}, model.ErrTerminalState
	}
	claim, err := u.store.GetClaim(ctx, taskID, tenantID)
	if err != nil {
		return model.Checkpoint{}, model.Lease{}, err
	}
	if !claimBelongsTo(claim, runtimeID, in.Generation) {
		return model.Checkpoint{}, model.Lease{}, model.ErrClaimLost
	}
	lease, err := u.store.GetLease(ctx, taskID, tenantID, in.Generation)
	if err != nil {
		return model.Checkpoint{}, model.Lease{}, err
	}
	extended, err := lease.ExtendLiveness(now, u.cfg.LivenessLeaseTTL)
	if err != nil {
		return model.Checkpoint{}, model.Lease{}, err
	}
	sequence, err := u.store.NextCheckpointSequence(ctx, taskID, tenantID, in.AttemptNumber)
	if err != nil {
		return model.Checkpoint{}, model.Lease{}, err
	}
	cp := model.Checkpoint{
		TaskID: taskID, TenantID: tenantID,
		AttemptNumber: in.AttemptNumber, Sequence: sequence,
		Generation: in.Generation, Phase: in.Phase,
		ResumeHint: in.ResumeHint, PayloadRef: in.PayloadRef,
		RecordedAt: now, RecordedBy: runtimeID,
	}
	if err := u.store.AppendCheckpointWithLease(ctx, cp, extended); err != nil {
		return model.Checkpoint{}, model.Lease{}, err
	}
	return cp, extended, nil
}

// Progress 是检查点的轻量别名：phase 必填、resume hint 可空。同一存储
// 动作（滚租约 + 追加行），不建第二张表。
func (u *Usecase) Progress(ctx context.Context, taskID, tenantID, runtimeID string, in CheckpointInput) (model.Checkpoint, model.Lease, error) {
	if in.Phase == "" {
		in.Phase = "progress"
	}
	return u.Checkpoint(ctx, taskID, tenantID, runtimeID, in)
}

// ---- 终态（result / failure / cancel）----

// ReportResult 写成功终态。succeeded 的 TaskResult.ErrorMessage 必空、
// FailureClass 必空（model.Validate 裁决）。
func (u *Usecase) ReportResult(ctx context.Context, taskID, tenantID, runtimeID string, in ResultInput) (model.Task, model.TaskResult, error) {
	now := u.clock.Now()
	claim, err := u.store.GetClaim(ctx, taskID, tenantID)
	if err != nil {
		return model.Task{}, model.TaskResult{}, err
	}
	if !claimBelongsTo(claim, runtimeID, in.Generation) {
		return model.Task{}, model.TaskResult{}, model.ErrClaimLost
	}
	attempt := model.AttemptRecord{
		TaskID: taskID, AttemptNumber: in.AttemptNumber, TenantID: tenantID,
		MaxAttempts: in.AttemptNumber, StartedAt: claim.ClaimedAt, EndedAt: &now,
		Outcome: model.AttemptOutcomeSucceeded, RuntimeID: runtimeID,
	}
	contract, _ := u.store.GetIdempotencyContract(ctx, taskID, tenantID)
	result := model.TaskResult{
		TaskID: taskID, AttemptNumber: in.AttemptNumber, TenantID: tenantID,
		Status: model.TaskStateSucceeded, EndedAt: now,
		OutputRef: in.OutputRef, OutputSize: in.OutputSize, OutputDigest: in.OutputDigest,
		SanitizerStatus: in.SanitizerStatus, SanitizerNotes: in.SanitizerNotes,
		CompletedBy:               runtimeID,
		PodIdempotencyFingerprint: contract.Fingerprint,
	}
	if err := result.Validate(); err != nil {
		return model.Task{}, model.TaskResult{}, err
	}
	task, err := u.store.WriteTerminal(ctx, taskID, tenantID, model.TaskStateSucceeded, attempt, result, now)
	if err != nil {
		return model.Task{}, model.TaskResult{}, err
	}
	return task, result, nil
}

// ReportFailure 写失败。可重试分类（IsRetryable ∧ HasRetryBudget ∧
// mutating 指纹匹配）关当前 attempt、任务回 admitted——不是终态；
// 否则落终态 failed。ADR D-5。
func (u *Usecase) ReportFailure(ctx context.Context, taskID, tenantID, runtimeID string, in FailureInput) (model.Task, bool, error) {
	now := u.clock.Now()
	if err := in.FailureClass.Validate(); err != nil {
		return model.Task{}, false, err
	}
	task, err := u.store.GetTask(ctx, taskID, tenantID)
	if err != nil {
		return model.Task{}, false, err
	}
	claim, err := u.store.GetClaim(ctx, taskID, tenantID)
	if err != nil {
		return model.Task{}, false, err
	}
	if !claimBelongsTo(claim, runtimeID, in.Generation) {
		return model.Task{}, false, model.ErrClaimLost
	}
	prev, err := u.store.GetAttempt(ctx, taskID, tenantID, in.AttemptNumber)
	if err != nil {
		return model.Task{}, false, err
	}
	retryable := in.FailureClass.IsRetryable(task.Safety) && prev.HasRetryBudget()
	if retryable && task.Safety.RequiresIdempotencyContract() {
		contract, cerr := u.store.GetIdempotencyContract(ctx, taskID, tenantID)
		if cerr != nil {
			retryable = false
		} else {
			recomputed := model.RecomputeFingerprint(contract.PayloadDigest, contract.TargetSystem, contract.Scope)
			if !contract.Matches(recomputed) {
				retryable = false
			}
		}
	}
	closed := prev
	closed.EndedAt = &now
	closed.Outcome = model.AttemptOutcomeFailed
	closed.FailureClass = in.FailureClass
	closed.FailureReason = in.FailureReason
	if retryable {
		updated, err := u.store.CloseAttemptAndRequeue(ctx, taskID, tenantID, model.TaskStateAdmitted, closed, now)
		if err != nil {
			return model.Task{}, false, err
		}
		return updated, true, nil
	}
	result := model.TaskResult{
		TaskID: taskID, AttemptNumber: in.AttemptNumber, TenantID: tenantID,
		Status: model.TaskStateFailed, EndedAt: now,
		FailureClass: in.FailureClass, ErrorMessage: in.FailureReason,
		SanitizerStatus: model.SanitizerStatusSkipped,
		SanitizerNotes:  "failure classification row; reason is worker-supplied and must be sanitized upstream",
		CompletedBy:     runtimeID,
	}
	if err := result.Validate(); err != nil {
		return model.Task{}, false, err
	}
	updated, err := u.store.WriteTerminal(ctx, taskID, tenantID, model.TaskStateFailed, closed, result, now)
	if err != nil {
		return model.Task{}, false, err
	}
	return updated, false, nil
}

// Cancel 是操作员取消：任意非终态 → cancelled。认领者不匹配不拦——
// 取消是控制面权力，不是认领者权力；但代际落后仍报 ErrClaimLost，
// 让调用方知道它在取消一个已被接管的任务。
func (u *Usecase) Cancel(ctx context.Context, taskID, tenantID, operator string) (model.Task, error) {
	now := u.clock.Now()
	task, err := u.store.GetTask(ctx, taskID, tenantID)
	if err != nil {
		return model.Task{}, err
	}
	if model.IsTerminal(task.State) {
		return task, model.ErrTerminalState
	}
	_ = operator // v1 只入审计行，不落任务行
	attemptNo := 1
	if _, cerr := u.store.GetClaim(ctx, taskID, tenantID); cerr == nil {
		if latest, aerr := u.store.LatestAttemptNumber(ctx, taskID, tenantID); aerr == nil && latest > 0 {
			attemptNo = latest
		}
		attempt, aerr := u.store.GetAttempt(ctx, taskID, tenantID, attemptNo)
		if aerr == nil {
			closed := attempt
			closed.EndedAt = &now
			closed.Outcome = model.AttemptOutcomeCancelled
			result := model.TaskResult{
				TaskID: taskID, AttemptNumber: attempt.AttemptNumber, TenantID: tenantID,
				Status: model.TaskStateCancelled, EndedAt: now,
				SanitizerStatus: model.SanitizerStatusSkipped,
				SanitizerNotes:  "operator cancellation; no worker output involved",
			}
			_, werr := u.store.WriteTerminal(ctx, taskID, tenantID, model.TaskStateCancelled, closed, result, now)
			if werr != nil {
				return model.Task{}, werr
			}
			return u.store.GetTask(ctx, taskID, tenantID)
		}
	}
	// 未认领（queued/admitted）：无 attempt 行，直接条件迁移。
	return u.store.TransitionTask(ctx, taskID, tenantID, model.TaskStateCancelled, now)
}

// ---- 陈旧恢复（§2.3 stale-recovery）----

// RecoverStale 扫描存活租约已过期的认领行，按任务安全级别与失败分类
// 决定接管（回 admitted）或终态失败。幂等：可重复调，已处理的行自然
// 不再匹配过期条件。
func (u *Usecase) RecoverStale(ctx context.Context, limit int) (RecoverResult, error) {
	now := u.clock.Now()
	res := RecoverResult{Recovered: []string{}, Terminated: []string{}}
	stale, err := u.store.ListStaleClaims(ctx, now, limit)
	if err != nil {
		return res, err
	}
	res.Scanned = len(stale)
	for _, claim := range stale {
		task, terr := u.store.GetTask(ctx, claim.TaskID, claim.TenantID)
		if terr != nil {
			continue
		}
		if model.IsTerminal(task.State) {
			continue // 终态后残留的认领行：跳过，不复活
		}
		fc := model.FailureClassLeaseExpired
		if task.State == model.TaskStateClaimed || task.State == model.TaskStatePreparing {
			// 准备租约过期：runtime_lost 语义（认领者失联）。
			fc = model.FailureClassRuntimeLost
		}
		retryable := fc.IsRetryable(task.Safety)
		if retryable && task.Safety.RequiresIdempotencyContract() {
			contract, cerr := u.store.GetIdempotencyContract(ctx, task.TaskID, task.TenantID)
			if cerr != nil {
				retryable = false
			} else if !contract.Matches(model.RecomputeFingerprint(contract.PayloadDigest, contract.TargetSystem, contract.Scope)) {
				retryable = false
			}
		}
		latest, aerr := u.store.LatestAttemptNumber(ctx, task.TaskID, claim.TenantID)
		if aerr != nil {
			latest = 0
		}
		attempt, gerr := u.store.GetAttempt(ctx, task.TaskID, claim.TenantID, latest)
		if gerr == nil {
			attempt.EndedAt = &now
			attempt.Outcome = model.AttemptOutcomeSuperseded
		} else {
			attempt = model.AttemptRecord{
				TaskID: task.TaskID, AttemptNumber: 1, TenantID: claim.TenantID,
				MaxAttempts: 1, StartedAt: claim.ClaimedAt, EndedAt: &now,
				Outcome: model.AttemptOutcomeSuperseded, RuntimeID: claim.ClaimedBy,
			}
		}
		if retryable && attempt.HasRetryBudget() {
			if _, err := u.store.SupersedeAndRequeue(ctx, claim, attempt, now); err == nil {
				res.Recovered = append(res.Recovered, task.TaskID)
				continue
			}
		}
		result := model.TaskResult{
			TaskID: task.TaskID, AttemptNumber: attempt.AttemptNumber, TenantID: claim.TenantID,
			Status: model.TaskStateFailed, EndedAt: now,
			FailureClass: fc, ErrorMessage: "claim lease expired; recovery classified the attempt",
			SanitizerStatus: model.SanitizerStatusSkipped,
			SanitizerNotes:  "control-plane recovery classification; no worker output",
		}
		if _, err := u.store.WriteTerminal(ctx, task.TaskID, claim.TenantID, model.TaskStateFailed, attempt, result, now); err == nil {
			res.Terminated = append(res.Terminated, task.TaskID)
		}
	}
	return res, nil
}

// claimBelongsTo 是认领校验的单一裁决点：runtime 身份与代际都必须匹配。
func claimBelongsTo(claim model.Claim, runtimeID string, generation int64) bool {
	return claim.BelongsToRuntime(runtimeID) && claim.Generation == generation
}

// ---- 只读查询（server 层的 list/get 视图入口）----

// ListTasks 按租户列任务（state 为空 = 全部；limit/offset 由存储层夹紧）。
func (u *Usecase) ListTasks(ctx context.Context, tenantID string, state model.TaskState, limit, offset int) ([]model.Task, error) {
	return u.store.ListTasks(ctx, tenantID, state, limit, offset)
}

// GetTask 读回单个任务（404 语义由 errs.ErrNotFound 承载）。
func (u *Usecase) GetTask(ctx context.Context, taskID, tenantID string) (model.Task, error) {
	return u.store.GetTask(ctx, taskID, tenantID)
}

// GetClaim 读回任务当前认领行（无认领时 errs.ErrNotFound，调用方按
// “未认领”投影，不算错误）。
func (u *Usecase) GetClaim(ctx context.Context, taskID, tenantID string) (model.Claim, error) {
	return u.store.GetClaim(ctx, taskID, tenantID)
}

// LatestAttemptNumber 是 worker 循环的恢复指针重建入口（§2.4，ADR
// D-6）：跨尝试续跑要读「上一尝试」的检查点，先取最新尝试序号再查
// 检查点行。无尝试返回 0。
func (u *Usecase) LatestAttemptNumber(ctx context.Context, taskID, tenantID string) (int, error) {
	return u.store.LatestAttemptNumber(ctx, taskID, tenantID)
}

// ListCheckpoints 按 (task_id, attempt_number) 列检查点行，Sequence
// 升序。worker 循环用它取上一尝试的 ResumeHint（代际过滤在调用侧，
// Checkpoint.IsForCurrentClaim）。
func (u *Usecase) ListCheckpoints(ctx context.Context, taskID, tenantID string, attemptNumber int) ([]model.Checkpoint, error) {
	return u.store.ListCheckpoints(ctx, taskID, tenantID, attemptNumber)
}
