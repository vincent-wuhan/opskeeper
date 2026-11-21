## Purpose

`core/base/pkg/notify` 不再持有第二个渠道注册表：渠道 factory 的唯一来源是启动期安装的
内核视图，写入内核的唯一入口是 `Host.Activate`。这是 `plugin-architecture-recovery-plan`
任务 4.5 迁移路径 B3 片——B1/B2 合入后包内仍并存"旧 map + 内核视图"两套表面，本能力
收口为只剩一套。

## ADDED Requirements

### Requirement: 渠道注册表只有一个来源

渠道 factory MUST 只来自启动期安装的 `ChannelResolver` 视图。`core/base/pkg/notify` 内
MUST NOT 存在包级注册表（map / slice / 变量）、MUST NOT 存在绕过 `Host.Activate` 的
渠道写入口（如 `RegisterChannel`）、MUST NOT 存在从旧注册表派生的诊断读数（如
`RegisteredChannels`）。

- **WHEN** 在 `core/base/pkg/notify` 的源文本中检索 `channelFactories` / `RegisterChannel`
  / `RegisteredChannels`
- **THEN** 零命中（由源级钉子测试执行）

#### Scenario: 收口后包内无第二注册表

- **WHEN** `TestThePackageHoldsNoSecondRegistry` 运行
- **THEN** `registry.go` / `resolver.go` 的文本里不出现上述三个标识符

### Requirement: 视图安装是替换而非改写

`InstallChannelResolver` MUST 只安装或卸载视图指针，MUST NOT 改写任何共享数据结构；
`InstallChannelResolver(nil)` MUST 等价于"无视图"状态。视图 MUST 是启动期一次性安装的
快照：运行期对内核条目的变更不改变已安装视图的答案，重启后生效。

- **WHEN** 先安装视图 A，再安装视图 B，再 `InstallChannelResolver(nil)`
- **THEN** 每次安装后的解析答案只由当前安装的那个视图决定，卸载后一律拒绝

#### Scenario: 卸载后进程回到无渠道状态

- **WHEN** `Wire` 装好视图、投递路径可建六类型之后调用 `InstallChannelResolver(nil)`
- **THEN** 同一进程内任何渠道类型（含内置六类型）的构建都返回 `unknown channel type`

### Requirement: 未装视图的进程一律拒绝构建

视图未安装（从未安装、或已卸载）的进程里，`BuildChannel` 对任何渠道类型（包括六个
内置类型）MUST 返回 `unknown channel type %q`，且 MUST NOT 产生任何日志。这是相对
B2 的行为变更：回落路径与"未装视图时读旧 map"的保证一并退役。

- **WHEN** 未安装视图的进程对 `webhook` / `slack` / `feishu` / `dingtalk` / `wecom` /
  `telegram` / 未知类型逐个调用 `BuildChannel`
- **THEN** 全部返回错误，文本均为 `unknown channel type "<type>"`，日志记录数为 0

#### Scenario: 错误文本与 grep 口径不变

- **WHEN** 一条类型为 `not-a-channel` 的 spec 在无视图进程里构建
- **THEN** 错误文本为 `unknown channel type "not-a-channel"`，与迁移前逐字相同
