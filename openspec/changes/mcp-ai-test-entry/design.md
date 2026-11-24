# Design — mcp-ai-test-entry：外部 AI 经 MCP 契约的探索通道

基线：`main@c0b3729e`（`docs(openspec): propose the MCP AI exploration harness (#138)`）的干净检出。
本文所有文件路径、行号、计数都是在这份检出上一手读到的。**本 change 只出方案，不写实现代码**；
文中出现的代码形状全部是设计草图，不是交付物。任务级拆解与每条任务的回写口径见
`tasks.md`（同 patch）。

## 0. Context —— 这条通道要回答什么问题

OpsKeeper 对外是真实 MCP server：`POST /api/v1/mcp`，JSON-RPC 2.0，仅 `tools` 能力
（`core/manager/server/mcp/http.go:408-552`）。身份经 `Authenticator` 解析
（`core/manager/server/mcp/middleware/auth.go:306`），工具可见性经 `toolAllowedForCaller`
做 RBAC + Worker 角色双重过滤（`http.go:647-652`），`tools/call` 每次写审计链并回传
`X-Opskeeper-Audit-ID`（`http.go:538,546`；审计行由 `core/manager/server/mcp/audit_sink.go:87-141`
同步 `EmitWithID` 落库，并把行号回填到 `AuditReceipt`）。

这份契约今天只有两类消费者：

| 消费者 | 位置 | 证明的是 |
|---|---|---|
| 协议面测试 | `make mcp-surface-check`（`Makefile:460-465`）→ `core/manager/server/mcp/loopback_test.go:28` 与 `protocol_test.go:68-258` | 协议是对的：握手回显、ping 空应答、分页游标、通知不回体、可见性过滤 |
| 仓内 e2e | `tests/e2e/`（27 个 `*_test.go`，`testenv` 默认全 fakes 零凭据） | 平台自己能用：登录、告警、RCA、通知…… |

**没有被检验过的性质**：一个外部 Agent 只凭 `tools/list` 和 `initialize.instructions`
（`http.go:298` 的 `mcpInstructions` 常量），像真实客户那样能不能把产品用明白、在哪里卡住。
这正是 `docs/mcp-surface.md` 存在的理由，却没有任何测试在测它。

另一半理由是产能：`docs/test/e2e-catalog.md` 列 105 条目标用例（`grep -cE '^\| [A-Z][0-9]+ \|'`
= 105），清单自标记 11 条（文末「当前实现：11 条」），表内 ✅ 实有 12 处。缺口量级不变：
逐条靠人写，编写速度就是覆盖率的上限。

## 1. Goals / Non-Goals

**Goals**

- 一条可重复的**探索通道**：一条 make 目标拉起一次性 testenv manager + 铸造限权令牌 + 输出章程；
  外部 AI 用标准 MCP 客户端接入，仓内不绑定模型与 Agent 框架。
- 一层**确定性判决**：以 manager 侧审计链与调用轨迹为唯一证据，断言五条不变量
  （`spec.md`「判决只来自确定性证据」）；AI 主观判断只记线索，不进门禁。
- 一条**固化通道**：失败/有价值轨迹导出为脚本化仓内客户端用例，进 e2e 套件成为回归。
- 通道定位为**夜间或手动**，非必过门禁。

**Non-Goals**

- 不让 AI 当裁判（`proposal.md` Non-goals 第一条；决策 119 在 deadcode 上写过教训：
  会误报的闸门教人跳过它）。
- 不扩大 MCP 工具面：不新增工具、不放宽可见性；探索暴露的契约缺陷另立 change。
- 仅 testenv、仅回环；不对生产/预发开放。
- 不绑定具体模型/Agent 框架；接入方只需标准 MCP 客户端。
- 不替换现有套件：确定性 e2e 仍是回归底座，探索通道是它的上游。

## 2. 现成落点 vs 新代码

这一节回答任务书的硬要求：「标明哪些需求在仓里已有现成落点，哪些需要新代码；已有落点给出文件级引用」。

| 需求（`spec.md`） | 现成落点（文件级） | 还缺什么（新代码） |
|---|---|---|
| 探索环境一次性且仅回环可达 | `tests/e2e/testenv/env.go:97-216`：`Start` 拉起 manager，`OPSKEEPER_HTTP_ADDR` 固定 `127.0.0.1:<随机端口>`（`env.go:135,156`），DB 走 testcontainers MySQL（`env.go:424-513`），默认全 fakes 零外部凭据；`Stop` 幂等（`env.go:217`） | **零新代码**即可满足监听地址与零凭据；缺的是「一次性探索 run」的编排（见 §4） |
| 令牌铸造的信任边界 | `POST /api/v1/agentteams/token`（`core/manager/iam/server/http.go:159,316-360`）→ `IssueAgentTeamsToken`（`core/manager/iam/biz/user/usecase.go:115-152`）→ `SignAgentTeamsService`（`core/base/pkg/auth/jwt.go:114-145`）。三段 JWT，issuer/audience/token_type 全钉死，TTL ≤15m，worker 必须等于 `opskeeper-<role>`，allowed_tools 必须是该角色允许集的子集 | **零新代码**即可铸造；缺的是「从结构上保证令牌打不到非 testenv 实例」（见 §5 D-4） |
| 判决证据：每次 `tools/call` 审计链有且仅有一行并带 `X-Opskeeper-Audit-ID` | `audit_sink.go:87-141`（同步 `EmitWithID` + `receipt.SetID`）；`http.go:538,546`（回传头）；`http.go:703-781`（`mcpContentWithAudit` 把 `audit_log_id` 内嵌进 `structuredContent`/`_meta`）；`http_test.go:633,656` 已有两条钉住它的单测 | **零新代码**在服务端；缺的是「读回这些行」的读口（见 §5 D-2） |
| `tools/list` 未列出的工具调不动 | `http.go:583-590`（`authorizeRBAC` + `MCPAuthorizer.Authorize` 双重判定）与 `core/manager/biz/loop/mcp_authorizer.go:48-89`（签名 token 范围 ∩ 角色策略）；`FilterToolNames`（`mcp_authorizer.go:91`）让 `tools/list` 与 `tools/call` 走同一套判断 | **零新代码**；判决层只需断言「调了看不见的工具 → 应答是错误 + 无副作用」（见 §6） |
| 写/破坏类调用进审批队列且不被自动放行 | `cloud_bash`：`Class="destructive"`，`InvokableRun` 必走 `ProposeAndAwait` 阻塞等人工（`core/manager/biz/aiops/tools/cloud_bash_basetool.go:83-91,118-135`）；`host_restart_service`/`recovery.execute`：`Class="write"` + `ReviewGate` 装饰器（`decorators/review_gate.go:60-150`）或 recovery 的 proposal gate（`registry_basetool.go:201-218`）；审批队列 `GET/POST /v1/approvals*`（`core/manager/server/approval/http.go:28-32`） | **零新代码**；判决层只需读 `/v1/approvals` 断言「有 pending 且无执行结果」（见 §6 I-3） |
| 应答不含凭据形状字符串 | `audit.Event.Payload` 注释要求 caller 先脱敏（`core/base/pkg/audit/port.go:66-68`）；`canonicalDigest`/`resultProjection` 只回摘要（`http.go:710-749`）；`WithSensitivity`（`decorators/sensitivity.go:41-50`）按 reader tier 拦 Confidential/Restricted | 服务端零新代码；**判决层需要新代码**：一份凭据形状正则 + 对轨迹里每个应答体扫描（见 §6 I-5） |
| 探索前后 `tools/list` 一致 | `http.go:470-512`（同一身份同一过滤路径，无状态） | **判决层新代码**：探索前后各取一次 `tools/list` 逐项 diff |
| 探索受预算约束 | 无 | **新代码**：章程里的 `max_tool_calls`/`max_steps` + 通道侧计数与终止 |
| 轨迹采集 | 服务端审计链已有全部「发生了什么」；**没有**「AI 看到了什么才做下一步」 | **新代码**：一份客户端侧 recorder（见 §5 D-2） |
| 回放固化 | `core/base/pkg/mcpclient`（`client.go:38-226`）已是仓内标准 MCP 客户端，`Initialize/ListTools/CallTool` 三件套齐全 | **新代码**：轨迹→回放用例的序列化格式 + 生成器（见 §5 D-3） |
| 报告落点 | `scripts/gatereport/main.go`（明确「什么都不存」的本地闸门汇总器，`Makefile:753`） | **新代码**：独立的探索报告产物（见 §5 D-5） |

**结论**：五条不变量里，服务端行为**全部已有**；本 change 的新代码集中在**三处**——
①探索 run 的编排与令牌铸造的 testenv 侧封装、②轨迹 recorder + 判决器（纯读侧）、
③回放用例生成器。服务端零行为变更，这与 `proposal.md` Impact 里「生产二进制零行为变更」一致。

## 3. 五个待决问题的裁决

proposal 末尾的五个问题，逐条裁决如下。每条给出候选、取舍、被否方案。

### D-1 章程格式：Markdown 清单 vs 结构化 YAML

**裁决：Markdown 文件为唯一事实源，机器可读字段以 YAML frontmatter 放在文件头，正文是给人（和 AI）读的任务描述。**

仓里已有这个形状的先例：`agents/*.md` 就是 `---` frontmatter（`name/avatar/description/when_to_use`）
+ Markdown 正文，`core/manager/biz/aiops/chatruntime/agent_registry.go:96` 已经按 frontmatter
`name` 取 persona。章程与 persona 同形，接入方和仓内工具链都不需要学第二套格式。

frontmatter 字段（闭集，缺一即 charter 非法）：

```yaml
---
id: explore-incident-read-path        # 全局唯一，kebab-case
title: 告警 → 事件详情 → 只读诊断
role: investigator                     # 铸造哪种 Worker 令牌（闭集，见 §5 D-4）
tenant: default                        # AgentTeams 身份里的 tenant
max_tool_calls: 60                     # 预算：工具调用数上限
max_steps: 40                          # 预算：对话轮数上限
catalog_refs: [E3, F2, I2]             # 可选：关联 docs/test/e2e-catalog.md 的条目号
---
```

正文是任务描述：想验证的产品路径、期望观察到的现象、禁止事项。它同时是**喂给 AI 的
`instructions` 之外的唯一任务输入**。

**与 `docs/test/e2e-catalog.md`「实现」列的互认：探索通过不算「实现」，只有回放固化后才勾。**

- 理由 1（提案已给倾向，此处坐实）：catalog 的「实现」列语义是「`tests/e2e/` 下有同名文件」
  （`docs/test/e2e-catalog.md` 表头与文末说明）。AI 探索一次通过不是一个文件，是一次运行；
  勾上去等于把「跑过一次」记成「有回归保护」，正是 `proposal.md` Non-goals 第一条反对的
  「trust me」。
- 理由 2：探索是非确定性的。同一个章程今天过、明天不过，勾与不勾都会说谎。
- 落地形式：探索报告里带 `catalog_refs`，报告是线索；只有该轨迹被固化成 `tests/e2e/` 下的
  回放用例、`make test-e2e` 绿之后，才在 catalog 勾 ✅ 并写文件路径——与既有 12 条 ✅
  的写法完全一致（如 `B1 ... ✅ tests/e2e/auth_login_test.go`）。

**被否方案 A：纯 YAML，无正文。** 否决理由：章程的一半价值是把「像真实客户那样用」
这个意图讲清楚，那需要散文；纯 YAML 会把任务描述压成一句 `description`，AI 拿到的信息
反而少于真实客户从 `initialize.instructions` 能读到的。

**被否方案 B：章程写进代码（Go 常量/结构体）。** 否决理由：章程要被非 Go 的接入方读、要能
不改代码就新增一份；写进代码等于把「探索什么」绑死在编译期，与「不绑定具体模型或框架」
的硬约束精神相悖。

**被否方案 C：把 catalog 当章程。** 否决理由：catalog 的 105 条是**仓内视角**的用例
（触发、断言点、优先级），不是**客户视角**的任务。把 E3 直接当章程交给外部 AI，等于让它
照着答案答题，测不出「只凭 tools/list 能不能用明白」。

### D-2 轨迹采集点：只靠 manager 侧审计链够不够

**裁决：审计链是判决的唯一证据源，但**不够**还原「AI 看到了什么才做下一步」；补一个客户端侧 recorder，它只提供线索，不提供判决证据。**

分工画清楚：

| 事实 | 谁记录 | 用途 |
|---|---|---|
| 哪次调用、什么参数摘要、什么结果摘要、哪个租户、成功/失败 | manager 侧审计链（`audit_sink.go:87-141` 写 start 行，`:143-169` 写 end 行；`arguments_sha256`/`result_sha256` 在 payload 里） | **判决证据**（唯一） |
| AI 在第 N 步看到的 `tools/list` 与 `initialize.instructions`、它选了什么、它说为什么 | 客户端侧 recorder（新代码） | **线索**（回放时重建上下文、报告里解释因果） |

recorder 的形状（设计草图）：一个 `httptest`/`http.RoundTripper` 级的中间层，接入方把
MCP 客户端指到它，它转发到 testenv manager，并把每个请求/应答的**红acted 摘要**
（method、tool name、args digest、status、`X-Opskeeper-Audit-ID`、`tools/list` 快照）按序
写成 JSONL。**它不解析业务语义，只做记录**——判决器完全不读它。

**为什么审计链不能提供「看到了什么」**：`tools/list` 在服务端是无状态读
（`http.go:470-512`），不写审计行——这是对的，读路径不审计是决策 333 之前的既定行为
（`core/domains/server/audit/http.go:11-13` 对读路径不审计给了理由：per-refresh rows
drown out the signal）。所以审计链里**没有**「AI 当时看到了哪些工具」，而这个恰恰是
「能不能用明白」的一半。

**被否方案 A：只靠审计链。** 否决理由：判「看不见的工具调不动」够用（审计链有
`mcp_tool_authorize` 拒绝行，`http.go:654-694` 的 `emitAuthorizationDenied`），但判「AI 卡在哪」不够用——
卡点往往在「工具列表里没有它能用的工具」，而这件事在审计链里是**沉默**的。

**被否方案 B：recorder 作为判决证据。** 否决理由：recorder 在接入方进程里，接入方可以改它；
把判决建立在可被判决对象修改的数据上，等于没有判决。这与 `spec.md`「判决 MUST 由确定性
代码给出，证据 MUST 仅取自 manager 侧审计链与调用轨迹」字面一致——「调用轨迹」指
manager 侧观察到的调用序列，不是客户端自述。

**被否方案 C：让服务端把 `tools/list` 也写审计。** 否决理由：这会改变生产二进制的行为
（每次 `tools/list` 写一行），违反硬约束 2「不扩大 MCP 工具面」的精神（虽未改契约，
但改了可观测行为与审计量），且与本仓「读路径不审计」的既定决策冲突。若探索真的需要，
另立 change。

### D-3 回放格式：轨迹序列化成什么、怎么重放、怎么对账

**裁决：JSON Lines，一条记录 = 一次 JSON-RPC 请求/应答对；回放用仓内 `pkg/mcpclient` 重放；
对账以「manager 侧审计链新增行与首次探索逐项对应（方法、工具名、租户）」为准，不比对响应体。**

格式（设计草图）：

```jsonl
{"seq":1,"method":"initialize","params":{...},"request_id":"req-1"}
{"seq":2,"method":"notifications/initialized","request_id":"req-2"}
{"seq":3,"method":"tools/list","params":{},"request_id":"req-3","observed_tools":["query_incidents", "..."]}
{"seq":4,"method":"tools/call","params":{"name":"query_incidents","arguments":{...}},"request_id":"req-4","audit_id":"1234"}
```

设计要点：

1. **只记请求，不记应答体**。应答体里带运行时数据（时间戳、随机 ID、fake LLM 输出），
   逐字比对必然 flaky；回放断言的是「我发出了同样的调用序列」，不是「我拿到了同样的字符串」。
   这与 `spec.md` 的回放场景逐字一致：「manager 侧审计链新增的行与首次探索逐项对应
   （方法、工具名、租户）」——三个字段全是审计行上有的。
2. **`observed_tools` 是线索字段**，回放器不依赖它；它保留在文件里是为了让读报告的人知道
   AI 当时看到了什么（D-2 的线索定位）。
3. **参数全量保留**（`arguments` 原样），但回放器在把参数发出去之前跑一遍凭据形状扫描；
   命中即拒绝生成回放用例——一条带着真凭据的轨迹不该被固化进仓里。
4. **生成物是一个 `tests/e2e/explore_replay_test.go` 里的 table-driven 用例**，用
   `testenv.Start` 起环境、用 `mcpclient.NewHTTP` 打真 handler。它自动受现有闸门约束：
   `//go:build e2e` 标签、`make e2e-manager-check`（`Makefile:230`）、catalog 同步。
   **不新增任何测试框架**。

**对账口径**：回放后从审计链读回该会话的行（经 D-4 的读口），与 JSONL 里每条
`tools/call` 记录做集合比对：`(method, tool_name, tenant)` 三元组序列必须一致；
数量不一致即回放失败。**不比对 `audit_id`**——那是自增主键，两次运行必然不同；
也不比对响应摘要——见要点 1。

**被否方案 A：录完整 HTTP 往返（含响应体）做 golden-file 回放。** 否决理由：见要点 1，
且与仓里既有实践相反——`mcp_test.go` 用固定标记 `echo-ok-T3` 而不是整段 golden，
就是为了这个。

**被否方案 B：回放走独立的 mock server。** 否决理由：回放的价值是「打真 manager、写真审计链」，
mock 掉就只剩协议层，而协议层已被 `mcp-surface-check` 钉住了。

**被否方案 C：把回放做成 shell 脚本/curl 序列。** 否决理由：仓内已有一等公民客户端
`pkg/mcpclient`，且 `TestOurOwnClientCanDriveOurOwnServer`（`loopback_test.go:28`）就是
它的端到端证明；用 curl 另起一套等于维护第二个客户端。

### D-4 令牌铸造的信任边界：进程内签发还是独立签发器

**裁决：复用现有 `POST /api/v1/agentteams/token` 铸造（不新增签发面），信任边界由
「JWT secret 每次随机 + 仅回环监听 + TTL ≤15m + 最小权限角色」四件套从结构上保证；
**否决**新增独立签发器与 testenv 专用铸造端点。**

四件套逐条对应到代码事实：

1. **JWT secret 每次 `Start` 随机**：`env.go:160` `"OPSKEEPER_JWT_SECRET": "test-jwt-secret-" + randomSuffix()`，
   `randomSuffix()` 读 `/dev/urandom` 4 字节（`env.go:690-695`）。这意味着 testenv 签出的
   令牌**在密码学上就不可能**通过任何其他实例的验签——每个 testenv 实例、每次运行
   都是不同的密钥。`Signer.Verify`（`core/base/pkg/auth/jwt.go`）只认本进程 secret。
   → **这一条就是 `spec.md`「测试令牌打不到别处」场景的结构性保证**，不需要任何新代码。
2. **仅回环监听**：`env.go:135,156`，`127.0.0.1:<随机端口>`。
3. **TTL ≤15m**：`jwt.go:134` 硬上限；`IssueAgentTeamsToken` 进一步限 60–900s
   （`usecase.go:128-131`）。
4. **最小权限角色**：章程的 `role` 字段决定铸造哪种 Worker 令牌，`allowed_tools` 必须是
   `AgentTeamsRoleAllows` 的子集（`usecase.go:134-140`），服务端 `SignAgentTeamsService`
   再校一遍（`jwt.go:129-133`）。章程默认取 `investigator`（只读 + 10 个工具，
   `jwt.go:184-193`），它天然满足「写类工具看不见」——但这**不豁免**判决层对 write/destructive
   的断言，因为章程可以显式要求 `repairer`（唯一可 mutate 的角色，`mcp_authorizer.go:149-159`）
   来验证审批队列路径。

**关于「探索需要 write 类工具可见」的矛盾**：`spec.md` 要求「class 为 write 或 destructive
的调用进入审批队列且不被通道自动放行」，但 `investigator` 角色的 `tools/list` 里根本没有
write 类工具（`FilterToolNames` 会滤掉）。所以这条不变量的验证需要章程**显式**选
`repairer`（`recovery.execute`）或 `admin`（全部工具）——判决层对两类章程都适用：
看不见 → 调用被拒（I-2 成立）；看得见 → 进队列（I-3 成立）。这一条写进章程模板的
`role` 字段说明，避免实现者只用 investigator 然后宣称 I-3「无法验证」。

**被否方案 A：独立签发器进程（单独的 token minter）。** 否决理由：多一个进程、多一份
secret 管理、多一个可攻击面，而它换来的唯一东西是「签发器可以离线」，但本通道的令牌
TTL ≤15 分钟且只打回环，离线签发没有收益。且现有 `POST /v1/agentteams/token` 已在
生产路径上（admin-only + 审计 `agentteams_token_issue`），复用它零新增代码。

**被否方案 B：testenv 专用铸造端点（新 HTTP 面）。** 否决理由：`proposal.md` Impact
里写的是「testenv 需要暴露两个测试专用的读口：令牌铸造，以及按会话导出审计行。仅测试构建
可见」——但一手核查后，**铸造这一半已经存在**（`/v1/agentteams/token` 就在
`RegisterProtected` 里，`iam/server/http.go:159`，testenv 起的 manager 就有），不需要新端点。
新增一个「测试专用铸造端点」反而是**扩大攻击面**（多一个能签发凭据的路由），与硬约束 3
相悖。→ **这是本设计对 proposal Impact 段的一处修订**，见 §7。

**被否方案 C：直接把 admin JWT 交给接入方。** 否决理由：admin JWT 能打所有 `/api/v1/*`
（含用户 CRUD、设置改写），权限面远大于「经 MCP 契约探索」所需；且它是三段 JWT 里
`token_type` 为空的那种，走的是 JWT 中间件而非 MCP Bearer 中间件，**根本打不通
`/api/v1/mcp`**（该路由挂在 Bearer 子路由下，`main.go:4059-4060`）。技术上就不成立。

### D-5 报告落点与线索/判决分呈

**裁决：独立产物（`openspec/changes/mcp-ai-test-entry/reports/<run-id>.json` + 同名 `.md`
人读版），不进 gatereport；报告顶层分 `verdict` 与 `leads` 两段，物理隔离。**

不进 gatereport 的理由是 gatereport 自己写下的：它「什么都不存。不写文件、不记分、不留一个
明天会过期的摘要——决策 462 花一刀删掉的正是那种『写进文档就没人重算的数』」
（`Makefile:738-746` 注释；`scripts/gatereport/main.go:12-27`）。探索报告恰恰必须落盘
（要回放、要对账、要人读），两者目标相反；把探索塞进 gatereport 会让它从「一眼睛看完全部
闸门」变成「混进一份长报告」。

报告结构（设计草图）：

```json
{
  "run_id": "2026-11-26T03-17- explore-incident-read-path",
  "charter_id": "explore-incident-read-path",
  "charter_digest": "sha256:...",
  "budget": {"max_tool_calls": 60, "used_tool_calls": 41, "max_steps": 40, "used_steps": 23},
  "verdict": {
    "outcome": "pass",                    // pass | fail | incomplete
    "invariants": [
      {"id": "I-1", "name": "每次 tools/call 恰好一行审计且带 Audit-ID", "status": "pass", "evidence": {"calls": 41, "audit_rows": 41}},
      {"id": "I-2", "name": "看不见的工具调不动", "status": "pass", "evidence": {"attempts": 2, "denied": 2}},
      {"id": "I-3", "name": "write/destructive 停在审批队列", "status": "not-exercised"},
      {"id": "I-4", "name": "不越租户", "status": "pass"},
      {"id": "I-5", "name": "应答不含凭据形状", "status": "pass", "evidence": {"scanned_responses": 41}}
    ]
  },
  "leads": [
    {"step": 17, "kind": "ux-friction", "note": "AI 连续三次调用 query_promql 都因时间格式被拒；instructions 没说格式"}
  ],
  "replay_artifact": "tests/e2e/testdata/explore/2026-11-26-explore-incident-read-path.jsonl"
}
```

`verdict` 段**只能**由确定性代码写；`leads` 段**只能**由 recorder/AI 自述写。两段在同一文件
里但不共享任何字段——判决器不读 `leads`，`leads` 里也放不进 `verdict` 的任何位置。
这满足 `spec.md`「AI 的判断不作门禁」及其场景「线索不改变结论」。

**落盘位置的理由**：`openspec/changes/mcp-ai-test-entry/reports/` 而不是 `docs/` 或 `tests/`，
因为报告是**过程产物**——它随 change 走，change 归档时报告一起归档；放进 `docs/` 会
让一份 2026-11-26 的运行记录被当成「文档」，放进 `tests/` 会让它被当成「测试数据」。
`.gitignore` 不排除它（要能回看），但**回放用的 JSONL** 放 `tests/e2e/testdata/explore/`
——那是它被 e2e 用例读的地方。

**被否方案 A：进 gatereport。** 否决理由见上；且 gatereport 的输出是「每道门一行」的表，
一份探索报告有五条不变量 + 若干线索 + 预算，塞不进一行。

**被否方案 B：报告只写 PR comment / issue comment。** 否决理由：回放对账需要机器可读的
完整报告；且 comment 不可 diff、不可 grep、随平台走。

**被否方案 C：判决与线索混在一张表里，用 `source: ai|deterministic` 区分。** 否决理由：
物理隔离才是「AI 的判断不作门禁」的结构性保证；一张表里加一列 `source`，迟早有人写
`if source == "ai" && severity == "high" { fail }`，那时门禁就已经被 AI 拿走了。

## 4. 切片顺序与首片范围

每片是独立 OpenSpec 任务 + 独立 PR。顺序按依赖排，**首片必须能独立验证且不依赖任何外部 AI**。
任务级拆解（含每条回写口径）见 `tasks.md`；本节给切片边界与理由。

### S1（首片）：探索通道的地基——零 AI 可验证

**范围**：

1. **章程格式与校验器**（D-1）：`docs/test/explore-charters/_template.md` + 两份真实章程
   ——`explore-incident-read-path.md`（role=investigator，只读路径）与
   `explore-repair-approval.md`（role=repairer，为 I-3 铺路，见 D-4 的矛盾说明）；
   校验器检查 frontmatter 闭集字段、`role` 合法（取自 `auth.AgentTeamsWorkerPermissions()`
   的角色集，`jwt.go:154-223`）、`max_tool_calls>0`、`max_steps>0`、`catalog_refs` 可解析。
2. **testenv 的 DSN 访问器**：`Env` 存下 `Start` 拿到的 dsn，暴露 `Env.DSN()`——
   测试包内六行改动，生产二进制零变更。
3. **审计读口**（新代码，测试侧）：不是新 HTTP 端点，而是 `tests/e2e/explore/` 里直连
   MySQL 读 `audit_logs` 的 Go 函数（DSN 经 `Env.DSN()`，`env.go:159`）。**不选用已有的
   `GET /v1/admin/audit-logs`**（`core/domains/server/audit/http.go:34`，挂在 protected
   下）作主读口，因为它分页上限 500（`repo.go:69-71`）、payload 是字符串化的 JSON，且
   走 admin JWT 而非探索身份——判决器要的是「按会话窗全量、机器可结构化」，直读 DB 最省；
   HTTP 读口留作交叉验证。
4. **探索编排**（新代码）：`Start` → `LoginAdmin` → `POST /api/v1/agentteams/token`
   铸造令牌 → 输出连接信息。**全部复用现有 API，零服务端改动。**
5. **判决器**（新代码）：五条不变量的确定性断言（§6），输入 = 审计行 + 轨迹 +
   探索前后 `tools/list` 快照 + 章程。**输入类型里没有放主观陈述的位置。**
6. **首片的独立验证（不依赖任何 AI）**：`harness_self_test.go` 用 `pkg/mcpclient` 扮演
   「假的探索者」——固定调用序列，然后跑判决器，断言五条不变量**全部按预期判定**
   （该 pass 的 pass、该 fail 的 fail、`not-exercised` 的 not-exercised）；另有一条
   **已知坏轨迹**直喂判决器必须 fail（`spec.md` 修订 2 的场景）。
   **这一步就是「首片能独立验证」的证明**：判决器在没有任何 AI 的情况下被端到端验证了。
7. **令牌边界验证**（`spec.md` 场景「测试令牌打不到别处」）：起**两个** testenv 实例
   （同一 `go test` 进程共享一个 MySQL 容器——`env.go:421` `sharedMySQL` 每进程一次——
   但每次 `Start` 随机 JWT secret，`env.go:160`），用 A 的令牌打 B 的 `/api/v1/mcp`，
   断言 401（`middleware/auth.go:603`）且 B 的 `audit_logs` 零新增行（鉴权拒绝路径只
   `log.Warn` 不写审计，`middleware/auth.go:607-609`）。这条测试是 D-4 四件套里第 1 条的
   **可执行证明**，也顺带钉住「共享 DB 不同密钥」这一精确边界。
8. **I-3 的通道内验证**：用 repairer 章程的假探索者调 `recovery.execute`，断言它停在
   审批队列且无 end 审计行——把「write 停在队列」这条不变量也在零 AI 前提下钉死，
   而不是等 S3 的回放才发现判决器没实现它。

**首片不包含**：recorder（D-2，需要真实 AI 才有意义）、回放生成器（D-3）、报告落盘
（D-5）。首片的产出是「一条能跑通判决的通道 + 一份被验证的判决器」。

**首片验证命令**：`make test-e2e`（新测试自动被 `./tests/e2e/...` 覆盖）或
`go test -tags=e2e ./tests/e2e/explore/ -v`。

### S2：客户端 recorder + 报告落盘

- recorder（D-2）：`http.RoundTripper` 级记录代理，输出 JSONL（请求/应答摘要 +
  `observed_tools`；不记 `Authorization` 头）。
- 报告生成（D-5）：`verdict` + `leads` 两段式 JSON + Markdown 人读版；`leads` 从
  recorder 的 AI 自述字段（接入方在章程 `leads_prompt` 段被要求输出的 `note`）收集。
- 章程模板补 `leads_prompt` 段：告诉 AI「如果你觉得哪里卡住了，在这一步的 note 里写下来」。
- 预算执行：通道侧计数，触顶终止、判 `incomplete`（`spec.md`「跑飞即终止」）。
- 验证：用一个**确定性**的假探索者（复用 S1 的形状）跑出一份完整报告，断言
  `verdict` 与 `leads` 两段互不污染（判决器签名层面就没有 leads 参数）。

### S3：回放固化

- 回放格式定稿（D-3）：JSONL schema + `ReplayRecord` 结构体；`observed_tools` 标注为
  线索字段。
- 回放用例生成器：JSONL → `tests/e2e/explore_replay_<charter-id>_test.go` 的
  table-driven 用例（`go run` 生成工具，生成前对参数跑凭据形状扫描，命中即拒绝）。
- 对账逻辑：回放后读审计链，与 JSONL 的 `tools/call` 三元组**序列**比对
  （方法、工具名、租户；数量与顺序都一致才绿；不比 `audit_id`、不比响应体）。
- 验证：用 S2 产出的 JSONL 生成一份回放用例，`make e2e-manager-check` 绿，
  且审计链对账通过；**此刻才允许**在 catalog 勾 ✅（D-1 回写口径）。

### S4：夜间编排与 catalog 回写

- `make explore-nightly`（夜间/手动，非必过门禁；`Makefile` 新 target + CI `schedule`
  触发，与既有 nightly delivery job 同一 `if: github.event_name == 'schedule'` 形状，
  `.github/workflows/ci.yml:499-500`）。
- catalog 回写闸门：断言「勾了 ✅ 的条目，其文件路径必须存在于 `tests/e2e/`」——
  防止把只有探索报告、没有回放用例的条目勾上。
- 凭据形状清单不漂移闸门：Go 侧正则与 `scripts/audit_open_source.py` 的形状清单
  必须覆盖同一组前缀。
- 一份 `docs/test/explore-charters/README.md` 写明：章程怎么写、外部 AI 怎么接、
  报告怎么读、什么算「实现」。

### 不做的方案（显式记录）

**方案 X：不做探索通道，继续手写 e2e。** 105 条缺口按当前速度（11–12 条已落地）
要很多个季度；且手写 e2e 测的是「仓内视角的功能」，永远测不出「外部 Agent 只凭契约
能不能用明白」这个性质。否决，但承认它是**回归底座**，探索通道不替代它（`proposal.md`
Non-goals 最后一条）。

**方案 Y：用仓内 real-llm e2e 替代。** `make test-e2e-real-llm`（`Makefile:168-176`）
测的是**平台内部**的 AI（manager 自己的 Agent），不是外部 Agent 经契约接入。两者
要回答的问题不同：前者问「我们的 Agent 行不行」，后者问「我们的契约对外人清不清楚」。
否决。

## 5. 成本与风险

### 非确定性

- **风险**：同一章程不同模型/不同温度下轨迹不同，`verdict` 可能翻。
- **缓解**：`verdict` 的五条不变量**全部是服务端性质**（审计行数、拒绝行为、队列状态、
  租户、凭据形状），与 AI 的选择无关——AI 只能决定「触不触发」某条不变量，不能决定
  「触发后判定结果」。所以 `verdict` 的翻只会是 `pass ↔ not-exercised`，不会是
  `pass ↔ fail`。这是「AI 当探索者不当裁判」的结构性收益，写进判决器单测。
- **残留风险**：`not-exercised` 的比例取决于 AI； charter 里给出「至少要触达 X 次 write
  调用」的最低覆盖要求，低于它判 `incomplete`。

### 模型额度

- **风险**：一次跑飞的探索就是一张账单（`proposal.md` Impact 第三条）。
- **缓解**：`max_tool_calls` / `max_steps` 双预算（D-1 frontmatter 必填），通道侧计数，
  触顶即终止并判 `incomplete`（`spec.md`「探索受预算约束」）。令牌 TTL ≤15m 是**第二道**
  硬闸——即使计数器有 bug，令牌过期也会切断。
- **成本量级**：一份章程 ≈ 几十次工具调用 × 每次几千 token 的上下文，属于「一次几毛到
  几块钱」的量级（取决于接入方模型）；夜间跑 5–10 份章程，月度成本可预估。

### 安全边界

- **令牌只打 testenv**：D-4 四件套；S1 的双实例测试是可执行证明。
- **令牌不落盘**：令牌只在 `Start` 的返回值里，写进章程输出时打码（只显示前 8 字符）；
  recorder 的 JSONL 不记 `Authorization` 头。
- **凭据形状扫描**：I-5 覆盖应答体；D-3 的回放生成器对参数再扫一遍。
- **testenv 的 DSN 访问器**（S1 新增）是测试代码，`//go:build e2e` 门控，
  生产二进制零变更；需在 PR 里说明并跑 `make audit-open-source`。

### 回放与首次探索的对账

- **风险**：回放「过了」但其实是「另一条路径也过了」，对账形同虚设。
- **缓解**：对账只比三元组序列（方法、工具名、租户），**数量与顺序都要求一致**；
  不一致即红。且 `observed_tools` 只作线索，不参与对账——避免「AI 看到的」与
  「服务端记的」两套事实混在一个断言里。

### 其他风险

| 风险 | 分级 | 缓解 |
|---|---|---|
| 外部 AI 不按章程走，轨迹无价值 | 中 | 章程正文写给 AI 读；`incomplete` 判定不掩盖；多份章程覆盖不同产品路径 |
| 判决器自身有 bug（假绿） | 高 | S1 的 self-test 用**已知的坏轨迹**断言判决器必须红；判决器对每条不变量有独立单测 |
| 审计读口耦合 `audit_logs` 表结构 | 中 | 读口在 testenv（测试代码），不进生产；只读 `action/resource_id/payload` 三列，列集变动时编译期暴露 |
| 新测试拖慢 `make test-e2e` | 低 | 探索测试起 2 个 manager（双实例测试），单次 ≈ 现有 e2e 单测的 2 倍；S1 只加 2–3 个测试 |

## 6. 五条不变量的判定规则（判决器规格）

| ID | 不变量（`spec.md`） | 判定输入 | 判定规则 |
|---|---|---|---|
| I-1 | 每次 `tools/call` 在审计链有且仅有一行并带 `X-Opskeeper-Audit-ID` | 轨迹里每次 `tools/call` 的 `audit_id` + 审计链 `mcp_tool_call` start 行 | ① 轨迹里每个非空 `audit_id` 都能在审计链按主键 id 找到对应行且 `payload.phase=="start"`；② 审计链里本会话的 start 行数 == 轨迹里**到达 handler 的** `tools/call` 数（鉴权即拒的调用只写 `mcp_tool_authorize` 行，不计入；两者靠 action 列区分，`port.go:434-435`）；③ 任何一次到达 handler 的调用的应答头缺 `X-Opskeeper-Audit-ID` 即红 |
| I-2 | `tools/list` 未列出的工具调用失败 | 探索前 `tools/list` 快照 + 每次调用的 tool name + 应答 | 调用名不在快照里 → 应答必须是 JSON-RPC error 或 HTTP 4xx；且审计链无该工具的 `mcp_tool_call` start 行（无副作用） |
| I-3 | write/destructive 进审批队列且不被自动放行 | 章程 role 的可见工具集 + 每次调用的 class + `/v1/approvals` 读回 | 若章程可见集里没有 write/destructive → `not-exercised`；有 → 每次调用后队列必须有对应 pending，且该调用的审计 end 行不存在（未执行） |
| I-4 | 调用不越出令牌的租户 | 令牌 claims 里的 tenant + 审计行 payload 里的 tenant | 所有 `mcp_tool_call` 行的 tenant 都 == 令牌 tenant；出现别的 tenant 即红 |
| I-5 | 应答不含凭据形状的字符串 | 轨迹里每个应答体 | 用一份闭集正则（`sk-`/`ghp_`/`AKIA`/PEM 头/`password=` 等形状）扫每个应答体；命中即红。正则复用 `scripts/audit_open_source.py:57-70` 的形状清单（同一份形状定义，避免两处漂移） |

**判决器必须是纯函数**：`(audit_rows, trace, before_tools, after_tools, charter) → Verdict`。
不读 `leads`，不调 LLM，不读系统时间（除了 `incomplete` 的预算比较）。

## 7. 对 delta spec 的修订（同 patch 提交）

一审 `spec.md` 后发现两处需要修订，理由如下：

### 修订 1：「探索前后 `tools/list` 一致」场景缺一个前置条件

`spec.md`「契约面不因探索而变化」的场景「探索前后工具清单相同」要求「用同一枚令牌各取一次
`tools/list`」。但 `spec.md` 同时要求「触及上限时通道终止探索，**令牌失效**」（「探索受预算
约束」场景）——若探索因预算终止，令牌已失效，第二次 `tools/list` 取不到。两条场景在
「跑飞」路径上互相矛盾。

**修订**：在「探索前后工具清单相同」场景补一句——「WHEN 探索**正常结束**（未因预算终止）」；
预算终止路径另加一个场景：「预算终止后工具清单不变」——用**第二枚**同身份令牌取 `tools/list`，
与探索前快照逐项相同。这样两条场景不再冲突，且「契约面不因探索而变化」在两条路径上
都被钉住。

### 修订 2：补「判决器对坏轨迹必须红」的元测试场景

`spec.md` 要求判决由确定性代码给出，但没有场景钉住「判决器本身不会假绿」。这是本 change
最大的单点风险（判决器有 bug → 整条通道变成噪音）。补一条场景：**用一条已知违反任一
不变量的轨迹喂判决器，判决必须是 fail，且不得因轨迹里附带的任何主观陈述而改判**。
这条场景同时就是首片 S1 的核心验收（§4 S1 第 5 步），把「首片能独立验证」写进 spec
而不是只写在 design 里；后半句把「线索不改变结论」从报告层下沉到判决器接口层——
判决器的输入类型里就没有放主观陈述的位置。

## 8. 回滚思路

- 探索通道是**纯增量**：新包 `tests/e2e/explore/`、新 make target、新 charter 目录。
  回滚 = 删掉这些文件 + 撤掉 nightly job。生产二进制零变更，无需回滚。
- 若判决器/S1 的 testenv DSN 访问器被认为越界：只回滚该访问器，判决器退化为
  「只读 HTTP 应答头里的 `X-Opskeeper-Audit-ID`」——精度下降但不阻塞通道。
- 已固化的回放用例（S3）是普通 e2e 测试，回滚 = 删测试文件；catalog 的 ✅ 同步撤回。
