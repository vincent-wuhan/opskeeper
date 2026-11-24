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
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	model "github.com/vincent-wuhan/opskeeper/core/manager/model/runtimemgmt"
)

// heartbeatOverdueReason 是心跳超期降级理由的固定前缀，恢复逻辑以它
// 区分「心跳类理由」与「能力 drift 理由」——只有前者随健康心跳清除。
const heartbeatOverdueReason = "pig heartbeat overdue: "

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
//
// 能力对账（任务 2.3）从同一入口落：注册/心跳事件携带的 CapabilityObservation
// 经 ReconcileCapabilities 对账后，暴露集写进注册行 CapabilitiesJSON，观察侧
// drift 并入 HealthReason（degraded / rejected）。不另开第二条健康写路径。
type LifecycleSink struct {
	store      RuntimeMgmtStore
	descriptor RuntimeDescriptor
	admission  AdmissionConfig
}

// AdmissionConfig 是归一层对账所需的 Manager 侧事实：发布清单（declared）
// 与角色准入表（allowlisted）。两者都由装配根钉住，不从子进程自报读。
type AdmissionConfig struct {
	Manifest CapabilityManifest
	Roles    []RoleAllowlist
	Policy   AdmissionPolicy
}

// NewLifecycleSink 构造。descriptor 必须是 ProcessDriver 的固定身份——
// 归一层写的注册行（digest、并发额度、Kind=pig）全部取自它。写库时戳
// 取事件 OccurredAt（驱动已归一为 UTC），不引入第二时钟。
func NewLifecycleSink(store RuntimeMgmtStore, descriptor RuntimeDescriptor) (*LifecycleSink, error) {
	return NewLifecycleSinkWithAdmission(store, descriptor, AdmissionConfig{})
}

// NewLifecycleSinkWithAdmission 构造带能力对账的归一层。manifest 非空时
// 必须与 descriptor.CapabilityDigest 同字节（ComputeCapabilityDigest）——
// 对账的 declared 侧就是注册行写的能力摘要，两者不一致即装配错误。
// 角色准入表按 Role 唯一；同一角色两张表是装配错误。
func NewLifecycleSinkWithAdmission(
	store RuntimeMgmtStore,
	descriptor RuntimeDescriptor,
	admission AdmissionConfig,
) (*LifecycleSink, error) {
	if store == nil {
		return nil, ErrInvalidContract
	}
	if descriptor.Kind != RuntimeKindPiG {
		return nil, ErrInvalidContract
	}
	if err := descriptor.Validate(); err != nil {
		return nil, err
	}
	if len(admission.Manifest.Tools) > 0 {
		if err := admission.Manifest.Validate(); err != nil {
			return nil, err
		}
		if ComputeCapabilityDigest(admission.Manifest) != descriptor.CapabilityDigest {
			return nil, ErrInvalidContract
		}
	}
	roles := make(map[string]struct{}, len(admission.Roles))
	for _, role := range admission.Roles {
		if err := role.Validate(); err != nil {
			return nil, err
		}
		if _, exists := roles[role.Role]; exists {
			return nil, ErrInvalidContract
		}
		roles[role.Role] = struct{}{}
	}
	return &LifecycleSink{store: store, descriptor: descriptor, admission: admission}, nil
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

// ErrAdmissionRejected 表示能力对账给出 Rejected 裁决（未声明副作用类
// 或观察身份冲突）：注册行已记 degraded，后续任务准入必须 fail closed。
var ErrAdmissionRejected = errors.New("pigruntime: capability admission rejected")

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
	admissionErr := s.applyCapabilityDrift(&rec, event.Capabilities, event.OccurredAt)
	if err := s.store.UpsertRuntime(rec); err != nil {
		return SinkWrite{}, fmt.Errorf("pigruntime: upsert runtime record: %w", err)
	}
	if admissionErr != nil {
		return SinkWrite{RuntimeTouched: true, Heartbeat: true}, admissionErr
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
			rec.HealthReason = heartbeatOverdueReason + event.Health.Reason
		} else if !event.Health.HeartbeatOverdue && rec.HealthReason != "" {
			// 恢复只清心跳类理由。能力 drift 是持续性异常——进程每次
			// 注册/心跳都在重报未声明工具，一次健康心跳抹不掉它；drift
			// 的清除只能由带 Capabilities 的事件重算对账（干净观察会
			// 走 applyCapabilityDrift 的 clean 分支重写）或人工介入。
			if strings.HasPrefix(rec.HealthReason, heartbeatOverdueReason) {
				rec.Health = model.RuntimeHealthOnline
				rec.HealthReason = ""
			}
		}
	}
	rec.UpdatedAt = at
	admissionErr := s.applyCapabilityDrift(&rec, event.Capabilities, event.OccurredAt)
	if err := s.store.UpsertRuntime(rec); err != nil {
		return SinkWrite{}, fmt.Errorf("pigruntime: save runtime heartbeat: %w", err)
	}
	if admissionErr != nil {
		return SinkWrite{RuntimeTouched: true, Heartbeat: true}, admissionErr
	}
	return SinkWrite{RuntimeTouched: true, Heartbeat: true}, nil
}

// applyCapabilityDrift 对账事件携带的能力观察并把结果并入注册行。调用
// 点是注册与心跳的同一条 UpsertRuntime 路径——不开第二条健康写路径
// （开工注记 1 的要求）。sink 保持无状态：观察来自事件的
// Capabilities 字段，不在归一层累积跨事件状态。
//
// 处置按 2026-11-21 默认裁决：clean → 只更新暴露集；degraded → 注册行
// 记 degraded + 理由；rejected → 注册行记 degraded + 理由，并返回
// ErrAdmissionRejected 让消费方把后续任务准入 fail closed——不当场杀
// 已启动的执行：未声明工具本来就不在暴露集里，观察是证据不是授权。
func (s *LifecycleSink) applyCapabilityDrift(
	rec *model.RuntimeRecord,
	observed []CapabilityObservation,
	occurredAt time.Time,
) error {
	// 只在事件携带观察时对账：心跳通常不带 Capabilities，把「没带」
	// 当「观察为空」会每次心跳都重写一遍 unobserved 证据。
	if len(s.admission.Manifest.Tools) == 0 || len(s.admission.Roles) == 0 ||
		len(observed) == 0 {
		return nil
	}
	worst := VerdictClean
	capabilitiesJSON := ""
	for _, role := range s.admission.Roles {
		result, err := ReconcileCapabilities(
			s.admission.Manifest, observed, role, s.admission.Policy)
		if err != nil {
			return err
		}
		if verdictRank(result.Verdict) > verdictRank(worst) {
			worst = result.Verdict
		}
		// 多角色时暴露集取并集——首切片单角色是常态；per-role 暴露面
		// 的持久化形态属于 3.x 网关表族，本切片先钉最危险裁决与证据。
		capabilitiesJSON = mergeExposedJSON(capabilitiesJSON, result.Exposed)
	}
	if capabilitiesJSON != "" {
		rec.CapabilitiesJSON = capabilitiesJSON
	}
	if worst == VerdictClean {
		// 干净观察负责清除 drift 降级（心跳不再误清，见 applyHeartbeat）。
		// 只清 drift 理由；心跳超期理由不是本对账的职责。
		if strings.HasPrefix(rec.HealthReason, capabilityDriftReasonPrefix) &&
			rec.Health == model.RuntimeHealthDegraded {
			rec.Health = model.RuntimeHealthOnline
			rec.HealthReason = ""
			rec.UpdatedAt = occurredAt.UTC()
		}
		return nil
	}
	rec.Health = model.RuntimeHealthDegraded
	rec.HealthReason = s.driftReason(worst, observed)
	rec.UpdatedAt = occurredAt.UTC()
	if worst == VerdictRejected {
		return ErrAdmissionRejected
	}
	return nil
}

// driftReason 重算一次只含最危险角色的 HealthReason 文案。走重算而非
// 缓存，保持无状态；角色数是配置常量，成本可忽略。
func (s *LifecycleSink) driftReason(worst DriftVerdict, observed []CapabilityObservation) string {
	var reason string
	for _, role := range s.admission.Roles {
		result, err := ReconcileCapabilities(
			s.admission.Manifest, observed, role, s.admission.Policy)
		if err != nil || verdictRank(result.Verdict) < verdictRank(worst) {
			continue
		}
		if result.HealthReason != "" {
			reason = result.HealthReason
		}
	}
	return reason
}

// verdictRank 把裁决闭集排成危险单调序：clean < degraded < rejected。
func verdictRank(verdict DriftVerdict) int {
	switch verdict {
	case VerdictClean:
		return 0
	case VerdictDegraded:
		return 1
	default:
		return 2
	}
}

// mergeExposedJSON 把多角色的暴露集并成一个 JSON 对象写进注册行
// CapabilitiesJSON。键是 StableToolID，值是统一 domain 拼写的类别——
// 与 server/runtimemgmt 读回路径的 map[string]any 形状兼容。序列化
// 失败归一为空串（capabilitiesJSON 校验拒绝落库），不因该列失败。
func mergeExposedJSON(current string, exposed []ExposedTool) string {
	merged := map[string]any{}
	if current != "" {
		_ = json.Unmarshal([]byte(current), &merged)
	}
	for _, tool := range exposed {
		merged[tool.StableToolID] = string(tool.Class)
	}
	if len(merged) == 0 {
		return "{}"
	}
	encoded, err := json.Marshal(merged)
	if err != nil {
		return "{}"
	}
	return string(encoded)
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
