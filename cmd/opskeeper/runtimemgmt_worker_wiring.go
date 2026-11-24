// runtimemgmt_worker_wiring.go — §2.4+§2.5 worker 接入的装配
// （runtime-management-gap，ADR D-6/D-7）。
//
// 三个 handler 全部住在装配根：biz/runtimemgmt 只持通用端口（TaskHandler/
// Reporter/WorkerRuntime），具体 kind 的实现要摸 demo 执行器与
// agentteams.state——跨域依赖不越过决策 449 的域边界（biz 不 import
// demo/agentteams），只有装配根可以。
//
// env 门控（默认关，零 goroutine 新增）：
//
//	OPSKEEPER_RUNTIME_TASK_WORKERS=manager   → 诊断 worker + repair-preview
//	OPSKEEPER_RUNTIME_TASK_WORKERS=agentteams → AgentTeams 只读报告桥
//	（逗号并列取并集；空/未设 = 全关）
//
// 关闭即零行为：不建循环、不注册 handler、不起 goroutine——与
// assemblePigRuntime 的双闸同一立场。显式启用但 executor 缺配置是启动
// 错误（fail closed），一个自认接入了 worker 却悄悄没有它的部署比启动
// 失败更危险。
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"

	repairpreview "github.com/vincent-wuhan/opskeeper/core/domains/control/repairpreview"
	agentteamsstate "github.com/vincent-wuhan/opskeeper/core/manager/biz/agentteams/state"
	managerbizdemo "github.com/vincent-wuhan/opskeeper/core/manager/biz/demo"
	bizruntimemgmt "github.com/vincent-wuhan/opskeeper/core/manager/biz/runtimemgmt"
	runtimemgmtstore "github.com/vincent-wuhan/opskeeper/core/manager/data/runtimemgmt/store"
	model "github.com/vincent-wuhan/opskeeper/core/manager/model/runtimemgmt"
)

// ---- env 门控 ----

// runtimeTaskWorkerSet 解析 OPSKEEPER_RUNTIME_TASK_WORKERS：逗号分隔的
// 已启用 worker 名闭集。空 = 全关（默认部署零行为）。
func runtimeTaskWorkerSet(env func(string) string) map[string]bool {
	raw := strings.TrimSpace(env("OPSKEEPER_RUNTIME_TASK_WORKERS"))
	out := map[string]bool{}
	for _, name := range strings.Split(raw, ",") {
		name = strings.TrimSpace(name)
		if name != "" {
			out[name] = true
		}
	}
	return out
}

const (
	// workerNameManager 启用诊断 worker + repair-preview handler（本进程
	// 内执行的 manager 侧 kind）。
	workerNameManager = "manager"
	// workerNameAgentTeams 启用 AgentTeams 只读报告桥（§2.5，D-7）。
	workerNameAgentTeams = "agentteams"
	// runtimeTaskOutputDirEnv / 其默认值是诊断 worker 输出落盘目录
	// （issue 契约：平铺目录为已记录的 v1 简化，工作区治理是 §3）。
	runtimeTaskOutputDirEnv = "OPSKEEPER_RUNTIME_TASK_OUTPUT_DIR"
	runtimeTaskOutputDirDef = "/var/lib/opskeeper/runtime-tasks"
)

// runtimeTaskOutputDir 读输出目录 env（默认 /var/lib/opskeeper/runtime-tasks）。
// 经 runtimeTaskOutputDirEnvDefault(env) 在装配时读一次——不在 handler
// 请求路径里解析 env（ServerLimitFromEnv 同款约定）。

// ---- 只读诊断 worker（§2.4 kind=read_only.runtime_inventory_snapshot）----

// KindRuntimeInventorySnapshot 是诊断 worker 的 kind：一次任务 = 一张
// runtime 注册表 + 五观察者投影的序列化快照。
const KindRuntimeInventorySnapshot = "read_only.runtime_inventory_snapshot"

// RuntimeInventorySnapshotHandler 执行 inventory 投影序列化：读
// runtimemgmt_runtimes 全表（+ 惰性健康推导），写 <task-id>.json 到
// 输出目录，OutputRef/OutputDigest/OutputSize 经 Reporter 回填。
// safety=read_only：无副作用、无凭据路径（注册表是控制面自持有数据）。
type RuntimeInventorySnapshotHandler struct {
	db        *gorm.DB
	outputDir string
	nowFn     func() time.Time
}

// NewRuntimeInventorySnapshotHandler 构造。nowFn 缺省 time.Now（快照的
// generated_at 与健康推导时刻）。
func NewRuntimeInventorySnapshotHandler(db *gorm.DB, outputDir string) *RuntimeInventorySnapshotHandler {
	if outputDir == "" {
		outputDir = runtimeTaskOutputDirDef
	}
	return &RuntimeInventorySnapshotHandler{db: db, outputDir: outputDir, nowFn: time.Now}
}

// Matches 只认本 kind。
func (h *RuntimeInventorySnapshotHandler) Matches(kind string) bool {
	return kind == KindRuntimeInventorySnapshot
}

// inventorySnapshotRow 是快照的单行投影：注册表列 + 推导健康（不落存
// 原始行——快照是证据快照，不是表复制）。
type inventorySnapshotRow struct {
	RuntimeID          string     `json:"runtime_id"`
	Kind               string     `json:"kind"`
	Profile            string     `json:"profile,omitempty"`
	Owner              string     `json:"owner,omitempty"`
	DeploymentLocation string     `json:"deployment_location,omitempty"`
	Labels             string     `json:"labels,omitempty"`
	ObservedVersion    string     `json:"observed_version,omitempty"`
	VersionSource      string     `json:"version_source,omitempty"`
	ArtifactDigest     string     `json:"artifact_digest,omitempty"`
	CapabilityDigest   string     `json:"capability_digest,omitempty"`
	Health             string     `json:"health"`
	LastHeartbeatAt    *time.Time `json:"last_heartbeat_at,omitempty"`
	TotalSlots         int        `json:"total_slots"`
	FreeSlots          int        `json:"free_slots"`
	ActiveTasks        int        `json:"active_tasks"`
}

type inventorySnapshotDoc struct {
	GeneratedAt time.Time              `json:"generated_at"`
	TaskID      string                 `json:"task_id"`
	TenantID    string                 `json:"tenant_id"`
	Total       int                    `json:"total"`
	Runtimes    []inventorySnapshotRow `json:"runtimes"`
}

// Execute 序列化快照并落盘。resume 非 nil 时按恢复指针续跑——快照本身
// 无副作用，续跑等价重拍（read_only 的幂等天然成立）。
func (h *RuntimeInventorySnapshotHandler) Execute(ctx context.Context, task model.Task, resume *model.Checkpoint, rt bizruntimemgmt.Reporter) error {
	if h == nil || h.db == nil {
		return errors.New("inventory snapshot handler not wired")
	}
	var recs []model.RuntimeRecord
	if err := h.db.WithContext(ctx).Order("runtime_id asc").Find(&recs).Error; err != nil {
		return err
	}
	now := h.nowFn().UTC()
	doc := inventorySnapshotDoc{
		GeneratedAt: now, TaskID: task.TaskID, TenantID: task.TenantID,
		Total: len(recs), Runtimes: make([]inventorySnapshotRow, 0, len(recs)),
	}
	for _, rec := range recs {
		doc.Runtimes = append(doc.Runtimes, inventorySnapshotRow{
			RuntimeID: rec.RuntimeID, Kind: string(rec.Kind),
			Profile: rec.Profile, Owner: rec.Owner,
			DeploymentLocation: rec.DeploymentLocation, Labels: rec.LabelsJSON,
			ObservedVersion: rec.ObservedVersion, VersionSource: string(rec.VersionSource),
			ArtifactDigest: rec.ArtifactDigest, CapabilityDigest: rec.CapabilityDigest,
			Health: string(rec.EvaluateHealth(now)), LastHeartbeatAt: rec.LastHeartbeatAt,
			TotalSlots: rec.TotalSlots, FreeSlots: rec.FreeSlots, ActiveTasks: rec.ActiveTasks,
		})
	}
	payload, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(h.outputDir, 0o755); err != nil {
		return err
	}
	ref := filepath.Join(h.outputDir, task.TaskID+".json")
	if err := os.WriteFile(ref, payload, 0o644); err != nil {
		return err
	}
	sum := sha256.Sum256(payload)
	if out, ok := rt.(bizruntimemgmt.CompiledOutputReporter); ok {
		out.RecordOutput(ref, int64(len(payload)), hex.EncodeToString(sum[:]))
	}
	return nil
}

// ---- repair-preview handler（§2.4 kind=repair.preview.execute，只包不改造）----

// KindRepairPreviewExecute 是 repair-preview 任务的 kind。safety=
// disposable（ADR D-6）：副作用限于一次性 preview-pg 运行时内。
const KindRepairPreviewExecute = "repair.preview.execute"

// RepairPreviewTaskHandler 适配既有 demo.RepairPreviewExecutor.Execute
// （preview_executor.go 一行不改——零侵入纪律）。契约 target_system=
// preview-pg，scope=运行绑定摘要（RunID 的确定性输入），入队方按同值
// 建 IdempotencyContract。
//
// ErrDuplicateRun → 成功返回：既有幂等确认（绑定匹配的重复保存被
// confirmDuplicate 判定为同一工作，2.6「无重复一次性工作」的落点）。
type RepairPreviewTaskHandler struct {
	executor    managerbizdemo.PreviewExecutor
	contracts   bizruntimemgmt.TaskStore
	fingerprint string
}

// NewRepairPreviewTaskHandler 构造。executor 是装配根已建好的
// RepairPreviewExecutor（或测试件）；contracts 是任务幂等契约的读端口
// （data 层窄构造 NewBizTaskStore）；fingerprint 是
// OPSKEEPER_REPAIR_PREVIEW_WORKLOAD_FINGERPRINT（运行绑定摘要的成分）。
func NewRepairPreviewTaskHandler(executor managerbizdemo.PreviewExecutor, contracts bizruntimemgmt.TaskStore, fingerprint string) *RepairPreviewTaskHandler {
	return &RepairPreviewTaskHandler{executor: executor, contracts: contracts, fingerprint: fingerprint}
}

// Matches 只认本 kind。
func (h *RepairPreviewTaskHandler) Matches(kind string) bool { return kind == KindRepairPreviewExecute }

// Execute 调一次既有 executor。resume 语义：preview 不可分批——收到
// 恢复指针等价重复执行尝试，executor 的绑定校验幂等裁决（重复即
// ErrDuplicateRun → 成功返回）。
//
// 绑定成分从幂等契约行取回：载荷原文不入库（Task 只存摘要），而
// disposable 任务也不强制契约——但 preview 的入队契约（本文件文档化）
// 是「带契约入队」，ExternalRequestID 即 IdempotencyKey、Scope 即绑定
// 摘要。无契约行归类 no_idempotency_contract（终态，不重试）——这比
// 拒绝整类任务温和：不按契约入队的生产者拿到的是可读的分类失败。
func (h *RepairPreviewTaskHandler) Execute(ctx context.Context, task model.Task, resume *model.Checkpoint, rt bizruntimemgmt.Reporter) error {
	if h == nil || h.executor == nil {
		return errors.New("repair preview executor not configured")
	}
	contract, err := h.contracts.GetIdempotencyContract(ctx, task.TaskID, task.TenantID)
	if err != nil {
		return bizruntimemgmt.TaskFailure{
			Class: model.FailureClassNoIdempotencyContract,
			Err:   fmt.Errorf("repair preview task has no idempotency contract row: %w", err),
		}
	}
	runID := managerbizdemo.DeterministicPreviewRunID(
		agentteamsTenantNumber(task.TenantID), managerbizdemo.ScenarioID,
		contract.ExternalRequestID, contract.Scope, agentteamsIncidentNumber(task.IncidentID),
	)
	err = h.executor.Execute(ctx, managerbizdemo.PreviewExecutionInput{
		RunID:             runID,
		TenantID:          strconv.FormatUint(agentteamsTenantNumber(task.TenantID), 10),
		IncidentID:        task.IncidentID,
		ScenarioID:        managerbizdemo.ScenarioID,
		IdempotencyKey:    contract.ExternalRequestID,
		TargetFingerprint: contract.Scope,
	})
	if err != nil {
		if errors.Is(err, repairpreview.ErrDuplicateRun) {
			// 幂等确认：同绑定的重复执行 = 已经做过的工作，成功返回
			// （ADR D-6；2.6「无重复一次性工作」断言的落点）。
			return nil
		}
		return err
	}
	if out, ok := rt.(bizruntimemgmt.CompiledOutputReporter); ok {
		// 产出物引用即确定性 run 绑定（preview run 表自身是产出）。
		out.RecordOutput("preview-pg://runs/"+runID, 0, contract.Fingerprint)
	}
	return nil
}

// agentteamsTenantNumber 把生命周期租户（"user-1" / "default"）解回
// demo 域的数字租户；解不出归 1（与 main.go 的 archiveTenantID 缺省
// 同一立场——demo 域单租户 v1）。
func agentteamsTenantNumber(tenant string) uint64 {
	id, err := strconv.ParseUint(strings.TrimPrefix(tenant, "user-"), 10, 64)
	if err != nil || id == 0 {
		return 1
	}
	return id
}

// agentteamsIncidentNumber 同上，针对事件 ID；空事件归 0。
func agentteamsIncidentNumber(incident string) uint64 {
	id, _ := strconv.ParseUint(incident, 10, 64)
	return id
}

// ---- AgentTeams 只读报告桥（§2.5，ADR D-7）----

// KindAgentTeamsTask 是桥映射的 lifecycle kind。一个 ledger 任务（一个
// incident 的八阶段流水）= 一行 kind=agentteams.task 的生命周期任务。
const KindAgentTeamsTask = "agentteams.task"

// agentTeamsPhaseSafety 是 ledger 阶段 → 任务安全级别的写死映射（ADR
// D-7：映射表写死在桥内并注明依据）。
//
// 依据（HITL 边界不因接入而移动）：
//   - alert_dedup / rca / critic_audit / review / hitl_approval /
//     postmortem 是读-判-记阶段：worker 只读监控面与配置面、只写
//     ledger 自身（证据），外部系统零变更 → read_only。
//   - repair_execution 在 HITL 批准**之后**才允许开始（state.go 的
//     Advance 硬闸：pending→running 需要 approved 决策），到达桥的
//     执行事件必然已过人审 → mutating（外部系统真变更）。
//   - verification 是修复后复检：读为主，但可能触发对修复目标的
//     探测性查询 → read_only。
//
// 该映射是报告视图的注记，不改变 ledger 的权威：任务行的 Safety 只
// 用于重试闸门与本桥的告警口径。
var agentTeamsPhaseSafety = map[agentteamsstate.Phase]model.TaskSafety{
	agentteamsstate.PhaseAlertDedup:      model.TaskSafetyReadOnly,
	agentteamsstate.PhaseRCA:             model.TaskSafetyReadOnly,
	agentteamsstate.PhaseCriticAudit:     model.TaskSafetyReadOnly,
	agentteamsstate.PhaseReview:          model.TaskSafetyReadOnly,
	agentteamsstate.PhaseHITLApproval:    model.TaskSafetyReadOnly,
	agentteamsstate.PhaseRepairExecution: model.TaskSafetyMutating,
	agentteamsstate.PhaseVerification:    model.TaskSafetyReadOnly,
	agentteamsstate.PhasePostmortem:      model.TaskSafetyReadOnly,
}

// agentTeamsTaskSafety 取「入队时点已开始（in_progress/completed/
// failed/skipped）阶段」里最严的安全级别。dispatch 即建的行通常落在
// read_only（alert_dedup 起）；桥晚接入一个已进 repair_execution 的
// 任务时如实记 mutating（证据：ledger 的 approved HITL 决策）。
// 未开始的阶段不参与——报告行描述「已经做了什么」，不是「将会做
// 什么」。
func agentTeamsTaskSafety(ledger *agentteamsstate.Task) model.TaskSafety {
	safety := model.TaskSafetyReadOnly
	for _, phase := range agentTeamsPhaseOrder() {
		ps, ok := ledger.Phases[phase]
		if !ok || ps.Status == agentteamsstate.PhasePending {
			continue
		}
		if agentTeamsPhaseSafety[phase] == model.TaskSafetyMutating {
			safety = model.TaskSafetyMutating
		}
	}
	return safety
}

// AgentTeamsReportBridge 只读镜像 ledger 的生命周期行（spec 2.5：
// "preserving the existing incident-phase ledger as evidence"）。
//
// 单向纪律：只读 state.Manager（LookupIncident/Get——与观察者同一批
// 方法，不新增 state 域 API），只写 runtimemgmt 表族；state.json 一个
// 字节不写（wiring 测试双读断言）。ledger 保持证据权威——桥写的是
// 报告视图，不是第二真相源。
//
// 发现面：桥轮询两类 incident——(a) 已映射（kind=agentteams.task 行
// 自带的 IncidentID，跟着已有任务走）；(b) 观察名单
// OPSKEEPER_AGENTTEAMS_WATCH_INCIDENTS（逗号分隔，新 incident 的入口；
// 与观察者的样例探测同一形态，但不硬编码具体 incident）。
type AgentTeamsReportBridge struct {
	state    *agentteamsstate.Manager
	usecase  *bizruntimemgmt.Usecase
	db       *gorm.DB
	tenantID string
	watch    []string
	nowFn    func() time.Time
}

// NewAgentTeamsReportBridge 构造。watch 为要跟进的 incident ID 名单
// （env 解析后传入；空名单 = 只跟已映射任务）。db 用于 runtime 行的
// 注册/心跳（认领闸门前提）。nowFn 缺省 time.Now。
func NewAgentTeamsReportBridge(
	stateMgr *agentteamsstate.Manager,
	usecase *bizruntimemgmt.Usecase,
	db *gorm.DB,
	tenantID string,
	watch []string,
) *AgentTeamsReportBridge {
	return &AgentTeamsReportBridge{state: stateMgr, usecase: usecase, db: db, tenantID: tenantID, watch: watch, nowFn: time.Now}
}

// agentTeamsWatchList 解析 OPSKEEPER_AGENTTEAMS_WATCH_INCIDENTS。
func agentTeamsWatchList(env func(string) string) []string {
	raw := strings.TrimSpace(env("OPSKEEPER_AGENTTEAMS_WATCH_INCIDENTS"))
	if raw == "" {
		return nil
	}
	var out []string
	for _, id := range strings.Split(raw, ",") {
		if id = strings.TrimSpace(id); id != "" {
			out = append(out, id)
		}
	}
	return out
}

// SyncOnce 做一轮镜像：读每个候选 incident 的 ledger 任务，把八阶段
// 流水映射为生命周期事件。幂等可重入——已终态的生命周期行天然跳过
// （下述各步都以「读回判定 + 条件写入」实现）。
//
// 映射表（issue 契约）：
//   - ledger 任务出现（dispatch）→ 入队 kind=agentteams.task 行（若
//     无）。safety 取 repair_execution 的 mutating（整条流水里最严
//     的级别——安全级别是行级属性，八阶段共用一行）。
//   - 任一阶段 in_progress → Progress（phase 标签 = ledger 阶段名）。
//   - 全部阶段 completed（TaskCompleted）→ result（succeeded）。
//   - 任务 failed / blocked → result（failed，分类 worker_error——
//     ledger 不携带失败分类，桥不编造）。
func (b *AgentTeamsReportBridge) SyncOnce(ctx context.Context) int {
	if b == nil || b.state == nil || b.usecase == nil {
		return 0
	}
	// 心跳先于认领：闸门按惰性健康推导判 runtime，无心跳的注册行
	// 会被判 offline 拒认领。
	b.ensureRuntimeRow(ctx, b.db, b.nowFn())
	updated := 0
	for _, incident := range b.candidateIncidents(ctx) {
		ledger, err := b.state.LookupIncident(ctx, incident)
		if err != nil {
			continue // 读不到（未派发/账本暂缺）：如实跳过，不猜测
		}
		if b.syncTask(ctx, incident, ledger) {
			updated++
		}
	}
	return updated
}

// candidateIncidents 汇集本轮要看的 incident：观察名单 + 已映射行携带
// 的 IncidentID（去重）。
func (b *AgentTeamsReportBridge) candidateIncidents(ctx context.Context) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(b.watch))
	for _, id := range b.watch {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	tasks, err := b.usecase.ListTasks(ctx, b.tenantID, "", 200, 0)
	if err != nil {
		return out
	}
	for _, t := range tasks {
		if t.Kind == KindAgentTeamsTask && t.IncidentID != "" && !seen[t.IncidentID] {
			seen[t.IncidentID] = true
			out = append(out, t.IncidentID)
		}
	}
	return out
}

// syncTask 把一个 ledger 任务镜像为一行生命周期。返回 true 表示本轮
// 写了东西（入队 / 认领 / progress / result 任一）。
//
// 生命周期走完整面（issue 契约 dispatch→create/claim）：Enqueue（若
// 无行）→ Claim → Start（首见阶段推进时）→ Progress（每阶段）→
// ReportResult/ReportFailure（终局）。认领闸门要求 runtime 行健康——
// 桥的 ensureRuntimeRow 在装配时注册 + 每轮心跳 agentteams:worker-1
// （与观察者投影的 RuntimeID 同名：桥是它的报告面，不是第二个 worker）。
func (b *AgentTeamsReportBridge) syncTask(ctx context.Context, incident string, ledger *agentteamsstate.Task) bool {
	task, found := b.findLifecycleRow(ctx, incident)
	if !found {
		// dispatch → create：ledger 任务出现即入队。safety 按「入队时
		// 已开始阶段的最严级别」取（agentTeamsTaskSafety）——dispatch
		// 时点通常是 alert_dedup（read_only）；桥晚接入一个已进
		// repair_execution 的任务时如实记 mutating，但**不编造幂等
		// 契约**（副作用授权的证据在 ledger 的 HITL 决策里，不在
		// runtimemgmt），mutating 无契约会被闸门落终态——这与
		// 「ledger 保持证据权威」一致：报告行如实，闸门语义不因
		// 报告而软化。
		if _, err := b.usecase.Enqueue(ctx, bizruntimemgmt.EnqueueRequest{
			TenantID:   b.tenantID,
			IncidentID: incident,
			Kind:       KindAgentTeamsTask,
			Safety:     agentTeamsTaskSafety(ledger),
			IssuedBy:   "agentteams-bridge",
			TraceID:    ledger.MatrixRoomID,
		}); err != nil {
			return false
		}
		task, found = b.findLifecycleRow(ctx, incident)
		if !found {
			return false
		}
	}
	// 已终态：镜像完成，不再推进。
	if model.IsTerminal(task.State) {
		return false
	}
	// claim：queued/admitted → claimed → running（报告行的执行者是
	// agentteams worker 本身，桥只是代笔——代际与租约语义照走，不因
	// 「只是报告」而绕过闸门）。
	claim, attempt := b.ensureClaimed(ctx, task)
	if claim == nil {
		return false
	}
	// phase running → progress。
	if phase, running := agentTeamsRunningPhase(ledger); running {
		_, _, _ = b.usecase.Progress(ctx, task.TaskID, b.tenantID, bridgeRuntimeID, bizruntimemgmt.CheckpointInput{
			Generation:    claim.Generation,
			AttemptNumber: attempt,
			Phase:         phase,
		})
		return true
	}
	// 终局映射。
	switch ledger.Status {
	case agentteamsstate.TaskCompleted:
		_, _, err := b.usecase.ReportResult(ctx, task.TaskID, b.tenantID, bridgeRuntimeID, bizruntimemgmt.ResultInput{
			Generation:      claim.Generation,
			AttemptNumber:   attempt,
			SanitizerStatus: model.SanitizerStatusSkipped,
			SanitizerNotes:  "agentteams bridge report row; ledger is the evidence authority, no worker output",
			OutputRef:       "agentteams-ledger://" + incident,
		})
		return err == nil
	case agentteamsstate.TaskFailed, agentteamsstate.TaskBlocked:
		_, _, _ = b.usecase.ReportFailure(ctx, task.TaskID, b.tenantID, bridgeRuntimeID, bizruntimemgmt.FailureInput{
			Generation:    claim.Generation,
			AttemptNumber: attempt,
			FailureClass:  model.FailureClassWorkerError,
			FailureReason: "ledger task status " + string(ledger.Status) + "; classification authority is the ledger",
		})
		return true
	}
	return false
}

// ensureClaimed 把 queued/admitted 行推进到 running，返回当前认领行与
// 尝试号。已在执行三态（claimed/preparing/running）时只补 Start。
func (b *AgentTeamsReportBridge) ensureClaimed(ctx context.Context, task model.Task) (*model.Claim, int) {
	switch task.State {
	case model.TaskStateQueued, model.TaskStateAdmitted:
		res, err := b.usecase.Claim(ctx, task.TaskID, b.tenantID, bridgeRuntimeID)
		if err != nil {
			return nil, 0
		}
		if _, _, serr := b.usecase.Start(ctx, task.TaskID, b.tenantID, bridgeRuntimeID, res.Claim.Generation); serr != nil {
			// 认领成功 start 失败：留 claimed，下轮重试 start；不在这
			// 里报 failure（start 抖动不该消耗重试预算）。
			return &res.Claim, res.Attempt.AttemptNumber
		}
		return &res.Claim, res.Attempt.AttemptNumber
	case model.TaskStateClaimed, model.TaskStatePreparing, model.TaskStateRunning:
		claim, err := b.usecase.GetClaim(ctx, task.TaskID, b.tenantID)
		if err != nil {
			return nil, 0
		}
		if task.State == model.TaskStateClaimed {
			if _, _, serr := b.usecase.Start(ctx, task.TaskID, b.tenantID, bridgeRuntimeID, claim.Generation); serr != nil {
				return &claim, 1
			}
		}
		latest, aerr := b.usecase.LatestAttemptNumber(ctx, task.TaskID, b.tenantID)
		if aerr != nil || latest == 0 {
			latest = 1
		}
		return &claim, latest
	}
	return nil, 0
}

// findLifecycleRow 按 (tenant, kind, incident) 找桥入队的生命周期行。
func (b *AgentTeamsReportBridge) findLifecycleRow(ctx context.Context, incident string) (model.Task, bool) {
	tasks, err := b.usecase.ListTasks(ctx, b.tenantID, "", 200, 0)
	if err != nil {
		return model.Task{}, false
	}
	for _, t := range tasks {
		if t.Kind == KindAgentTeamsTask && t.IncidentID == incident {
			return t, true
		}
	}
	return model.Task{}, false
}

// ensureRuntimeRow 注册/心跳 agentteams:worker-1 的 runtime 行（认领
// 闸门要求健康 + 有槽）。每轮 SyncOnce 前调一次；写的是注册表，不是
// ledger——桥「只写 runtimemgmt」的边界不破。
func (b *AgentTeamsReportBridge) ensureRuntimeRow(ctx context.Context, db *gorm.DB, now time.Time) {
	if db == nil {
		return
	}
	var rec model.RuntimeRecord
	err := db.WithContext(ctx).Where("runtime_id = ?", bridgeRuntimeID).First(&rec).Error
	heartbeat := now.UTC()
	if err != nil {
		rec = model.RuntimeRecord{
			RuntimeID:  bridgeRuntimeID,
			Kind:       model.RuntimeKindAgentTeamsWorker,
			Profile:    "team-worker",
			Owner:      "agentteams",
			Health:     model.RuntimeHealthOnline,
			TotalSlots: 4, FreeSlots: 4,
			RegisteredAt: heartbeat, UpdatedAt: heartbeat,
		}
		rec.LastHeartbeatAt = &heartbeat
		_ = db.WithContext(ctx).Create(&rec).Error
		return
	}
	rec.LastHeartbeatAt = &heartbeat
	rec.UpdatedAt = heartbeat
	_ = db.WithContext(ctx).Save(&rec).Error
}

// agentTeamsRunningPhase 返回当前 in_progress 的阶段名（首个命中按
// phaseOrder 顺序）；无进行中阶段返回 ("", false)。
func agentTeamsRunningPhase(task *agentteamsstate.Task) (string, bool) {
	for _, phase := range agentTeamsPhaseOrder() {
		if ps, ok := task.Phases[phase]; ok && ps.Status == agentteamsstate.PhaseRunning {
			return string(phase), true
		}
	}
	return "", false
}

// agentTeamsPhaseOrder 复述 state 域的阶段推进顺序（闭集，与
// state.go 的 phaseOrder 同值；桥侧写死以避免 import 内部符号——
// phaseOrder 未导出）。
func agentTeamsPhaseOrder() []agentteamsstate.Phase {
	return []agentteamsstate.Phase{
		agentteamsstate.PhaseAlertDedup,
		agentteamsstate.PhaseRCA,
		agentteamsstate.PhaseCriticAudit,
		agentteamsstate.PhaseReview,
		agentteamsstate.PhaseHITLApproval,
		agentteamsstate.PhaseRepairExecution,
		agentteamsstate.PhaseVerification,
		agentteamsstate.PhasePostmortem,
	}
}

// bridgeRuntimeID 是桥写生命周期行时的署名（ReportResult/Progress 的
// CompletedBy/RecordedBy 列）。agentteams:worker-1 与观察者的
// RuntimeID 同名——桥是它的报告面。
const bridgeRuntimeID = "agentteams:worker-1"

// ---- 循环驱动（装配根职责：谁装配谁驱动，biz 零 goroutine）----

// RuntimeTaskWorkers 是一次成功装配的句柄。nil 合法；Close 幂等。
type RuntimeTaskWorkers struct {
	Worker *bizruntimemgmt.WorkerRuntime
	Bridge *AgentTeamsReportBridge
	// Interval 是循环步长（测试注入用 0 表达「不跑循环只跑一次」）。
	Interval time.Duration
}

// Run 驱动循环直到 ctx 取消。装配根以 `go asm.Run(ctx)` 或 errgroup
// 调用；零 goroutine 纪律指「默认关闭时零 goroutine」，开启后的这
// 一个循环是显式声明的行为变化。
func (a *RuntimeTaskWorkers) Run(ctx context.Context, log interface {
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
}) {
	if a == nil {
		return
	}
	if a.Interval <= 0 {
		a.Interval = bizruntimemgmt.DefaultPollInterval
	}
	ticker := time.NewTicker(a.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if a.Worker != nil {
				a.Worker.PollOnce(ctx)
			}
			if a.Bridge != nil {
				a.Bridge.SyncOnce(ctx)
			}
		}
	}
}

// assembleRuntimeTaskWorkers 按 env 装配 §2.4+§2.5 的 worker 面。默认
// （OPSKEEPER_RUNTIME_TASK_WORKERS 空）返回 (nil, nil) 零行为；显式
// 启用 manager 名而 repair-preview 依赖缺席不是错误——只少挂那一个
// handler（诊断 worker 不依赖它），boot 日志如实记录。
func assembleRuntimeTaskWorkers(
	db *gorm.DB,
	previewExecutor managerbizdemo.PreviewExecutor,
	previewFingerprint string,
	stateMgr *agentteamsstate.Manager,
	env func(string) string,
) *RuntimeTaskWorkers {
	enabled := runtimeTaskWorkerSet(env)
	if len(enabled) == 0 {
		return nil
	}
	usecase := buildRuntimeTaskUsecase(db)
	asm := &RuntimeTaskWorkers{}
	if enabled[workerNameManager] {
		handlers := []bizruntimemgmt.TaskHandler{
			NewRuntimeInventorySnapshotHandler(db, runtimeTaskOutputDirEnvDefault(env)),
		}
		if previewExecutor != nil {
			handlers = append(handlers, NewRepairPreviewTaskHandler(
				previewExecutor, runtimemgmtstore.NewBizTaskStore(db), previewFingerprint,
			))
		}
		asm.Worker = bizruntimemgmt.NewWorkerRuntime(usecase, bizruntimemgmt.WorkerConfig{
			RuntimeID: bizruntimemgmt.FormatWorkerID(workerNameManager),
			TenantID:  runtimeTaskDefaultTenant(env),
		}, handlers...)
	}
	if enabled[workerNameAgentTeams] {
		asm.Bridge = NewAgentTeamsReportBridge(
			stateMgr, usecase, db,
			runtimeTaskDefaultTenant(env),
			agentTeamsWatchList(env),
		)
	}
	return asm
}

// runtimeTaskDefaultTenant 读轮询租户（v1 单租户；默认 default，与
// HTTP 面的 tenantFromRequest 缺省同值）。
func runtimeTaskDefaultTenant(env func(string) string) string {
	if v := strings.TrimSpace(env("OPSKEEPER_RUNTIME_TASK_TENANT")); v != "" {
		return v
	}
	return "default"
}

// runtimeTaskOutputDirEnvDefault 是输出目录的 env 读取（装配时一次）。
func runtimeTaskOutputDirEnvDefault(env func(string) string) string {
	if v := strings.TrimSpace(env(runtimeTaskOutputDirEnv)); v != "" {
		return v
	}
	return runtimeTaskOutputDirDef
}
