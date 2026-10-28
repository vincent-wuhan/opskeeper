# 4.9 repair 插件强化方案（计划预览 / 独立验证 / 回滚说明 / 审批证据）

> 状态：**初稿，待评审**。对应 `tasks.md` 4.9；评审通过前不勾选 4.9。
>
> 基线：`main@07499880`（PR #109 之后）。
>
> 本文只出方案。第 10 节的实现切片**不在本任务内合入**；如要落地，请用户单独开 change，并先裁决第 4.4 节列出的 D1–D5。
>
> 审计范围：`plugins/pig-ops/opskeeper-sre-repair`（L2，5 工具）、`core/domains/control/repairpreview`、`core/manager/biz/aiops/tools/{recovery,configchange}`、`core/manager/biz/aiops/tools/decorators/review_gate.go`、`core/manager/biz/hitl`、`core/edge/{toolbroker,policygate}`、`core/manager/biz/loop/mcp_adapter.go`。均为只读审计，本任务不改任何一行。

---

## 0. 结论摘要

tasks.md 4.9 的四个关键词，现状不是同一个成色。先给结论，证据在第 2–3 节。

| 关键词 | 现状评级 | 一句话结论 | 缺口 |
|---|---|---|---|
| **计划预览** | 琥珀偏红 | 预览域本身是本仓最完整的一段（受控回放 + PASS/FAIL 闸门 + 指纹绑定），但"预览"今天只对 `resize_pool` 一个动作成立，且预览证据对模型不可读 | P1–P4 |
| **独立验证** | 琥珀 | verifier persona、角色矩阵里的执行/验证分离都在；但验证调用**不绑定它验证的那一次执行**，且测量工具住在 L2 写包里 | V1–V4 |
| **回滚说明** | **红** | 全链路只有一处 rollback 字段，是硬编码散文；回滚终态在数据层有模型、在生产代码里零调用者 | R1–R4 |
| **审批证据** | 琥珀 | 双签、提案预留/完成、审计账本三段都在；但预览绑定不是一等字段，两本账无共同证据 id，且有两处按**名字**而不是按 **class** 判定 mutation | A1–A5 |

一个贯穿四项的观察：**repair 这条链的强项在"闸门"，弱项在"证据对象"。** 每一道会不会放行的判定都写得非常好（preview gate、policygate、dualsign、proposal reservation）；但放行之后留下的是什么——预览里改了什么、验证的是哪一次执行、回滚怎么做、审批依据哪条证据——大多还是叙述、散文或埋在 `ArgsJSON` 里的原始参数。第 5–8 节的设计因此有一个共同形状：**把已有的闸门语义投影成可被引用、可被哈希、可被另一处对账的证据对象**，而不是新加一道闸门。

---

## 1. 目标与边界

### 1.1 目标

对 `opskeeper-sre-repair` 插件包与 `core/domains/control/repairpreview` 预览域，在四个关键词上：

1. 给出**有代码证据的现状审计**与缺口定性（§3，17 道）。
2. 对每个缺口给出**明确的设计归属**：落在插件包内、预览域内，还是宿主审批链（§4–§8）。
3. 给出**显式的 scope / 安全级别对照**，repair 是 L2、涉及写，任何表述不得降级（§9）。
4. 给出实现切片建议（§10），落地另开 change。

### 1.2 承载面与归属判定规则

repair 链上有三处可以放东西，判定规则只有一条：**谁拥有这个词表，缺口就归谁。**

| 承载面 | 拥有的词表 | 归属判据 |
|---|---|---|
| 插件包 `plugins/pig-ops/opskeeper-sre-repair` | 模型能看到的工具名、参数 schema、SKILL 纪律文本 | 缺口是"模型/操作员拿不到某个输入或某个输出字段"，且不要求新词表 |
| 预览域 `core/domains/control/repairpreview` | `Run`/`Candidate`/`WorkloadSpec` 的字段、决策规则、指纹算法 | 缺口是"预览记录本身缺少一个可绑定的对象" |
| 宿主审批链（`biz/hitl`、`decorators/review_gate.go`、`mcp_authorizer.go`、`dualsigngate.go`） | 提案负载字段、决策行字段、mutation 判定规则、双签规则 | 缺口是"审批/审计这条链自己产出的证据不完整" |

反过来的判定同样成立，并且同样重要：**一个缺口若需要改动它不拥有词表的那一侧，就必须写成跨面契约（谁产、谁消费），而不是让某一侧顺手扩。** §4.2 的归属总表按这条规则逐行给理由。

### 1.3 与相邻任务的边界

| 相邻任务 | 边界 | 本方案的处理 |
|---|---|---|
| `runtime-management-gap` 2.4 / 2.5 | 修复预览与只读诊断接入**共享任务生命周期**（任务 idempotency / admission / lease / checkpoint / 终态幂等） | **不动。** 本方案的预览增强全部发生在"一次预览 run 的内容"层面，不触碰 run 的调度、租约与生命周期模型。§5 的 `RepairPlanEvidence` 是 run 内的字段，不是新的任务类型 |
| 5.14（证据优先审批卡、normalized plan preview、hash 绑定、双签状态、break-glass 审计） | **呈现侧**：审批卡渲染什么、hash 如何绑定到一次决策、双签状态怎么展示、break-glass 怎么审计 | **不重叠。** 本方案只做**供给侧**：预览域与插件包产出"可被 5.14 呈现的证据对象"。§4.3 给出逐条切分 |
| 5.12 | 双签**数量**约束 | **不动。** 本方案不新增/减少任何一签 |
| 4.8（readonly / observability / middleware 业务缺口） | `opskeeper-sre-readonly` 包的能力清单 | 本方案不往 readonly 包加工具；`preview.read` 的包归属是 D3 决策点，默认落 repair 包，理由见 §6.3 |
| 4.10（manifest / 版本 / scope / 安全级别 / 签名 / 回滚文档台账） | 每个新增插件包的治理台账 | 本方案若新增工具（仅 `preview.read`），其 manifest 变更在 §9 单列对照，正式台账归 4.10 |
| PiG Runtime Adapter 3.6–3.9（工具对账 / 持久化调用记录） | PiG 侧的工具对账与调用持久化 | **不动。** 本方案的 `evidence_id`（§8.3）是 Manager 侧两本账的关联键，不进入 PiG 的 tool-call 持久化模型 |

### 1.4 非目标

1. 不做任务生命周期接入（`runtime-management-gap` 2.4）。
2. 不动审批双签数量约束（5.12 / 5.14 归属）。
3. 不改内核：不动 `core/floor/kernel/kind.go` 的六个 Kind，不新增 Kind、不新增 scope 常量（§9 论证 `preview.read` 不需要新 scope）。
4. 不做 L3 自治，不上调任何 profile 的 `Ceiling` / `MaxRadius`。
5. **不新增写工具。** 回滚工具（disable/delete alert rule、`systemctl start` 逆操作、`resize_pool` 逆操作）是本方案识别出的真实缺口，但每新增一个写工具都是 4.10 的一个治理对象；v1 只承诺"回滚说明**可读、可执行于人**"，工具化留给独立 change（D5）。
6. 不实现。本文档是方案，§10 是切片建议。

---

## 2. 现状证据：repair 链路全貌

### 2.1 链路图

```text
┌───────────────────────────────────────────────────────────────────────────────────────┐
│ 模型侧（插件包声明，5 工具）                                                            │
│  draft_config_change(R)  apply_config_change(W)  host_restart_service(W)              │
│  recovery.execute(W)     verify_recovery(R)                                            │
│  personas: opskeeper-repairer / opskeeper-verifier（同包 skills/，package.json pi.skills）│
└───────────────┬───────────────────────────────────────────────────────────────────────┘
                │ gate（session→actor，class 上限 viewer/operator/admin）
                │ broker（同 registry 二次校验 + receipt）        core/edge/{gatesocket,toolbroker,policygate}
┌───────────────▼───────────────────────────────────────────────────────────────────────┐
│ 宿主执行侧（Manager）                                                                  │
│  configchange.ConfigTool ── alertdraft 编译 ── alert_rule_manager（rollback 散文）      │
│  RestartServiceTool ── tunnel ── core/edge/restart_service                             │
│  RecoveryExecuteTool ── previewGate.Eligible ── auditRepo.Reserve/Complete             │
│  VerifyRecoveryTool ── MetricQuerier(PromQL) ── RecoveryStateStore                     │
│  装饰链 tenant_bind → pause_point → REVIEW_GATE → timeout → audit → ratelimit → metric │
└───────┬──────────────────────────┬─────────────────────────────────────┬──────────────┘
        │                          │                                     │
┌───────▼───────────┐   ┌──────────▼─────────────────┐   ┌───────────────▼──────────────┐
│ 预览域             │   │ HITL / 双签                │   │ 审计                          │
│ repairpreview     │   │ biz/hitl（proposal 状态机）│   │ chat_tool_calls（执行日志）    │
│  Run / Candidate  │   │ dualsigngate.go（ADR-019） │   │ chat_mutating_proposals（决策）│
│  WorkloadSpec     │   │ model.AgentTeamsPayload    │   │ edge audit ledger（hash 链）  │
│  Gate.Eligible    │   │                            │   │ mcp _meta.opskeeper_mcp_evidence│
└───────────────────┘   └────────────────────────────┘   └──────────────────────────────┘
```

### 2.2 五工具逐项对照

| 工具 | manifest class | BaseTool Class | scope | 走预览闸门 | 输出含回滚 | 输出含验证结论 |
|---|---|---|---|---|---|---|
| `draft_config_change` | read | read | —（不持久化） | 否 | 草稿含 `Rollback` 字符串 | — |
| `apply_config_change` | write | write | `alert.write` | 否 | `Rollback`（硬编码散文） | — |
| `host_restart_service` | write | write | `host.write` | **否** | **无字段** | — |
| `recovery.execute` | write | write | `host.write` | **是**（仅 AgentTeams caller） | **无字段** | — |
| `verify_recovery` | read | read | — | 否 | — | `VerifiedDelta` |

证据：`plugins/pig-ops/opskeeper-sre-repair/pig-ops.yaml`（`spec.tools` 五行、`safety_level: L2`、`required_scopes: [host.write, alert.write]`、`approval.required: true`、`max_blast_radius: pod`）；`core/manager/biz/aiops/tools/restart_service_basetool.go:137`（`restartServiceResultEnvelope` 无回滚字段）；`core/manager/biz/aiops/tools/recovery/recovery_execute_basetool.go:141`（`recoveryExecuteEnvelope` 无回滚字段）。

### 2.3 必须先说清的三个事实

这三个事实决定了后文所有缺口的定性，先立在这里：

1. **`approval.required: true` 是声明位，不是执行位。** `core/edge/policygate/policy.go:221` 的 `NeedsApproval` 只看 effective class 是否 `AtLeast(ClassWrite)`；包级 `approval.required` 不参与逐调用判定。一个 read 工具住在 `approval.required: true` 的包里，不需要任何审批就能跑——这是**正确**的行为（读不该等审批），但它意味着"`approval.required: true` = 这个包的每个调用都要人批"是一种容易产生的误读。§9 把它作为显式对照项写死。
2. **节点的角色天花板只有三档，且是 class 级的。** `cmd/opskeeper-edge/policy.go:45` 的 `roleCeiling`：`admin→destructive`、`operator→write`、其余→read。repairer / verifier 这两个 persona 名在**节点层不存在**；persona 分离只发生在 AgentTeams MCP 层（`core/base/pkg/auth/jwt.go:178` 的七角色工具矩阵）。所以"执行者与验证者是否共享状态"这个问题在两层的答案不同，§3.2 逐层回答。
3. **同一个验证能力有两个 wire 名。** `verify_recovery`（BaseTool / pig-ops manifest）与 `recovery.verify`（`core/manager/biz/loop/mcp_adapter.go:21` / teamharness `mcp/names.py`）。两个表面都活着，且 harness 的严格证据校验认的是后者（`core/harness/runner/real_agentteams.go:472`）。

---

## 3. 现状审计（本方案的首要产出）

### 3.1 P：计划预览

#### P1 预览证据对模型不可读 —— **红**

**问题**：repairer 在 AgentTeams 路径上**必须**提供 `preview_run_id` 与 `preview_candidate_id`（`recovery_execute_basetool.go:292-293`），但 94 个已声明工具里**没有任何一个**能读到预览内容。预览的唯一读回面是两个 HTTP 路由（`core/domains/server/incident/http.go:51` 的 `/v1/incidents/{id}/repair-preview-summary` 与 archive 里的 `repair_previews`），面向 UI，不面向模型。

**后果**：模型引用的是两个它无法打开的 id。它既不能向操作员复述"预览里 Candidate A 改了什么、为什么 PASS"，也不能在引用前确认这两个 id 属于当前 incident。这一步今天靠 Manager 在对话里转述——转述是叙述，不是证据。

**定性**：插件包缺口（模型侧缺输入）+ 预览域缺口（缺一个面向工具的只读投影）。归属见 §5.2。

#### P2 预览动作词表与执行器命令词表不相交（除一例外） —— **红**

**问题**：预览执行器只认两个动作，`core/domains/control/repairpreview/runner.go` `applyCandidateAction` 的 switch 只有 `resize_pool` 与 `reset_pool`；而 `recovery.execute` 的 `parameters.command` 是 `restart_service | kill_process | resize_pool | noop`（`recovery_execute_basetool.go:85`）。预览闸门按 `params.Command` 反查候选（`gate.go` `Eligible(..., action)` → `selected.Action != action → ErrNotEligible`）。

**后果**：对 AgentTeams caller，`restart_service` 与 `kill_process` 这两个命令**没有任何可生成的预览**，调用必然在预览闸门处被拒；而 `host_restart_service` 这个直连工具又**完全不走预览闸门**。于是"重启 systemd 服务"这条最常用的修复路径处在两头都不覆盖的位置：走 `recovery.execute` 会被闸门拒（因为没有预览可引），走 `host_restart_service` 则连预览都不需要。同一个动作，安全性取决于入口而非动作本身。

**定性**：词表缺口，横跨预览域与宿主执行器。这是 17 道里唯一一道**必须在两个 owner 之间裁决**的，见 D1。

#### P3 预览里"改了什么"是叙述，不是对象 —— **琥珀**

**问题**：`Candidate.ChangeSummary` 是 workload YAML 里的自由文本（`deploy/repair-preview/pg-pool-workload.yaml` 的 `change_summary: Increase preview pool capacity`），而真正执行的 SQL 硬编码在 `runner.go` `applyCandidateAction`（`UPDATE repair_preview_pool_settings SET pool_size = 40 WHERE id = 1`）。两者之间**没有任何绑定**：改 YAML 的文案不需要改 SQL，改 SQL 也不需要改文案。`Candidate` 结构里没有参数、没有语句、没有指纹能对上"实际执行了什么"。

**后果**：预览证据链上最关键的一环——"这个 PASS 的候选到底做了什么"——是不可验证的。审批卡若引用 `ChangeSummary`，引用的是一句人写的话。

**定性**：预览域缺口。§5.1。

#### P4 预览候选无参数形状，无法与执行参数对账 —— **琥珀**

**问题**：`WorkloadSpec` 的 `CandidateSpec`（`workload.go:49`）只有 `candidate_id / name / kind / action / change_summary` 五个字段，没有参数。而 `recovery.execute` 的 `parameters` 是带 `device_id / service / pool_manifest_id / preview_run_id / preview_candidate_id` 的结构化对象，且 HITL 提案负载里的 `RecoveryExecutionParameters`（`core/manager/model/hitl/model.go:98`）会把这些参数做 canonical JSON + SHA256。

**后果**：提案负载的 hash 覆盖了执行参数，预览候选没有任何可被 hash 的参数。于是"审批时看到的参数"与"预览时验证过的参数"之间**不存在可机器对账的字段**，只能靠 `action` 名字对齐（P2 已说明这个名字对齐只覆盖一个动作）。

**定性**：预览域缺口 + 提案负载缺口的组合。§5.1 / §8.1。

### 3.2 V：独立验证

#### V1 验证调用不绑定它验证的那一次执行 —— **红**

**问题**：`verify_recovery` 的入参是 `skill_id / target / resource_type / baseline_window / compare_window / tolerance / metrics / sensitivity`（`verify_recovery_basetool.go:84`），**没有** `incident_id`、`proposal_id`、`preview_run_id`，也没有任何执行标识。它的输出 `VerifyOutcome` 同样不含任何对被验证动作的引用。

**后果**：verifier 交回的"passed=true"在证据链上**指向不明确**。它证明的是"这台主机的 CPU 在这两个窗口的对比在容差内"，而不是"**那次**被批准的 `recovery.execute`（proposal X，preview candidate A）生效了"。两者的差别在复盘时才显现：一个 incident 有三次修复尝试时，第三次 verify 的 passed 无法在档案里挂到任何一次执行上。harness 的严格证据校验（`real_agentteams.go:496-500`）也只要求"verifier 调过一次 `recovery.verify`"，不要求它与 `recovery.execute` 的 proposal / payload hash 对上——执行那一条要求了，验证那一条没要求，这个不对称本身就是缺口。

**定性**：插件包缺口（入参/出参 schema）+ 宿主链缺口（无关联键）。§6.1 / §8.3。

#### V2 测量工具住在 L2 写包里 —— **琥珀**

**问题**：`verify_recovery` 在 `opskeeper-sre-repair`（L2）的 `spec.tools` 里声明为 `class: read`。`opskeeper-sre-readonly`（L1）**也有一个同名的 verifier persona**（`plugins/pig-ops/opskeeper-sre-readonly/skills/opskeeper-verifier/`），但那个包的 20 个工具里没有 `verify_recovery`。

**后果**：一个只装了 readonly 包的节点，verifier persona 有名无器——技能文本要求它"用 `verify_recovery` 拿数字"，而它拿不到这个工具。要获得验证能力必须安装整个 L2 写包，也就是**验证能力的可用性与写能力的安装绑死**。这与"独立验证"的方向相反：越想让验证独立，越不应该让验证依赖写包的存在。

**定性**：包归属缺口。D3 决策点。注意这不是安全级别错误——`class: read` 是准确的，问题在**包的选择**，不在级别的选择。

#### V3 verifier 角色的证据面窄于其自身技能的承诺 —— **琥珀**

**问题**：AgentTeams 角色矩阵给 verifier 的工具是 `recovery.verify / query_knowledge / incident.record`（`core/base/pkg/auth/jwt.go:218-222`）。而 repair 包里的 `opskeeper-verifier/SKILL.md` 要求它做四件事：拿前后测量数字、确认症状真消失、**检查邻居**（"A repair that fixed one service by taking resources from another has moved the incident"）、说清**无法验证什么**。"检查邻居"需要 `query_promql` / `get_host_load`——这两者在矩阵里只属于 investigator。

**后果**：技能承诺的行为与角色实际可达的工具集不一致。verifier 要么会违规借道（走不通，会被 `FilterToolNames` 拒），要么会静默降级为只报 `verify_recovery` 的结论——后者正是 SKILL.md 自己警告的"unverifiable claim reported as verified"的反面：**该查而查不了的邻居，不会出现在"无法验证什么"里，因为它根本不在 verifier 的视野里**。

**定性**：宿主链缺口（角色矩阵）+ 插件包缺口（SKILL 承诺与工具面不对齐）。§6.3。

#### V4 同一能力双名，两套表面各自存活 —— **琥珀**

**问题**：`verify_recovery`（BaseTool，`recovery/verify_recovery_basetool.go:55`）与 `recovery.verify`（loop MCP adapter，`mcp_adapter.go:21`）是同一能力的两个 wire 名。teamharness 侧 `mcp/names.py` 明确记录了这层关系，但 `plugin.yaml` 的 `mcp.servers[].tools` 列的是 `recovery.verify`，pig-ops manifest 列的是 `verify_recovery`。同样地 `host.restart_service`（plugin.yaml）与 `host_restart_service`（backend `ToolNameRestartService`）是第二对。

**后果**：这是 4.1 §3 已经记录过的"工具面一致性"问题在 repair 域的具体形态。双名不是即刻的安全缺口（两条路各自都有闸门），但它是漂移的温床：一侧改 schema、加参数（本方案 §6.1 正要这么做），另一侧若不同步，就会出现"AgentTeams 路径能验证、节点路径不能"或反之。**本方案的 V1 修复恰好会触发这个风险**，所以 D2 必须与 V1 同裁决。

**定性**：词表/命名缺口，归宿主链。§6.2。

### 3.3 R：回滚说明

#### R1 两个核心写工具的输出没有任何回滚对象 —— **红**

**问题**：`host_restart_service` 的 `restartServiceResultEnvelope`（`restart_service_basetool.go:137-151`）有 `DeviceID / Service / Restarted / Mocked / Argv / StartedAt / EndedAt / Error`，**没有回滚字段**。`recovery.execute` 的 `recoveryExecuteEnvelope`（`recovery_execute_basetool.go:141-148`）有 `SchemaVersion / IncidentID / Executed / Command / RestartedAt / ResultJSON`，**也没有**。

**后果**："重启是可逆的——`systemctl start` 能把它带回来"这句话只存在于代码注释里（`restart_service_basetool.go:28`），不存在于任何操作员或模型能读到的输出里。SKILL.md 要求 repairer"Write the rollback before you call the tool"，但工具输出不提供回滚内容，意味着回滚说明只能由模型自己生成——也就是由最不该独自生成它的那一方生成。

**定性**：插件包缺口（输出 schema）+ 宿主执行器缺口（谁来填）。§7.1。

#### R2 `apply_config_change` 的 rollback 是硬编码散文，且无对应工具 —— **红**

**问题**：`core/manager/biz/aiops/alertconfig/alert_rule_manager.go:169` 与 `:310` 把 `Rollback` 硬编码为同一句："可在 Alerts 规则列表中禁用或继续编辑该规则。" 94 个已声明工具里**没有** disable / delete / update alert rule 的工具（工具清单核对：readonly 20 / observability 12 / middleware 56 / repair 5 / autonomy 1，无一能撤销一次 `create`）。

**后果**：回滚说明既不含被创建规则的 `resource_id`（apply 结果里有 `ResourceID`，但 rollback 文案不引用它），也不指向任何可执行路径。它是一句对任何规则都成立的通用话，也就是对任何规则都不具体的话。

**定性**：宿主链缺口（文案生成）+ 工具面缺口（无逆操作，属 D5 非目标）。§7.1 / §7.2。

#### R3 回滚终态有模型、无生产者 —— **红**

**问题**：数据层完整地支持回滚记录：`core/manager/data/aiops/store/mutating_proposal.go:149` 的 `MarkRolledBack` 会做 `DecisionRolledBack` 迁移并打 `rolled_back_at` / `rollback_of`；`core/manager/biz/hitl/proposal.go:69` 的 `CompleteOpts.State` 也接受 `rolled_back`。**两者在生产代码里都没有任何调用者**（全仓 grep，仅测试与定义）。同时 `mcp_adapter.go` 的 `MCPVerifyOutput.RollbackRecommended` 是 `!verified.Passed` 的布尔值——它说"建议回滚"，但系统里没有任何东西能接住这个建议。

**后果**：一条完整的回滚审计通路建好了，两头的门都没接。"verify 失败 → 建议回滚 → 回滚被记录 → 回滚被验证"这条链，今天只有第一环和最后一环的模型存在。

**定性**：宿主链缺口（接线缺失，不是模型缺失）。§7.3。

#### R4 预览的 baseline 分支即建即删，无法承担回滚演练 —— **琥珀**

**问题**：`runner.go` `executeBranch` 里 `CREATE SCHEMA` 后 `defer DROP SCHEMA ... CASCADE`。`Candidate.WriteImpact` 恒为 `preview_only`、baseline 恒为 `none`。

**后果**：预览能回答"这个候选在受控负载下不比 baseline 差"，**不能**回答"如果上生产后要撤回，撤回到什么状态、要多久、会不会丢数据"。spec `repair-preview-readback` 的措辞是"controlled workload boundary"，没有承诺回滚演练——所以这是**能力边界**而不是**违约**，但它直接限制了对"回滚说明"的支撑力度：审批卡上的回滚说明今天没有任何预演证据可引。

**定性**：预览域的能力边界，如实记录，v1 不扩（扩它等于把预览域改成回滚演练平台，成本与 4.9 不成比例）。§7.4。

### 3.4 A：审批证据

#### A1 预览绑定在决策行里不是一等字段 —— **琥珀**

**问题**：ReviewGate 的 `MutatingProposalEvent`（`decorators/review_gate.go:190`）字段为 `ToolName / ArgsJSON / ToolClass / ReviewerAgent / ReviewerTaskID / OperatorUserID / SessionID / CreatedAt`。`preview_run_id` / `preview_candidate_id` 只能作为 `ArgsJSON` 的原文存在。HITL 提案负载（`model.AgentTeamsPayload`）里它们倒是结构化的（`model.go:106-107`），但那是另一张表、另一条链。

**后果**：决策日志（`chat_mutating_proposals`）无法按预览反查。要回答"这个 preview candidate 被批了几次、被谁批的、有没有被拒过"，得对 `ArgsJSON` 做字符串匹配——也就是把一个结构化问题降级成一个正则问题。

**定性**：宿主链缺口。§8.1。

#### A2 提案负载 hash 与预览候选指纹之间无绑定 —— **琥珀**

**问题**：`hitl.CanonicalPayloadHash` 对 `AgentTeamsPayload`（含 `RecoveryExecutionParameters`，含 `PreviewRunID` / `PreviewCandidateID`）做 canonical JSON + SHA256；预览域有自己的 `WorkloadFingerprint` / `BindingFingerprint` / `ResultChecksum`。**两组指纹从不互相引用**。

**后果**：审批时操作员看到的 hash 覆盖"要执行什么参数"，不覆盖"这个参数组合在预览里得到了什么结论"。反过来，预览域也不知道自己的哪个候选被哪次提案引用。P4 是它的字段侧成因。

**定性**：横跨提案负载与预览域的契约缺口。§8.1 / §8.2。

#### A3 执行闸门只校验 workload 指纹 —— **琥珀**

**问题**：`repairpreview.NewGate(repository, expectedWorkloadFingerprint)`（`gate.go:23`）在执行时校验：run 绑定 tenant/incident/run、候选存在且 `Decision == PASS`、候选 `Action` 与命令一致、run 的 `WorkloadFingerprint` 等于**部署期配置的常量**。它**不**校验：`TargetFingerprint` 是否仍是当前目标（target 指纹只在预览执行时由 `ReadTargetIdentity` 比对过一次，`preview_executor.go:89-95`），预览 run 相对于提案批准时刻是否新鲜（无时间窗），以及候选的 `ResultChecksum` 是否与提案批准时一致。

**后果**：审批与执行之间的时间窗内，目标状态可以变化（预览针对的是 pool A 的指纹，执行时 pool 已重建），闸门不会发现。`ErrStaleWorkload` 只防 workload 文件升级，不防目标漂移。

**定性**：预览域缺口（闸门校验集）+ 提案负载缺口（无批准时快照可对）。§8.2。

#### A4 两本账无共同证据 id —— **琥珀**

**问题**：Manager 侧的 `chat_mutating_proposals`（决策）/ `chat_tool_calls`（执行）与节点侧的 edge audit ledger（`core/ports/audit.go:34` 的 `AuditEntry`，带 `PrevHash`/`Hash` 链）各自完整，但**没有任何共享键**。MCP 表面有 `_meta.opskeeper_mcp_evidence`（`mcp/http.go:751-778`，含 `audit_log_id` + `arguments_sha256` + `response_sha256`），这是最接近证据对象的东西，但它只覆盖 MCP 这一条路，且 `audit_log_id` 与 proposal id 之间没有桥。

**后果**：一次修复的"决策、执行、验证"三段证据分属三处，靠 incident_id 和时间戳做软关联。这正是 5.14 要在呈现侧解决的"证据优先审批卡"的供给侧前提——**没有共同证据 id，审批卡要么只能展示一段，要么得自己发明关联**。

**定性**：宿主链缺口，且必须与 5.14 划清供给/呈现边界。§4.3 / §8.3。

#### A5 两处按名字而非按 class 判定 mutation —— **琥珀**

**问题**：两处。
- `core/manager/biz/aiops/agent/agent.go:232` 的 `legacyKernelMutatingTools` 只列了 `host_restart_service`，**不含** `recovery.execute` 与 `apply_config_change`。注释自己写着"every PR-N addition of a write/destructive tool MUST add its wire name here"——这条纪律没有被遵守。
- `core/manager/biz/loop/mcp_authorizer.go:149-158` 的 `roleAllowsMutation` 只认 `strings.HasSuffix(tool, ".execute")` 与 `tool == "host_restart_service"`，**不含** `apply_config_change`。

**后果**：两处都是**按名字枚举 mutation**，而本仓已经有一个按 class 判定的权威词表（`domain.ToolClass.AtLeast(ClassWrite)`，`core/edge/policygate/policy.go:221` 正确地用了它）。`apply_config_change` 今天不在任何角色的 `Tools` 列表里，所以这是**潜伏**而非现行漏洞；但"潜伏"意味着下一个把这个工具加进某个角色的人，会得到一个没有任何告警的绕过。`recovery.execute` 在 legacy kernel 路径上则是现行的：legacy 内核没有 ReviewGate 装饰链（`agent.go:225-231` 的注释承认这一点），deny-list 是唯一的网，而网上有洞。

**定性**：宿主链缺口，且是 17 道里唯一一道**修复本身很小但必须谨慎论证影响面**的。§8.4。

### 3.5 审计小结

| 组 | 红 | 琥珀 | 合计 |
|---|---|---|---|
| P 计划预览 | P1, P2 | P3, P4 | 4 |
| V 独立验证 | V1 | V2, V3, V4 | 4 |
| R 回滚说明 | R1, R2, R3 | R4 | 4 |
| A 审批证据 | — | A1, A2, A3, A4, A5 | 5 |
| **合计** | **6** | **11** | **17** |

为什么是这 17 道而不是更多：审计时另外排除掉了三类。一是**属于 2.4 的**（预览 run 不走共享任务生命周期、无租约无 checkpoint）——非目标；二是**属于 5.14 的**（审批卡上双签状态怎么展示、break-glass 怎么审计）——呈现侧；三是**修复后不改变行为的描述性观察**（例如 §2.3 的事实 1、R4 的能力边界），它们写下来是为了让对照显式，不是要改。

---

## 4. 归属判定与不重叠论证

### 4.1 归属规则回顾

见 §1.2。一句话：**词表归谁，缺口归谁；要跨面就写成契约。**

### 4.2 缺口归属总表

| 缺口 | 归属 | 理由 | 设计落点 |
|---|---|---|---|
| P1 | **插件包 + 预览域**（契约） | 模型缺输入（工具），预览缺投影（域内只读视图）。两侧各自拥有自己那半个词表 | §5.2 |
| P2 | **预览域 + 宿主执行器**（需 D1 裁决） | 动作词表一边在预览域、一边在执行器，谁单方面改都会造成另一侧静默失效 | §5.3 |
| P3 | **预览域** | `ChangeSummary` 与实际语句都是预览域自己的字段/代码 | §5.1 |
| P4 | **预览域**（字段）+ **宿主链**（对账消费方） | 参数形状在 `CandidateSpec`，对账发生在提案负载 | §5.1 / §8.1 |
| V1 | **插件包**（schema）+ **宿主链**（关联键） | 入出参是插件包的；关联键要落提案与审计 | §6.1 / §8.3 |
| V2 | **插件包归属决策**（D3） | 不是级别错误，是包选择问题 | §6.3 |
| V3 | **宿主链**（角色矩阵）+ **插件包**（SKILL 对齐） | 矩阵在 `core/base/pkg/auth`；SKILL 承诺在包内 | §6.3 |
| V4 | **宿主链**（命名裁决，D2） | 两个 wire 名都由 Manager 侧定义 | §6.2 |
| R1 | **插件包**（输出 schema）+ **宿主执行器**（内容来源） | envelope 在包内声明，内容由宿主实现填 | §7.1 |
| R2 | **宿主链**（文案）+ **工具面缺口留 D5** | 文案生成在 `alertconfig`；逆操作工具是独立治理对象 | §7.1 / §7.2 |
| R3 | **宿主链**（接线） | 模型与数据层都已存在，缺生产者 | §7.3 |
| R4 | **预览域**（如实记录，v1 不扩） | 能力边界，扩它超出 4.9 比例 | §7.4 |
| A1 | **宿主链**（决策行字段） | `chat_mutating_proposals` 的行形状归审批链 | §8.1 |
| A2 | **预览域 + 宿主链**（契约） | 指纹在预览域，引用在提案负载 | §8.1 / §8.2 |
| A3 | **预览域**（闸门校验集）+ **宿主链**（批准时快照） | `Gate.Eligible` 在预览域；快照要在提案侧存 | §8.2 |
| A4 | **宿主链**（共同证据 id） | 两本账都在宿主 | §8.3 |
| A5 | **宿主链**（判定规则） | 两处枚举都在 Manager | §8.4 |

**没有任何一道缺口落在"改内核"或"新增扩展点"上。** 这一点值得写明：repair 链的所有缺口都能在既有承载面内解决，不需要 `core/floor/kernel/kind.go` 的任何变化——这也是 4.9 能在"插件能力恢复"阶段（而非架构阶段）成立的原因。

### 4.3 与 5.14 的边界：供给侧 vs 呈现侧

5.14 的原文是"证据优先审批卡、normalized plan preview、hash 绑定、双签状态和 break-glass 审计"。这五件事里，前三件与本方案相邻，必须切干净。切法是**按"谁产、谁呈现"**：

| 5.14 的词 | 4.9 做什么（供给侧） | 5.14 做什么（呈现侧） | 不重叠论证 |
|---|---|---|---|
| 证据优先审批卡 | 产出**可被引用的证据对象**：`RepairPlanEvidence`（§5.1）、`preview.read` 投影（§5.2）、`RollbackPlan`（§7.1）、`evidence_id`（§8.3） | 决定**卡上排什么、什么顺序、什么算"证据优先"** | 4.9 不写任何 UI/卡片布局，不定义"优先"；5.14 不定义证据对象的字段形状。若 5.14 需要一个 4.9 没有的字段，走"4.9 补供给侧"而不是"5.14 自己发明" |
| normalized plan preview | 提供预览的**规范化数据源**（`Candidate` 结构化字段 + 参数 + 指纹） | 决定**规范化文本如何渲染**（排序、单位、双语、截断） | 4.9 的"normalized"止于数据结构；文本形态归 5.14 |
| hash 绑定 | 提供**可被绑定的 hash**：预览候选指纹进提案负载（§8.1），执行闸门做三重对账（§8.2） | 决定 hash **绑定到哪一次决策**、如何在卡上展示与校验 | 4.9 不触碰 `dualsigngate.go` 的签名数量与校验逻辑，不新增签名，不改 `Scope`/`Signer` 的语义 |
| 双签状态 | **不做**（5.12/5.14 归属） | 全部 | — |
| break-glass 审计 | **不做**（属审批链自身的例外通道） | 全部 | — |

一句话版：**4.9 让证据"存在且可引用"，5.14 让证据"被看见且被绑定"。** 若评审认为某一条越界，越界的一定是 4.9 这一侧，请按上表退回。

### 4.4 决策点（评审需先裁决，落地切片依赖）

| # | 决策 | 选项 | 本方案倾向 | 理由 |
|---|---|---|---|---|
| **D1** | 预览动作词表与执行命令词表如何统一 | (a) 扩预览词表，为 `restart_service` / `kill_process` 提供预览语义；(b) 在 `recovery.execute` 的 schema 与 SKILL 中显式声明"AgentTeams 路径仅 `resize_pool` 有预览"，其余命令必须走非 AgentTeams 路径 | **(b)** | (a) 意味着预览域要能重建"一台主机的 systemd 单元状态"，这与预览域当前"disposable preview-pg + 固定负载回放"的隔离边界（`Run.IsolationBoundary`（`model.go:35`））不兼容，成本与 4.9 不成比例。(b) 是**把现状写成诚实的契约**而非掩盖：今天 (a) 的能力不存在，掩盖它的方式是让闸门静默地拒，而诚实的方式是在 schema 与 SKILL 里说清楚 |
| **D2** | `verify_recovery` / `recovery.verify` 双名 | (a) 统一为一个名（含兼容期别名）；(b) 保持双名，但加一条对齐测试 | **(b)，(a) 作为后续独立 change** | 本方案 §6.1 要改 schema，若同时改名则一次变更同时触碰命名、schema、两份 manifest、harness 严格校验——评审面过大。先加对齐测试把漂移变成会失败的测试，改名单独走 |
| **D3** | `preview.read` 工具放哪个包 | (a) `opskeeper-sre-repair`；(b) `opskeeper-sre-readonly` | **(a)**，理由是 4.9 的边界 | (b) 能同时解决 V2（readonly 节点获得验证工具），但 4.8 才是 readonly 包的 owner，4.9 越过去会让两个任务的评审面交叉。倾向 (a) + 把 (b) 作为 V2 的**记录在案的建议**交给 4.8 |
| **D4** | `host_restart_service` / `recovery.execute` 的 class 表述 | (a) 维持 `write`，在词汇表处记录一次显式例外；(b) 上调为 `destructive` | **(a)**，**但必须落档** | 见 §9.2 —— 这是 17 道之外发现的一个词汇表张力，两选项都有代价，静默维持任何一个都是错的 |
| **D5** | 是否新增回滚工具 | (a) v1 不新增，只做"可读、可执行于人"；(b) 新增逆操作工具 | **(a)** | 见 §7.2 与 §1.4 非目标 5 |

---

## 5. 强化设计 A：计划预览

### 5.1 `RepairPlanEvidence`：把"改什么"从叙述变成对象（P3 / P4，预览域）

预览域新增一个**从 `Run`/`Candidate` 派生**的证据对象，不新增持久化表（run 里已有的字段够用，缺的字段随本设计补进 `Candidate`）：

```text
RepairPlanEvidence          // 预览域导出，供 §5.2 的工具与 §8.1 的提案负载消费
  run_id, tenant_id, incident_id
  scenario_id, idempotency_key, target_fingerprint, binding_fingerprint   // 既有
  workload_fingerprint, workload_revision, isolation_boundary              // 既有
  plan_digest                // 新：候选参数 + 动作 + change_summary 的 SHA256
  candidates[]:
    candidate_id, name, kind, action
    parameters               // 新：CandidateSpec.parameters（P4）
    change_summary
    plan_digest              // 新：该候选自己的参数指纹（P3）
    decision, rejection_reason
    consistent, business_probe_pass, write_impact
    average_latency_ms, p95_latency_ms, tps, sample_count, storage_delta_bytes
```

三条设计约束：

1. **`plan_digest` 是"实际执行了什么"的指纹，不是"文案说了什么"的指纹。** 计算材料是 `action + parameters`，`change_summary` 只作展示字段、不进 digest。这样 P3 的"文案与实现可以各自漂移"被降级为"文案可以漂移但指纹不会"——文案漂移最多是显示问题，指纹漂移会让 §8.2 的对账失败，是硬错误。
2. **`parameters` 进 `CandidateSpec`（workload YAML），且 `LoadWorkload` 的 `KnownFields(true)` 会拒绝未知字段**——这是预览域已经有的纪律（`workload.go:60`），新字段天然被它约束，YAML 里写了 schema 不认识的参数会在加载时失败而不是运行时静默丢弃。
3. **不引入新持久化。** `RepairPlanEvidence` 是 `Run` 的只读投影。这样它与 `runtime-management-gap` 2.4 的任务生命周期工作**结构上不冲突**：2.4 改的是 run 怎么被调度，本设计改的是 run 里有什么。

**不做的事**：不改 `Evaluate` 的判定规则，不改 `allowedWriteImpact` 的三值闭集，不放宽 `SanitizeErrorSummary` 的脱敏。预览域的判定语义是健康的，缺的是证据形状，不是判定。

### 5.2 `preview.read` 工具：让预览证据对模型可读（P1，插件包 + 预览域契约）

插件包新增一个 `class: read` 工具，`opskeeper-sre-repair/extensions/opskeeper-sre-repair/tools.go` 的清单从 5 变 6：

```text
preview.read
  入参： incident_id（必填）, run_id（可选，缺省取该 incident 最近一次）
  出参： RepairPlanEvidence 的 §5.1 投影（脱敏后）
  class: read, scope: 无新增（见 §9.3）
```

设计约束：

1. **read-only、幂等、无副作用**，与 `draft_config_change` 同级。`approval.required: true` 的包级声明对它**不产生逐调用审批**（§2.3 事实 1），这是正确行为：让 repairer 在引用前能打开预览，本身就是降低而非提高风险。
2. **数据来源是宿主的预览 store**，与 `/v1/incidents/{id}/repair-preview-summary` 同源，不另建通道。工具经 broker 上call 到控制面（repair 包所有工具的既有路径），所以**不需要新的出口、不需要新凭据**。
3. **复用既有脱敏**：出参经过 `SanitizeErrorSummary` 同源的规则，且不返回原始 SQL 语句文本（只返回 `action` + `parameters` + digest）——预览的 SQL 是合成 fixture 的，但把"返回语句文本"养成习惯会在这条链扩到非合成目标时变成泄露面。
4. **SKILL 同步**：`opskeeper-repairer/SKILL.md` 的"approval round trip"第 1 步（"State the change, the evidence for it, the blast radius, and the rollback"）补一句：证据从 `preview.read` 读，不要凭记忆复述。这是把 P1 的修复钉进纪律文本，防止工具存在但纪律不引用它。

### 5.3 动作词表统一（P2，按 D1 裁决）

按 D1 倾向 (b)，落点是**两处显式声明**而非扩预览：

1. `recovery.execute` 的 schema：`parameters.command` 的 description 改为明示"AgentTeams callers: only `resize_pool` has a preview candidate vocabulary; `restart_service` / `kill_process` will be refused by the preview gate"。
2. `opskeeper-repairer/SKILL.md`：在"Never reach for"或"When not to repair"里加一条对齐说明。

同时，**`host_restart_service` 不走预览闸门这一事实**要写进 `pig-ops.yaml` 的注释里（它已经有非常优秀的注释文化），说明这是有意的设计而非遗漏：直连工具走的是 ReviewGate + 双签路径，预览闸门是 AgentTeams 闭环路径的专属前置。今天这个区别只存在于代码结构里，写进 manifest 注释才会在评审一个 diff 时被看见。

若评审裁决为 D1(a)，则本节作废，改为：预览域为 `restart_service` 定义"单主机单单元"的候选语义，并在 `Run.IsolationBoundary` 字段与 spec `repair-preview-readback` 里显式扩展隔离边界的含义（预览域将不再是"disposable preview-pg"一种形态）。**这条路的成本必须由裁决者签字，不能由实现者顺手走。**

---

## 6. 强化设计 B：独立验证

### 6.1 验证绑定到被验证的那一次执行（V1，插件包 schema + 宿主链关联键）

`verify_recovery` 的入参与出参各加一组**可选但强烈建议**的绑定字段：

```text
入参新增（全部 optional，保持向后兼容）：
  incident_id      // 与 recovery.execute 同名同义
  proposal_id      // 同上；提供时宿主校验该提案已 terminal
  execution_ref    // {evidence_id}（§8.3），最强绑定

出参新增：
  verified_ref     // 回显上述三者中已提供的
  evidence_id      // 本次验证自己的证据 id（§8.3）
```

三条设计约束：

1. **可选而非必填**，理由是 `verify_recovery` 有一个合法的独立用途——ops 驱动的抽查（`core/manager/server/loop/http.go:451` 的"ops-driven re-checks"）。把它改成必须绑提案，会废掉这条只读路径。
2. **提供即校验**：给了 `proposal_id` 而该提案不存在 / 非 terminal / 不属于该 incident，返回错误而不是静默忽略。一个被忽略的绑定字段比没有绑定字段更危险，因为它制造了"我引用了"的假象。
3. **harness 严格证据校验同步加一条**：`real_agentteams.go` 的 `validateMCPEvidence` 中，`recovery.verify` 分支目前只查 `call.Role == "verifier"`；应加上"若存在 `recovery.execute` 调用，则 verify 必须回显同一 `proposal_id`"。这一条是**把 V1 从"字段存在"提升为"闭环被校验"**的关键，不加它，字段就是装饰。

**不做的事**：不给 verifier 开任何写能力。验证的独立性来自"它不能改"，不是来自"它和修复者跑在不同进程里"。

### 6.2 双名对齐（V4，按 D2 裁决）

按 D2 倾向 (b)：**加一条对齐测试，不改名。**

落点是 `core/manager/biz/loop/` 或 `core/harness/` 的一个测试：断言 `recovery.verify`（MCP adapter）与 `verify_recovery`（BaseTool）的入参 schema 字段集合一致、出参的判定字段（`passed` / `deltas` / `tolerance` / `warning_level`）一一对应。这条测试存在的意义是把"§6.1 只改了一侧"变成一个会红的测试，而不是一次会漏的 code review。`host.restart_service` / `host_restart_service` 这一对一并覆盖。

若后续走 D2(a) 统一命名，兼容期做法是 MCP adapter 保留旧名为别名并在 `tools/list` 标注 deprecated，pig-ops manifest 与 teamharness `plugin.yaml` 同步切换，harness 严格校验同时接受两名并在证据里记录实际使用的名字。

### 6.3 验证者的证据面（V2 / V3）

V2 与 V3 是同一个问题的两面：**verifier 要么缺工具（V2），要么缺权限（V3）**。本方案的处理是"把缺口写成显式契约，把扩面留给 owner"：

1. **V2（D3 倾向 (a)）**：`preview.read` 落 repair 包。同时在本文档记录：**建议 4.8 评估把 `preview.read`（或一个等价的只读投影工具）贡献进 `opskeeper-sre-readonly`**，因为预览证据是只读事实，它的可用性不应依赖 L2 写包的安装。这条建议**不在本任务执行**，避免与 4.8 的评审面交叉。
2. **V3（宿主链，角色矩阵）**：verifier 角色的工具集应至少包含它能独立完成 SKILL 承诺所需的最小只读集。本方案建议的具体清单：`recovery.verify`、`query_promql`（邻居检查）、`preview.read`（读它要验证的那个预览）、`query_knowledge`、`incident.record`。**注意这是建议，落点在 `core/base/pkg/auth/jwt.go` 的 `WorkerPermission` 矩阵，且该矩阵有 CI 级的 spec 对账（`jwt.go:156-159`），所以这条变更必须伴随 spec 的同步更新——这是宿主链自己的纪律，4.9 只提出需求。**
3. **SKILL 对齐（插件包）**：在裁决前，`opskeeper-verifier/SKILL.md` 的"Say what you could not verify"一节应加一句："如果本角色的工具面不含某个测量能力，把它列为无法验证项，而不是用叙述替代测量。"这把 V3 从"能力缺失"转成"能力缺失被如实披露"，是零成本的诚实化。

**关于"与修复者是否共享状态"的正面回答**（issue 原文的提问）：两层答案不同，都要写下来。

- **AgentTeams MCP 层：不共享。** `WorkerPermission` 矩阵给 repairer 的是 `recovery.execute / query_knowledge / incident.record`，给 verifier 的是 `recovery.verify / query_knowledge / incident.record`——交集只有 `query_knowledge` 与 `incident.record`，两个都是只读/追加。执行与验证在工具面上是分离的，且由 `FilterToolNames`（`mcp_authorizer.go:91`）在 `tools/list` 与 `tools/call` 两处强制。
- **节点层：共享。** `roleCeiling` 只有 viewer/operator/admin 三档且是 class 级；一个 `operator` 角色的会话可以同时运行 `recovery.execute` 与 `verify_recovery`。这不是缺陷（节点层的威胁模型是"操作员授权的会话"，不是"persona 互斥"），但它意味着**"执行者与验证者分离"这个保证只在 AgentTeams 路径上成立**。这个边界应该写进 `pig-ops.yaml` 的注释，写明 persona 分离由 MCP 层的角色矩阵提供、节点层只提供 class 天花板。

---

## 7. 强化设计 C：回滚说明

### 7.1 `RollbackPlan`：三个写工具的输出都带结构化回滚（R1 / R2）

定义一个统一的回滚对象，三个写工具的输出 envelope 各挂一个：

```text
RollbackPlan
  strategy        // enum: in_place_reverse | manual_console | external_runbook | not_reversible
  description     // 人可读、含具体标识符（rule id / unit 名 / pool 名），不是通用话
  steps[]         // 有序步骤文本；每步含"在哪、做什么、怎么确认完成"
  expected_state  // 回滚完成后的可判定状态描述
  verification    // 怎么确认回滚生效（指向 verify 的 metric/窗口，若适用）
  artifacts[]     // 引用的证据：preview run、baseline 快照、rule id
```

逐工具的内容来源（这是 §4.2 中 R1/R2 的"内容来源归宿主执行器"）：

| 工具 | `strategy` | `description` 应含 | 生成方 |
|---|---|---|---|
| `host_restart_service` | `in_place_reverse` | unit 名、设备 id、"`systemctl start <unit>` 恢复；重启窗口内的 in-flight 连接不可恢复" | `RestartServiceTool`（宿主实现，非模型生成） |
| `recovery.execute`（restart_service） | `in_place_reverse` | 同上 + incident / proposal 引用 | `RecoveryExecuteTool` |
| `recovery.execute`（resize_pool） | `in_place_reverse` | 当前容量与目标容量、逆操作数值 | `PoolRecoveryExecutor` |
| `recovery.execute`（kill_process） | `not_reversible` | **明确说不可逆**，进程由 fixture 重建路径与耗时 | `HostProcessTerminator` |
| `apply_config_change` | `manual_console` | 被创建规则的 `resource_id`、在哪个页面禁用/删除、禁用与删除的差别 | `alertconfig`（替换两处硬编码文案） |

三条设计约束：

1. **`strategy` 是闭集 enum，且 `not_reversible` 必须存在。** 一个回滚对象最大的风险不是"不够详细"而是"假装可逆"。`kill_process` 没有逆操作，把它写成 `in_place_reverse` 加一句"重启即可"是比不写字段更糟的误导。§9.2 的 D4 张力在这里有一个具体后果：**如果评审把 `recovery.execute` 的 class 上调为 `destructive`，`not_reversible` 这个值就成为必须的诚实标注，而不是可选的细致。**
2. **内容由宿主实现生成，不由模型生成。** 这是 R1 的核心：SKILL 要求 repairer 先写回滚，但 SKILL 只能要求它**陈述**；工具输出提供**事实**。模型陈述 + 工具事实两边都有，操作员才有一个可以对照的锚。
3. **`apply_config_change` 的 `Rollback` 文案替换为含 `ResourceID` 的具体文本。** 这是最小改动：`alert_rule_manager.go:169` 与 `:310` 两处，从常量改为 `fmt.Sprintf` 引用刚创建的 `resource_id`。它不解决"没有逆操作工具"（那是 D5），但把"回滚说明不含标识符"这一半先修掉。

### 7.2 不新增回滚工具（D5，v1）

明确写下**为什么不做**，比做它更需要记录：

- 每个逆操作工具都是 4.10 的一个治理对象：manifest、版本、scope、签名、回滚文档、灰度闸门。`rollback_config_change`（disable/delete alert rule）需要 `alert.write` scope（repair 包已有，无新增 scope）但需要新的判定逻辑（"谁能删谁创建的规则"），这条判定不属于 4.9。
- `systemctl start` 逆操作会把 repair 包的动作面从"重启"扩到"启停"，即同一个 blast radius 下多了一类"停"——"停"与"重启"在 L2 的"reversible-by-a-human"论证（`pig-ops.yaml` 的注释）里不是同一件事：重启的逆是停，停的逆是启，但"停"本身可以作为攻击面。
- 因此 v1 的承诺收敛为：**回滚说明是结构化的、含标识符的、可被人按步骤执行的**。工具化（含 `alert.write` 下的 disable 工具、resize 逆操作）留给独立 change，且该 change 必须按 §9 的对照表重新论证 scope 与安全级别。

### 7.3 回滚终态接线（R3，宿主链）

这是 17 道里**修复成本最低、且纯粹是补接线**的一道：

1. `MCPVerifyOutput.RollbackRecommended == true` 时，由闭环 orchestrator（`core/manager/biz/loop/recovery.go` 一侧）调用既有的 `MutatingProposalRepo.MarkRolledBack`，把对应提案迁移到 `DecisionRolledBack` 并记录 `rollback_of`。数据层与状态机已就绪，缺的只是这一个调用点。
2. `hitl.Service.Complete` 的 `State == "rolled_back"` 分支同样需要一个生产者：谁在什么条件下宣告一次 HITL 提案被回滚。建议的规则是**与 1 同一触发**（verify 失败且判定建议回滚），避免两个入口各自发明条件。
3. **不做的事**：不给"回滚"本身自动执行（那需要 D5 的工具），只做**记录**。也就是说 R3 修复后，系统能回答"这次修复被回滚了吗、什么时候、为什么"，仍然不能自动回滚。这个诚实边界要写进闭环的 spec，防止读者以为 `rolled_back` 状态意味着有自动化。

### 7.4 预览的回滚演练语义（R4，如实记录）

v1 不扩。记录三件事作为将来扩展的判断材料：

1. 预览域的 baseline 分支在 `executeBranch` 的 `defer` 里被 `DROP ... CASCADE`，所以"回到 baseline"在预览里是**免费的且瞬时的**——这恰恰说明预览里的回滚语义不能外推到生产。
2. 若将来要做回滚演练，正确的形状是给 `Candidate` 加一个"回放回滚动作"的阶段（在候选分支上先 apply 候选、再 apply 逆动作、再跑同一负载），而不是保留 baseline 分支。后者会把预览域变成有状态的长驻对象，直接与 2.4 的任务生命周期工作冲突。
3. 在那之前，审批卡上的回滚说明应标注"未经预演"（这是 5.14 呈现侧的一个字段需求，4.9 这边只需要在 `RollbackPlan` 里提供 `rehearsed: false` 的来源字段）。

---

## 8. 强化设计 D：审批证据

### 8.1 预览绑定升为一等字段（A1 / A2 / P4 消费侧）

两处，一处是行，一处是负载：

1. **决策行**：`MutatingProposalEvent` / `chat_mutating_proposals` 增加 `preview_run_id`、`preview_candidate_id`、`preview_plan_digest` 三个可空列。空的语义是"这次调用不引用预览"（例如 `apply_config_change`、非 AgentTeams 的 `host_restart_service`），**不是"未知"**——区分这两者很重要，因为 A1 的反查需求是"哪些调用引用了预览"，可空列直接回答；若把"不引用"与"未知"混为一谈，反查就得回退到 `ArgsJSON` 匹配。
2. **提案负载**：`model.AgentTeamsPayload` 在 `Parameters` 之外增加一个 `Preview` 块（`run_id` / `candidate_id` / `plan_digest` / `workload_fingerprint`），**进入 `CanonicalPayloadHash` 的计算材料**。这是 A2 的修复点：批准时操作员签的 hash 从此同时覆盖"要执行什么"与"这个执行在预览里得到了什么结论"。

设计约束：`Preview` 块**由宿主在创建提案时从预览 store 填**，不由 caller 声明——`recovery.execute` 的 caller 只给 `preview_run_id` / `preview_candidate_id`，宿主反查并填充 digest。这防止一个模型把"预览结论很好"写进自己签名的负载里。

### 8.2 执行闸门三重对账（A3，预览域）

`Gate.Eligible` 的校验集从"一重"（workload 指纹）扩到"三重"：

| 校验 | 现状 | 扩展 | 防什么 |
|---|---|---|---|
| workload 指纹 | run vs 部署期常量 | 保留 | workload 文件升级后旧 run 仍被引用 |
| **target 指纹** | 无 | run 的 `TargetFingerprint` vs `ReadTargetIdentity` 的当前值 | 审批-执行窗口内目标被重建/漂移 |
| **plan digest** | 无 | 提案负载里的 `preview_plan_digest` vs 候选的 `plan_digest` | 审批后预览域的候选定义被改（YAML 变更生成新 run 时旧 digest 不匹配） |

第三重依赖 §8.1 的负载字段，所以这两节是一个切片（§10 的 S-3）。**注意**：`ReadTargetIdentity` 是对 preview-pg 的查询，把它加进执行路径意味着执行闸门多一次数据库往返——这是一笔真实的延迟成本，应该在切片里明说并测量，而不是当作免费。

### 8.3 共同证据 id（A4，宿主链）

一次修复闭环引入一个 `evidence_id`（UUID v5，命名空间 `opskeeper:repair-evidence`，材料 = `tenant_id + incident_id + proposal_id`，与预览域 `deterministicCandidateID` 的做法同族——确定性、可复算、不需要新存储）。它只做一件事：**让三段证据可以互相指认**。

```text
evidence_id 出现在：
  - chat_mutating_proposals 的行（决策）
  - chat_tool_calls 的执行行（经 ToolStartEvent 带入）
  - recovery.execute / verify_recovery 的输出 envelope（模型可见）
  - RollbackPlan.artifacts[]（§7.1）
  - 5.14 审批卡的字段来源（呈现侧消费，4.9 不定义卡片）
```

三条边界：

1. **不进 edge audit ledger 的 hash 链材料。** `AuditEntry.PrevHash/Hash` 是节点侧自己的完整性保证，Manager 侧往里塞关联键会改变链的含义。`evidence_id` 走 `Detail` 字段（`core/ports/audit.go:43` 明确允许 action-specific 结构化上下文），不参与哈希。
2. **不替代 `audit_log_id`。** MCP 表面已有的 `_meta.opskeeper_mcp_evidence.audit_log_id` 是单次调用的行 id，`evidence_id` 是跨段闭环 id，两者是不同粒度，都保留。
3. **不进 PiG 的 tool-call 持久化**（3.9 的归属）。`evidence_id` 是 Manager 侧两本账的关联键，PiG adapter 的对账有自己的模型，硬统一会把 3.x 的范围拖进 4.9。

### 8.4 class 驱动的 mutation 判定（A5，宿主链）

两处枚举换成 class 判定，但**分开论证**，因为它们的威胁面不同：

1. **`legacyKernelMutatingTools`（`agent.go:232`）**：这是一张**安全网**（legacy 内核没有装饰链），它的失效是真实漏洞。修复是把 `recovery.execute` 与 `apply_config_change` 加进这张表。**不改为 class 判定**，理由：legacy 内核是闭包式内核，BaseTool 在这条路径上不经过 `chain.Wrap`，因此**在这条路径上没有可靠的 class 来源**——硬改成 class 判定会引入"从哪读 class"的新问题。加名字是诚实的做法，代价是这张表必须随每个新写工具维护（注释已经这么要求了，只是没被执行）。**同时建议在表中加一条测试：遍历 `Registry.BuildBaseTools` 产出的所有 `Class != read` 的工具，断言其 wire 名都在这张表里**——把"必须记得加"变成"忘了会红"。
2. **`roleAllowsMutation`（`mcp_authorizer.go:149`）**：这条路径**有**可靠的 class 来源（`h.baseTools` 里的 BaseTool `Info().Class`），因此应当改为 class 判定：查 `Info().Class.AtLeast(ClassWrite)`。这同时覆盖 `apply_config_change` 与未来的任何写工具，把 A5 的潜伏面消掉。**代价**：`FilterToolNames` 会对每个候选工具做一次 `Info()` 调用，需要确认这个调用在 MCP `tools/list` 的路径上是廉价的（`decorators/audit.go:86` 的注释表明 `Info` 被设计为廉价且多为常量返回，风险低但应实测）。

---

## 9. scope / 安全级别对照（L2 写语义，无静默降级）

### 9.1 逐项对照表

| 本方案的对象 | 承载面 | safety_level | ToolClass | blast radius | scope 变化 | 是否需要上调宿主上限 |
|---|---|---|---|---|---|---|
| `preview.read`（新增工具） | 插件包 | L2（随包，不单独定级） | **read** | **不声明**（read 无半径语义） | **无新增** | **否** |
| `Candidate.parameters`（字段） | 预览域 | 不适用 | 不适用 | 不适用 | 不适用 | 否 |
| `RepairPlanEvidence`（投影） | 预览域 | 不适用（只读派生） | 不适用 | 不适用 | 不适用 | 否 |
| `RollbackPlan`（输出字段） | 插件包 + 宿主执行器 | 不变（L2） | 不变（write） | 不变（pod） | 无 | 否 |
| `verify_recovery` 绑定入参 | 插件包 | 不变 | 不变（read） | 不变 | 无 | 否 |
| `Preview` 块进提案负载 hash | 宿主链 | 不适用 | 不适用 | 不适用 | 不适用 | 否 |
| `Gate.Eligible` 三重对账 | 预览域 | 不适用（收紧） | 不适用 | 不适用 | 不适用 | 否（只会更严） |
| `evidence_id` | 宿主链 | 不适用 | 不适用 | 不适用 | 不适用 | 否 |
| A5 两处判定修正 | 宿主链 | 不变 | 不变 | 不适用 | 无 | 否（只会更严） |

**结论：整个方案没有任何一项需要上调 `profile.Ceiling`、`MaxRadius`、或新增 `domain.Scopes` 常量。** 全部变更要么是只读投影、要么是收紧校验、要么是不改变权限语义的字段增加。

### 9.2 必须显式记录的词汇表张力（D4）

这是审计发现的、**17 道之外**的一个问题，必须单列，因为它的两个解都有代价：

`core/domain/safety.go:27-31` 对 `ClassWrite` 的定义是 "**mutates OpsKeeper-managed state** (silencing an alert, editing a config draft)"，对 `ClassDestructive` 的定义是 "**mutates state outside OpsKeeper's control: a service restart, a kubectl apply, a SQL statement**"。

按这个定义，`host_restart_service`（重启 systemd 单元）与 `recovery.execute`（执行 SQL / 终止进程 / 调整 pool）**是 `destructive`**，而它们在 manifest 里声明为 `class: write`、在 BaseTool 里是 `Class: "write"`。`restart_service_basetool.go:28` 的注释给出的理由是"`destructive` 保留给不可逆操作（rm、drop table、reboot）；服务重启是可逆的"。

**这是同一词表的两份权威文档在打架**：`core/domain/safety.go` 说"重启 = destructive"，`restart_service_basetool.go` 说"重启 = write，destructive 留给不可逆"。而 `core/floor/skill` 的 `ClassMutating` → `ClassWrite` 映射（`cmd/opskeeper-edge/policy.go:163-170`）站在后者一边。

两个选项的代价：

- **(a) 维持 `write` 并落档**：代价是 `safety.go` 的 docstring 需要显式记录这个例外（"a service restart is reversible by a human, which is why the repair package's restart is `write` rather than `destructive`"），否则下一个读到 safety.go 的人会得出与本仓实现相反的结论。**不改任何行为。**
- **(b) 上调为 `destructive`**：代价是——`MinimumClass(L2)` 是 write，destructive 工具需要 L3；L3 在 financial profile 被 `BlockedNote` 显式拒绝（"an L3 package or one declaring a radius wider than pod is refused here"）。**也就是说 (b) 会让 `opskeeper-sre-repair` 在默认 profile 上无法安装**，等于从主部署形态里移除修复能力。这是一个产品级决策，不属于 4.9。

本方案倾向 **(a)**，并且强调：**本方案全文没有任何一处把 repair 的写语义降级表述**——`safety_level: L2`、`approval.required: true`、`max_blast_radius: pod`、`required_scopes: [host.write, alert.write]` 四项原样成立；§7.1 的 `not_reversible` 反而是**往更诚实的方向**收紧（把"kill_process 不可逆"这个今天没被任何字段承载的事实写出来）。若评审裁决 (b)，本方案的 §7.1/§7.2/§10 需要重写，且必须先解决"L3 在默认 profile 被拒"这一前置矛盾。

**顺带的显式化（§2.3 事实 1 的落档）**：`pig-ops.yaml` 的 `approval.required: true` 注释应补一句——包级 `approval.required` 是**包的承诺与宿主准入的声明位**，逐调用的审批判定由工具的 effective class 决定（`policygate` 的 `NeedsApproval`）；因此本包的 `draft_config_change` 与 `verify_recovery`（及新增的 `preview.read`）**不需要审批即可执行**，这是设计而非缺口。

### 9.3 `preview.read` 为什么不需要新 scope

`domain.Scopes` 是**凭据**的词表（`repair_profile_test.go:172` 的注释："A scope is a credential"）。`preview.read` 读的是 OpsKeeper 自己的预览 store，经 broker 上call 到控制面，全程使用宿主自己的数据库连接，**不接触任何外部系统的凭据**。为它引入 `preview.read` 这个 scope 会把"凭据授予"与"数据可见性"混为一谈：前者是 `required_scopes` 的语义（宿主注入凭据，包要不到没申请的），后者由 tenant 隔离与 API 层处理（`repair-preview-readback` spec 的"Tenant cross-access is denied"已覆盖）。**结论：不加 scope，靠既有租户隔离；若评审认为预览数据需要独立的作用域控制，那是一个新的治理需求，应该作为独立决策点提出，而不是借 4.9 顺手实现。**

---

## 10. 实现切片建议（不在本任务合入）

按依赖排序；每片给出落点、边界、回滚与验收。S-1 与 S-2 无相互依赖，可并行。

### S-1 预览域证据对象（P3 / P4 / P2-b，绿）

- **落点**：`core/domains/control/repairpreview/{model.go,workload.go,readback.go}` + `deploy/repair-preview/pg-pool-workload.yaml`（为两个既有候选补 `parameters`）+ 对应 `*_test.go`。
- **内容**：`CandidateSpec.parameters` 字段；`Candidate.plan_digest`；`RepairPlanEvidence` 投影与构造函数；`Evaluate` 判定不变。
- **不触碰**：`runner.go` 的执行语义、`repository.go` 的存储、`gate.go`（S-3 才动）。
- **回滚**：纯字段增加 + YAML 补参数；旧 run 无 `parameters`/`plan_digest` 时投影按空值处理并标注 `incomplete`，不拒读。回滚即 revert 单个 commit，无数据迁移。
- **验收**：`go test ./core/domains/control/repairpreview/...`；新增一条"文案与参数不一致时 digest 仍稳定"的测试（P3 的回归测试）。

### S-2 插件包工具面（P1 / V1 / D3，绿）

- **落点**：`plugins/pig-ops/opskeeper-sre-repair/extensions/opskeeper-sre-repair/tools.go`（+`preview.read`，改 `verify_recovery` schema）、`pig-ops.yaml`（工具清单 5→6 + 注释）、`skills/opskeeper-{repairer,verifier}/SKILL.md`、`core/floor/pluginmanifest/repair_profile_test.go`（`mutatingToolsInRepairProfile` 不变，新增 read 工具清单断言）。
- **内容**：`preview.read` 工具声明与宿主投影对接；`verify_recovery` 绑定入参/回显出参；D1(b) 的两处显式声明。
- **不触碰**：宿主执行器逻辑（`preview.read` 复用 `/v1/incidents/{id}/repair-preview-summary` 的数据源）。
- **回滚**：manifest 工具清单回退即可让节点不再暴露该工具（host allow-list 按 manifest 建，未声明即拒）；schema 变更是向后兼容的可选字段。
- **验收**：`make pi-plugin-guard`；`go test ./core/floor/pluginmanifest/...`（manifest 与工具集 drift 测试）；`cmd/opskeeper-edge/repairpath_test.go` 扩一条"read 工具不需要 receipt 即可过 broker"。

### S-3 审批链一等绑定（A1 / A2 / A3 / P4 消费侧，绿）

- **落点**：`core/manager/biz/aiops/tools/decorators/review_gate.go`（事件字段）、`core/manager/model/aiops/mutating_proposal.go` + 迁移、`core/manager/model/hitl/model.go`（`Preview` 块）、`core/manager/biz/hitl/agentteams.go`（canonical hash 材料）、`core/domains/control/repairpreview/gate.go`（三重对账）。
- **依赖**：S-1（`plan_digest` 存在）。
- **回滚**：新列可空、`Preview` 块可选，旧提案/旧 run 继续可读；三重对账的 target/digest 两重在无对应字段时**跳过而非失败**（否则一次发布就把所有存量 incident 的修复路径堵死）。
- **验收**：新增"提案负载 hash 变化当且仅当 Preview 块变化"；"target 指纹漂移时 Eligible 返回 ErrStaleWorkload 同族的显式错误"；**延迟实测**（`ReadTargetIdentity` 进执行路径的耗时）。

### S-4 回滚对象与终态接线（R1 / R2 / R3，绿）

- **落点**：`core/manager/biz/aiops/tools/{restart_service_basetool.go,configchange/config_tools.go}`、`core/manager/biz/aiops/tools/recovery/recovery_execute_basetool.go`、`core/manager/biz/aiops/alertconfig/alert_rule_manager.go`（两处文案）、`core/manager/biz/loop/recovery.go`（`MarkRolledBack` 调用点）。
- **回滚**：`RollbackPlan` 是输出 envelope 的可选字段，旧消费方忽略即可；`MarkRolledBack` 接线是新增调用点，revert 即回。
- **验收**：每个 command 一条"输出含正确 `strategy`"的测试（`kill_process` 必须是 `not_reversible`）；一条"verify 失败 → 提案迁移到 `rolled_back`"的闭环测试。

### S-5 class 驱动判定（A5，琥珀——影响面需单独评审）

- **落点**：`core/manager/biz/aiops/agent/agent.go`（deny-list 补两名 + 新增遍历断言测试）、`core/manager/biz/loop/mcp_authorizer.go`（`roleAllowsMutation` 改 class 判定）。
- **为何琥珀**：这一片改变的是**判定规则**而非字段，失效模式是"本该被拒的调用被放行"，属于安全敏感变更，需要单独的威胁面评审，不宜与文档/字段类变更混在一个 PR。
- **回滚**：两处均可独立 revert；class 判定回退到名字枚举不丢数据。
- **验收**：`apply_config_change` 加入某角色工具集后，非 repairer 角色被拒的测试；legacy deny-list 遍历断言测试。

### S-6 双名对齐测试与 D4 落档（V4 / D4，琥珀——纯测试与文档）

- **落点**：新增对齐测试（`verify_recovery` ↔ `recovery.verify`、`host_restart_service` ↔ `host.restart_service` 的 schema/字段对账）；`core/domain/safety.go` docstring 记录 D4 例外；`plugins/pig-ops/opskeeper-sre-repair/pig-ops.yaml` 注释补 §9.2 / §6.3 的两条边界说明。
- **回滚**：测试与注释，无行为。
- **验收**：对齐测试本身；`python3 scripts/audit_open_source.py` 通过（确认无内部任务编号、无私有路径）。

---

## 11. 通用闸门（沿 4.3–4.7 的清单）

| 闸门 | 要求 |
|---|---|
| **manifest 闸门** | `sdk.Decode → sdk.Validate → sdk.Admit` 全过；`preview.read` 的 `class: read` 与宿主 `Info().Class` 一致（drift 测试会抓） |
| **scope** | `required_scopes` 保持 `[host.write, alert.write]` 不变；本方案论证过 `preview.read` 不需要新 scope（§9.3） |
| **安全级别** | `safety_level: L2`、`approval.required: true`、`max_blast_radius: pod` 三项不变；D4 裁决前**不改 class**，裁决后按 §9.2 重写对照 |
| **签名** | 经 `core/floor/pluginmanifest/signing.go` 通道，签名随 manifest 分发 |
| **测试** | unit + integration + contract + failure 四层；S-3/S-5 必须含"拒绝路径"测试（target 漂移、digest 不匹配、非 repairer 角色调用写工具） |
| **回滚文档** | 每片含"回退前 / 回退后 / 不变量"三段（§10 已给） |
| **闸门命令** | `make pi-plugin-guard && make module-check && go test ./scripts/ledgercheck` 全过 |
| **开源审计** | `python3 scripts/audit_open_source.py` 通过；**受审计文件不含内部任务编号**（正则 `\b(?:LUM|BENY)-\d+`，见 `scripts/audit_open_source.py:61`），任务编号只出现在 PR 描述与评审评论 |

---

## 12. 与 4.1 / 4.2 一致性自查表

| 自查项 | 4.1 / 4.2 结论 | 本方案是否一致 | 证据 |
|---|---|---|---|
| 修复预览归"保留 core"、未接共享任务生命周期 | §4 ⑥、§6 缺口表 → `runtime-management-gap` 2.4 | ✓ 不动 run 的调度/租约/checkpoint；`RepairPlanEvidence` 是 run 内字段（§5.1 约束 3） | §1.3 / §5.1 |
| 修复执行已迁为插件（L2，5 工具） | §4 ⑥ | ✓ 不改包的身份；工具 5→6 是 4.9 的正当扩面，且新工具是 read | §5.2 / §9.1 |
| 独立验证现状"部分" | §4 ⑥ | ✓ 本方案把"部分"具体化为 V1–V4，并显式记录"persona 分离只在 AgentTeams 层成立"这一边界 | §3.2 / §6.3 |
| 审批/HITL 双签归宿主 | §4 ⑦、§九 | ✓ 不动 `biz/hitl` 的状态机语义与 `dualsigngate.go`；只加字段与调用点 | §8.1 / §8.3 |
| 六扩展点封闭 | `core/floor/kernel/kind.go` | ✓ 不新增 Kind / 信号 / family；全部变更落在既有承载面 | §4.2 末段 |
| 不与 5.14 重叠 | tasks 5.14 | ✓ 供给侧 / 呈现侧逐条切分，且写明"越界的一定是 4.9 这一侧"的退回规则 | §4.3 |
| 不与 5.12 重叠 | tasks 5.12 | ✓ 不动双签数量 | §1.3 |
| 不做 L3 自治 | 4.1 §4 ⑥"非目标" | ✓ 不上调任何 profile 的 `Ceiling` / `MaxRadius`；D4(b) 被明确指出会与 L3 拒绝规则冲突而**不倾向** | §9.1 / §9.2 |
| 不改内核 | 计划决策 3 | ✓ 零内核改动 | §1.4 非目标 3 |
| 4.10 的治理台账 | tasks 4.10 | ✓ 新工具的 manifest 变更在 §9 单列，正式台账归 4.10 | §1.3 / §9.1 |

---

## 13. 备选方案与放弃原因

| 备选 | 内容 | 是否采用 | 原因 |
|---|---|---|---|
| **方案 A：只写现状审计，不给设计** | 17 道缺口 + 代码证据，设计留给落地 change | 否 | tasks 4.9 的原文是"强化 repair 插件的计划预览、独立验证、回滚说明和审批证据"，强化方案是交付物本身；只交审计会让"归属判定"与"不重叠论证"这两个验收项落空，而这两项正是跨任务协作里最容易出事的部分 |
| **方案 B：把 17 道全部修掉** | 本任务同时出方案与实现 | 否 | ① 本 change 的定位是"插件能力恢复"的方案阶段，tasks 4.3–4.7 全部是"方案产出为主，实施另批"；② S-5 是安全敏感的判定规则变更，与文档类变更混在一个 PR 会让评审面失焦；③ 我的角色是架构守护，不做实现 |
| **方案 C：D1 走 (a)，为 restart/kill 提供预览** | 扩预览域词表 | 否 | 预览域的隔离边界是"disposable preview-pg + 固定负载回放"（`Run.IsolationBoundary`（`model.go:35`）、spec `repair-preview-readback` 的措辞），为 systemd 单元提供预演需要另一种隔离形态，成本与 4.9 不成比例且会改写 spec 的承诺 |
| **方案 D：D5 走 (b)，新增回滚工具** | disable alert rule / `systemctl start` / resize 逆操作 | 否 | 每个逆操作都是 4.10 的治理对象；且"停"与"重启"在 L2 的可逆性论证里不是同一件事（§7.2）。本方案把回滚承诺收敛为"可读、可执行于人"，这是诚实的而不是偷懒的 |
| **方案 E：用 `evidence_id` 统一 Manager 与 edge 两本账的存储** | 建一张跨账索引表 | 否 | A4 需要的是**关联键**不是**合并存储**；`AuditEntry` 的 hash 链语义不应被 Manager 侧的关联需求改变（§8.3 边界 1）。合并存储是数据架构决策，不是 4.9 的 |
| **不做（整体）** | 4.9 写一句"现状已可接受" | 否 | 回滚说明一组是 3 红 1 琥珀，其中 R3 是"模型与数据层都在、零调用者"的纯粹断线；把它写成"可接受"等于把一条建好的审计通路当装饰品 |

---

## 14. 验收清单（对应任务描述）

- [x] 四个关键词各有现状审计（代码证据）与缺口定性 —— §3（P/V/R/A 四组共 17 道，每道含文件级证据与红/琥珀定性）
- [x] 每个缺口有明确设计归属（插件包/预览域/宿主链）与不重叠论证 —— §4.2 归属总表逐行给理由；§4.3 给出与 5.14 的供给侧/呈现侧切分与退回规则
- [x] scope/安全级别对照显式（L2 写语义，无静默降级） —— §9.1 逐项对照表（结论：零项需要上调宿主上限）；§9.2 单列 D4 词汇表张力并说明为何不静默维持；§9.3 论证不加 scope
- [x] 实现切片建议（S-1…S-6），落地需另开 change —— §10，含落点/边界/回滚/验收
- [x] 4.1/4.2 一致性自查表 —— §12（11 项逐项对照）

## 15. 提交与执行规范

- 提交语言：本任务使用中文注释 / 中文提交说明（沿 4.3–4.7 的先例；仓库根 `CLAUDE.md` 的英文默认在历次方案类提交中按此例外处理）。
- 关联：OpenSpec change `plugin-architecture-recovery-plan` 任务 4.9。**内部任务编号不写入任何受开源审计的跟踪文件**（`scripts/audit_open_source.py:61` 的正则会拒收），只记录在 PR 描述与评审评论中。
- 落盘路径：本文档落盘 `openspec/changes/plugin-architecture-recovery-plan/repair-hardening-plan.md`。
- 实现切片（§10）**不**在本任务内合入；落地请用户单独开 change，并先裁决 **D1–D5**（§4.4）。
- PR 描述需含贡献指南勾选项（`CONTRIBUTING.md` 的 Code style / Commit messages / Branching & PRs 三节）与本方案的 D1–D5 决策点清单，使评审可以在 PR 内直接裁决。
