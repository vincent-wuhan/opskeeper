## Purpose

规定一条渠道类型被解析成 Sender 时的读取顺序，以及顺序的装配前提。B1+B2 期间
（`notifier-builtin-kernel-migration`）的顺序是"先读内核视图，未命中回落旧 map"；B3 删除
旧 map 之后收窄为"只读视图"，并把"急切构渠道的路径必须运行在装好视图之后"写进装配序。
错误文本不变。

本 delta 创建该 capability：B1/B2 的 change 未携带本名的 spec delta（`notifier-builtin-
kernel-registration` 是另一个名字），所以这里用 ADDED 而非 MODIFIED。

## ADDED Requirements

### Requirement: 渠道解析只读内核视图

`notify.BuildChannel` MUST 只读启动期安装的渠道视图（内核 `KindNotifier` 的解析结果）。
视图未安装时 MUST 对任何渠道类型返回 `unknown channel type %q`，错误文本与迁移前一字
不改，且 MUST NOT 产生任何日志。空名 sender 的 `built an unnamed sender` 拒绝文本与
判定 MUST 不变。

- **WHEN** 视图已安装且包含该渠道类型
- **THEN** 用视图里的 factory 构造 Sender

- **WHEN** 视图已安装但不含该类型
- **THEN** 返回 `unknown channel type %q`，不回落、不打日志

- **WHEN** 视图未安装
- **THEN** 同样返回 `unknown channel type %q`，不产生任何日志

#### Scenario: 未知类型的错误文本不变

- **WHEN** 一条类型为 `not-a-channel` 的 spec 分别在视图未安装、视图安装且未命中两种状态下构建
- **THEN** 两次的错误文本完全相同，均为 `unknown channel type "not-a-channel"`

#### Scenario: 部分失败不拖垮整体

- **WHEN** `NewRouterFromSpecs` 收到一条未知类型与一条已知类型的 spec
- **THEN** 未知类型被跳过并按调用方日志 Warn，已知类型照常注册

### Requirement: 急切构渠道的路径运行在视图安装之后

`NewFromConfig` / `NewRouterFromSpecs` 在构造期逐 spec 解析 factory（急切构），因此装配
根 MUST 保证它们在 `notifybiz.Wire` 安装视图**之后**执行；DB 渠道行的懒构路径
（`BuildSenderFromChannel`）不受此约束（投递期才解析）。视图未安装时急切构 MUST 产出空
router（每条 spec 被跳过），MUST NOT 报错中断进程。

- **WHEN** 装配根以配置了 URL 的渠道执行 `NewFromConfig`，且 `Wire` 已成功
- **THEN** 每条配置渠道在 router 上可按名解析

- **WHEN** 同一配置在未安装视图的状态下执行 `NewFromConfig`
- **THEN** router 为空，且进程继续启动（跳过语义，非致命）

#### Scenario: 装配序钉子

- **WHEN** `TestNewFromConfigBuildsChannelsThroughTheInstalledView` 先装视图再跑
  `NewFromConfig`，随后卸视图再跑一次
- **THEN** 第一次渠道建成，第二次 router 为空——两条断言都在同一条测试里

### Requirement: 解析等价性由测试钉住

六个内置渠道经视图（`Wire` + `BuildChannel`）与经 `Builtins()` factory 直接构造的产物
MUST 等价：Sender 名、HTTP 方法、请求路径、请求体字节、签名相关 header 全等。既有
notify e2e 用例 MUST 在零修改的前提下继续通过。

- **WHEN** 同一条 `ChannelSpec` 分别经 `Builtins()` factory 与经 `Wire` 安装的视图构造
  Sender 并投递到同一捕获端点
- **THEN** 两次请求的字节级形状完全一致

#### Scenario: 六渠道逐类型比对

- **WHEN** 对 webhook / slack / feishu / dingtalk / wecom / telegram 各取一条真实 spec，
  两条路径各构造一次并投递
- **THEN** 六组比对里 Sender 名、HTTP 方法、路径、请求体字节、签名 header 全部一致，
  `TestTheSixBuiltinChannelsResolveIdenticallyThroughTheKernel` 通过
