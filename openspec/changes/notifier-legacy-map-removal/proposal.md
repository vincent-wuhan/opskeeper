# Why

B1+B2（`notifier-builtin-kernel-migration`，PR #118）把六个内置渠道接进了内核：装配层在
`ph.Load(ctx)` 之后 `notifybiz.Wire(...)`，`BuildChannel` 先读启动期安装的内核视图。但迁移
收尾的那一刀被明确留到 B3：`core/base/pkg/notify/registry.go` 的 `channelFactories` 旧 map
还在，`BuildChannel` 未命中视图时仍回落它，`resolver.go` 的安装逻辑还要围绕这张 map 做
改写与还原。

这是一个**双注册表**状态，不是稳定态：

- 同一份行为（六个 factory 闭包）在 `registry.go` 的 map 与 `biz/notify.Builtins()` 各存一份，
  靠 `TestTheSixBuiltinChannelsResolveIdenticallyThroughTheKernel` 钉住不漂移——那是迁移期的
  临时保险，不是长期契约；
- `InstallChannelResolver` 的实现因此复杂了整整一层（改写 map、记录 displaced/added、
  还原、每类型至多一条的回落 Warn），而这一层的全部存在理由就是"旧 map 还活着"；
- `RegisterChannel` 是旧注册表的写入口，**全仓生产调用者为零**；`RegisteredChannels` 从
  旧 map 派生，也只剩测试在读。

方案（`plugin-architecture-recovery-plan/notifier-plugin-plan.md` §5.1 B3 行）把这刀的形状
写死了："`BuildChannel` 只读内核，删除遗留 map 与回落；`RegisterChannel` 要么删除、要么
改为包装 `Activate`"。本 change 落地这一刀。

# What Changes

- **`core/base/pkg/notify/registry.go`**：删除 `channelFactories` 旧 map、`channelMu`、
  `RegisterChannel`、`RegisteredChannels` 与回落路径；`BuildChannel` 只读启动期安装的
  `ChannelResolver` 视图。**这是行为变更**：未安装视图的宿主路径从"读旧 map 建出六个内置
  渠道"变为"`unknown channel type`"——影响面与理由见 `design.md` §2。
- **`core/base/pkg/notify/resolver.go`**：`InstallChannelResolver` 从"改写旧 map"改为
  "持有一个指针"；安装/卸载仍是启动期一次性的，nil 仍是合法值（卸载后 `BuildChannel`
  一律 `unknown channel type`）。回落 Warn、`displacedByView` / `addedByView` /
  `restoreRegistryLocked` / `fallbackFactory` 随回落路径一并退役。
- **`ChannelResolver` 接口收窄**：删除 `Types()` 与 `Logger()`——它们在旧实现里的用途
  （遍历旧 map 改写、回落 Warn 的落点）已随 map 消失；`Set.Logger()` 相应删除。
- **等价性测试收口**：`TestTheSixBuiltinChannelsResolveIdenticallyThroughTheKernel` 按
  "只剩一条路径"改写（旧路径一侧改为直接以 `Builtins()` factory 构造，钉住的是"内核
  装配产出的 factory 与 `Builtins()` 是同一行为"，不再是"两条注册表不漂移"）；
  `TestBuildChannelFallsBackToTheLegacyMapOncePerType` / `TestInstallNilRestoresThePreMigrationPath`
  随回落路径退役，替代用例钉住新语义（未装视图 ⇒ 一律 `unknown channel type`，且零日志）。
- **既有测试**：`registry_test.go` 里依赖 `RegisterChannel` / `RegisteredChannels` /
  "未装视图可建内置渠道"的用例按新语义改写或退役；`notify_test.go` / `webhook_test.go`
  不动（它们不经过注册表面）。
- **不改面（硬约束，与 B1/B2 相同）**：`kind.go`、`datasource.go`（含 `ManagerPolicy()`）、
  `sources_plugin.go`、告警派发主流程（`MaybeNotify` / `FinishDelivery` / `RetryWorker`）、
  `plugins/`、manifest schema、`core/floor/kernel/`。

# Capabilities

## New Capabilities

- `notifier-single-registry`: `core/base/pkg/notify` 内不再存在第二个渠道注册表——渠道
  factory 的唯一来源是启动期安装的内核视图；写入内核的唯一入口是 `Host.Activate`；
  `InstallChannelResolver` 只是视图的安装/卸载，不构成注册通道。

## Modified Capabilities

- `notification-channel-resolution`: 读取顺序从"先视图、未命中回落旧 map"收窄为
  "只读视图"；未装视图的宿主一律 `unknown channel type`，错误文本不变；视图仍是
  启动期一次性安装的快照。
