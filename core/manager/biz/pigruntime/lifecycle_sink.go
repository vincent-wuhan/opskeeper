// lifecycle_sink.go 把 ProcessDriver 归一出的 LifecycleEvent 流落进
// OpsKeeper 的运行时注册表与任务终态记录（pig-runtime-adapter 任务 2.2）。
//
// 映射决策（ADR 段，见 openspec/changes/pig-runtime-adapter/design.md 与
// tasks.md 2.2 注记）：PiG 生命周期事件 → RuntimeRecord 走**进程内直写
// Store 端口**，不走 `POST /v1/runtimes` 自注册。理由：
//
//  1. PiG 是 Manager 自己启动、自己持有 stdin/stdout 管道的子进程，不是
//     部署在别处的远端执行面。注册表四条 HTTP 路由的服务对象是「注册方
//     与 Manager 不在同一信任域」的执行面（edge agent、preview-pg）；
//     PiG 的注册事实（RuntimeID/Kind/digest）由 Manager 在 Launch 前就
//     全部知晓——经 HTTP 自注册只会把进程内事实降级为一份网络声明，还要
//     为子进程铸造 admin 凭据，凭据本身就成了新的攻击面。
//  2. 归一层写的值（digest、并发额度）来自 `NewProcessDriver` 已校验的
//     descriptor/config，不来自子进程自报；HTTP 注册接受自报值并做漂移
//     降级，对 PiG 是错误的方向——Manager 侧事实优先。
//  3. 观察是单向的：归一层只消费驱动事件流并写注册表/任务结果，不从
//     注册表读回任何决策，不产生第二份权威。
//
// HTTP 注册路径保持给真远端 runtime；本文件不注册新路由。
package pigruntime

import (
	"errors"
	"fmt"

	model "github.com/vincent-wuhan/opskeeper/core/manager/model/runtimemgmt"
)

// ErrRuntimeRecordMissing 表示 GetRuntime 命中的是「行不存在」。装配根
// 的 gorm 适配方把 gorm.ErrRecordNotFound 归一成它；归一层在该信号上走
// 首次注册分支，其余错误原样上抛。
var ErrRuntimeRecordMissing = errors.New("pigruntime: runtime record not found")

// RuntimeMgmtStore 是归一层需要的最窄持久化端口。装配根用 gorm 适配
// （与 server/runtimemgmt.Store 同一张表），测试用内存 fake；端口放这里
// 而不放 data 层，遵循「接口由消费方声明」的既有边界（决策 283 同型）。
type RuntimeMgmtStore interface {
	// GetRuntime 按 RuntimeID 取注册行；不存在时返回 ErrRuntimeRecordMissing。
	GetRuntime(runtimeID string) (model.RuntimeRecord, error)
	// UpsertRuntime 幂等注册/整行写回。
	UpsertRuntime(rec model.RuntimeRecord) error
	// SaveTerminalResult 持久化任务终态（runtimemgmt_task_results）。
	SaveTerminalResult(result model.TaskResult) error
}

// LifecycleSink 把一个 PiG RuntimeDescriptor 钉住的事件流归一为注册表与
// 任务终态写入。零 goroutine：ApplyEvent 由驱动的消费方（装配根的观察
// 循环或测试）同步调用，写入顺序即事件顺序。
type LifecycleSink struct {
	store      RuntimeMgmtStore
	descriptor RuntimeDescriptor
}

// NewLifecycleSink 构造。descriptor 必须是 ProcessDriver 的固定身份——
// 归一层写的注册行（digest、并发额度、Kind=pig）全部取自它。写库时戳
// 取事件 OccurredAt（驱动已归一为 UTC），不引入第二时钟。
func NewLifecycleSink(store RuntimeMgmtStore, descriptor RuntimeDescriptor) (*LifecycleSink, error) {
	if store == nil {
		return nil, ErrInvalidContract
	}
	if descriptor.Kind != RuntimeKindPiG {
		return nil, ErrInvalidContract
	}
	if err := descriptor.Validate(); err != nil {
		return nil, err
	}
	return &LifecycleSink{store: store, descriptor: descriptor}, nil
}

// SinkWrite 是 ApplyEvent 一次调用产生的持久化观察，测试与调用方日志用。
type SinkWrite struct {
	RuntimeTouched bool // 注册或心跳写落到 runtimemgmt_runtimes
	Heartbeat      bool // 本次写刷新了 LastHeartbeatAt
	TaskResult     bool // 终态结果写落到 runtimemgmt_task_results
}

// ErrTerminalRejected 表示终态事件的脱敏裁决无法保守映射，结果被拒绝
// 持久化（fail closed，与 model.TaskResult.Validate 同一立场）。
var ErrTerminalRejected = errors.New("pigruntime: terminal result rejected by sanitizer gate")

// ApplyEvent 归一一条生命周期事件。
//
//	registered → 注册/更新 RuntimeRecord（digest 来自 descriptor，版本
//	           来源 release_metadata——Manager 钉住的工件摘要即发布事实）
//	heartbeat  → 刷新 LastHeartbeatAt，容量取事件 Health 的活跃数
//	progress   → 只校验，不写库（进度属于事件流，不是注册表事实）
//	cancelled / failed / completed → 写 TaskResult 终态
//
// 终态脱敏映射（保守方向）：
//
//	Sanitized=true                    → applied
//	Sanitized=false, SkippedSanitizer → skipped（notes 必填，缺理由即拒绝）
//	两者皆非                           → 拒绝持久化（TerminalResult.Validate
//	                                  已在驱动侧拒绝；此处对绕过驱动的
//	                                  输入再拦一次，双闸不互信）
func (s *LifecycleSink) ApplyEvent(event LifecycleEvent) (SinkWrite, error) {
	if err := event.Validate(); err != nil {
		return SinkWrite{}, err
	}
	if event.RuntimeID != s.descriptor.RuntimeID || event.TenantID == "" || event.TaskID == "" {
		return SinkWrite{}, ErrInvalidContract
	}
	switch event.Type {
	case LifecycleEventRegistered:
		return s.applyRegistration(event)
	case LifecycleEventHeartbeat:
		return s.applyHeartbeat(event)
	case LifecycleEventProgress:
		return SinkWrite{}, nil
	case LifecycleEventCancelled, LifecycleEventFailed, LifecycleEventCompleted:
		return s.applyTerminal(event)
	default:
		return SinkWrite{}, ErrInvalidContract
	}
}

func (s *LifecycleSink) applyRegistration(event LifecycleEvent) (SinkWrite, error) {
	rec, err := s.store.GetRuntime(s.descriptor.RuntimeID)
	if err != nil && !errors.Is(err, ErrRuntimeRecordMissing) {
		return SinkWrite{}, fmt.Errorf("pigruntime: read runtime record: %w", err)
	}
	if err != nil {
		rec = model.RuntimeRecord{
			RuntimeID:              s.descriptor.RuntimeID,
			Kind:                   model.RuntimeKindPiG,
			RegisteredAt:           event.OccurredAt.UTC(),
			Health:                 model.RuntimeHealthStarting,
			PendingCommandAcksJSON: "{}",
			LabelsJSON:             "{}",
		}
	}
	// 注册行全部读 Manager 侧钉住的事实，不读子进程自报值：digest 来自
	// NewProcessDriver 校验过的 descriptor，版本来源因此是 release_metadata。
	rec.Kind = model.RuntimeKindPiG
	rec.Owner = "manager"
	rec.DeploymentLocation = "subprocess"
	rec.ObservedVersion = s.descriptor.Version
	rec.VersionSource = model.VersionSourceReleaseMetadata
	rec.ArtifactDigest = s.descriptor.ReleaseDigest
	rec.CapabilityDigest = s.descriptor.CapabilityDigest
	rec.TotalSlots = s.descriptor.MaxConcurrency
	rec.UpdatedAt = event.OccurredAt.UTC()
	// 注册事件本身即一次存活观察：心跳时戳落在事件时刻，EvaluateHealth
	// 由此把 starting 推进到 online/degraded，而不是等第一条心跳。
	at := event.OccurredAt.UTC()
	rec.LastHeartbeatAt = &at
	if rec.Health == model.RuntimeHealthStarting {
		rec.Health = model.RuntimeHealthOnline
	}
	if err := s.store.UpsertRuntime(rec); err != nil {
		return SinkWrite{}, fmt.Errorf("pigruntime: upsert runtime record: %w", err)
	}
	return SinkWrite{RuntimeTouched: true, Heartbeat: true}, nil
}

func (s *LifecycleSink) applyHeartbeat(event LifecycleEvent) (SinkWrite, error) {
	rec, err := s.store.GetRuntime(s.descriptor.RuntimeID)
	if err != nil {
		// 乱序：心跳先于注册到达。进程内归一层按事件顺序消费，正常不
		// 可能出现；出现即上游丢了注册观察，拒绝比自造一行更保守。
		return SinkWrite{}, fmt.Errorf("pigruntime: heartbeat before registration: %w", err)
	}
	at := event.OccurredAt.UTC()
	if rec.LastHeartbeatAt != nil && at.Before(*rec.LastHeartbeatAt) {
		return SinkWrite{}, fmt.Errorf("%w: heartbeat %v regressed before %v",
			ErrEventStreamInvalid, at, rec.LastHeartbeatAt)
	}
	rec.LastHeartbeatAt = &at
	if event.Health != nil {
		rec.ActiveTasks = event.Health.ActiveExecutions
		if event.Health.HeartbeatOverdue && rec.Health != model.RuntimeHealthDegraded {
			rec.Health = model.RuntimeHealthDegraded
			rec.HealthReason = "pig heartbeat overdue: " + event.Health.Reason
		} else if !event.Health.HeartbeatOverdue && rec.HealthReason != "" {
			rec.Health = model.RuntimeHealthOnline
			rec.HealthReason = ""
		}
	}
	rec.UpdatedAt = at
	if err := s.store.UpsertRuntime(rec); err != nil {
		return SinkWrite{}, fmt.Errorf("pigruntime: save runtime heartbeat: %w", err)
	}
	return SinkWrite{RuntimeTouched: true, Heartbeat: true}, nil
}

func (s *LifecycleSink) applyTerminal(event LifecycleEvent) (SinkWrite, error) {
	if event.Terminal == nil {
		return SinkWrite{}, ErrTerminalRequired
	}
	status, notes, err := sanitizerDecision(*event.Terminal)
	if err != nil {
		return SinkWrite{}, fmt.Errorf("%w: %v", ErrTerminalRejected, err)
	}
	result := model.TaskResult{
		TaskID:          event.TaskID,
		AttemptNumber:   1,
		TenantID:        event.TenantID,
		Status:          terminalStateOf(event.Terminal.State),
		EndedAt:         event.Terminal.CompletedAt.UTC(),
		ErrorMessage:    event.Terminal.FailureReason,
		OutputRef:       event.Terminal.OutputRef,
		OutputDigest:    event.Terminal.OutputDigest,
		SanitizerStatus: status,
		SanitizerNotes:  notes,
		CompletedBy:     s.descriptor.RuntimeID,
	}
	if result.Status == model.TaskStateFailed {
		result.FailureClass = model.FailureClassWorkerError
	}
	if err := result.Validate(); err != nil {
		return SinkWrite{}, fmt.Errorf("%w: %v", ErrTerminalRejected, err)
	}
	if err := s.store.SaveTerminalResult(result); err != nil {
		return SinkWrite{}, fmt.Errorf("pigruntime: save terminal result: %w", err)
	}
	return SinkWrite{TaskResult: true}, nil
}

// sanitizerDecision 把驱动 TerminalResult 的脱敏布尔对保守映射为
// model.SanitizerStatus 闭集。三态映射（issue 2.2 要求）：
//
//	Sanitized=true                    → applied
//	Sanitized=false, SkippedSanitizer=true → skipped，notes 必填（缺省给
//	                                  固定理由：跳过必须留痕，无痕跳过即拒绝）
//	两者皆非                           → 拒绝（fail closed）
//
// 「failed」态（脱敏流水线自身出错）在驱动契约里没有对应布尔位——
// ProcessDriver 的兜底终态自带 Sanitized=true，驱动不会产出 failed 语义；
// 该态留给未来脱敏中间件直连时使用，本层不猜测产出它。
func sanitizerDecision(terminal TerminalResult) (model.SanitizerStatus, string, error) {
	switch {
	case terminal.Sanitized && terminal.SkippedSanitizer:
		return "", "", errors.New("sanitized and skipped are mutually exclusive")
	case terminal.Sanitized:
		return model.SanitizerStatusApplied, "", nil
	case terminal.SkippedSanitizer:
		return model.SanitizerStatusSkipped, "pig terminal result declared sanitizer skip by pinned release", nil
	default:
		return "", "", errors.New("terminal result carries no sanitizer decision")
	}
}

func terminalStateOf(state TerminalState) model.TaskState {
	switch state {
	case TerminalSucceeded:
		return model.TaskStateSucceeded
	case TerminalCancelled:
		return model.TaskStateCancelled
	default:
		return model.TaskStateFailed
	}
}
