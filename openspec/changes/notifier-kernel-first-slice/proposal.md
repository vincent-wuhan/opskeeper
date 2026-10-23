# Why

`notifier` 是六个扩展点里唯一"加一个渠道就必须改编译内代码"的一个（`plugin-architecture-recovery-plan` 4.1 §6 的缺口行）。把它接进内核之前，有两个硬伤必须先拆掉，否则内核那一侧根本落不下去：

**一、内核准入对所有 Kind 一视同仁。** 装配层只有一个 manager 内核宿主，它按 `ManagerPolicy()` 的 `MaxSafetyLevel: L2` 准入所有 Kind。而 notifier 必须是 L3——每发一条通知都在别人的系统里造一个对象，`safety.go` 对这一点的措辞是"不可逆且对外可见"。于是六个 notifier 类型的 L3 manifest 在 L2 ceiling 下**全部被拒**（错误文本 `spec.safety_level: plugin is L3, target policy allows at most L2`，见 `notifier-plugin-plan.md` §11.1 实测表）。抬高全局 ceiling 是治理回退：它把 L3 准入一并交给 `tool` / `adapter` / `datasource`，而那份决定本来是留给部署 profile 的。

**二、投递结果只看 HTTP 状态码。** 钉钉 / 飞书 / 企业微信在签名错、限流、机器人被收回这三种故障下都回 **HTTP 200**，只是 body 里的 `errcode` 非零。存量 sender 停在状态码，于是这三种故障全被记成"投递成功"。运维看到的现象是"消息再也不来了，但系统说一切正常"（`notifier-plugin-plan.md` §2.3 的缺口 G-1）。这不是候选插件包的问题——**存量六渠道今天就在丢告警**，装不装候选包决定钉钉签名错误是可见还是静默，这个差异本身就会变成运维困惑。

# What Changes

- **S-1 —— 内核 per-kind safety ceiling。** `core/floor/kernel/host.go` 的 `Policy` 增加 `KindCeilings map[Kind]domain.SafetyLevel`，配一对方向相反的写入入口（收窄入口只收窄、放宽入口只放宽）、两个查询口（`EffectiveSafetyCeiling` / `WidestSafetyCeiling`）；包级准入用最宽 ceiling，per-kind 的闸门落在 `Host.Activate` 的 scoped registrar 上——那里才知道条目落在哪个 Kind，拒绝走既有的整包回滚路径。`NewHost` 校验每个条目，拒绝一个宿主自己执行不了的 ceiling。
- **S-4 —— 内置 webhook sender 的渠道判定钩子。** `core/base/pkg/notify/webhook.go` 的 `webhookSender` 增加一个**可空**的响应体判定钩子，只在 `NewFeishuSender` / `NewDingTalkSender` / `NewWeComSender` 三处接线；判定规则按渠道自己的协议走（`errcode` / `code` / `StatusCode` 非零即失败），错误文本带上渠道给的错误码与原因。构造函数签名不变。
- **不改的东西（硬约束）。** `core/floor/kernel/kind.go`（六个封闭 Kind）、`core/base/pkg/notify/registry.go`、`core/manager/biz/aiops/datasource/datasource.go`（含 `ManagerPolicy()`）、`core/manager/middleware/toolset/sources_plugin.go`（含 `adapterPolicy()`）一字不改；告警派发主流程（`MaybeNotify` / `FinishDelivery` / `RetryWorker`）不改；`plugins/` 不动；不新增 Kind；manifest schema 不加字段。
- **逐位不变的底线。** `KindCeilings` 为空 ⇒ 行为与改动前逐位相同，这是 S-1 的第一条验收标准。既有三个 `Policy()` 构造点（`datasource.ManagerPolicy()`、`toolset.adapterPolicy()`、edge 的 `Profile.Policy()`）零源码改动，并有测试钉住。
- **既有 e2e 断言一字不改。** G4 的 Feishu / DingTalk 签名断言原样保留，只新增子用例；`testenv.FakeSlack` 只做增量（默认仍回 200 "ok"）。

# Capabilities

## New Capabilities

- `plugin-admission-ceiling`: 内核宿主按扩展点（Kind）分别设定安全级别上限；条目生效、无条目回落全局；写入方向由入口点区分，写反方向 panic 而不是静默忽略。
- `notification-delivery-verdict`: 内置聊天渠道的投递结果由**渠道自己的回包**判定，而不是只看 HTTP 状态码；渠道拒收必须落成可重试的失败并把渠道给的原因带进错误文本。

## Modified Capabilities

- None.

# Impact

- **改动面**：`core/floor/kernel/host.go`、`core/base/pkg/notify/webhook.go`，以及它们的测试文件；`core/manager/biz/alert/`、`core/manager/middleware/toolset/`、`core/edge/pluginhost/` 三个既有测试文件追加"逐位不变"断言；`tests/e2e/` 新增一条子用例与 `testenv.FakeSlack` 的增量响应开关。
- **不改面**：六个封闭 Kind、`notify` 的渠道类型注册表、`datasource.ManagerPolicy()`、`toolset.adapterPolicy()`、告警派发主流程、`plugins/`、manifest schema。
- **运行时影响**：`KindCeilings` 为空 ⇒ 无影响；per-kind 条目只有显式写入才生效。
- **关联**：本 change 是 `plugin-architecture-recovery-plan` 任务 4.5 的落地切片，实现切片建议见该方案的 §13（S-1 与 S-4 两行）。
- **与方案文字的偏差**：§4.5 字面写的是"生效等级 = min(MaxSafetyLevel, KindCeilings[k])，只能收紧不能放宽"，而 §4.6 的草案把 `.WithKindCeiling(KindNotifier, L3)` 写在全局 L2 之上——两者互相矛盾（min 让条目变成空操作，收窄入口还会拒掉这次写入）。本 change 的裁定见 `design.md` §2.1：条目生效、无条目回落全局，写入方向由两个入口点区分。偏差与理由记在 `design.md` §6。
