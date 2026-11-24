## Why

`pig-runtime-adapter` 任务 3.1–3.5（治理工具网关集成）在 S1（#127 生命周期归一）与 S2（#134 三方对账 + 四禁）合入 `main@f771c1bd` 后前置就绪。父 change 的 design 决策 4 写明：所有副作用类调用必须过 Governed Tool Gateway，PiG 扩展不得绕过审批台账或直接执行 API。本切片把该决策落成可测试的代码。

任务书要求先裁决网关接入哪个既有面（方案 A：Manager `decorators` 链 / 方案 B：edge `policygate` 线协议）。裁决与不做方案见 design.md §一；PR 描述同步转载。

## What Changes

- **3.1 身份翻译**：`biz/pigruntime/tool_identity.go` 的 `TranslateToolCall` 把 PiG 侧 `(RuntimeToolID, 进程上报的类别)` 翻译为稳定 OpsKeeper 身份。翻译只消费 S2 对账输出（`AdmissionResult.Exposed`）与发布清单（`CapabilityManifest`），不做第二次映射；RuntimeToolID 不出网关层。
- **3.2 分发前强制**：`biz/pigruntime/gateway.go` 的 `ToolGateway.Admit` 在分发前依次强制 tenant / role / incident scope / resource scope / capability / read-only / rate-limit 七条策略。前六条是本切片新增的 PiG 特有闸（含任务书点名的 incident scope、resource scope、read-only 硬闸三条）；rate-limit 复用 `decorators.Limiter` 端口，以 `(StableToolID, tenant/role/worker)` 为键。
- **3.3 调用持久化**：`pigruntime.ToolCallLedger` 端口（`RecordStart` / `RecordEnd`），生产绑定 `RuntimeMgmtStore` 既有 gorm 适配写新表 `runtimemgmt_pig_tool_calls`。落起止时戳、调用者身份（tenant / role / worker）、release 与 capability digest、参数哈希（不落原文）、结果态、错误、trace 关联。
- **3.4 失败闭合矩阵**：`gateway_fail_closed_test.go` 七类（undeclared / duplicate / drifted / write / destructive / cross-incident / cross-resource）各至少一条，断言拒绝、未分发、且留审计行。
- **3.5 proposal-only**：`write` / `destructive` 类调用在首切片 `ReadOnly=true` 时直接 `Blocked`（不进审批）；`ReadOnly=false` 的后续切片走 `decorators.Wrap` 的 review_gate → `chat_mutating_proposals` 路径产 proposal，执行仍绑定既有 human-approved exact-payload 路径。本切片交付后者所依赖的 `Dispatch` 接缝与一条端到端 proposal-only 测试，不点亮生产写入方。

### 裁决：allowlist 持久化表族本切片仍不建表

S2 把 allowlist 表族决策留给 3.x。本切片核对后维持不建：`RoleAllowlist` 的唯一生产来源仍是装配根钉死的常量，没有 API、没有 UI、没有租户级写入方。建一张无人写的表只会在 deadcode 棘轮上欠新债。网关经 `GatewayConfig.Roles` 消费 `[]RoleAllowlist` 纯值，与 `LifecycleSink` 同源；持久化落点仍是 `ReconcileCapabilities` 的入参签名，不返工。

### 明确不在本切片内

- 不新增 edge ↔ manager 跨模块边：manager 不得链接 node plane（modulecheck `testOnlyImports`），policygate 协议不进本切片。
- 不建 allowlist 表、不建 per-role 暴露面持久化。
- 不点亮 PiG 生产 Launch 消费点：`ToolGateway` 本切片无生产调用方，装配根不变。终态写入约束（预铺注记第 3 条）因此在本切片不被触碰——若后续切片点亮，终态必须走 #132 任务状态机。
- 不实现 redaction 中间件、不实现 PiG 侧工具执行器（子进程内工具仍走既有 gate courier）。

## Capabilities

### New Capabilities

- `pig-gateway-slice-s3`：PiG 工具调用的身份翻译、分发前策略强制、调用持久化、失败闭合矩阵与 proposal-only 契约。

### Modified Capabilities

- 无。父 change `pig-runtime-adapter` 的 spec「Tool calls pass the governed gateway」在本切片由代码兑现，不改写其 Requirement 文本。

## Impact

- `core/manager/biz/pigruntime/`：新增 `tool_identity.go`、`gateway.go`、`tool_call_ledger.go` 与三份测试；`admission.go` 的 `toDomainToolClass` / `fromDomainToolClass` 首次获得生产调用方。
- `core/manager/model/runtimemgmt/`：新增 `tool_call.go`（一张表 `runtimemgmt_pig_tool_calls`）。
- `core/manager/data/runtimemgmt/store/migrate.go`：新增 `MigrateToolCallTable` 并入 `managerMigrators`（计数 26 → 27）。
- `core/base/pkg/audit/port.go`：新增 `ActionPigToolCall`；`core/domains/model/audit/log.go` 同步 re-export。
- deadcode 棘轮预期上调，理由写在 `ratchet_test.go` 注记与 PR 描述。
