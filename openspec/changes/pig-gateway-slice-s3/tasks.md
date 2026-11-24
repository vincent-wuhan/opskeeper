# Tasks: pig-gateway-slice-s3

对应 `pig-runtime-adapter` tasks 3.1–3.5（治理工具网关集成）。基线 main@f771c1bd。

## 1. OpenSpec change

- [x] 1.1 开 `openspec/changes/pig-gateway-slice-s3/`，proposal/design/tasks/specs 齐备
- [x] 1.2 design.md §一写 A/B 对比 + 不做方案（PR 描述同步转载）
- [x] 1.3 allowlist 表族裁决：本切片仍不建表，理由与落点写进 proposal

## 2. 身份翻译（3.1）

- [x] 2.1 `tool_identity.go`：`TranslateToolCall` 消费 manifest + 暴露集，输出 `ResolvedTool`（RuntimeToolID 不出翻译层）
- [x] 2.2 清单内 RuntimeToolID 撞名 → `ErrDuplicateObservation`；闭集外类别 → 拒绝
- [x] 2.3 `fromDomainToolClass` / `toDomainToolClass` 进入生产调用图

## 3. 分发前强制（3.2）

- [x] 3.1 `gateway.go`：`ToolGateway.Admit` 七条闸（翻译/tenant/role/incident/resource/read-only/限流），顺序即文档
- [x] 3.2 限流复用 `decorators.Limiter` 端口，subject = tenant/role/worker
- [x] 3.3 read-only 硬闸：ReadOnly=true 时非 ClassRead 直接 Blocked，不进审批

## 4. 调用持久化（3.3）

- [x] 4.1 `model/runtimemgmt/tool_call.go`：表 `runtimemgmt_pig_tool_calls`，参数/结果只落哈希
- [x] 4.2 `ToolCallLedger` 端口 + gorm 绑定（`RuntimeMgmtStore` 扩两方法）
- [x] 4.3 `MigrateToolCallTable` 并入 `managerMigrators`，计数 26 → 27，测试同步
- [x] 4.4 audit ledger 双写：`ActionPigToolCall` / `ResourcePigTool` 入闭集 + re-export

## 5. 失败闭合矩阵（3.4）

- [x] 5.1 七类各至少一条测试，断言「拒绝 + 未分发 + denied 审计行」三要素

## 6. proposal-only（3.5）

- [x] 6.1 ReadOnly=true：write/destructive 走硬闸拒绝留痕
- [x] 6.2 ReadOnly=false：`ProposalRequired` 裁决 + Dispatch 接缝，fake ReviewSpawner 走通 proposal 落 fake sink 且不执行

## 7. 闸门

- [x] 7.1 module-check / vet / 全包测试 一手复验
- [x] 7.2 migrator 计数与 deadcode 棘轮读数注记
- [x] 7.3 tasks.md 勾选附证据，独立 PR 引用 BENY-1059
