# 4.5 notifier 接内核改造方案 + 渠道插件化（飞书 / 钉钉 / Slack / PagerDuty / Webhook）

> 状态：**方案 v0.1（待评审）**。对应 `tasks.md` 4.5；评审通过前不勾选 4.5。
>
> 基线：`main@9f8bc820`（本任务打开时的 HEAD）。与 4.1 `business-domain-mapping.md`、4.2 结论保持口径一致。
>
> 范围：**仅方案产出**。本文档回答"通知渠道怎么从宿主内工厂注册表接到 `kernel.KindNotifier`、`Activate` 是不是唯一注册入口、六条既有渠道怎么在默认部署行为不变的前提下迁移、每个候选渠道的差异化能力与回滚"。§13 的实现切片建议**不**在本任务内合入；任何对 `core/floor/kernel/kind.go`、`core/base/pkg/notify/registry.go`、`core/manager/biz/alert` 告警派发主流程或 `plugins/` 包结构的真实改动，必须另开 change 并独立评审。

## 1. 目标与边界

### 1.1 目标

把 `notifier` 扩展点接进微内核，并把通知渠道从"改一行编译内代码"变成"加一个包"。三件事：

| # | 目标 | 判定 |
|---|---|---|
| G1 | `core/base/pkg/notify` 的自有 `ChannelFactory` 注册表迁移到 `kernel.KindNotifier`，`Activate` 成为注册唯一入口 | §3、§5 |
| G2 | 六个既有渠道（webhook / slack / feishu / dingtalk / wecom / telegram）成为**编译内预装插件**，默认部署行为不变 | §5.2 |
| G3 | 第一批第三方/增强候选：PagerDuty（新渠道）+ 飞书卡片 / Slack Block Kit / 钉钉 markdown / Webhook 强化，每候选有 manifest、注册路径、测试、回滚 | §6–§10 |

### 1.2 与 4.1 / 4.2 的边界

- **4.1 §3**：`notifier` 状态为"**未接内核**"，证据 `core/base/pkg/notify/registry.go` 自有 `ChannelFactory` 注册表。本方案的全部内容就是消掉这一行的状态，不新增第七个扩展点。
- **4.1 §4 ⑤ 事件板块**："告警外发通知"行标注为"宿主注册表已有，未接内核 / 迁移为插件 / 后续任务 4.5"。本方案不改这一行的归属，只补上路径。
- **4.1 §6 缺口表**：`notifier 未接内核，通知渠道只能编译内增加 → 事件、企业集成 → 4.5`。本方案收口该行。
- **4.2 四分类**：notifier 归"**迁移为插件，未接内核**"。本方案只在"迁移"分支内推进，不触碰"保留在 core / 暂缓迁移 / 淘汰"三类。
- **内核契约**：`core/floor/kernel/kind.go` 的六 Kind 封闭集合不变。notifier 复用 `KindNotifier`，**不新增字符串**（`kind.go` 中该 Kind 的注释本身就写着 "replaces the hardcoded switch in the alert dispatcher"）。
- **4.1 §7 待评审问题**：本方案不触碰其中任何一条——嵌入模型、MCP/技能宿主内管理、告警规则插件化归类，均不在本文范围（告警规则在 4.1 §4 已标注"需在权威 change 补任务"，本方案只读不改）。

### 1.3 非目标

- **不写实现代码。** §13 只给骨架与禁项。
- 不改 `core/floor/kernel/kind.go`（六 Kind 封闭集合）。
- 不改 `core/base/pkg/notify/registry.go` 的现有语义（§5 明确说明这如何迫使迁移分三片，以及第三片为什么必须独立 change）。
- 不改告警派发器主流程（`core/manager/biz/alert/usecase.go` 的 `MaybeNotify` / `resolveChannels` / 重试 worker）。
- 不改 `plugins/` 包结构：候选包**不落**本仓 `plugins/` 目录（理由见 §6.2）。
- 不做 provider / collector / adapter（4.6 / 4.7 / 4.4）。
- 不引入入站方向的 IM 能力（`core/manager/biz/imbridge`）——那是入站，不属六扩展点，4.1 §4 ⑨ 已注明。
- 不启用 HITL 审批桥（5.5，默认 `approvalBridgeEnabled=false`）。

## 2. 现状证据

### 2.1 出站通知链路全貌

```text
三条入口，一条出口：

  ① 告警派发        core/manager/biz/alert/usecase.go: MaybeNotify
     │                 resolveChannels → []*model.Channel（DB 行）
     │                 BuildSenderFromChannel(ch)  ← 逐行从 ConfigJSON 构造
     ▼
  ② 渠道工厂        core/base/pkg/notify/registry.go
     │                 channelFactories{ webhook, slack, feishu,
     │                                    dingtalk, wecom, telegram }   ← 硬编码字面量
     │                 RegisterChannel(typ, f)   ← 包级注册，init 期调用
     │                 BuildChannel(spec)       ← 唯一解析入口
     │                 NewRouterFromSpecs(...)  ← 配置面 specsFromConfig
     ▼
  ③ 发送器          core/base/pkg/notify/webhook.go
                       newWebhookSender(buildBody, signTarget) 六种组合
     ▼
     Router.Send(ctx, msg, channels...)   ← 按实例名分发，逐渠道一个 timeout
     ▼
     ④ 投递留痕       notification_deliveries（宿主写）
                       StartDelivery / FinishDelivery
                       RetryWorker 失败重试，默认 5 次、线性退避 1 分钟起步

另一条入口：Router.SendVia(ctx, msg, sender) —— 直接给一个已构造的 Sender，
用于 DB 渠道行；与 ② 同源（同一个 BuildChannel）。

环境配置面：config.NotificationConfig → specsFromConfig → NewRouterFromSpecs

  ┌──────────────────┬─────────────┬────────────────────────┐
  │ ① 进程配置       │ ② 渠道工厂  │ ③ 发送器               │
  │                  │             │                        ▼
  │                  │             │   Router.Send（按实例名）
  │                  │             └─ BuildChannel（按渠道类型，
  │                  │               DB 渠道行走这里）        │
  └─ notification_channels 表（channel_type + config_json）    │
                                                                 ▼
                                        外部系统（Slack / 飞书 / 钉钉 / …）
```

### 2.2 三个必须改口径的事实

| # | 事实 | 证据 | 对方案的影响 |
|---|---|---|---|
| F1 | 通知是**唯一一条会主动改外部系统**的扩展点链路 | 六个渠道每次发送都在外部系统里创建一个消息对象；`core/domain/safety.go:105-111` 把 L3 定义为 "mutates external systems" | 安全级别不能沿用 datasource 的 L1/只读假设，见 §4.2 |
| F2 | 现有投递成功判定**只看 HTTP 状态码** | `webhook.go` `Send()`：`resp.StatusCode < 200 || >= 300` 即失败，2xx 一律成功 | 钉钉/飞书/企微的 `errcode != 0` 会被记成投递成功（§2.3 缺口 G-1） |
| F3 | 渠道类型名在服务端**没有白名单** | `core/manager/service/alert/service.go:773 validateChannelInput` 只校验 name/type/endpoint 非空，不校验 type 是否已知 | 新渠道类型不需要改 model 常量就能建行；缺的是工厂，不是校验 |

### 2.3 已识别的三个缺口（迁移后仍在，候选包解决）

| 缺口 | 现象 | 证据 | 本方案的处置 |
|---|---|---|---|
| G-1 **2xx 即成功** | 钉钉签名错（`310000`）、限流（`130101`）、无权限（`300001`）都返回 HTTP 200 + 非零 `errcode`，被记成投递成功，重试 worker 不重试 | `webhook.go` `Send()` 只判 status；`tests/e2e/notify_signed_test.go` 的假端点固定回 200 | §9（钉钉候选）与 §10（webhook 候选）解决；存量六渠道的修复需独立 change（§17.1） |
| G-2 **Router 实例名覆盖** | `NewRouter` 里 `r.channels[sender.Name()] = sender`，同名 sender 后者静默顶替前者，无日志无告警 | `core/base/pkg/notify/notify.go` `NewRouter` | **不改**（`notify.go` 不在本任务改动面）。§3.4 记录为已知缺口 + 实现切片建议 |
| G-3 **渠道能力面在配置里不可见** | 渠道行只有 `endpoint` + `secret` 两个字段，飞书卡片 / 钉钉 @ / Slack thread 这类能力既没有配置位也没有清单位 | `encodeChannelConfig(in.Endpoint, in.Secret)`（`service.go:458`）；`web/src/pages/settings/Notifications.tsx:38` 的类型是六个字符串字面量 | §13 S-5 提出"能力由 manifest 声明、配置只选变体"的分工 |

## 3. 注册语义：内核 entry 名 = 渠道类型

这是整个方案的地基。**内核 `KindNotifier` 的注册名就是渠道类型**（`webhook` / `slack` / `feishu` / `dingtalk` / `wecom` / `telegram` / `pagerduty` / …），注册的实现体是 `notify.ChannelFactory`。

### 3.1 为什么是"类型"而不是"实例"

| 候选命名 | 后果 | 判定 |
|---|---|---|
| **entry 名 = 渠道类型** | 与今天的 `channelFactories` map 键一一对应；一个包实现一个类型；多实例由 Router 的实例名层处理；`ErrDuplicate` 的语义 = "同一渠道类型只能有一个实现" | **采用** |
| entry 名 = 渠道实例名（`ops-slack-prod`） | 同一 Slack 要装两个包就得给两个实例起两个包名；而 `Activate` 的贡献体是**工厂**，不是发送器实例，注册实例等于把配置数据塞进插件注册表 | 拒绝：注册表是能力目录，不是配置存储 |
| entry 名 = 包名（`opskeeper-notifier-slack`） | 类型与包名解耦后，`slack` 这个键要二次查表，等于把 `channelFactories` 原样搬进内核 | 拒绝：迁移变成"多搬一层"，不接内核也做不到 |

采用第一条还有一个副产品：`pluginhost.Inventory()` 里 notifier 条目的 `name` 直接就是运维在设置页看到的 `channel_type`，`impl` 是工厂的类型名（`kernel/inventory.go` 故意只渲染类型名而不渲染实现值，以免把 URL 或 token 打进日志）。诊断面不需要另写映射。

### 3.2 同名互斥：`ErrDuplicate`，无覆盖语义

- `core/floor/kernel/registry.go:68-72` 对同一 `(Kind, Name)` 的二次 `Register` 返回 `ErrDuplicate`，**不覆盖**。notifier 继承这一条，不另发明语义。
- `core/floor/kernel/host.go` `Activate` 对已激活的包名返回 "plugin is already active"，同样不是替换。
- 落到本扩展点的具体含义（这是本方案要写给评审看的一句）：**一个渠道类型在一个进程内只有一个实现包**。`opskeeper-notifier-slack` 与 `opskeeper-notifier-slack-blocks` 都注册 `slack`，同进程共存即启动失败，错误信息里带持有者包名。想同时用两个 Slack 实现，唯一办法是给其中一个换 `channel_type`（如 `slack-blocks`），于是它在设置页、在 `RegisteredChannels()`、在 inventory 里都是**第三个可见类型**，而不是一个隐藏在配置里的分支。宁可让运维看见第三种类型，也不要让"配置里写了 slack、跑起来的是别人"。

### 3.3 配置期单激活，与"多实例"的区别

| 概念 | 键 | 互斥规则 | 谁负责 |
|---|---|---|---|
| **实现单激活** | 渠道类型（`slack`） | `ErrDuplicate`，一个类型一个实现 | 内核 registry + 装配层 |
| **实例多开** | 渠道实例名（`ops-slack`、`ops-slack-backup`） | 不互斥；同一 factory 可构造任意多个 Sender | Router 的 `channels[name]` |

结论：**`ErrDuplicate` 卡住的是"同一类型的两个实现"，不是"同一类型的两个实例"。** `BuildSenderFromChannel` 逐行调 `BuildChannel(spec)` 构造 DB 渠道行的发送器，本来就是同一 factory 构造 N 次，迁移后完全不变。

装配层"只激活一个"的形状与 `aiopsdatasource.Backends` 一致：`Backends` 的字段为 nil 表示该后端未配置、不激活，`Activate` 跳过（`datasource.go:135-149`）。notifier 的 `Backends` 同形：`Slack == nil` 时 `opskeeper-notifier-slack` 不激活，于是装了 `opskeeper-notifier-slack-blocks` 的部署不会撞 `ErrDuplicate`，而默认部署（未安装任何候选包）行为不变。

**一处必须与 datasource 分道扬镳的地方**：装配顺序是 `ph.Load(ctx)`（第三方包）先、`notifier.Activate(...)`（编译内一方包）后，且 **`Activate` 遇 `ErrDuplicate` 不视为启动失败**，而是记录"类型 `slack` 已由 `opskeeper-notifier-slack-blocks` 提供"后跳过。这与 `aiopsdatasource.Activate` 出错即 `os.Exit(1)` 的处理相反，理由是：notifier 的类型竞争是**预期的运维选择结果**（配置选了增强变体），不是配置损坏；为一个正确的选择杀掉整个控制面是错的。这条判断的失败模式与 `NewRouterFromSpecs` 今天"一个渠道构建失败就跳过、不拖垮其余渠道"完全同源。

### 3.4 已知缺口 G-2：Router 实例名覆盖（本任务不改）

`NewRouter` 的 `r.channels[sender.Name()] = sender` 是 last-wins。两个 DB 渠道行同名时，先注册的被静默顶替，`FinishDelivery` 仍会给两个行都记投递结果，于是**两行都显示投递成功，实际只发了一次**。它今天就在发生，与本次迁移无关，迁移也不加重它（内核键是类型，不是实例名）。

处置：§13 S-6 建议改为"首个胜出 + `log.Warn` 命名冲突的两个渠道"，或"拒绝重名 + `log.Error`"。需要独立 change，因为它落在 `notify.go`。

## 4. 宿主 Policy 设计

### 4.1 为什么不能沿用 datasource 的 L1 假设

4.3 方案的三个候选全部是 `safety_level: L1` + `capabilities: [read]`，理由是"只读，没有成功状态可言"。这个推理对 VictoriaMetrics / Elasticsearch / OpenSearch 完全成立，对通知渠道**不成立**：

- 一次 `Send` 在 Slack workspace / 飞书群 / 钉钉群里**创建了一个对象**。它有成功状态（消息出现了）、有失败状态（限流 / 签名错 / 无权限）、有外部可见的后果（有人被吵醒）。
- 更强的证据：datasource 的写权限在 OpsKeeper 内部（保存查询、缓存结果），不改变外部世界；通知渠道的写权限**在别人的系统里**。
- `core/base/pkg/notify/doc.go` 把这个包定义为"transport adapters"，业务无关——业务无关不等于无副作用。副作用在**对端系统**里。

所以 §4.2 用仓库自己的词表给出结论，而不是给一个"看起来合理"的等级。

### 4.2 安全级别论证：L3 + `destructive`

| 依据（原文出处） | 结论 |
|---|---|
| `core/domain/safety.go:99-101`：L2 = "mutates **OpsKeeper state**, single approval"；L3 = "mutates **external systems**, scoped approval with a blast radius" | 通知渠道写的不是 OpsKeeper 状态（投递行由宿主 `StartDelivery` / `FinishDelivery` 写，插件碰不到），而是外部系统 → **L3** |
| `core/domain/safety.go:24-25`：`ClassWrite` = "mutates OpsKeeper-managed state"；`ClassDestructive` = "mutates state **outside OpsKeeper's control**" | 同理 → **`capabilities: [destructive]`** |
| `core/domain/safety.go:132-134`：L3 的 `MinimumClass()` 恰是 `ClassDestructive` | 声明 `[destructive]` 正好压在上限，不是"够到天花板"；`sdk.Validate` 的 `validateGovernance` 通过 |
| `core/domain/safety.go:138`：未识别的等级按 L3 排名 | 写错等级只会更严，不会更松——这意味着**低报没有好处**，如实写 L3 不承担额外风险 |

**L3 在本扩展点上真正买到的是强制力，不是仪式**：`Admit` 比较 `SafetyLevel.Rank() <= MaxSafetyLevel.Rank()`，所以一个从未决定要接外部副作用的控制面，其 notifier ceiling 低于 L3 时**装不上**任何通知包（包括今天这六个）。这也是本方案必须解决 per-kind ceiling 的原因（§4.5）。

### 4.3 BlastRadius：借词表，并说明借的是哪一段

`BlastRadius` 的五个取值是 k8s 语汇（`pod` / `single-ns` / `namespace` / `cluster`，`core/domain/safety.go:165-172`）。通知渠道没有 k8s 对象，所以这里**明确借用其语义位**："一次被批准的动作最远能触达多宽"。映射与取值：

| 渠道 | 一次发送的实际触达 | manifest 声明 | 理由 |
|---|---|---|---|
| webhook / slack / feishu / dingtalk / wecom / telegram | 一个群 / 一个会话 | 不写（`RadiusNone`） | 触达就是配置里那一个目标，无可再宽 |
| pagerduty | 一条升级策略可能覆盖整个 on-call 团队 | `single-ns` | 与宿主 ceiling 相等，不申请更宽 |

**宿主 ceiling 取 `single-ns`，与 `ManagerPolicy()` / `adapterPolicy()` 完全一致且不改**。这条设计的收益是 per-kind 只需要一个维度：安全级别。半径沿用全局，不引入第二张 per-kind 表。

为什么不留 `RadiusNone` 走"只要 `approval.required` 就够"：L3 下 `required: false` + `RadiusNone` 的组合在审阅时读起来是"这个包会改外部系统，但不声明任何触达上限"，而 PagerDuty 恰恰是唯一有理由被追问上限的那个渠道。写一个字面量比留空更可比较，且 `Admit` 会按它做真实比较（申请 `namespace` 会被拒，直到运维在 policy 里显式放宽）。

**关于 `approval.required: false` 的诚实说明**：`ApprovalPolicy.Required` 描述的是"agent 工具调用的审批往返"（`core/domain/plugin.go:114-117`），而 notifier **不声明 `spec.tools`**，那条路径对它不存在。写 `true` 等于声明一个不存在的控制。通知外发真正的授权在宿主侧，而且今天就有两处：

1. `notification-channels` 的增删改是 **admin-only**（`core/manager/server/alert/http.go:662` `requireAdmin`），并写 `channel_create` / `channel_update` 审计事件；
2. 某条告警走哪些渠道由告警规则的渠道绑定决定，同样在审计里。

即"谁能配置一条会往外发消息的通道"是受控的，"一次发送是否需要逐次审批"是刻意不做的——因为在一次正在发生的故障里，先审批再通知，等于没有人被通知。这两句必须同时出现在 PR 描述里，否则 `required: false` 会被读成"绕过了审批"。

### 4.4 GrantedScopes：ceiling 与 ask 分离

| 位置 | 集合 | 理由 |
|---|---|---|
| `NotifierPolicy().GrantedScopes`（**ceiling**，运维授的） | `alert.read` / `alert.write` / `observability.read` / `topology.read` | 前两个是通知的必要条件；后两个是给差异化卡片留的口子——卡片要画指标曲线、要标注设备归属时不必再动 policy |
| 本方案六个 manifest 的 `required_scopes`（**ask**，包要的） | `alert.read` / `alert.write` | 只问现在真用的。`ManagerPolicy()` 那份含 `db.read` 的清单是查询后端需要的，通知渠道一个存储都不读，抄过来就是 scope 膨胀 |

这一条直接对应 `core/domain/plugin.go:88` 的 `Satisfies`：ask ⊆ ceiling，缺一即拒；未在常量集里的 scope 按 fail-closed 处理。所以本方案**不新增任何 domain 常量**，六个 manifest 全部收敛到 `core/domain/plugin.go:60-73` 已有的四个常量以内。新 scope 的建议见 §12，标注为独立 change 决策点。

### 4.5 per-kind ceiling：候选比较与取舍

这是本方案唯一需要动内核的地方，因此单独做取舍记录。

**问题陈述**：装配层只有**一个** manager 内核宿主（`cmd/opskeeper/main.go` 注释 "ONE control-plane kernel host"，`pluginhost.New(dir, aiopsdatasource.ManagerPolicy(), log)`）。它当前对所有 Kind 一视同仁地按 `ManagerPolicy()` 的 `MaxSafetyLevel: L2` 准入。notifier 要 L3，于是"要么六个既有渠道也升到 L3 而宿主 ceiling 升到 L3，要么 notifier 只能停在 L2"。

| 候选 | 做法 | 收益 | 代价 / 失败模式 | 判定 |
|---|---|---|---|---|
| **A. per-kind ceiling** | `kernel.Policy` 增加 `KindCeilings map[Kind]SafetyLevel`，生效等级 = `min(MaxSafetyLevel, KindCeilings[k])`（**只能收紧，不能放宽**）；`NotifierPolicy()` = `ManagerPolicy()` 再 `.WithKindCeiling(KindNotifier, L3)` | tool / adapter / datasource 的 L2 ceiling 一字未动；六个 notifier 类型照常进**同一个 registry**，`Inventory()` 仍是"这个部署能触达什么"的唯一答案 | 改 `core/floor/kernel/host.go`（一个字段 + `Admit` 里三行比较），内核核心改动，需独立 change 与内核评审；未覆盖的 Kind 回落到全局 ceiling，语义是"没单独说过的一律按全局" | **采用** |
| B. notifier 单独一个 `kernel.Host` + `Registry` | 第二个注册表，`Resolve(reg)` 从它读 | 零内核改动 | `Inventory()` 分裂成两份；直接违反 main.go 里"一个注册表回答所有扩展点"这条既有不变量；跨包回滚要同时 deact 两处，漏一处就留下孤儿条目 | 拒绝 |
| C. 抬高 manager 全局 ceiling 到 L3 | 一行改动 | 最省事 | `tool` / `adapter` 一并获得 L3 准入。`core/floor/pluginmanifest/profiles.go` 里金融档 profile 明确把"L3 被拒绝"当作把 `opskeeper-sre-autonomy` 挡在外的**预期答案**；全局抬到 L3 等于替所有部署把这个决定做了 | 拒绝（若 A 被否决，这是降级选项，需用户显式决策） |
| D. notifier 一律降为 L2 / L1 | manifest 写 L2 | 零改动，ceiling 不动 | 与 `safety.go` 的定义直接相悖；一旦接 PagerDuty（会唤醒人、触发外部升级链）就无处安放，只能临时抬 ceiling——把决策推到最贵的时刻 | 拒绝 |
| **不做** | 维持现状 | 零成本 | `notifier` 继续是唯一"加渠道要改编译内代码"的扩展点，4.1 §6 的缺口行永远关不掉 | 拒绝 |

**A 的 fail-closed 性**（这是它能通过评审的关键）：per-kind 条目只能把 ceiling 调**低**。一个手滑写 `KindCeilings[KindNotifier] = L0` 的 policy 会让 notifier 包全部被拒，而不是被放行；`KindCeilings` 为 nil 时行为与今天**逐位相同**（现有三个 `Policy()` 构造点不受影响，向后兼容）。与 `Policy` 现有的零值拒绝语义同族：`MaxSafetyLevel == ""` 时 `Admit` 拒绝一切。

**A 的实际影响面**（已实测，见 §11.1）：把六个草案按 `sdk.Decode → sdk.Validate → sdk.Admit` 跑一遍，结果是——在 `MaxSafetyLevel: L2` 下**全部被拒**（`plugin is L3, target policy allows at most L2`），在 `L3` ceiling 下**全部通过**。这说明 per-kind ceiling 不是装饰，是唯一能让这套 manifest 落地的东西。

### 4.6 `NotifierPolicy()` 草案

对齐 `datasource.ManagerPolicy()` / `toolset.adapterPolicy()` 的形状（`Target` / `MaxSafetyLevel` / `MaxBlastRadius` / `GrantedScopes`），差别只有两处，都写在下面：

```go
// NotifierPolicy is the admission ceiling for a control-plane host that
// hosts notification channels.
//
// It is ManagerPolicy with one addition and one change, and both are
// forced by what a notification channel is.
//
// The addition is the per-extension-point ceiling: a channel is L3 because
// it writes to somebody else's system, and a datasource is L1 because it
// only reads. One host runs both, so one flat ceiling cannot be correct
// for both — raising it for the notifier would hand L3 admission to every
// manager-targeted tool package, which is the decision the deployment
// profiles exist to keep out of package authors' hands. The per-kind entry
// can only narrow, never widen, so a policy that forgets it behaves
// exactly as it does today.
//
// The change is alert.write in the granted scopes. A notification's whole
// job is to act on an alert, and a ceiling that does not grant that scope
// cannot admit a channel at all. observability.read and topology.read are
// granted alongside it so a richer card can ask for them in a later
// version without a policy change; a manifest that asks today still does
// not get them, because the manifest's list is the ask and the policy's
// is the ceiling.
//
// The blast radius is unchanged from ManagerPolicy: single-ns. Six chat
// channels reach one group each, and PagerDuty asks for exactly this much
// and no more.
func NotifierPolicy() kernel.Policy {
	return kernel.Policy{
		Target:         domain.TargetManager,
		MaxSafetyLevel: domain.SafetyL2,       // 全局不变
		MaxBlastRadius: domain.RadiusSingleNS, // 与 ManagerPolicy 一致
		GrantedScopes: domain.Scopes{
			domain.ScopeAlertRO,
			domain.ScopeAlertWrite, // ← 相对 ManagerPolicy 的新增
			domain.ScopeMetricsRO,
			domain.ScopeTopologyRO,
		},
	}.WithKindCeiling(kernel.KindNotifier, domain.SafetyL3) // ← 相对 ManagerPolicy 的新增
}
```

装配层调用点（一行，不新增分支）：

```go
// 现状
ph := pluginhost.New(managerPluginsDir, aiopsdatasource.ManagerPolicy(), pluginLog)
// 之后
ph := pluginhost.New(managerPluginsDir, notifier.NotifierPolicy(), pluginLog)
// datasource.ManagerPolicy() 本身一字未改，toolset.adapterPolicy() 亦然
```

`aiopsdatasource.ManagerPolicy()` 保持原样返回，由 notifier 包在其上派生；这样 datasource 方案里"不改 `datasource.go` 主流程"的约束继续成立。

**部署 profile 与本 ceiling 的关系**（避免误读）：`pluginmanifest.ProfileForName` 目前只服务 edge 宿主（`cmd/opskeeper-edge/agentconfig.go:97`、`core/edge/pluginhost/host.go:252`），manager 目标包**不经**部署 profile 闸门。因此 notifier 包的准入完全由 `kernel.Policy` 决定；边缘档位在本扩展点上不适用，这一点要在评审时说清，否则会有人以为"金融档会挡掉通知包"。

**`install.strategy` 在本路径上的地位**：manager 侧 `pluginhost.Load` 不消费 `strategy`（只有 `core/floor/pluginmanifest/catalog.go:148` 把它读出来展示，节点侧灰度编排在 `core/domains/service/plugin/rollout.go` 面向节点队列）。因此 notifier 六个包一律 `pin`：这是**治理声明**而非生效开关，但它准确表达了"通知外发没有 1/10 灰度的语义"，也扛得住将来真出现 rollout 引擎的那天。若评审认为"写一个不生效的开关"不可接受，替代方案是照 datasources 写 `rolling`，代价是把一个错误的灰度语义写进六个 manifest——不推荐。

## 5. 既有六渠道的兼容迁移路径

### 5.1 阶段划分

迁移必须落在三个可独立回滚的切片上，因为 `registry.go` 在本任务的非目标清单里，而内核改动必须独立评审。

| 切片 | 内容 | 是否碰 `registry.go` | 是否碰内核 | 回滚 |
|---|---|---|---|---|
| **B1** | 新增 `core/base/pkg/notify/kernel.go`：`Plugin*` 包名常量、`channelPlugin` 贡献体、`manifestFor()`、`Backends`、`Activate()`、`Resolve()`、`Set`。六个 factory 从 `registry.go` 的 map 字面量**原样搬进** `Backends` 装配，函数体逐字不动 | ✗（只读） | ✗ | 删掉 `kernel.go` 与装配调用。`registry.go` 的 map 仍是唯一生效路径，行为 100% 不变 |
| **B2** | 装配层在 `ph.Load(ctx)` **之后**调 `notifier.Activate(ph.Kernel(), backends)`；`ErrDuplicate` 记 Info 后跳过。`notify` 内加 `Set` 视图，`BuildChannel` 优先读内核、未命中回落旧 map（回落路径打一条 Warn） | ✗ | ✗（`BuildChannel` 的改写在 B3） | 关掉装配调用即可回到纯旧路径 |
| **B3** | `BuildChannel` 只读内核，删除遗留 map 与回落；`RegisterChannel` 要么删除、要么改为包装 `Activate` | ✓ | ✓（`WithKindCeiling` 必须先合，否则 B3 的 L3 manifest 会被拒） | git revert；此片之后 `notify` 里不再存在第二个注册表 |

B1 + B2 合入期间，**默认部署**（`OPSKEEPER_MANAGER_PLUGINS_DIR` 为空或不存在）的行为由 B2 的回落路径保证不变；B3 是把回落删掉的那一刀。

B1/B2 需要 `WithKindCeiling` 吗？需要——两个切片都要让六个 L3 manifest 通过 `Admit`。因此 **`WithKindCeiling` 是 B1 的前置**，建议单独一个 PR 先行（改动面：`core/floor/kernel/host.go`；三个既有 `Policy()` 构造点零改动；加一个回归测试）。

### 5.2 默认部署行为不变：六条论证

| # | 不变量 | 论证 |
|---|---|---|
| 1 | **配置面不变** | `specsFromConfig` 读的仍是 `config.NotificationConfig` 的四个子结构（webhook / slack / feishu / dingtalk），产出仍是 `[]ChannelSpec`。DB 渠道行的 `channel_type` 取值仍是那六个字符串，`validateChannelInput` 不校验白名单，存量行不需要迁移 |
| 2 | **工厂实现不变** | 六个 factory 的函数体**逐字搬迁**，仍是 `newWebhookSender(name, url, secret, client, buildBody, signTarget)` 的六种组合。payload、签名、header 一处不改——签名算法在 §10 明确列为"候选包的新增，不原地改"，正是因为原地改会破坏接收方 |
| 3 | **解析错误语义不变** | `BuildChannel` 今天对未知类型报 `unknown channel type %q`、对空名 sender 报 `built an unnamed sender`。迁移后两条消息一字不改，只是未知类型的来源从 map 查不到变成 registry 查不到 |
| 4 | **部分失败不拖垮整体** | `NewRouterFromSpecs` 的"跳过并 Warn"语义、Router 逐渠道独立 timeout、`Router.Send` 的 `errors.Join` 聚合，全部不动 |
| 5 | **投递留痕不动** | `notification_deliveries` 的写入方是宿主（`StartDelivery` / `FinishDelivery`），`RetryWorker` 的重试与退避不碰。插件的 `audit.emits: true` 是声明，账本条目仍由宿主从自己的 gate 事件派生（`core/domain/plugin.go:95-103`），插件既不能伪造也不能抑制 |
| 6 | **回归证据不需改断言** | `core/base/pkg/notify/notify_test.go`、`registry_test.go`、`tests/e2e/notify_slack_test.go`（G3）、`tests/e2e/notify_signed_test.go`（G4）在 B1/B2 后**不需要修改任何断言**即可通过；新增 `TestTheSixBuiltinChannelsResolveIdenticallyThroughTheKernel`（同一 `ChannelSpec` 分别走旧表与内核，断言 `Sender.Name()`、HTTP 方法、请求体字节、签名头全等）作为等价性的活锁 |

补充一条**不会**发生的事：`ManagerPolicy()` 不改、`datasource.go` 不改、告警派发主流程不改、告警规则与 `channel_type` 的取值不新增。

### 5.3 代码骨架建议（不在本任务合入）

```
core/base/pkg/notify/
├── registry.go        （只读引用；B3 才改）
├── kernel.go          （B1 新增）
│   ├── PluginWebhook / PluginSlack / PluginFeishu / PluginDingTalk /
│   │   PluginWeCom / PluginTelegram   —— 包名，与 channel type 分开
│   ├── channelPlugin{plugin, typ string, f ChannelFactory}
│   ├── manifestFor(plugin string) pluginmanifest.Plugin   —— L3 / destructive
│   ├── Backends{Webhook, Slack, Feishu, DingTalk, WeCom, Telegram ChannelFactory}
│   ├── Activate(host *kernel.Host, b Backends) (Set, error)
│   ├── Resolve(reg *kernel.Registry) Set
│   └── Set{ map[string]ChannelFactory } + Set.Factory(typ) (ChannelFactory, bool)
└── webhook.go         （不改；差异化 payload 在候选包里）

core/manager/middleware/notifier/          （或 core/manager/biz/notify/，与 datasource 同位）
├── policy.go           —— NotifierPolicy()
└── host.go             —— Backends 从 config 装配、Activate 调用点

cmd/opskeeper/main.go
├── +1 行：pluginhost.New(dir, notifier.NotifierPolicy(), log)
└── +1 处：ph.Load(ctx) 之后 notifier.Activate(ph.Kernel(), backends)
```

**禁项**（与 4.3 §6.3 同款，写在这里是为了让评审能直接判）：

- 不在 `webhook.go` 里给某个渠道加 `if` 分支。差异化能力一律是新包。
- 不把候选包的客户端塞进 `core/base/pkg/notify`（那是宿主传输层，第三方实现放进去等于把 vendor 代码进主仓）。
- 不在 `registry.go` 里新增"自动回落到某个目录扫描"的旁路。
- 不为 notifier 新增第七个 Kind，不改 `kind.go`。

## 6. 候选 1：PagerDuty（新增第三方渠道）

### 6.1 选型理由与差异化能力

| 维度 | 六个既有渠道 | PagerDuty |
|---|---|---|
| 语义 | 在群里贴一条消息 | 在外部事故系统里开一个 incident，并可能沿升级策略呼叫真人 |
| 聚合 | 无（每次一条） | `dedup_key` 聚合同一事故的多次事件 |
| 交付 | 尽力一次 | 有明确的送达语义与升级路径 |
| 凭证 | webhook URL 本身 | `routing_key`（Events API v2） |

**最值得做的一点**：`notify.Message.DedupeKey` 已经存在（`core/base/pkg/notify/notify.go:29`），而 OpsKeeper 在派发前已经算好了 `pipeline:rule:label-set` 的去重键。PagerDuty 的 `dedup_key` 语义与之**逐字对应**——"这些事件是同一个事故"。直接透传的结果是：一条每小时触发 40 次的规则产生 1 个 PagerDuty incident（挂 40 个事件），而不是 40 个 incident 和 40 次呼叫。这是本仓降噪能力在通知侧唯一没有出口的地方。

- 协议：`POST https://events.pagerduty.com/v2/enqueue`
  `{routing_key, event_action: "trigger", dedup_key, payload: {summary, severity, source, component, group, class, timestamp, links: [...]}}`
- 严重度映射：`SeverityCritical→critical`、`SeverityWarning→error`、`SeverityInfo→info`。注意 PD 的 `warning` 档位留给"有影响但不紧急"，OpsKeeper 的 `warning` 更接近 PD 的 `error`；这是一处**有意的错位映射**，在包注释里写明，避免后来者按字面对齐把它改回 `warning`（那会让一批规则降级呼叫）。
- 响应体必须解析：`{"status":"success","message":"Event processed","dedup_key":"..."}`。未知 `routing_key` 返回 **HTTP 400** + `errors[]`，所以 §2.3 的缺口 G-1 在这个渠道上是硬约束，不是优化。
- `event_action`：`trigger` 为 v1。`acknowledge` / `resolve` 需要宿主暴露 incident 状态迁移，属 5.x HITL 闭环范围，**不在 v1**，理由写进包注释，避免有人以为漏了。

### 6.2 manifest 草案

详见 `notifier-manifests/pagerduty.yaml`。要点：包名 `opskeeper-notifier-pagerduty`（0.1.0）/ `vendor: pagerduty` / `targets: [manager]` / `safety_level: L3` / `capabilities: [destructive]` / `required_scopes: [alert.read, alert.write]` / `approval.max_blast_radius: single-ns` / `install.strategy: pin`。

**为什么候选包不落本仓 `plugins/`**：本仓 `plugins/` 是两个不同的发布面——`plugins/pig-ops/*/pig-ops.yaml`（PiG 垂域包，跑在节点上）与 `plugins/opskeeper-teamharness/plugin.yaml`（AgentTeams 包，schema 都不同）。notifier 是 manager 目标包，其分发路径是 `OPSKEEPER_MANAGER_PLUGINS_DIR`（`cmd/opskeeper/main.go` 读取），由各 vendor 各自打包投放。把一个 `vendor: pagerduty` 的包放进本仓开源目录，等于给第三方实现背书并把它纳入本仓的开源审计与发布节奏——这是治理问题，不是打包问题。

### 6.3 注册路径

1. 包在 `OPSKEEPER_MANAGER_PLUGINS_DIR/opskeeper-notifier-pagerduty/` 下带 `pig-ops.yaml`。
2. 装配层 `ph.RegisterActivator("opskeeper-notifier-pagerduty", func() kernel.Contribution { return PagerDuty{F: ...} })`（`core/manager/middleware/pluginhost/host.go:125` 的既有机制，与 manager 侧第三方包同一条路）。
3. `ph.Load(ctx)` 读目录 → `Admit`（`KindNotifier` ceiling L3 + `alert.*` scope）→ `Activate` → `r.Register(KindNotifier, "pagerduty", factory)`。
4. 运维在**设置 → 通知渠道**建一行 `channel_type=pagerduty`、`endpoint=https://events.pagerduty.com/v2/enqueue`、`secret=<routing_key>`（`BuildSenderFromChannel` 已把 `secret` 同时喂给 `Secret` 与 `ChatID`，本包只读 `Secret`、忽略 `ChatID`，无需改派发侧）。
5. 告警规则绑定该渠道 → `MaybeNotify` → `BuildSenderFromChannel` → `BuildChannel` 命中内核 `pagerduty` → 发送。

配置面的 `channel_type` 枚举、`Channels.tsx` / `Notifications.tsx` 的下拉、model 常量都**需要新增 `pagerduty` 字面量**——这是实现切片 S-5 的内容，本任务不写。

### 6.4 测试方案

| 层 | 用例 | 通过条件 |
|---|---|---|
| Unit | `Message` → Events API body 映射（含 severity 错位表、`dedup_key` 透传、`payload.timestamp` 取 `OccurredAt` 而非 `now`） | 字段级快照比对 |
| Unit | 同一 `dedup_key` 连续 3 次 trigger | body 三次完全相同（不含随机量、不含 `time.Now()`） |
| Unit | 响应体 + HTTP 状态双重判定 | 400 + `errors[]` 返回错误且错误文本含 `routing_key`；只有 `status: "success"` 才算成功 |
| Integration | 本地 fake `httptest` 端点跑完整 enqueue 契约（fixture 放 `core/harness/fixtures/notifier/pagerduty/`） | 捕获的 POST 与契约逐字段一致 |
| E2E（手工，可选） | 填真实 `routing_key` 打一次 Events API v2 | 目标服务里出现一条 incident，`dedup_key` 与告警一致 |
| Contract | 与 webhook 渠道在同一 `Message` 上的行为差异文档化 | 差异写进包 README，不留空白 |
| Failure | 超时 / 429 / 403 / 400 / DNS 失败 | 错误可分类、不吞错；429 能被重试路径判定 |

### 6.5 回滚方案

回滚因果链（精确版，与 4.3 §3.7 同一写法）：

1. **先摘路由**：把绑定该渠道的告警规则解绑，或对相关 incident 设 silence。**这一步不能省**——PagerDuty 通常承载 critical，卸载后这些告警会一路走到"找不到工厂"为止。
2. 从 `OPSKEEPER_MANAGER_PLUGINS_DIR` 移走（或禁用）`opskeeper-notifier-pagerduty`。
3. 重启 Manager：`ph.Load` 不再激活它，内核里 `notifier/pagerduty` 条目消失。
4. 此时 `type=pagerduty` 的渠道行仍在库里，`BuildChannel` 未命中 → 报 `unknown channel type "pagerduty"` → `MaybeNotify` 把 `sendErr` 写进 `notification_deliveries.failed` 并 Warn，`RetryWorker` 重试至上限（默认 5 次）后停止。**这是可见失败，不是静默丢失**：告警本身仍记录在 `alert_events`，操作员在告警事件页能看到投递失败行。
5. 回滚后的验证：`ph.Kernel().Inventory()` 的 `kind_counts` 不再含该条目；其余六个渠道的条目、库存与投递行为逐条比对无变化；`make pi-plugin-guard && make module-check && go test ./scripts/ledgercheck` 全过。

**回滚不变量**：卸载 PagerDuty 包不触碰其余六个渠道；不改 `registry.go`；不改告警派发主流程。若第 1 步没做，回滚**不会**造成告警数据丢失（投递失败是可见的），但会造成 critical 告警在故障期间无人收到——这是操作风险，不是数据风险。

## 7. 候选 2：飞书交互式卡片

- **现状**：`NewFeishuSender` 发 `msg_type: text`，签名（`timestamp` + `sign`，HMAC-SHA256 base64 over `"<unix>\n<secret>"`）已在 body 顶层。
- **差异化**：`msg_type: interactive`，`card.config.wide_screen_mode: true`；`header.template` 按 severity 取 red / orange / blue / grey（与 `slackColor` 同一思路，把色带 pin 住而不是交给客户端默认值）；`elements` 分 `div(lark_md)` / `hr` / `note` 三段；`at` 段支持 `<at id=all></at>` 与按 label 指定 user_id。
- **签名不变**：加签逻辑逐字复用 `signFeishu`，带 secret 的机器人无需重新配置。
- **明确不做（v1）**：
  - `tenant_access_token`（app_id + app_secret）鉴权模式——需要 `core/domains/biz/secret` 增加一种凭据类型，属独立 change；
  - 卡片按钮回调（审批 / 认领）——需要**入站** HTTP 端点与签名校验，与 5.5 的审批桥耦合，而审批桥默认关闭；在这里做等于用一个通知包绕过审批设计。
- **链接**：卡片想带 runbook / incident 深链，只能从 `Message.Labels` 取（如 `labels["runbook_url"]`）。`Sender` 拿不到 `model.Incident.RunbookURL`（那是派发侧的字段，填充它需要改 `usecase.go`，在本任务非目标里）。→ v1 卡片不带链接；带链接作为 S-3 的依赖项单列。
- **manifest**：`notifier-manifests/feishu-card.yaml`，`opskeeper-notifier-feishu-card`，注册类型 `feishu`（与内置 `feishu` 二选一）。
- **注册路径**：同 §6.3，type 换成 `feishu`。内置 `feishu` 是否激活由配置决定（`Backends.Feishu == nil` 时不激活），从而避免 `ErrDuplicate`。
- **测试**：unit 覆盖 card JSON 结构与 severity→template 映射；integration 打到假端点断言 `msg_type == "interactive"` 且签名与现状一致（回归断言不能因为换 payload 而放松）；E2E 在 `notify_signed_test.go` 里加一个子用例，与 G4 现有断言并存。
- **回滚**：配置切回内置 text 变体 → 重启 → `notifier/feishu` 条目由 `opskeeper-notifier-feishu` 重新持有。已发出去的卡片不受影响（外部系统的历史消息不可回滚，这一点对所有候选包都成立，回滚文档统一按"对外副作用不可撤销、内部分发可撤销"写）。

## 8. 候选 3：Slack Block Kit

- **现状**：`formatSlack` 发旧式 `attachments`，带 severity 色带（`#d92f2f` / `#f2c037` / `#36a64f` / `#6f7a87`）与字段数组；顶层 `text` 已填充作 fallback；`mrkdwn_in: ["text"]`。
- **差异化**：`blocks` 数组——`header`（severity 色带落在这里）、`section` + `fields`（最多 10 个字段，超出折叠进 `context`）、`divider`、`context`（时间戳 + 来源标识）。**顶层 `text` 必须保留**：Slack 的推送预览、移动端通知、邮件摘要都读它，只填 `blocks` 的实现在手机上看起来就是"空的"。
- **明确不做（v1）**：`thread_ts` 线程聚合。它需要 Slack Web API（`chat.postMessage` + bot token），是**另一种凭证模型**（token 而非 incoming webhook URL），而 `ChannelSpec` 今天的 `Secret` 语义是"签名密钥"。混用会让 `BuildSenderFromChannel` 的同一列承载两种东西。→ 作为 S-7（需扩 `ChannelSpec`）单列。
- **manifest**：`notifier-manifests/slack-blocks.yaml`，`opskeeper-notifier-slack-blocks`，type `slack`，与内置二选一。
- **测试**：unit 覆盖 blocks 结构 + 字段数上限 + 顶层 `text` 非空断言；integration 假端点断言 `blocks` 存在且 `attachments` 不存在；contract 与内置版的 severity 映射表逐条比对（色带 hex 必须一致，否则一次升级会让值班同学重新适应配色）。
- **回滚**：配置切回内置 `attachments` 变体 → 重启。

## 9. 候选 4：钉钉 markdown + 响应体错误码

- **现状**：`NewDingTalkSender` 发 `msgtype: text`；签名走 URL query（`signDingTalkURL`，毫秒时间戳）。
- **差异化**：
  1. `msgtype: markdown` + `markdown: {title, text}`；`at: {atMobiles, atUserIds, isAtAll}`，让规则能直接 @ 到人而不是往群里丢。
  2. **响应体错误码判定**（本候选最重要的产出）：`{"errcode":0,"errmsg":"ok"}`。`310000` 签名错误、`130101` 消息发送频率超限、`300001` 无权限、`400013` 群不存在——四种今天全部被记成**投递成功**（缺口 G-1）。本包把非零 `errcode` 判为失败，于是它们进入 `notification_deliveries.failed` 并被 `RetryWorker` 重试。
- **对运维的直接价值**：一个配错签名的钉钉机器人，今天的表现是"消息再也不来了，但系统说一切正常"；改完之后的表现是"投递失败，重试 5 次，告警事件页有记录"。这是把静默故障变成可见故障。
- **manifest**：`notifier-manifests/dingtalk-markdown.yaml`，`opskeeper-notifier-dingtalk-markdown`，type `dingtalk`，与内置二选一。
- **测试**：
  - unit：`errcode` 非零 → 错误；`errmsg` 进错误文本（运维要靠它定位）；
  - integration：假端点分别回 `{"errcode":0}`、`{"errcode":310000,...}`、`{"errcode":130101,...}`，断言投递结果与 `notification_deliveries` 状态；
  - E2E：复用 `tests/e2e/testenv` 的 `FakeSlack` httptest server（现有 G4 已经在用它接钉钉），加一个"返回非零 errcode → 渠道测试接口报错"的子用例；
  - 签名回归：现有 G4 的签名断言**一字不改**继续通过（证明换 msgtype 没换签名）。
- **回滚**：配置切回内置 text 变体 → 重启。注意：本候选**降低**静默失败率，回滚等于把静默失败率调回去，这个方向的回滚要在变更单里写明（不是"回到更好的状态"）。

## 10. 候选 5：Webhook 强化签名与响应校验

- **现状**：`signGenericWebhook` 对 body 做 HMAC-SHA256，输出 `X-OpsKeeper-Signature: sha256=<hex>`；无时间戳、无重放窗口。
- **差异化**：
  1. 签名串改为 `"<unix-seconds>.<body>"`，并把时间戳重复到 `X-OpsKeeper-Timestamp`，让接收方能在窗口内拒重放（现状一份被截获的请求可以无限重放）；
  2. 响应体若形如 `{"code":..,"message":..}` 则解析，非零 code 判为失败（与 §9 同一条推理）；
  3. 连接 / 读取超时显式设定，不再继承 `http.DefaultClient`——今天一个慢接收方能占住派发循环直到 Router 的整体 timeout。
- **为什么是独立包而不是原地改**：**签名算法变了。** 原地改会让所有按现状算法验签的接收方在升级瞬间全部失败，而"默认部署行为不变"是本方案的第一约束。所以它与内置 `webhook` 二选一，靠配置切换。
- **manifest**：`notifier-manifests/webhook-strict.yaml`，`opskeeper-notifier-webhook-strict`，type `webhook`，与内置二选一。
- **测试**：unit 覆盖签名串拼接与 hex 形态（对外仍用 hex，与现状一致，降低接收方改动面）；integration 断言时间戳头与签名可被独立复算；failure 覆盖慢响应方不再占住派发循环（用一个 sleep 超过新超时的假端点）。
- **回滚**：配置切回内置 `webhook` → 重启 → 接收方代码无需回滚（旧算法仍在另一包里）。这是二选一设计相对原地改的核心收益，写在回滚文档第一句。

## 11. 通用闸门（每个候选都必须通过）

### 11.1 manifest 闸门（已实测）

按 `tasks.md` 4.10 / 4.11，对 `notifier-manifests/*.yaml` 逐个跑 `sdk.Decode → sdk.Validate → sdk.Admit`：

| 文件 | name | level | capabilities | required_scopes | radius | strategy | L2 ceiling | L3 ceiling |
|---|---|---|---|---|---|---|---|---|
| `builtin-channels.yaml` | `opskeeper-notifier-slack` | L3 | `[destructive]` | `alert.read, alert.write` | — | pin | 拒 | **过** |
| `pagerduty.yaml` | `opskeeper-notifier-pagerduty` | L3 | `[destructive]` | `alert.read, alert.write` | `single-ns` | pin | 拒 | **过** |
| `feishu-card.yaml` | `opskeeper-notifier-feishu-card` | L3 | `[destructive]` | `alert.read, alert.write` | — | pin | 拒 | **过** |
| `slack-blocks.yaml` | `opskeeper-notifier-slack-blocks` | L3 | `[destructive]` | `alert.read, alert.write` | — | pin | 拒 | **过** |
| `dingtalk-markdown.yaml` | `opskeeper-notifier-dingtalk-markdown` | L3 | `[destructive]` | `alert.read, alert.write` | — | pin | 拒 | **过** |
| `webhook-strict.yaml` | `opskeeper-notifier-webhook-strict` | L3 | `[destructive]` | `alert.read, alert.write` | — | pin | 拒 | **过** |

"L2 ceiling 拒"的错误文本是 `spec.safety_level: plugin is L3, target policy allows at most L2`。**这一列是 §4.5 候选 A 的实测依据**：per-kind ceiling 不是把 L3 写进 manifest 就能绕过的装饰，它决定了同一份 manifest 在两种 policy 下有两种命运。

（复核方式：把 `notifier-manifests/` 作为目录交给 `sdk.Decode` + `sdk.Validate` + `sdk.Admit`，`Admission` 取 §4.6 的字段。这是本任务期间一次性跑过的验证，未在仓内留下测试代码——S-1 建议把它固化成 `notifier-manifests_test.go`。）

### 11.2 其余闸门

| 闸门 | 内容 |
|---|---|
| **精确版本** | `metadata.version` 用 semver 锁（`0.1.0` / `1.0.0`），不得 `latest` / `main` |
| **scope 最小化** | 六个 manifest 都只要 `alert.read` + `alert.write`；多写一个就应在评审被打回 |
| **安全级别** | notifier 一律 L3；写 L1/L2 即为**误述**，评审应直接拒（理由见 §4.2） |
| **兼容范围** | 第三方包写 `min_pig_version` / `min_edge_version`；合成的一方 manifest 不写（一个跟二进制同构建的包不需要兼容下限，见 `notifier-manifests/builtin-channels.yaml` 末尾的说明） |
| **安装策略** | 一律 `pin`，理由见 §4.6 |
| **测试** | unit + integration + contract 三层；integration 在 CI 用假端点跑真 HTTP 契约 |
| **签名** | 经 `core/floor/pluginmanifest/signing.go` 通道，签名文件随包分发；`Profile.Policy()` 已把 `AllowUnsigned` 置 false |
| **回滚文档** | 含"卸载前 / 卸载后 / 不变量"三段，且必须写出**对外副作用不可撤销**这一事实 |
| **灰度** | notifier 包**不做 canary**（见 §4.6），灰度靠"先装包不切配置"，即渠道类型二选一 |
| **闸门命令** | `make pi-plugin-guard && make module-check && go test ./scripts/ledgercheck` |

## 12. scope 新增建议（独立 change 决策点，本方案不落）

本方案的六个 manifest **全部**收敛在 `core/domain/plugin.go:60-73` 已有的常量内，无新增。以下两项是**评审时可能被问到**的缺口，列出并标注为独立 change 决策点：

| 缺口 | 触发场景 | 建议的 domain 常量新增 | 位置 | 影响面 |
|---|---|---|---|---|
| **非告警通知** | 第三方 notifier 要发日报、任务结果、演练通告、审批结果回执——都不是"告警" | `ScopeNotifySend Scope = "notify.send"`（与 `alert.*` 平级，语义是"向外部系统投递一条消息"） | `core/domain/plugin.go` 常量块，`ScopeAlertWrite` 之后 | 需同时改：① domain 常量；② `NotifierPolicy().GrantedScopes` 加这一项；③ 现有六个 manifest **不必**改（它们不申请）；④ `pluginmanifest` 的 profile 目录里金融档 `NotGranted` 需显式点名，否则该 scope 在金融档上落空 |
| **只读收窄** | 运维只想让某个部署"能收不能发"（例如 staging 接生产告警但绝不外发） | 不新增常量；改为在 profile / policy 层用**不授予** `alert.write` 来表达 | `core/floor/pluginmanifest/profiles.go` | 只加一个 profile 条目，不动 domain 词表。**优先走这条**，因为新增 scope 是 ABI 变更（`core/domain/plugin.go` 文件头明确写了常量属于插件 ABI） |

**为什么把"不新增"排在前面**：本方案六个候选只需要 `alert.read` + `alert.write`，而这两个已经在 `profiles.go` 里被金融档以"repair 包需要它"的理由授予（`profiles.go` 中金融档 profile 的 `Intent` 字段）。为一个尚不存在的用例扩 ABI，比等用例真的来了再扩要便宜。

## 13. 实现切片建议（不在本任务合入）

| 切片 | 内容 | 触碰文件 | 前置 | 验收 |
|---|---|---|---|---|
| **S-1** | `WithKindCeiling`（§4.5 候选 A）+ manifest 准入回归测试（把 §11.1 固化成测试） | `core/floor/kernel/host.go`、新测试 | — | 既有三个 `Policy()` 构造点行为逐位不变；per-kind 只能收窄（有测试断言"写宽了被拒"） |
| **S-2** | B1：`notify/kernel.go` + 六方 factory 搬迁 + 等价性测试 | `core/base/pkg/notify/kernel.go`（新） | S-1 | `notify_test.go` / `registry_test.go` / 两个 e2e 断言零修改通过；`make module-check` 通过 |
| **S-3** | B2：装配层 Activate + `ErrDuplicate` 跳过语义 | `cmd/opskeeper/main.go`、notifier 宿主包 | S-2 | 装 `opskeeper-notifier-slack-blocks` 且配置仍选内置时，启动日志出现"类型 slack 已由 …提供"，进程正常；默认部署启动日志与改动前逐行相同 |
| **S-4** | B3：删除遗留 map 与回落；`RegisterChannel` 移除或改包装；顺带给内置 `webhookSender` 加响应体校验钩子（缺口 G-1 的存量修复） | `core/base/pkg/notify/registry.go`、`webhook.go` | S-3、评审通过 | `grep -rn "channelFactories"` 在 `notify` 包内只剩内核路径；存量 e2e 全过；钉钉签名错在存量路径上也落 failed |
| **S-5** | 渠道类型枚举打通：`model.ChannelTypePagerDuty`、service 校验、web 前端下拉与 i18n、集成配置页 | `core/manager/model/alert/model.go`、`core/manager/service/alert/service.go`、`web/src/pages/settings/{Channels,Notifications}.tsx` | S-3 | 建 `pagerduty` 渠道行 → `POST /test` 打到假端点；未知类型仍被 `BuildChannel` 拒 |
| **S-6** | 缺口 G-2：Router 实例名重名处理（首个胜出 + Warn） | `core/base/pkg/notify/notify.go` | 独立 change | 两个同名渠道行的用例：只发一次，且日志点名冲突的两个渠道 |
| **S-7** | §8 的 `thread_ts`：Slack Web API token 与 incoming webhook URL 两种凭证需要扩 `ChannelSpec` | `notify.ChannelSpec`、`webhook.go` | 独立 change | 凭证模型显式分离，不复用 `Secret` |
| **S-8** | 候选包首切片（建议顺序：dingtalk-markdown → slack-blocks → feishu-card → pagerduty → webhook-strict） | `OPSKEEPER_MANAGER_PLUGINS_DIR` 外部目录 + 装配层 activator | S-3、S-5 | 每包独立 PR、独立版本、独立回滚 |

**顺序理由**：先做 dingtalk-markdown，因为它同时验证"二选一互斥"与"响应体判定"两件事，而这两件是其余四个包的前提；pagerduty 排最后，因为它需要 S-5 的渠道类型打通。

## 14. 与 4.1 / 4.2 一致性自查表

| 自查项 | 4.1 / 4.2 原文 | 本方案是否一致 | 证据 |
|---|---|---|---|
| notifier 归"迁移为插件，未接内核" | 4.1 §3 状态列"未接内核"；4.2 四分类 | ✓ 本方案只推进"迁移"，不触碰保留 / 暂缓 / 淘汰三类 | §1.2 / §3 / §5 |
| 收口 4.1 §6 缺口行 | "`notifier` 未接内核，通知渠道只能编译内增加 → 事件、企业集成 → 4.5" | ✓ §3 + §5 给出注册路径，§6–§10 给出第三方候选 | §3.2 / §5.1 |
| 内核六 Kind 封闭 | `core/floor/kernel/kind.go` 六元封闭集合 | ✓ 复用 `KindNotifier`，不新增字符串；`kind.go` 不在触碰清单 | §1.3 / §5.3 禁项 |
| `Activate` 是注册唯一入口 | 4.3 §2.2 的关键不变量 | ✓ 六渠道与第三方包一律经 `Host.Activate`，无并行注册表、无快速通道；B3 之后 `notify` 内不再有第二个注册表 | §3.2 / §5.1 / §5.3 |
| 同信号 / 同名互斥，`ErrDuplicate` fail-closed | `core/floor/kernel/registry.go:68` | ✓ 类型键即互斥键，无覆盖语义；并给出"想共存就换 channel_type"的显式出路 | §3.2 / §3.3 |
| 配置期单激活 | 4.3 §3.3 的一方 / 第三方二选一 | ✓ `Backends` 的 nil 字段即"未配置不激活"，与 datasource 同形；差异处（`ErrDuplicate` 不致命）单列并说明理由 | §3.3 |
| `required_scopes` ⊆ 既有 domain 常量集 | `core/domain/plugin.go:60-73`；4.3 §2.3 | ✓ 六个 manifest 全部只用 `alert.read` / `alert.write`；`db.read` 等不抄 | §4.4 / §11.1 |
| 需要新 scope 时给 domain 常量建议 + 标独立 change | 任务验收项 | ✓ §12 列出两项建议、给出位置与影响面，并优先推荐"不新增"路线 | §12 |
| 不套用 datasource 的 L1 / 只读假设 | 任务明确要求 | ✓ §4.1 论证为什么推理不成立；§4.2 用 `safety.go` 原文给出 L3 + destructive 的依据 | §4.1 / §4.2 |
| 一方后端不重写 | 4.3 §1.2 / §6.3 | ✓ `datasource.go` 与 `ManagerPolicy()` 一字未改；notifier 在其上派生 policy | §4.6 |
| 协同面只描述接口 | 4.3 §8 对应行 | ✓ 只描述 `pluginhost.RegisterActivator` / `ph.Load` 的既有接口，不改其实现 | §6.3 |
| 与派发主流程的关系只标注不改 | 4.3 §3.5 对应行 | ✓ `MaybeNotify` / `RetryWorker` / `notification_deliveries` 均标注为"宿主写、不改" | §2.1 / §5.2 第 5 条 / §6.5 |
| 受审计文件不含内部任务编号 | `scripts/audit_open_source.py` 的 `internal task ID` 规则 | ✓ 本文与 manifest 草案均不含执行任务编号；编号只出现在 PR 描述与评审评论 | §16 |
| 中文注释与中文提交说明 | 计划任务 7.4 | ✓ 文档与 manifest 注释为中文；提交说明为中文 | §16 |

## 15. 验收清单（对应任务描述）

- [x] 接内核改造方案完整：`Activate` 唯一注册入口（§3.2）、同名 / 同信号互斥 `ErrDuplicate` + 配置期单激活（§3.2 / §3.3）
- [x] 宿主 Policy 设计：`Target` / `MaxSafetyLevel` / `BlastRadius` / `GrantedScopes` 四项齐备，**安全级别论证显式且不套用 datasource 的 L1 假设**（§4.1–§4.6）
- [x] 六渠道兼容迁移路径：分三片（B1 / B2 / B3），每片标注触碰面与回滚方式；B1/B2 不碰 `registry.go`（§5.1）
- [x] 默认部署行为不变有论证：六条不变量 + 既有测试零修改断言通过的判据（§5.2）
- [x] per-kind ceiling 的候选比较与取舍：四个候选 + "不做"，含每个的失败模式（§4.5）
- [x] 每个候选有 manifest 草案：`notifier-manifests/` 下 6 个文件（§6.2、§11.1）
- [x] 每个候选有注册路径（§6.3；§7–§10 各自标注 type 与二选一关系）
- [x] 每个候选有测试方案（§6.4；§7–§10 各自一张表）
- [x] 每个候选有回滚方案（§6.5 为范式；§7–§10 各自标注，含"对外副作用不可撤销"）
- [x] scope 硬约束：`required_scopes` ⊆ `core/domain/plugin.go:60-73`；新 scope 建议与独立 change 决策点已标注（§12）
- [x] 与 4.1 §3 / §4 ⑤ / §6、4.2 结论一致，无越界改动（§14；非目标清单见 §1.3）
- [x] 六个 manifest 已按 §11.1 的闸门实测通过，并记录 L2 ceiling 下被拒的对照结果
- [x] 实现切片仅给建议，未在本任务内合入（§13）
- [x] 受审计跟踪文件中不含内部任务编号
- [x] 中文注释与中文提交说明；PR 描述关联 OpenSpec change `plugin-architecture-recovery-plan` 任务 4.5

## 16. 提交与执行规范

- **落盘路径**：本文档 `openspec/changes/plugin-architecture-recovery-plan/notifier-plugin-plan.md`；manifest 草案 `openspec/changes/plugin-architecture-recovery-plan/notifier-manifests/`（`pagerduty.yaml`、`feishu-card.yaml`、`slack-blocks.yaml`、`dingtalk-markdown.yaml`、`webhook-strict.yaml`、`builtin-channels.yaml`）。
- **提交语言**：本任务 PR 使用中文注释 / 中文提交说明（按计划任务 7.4）。
- **关联**：OpenSpec change `plugin-architecture-recovery-plan` 任务 4.5；执行任务编号只记录在 PR 描述与评审评论中，**不写入受审计文件**（`scripts/audit_open_source.py` 的 `internal task ID` 规则会拒绝 `BENY-\d+` / `LUM-\d+` 形态）。
- **勾选状态**：本文档为方案 v0.1，评审通过前不勾选 `tasks.md` 4.5（与 4.1 / 4.3 的处理一致）。
- **实现切片**（§13）**不**在本任务内合入；如要落地，请用户单独开 change。

## 17. 风险与开放问题

### 17.1 风险分级

| 级 | 风险 | 触发条件 | 缓解 |
|---|---|---|---|
| **高** | per-kind ceiling 被否决（§4.5 A 未获批） | 内核改动需独立评审而评审未过 | 降级选项是候选 C（抬高 manager 全局 ceiling），代价是 `tool` / `adapter` 一并获得 L3 准入——**这是治理回退，需要用户显式决策**，不能由实现者默认选择 |
| **高** | "六个既有渠道其实都是 L3"这一结论未获评审认可 | 评审倾向"通知不算危险能力" | 该结论有 `safety.go` 原文支撑（§4.2）；若仍被否，方案退化为 §4.5 候选 D，PagerDuty 无处安放，需另开一次范围更大的讨论 |
| **中** | 候选包进入本仓 `plugins/` 造成治理混淆 | 实现者图省事 | §6.2 已写明理由；S-8 的包一律落在 `OPSKEEPER_MANAGER_PLUGINS_DIR` |
| **中** | 缺口 G-1（2xx 即成功）在存量六渠道上继续静默丢告警 | 本方案只在新包里修 | S-4 顺带做存量修复。**建议同批交付**，否则"装不装候选包"决定了钉钉签名错误是可见还是静默，这个差异本身会变成运维困惑 |
| **中** | 二选一配置项散落（六个类型各一个变体开关） | S-3 + S-8 落地后 | 配置面收敛为一个 `notify.channel_variant{type: variant}` 映射，别做成六个独立布尔；缺口 G-3 已记 |
| **低** | `Router` 实例名静默覆盖 | 存量问题，迁移不加重 | S-6；本任务不动 `notify.go` |
| **低** | 候选包签名 / 分发链路未走通 | S-8 阶段 | `pluginmanifest` 的签名通道与 marketplace 已有；S-8 的第一个包（dingtalk-markdown）先走通全链路 |

### 17.2 待评审问题

1. **`WithKindCeiling` 是否接受为内核改动？**（§4.5）这是本方案唯一的内核变更，也是唯一的阻塞项。
2. **`approval.required: false` 的表述是否够清楚？** 评审者可能读成"外发通知绕过了审批"。§4.3 的两段解释（字段语义 + 真正的授权在 admin-only 的渠道配置上）建议直接进 PR 描述。
3. **`install.strategy` 写 `pin` 而它是死字段**是否可接受？（§4.6）替代方案是照 datasources 写 `rolling`。
4. **合成的一方 manifest 不写 `min_pig_version`**，而第三方 manifest 写——同一目录内两种形状，是否接受？（`notifier-manifests/builtin-channels.yaml` 末尾给了理由。）
5. **飞书卡片按钮 / 审批回调**在 5.5 审批桥落地后是否要进 notifier 的范围？本方案按"不进"处理，理由是它需要入站端点，绕过审批设计。
6. **PagerDuty 的 `acknowledge` / `resolve`** 需要宿主暴露 incident 状态迁移。本方案按"v1 只做 trigger"处理，是否需要提前到首版？
