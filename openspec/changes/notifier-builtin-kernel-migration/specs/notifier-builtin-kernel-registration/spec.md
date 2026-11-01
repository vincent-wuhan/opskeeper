## Purpose

把"通知渠道怎么来的"从一份编译内 map 换成内核注册表的一个扩展点：六个内置渠道是编译内
预装插件，经同一道准入闸门进 `KindNotifier`；一个渠道类型一个实现，第三方变体先装则一方
包让位。这是 `plugin-architecture-recovery-plan` 任务 4.5 迁移路径的 B1 片（B2 见
`notification-channel-resolution`）。

## ADDED Requirements

### Requirement: 六个内置渠道是编译内预装插件

webhook / slack / feishu / dingtalk / wecom / telegram 六个内置渠道 MUST 作为编译内预装
插件注册进内核 `KindNotifier`：一个渠道类型一个插件包名（`opskeeper-notifier-<type>`），
条目名是**渠道类型**本身，实现体是 `notify.ChannelFactory`。包名与渠道类型 MUST NOT 合并
成同一个字符串。

- **WHEN** 装配层以默认 backends 激活六个内置渠道
- **THEN** 内核 `KindNotifier` 下出现六个条目，条目名恰为六个渠道类型字符串
- **AND** `Host.Live()` 含六个 `opskeeper-notifier-*` 包名

#### Scenario: 默认部署六个类型全部进内核

- **WHEN** `Wire(host, Builtins(), log)` 在无第三方 manager 插件包的宿主上执行
- **THEN** 返回的 `Set` 含六个类型，每个类型的持有包都是对应的一方包名
- **AND** 不产生 Warn 或更高级别的日志

#### Scenario: 未配置的渠道类型不激活

- **WHEN** `Backends` 的某字段为 nil
- **THEN** 该类型不激活、内核无该条目、`Resolve` 后该类型查不到
- **AND** 不视为错误

### Requirement: 合成 manifest 与已评审草案一致

六个内置渠道的合成 manifest MUST 声明 `safety_level: L3`、`capabilities: [destructive]`、
`required_scopes: [alert.read, alert.write]`、`targets: [manager]`、
`audit: {emits: true, mutates: false}`、`approval: {required: false}`、
`install: {strategy: pin}`，且 MUST NOT 写 `min_pig_version`。

- **WHEN** 比较合成 manifest 与
  `plugin-architecture-recovery-plan/notifier-manifests/builtin-channels.yaml`
- **THEN** 除 `metadata.name` 逐包不同外，其余字段逐个一致

#### Scenario: L3 是唯一合法的安全级别

- **WHEN** 有人把某个内置渠道的合成 manifest 改成 L1 或 L2
- **THEN** 该改动与本能力的 Requirement 冲突，评审直接拒绝：通知渠道写的是别人的系统，
  低报安全级别是这个文件里后果最重的一句假话

### Requirement: 第三方变体先装则一方包让位

装配顺序 MUST 是 `ph.Load`（第三方包）先、一方内置包后。一方包激活遇到
`kernel.ErrDuplicate`（该渠道类型已被第三方包持有）MUST 记 **Info** 后跳过该类型，
MUST NOT 视为启动失败；除 `ErrDuplicate` 之外的任何激活错误 MUST 使启动失败。

- **WHEN** 一个第三方包已注册 `KindNotifier/slack`，随后内置 slack 包激活
- **THEN** 激活不报错，日志出现一条 Info，点名让位的一方包与持有该类型的第三方包
- **AND** 其余五个内置类型照常激活
- **AND** 该渠道类型的解析结果来自第三方实现，不是内置实现

#### Scenario: 变体部署启动成功且变体生效

- **WHEN** `OPSKEEPER_MANAGER_PLUGINS_DIR` 里装着注册 `slack` 的
`opskeeper-notifier-slack-blocks`
- **THEN** manager 正常启动，`slack` 由该包持有，其余五个内置类型由一方包持有

#### Scenario: 非重复的激活失败是致命的

- **WHEN** 某内置渠道的激活因 per-kind ceiling、scope 或 manifest 校验被拒
- **THEN** 装配层记 Error 并退出进程
- **AND** 不存在"六个渠道少了一个而启动日志看着正常"的状态

### Requirement: 内核胶水不落在共享底座

内核胶水（包名常量、贡献体、manifest 合成、backends、激活、解析、policy）MUST 落在
manager 侧（`core/manager/biz/notify/`），MUST NOT 落在 `core/base/pkg/notify`。
`core/base/pkg/notify` MUST NOT 因此新增 `core/floor/kernel`、`core/floor/pluginmanifest`
或 `core/domain` 的 import。

- **WHEN** `make module-check` 在本 change 的树上运行
- **THEN** 无边界违例报告

#### Scenario: 把 manifest 合成放进共享底座会被闸门拒绝

- **WHEN** `core/base/pkg/notify` 的某个文件 import `core/domain`（`manifestFor` 需要
  安全级别与 scope 常量）
- **THEN** `make module-check` 报 `shared_pkg imports ... (oxcore_domain), which no rule
  in .go-arch-lint.yml permits`，本 change 不合入

#### Scenario: 底座只认识解析视图这个接口

- **WHEN** 检视 `core/base/pkg/notify` 的 import 列表
- **THEN** 除既有依赖外没有新增项；内核与治理词表只出现在 manager 侧的胶水包里

### Requirement: manager 宿主准入策略派生自 datasource 策略且恰好两个 delta

manager 内核宿主的准入策略 MUST 由 `aiopsdatasource.ManagerPolicy()` 派生，且与它的差
MUST 恰好为两处：`KindNotifier` 的 ceiling 放宽到 L3（经 `WithWiderKindCeiling`），以及
`GrantedScopes` 追加 `alert.write`。`Target`、`MaxSafetyLevel`、`MaxBlastRadius` 与既有
scope 集合 MUST 逐位不变。`datasource.ManagerPolicy()` 本身 MUST NOT 被修改。

- **WHEN** 比较 `notifybiz.Policy()` 与 `aiopsdatasource.ManagerPolicy()`
- **THEN** 仅 `KindCeilings[KindNotifier]` 与 `GrantedScopes`（多一个 `alert.write`）不同

#### Scenario: L3 包在其他扩展点上仍被拒

- **WHEN** 一个 L3 的 manager 目标包在 `KindTool` 上注册条目
- **THEN** 包级闸门放行（最宽 ceiling 为 L3），条目在 per-kind 闸门被拒，整包回滚
- **AND** 该拒绝不致命、进入加载报告的 Refused 列表
