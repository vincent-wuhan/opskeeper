# 4.12 告警规则 / 草稿 / 降噪插件化方案（extension tool + command）

> 状态：**已评审（2026-10-31）**。任务来源：OpenSpec change `plugin-architecture-recovery-plan` 任务 4.12，承接 4.1 §7 结论⑤（告警配置写路径未进插件词表）与 4.8（plugin-gap-plan）的三面盘点。基线：`main`（PR #111 之后，`21efd90a`）。
>
> **评审裁决（2026-10-31，用户确认）**：D-3 维持现状分工（radius 表物理触达、scope_type+draft_hash 表语义触达，不新增 config 枚举）；D-4 删除 SemanticDedupService（独立小 PR，同步下调死码棘轮）；S-3 会话面词表扩展排 R1 之后第二切片；S-4 command pack 随 alert 包同仓库发布。
>
> **这是纯方案产出**：不实现任何包、不改 `core/floor/pluginmanifest` 与 `sdk` 代码、不动 `core/manager/biz/alert` 主流程、不做告警事件的 incident 化（5.x）。所有实现内容以切片（§10）形式留给后续 PR，每个切片自带测试与回滚。
>
> 产出物：本文（现状审计 + 归属判定 + 方案 + 安全级别论证 + 测试/回滚）、manifest 草案（`alert-manifests/alert-rule-config.yaml`）、command 草案（`alert-manifests/command-alert-rule.md`）、台账新行（`plugin-governance-ledger.md` §3.2）。

---

## 0. 结论摘要

1. **三块的成熟度不一样，方案对三块给出的答案也不一样。** 规则块与草稿块已经成器：9 种规则类型、`draft → confirm → apply` 的哈希绑定流、双内核的散文拦截（draftguard）都在 manager 编译内面；降噪块只有一件是真接线（per-rule 发送策略），另外三件（inhibit 硬编码规则、LLM 语义去重、关联器）分别是"注释里承诺的未来表"、"零生产调用者的完整实现"、"只有 demo 接线的接缝"。
2. **词表归属一句话**：工具名与工具 JSON schema 归插件包；规则 DSL（kind/scope/spec/join_mode/severity）与降噪语义归宿主。跨界词表只有一个——发送策略的两个 rule 字段（`NotifyWindowSeconds` / `NotifyMinFires`）——它们必须出现在插件包的 schema 里，因为草稿要校验它们，但语义归宿主（§4）。
3. **方案**：新建包 **`opskeeper-sre-alert`**（edge / L2 / read+write / alert.read+alert.write / pin / 2 工具），把 `draft_config_change` 与 `apply_config_change` 从 `opskeeper-sre-repair` **硬切**过来。理由不是"搬家更好看"，而是 repair 的章程（"对单个具名目标的、人可回退的有界操作"）与"决定什么时候叫醒人"的检测策略是两种不同的承诺，捆绑时弱承诺治理强承诺（§5）。四个候选（新包 / repair 增量 / readonly 扩展 / 不做）都写明取舍与放弃原因。
4. **写路径安全级别显式论证**：L2（写的是 OpsKeeper 自管的 `alert_rules` 行，不碰 OpsKeeper 控制之外的任何状态，L3 是夸大）、write 而非 destructive（每个动作在同词表内有逆）、approval 两重闸（broker 收据 + 工具自身哈希绑定）、radius=pod（词表里最窄值；词表无法表达"一条全局规则改变整个舰队被叫醒的行为"这一真实触达，作为决策点 D-3 如实记录而非放大数字）。**回滚是一等字段**：吸收 4.9 R2（`alert_rule_manager.go:169`/`:310` 的回滚散文），`RollbackPlan` 结构化、delete 草稿自带规则当前定义即回滚（§7.4）。
5. **降噪在 v1 不插件化为包**（§9）：它的语义长在评估管线里，没有独立的工具面；把它做成包等于把"抑制规则"做成一个可分发工件，而它今天连管理者自定义的表都没有。未来的插拔点（`Inhibitor` 接口、`inhibition_rules` 表、`FiringCorrelator` 接缝）已经以接口/接缝形态存在，留给出现第三个实现诉求时再决定承载面。
6. **command 半边有一个诚实的坏消息**（§8）：通道存在（`core/extension/biz/container/command_parser.go:27` 把 `commands/<name>.md` 解析成 `cmd_<name>` 关键词技能），但 pig-ops 树不在 manager 的技能根里（`cmd/opskeeper/main.go:3092-3094`），所以"在插件包里放一个 commands/ 目录"是惰性的。送达路径有两个候选（marketplace pack / 宿主接线），方案取 marketplace pack。
7. **manifest 闸门状态如实声明**：本任务运行环境无 Go 工具链，sdk 三段闸门（`Decode`/`Validate`/`Admit`）**未实测**，§11.1 给出的是对 `sdk/manifest.go` 规则的逐条静态推演；把推演固化成仓内测试（并入 4.11 的闸门 harness）是 S-1 切片的第一条验收（沿用 4.5 §11.1 的先例：任务期间一次性跑过的验证，落成仓内测试）。
8. **两条原始审计发现**：①（红，G0 级）`draft_config_change`/`apply_config_change` 声明在 repair 的 manifest、能过 edge broker 的允许清单，但 manager 上呼闭包注册表里没有分发——4.8 对这两项的"已恢复"过报（§3 R1）；②（红）会话面比管理面窄：管理面有 update/enable/disable/delete，会话工具只认 create（`config_tools.go:310`），于是"禁用一条规则"这个最高频的降噪动作在会话面不可表达（§3 R3）。

---

## 1. 目标与边界

### 1.1 目标

- 给告警**规则**、**草稿**、**降噪**三块各做一份有代码证据的现状审计（§2–§3），而不是把三块笼统说成"告警域"。
- 按 4.9 §1.2 的归属判定规则，对三块涉及的全部词表**逐项**判定归属（插件包 / 宿主 / 已有包不动），并写出跨面契约：谁生产、谁消费、以什么版本约束（§4）。
- 给出 extension tool + command 的**方案**（含 manifest 草案与 command 草案）、写工具的安全级别显式论证（L2/write/approval/回滚一等字段，无静默降级）、测试与回滚计划（§5–§11）。
- 对降噪给出"是否插件化、哪些点可插拔、为什么 v1 不做成包"的判定而不是含糊的"待定"（§9）。

### 1.2 承载面与归属判定规则

沿用 4.8 的三面模型与 4.9 §1.2 的判定规则，本域涉及的面：

| 面 | 定义 | 本域现状 |
|---|---|---|
| 节点插件面 | pig-ops manifest 声明的工具/能力/scopes | `opskeeper-sre-repair` 声明了两个告警配置工具（`plugins/pig-ops/opskeeper-sre-repair/pig-ops.yaml:53-54`） |
| manager 编译内面 | `aiops/tools` 注册表 + 图内核 `BuildBaseTools` | 两个 BaseTool 在 `configchange/config_tools.go`（556 行），`registry.go:230` 只存引用 |
| 节点上呼面 | broker → tunnel `agent.tool` → `agentToolUpcall` → 闭包注册表 | `dispatchAgentTool`（`main.go:7043`）只查闭包注册表；两工具**不在其中**（§3 R1） |

归属判定规则（4.9 §1.2 原文引述）：**工具名与调用 schema 是插件包的词表**（包是它的生产者与唯一修改者，改名/改 schema 是包的版本内增量）；**规则 DSL 与领域语义是宿主的词表**（插件包只是消费方，宿主扩展 DSL 不需要任何包发版）；**已落在别的包里的名字不动**（4.8 已裁定 `query_alert_rules` 归 readonly、`list_metric_catalog` 归 observability，本方案不重复裁定）。

### 1.3 与相邻任务的边界

| 相邻任务 | 边界 |
|---|---|
| **5.x（IM 告警路由 / incident 化）** | 本方案管"规则怎么被创建、草稿怎么被确认、通知前怎么被降噪"，不管"一条已确认的告警事件去哪个 IM 群、被升级成什么 incident"。`notify_channel_ids` 只作为规则配置字段穿过草稿，路由语义不进本包词表。§9.4 的 `FiringCorrelator` 接缝是 5.x 的天然入口，本方案只登记不实现。 |
| **4.5（notifier 出站）** | 4.5 管"出站通道插件化"（渠道工厂、L3/destructive、pin）；本方案管"入站侧的检测策略"。两者共享 `alert.write` scope 但互不引用：本包没有任何工具能触达渠道，4.5 的包没有任何工具能触达规则。台账里两行各自独立。 |
| **4.8（plugin-gap）** | 4.8 已裁定只读 `query_alert_rules` 维持现状、不扩展写路径；本方案是那条裁定的写侧对应物。4.8 对"两个配置工具已恢复"的结论被本方案 §3 R1 修正为"声明已恢复、上呼未接线"。 |
| **4.9（repair 强化）** | 4.9 D5 明确"反向工具（undo）不在 repair 内做、留独立 change"——本方案承接该结论：回滚以 `RollbackPlan` 一等字段与 delete 草稿自带定义的形态进 alert 包，不做通用 undo 工具。 |
| **4.10（台账）/ 4.11（闸门）** | 本方案新增一行台账（§3.2），其测试列即 4.11 闸门的输入；闸门本身的执行状态如实写"未实测"（§11.1）。 |

### 1.4 非目标

- 不实现任何包、不写生产代码、不改内核；
- 不动 `core/manager/biz/alert` 评估/降噪主流程（发送策略校验、inhibit、correlate 的行为一字不改）；
- 不做告警事件的 incident 化、不做 IM 路由（5.x）；
- 不做通用回滚/undo 工具（4.9 D5 已裁定不做，本方案以字段承接）；
- 不裁决 `SemanticDedupService` 的去留实现（那是 D-4 决策点 + 独立小 PR 的事，本方案只给出选项与推荐）。

---

## 2. 现状证据：三块链路全貌

### 2.1 链路图

```
【规则 + 草稿：三个面】
pig-ops manifest（repair:53-54）
  draft_config_change(read) / apply_config_change(write)
        │  edge broker：允许清单 = 已准入包的 manifest 投影
        │  （cmd/opskeeper-edge/policy.go:56 manifestsOf；:79-86 写类须带收据）
        ▼
节点 agent 调用 ──tunnel agent.tool──▶ manager agentToolUpcall.RunAgentTool（main.go:7008）
        │                                 └─ dispatchAgentTool（main.go:7043）只查闭包注册表
        │                                    ⚠ 两工具不在闭包注册表 → errs.ErrNotFound（§3 R1）
        ▼
manager 编译内面（真正的实现）
  configchange/config_tools.go   两个 BaseTool + JSON schema + validateApplyGate(:284)
  alertconfig/alert_rule_manager.go   PreviewRule/CreateRule + 30min 草稿 TTL
  alertconfig/draft_store_memory.go   内存草稿仓 + beginApply 租约（哈希/过期/同人/动作一致/在途互斥）
  alertdraft/draftguard.go       双内核散文拦截（DraftConfigChangeToolName :23，BlockedMessage :30）
  agentkernel/deferredgate.go    apply 的延迟闸（:26 注释明确它是 draft/confirm 对的变更半边）
        ▼
规则事实（宿主词表）
  model/alert/model.go:95-99,104-107   9 种 RuleKind
  server/alert/http.go:194-195          update/enable/delete 管理面路由（requireAdmin）
  base/pkg/audit/port.go:144-146        ActionRuleCreate/Update/Delete 审计动作

【降噪：评估管线内，无独立工具面】
PipelineEvaluator.evaluate（pipeline.go:174）
  ├─ 发送策略 dampening   usecase.go:1312-1321（窗口内 fires < NotifyMinFires 则不发）
  ├─ 抑制 inhibit         inhibit.go BuiltinInhibitor——两条硬编码规则（:22-37 注释承诺未来 inhibition_rules 表）
  ├─ 关联 correlate       correlate.go:30 FiringCorrelator 接缝；唯一接线是 demo（main.go:1895）
  └─ 语义去重 semantic    semantic_dedup.go:242 SemanticDedupService（LLM + 熔断）——零生产调用者
```

### 2.2 规则块逐项

| 项 | 证据 | 归属面 |
|---|---|---|
| 9 种规则类型（metric_threshold/anomaly/forecast/burn_rate/raw + log_match/log_volume + trace_latency/trace_error_rate） | `core/manager/model/alert/model.go:95-99,104-107` | 宿主（DSL） |
| 规则读写管理面：create/update/enable/delete，全部 `requireAdmin` | `core/manager/server/alert/http.go:194-195`（`setRuleEnabled`:861、`deleteRule`:914） | 宿主 |
| 规则审计事件 | `http.go:821/852/882/929` → `core/base/pkg/audit/port.go:144-146`（rule_create/rule_update/rule_delete + ResourceRule） | 宿主 |
| 会话侧规则读回 | `query_alert_rules`（readonly 包，4.8 已裁定不动） | 插件包（readonly） |
| 指标目录（草稿前置证据） | `list_metric_catalog`（observability 包），且其指令字段显式把用户引向 `draft_config_change`（`metriccatalog/metric_catalog_tool.go:30,211,213`） | 插件包（observability） |

### 2.3 草稿块逐项

| 项 | 证据 | 说明 |
|---|---|---|
| 工具对 | `configchange/config_tools.go`（556 行） | `draft_config_change`（校验 + 预览，不落库，返回 draft_hash）与 `apply_config_change`（提交） |
| 应用闸 | `validateApplyGate`（:284） | `confirmed=true` 且 admin/superuser，二者缺一即拒 |
| 动作词表 | `normalizeConfigCreateAction`（:304） | **只认 create**：`:310` "action must be create; v1 only supports creating alert rules"；`:300` 同句限定 domain=alert_rule |
| 哈希绑定 | `AlertRuleConfigDraftHashForID`（:386） | SHA256，规范 JSON 序列化后取哈希，前缀 `sha256:`；apply 侧 `validateDraftHash`（:339）重算比对 |
| 脱敏 | `scrubConfigSecrets`（:437） | 草稿哈希计算前对密钥类字段脱敏，避免"哈希泄密" |
| 草稿仓 | `alertconfig/draft_store_memory.go` | 内存实现：`records`/`applying` 两张表，30 分钟 TTL（`alertRuleDraftTTL`），`beginApply` 租约校验草稿存在、哈希一致、未过期、**同一 UserID**、动作+哈希匹配、在途互斥 |
| 散文拦截 | `alertdraft/draftguard.go` | 关键词嗅探 + 工具结果形态判定（`ToolResultLooksLikeConfigDraft` :153），两处内核都挂了；`:30` 的 BlockedMessage 直接向用户解释"已拦截文字草案" |
| 延迟闸 | `agentkernel/deferredgate.go:26` | 把 apply 推迟到人确认之后，注释明确它是"draft/confirm 对的变更半边" |
| 编译内注册 | `registry.go:230` `SetConfigManager` | 注释原文："The legacy closure-style registry does not expose these mutating flows."；唯一调用方 `cmd/opskeeper/toolwiring.go:189`（图内核 BaseTool 面） |
| 回滚现状 | `alertconfig/alert_rule_manager.go:169`、`:310` | `Rollback: "可在 Alerts 规则列表中禁用或继续编辑该规则。"`——**散文**，4.9 R2 已点名，本方案 §7.4 承接为结构化字段 |

### 2.4 降噪块逐项

| 项 | 证据 | 接线状态 |
|---|---|---|
| 发送策略（窗口 + 阈值抑制） | 字段 `usecase.go:667-673`；校验 `:820-831`（成对出现否则拒、窗口 60s–24h、阈值 1–100）；执行 `:1312-1321`（窗口内 fires 计数不足则跳过通知） | **真接线**，且校验测试在 `dampening_test.go`（拒绝只填一半的混合策略） |
| 抑制（inhibit） | `inhibit.go:22-37` `BuiltinInhibitor`：edge_offline 抑制同机 host:* ；prom_ingest_fail 抑制 scrape_down:* | 接线，但**两条规则硬编码**；:25-27 注释承诺"admin-defined inhibition lands with a future inhibition_rules table" |
| 关联（correlate） | `correlate.go:30` `FiringCorrelator` 接口 + `:43` nil 安全调用点 | 接缝存在；唯一实现是 demo（`core/manager/biz/demo/correlate.go:21`，`main.go:1895` 接线） |
| 语义去重（LLM 聚合） | `semantic_dedup.go:242` `SemanticDedupService`（中文 prompt :41、`LLMClient` :145、熔断器 :159、`SemanticDedupConfig` :268） | **零生产调用者**：仓内除自身与其测试外无任何引用（§3 A1） |

### 2.5 必须先说清的三个事实

1. **声明与实现之间有一段真空**（§3 R1）：manifest 声明 → broker 放行 → 上呼分发表没有。任何"把工具搬进新包"的方案若不补这段接线，只是把真空换个地址。
2. **降噪没有工具面**：四件东西里没有一件是"被模型调用的工具"。发送策略是规则字段，inhibit/correlate 是管线内接口，语义去重是没人调的服务。所以"降噪插件化"若指做成 pig-ops 包，v1 没有承载物（§9）。
3. **command 通道对 pig-ops 树惰性**（§3 A3）：`ConvertCommandFile`（`command_parser.go:27`）确实把 `commands/<name>.md` 变成 `cmd_<name>` 关键词技能（`:41-43`），但 manager 的技能根是 `./skills`、`./agents` 与 marketplace 根 `/var/lib/opskeeper/skills`（`main.go:3092-3094`），pig-ops 树不在其中——插件包里放 commands/ 目录今天不会被发现。

---

## 3. 现状审计缺口（红 / 琥珀 + 代码证据）

| 编号 | 级别 | 缺口 | 证据 | 归属切片 |
|---|---|---|---|---|
| **R1** | 红（G0 级） | **上呼分发缺口**：两个配置工具声明在 repair manifest（`pig-ops.yaml:53-54`）、能过 edge broker 允许清单（`policy.go:56` 清单即已准入包的 manifest 投影），但 manager 的 `dispatchAgentTool` 只查闭包注册表（`main.go:7043`），`Registry.Invoke` 对未知名返回 `errs.ErrNotFound`。**净效果：节点上模型"看得见、调不通"。** 4.8 对这两项的"已恢复"过报。 | `registry.go:230-231`、`main.go:7043`、`pig-ops.yaml:53-54` | S-1（新包 + 分发表补两行） |
| **R2** | 红（承 4.9 R2） | **回滚是散文**：草稿与应用两处都返回同一句中文提示，模型可以复述、无法执行、无法进变更单。 | `alert_rule_manager.go:169`、`:310` | S-2（`RollbackPlan` 结构化） |
| **R3** | 红 | **会话面比管理面窄**：管理面有 update/enable/disable/delete（`http.go:194-195`，审计 :852/:882/:929），会话工具只认 create（`config_tools.go:300`、`:310`）。后果有二：最高频的降噪动作"禁用一条规则"在会话面不可表达，操作者被推向控制台或散文；且"delete 草稿自带定义即回滚"这条回滚一等路径没有会话载体。 | `config_tools.go:310` vs `http.go:194-195` | S-3（宿主草稿域词表扩展，非包自发扩） |
| **A1** | 琥珀 | **`SemanticDedupService` 零生产调用者**：模型、prompt、熔断、配置注入齐全，但仓内无任何生产调用。与 4.9 R3 同型（"数据层与模型都在、零调用者"）。留着是"看起来有 LLM 降噪"的错觉，删了要重写。 | `semantic_dedup.go:242`；全仓 grep 仅自身 + 测试 | D-4 决策点（§9.3） |
| **A2** | 琥珀 | **抑制规则硬编码两条**，管理者自定义抑制要等 `inhibition_rules` 表（注释已承诺）。这不是缺陷而是诚实的未建，但要在方案里写清它是不是插拔点（§9.4 判定：是，且承载面是宿主表不是包）。 | `inhibit.go:22-37` | 不切片（登记） |
| **A3** | 琥珀 | **command 通道对 pig-ops 惰性**：`commands/` 目录只有 marketplace 安装的 pack 会被走查（`plugin_container.go:113-115`、`main.go:3092-3094`）。"extension tool + command"的 command 半边需要一个送达决定。 | `command_parser.go:27`、`main.go:3092` | S-4（marketplace pack） |
| **A4** | 琥珀 | **可准入 ≠ 已组合**：`opskeeper-sre-alert`（L2/pod）在 finance（L2/pod）与 SaaS（L3/ns）两 profile 下都能过 `Admit`，但两 profile 的 `Composes` 都没有它——不进 Composes 的包不会被装，闸门绿了也只是"能装"不是"会装"。 | `profiles.go:136-140`、`:188-193` | S-1（两 profile 各加一行 + 意图改写） |
| **A5** | 琥珀（决策点化） | **radius 词表是基础设施形状的**：`BlastRadius` 枚举（none/pod/single-ns/namespace/cluster）能说"一次 apply 写一行"，不能说"一条全局规则改变整个舰队被叫醒的行为"。放大数字是撒谎，如实记录是唯一诚实选项。 | `sdk/manifest.go`（Admit 对 write 缺省推导 namespace） | D-3 决策点（§7.3） |

**审计小结**：红三项全部是"声明面与执行面不对齐"这一类病——R1 是包声明 vs 上呼分发，R2 是回滚字段 vs 散文，R3 是管理面词表 vs 会话面词表。琥珀四项里 A1/A3 需要裁决，A2/A4/A5 是登记与切片事项。**没有发现降噪主流程本身的行为缺陷**——发送策略校验连"只填一半"都拒（`usecase.go:823`），这部分不需要本方案救。

---

## 4. 词表归属判定（逐项 + 跨面契约）

判定规则见 §1.2。下表逐项给出归属、生产者、消费者与版本约束。

| # | 词表条目 | 归属 | 生产者 | 消费者 | 跨面契约（一句话） |
|---|---|---|---|---|---|
| 1 | 工具名 `draft_config_change` / `apply_config_change` | **插件包**（opskeeper-sre-alert） | 包 manifest（`alert-manifests/alert-rule-config.yaml` tools 节） | edge broker 允许清单、manager 分发表、模型 | 包是名字的唯一修改者；改名是包的版本内增量，且受"全包工具名唯一"测试约束（§6.2） |
| 2 | 工具 JSON schema（`action`/`domain`/`rule{...}`/`confirmed`/`draft_hash`/`payload`） | **插件包**（从 `config_tools.go` 的 schema 拷贝为契约基线） | 包 | 模型（参数生成）、broker（class 判定） | schema 与宿主实现双向对齐由 contract 测试锁（§10 S-1），漂移即测试红 |
| 3 | `draft_hash` 算法（SHA256 + 规范 JSON + `sha256:` 前缀） | **宿主**（草稿域） | `AlertRuleConfigDraftHashForID`（`config_tools.go:386`） | apply 闸（`:339`）、审计 | 包**只搬运不计算**：哈希必须由发草稿的同一 manager 进程算出才有效力；包内实现哈希 = 绕过租约 |
| 4 | 规则 DSL：`kind`（9 值）、`scope_type`、`spec`、`join_mode`、`severity`、`enabled` | **宿主**（`model/alert/model.go`） | 宿主 | 草稿校验、预览、评估器、包 schema（引用） | 宿主扩 DSL 不需要包发版；包 schema 的枚举是**快照**，contract 测试保证未知 kind 被拒而不是被静默吞 |
| 5 | 发送策略 `NotifyWindowSeconds` / `NotifyMinFires` | **宿主**（`usecase.go:667-673`） | 宿主（校验 :820-831、执行 :1312-1321） | 草稿 schema（字段穿过） | **唯一跨界的降噪词表**：包 schema 必须带这两个字段（草稿要校验成对性），但语义（窗口计数、阈值判定）归宿主管线 |
| 6 | 抑制语义（inhibit 规则） | **宿主**（`inhibit.go`；未来 `inhibition_rules` 表） | 宿主 | 管线 | **不进包**（§9.2）：无工具面，且自定义抑制是表数据不是可分发工件 |
| 7 | 关联语义（`FiringCorrelator` 接缝） | **宿主**（`correlate.go:30`） | 宿主定义接口 | demo（现状）/ 5.x 实现 | 接缝登记给 5.x；本包不实现不引用 |
| 8 | 语义去重（`SemanticDedupService`） | **宿主**（待 D-4 裁决去留） | — | —（零调用者） | 不进包；若留则接线归宿主，若删则独立小 PR（§9.3） |
| 9 | `query_alert_rules` | **readonly 包**（4.8 已裁定） | readonly | 模型、command 草案引用 | 本方案不动；command 草案的 `allowed-tools` 只是引用方 |
| 10 | `list_metric_catalog` | **observability 包**（4.8 已裁定） | observability | 草稿前置证据（其指令字段已显式指向 `draft_config_change`，`metric_catalog_tool.go:30`） | 同上；两个包之间通过 prompt 指令弱耦合，无 schema 依赖 |
| 11 | 通知渠道字段 `notify_channel_ids` | **宿主**（规则配置字段） | 宿主 | 4.5 的出站侧消费 | 本包词表里它只是"规则的一列 id"，发送语义归 4.5；command 草案已写明"发送不是你的事" |
| 12 | command 文件（`command-alert-rule.md`） | **插件包**（prompt 面） | 包 | marketplace pack → manager 技能根 | frontmatter `allowed-tools` 是**信息性**的，不是闸门；真实控制仍是 broker 允许清单 + 收据（§8） |

**判定中容易被问"为什么"的三条**：

- **#2 为什么 schema 归包、#4 为什么 DSL 归宿主**：schema 是"模型怎么叫这个工具"，是包的对外 ABI；DSL 是"一条规则是什么"，是域事实。宿主明天加第 10 种 kind，包不需要发版（contract 测试会证明未知 kind 被明确拒绝）；包明天改参数名，是包的 0.2.0。
- **#3 为什么哈希归宿主**：`beginApply` 租约校验的是"同一 manager 进程发给同一 UserID 的草稿"。若包能算哈希，包就能替任何人伪造确认对象——哈希的效力正来自"发出者与校验者是同一进程"。
- **#5 为什么发送策略是唯一跨界词表**：它是降噪四件里唯一以**规则字段**形态存在、因此唯一会出现在草稿 schema 里的。其余三件（inhibit/correlate/语义去重）都不是规则字段，没有进 schema 的理由。

---

## 5. 方案选型（四候选 + 取舍）

### 5.1 候选 A（采用）：新包 `opskeeper-sre-alert`

形状：edge / L2 / read+write / `alert.read`+`alert.write` / pin / 2 工具（工具名从 repair 硬切）。manifest 草案见 `alert-manifests/alert-rule-config.yaml`，关键论证在 §7。

**为什么必须动 repair**：repair 自己的 manifest 头写它的承诺是"对单个具名目标的、人可回退的有界操作"。重启一台机器的 systemd 单元是；决定**什么时候叫醒人**的检测策略不是。两者捆绑的代价已经在 `profiles.go:125` 显形——finance profile 的 Intent 被迫解释"为什么授予 alert.write"："the repair package that does the restarting also commits confirmed alert-rule drafts — a package that bundles the two, so granting one without the other leaves the flagship package uninstallable"。**这是把章程错配写进了部署档案**：一个只想让包重启主机的舰队被迫同时接受"包能改告警规则"，反之亦然。拆开后两种舰队各取所需，弱承诺不再治理强承诺。

### 5.2 候选 B（放弃）：留在 repair 做版本内增量

即 4.8/4.9 的默认走向：repair 0.3.0 继续带着这两个工具。

- 优点：零迁移成本，不动 repair 的版本号语义，不动两个 profile 的 Composes。
- 放弃原因：①它把 §5.1 的章程错配**固化**而非修复，finance Intent 那段解释性文字会越来越长；②4.9 刚给 repair 加了 `preview.read`（台账 §3.4），包在向"更纯粹的宿主操作"收敛，这时往里塞一个域事件策略面方向相反；③rollback 语义不同质——repair 的回滚是"重启你刚重启的东西"，告警的回滚是"恢复你刚改的规则定义"，两类回滚塞进同一个工具的输出 schema 会互相污染。

### 5.3 候选 C（放弃）：readonly 扩展（加写工具进 readonly）

- 放弃原因：一句话否决——readonly 的章程是 read-only。往里加 write 工具等于让"最广部署的包"（20 个只读工具、几乎每个 profile 都组合）携带写能力，这与 4.8 拆出 readonly 的初衷正好相反。列在这里只为完整：这是四个候选里唯一一个"看起来省事、实则最危险"的。

### 5.4 候选 D（放弃）：不做（维持现状）

- 现状 = R1 的真空 + R2 的散文 + R3 的窄词表 + alert.write 捆在 repair 里。
- 放弃原因：R1 是 G0 级（声明了、放行了、调不通），不修等于让 manifest 持续说谎；4.1 §7 结论⑤点名这个缺口；"不做"还把 finance Intent 的错配解释永久化。

### 5.5 取舍表

| 维度 | A 新包（采用） | B repair 增量 | C readonly 扩展 | D 不做 |
|---|---|---|---|---|
| 修 R1（上呼缺口） | ✓（S-1 顺带补分发表） | ✓ | ✓ | ✗（缺口留着） |
| 修章程错配（finance Intent） | ✓（Intent 改写为两段各自成立） | ✗（固化） | ✗（更糟） | ✗ |
| 迁移成本 | 硬切一个版本窗口（§6.2） | 最低 | 低 | 无 |
| 审批面评审负担 | 2 工具一个包，一个 approval 语义 | repair 的 approval 语义被迫覆盖两种承诺 | readonly 被迫定义写路径审批 | — |
| 部署灵活性（舰队可单选） | ✓ | ✗ | ✗ | ✗ |
| 风险 | 迁移窗口内两工具短暂"无家"（护栏：唯一性测试） | 无迁移风险但欠债增大 | 章程破坏 | 声明面持续失真 |

---

## 6. 设计：extension tool 面

### 6.1 包形状

见 `alert-manifests/alert-rule-config.yaml`。要点（论证全文在 §7 与文件内注释）：

- `targets: [edge]`：包装进节点 agent，工具经上呼由控制面服务——规则仓、预览器、草稿租约都在数据库所在处，且**哈希必须由发草稿的进程算**。
- `tools` 恰好两个：`draft_config_change`（read）/ `apply_config_change`（write）。**刻意不是一个动作一个工具**：draft→confirm→apply 这个形状本身就是控制，五个动作专属工具就是五条要评审的审批路径、五个可能漏绑哈希的地方。
- `required_scopes: [alert.read, alert.write]`，都是既有 scope（4.5 已用 alert.write），**零 ABI 变更**。
- `install.strategy: pin`：决定"什么时候叫醒人"的包不该被自动升级——操作者按 0.1.0 的校验规则批准的草稿，不等于按 0.2.0 的校验规则批准的。

### 6.2 硬切而非双宿

仓内的所有权测试断言一个工具名恰好被一个包声明（`core/floor/pluginmanifest/repair_profile_test.go` 的唯一性断言；节点允许清单也无法对同一个名字兑现两种承诺）。因此发布顺序是**一个版本窗口内的原子对切**：

1. 同一 PR：repair 0.3.0 的 tools 删两行 + 新包 0.1.0 加两行 + 两 profile `Composes` 各加 `opskeeper-sre-alert` + finance `Intent` 改写（把"捆绑所以一起授"那句换成两段各自成立的理由）。
2. 唯一性测试是该窗口的护栏：任何"两边都留着"的中间态会让测试红，红就是设计意图。
3. 回滚 = 反向同一 PR（版本号再 +1），无数据迁移、无状态清理——工具名到实现的映射是声明性的。

### 6.3 上呼接线（S-1，兑现 4.8 的"已恢复"）

`dispatchAgentTool`（`main.go:7043`）的分发表加两个名字 → `configchange` 的两个 BaseTool。这是 §3 R1 的修复，也是新包"看得见也调得通"的前提。分发表的映射与 manifest 的 class 声明一致性由 S-1 的 contract 测试断言（manifest 说 read 的，分发表路由到的实现不得有写副作用）。

### 6.4 动作词表的扩张归宿主（S-3，非包自发）

R3 的修复是**宿主草稿域**扩 `action ∈ {update, delete}`（`normalizeConfigCreateAction` → 泛化的 `normalizeConfigAction`），包 schema 随之扩枚举。顺序不能反：包先把 `update` 写进 schema，宿主还是会以 `:310` 拒绝——那只是把拒绝从 schema 层挪到运行时层，没有增加任何能力。`delete` 的草稿必须携带规则当前定义（那就是回滚的唯一副本），这条进宿主校验而不是包 prompt。

---

## 7. 写工具安全级别论证（无静默降级）

### 7.1 L2，不是 L3

`ClassWrite` 的定义是"改变 OpsKeeper 管理的状态"；`ClassDestructive` 是"改变 OpsKeeper 控制之外的状态"。一次 apply 写**一行 `alert_rules`**——OpsKeeper 自管的表、自管的行。声明 L3 是夸大，而夸大的代价是具体的：finance profile 的 L3 上限（`profiles.go:135`）会把这个包整个拒之门外，换不来任何真实安全。

### 7.2 write + approval，两重闸

- **broker 收据**：`cmd/opskeeper-edge/policy.go:79-86`——允许清单是已准入包 manifest 的投影（:56），任何写级及以上调用必须携带闸门收据，收据被消费，一次批准一次执行（`core/edge/policygate/fence.go:38-43`：批准、收据、拒绝是同一键的三种视图）。
- **工具自身哈希绑定**：`validateApplyGate`（admin + confirmed）之上，`validateDraftHash` 重算并比对，`beginApply` 租约再核草稿存在/未过期/**同一 UserID**/动作一致。第二重闸让"批准了"意味着"批准了**这条**规则"，而不是"刚才批准过点什么"。

两闸都不依赖包自觉：包不实现收据校验（broker 层），也不计算哈希（宿主层）。**没有静默降级路径**——缺收据被 broker 拒，缺哈希被工具拒，非 admin 被角色闸拒，三者都是硬错误不是警告。

### 7.3 radius = pod，与决策点 D-3（词表盲区，如实记录）

`max_blast_radius: pod` 是词表里最窄的值，也是对**这次变更本身**诚实的值：一次 apply 恰好写一行。但词表是基础设施形状的（none/pod/single-ns/namespace/cluster），它没有值能表达"一条 `global` 规则改变整个舰队被叫醒的行为"。这个真实触达由三样东西承载，且都不在本字段里：规则自身的 `scope_type`（草稿披露、操作者确认的对象）、绑定确认到该 scope 的 draft_hash、以及审批面人类读到的 scope 摘要（`alertRuleScopeSummary`）。

**D-3（评审裁决）**：是否为"配置类写"引入新的 radius 语义（如 `config`），还是维持"radius 描述单次变更的物理触达、语义触达由草稿披露"的现行分工？**本方案推荐后者**：改词表是 ABI 变更、波及全部既有包的重新评审，而现行分工加上 command 草案第 3 步"把 scope 说出声再停"已经把语义触达推到人眼前。维持推荐的同时把它记为决策点而不是悄悄不改——这是 4.9 D4 的同款处理。

### 7.4 回滚是一等字段（承接 4.9 R2 / D5）

现状两处散文（`alert_rule_manager.go:169`/`:310`）。S-2 把输出里的 `Rollback` 换成结构化 `RollbackPlan`：

```
RollbackPlan {
  action     string   // 逆动作：create→delete_rule / update→update_rule(旧值) / delete→create_rule(旧定义)
  rule_key   string   // 目标规则的稳定键（不是自增 id）
  payload    json     // 可直接喂给逆动作的草稿载荷（delete 场景=被删定义；update 场景=旧字段值）
  manual     string   // 控制台路径（散文保留，但从"唯一内容"降级为"人工兜底"）
}
```

- `payload` 由**宿主**生成（发草稿的进程才有旧值），随草稿一起返回、随 apply 一起入审计——4.9 A 系"审批证据"的结论在这里复用：人批准的对象应当携带它的逆。
- delete 的回滚唯一副本在草稿里（§6.4），这条规则进宿主校验。
- **不做通用 undo 工具**（4.9 D5 已裁定）：`RollbackPlan` 是可机器消费的字段，但执行它的入口仍是 `draft_config_change`（把 payload 当新草稿再走一遍确认）——回滚与人向变更走同一条被批准的路，不另开后门。

### 7.5 scope 集合：两个，恰好两个

`alert.read` 是起草者改/删现有规则时的读回需要；`alert.write` 是 apply。多一个都是"没有锁的钥匙"——本包没有工具能触达主机、数据库或渠道。

---

## 8. 设计：command 面

### 8.1 通道事实

`ConvertCommandFile`（`command_parser.go:27`）把 `commands/<name>.md` 解析为 `cmd_<name>` 关键词技能（`:41-43`：`/foo` 与 `foo` 两个关键词）。frontmatter 的 `allowed-tools` 是**信息性的**——解析器不强制它，真实控制仍是 broker 允许清单 + 收据。这恰好是"command 管纪律、工具管权限"的正确分工：command 文件被篡改不会提权，只会得到一个话术很差的助手。

### 8.2 送达路径（A3 的裁决）

| 选项 | 做法 | 判定 |
|---|---|---|
| **marketplace pack（采用）** | command 文件随一个 pack 发布，marketplace 安装落进 `/var/lib/opskeeper/skills`（`main.go:3094`），被既有走查发现 | 零内核改动，符合"prompt 面也是可分发工件"的插件叙事；pack 的安装审批就是 command 的送达审批 |
| 宿主接线 | 把 pig-ops 树加进 manager 技能根 | 放弃：让 governance 树与 prompt 树共享一个根，等于让"被准入"隐式意味着"被注入 prompt"，这个耦合迟早咬人 |
| 不做 | command 半边放弃 | 放弃：4.1 §7 结论⑤明确要求 tool + command 两个半边；且 R3 场景（"用户要禁用规则"）正是 prompt 纪律最值钱的地方 |

草案 `alert-manifests/command-alert-rule.md` 的四步流（先读后写 → 起草 → **把 scope 说出声然后停** → 只 apply 被确认的）与"Never"清单（不出无哈希的规则散文、不在首次提出的同一轮 apply、不静默放宽 scope、不把通知发送当成本流一部分）都是对着本域的真实事故形态写的。

### 8.3 command 与工具面同包不同子树

`opskeeper-sre-alert` 的发布物 = manifest（tools 面）+ marketplace pack（prompt 面）。两者版本各自走（prompt 改话术不值得 bump 包版本），但 command 里引用的工具名以包的 tools 清单为准——command 引用一个包未声明的工具不是错误（allowed-tools 不强制），但会让助手在纪律上号召一次必然失败的动作，contract 测试按"引用集 ⊆ 包内 ∪ 其它已落地包"断言。

---

## 9. 降噪插件化判定

### 9.1 结论：v1 不做成包

降噪四件（§2.4）没有一件有工具面。pig-ops 包的承载物是"工具 + manifest 声明的能力/scopes"，把没有工具面的语义做成包，得到的只是一个装着声明的空壳——而声明没有对应执行面正是 R1 的病。

### 9.2 发送策略是唯一跨界词表，契约即字段校验

`NotifyWindowSeconds`/`NotifyMinFires` 出现在包 schema 里（草稿要校验成对性、范围），语义（窗口计数、阈值判定）归宿主（`usecase.go:1312-1321`）。宿主若改判定语义（比如窗口改为滑动），包 schema 不动、command 话术不动——契约窄到只有"这两个字段成对、有界"。

### 9.3 `SemanticDedupService` 处置（决策点 D-4）

- **选项 1（推荐）：删除**，理由与 4.9 R3 一致——零调用者的完整实现是"看起来有 LLM 降噪"的错觉；真要语义去重，正确入口是 §9.4 的接缝而非旁路服务。删除是独立小 PR，回滚 = revert。
- **选项 2：接线**——接进管线（inhibit 之后、发送之前），但那是一次行为变更（LLM 进告警关键路径），需要熔断默认开、灰度开关、失败放行（fail-open 到"不聚合"）三件套，成本远超本方案范围。
- **选项 3：留置**——不推荐，留置 = A1 琥珀永久化。
- 无论选哪个，**都不进包**：它是管线内语义，不是工具。

### 9.4 未来插拔点登记（已具备接口形态，承载面判定先行）

| 插拔点 | 现状 | 若要插件化，承载面 |
|---|---|---|
| `Inhibitor` 接口（`inhibit.go:13`） | 接口 + 内置实现 | **宿主表**（`inhibition_rules`）：自定义抑制是运维数据，改它不该发版工件；包形态只在"第三方抑制算法"出现时才值得讨论 |
| `inhibition_rules` 表 | 注释承诺（`inhibit.go:25-27`），未建 | 宿主数据面 + 管理面 CRUD（走既有 rule CRUD 的审计形态），**不是**本包工具 |
| `FiringCorrelator` 接缝（`correlate.go:30`） | 接缝 + demo 接线（`main.go:1895`） | 5.x 的天然入口；等价于"故事线预开"的领域语义，登记给 5.x 不抢注 |
| 语义去重 | 见 §9.3 | 不插件化 |

**这一节的存在本身是交付物**：4.1 §7 结论⑤问的是"降噪怎么插件化"，诚实的答案是"它今天没有可插件化的承载物；接缝已经预埋好，承载面判定如表"。

---

## 10. 实现切片（纯方案，以下均为后续 PR 的内容与验收）

| 切片 | 内容 | 测试 | 回滚 |
|---|---|---|---|
| **S-1 新包落地 + R1 修复** | 新包目录与 manifest；repair 0.3.0 删两行；两 profile `Composes` + finance `Intent` 改写；`dispatchAgentTool` 分发表补两行；sdk 三段闸门固化进仓内测试（§11.1 的推演变实测，并入 4.11 harness） | ① manifest 三段闸门（finance 与 SaaS 两 Admission 各跑一遍，含零值 Admission 拒绝）② 工具名全包唯一 ③ 分发表 class 与 manifest class 一致 ④ 上呼 integration：节点发起 draft→apply，断言两重闸与 ErrNotFound 消失 | 反向同 PR（版本 +1，声明性映射，无数据迁移）；repair 回 0.2.0 工具清单即恢复原状 |
| **S-2 RollbackPlan 结构化** | `alert_rule_manager.go:169`/`:310` 的散文换成结构化字段（§7.4），宿主生成 payload，随草稿与 apply 进审计 | ① 单测：RollbackPlan 形态（S-2 时只有 create 场景可测，update/delete 场景断言随 S-3 补齐）② 断言 payload 可直接作为逆动作草稿通过校验 ③ 审计事件含 rollback 摘要 | 字段加在输出里，旧消费者忽略未知字段即兼容；回滚 = revert |
| **S-3 动作词表扩展（update/delete）** | 宿主 `normalizeConfigAction` 泛化；delete 草稿强制携带当前定义；包 schema 枚举扩展；command 草案 Actions 节改写；`http.go` 与会话面词表对齐测试 | ① 宿主单测：非法动作仍拒 ② 会话面能表达 enable/disable（R3 关闭断言）③ contract：管理面与会话面动作集一致 | 词表扩展按"未知动作明确拒绝"实现，回滚 = revert（旧客户端发新动作得到明确错误而非半成功） |
| **S-4 command pack** | `command-alert-rule.md` 进 marketplace pack；引用集断言测试 | ① command 解析单测（关键词、frontmatter）② 引用集 ⊆ 包内 ∪ 已落地包 ③ pack 安装后被技能根走查（integration） | 卸载 pack 即移除 command；工具面不受影响 |

每片都过 4.11 通用闸门（§11）。

---

## 11. 通用闸门

沿用 4.5 §11 / 4.9 §11 的形状，逐项给本包的答案：

| # | 闸门 | 本包答案 | 状态 |
|---|---|---|---|
| 1 | manifest 三段闸门（Decode/Validate/Admit） | 见 §11.1 推演 | **静态核对（未实测）**，S-1 固化 |
| 2 | 工具名全包唯一 | 两个名字从 repair 硬切，窗口内唯一性测试是护栏 | 设计保证，S-1 实测 |
| 3 | scope ⊆ 宿主授予集 | `alert.read`/`alert.write` 均已授予（finance Intent :125 明文包含 alert.write） | 绿（静态） |
| 4 | 安全级别 ≤ profile 上限 | L2 ≤ finance L2；L2 ≤ SaaS L3 | 绿（静态） |
| 5 | radius ≤ profile 上限 | pod ≤ finance pod；pod ≤ SaaS ns | 绿（静态） |
| 6 | approval 收据链 | broker 收据 + 工具哈希绑定两重闸（§7.2） | S-1 integration 实测 |
| 7 | 审计不可伪造 | `audit.emits: true` + `mutates: false`；台账条目由宿主闸门事件派生 | 绿（静态） |
| 8 | 台账行同步 | `plugin-governance-ledger.md` §3.2 新行随本 PR | 本 PR 已加 |
| 9 | 签名 | 待签（分发通道未接线，同 4.3–4.7 各包） | S-4 时处理 |
| 10 | 中文注释/提交说明 | manifest 与本方案注释为中文/双语，提交说明中文 | 本 PR 满足 |

### 11.1 manifest 三段闸门静态推演（未实测，如实声明）

**声明**：本任务运行环境无 Go 工具链（`go` 不在 PATH，无 Docker、无模块代理网络），sdk 三段闸门**未实际执行**。下表是对 `sdk/manifest.go` 规则的逐条推演；S-1 切片的第一条验收就是把它固化成仓内测试（沿用 4.5 §11.1 的先例：任务期间的验证落成仓内测试）。落位上应并入 4.11 的闸门 harness（任务 4.11：把各方案的准入闸门固化成可重复测试），本包只是给它添一行输入，而不是另起一份 `alert-manifests_test.go`——若 4.11 尚未合入，S-1 先自建、4.11 合入时收敛。

| 段 | 规则（来源 `sdk/manifest.go`） | 草案的符合性 |
|---|---|---|
| `Decode`（:62） | YAML + `KnownFields(true)`：未知字段即拒 | 草案只用了既有个字段（apiVersion/kind/metadata/spec 的 targets/safety_level/capabilities/tools/required_scopes/audit/approval/install），无未知键 ✓ |
| `Validate`（:94） | 身份：name 非空且合式、version 严格 semver | `opskeeper-sre-alert` / `0.1.0` ✓ |
| | safety_level ∈ {L0..L3} | `L2` ✓ |
| | write 级包必须声明 radius | 已声明 `pod`（缺省会推导 namespace，声明是主动收窄）✓ |
| | 只读级包携带 radius 被拒 | 包含 write 工具，不适用 |
| | `audit.mutates: true` 在装载期被拒 | 草案 `false` ✓ |
| | tools 名字非空、class ∈ {read, write, destructive} | 两工具 read/write ✓ |
| `Admit`（:392） | `required_scopes ⊆ GrantedScopes` | finance：alert.read、alert.write 均在授予集（Intent :125）✓；SaaS：SaaS 授予集更宽 ✓ |
| | `safety_level ≤ MaxSafetyLevel` | finance L2 ≤ L2 ✓；SaaS L2 ≤ L3 ✓ |
| | `max_blast_radius ≤ MaxBlastRadius` | finance pod ≤ pod ✓；SaaS pod ≤ ns ✓ |
| | 零值 Admission 拒绝一切 | 设计意图（fail-closed），S-1 测试断言 |

---

## 12. 4.1 / 4.2 一致性自查表

### 12.1 对 4.1（business-domain-mapping）

| 4.1 的板块/结论 | 本方案的一致性 |
|---|---|
| 板块⑤：告警配置写路径未进插件词表（§7 结论⑤） | 本方案即其写侧答案：新包 + 硬切 + 上呼修复；只读侧（`query_alert_rules`）按 4.8 维持 readonly 不动 |
| 4.1 §7 结论：告警规则语义归宿主 | §4 #4/#5/#6/#7 一致：DSL、发送策略、抑制、关联全部归宿主 |
| 4.1 的"extension tool + command"双半边 | §6 工具面 + §8 command 面，且 §8.1 明示 allowed-tools 非强制、真实控制仍在 broker |
| 4.1 对降噪的期待 | §9 给出判定而非含糊：v1 不成包、唯一跨界词表是发送策略两字段、插拔点登记 |

### 12.2 对 4.2（inventory）

| 4.2 的盘点项 | 本方案的细化 |
|---|---|
| 会话侧配置工具（2 个） | 归属从 repair 改判为 alert 新包（§5），台账行同步 |
| 草稿流（draft→confirm→apply） | 完整证据链 §2.3；缺口 R2（散文回滚）/R3（窄词表）与切片 S-2/S-3 |
| 降噪管线 | §2.4 四件逐项 + §9 判定；`SemanticDedupService` 零调用者作为 A1/D-4 |
| 审计事件 | `rule_create/update/delete` 已存在（`port.go:144-146`），会话路径复用同一动作集（S-2 起带 rollback 摘要） |
| 与 5.x 的界面 | §1.3：`FiringCorrelator` 接缝登记给 5.x，本方案不抢注 |

### 12.3 自查结论

无与 4.1/4.2 冲突的判定；两处**修正**（4.8 对两工具"已恢复"的过报 → R1；repair 携带告警工具的既成事实 → §5 拆包）都已给出证据与承接切片。

---

## 13. 备选方案与放弃原因（浓缩）

见 §5.5 取舍表。一句话版：**B（repair 增量）省一个版本窗口但固化章程错配；C（readonly 扩展）违反 readonly 章程且让最广部署的包携带写能力；D（不做）把 G0 级声明真空留在 manifest 里持续说谎。** 降噪侧的备选（做成包 / 留置语义去重）在 §9.1/§9.3 给出并放弃。

---

## 14. 验收对照

| 验收项 | 状态 |
|---|---|
| 现状审计有代码证据覆盖三块 | ✓ §2.2/§2.3/§2.4 逐项带文件:行号 |
| 词表归属逐项判定且跨面契约明确 | ✓ §4 十二项，每项有生产者/消费者/契约 |
| 写工具安全级别显式论证（L2/write/approval/回滚一等字段）无静默降级 | ✓ §7 四小节；两重闸、三处硬拒绝、RollbackPlan |
| manifest 闸门按 sdk 三段实测 | **未实测**（无 Go 工具链），§11.1 静态推演 + S-1 固化承诺 |
| 降噪插件化点判定（或明确不成包） | ✓ §9，含不成包的理由与插拔点登记 |
| manifest 草案 | ✓ `alert-manifests/alert-rule-config.yaml` |
| command 草案 | ✓ `alert-manifests/command-alert-rule.md` |
| 4.1/4.2 一致性自查表 | ✓ §12 |
| 边界（5.x / 4.5）显式 | ✓ §1.3 |
| 受审计文件不含内部任务编号 | ✓（本文件与 manifest 均以 OpenSpec 任务号 4.12 表述，不含内部编号） |

---

## 15. 提交与执行规范

- 本 PR 只含方案与草案：`alert-plugin-plan.md`、`alert-manifests/` 两份草案、台账 §3.2 新行、`tasks.md` 4.12 勾选。**不含任何生产代码**。
- 提交说明中文，注明关联 OpenSpec change 任务 4.12。
- 四个决策点（D-3 radius 语义、D-4 语义去重去留、S-3 词表扩展的时点、S-4 pack 归属）在 PR 描述列出，评审先裁决 D-3/D-4 再排 S-2/S-3。
- 切片落地时逐片过 §11 通用闸门并同步台账行状态（待实施 → 已落地）。
