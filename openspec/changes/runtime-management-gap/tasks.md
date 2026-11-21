# Tasks: runtime-management-gap

## 1. Runtime Inventory and Readback

- [x] 1.1 Define runtime identity, health, capability, capacity, version, artifact-digest, and deployment-location persistence models.
  - 已交付：`core/manager/model/runtimemgmt/runtime.go`（`RuntimeRecord` gorm 模型 + `runtimemgmt_runtimes` 表）与 `core/manager/data/runtimemgmt/store/migrate.go`（注册进 `managerMigrators()`，migrations_mysql_test 计数 24 → 25）。刻意不带 TenantID（运行时是平台资产，ADR §4.1 与 Task 的显式差异，由 `TestRuntimeRecordTableShape` 钉住）。
- [x] 1.2 Add manager APIs for runtime registration, heartbeat, command acknowledgement, inventory, and health transition.
  - 已交付：`core/manager/server/runtimemgmt/http.go` 四条路由（`POST /v1/runtimes`、`POST /v1/runtimes/{id}/heartbeat`、`POST /v1/runtimes/{id}/commands/{cid}/ack`、`GET /v1/runtimes`），挂 protected 组，写路由经 route-audit 表（3 条 verdict）与审计端口（`runtime_register` / `runtime_heartbeat` / `runtime_command_ack` 三个动作 + `runtime` 资源）。
- [x] 1.3 Implement read-only adapters for Edge Agent, Manager Chat Worker, AgentTeams Worker, TeamHarness/plugin manager, and preview-pg.
  - 已交付：`cmd/opskeeper/runtimemgmt_wiring.go` 五份观察者（观察者在装配根而非 runtimemgmt 域内，域内只见 `ReadonlyRuntimeSource` 单方法端口——零条新跨域边）。只读性由编译期（单 Describe 方法）+ 测试期双读比对（`runtimemgmt_wiring_test.go`）共同保证；探不到的读数返回空投影或 `health=unknown`。
- [x] 1.4 Make every runtime report version and artifact digest from manifest/release metadata and reject hard-coded authority.
  - 已交付：注册面只接受 `manifest` / `release_metadata` 两个 `VersionSource`（`TestRegistrationRejectsUnknownKindAndSource` 钉住拒绝 `hardcoded`/`script_constant`）；五个观察者的版本来源分别是心跳自报（release_metadata 同源）、manager build version（ldflags 注入）、plugin.yaml metadata.version、pluginmanifest.Metadata.Version + TreeDigest（sha256）、workload spec fingerprint（manifest 等价物，ADR §4.4 表格）。拿不到时留空——空合法，猜测不合法。
- [x] 1.5 Add heartbeat liveness grace, `starting`, `degraded`, `recently_lost`, and `offline` transitions.
  - 已交付：`RuntimeRecord.EvaluateHealth` 惰性推导（读时推导，无后台 sweeper、零新 goroutine），窗口常量 `DefaultHeartbeatInterval=30s` / `ReconnectGrace=2×` / `OfflineAfter=5×`；九格转移表由 `TestEvaluateHealthTransitionTable` 钉死，心跳漂移降级但存活仍被接受（`TestHeartbeatDriftDegradesButAcceptsLiveness`）。
- [x] 1.6 Add focused model/API/adapter tests and a read-only runtime inventory view.
  - 已交付：三层测试——`runtime_health_test.go`（模型）、`http_test.go`（API 契约 12 条，含空部署返回 `{"total":0,"items":[]}` 的零行为断言、注册表行覆盖投影行的合并语义、惰性健康推导）、`runtimemgmt_wiring_test.go`（适配器只读断言 + 五源齐备断言）。默认部署零注册零心跳时 GET `/v1/runtimes` 返回空数组。

## 2. Durable Task Lifecycle

- [x] 2.1 Define task state, idempotency, admission, claim, lease, checkpoint, attempt, failure classification, and terminal-result models.
  - 已随 PR #98 落入 `core/manager/model/runtimemgmt/`（task{,_state,_idempotency,_admission,_claim,_lease,_checkpoint,_attempt,_result,_safety}.go + failure_class.go + errors.go，均带测试）。本勾选为 2026-11-01 读数纠偏（此前 change 显示 0/41 但 2.1 实际已交付）。
- [x] 2.2 Implement slot-before-claim admission using server limits, runtime health, free slots, and local capacity.
  - 已交付（执行任务见对应 OPC 线程）：`biz/runtimemgmt.Usecase.evaluateAdmission`（server limit ∩ runtime 健康 ∉ {offline, recently_lost} ∩ `ProjectedFreeSlots>0`，兜底投影用 `ActiveTasks<TotalSlots`）；`OPSKEEPER_RUNTIME_TASK_SERVER_LIMIT`（默认 64，装配根一次读取）；入队内联评估一次 + `Claim` 时懒重评，零后台 sweeper；拒绝 → 任务留 queued + rejected `AdmissionDecision`（Reason=no_capacity/runtime_not_ready），零 claim 行。`TestAdmissionRejectedLeavesNoClaim` / `TestAdmissionRuntimeNotReady` / `TestAdmissionFreeSlotsZeroRejected` 钉死。
- [x] 2.3 Add claim, prepare-lease renewal, start, checkpoint, progress, result, cancel, stale-recovery, and failure APIs.
  - 已交付（执行任务见对应 OPC 线程）：`server/runtimemgmt/tasks_http.go` 十二条路由挂 `/v1/runtime-tasks`（`/v1/tasks` 被 report 占用，ADR D-2）；biz/data 分层（`biz/runtimemgmt` 状态机 + `data/runtimemgmt/store.TaskStore` SQL，D-3：认领/终态写全部单条条件 UPDATE，零 read-modify-write）；八表迁移 `MigrateTaskTables`（migrator 计数 25→26）；幂等双闸（入队 fail-fast + 认领前 durable 终态 `no_idempotency_contract`）；可重试失败 `IsRetryable ∧ HasRetryBudget ∧ 指纹匹配` 回 admitted；审计七动词 + 三条高频无行路由 settled 裁决；装配根 `cmd/opskeeper/runtimemgmt_task_wiring.go` 留 §2.4 worker 注册位。`usecase_test.go` 十三条验收测试（含并发双认领恰一成功、旧代际 ErrClaimLost、终态写入拒收）。
- [x] 2.4 Integrate one read-only diagnostic worker and repair-preview execution with the shared task lifecycle.
  - 已交付：`biz/runtimemgmt/worker.go`（ADR D-6）——`TaskHandler` 端口（`Matches` 路由 + `Execute(ctx, task, resume, rt)`，续跑指针与 Reporter 检查点一体滚租约）与 `WorkerRuntime` 通用循环（Poll→Claim→Start→Execute→result/failure，`resumeFor` 取上一尝试最高序检查点做跨代际续跑，零 goroutine 在 biz）。装配根 `cmd/opskeeper/runtimemgmt_worker_wiring.go` 两只 handler：诊断 worker kind=`read_only.runtime_inventory_snapshot`（safety=read_only，`<task-id>.json` 落盘 + OutputRef/Digest/Size 经 `CompiledOutputReporter` 回填）与 repair-preview handler kind=`repair.preview.execute`（safety=disposable，包一层 demo `PreviewExecutor`，契约缺失 fail-closed 为 `no_idempotency_contract`，`ErrDuplicateRun`→成功）。`OPSKEEPER_RUNTIME_TASK_WORKERS` env 门控（默认空=零 worker 零 goroutine）。输出目录 `OPSKEEPER_RUNTIME_TASK_OUTPUT_DIR`（默认 `/var/lib/opskeeper/runtime-tasks`，install manifest 已加 host bind-mount + state-dirs.sh uid 行）。
- [x] 2.5 Integrate AgentTeams/TeamHarness task reporting while preserving the existing incident-phase ledger as evidence.
  - 已交付：`AgentTeamsReportBridge`（ADR D-7，装配根，`OPSKEEPER_RUNTIME_TASK_WORKERS=agentteams` 门控）——轮询 state.Manager 只读面（LookupProject/Get），incident-phase 任务映射 kind=`agentteams.task` 生命周期行（dispatch→入队+认领+running、in_progress 阶段→progress 检查点、completed→succeeded）。safety 映射硬编码：全阶段 read_only，唯 `repair_execution` mutating（桥只报告已开始阶段的最严值——report 行描述「已经做了什么」，不预支未开始阶段）。**ledger 零写入**：双读断言（字节+mtime）钉在 `runtimemgmt_worker_wiring_test.go`（先例 `runtimemgmt_wiring_test.go`）。认领闸门经 `agentteams:worker-1` runtime 注册行 + 心跳（`ensureRuntimeRow`）。watch 列表 `OPSKEEPER_AGENTTEAMS_WATCH_INCIDENTS` + 从既有 runtimemgmt 行自发现。
- [ ] 2.6 Add restart/failover tests proving no duplicate read-only or disposable-preview work.

## 3. Workspace and Disk Governance

- [ ] 3.1 Define per-task `workdir/output/logs/config/temp` layout, claim lock, GC metadata, retention class, and evidence policy.
- [ ] 3.2 Implement task workspace creation, ownership validation, lock hold/release, and fail-safe reset.
- [ ] 3.3 Add bounded disk scans and per-task/incident/workspace/runtime/node/fleet usage reporting.
- [ ] 3.4 Implement policy-driven reclaim for regenerable artifacts, temporary data, stale preview environments, and expired workspaces.
- [ ] 3.5 Add quota/TTL admission checks and crash/recovery/GC tests that preserve evidence.

## 4. Task Credential Broker

- [ ] 4.1 Define task token, external credential grant, scope, expiry, renewal, revocation, and destruction models.
- [ ] 4.2 Mint task-bound credentials at claim time and revoke them at terminal state, lease expiry, cancellation, or runtime loss.
- [ ] 4.3 Broker or narrowly scope cloud, database, plugin API, and SSH credentials instead of exporting broad long-lived environment secrets where feasible.
- [ ] 4.4 Audit credential issuance, use, renewal, denial, revocation, and destruction.
- [ ] 4.5 Add centralized output/error sanitization for task results, logs, evidence, telemetry, and UI.
- [ ] 4.6 Add token expiry, revocation, cross-task denial, audit, and redaction tests.

## 5. Plugin and Worker Rollout

- [ ] 5.1 Define immutable plugin release, artifact digest, signature/verification state, compatibility matrix, and capability manifest.
- [ ] 5.2 Add per-node/per-runtime desired version and observed readback state.
- [ ] 5.3 Implement plugin reconciliation on Node Runtime Agent with version, digest, capability, health, and compatibility readback.
- [ ] 5.4 Add canary, ordered batch, health gate, pause/resume, failure isolation, and rollback APIs.
- [ ] 5.5 Pin the plugin release snapshot used by each running task.
- [ ] 5.6 Add rollback and mixed-version compatibility tests.

## 6. Runtime Event Timeline and Operations UI

- [ ] 6.1 Define append-only runtime event schema and correlation IDs.
- [ ] 6.2 Emit registration, health, drift, task, workspace, credential, plugin, rollout, and rollback events.
- [ ] 6.3 Add timeline query APIs filtered by runtime, task, incident, plugin, approval, and trace.
- [ ] 6.4 Add runtime inventory, task recovery, capacity, disk, credential, rollout, and event-timeline UI.
- [ ] 6.5 Add retention, authorization, and event-completeness tests.

## 7. End-to-End Verification

- [ ] 7.1 Verify read-only inventory across all first-slice runtimes.
- [ ] 7.2 Simulate Manager, worker, edge, plugin, and preview-runner restarts and verify recovery semantics.
- [ ] 7.3 Simulate network partition and verify fail-closed new mutating work with bounded local autonomy.
- [ ] 7.4 Verify canary failure pauses rollout and automatically restores the previous healthy release.
- [ ] 7.5 Verify disk GC preserves evidence and only reclaims policy-approved artifacts.
- [ ] 7.6 Verify task credentials expire, revoke, audit, and redact as required.
- [ ] 7.7 Run security, migration, Go, Python, and end-to-end regression suites.
