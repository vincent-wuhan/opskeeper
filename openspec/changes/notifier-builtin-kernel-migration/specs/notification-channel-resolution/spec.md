## Purpose

规定一条渠道类型被解析成 Sender 时的读取顺序：先读启动期安装的内核视图，未命中回落
既有 map；视图未安装时行为与迁移前逐位相同。这是迁移 B1+B2 期间保证"默认部署行为
不变"的那一半，也是 B3 删除旧 map 之前唯一允许的回落路径。

## ADDED Requirements

### Requirement: 渠道解析先读内核视图再回落旧表

`notify.BuildChannel` MUST 先读启动期安装的渠道视图（内核 `KindNotifier` 的解析结果），
未命中再回落 `registry.go` 的既有 map。视图未安装时 MUST 走旧 map 且行为与迁移前逐位
相同（含零日志）。两边都没有时错误文本 MUST 保持 `unknown channel type %q` 一字不改。

- **WHEN** 视图已安装且包含该渠道类型
- **THEN** 用视图里的 factory 构造 Sender

- **WHEN** 视图已安装但不含该类型，旧 map 含有
- **THEN** 用旧 map 的 factory 构造 Sender，并记一条 Warn（每个类型至多一条），说明这是
  将在后续切片删除的回落路径

- **WHEN** 视图未安装
- **THEN** 只读旧 map，不产生任何日志，行为与迁移前逐位相同

#### Scenario: 回落路径可观测但不刷屏

- **WHEN** 同一个仅在旧 map 里的渠道类型被连续构建三次
- **THEN** 三次都构造成功，Warn 恰好一条

#### Scenario: 未知类型的错误文本不变

- **WHEN** 一条类型为 `not-a-channel` 的 spec 分别在视图未安装、视图安装且未命中两种状态下构建
- **THEN** 两次的错误文本完全相同，均为 `unknown channel type "not-a-channel"`

### Requirement: 视图是启动期一次性安装的

渠道视图 MUST 由装配根在启动期安装一次；`InstallChannelResolver(nil)` MUST 卸载视图并
恢复迁移前路径。视图 MUST 是启动期快照：运行期对内核条目的变更不改变已安装视图的答案，
重启后生效。

- **WHEN** `InstallChannelResolver(nil)` 之后调用 `BuildChannel`
- **THEN** 行为与从未安装过视图逐位相同

- **WHEN** 运行期 `Deactivate` 一个持有某渠道类型的包
- **THEN** 已安装视图对该类型的答案不变，直至进程重启

#### Scenario: 卸载视图即回到迁移前

- **WHEN** `InstallChannelResolver(nil)` 之后用一条仅存在于旧 map 的渠道类型构造 Sender
- **THEN** 构造成功，且不产生任何日志——与从未装过视图的表现逐位相同

### Requirement: 解析等价性由测试钉住

六个内置渠道在旧 map 与内核两条路径上的构造结果 MUST 等价：Sender 名、HTTP 方法、请求
路径、请求体字节、签名相关 header 全等。既有 notify 单元测试与 notify 相关 e2e 用例
MUST 在零修改的前提下继续通过。

- **WHEN** 同一条 `ChannelSpec` 分别经旧 map 与内核视图构造 Sender 并投递到同一捕获端点
- **THEN** 两次请求的字节级形状完全一致

#### Scenario: 六渠道逐类型比对

- **WHEN** 对 webhook / slack / feishu / dingtalk / wecom / telegram 各取一条真实 spec，
  两条路径各构造一次并投递
- **THEN** 六组比对里 Sender 名、HTTP 方法、路径、请求体字节、签名 header 全部一致，
  `TestTheSixBuiltinChannelsResolveIdenticallyThroughTheKernel` 通过
