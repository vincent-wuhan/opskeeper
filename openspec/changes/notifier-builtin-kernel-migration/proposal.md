# Why

`notifier` 仍是六个扩展点里唯一"加一个渠道就要改编译内代码"的一个：渠道工厂住在
`core/base/pkg/notify/registry.go` 的包级 map 里，`kernel.KindNotifier` 在内核里空着。
`plugin-architecture-recovery-plan` 任务 4.5 的方案（§5.1 / §5.2 / §5.3）把迁移切成
B1 / B2 / B3 三片，前置的 S-1（per-kind safety ceiling）与 S-4（渠道响应体判定钩子）
已随 `notifier-kernel-first-slice` 合入。本 change 落地 **B1 + B2**：

- **B1**：六个内置渠道（webhook / slack / feishu / dingtalk / wecom / telegram）成为
  编译内预装插件——每类型一个包名、一份合成 manifest、一个贡献体，经
  `Host.Activate` 注册进 `KindNotifier`。
- **B2**：装配层在 `ph.Load(ctx)` **之后**激活这六个一方包，`ErrDuplicate` 记 Info
  后跳过（第三方变体拥有该类型时不撞死启动）；`BuildChannel` 先读内核视图、未命中
  回落旧 map。

**B3（删除旧 map 与回落）不在本 change**，它动 `registry.go`，必须独立评审。

# What Changes

- **B1 新增内核胶水包 `core/manager/biz/notify/`（与 `biz/aiops/datasource` 同位）**：`Plugin*` 六个包名常量、
  `channelPlugin` 贡献体、`manifestFor()`（L3 / destructive / alert.read+alert.write /
  pin）、`Backends`（六个 factory 逐字复制自 `registry.go` 的 map 字面量）、
  `Activate(host, b)`、`Resolve(reg)`、`Set`、`Policy()`。
  落点不在方案 §5.3 草图写的 `core/base/pkg/notify/kernel.go`：`core/base/pkg` 不被
  允许 import `core/domain`，而 `manifestFor` 必须用 domain 的安全级别与 scope 常量。
  实测证据与裁定见 `design.md` §2。
- **B2 `core/base/pkg/notify` 只加一个读取视图**：`ChannelResolver` 接口 +
  `InstallChannelResolver`（装配根在启动期注入；传 nil 即回到迁移前路径）。`BuildChannel`
  先问视图、未命中回落旧 map 并打一条 Warn。`notify` 包**不**因此新增任何 import
  （不进 kernel / pluginmanifest / domain）。
- **`cmd/opskeeper/main.go` 三处增量**：宿主 policy 换成 `notifybiz.Policy()`（=
  `aiopsdatasource.ManagerPolicy()` + 恰好两个 delta，见 `design.md` §4）；`ph.Load` 之后调
  `notifybiz.Wire(ph.Kernel(), notifybiz.Builtins(), pluginLog)`（激活六包、装视图、打一条
  Info 汇总）；import 别名 `notifybiz`（`aiopsdatasource` 同款先例）。
- **等价性测试** `TestTheSixBuiltinChannelsResolveIdenticallyThroughTheKernel`：同一
  `ChannelSpec` 分别走旧 map 与内核，断言 Sender 名、HTTP 方法、请求体字节、签名头全等。
- **既有测试零修改**：`notify` 的三个测试文件、`tests/e2e/notify_*` 与
  `testenv/fakes.go` 一字不改仍然全绿。

# Capabilities

## New Capabilities

- `notifier-builtin-kernel-registration`: 六个内置渠道是编译内预装插件，经
  `Host.Activate` 注册进 `KindNotifier`；一包一类型；类型键即互斥键；第三方变体
  先装则一方包让位（`ErrDuplicate` 记 Info 跳过），其余类型照常激活。
- `notification-channel-resolution`: 渠道解析的读取顺序——先读启动期注入的内核视图，
  未命中回落旧 map；视图未注入时行为与迁移前逐位相同；错误文本不变。

## Modified Capabilities

- None.

# Impact

- **改动面**：新增 `core/manager/biz/notify/`（policy.go / kernel.go / 测试）；
  `core/base/pkg/notify/` 新增一个文件（resolver 视图 + `BuildChannel` 的读取顺序）；
  `cmd/opskeeper/main.go` 三处增量；`docs/opskeeper2-architecture.md` 的模块读数与
  notifier 段落。
- **不改面（硬约束）**：`core/floor/kernel/kind.go`（六 Kind 封闭集合）、
  `core/base/pkg/notify/registry.go`（B3 才动）、`core/manager/biz/aiops/datasource/datasource.go`
  含 `ManagerPolicy()`、`core/manager/middleware/toolset/sources_plugin.go` 含
  `adapterPolicy()`、告警派发主流程（`MaybeNotify` / `FinishDelivery` / `RetryWorker`）、
  `plugins/`、manifest schema、`core/floor/kernel/host.go`。
- **运行时影响（默认部署）**：启动日志**逐行不变**，新增白名单内的 7 条 Info（6 条来自
  内核的 `plugin activated` + 1 条装配层汇总），无新增 Warn / Error；渠道解析结果与
  改动前逐位相同；`ph.Kernel().Inventory()` 的 `notifier` 计数从 0 变 6（这是本切片的
  目的，不是副作用）。
- **治理影响**：manager 宿主的准入策略多授予 `alert.write` 一个 scope、并把
  `KindNotifier` 的 ceiling 放宽到 L3。这两条是**宿主级**的（policy 是一个宿主一份），
  其后果与边界见 `design.md` §4。
- **回滚**：注释掉装配层的 `notifybiz.Wire` 调用（或 `InstallChannelResolver(nil)`）
  即回到纯旧路径；`git revert` 整个 PR 亦无残留——内核条目随进程消失，旧 map 从未被动过。
