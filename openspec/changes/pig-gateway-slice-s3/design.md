# Design: pig-gateway-slice-s3

任务依据：`openspec/changes/pig-runtime-adapter/tasks.md` 3.1–3.5 与 BENY-1072 任务书。基线 `main@f771c1bd`（S1 #127 + S2 #134 已合入）。

## 一、A / B 裁决：接入 Manager decorators 面（方案 A）

任务书要求对比两个既有治理面后裁决，对比与不做方案如下（同步转载 PR 描述）：

| 维度 | A：Manager `decorators` 链 | B：edge `policygate` 线协议 |
| --- | --- | --- |
| 工具落点 | 首切片工具在 Manager 进程内（`pig.host.status → opskeeper.host.status` 钉在装配根，子进程只收 `PIG_*` 环境变量，无 gate socket）——预铺注记已确认 | 为节点面子进程内工具设计（`OPSKEEPER_GATE_SOCKET` unix socket 线协议） |
| 边界代价 | 零新边：pigruntime 与 decorators 同在 `core/manager` 模块，import 即合法 | manager 生产代码链接 node plane，modulecheck `testOnlyImports` 直接红，需过架构评审开新边 |
| 既有能力 | 类别闸（review_gate 按 Class 双签）、限流（TokenBucket per-(tool,subject)）、超时、审计（`ToolCallAuditSink` → 两方法端口）、proposal 台账（`chat_mutating_proposals`） | Admit 三态、审批 TTL、digest、幂等 fence——语义面向「节点执行 host 命令」 |
| 与 S2 对账的衔接 | `AdmissionResult.Exposed` 就是 Manager 侧内存值，直接消费 | 需要把暴露集下发到 gate 线协议，多一次序列化与一致性窗口 |

**裁决：A。** S1 合入后首切片工具落点在 Manager 进程内，A 是阻力最小路径；B 的线协议留给未来「工具移进子进程」的切片（届时按任务书边界纪律先提请架构裁决）。**不做方案：第三套自研网关**——repo 已有两个治理面，再造第三个会让策略语义分叉（同一个 write 调用在三个面有三种审批语义），维护成本高于任何收益。

**A 的落地方式是「复用 + 增补」而非「挂链」**：`decorators.Wrap` 的链位置为 BaseTool 生态设计（tenant_bind 最外层从 ctx 解析租户），而 PiG 网关的输入是结构化 `ToolCallRequest`（tenant/incident/role 是显式字段，不依赖 ctx 侧信道）。把 PiG 调用硬塞进 `Wrap` 需要在 ctx 里伪造一次 tenant 解析，再从 audit 装饰器里反解——绕一圈只为了让审计行落进 `chat_tool_calls`，而该表的 `message_id NOT NULL` 语义（挂 chat 会话）对 PiG 任务调用本就不成立。因此：

- **策略强制**（本切片新增的六条 PiG 特有闸 + 限流）：落在 `pigruntime.ToolGateway.Admit`，净新增、可单测；
- **限流器实现**：直接复用 `decorators.TokenBucketLimiter`（经 `decorators.Limiter` 端口注入），不重写令牌桶；
- **proposal-only**（3.5）：`ReadOnly=false` 的后续切片把 `write`/`destructive` 调用交给 `decorators.WithReviewGate` 包裹的执行器，proposal 落 `chat_mutating_proposals`——本切片交付 `Dispatch` 接缝证明该路径可达，不点亮生产写入方；
- **调用持久化**（3.3）：新表 `runtimemgmt_pig_tool_calls`（理由见 §四），不复用 `chat_tool_calls`。

## 二、3.1 身份翻译（只消费，不二次翻译）

S2 的立场：`ReconcileCapabilities` 的输出只出 StableToolID，RuntimeToolID 不出对账层。网关的翻译职责因此收窄为「按图索骥 + 校验类别」：

```
TranslateToolCall(manifest, exposed, call) → (ResolvedTool, error)
```

1. `call.RuntimeToolID` 在 `manifest` 里查到声明 → 得 StableToolID；
2. 声明不在对账暴露集 `exposed`（按 StableToolID）→ `ErrUndeclaredTool`（未放行 / 未对账，统一 fail closed）；
3. 进程上报类别（`call.Class`，pigruntime 拼写）与声明类别（`fromDomainToolClass(exposed 类别)`）不一致 → `ErrDriftedClass`——观察侧类别与暴露面类别分叉即拒，与 S2 的 class_mismatch 立场一致；
4. 通过后输出 `ResolvedTool`：StableToolID、domain 类别（`toDomainToolClass`）、ResourceScope。**RuntimeToolID 不出现在 `ResolvedTool` 里**——它到 `TranslateToolCall` 为止。

`fromDomainToolClass` 的闭集外语义（返回零值必须拒绝）在这里成为生产约束：暴露集类别异常 → 翻译拒绝，不把空串当 `read_only`。

## 三、3.2 分发前强制（顺序即文档）

`ToolGateway.Admit(ctx, request)` 在 `Dispatch` 之前依次执行，**每一步拒绝都落一条 `denied` 审计行**（见 §五）：

```
① 身份翻译      TranslateToolCall（undeclared / duplicate 在此拦）
② tenant 闸     request.TenantID 非空且 == 暴露集配置的租户
③ role 闸       request.Role 在 RoleAllowlist 表内且该工具被放行
④ incident 闸   request.IncidentID 非空且 == request.Scope.IncidentID（跨事件拒绝）
⑤ resource 闸   request.ResourceID 非空且属于 ResolvedTool.ResourceScope 允许集
⑥ read-only 硬闸 ReadOnly=true 时任何非 ClassRead 调用直接 Blocked，不进审批
⑦ 限流          Limiter.Allow(StableToolID, tenant/role/worker)，拒绝即 ErrRateLimited
```

设计要点：

- **④⑤ 的语义**：PiG 任务是事件作用域的（任务书：跨事件拒绝、跨资源拒绝）。`Scope` 在任务 Launch 时钉死（`RuntimeSemantics` 已有 IncidentID），调用携带的 incident/resource 必须落在钉死的作用域内。跨事件 = 调用携带的 IncidentID 与任务作用域不一致；跨资源 = ResourceID 不在工具 ResourceScope 声明的允许集内。首切片 ResourceScope 形态是清单里的自由串（如 `"host"`），⑤ 的实现是「request.ResourceID 非空时必须命中 `Scope.Resources` 白名单」，空集表示该工具不限定具体资源（只读诊断工具的常态）。
- **⑥ 的语义**（任务书原文：read-only 硬闸）：首切片 `SandboxProfile.ReadOnly=true`，此时 write/destructive 连审批都不进——审批是给合法变更准备的通道，只读发布里的变更请求本身就是策略违规。
- **① 里的 duplicate**：同一观察流对同一稳定身份的两份不一致观察在 S2 对账层已整体拒绝（`ErrDuplicateObservation`）；网关层的 duplicate 指调用携带的 RuntimeToolID 在清单内命中多个稳定身份——`CapabilityManifest.Validate` 已拒绝清单侧重复 StableToolID，但 RuntimeToolID 到 StableToolID 的映射重复（两个声明撞名）仍需一道闸，翻译层按 RuntimeToolID 建索引时发现撞名即 `ErrDuplicateObservation`。

## 四、3.3 调用持久化（新表，不复用 chat_tool_calls）

**决策：新表 `runtimemgmt_pig_tool_calls`。** 复用 `chat_tool_calls` 的三个不成立点：

1. `message_id NOT NULL`——该行挂 chat 会话消息；PiG 工具调用挂在 (task, incident, runtime) 上，没有消息可指。伪造 message_id 会污染 chat 侧重放路径（LLMCallID 配对逻辑）。
2. 缺治理列——release digest / capability digest / role / incident_id 没有落点；任务书明确要求这两个 digest 进持久化。
3. 表族归属——`chat_tool_calls` 是 aiops 会话域；PiG 运行时调用是 runtimemgmt 域（与 `runtimemgmt_task_results` 同族，S2 已把 PiG 终态写在那里）。

表结构（`model/runtimemgmt/tool_call.go`，纯增量）：

| 列 | 说明 |
| --- | --- |
| `id` char(36) PK | UUID，`RecordStart` 返回作 correlation |
| `runtime_id` / `task_id` / `tenant_id` | 调用归属 |
| `incident_id` | 事件关联（任务作用域） |
| `stable_tool_id` | 稳定身份（不是 wire name，不是 RuntimeToolID） |
| `tool_class` | `read`/`write`/`destructive`（domain 拼写） |
| `caller_role` / `caller_worker` | 调用者身份 |
| `release_digest` / `capability_digest` | 快照证据 |
| `parameters_sha256` | **参数哈希，不落原文** |
| `trace_id` | trace 关联（可空，`otel` span 未起时为空串） |
| `started_at` / `ended_at` | 起止时戳 |
| `status` | `dispatched`/`blocked`/`success`/`error`/`timeout` 闭集 |
| `decision` | `allowed`/`blocked`（Admit 裁决；denied 行也有 status） |
| `deny_reason` | 拒绝理由码（闭集） |
| `result_sha256` | 结果哈希（不落原文） |
| `error_message` | 错误摘要 |

**参数哈希而非原文**：与 MCP audit sink 的 `arguments_sha256` 同一取舍——审计要的是「可比对」（重放检测、漂移比对），不是「可重放」。PiG 调用参数可能携带诊断输出片段，原文落库会二次扩大泄露面。`chat_tool_calls.arguments_json` 落原文是因为 chat 重放需要 tool 消息配对，PiG 调用没有该消费者。

**为什么顺便加 `decision`/`deny_reason`**：任务书 3.4 要求断言「拒绝且被审计，不是静默放行」——被审计的证据就是 denied 行本身。若拒绝只进日志不进表，失败闭合矩阵就没有可断言的持久化证据。

TEXT 列无 DB 默认值（MySQL 1101 陷阱，遵循 `migrate.go` 既有先例）。

## 五、审计（audit ledger 双写）

表是运维/证据面，HLD-010 审计链是合规面。`gateway.go` 在每次 Admit（无论裁决）经 `audit.Event` 端口发一条 `ActionPigToolCall`：

- `ResourceType` = 新增 `ResourcePigTool`，`ResourceID` = StableToolID；
- payload 携带 phase（admit/end）、decision、deny_reason、parameters_sha256、digest 两枚、task/incident/runtime 关联——与表列同源；
- `Status`：allowed → success；blocked → denied。

审计词表是闭集（决策 337/449 的已知账目形态），新增 `ActionPigToolCall` + `ResourcePigTool` 两个符号，deadcode 棘轮 +2 有注记（与 §2.2 任务生命周期七动词同刀的先例完全同型）。

## 六、3.4 失败闭合矩阵（七类的落点）

| 类别 | 触发 | 断言 |
| --- | --- | --- |
| undeclared | RuntimeToolID 不在清单 | Admit 拒 + 未分发 + denied 行 |
| duplicate | 清单内两个声明撞同一 RuntimeToolID | 翻译拒绝 + 未分发 + denied 行 |
| drifted | 上报类别 ≠ 暴露面类别 | Admit 拒 + 未分发 + denied 行 |
| write | ReadOnly=true + ClassWrite | Blocked（不进审批）+ denied 行 |
| destructive | ReadOnly=true + ClassDestructive | Blocked + denied 行 |
| cross-incident | 调用 incident ≠ 任务作用域 | Admit 拒 + denied 行 |
| cross-resource | ResourceID 不在作用域白名单 | Admit 拒 + denied 行 |

「未分发」的断言方式：注入 fake Dispatch，断言其调用次数为零。这是矩阵的核心——拒绝必须发生在分发之前，而不是分发失败后补记。

## 七、3.5 proposal-only

首切片 `ReadOnly=true`：write/destructive 由 ⑥ 硬闸拦截，**没有 proposal 路径**——只读发布里的变更请求不产生待审批项，直接拒绝留痕。

`ReadOnly=false`（后续切片点亮）的契约在 `GatewayConfig.ReadOnly` 上：

- `Admit` 对 write/destructive 返回 `Decision=ProposalRequired`（不是 Allowed）；
- 调用方（未来的 Manager 装配）把 proposal 交给 `decorators.WithReviewGate` 包裹的执行器：reviewer 判定落 `chat_mutating_proposals`，approve 后执行仍走既有 human-approved exact-payload 路径；
- 本切片的 proposal-only 测试用 fake ReviewSpawner + fake MutatingProposalSink 走通「write 调用 → proposal 行落 fake sink → 不执行」的路径，证明接缝形状与 review_gate 兼容，不点亮生产写入方。

## 八、不做清单与边界核对

- modulecheck：pigruntime → decorators 同模块 import 合法（manager 模块内）；不新增 edge import。
- deadcode 棘轮：+2 审计词表符号（§五），可能 +若干净测试暴露的既有 test-only 符号——按实际读数注记。
- migrator 计数：26 → 27（`MigrateToolCallTable`），同步改 `migrations_mysql_test.go` 的 want 与注记。
- 装配根不变：`pigruntime_wiring.go` 零改动，`ToolGateway` 无生产调用方——首切片的工具调用消费点（chat loop / task executor 接 PiG 工具）属于父 change 后续任务，本切片交付库与测试。

## 九、风险与取舍

- **无生产调用方的库会不会是死代码？** 会进 deadcode 报告，但不建 fake 消费点：伪造生产调用边比诚实欠债更糟（S2 对 allowlist 表的同型裁决）。棘轮注记写明「3.x 消费点点亮时转入生产调用图」。
- **审计双写（表 + ledger）会不会发散？** 表是 RecordStart/RecordEnd 端口的实现细节，ledger 是 Admit 的旁路发射；两处字段同源（同一个 `denyReason`/digest 值），测试断言两侧一致。不追求分布式事务——审计行丢失不回滚裁决，裁决才是安全边界。
- **限流键没有 user**：PiG 调用者是 (tenant, role, worker) 三元组不是 user——`subject = tenant/role/worker`，与 AgentTeams worker 在 ratelimit.go 的既有 subject 形态一致。
