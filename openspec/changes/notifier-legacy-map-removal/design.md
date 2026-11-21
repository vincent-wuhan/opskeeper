# Design — notifier 旧 map 与回落路径删除（B3）

本文件是实现依据。方案来源：`plugin-architecture-recovery-plan/notifier-plugin-plan.md`
§5.1 B3 行与 §5.2 论证表；前置：`notifier-kernel-first-slice`（S-1 / S-4，已合入）、
`notifier-builtin-kernel-migration`（B1 + B2，PR #118，已合入）。

## 1. 范围

| 事项 | 处置 |
|---|---|
| `channelFactories` 旧 map 与 `channelMu` | **删除** |
| `BuildChannel` 的回落路径与 Warn | **删除**；只读视图 |
| `RegisterChannel`（写入口） | **删除**（ADR-1） |
| `RegisteredChannels`（诊断读） | **一并删除**（ADR-2） |
| `InstallChannelResolver` 改写旧 map 的整层机制 | 改为持有指针 |
| `ChannelResolver.Types()` / `Logger()` | **收窄删除**（ADR-3） |
| 等价性测试 | 按"只剩一条路径"改写 |
| 回落路径的钉子测试 | 退役，换新语义钉子 |
| `kind.go` / `datasource.go` / `sources_plugin.go` / 告警派发主流程 / `plugins/` / manifest schema / `core/floor/kernel/` | **零改动** |

非目标：候选插件包（S-8）、渠道类型枚举扩展（S-5）、Router 重名（S-6）、`ChannelSpec`
凭证模型（S-7）、per-kind scope（Q2）、运行期重载（Q3）。

## 2. 裁定 D-1：未装视图的宿主路径改变——这是行为变更，PR 描述必须写明

B2 的回落路径保证"未装配 notifier 的二进制（全部既有单测、e2e 里不经装配根的部分）仍能
从旧 map 建出六个内置渠道"。B3 删 map 后，这条保证**不再成立**：`BuildChannel` 在未安装
视图的进程里对任何类型（包括 `webhook`）都返回 `unknown channel type %q`。

这个变更为什么可以接受，逐类盘点（全仓实测，`grep -rn "BuildChannel\|NewRouterFromSpecs\|NewFromConfig" --include="*.go" cmd/ core/ tests/`）：

| 调用方 | 路径 | B3 后的行为 |
|---|---|---|
| `cmd/opskeeper/main.go` `notify.NewFromConfig` | 装配根，**急切构渠道**（`NewRouterFromSpecs` 在构造期逐 spec 调 `BuildChannel`） | **不变，前提是装配序修正**——见下 D-1b：迁移前现序是 `NewFromConfig`（635 行）先于 `Wire`（1839 行），B2 靠回落掩盖了这个倒挂；本 change 把 `notifyRouter :=` 连同其第一个消费者 `alertSvc` 的构造移到 `Wire` 之后 |
| `core/manager/biz/alert/usecase.go` `BuildSenderFromChannel` | 生产告警投递，逐行懒构 sender（`MaybeNotify` 时才调） | **不变**（投递远晚于启动，视图已装） |
| `tests/e2e/*`（testenv 起真 manager 二进制） | 同生产进程 | **不变**；且 testenv 不配置 config-file 渠道（渠道走 DB 行），所以这组测试**看不见** D-1b 那个倒挂——这正是本 change 在闸门之外补装配序钉子测试的原因 |
| `core/base/pkg/notify` 自身单测 | **不经装配根** | 改写：等价性/注册表用例按新语义重写 |
| 其他任何把 `notify` 当库用的潜在二进制 | 假设不存在 | 本仓实测为零；`core/edge` 不 import 它 |

### D-1b：装配序修正（本 change 的第二处代码改动，与删 map 同一片内交付）

D-1 的盘点最初假设"`Wire` 先于 `NewFromConfig`"——**实测相反**，且 B1/B2 都没发现：

- 迁移前 main.go 现序：`notify.NewFromConfig` 于 635 行，`notifybiz.Wire` 于 1839 行，
  两者都在 `main()` 内顺序执行，**构造先于装视图**。
- `NewFromConfig → NewRouterFromSpecs → BuildChannel` 是**急切**路径：每个配置了 URL
  的渠道在构造期就要解析 factory。B2 期间这个窗口由回落兜住（map 里有六类型，命中即
  返回）；B3 删 map 后，同一个 635 行的调用会拿到 `unknown channel type`，被
  `NewRouterFromSpecs` 的"跳过并 Warn"语义**静默吞掉**——配置文件渠道（env 的
  webhook/slack/feishu/dingtalk 四项）全部消失，Router 退化为空表，默认渠道投递报
  `no channels configured`，而进程照常启动。
- 为什么 B1/B2 的闸门没抓住：e2e 的渠道全部走 DB 行（`SeedChannelsFromConfig` →
  `BuildSenderFromChannel`，懒构、投递期解析），`NewFromConfig` 的急切路径在 e2e
  配置里是空的；单测不跑 main()。

处置：**移动而非修补**。把 `notifyRouter := notify.NewFromConfig(...)` 连同其第一个
消费者（`alertSvc` 构造块，含 `SetPreviewDeps`）整体搬到 `Wire` 成功之后。选这个形状
而不是"把 Wire 提到 635 行之前"有两个理由：

1. `Wire` 需要完整的插件序——第三方 manager 包（`ph.Load`）必须先于一方渠道激活，而
   `ph` 在 1719 行才建。把 Wire 提前等于把整个 pluginhost 块提前，牵动
   `promQueryClient`（1315 行起）等一串依赖，改动面大一个数量级。
2. `notifyRouter` 在 636–1671 行之间**零消费者**（实测 grep），`alertSvc` 在其后到
   Wire 之间也零消费者；搬移是纯顺序变化，无语义涟漪。`alertSvc` 原注释"Built before
   the AIOps runtime"仍然成立（AIOps runtime 构建于 2200 行起，仍在 alertSvc 之后）。

新增钉子 `TestNewFromConfigBuildsChannelsThroughTheInstalledView`（`core/base/pkg/notify`）
以装好视图 + 一条 webhook 配置跑 `NewFromConfig`，断言渠道建成——把"急切构路径必须在
视图下可解析"钉进单测层，装配序再倒回去时先红的是它而不是生产。

换句话说：**仓库里所有生产路径在装配序修正后都装了视图**，唯一"未装视图"的消费者是
`core/base/pkg/notify` 自己的测试。删除回落等于把这个包的测试从"依赖一个包级全局注册表"
改造成"显式安装视图"，这本来就是微内核方向上正确的测试形状。

仍把它定性为行为变更并写进 PR 描述，理由有三：

1. **它是公开 API 的语义变化**。`BuildChannel` 对"视图未安装"这一状态的答案从"六类型可
   建"变为"一律拒绝"。任何仓库外的下游（本项目开源，理论存在）若直接以库形式 import
   `core/base/pkg/notify` 且不经装配根，B3 之后启动即坏。这个风险已知且接受：该包的
   doc.go 定位是"宿主传输层"，其注册表面从来不是对外承诺的稳定 API；CHANGELOG 须记
   BREAKING 一条。
2. **错误可见性变好而不是变坏**。回落路径的一个已知坏性质是"静默装了六渠道但内核里没有
   它们"——inventory 为 0 而投递照常，治理面与行为面分叉。B3 之后这种分叉不可能存在：
   视图没装，投递就失败，失败文本可 grep。
3. **回滚仍然是一刀**：`git revert` 本 PR。B2 的"注释掉 Wire"回滚杠杆在 B3 之后变成
   "整进程不可用"，但那正是收口的本意——迁移窗口（B1 合入至今）已经给了部署足够的
   观察期，方案 §5.1 把 B3 的回滚方式就写成 `git revert`。

## 3. ADR-1：`RegisterChannel` 删除，而不是包装 `Activate`

方案 §5.1 给了两个选项。裁决：**删除**。

事实基础（全仓实测）：

- 生产调用者：**0**。`grep -rn "RegisterChannel" --include="*.go"` 只命中
  `registry.go` 自身定义与 `registry_test.go` / `resolver_test.go` 的测试调用。
- 测试调用者：4 处（重复拒绝、空/nil 拒绝、第三方注册后可建、unnamed-sender 三路径）。

为什么不是"包装 `Activate`"：

1. `Activate` 需要三样东西：`*kernel.Host`、合成 manifest、贡献体。`RegisterChannel(typ, f)`
  的签名一样都给不出——它是包级函数，没有宿主可抓。包装它在签名上只能撒谎（隐式全局
  host 或 panic），而 B2 的 design 已把"包级服务定位器"这类形状判过一次（§2.1）。
2. 它存在的理由已经消失：B1 之后"第三方渠道"的注册入口是"`Host.Activate` + 一份
  manifest"，这正是方案要的唯一入口；保留一个绕过准入闸门的写入口，等于在收口 PR 里
  再开一个后门。
3. "万一有下游在用"不构成保留理由：**本仓零调用者**，且上游 README 未承诺该 API。

## 4. ADR-2：`RegisteredChannels` 一并删除

它从旧 map 派生，map 删除后它要么跟着死，要么改读视图。裁决：**删除**，理由：

- 生产调用者：**0**（全仓 grep 只命中定义与 `registry_test.go` 的断言）。
- 诊断语义已有更好的承担者：B2 之后运维读 notifier 持有情况走装配层那条
  `notification channels activated` Info（按类型给出持有包），比 `RegisteredChannels`
  的裸类型列表信息更多；inventory 页读 `ph.Kernel().Inventory()`。
- 若改为读视图，它就变成 `Set.Types()` 的转发——一个零调用者的转发函数，留在公共
  API 里只会被 deadcode 棘轮点名为 test-only。

与 ADR-1 同理：开源不等于这些内部函数是承诺面。CHANGELOG 的 BREAKING 条目把两个名字
都列上。

## 5. ADR-3：`ChannelResolver` 收窄到 `Factory(typ)` 一个方法

B2 的接口有三个方法：`Types()` / `Factory(typ)` / `Logger()`。逐一复查它们在
"没有旧 map 可改写"的世界里的用途：

| 方法 | B2 中的用途 | B3 后 |
|---|---|---|
| `Factory(typ)` | `BuildChannel` 的读取口 | **保留**（唯一必需） |
| `Types()` | 安装时遍历，找出视图持有而旧 map 没有的类型加进 map | 无 map 可加，**删除** |
| `Logger()` | 回落 Warn 的落点（`BuildChannel` 的 log 参数可能为 nil） | 无回落即无 Warn，**删除** |

收窄的收益是接口回到最小面；代价是 `Set.Logger()` 一并删（它只服务 `Logger()`）。
`Set` 其余三个方法（`Factory` / `Holder` / `Types`）不受影响——`Holder` 是那条
Info 汇总的来源，`Types` 是装配层自己用的。

`stubResolver`（resolver_test.go）与 `Set` 同步收窄。这是本 change 对测试文件的
唯一"顺手"改动，但它不是顺手：接口收窄后这些方法没有调用方，留着编译不过。

## 6. `InstallChannelResolver` 的新形状

```go
// resolver.go 全部内容（概念）
var installedView ChannelResolver

func InstallChannelResolver(r ChannelResolver) { installedView = r }

func BuildChannel(spec ChannelSpec, log *slog.Logger) (Sender, error) {
    f, ok := installedView.Factory(spec.Type)   // nil 解引用安全：接口 nil 检查
    if !ok {
        return nil, fmt.Errorf("notify: unknown channel type %q", spec.Type)
    }
    sender, err := f(spec, log)
    ...
}
```

三个写死的细节：

1. **nil 视图的判定**：`installedView` 是接口类型，`InstallChannelResolver(nil)` 与
   从未安装不可区分——这正是要的语义（两者都 ⇒ 一律 `unknown channel type`）。
   判定用 `f, ok := installedView.Factory(...)` 之前先 `if installedView == nil`，
   免得依赖"接口 nil 时方法调用的行为"（typed-nil 陷阱：`Set` 是值类型实现，
   不会出现 typed nil，但防御一行换掉一类坑）。
2. **锁删除**：旧实现需要 `channelMu` 是因为安装会**改写** map 而读取走 map。新实现
   安装是**一次指针赋值**，读取是一次指针读——`Wire` 在任何 `BuildChannel` 之前完成
   （装配序），不存在并发窗口。启动期一次性赋值 + 之后只读，是 Go 内存模型下安全的
   常规形状（与 `sort.Strings` 一个级别的确定性）。若未来出现运行期重载（Q3），
   那一天要回来加锁，design 记录在案。
3. **错误文本不变**：`unknown channel type %q` 与 `built an unnamed sender` 一字不改
   （B2 的 I3 不变量在本 change 继续成立，来源从"map 查不到"变为"视图查不到或视图
   没装"——对 grep 的人是同一个字符串）。

## 7. 测试设计

### 7.1 改写：`TestTheSixBuiltinChannelsResolveIdenticallyThroughTheKernel`

B2 版本对比"旧 map 路径"与"内核路径"。B3 之后旧 map 不存在，测试改为对比：

- 一侧：`Builtins()` 直接以 factory 构造 sender（这是 B1 逐字复制的那份闭包）；
- 另一侧：`Wire` 进真 kernel host 后 `BuildChannel`（视图路径）。

钉住的命题从"两份注册表不漂移"变为"**视图拿到的 factory 就是 `Builtins()` 的行为**"。
线上的对比口径（Name / HTTP 方法 / 路径 / body 字节 / 签名 material / 时钟字段归一化）
全部保留——它们防的是 factory 实现漂移，与注册表数量无关。

### 7.2 退役与替代

| B2 用例 | 处置 |
|---|---|
| `TestBuildChannelFallsBackToTheLegacyMapOncePerType` | **退役**（回落不存在）。替代：`TestBuildChannelWithoutAViewRefusesEveryType`——未装视图时六类型与未知类型一律 `unknown channel type`，且零日志 |
| `TestInstallNilRestoresThePreMigrationPath` | **退役**（迁移前路径不复存在）。替代：`TestInstallNilReturnsTheProcessToNoChannels`——装过视图再 `InstallChannelResolver(nil)` 后一律拒绝；卸载杠杆仍可用（测试隔离靠它） |
| `TestUnknownChannelTypeErrorTextIsUnchanged` | 改写为单一状态（无视图）；文本断言保留 |
| `TestTheUnnamedSenderErrorTextIsUnchangedAcrossTheThreePaths` | 三路径收为两路径（视图持有 / 未装视图）；文本断言保留 |
| `TestTheViewAnswersForATypeItHolds` | 保留，断言零 Warn 的部分改零日志 |
| `TestRegisteredChannelsCoversBuiltins` / `TestRegisterChannel*` / `TestRegisterThenBuildThirdPartyChannel` | **退役**（ADR-1/2）；第三方注册的语义已由 `TestAThirdPartyVariantOwningSlackWinsOverTheBuiltinAndBootStillSucceeds`（B2）钉住，该用例不动 |
| `TestNewRouterFromSpecsBuildsRegisteredChannels` / `TestUnknownChannelTypeIsSkippedNotFatal` | 改写为显式装视图（`stubResolver` 持六类型）；断言保留 |
| `TestNewFromConfigBuildsTheSameChannels` | 改写为装视图后走 `NewFromConfig`（它在生产里就是装了视图运行的）；断言保留 |

### 7.3 新增钉子

- `TestThePackageHoldsNoSecondRegistry`（`core/base/pkg/notify`）：源级断言——
  `registry.go` / `resolver.go` 里不再出现 `channelFactories` / `RegisterChannel` /
  `RegisteredChannels` 标识符（读文件文本比对）。这是方案 S-4 验收句
  "`grep -rn \"channelFactories\"` 在 `notify` 包内只剩内核路径"的可执行化。
- `TestWireInstallsTheViewAndBuiltinsAnswerThroughIt`（`biz/notify`）：`Wire` 后不
  `InstallChannelResolver(nil)` 也能建六类型（钉住装配序：视图在投递路径可达前已装）。

### 7.4 不动的测试

`notify_test.go`（Router 语义）、`webhook_test.go`（sender 行为与响应体钩子）、
`biz/notify` 的 `TestAThirdPartyVariantOwningSlack...` / `TestWireLogsExactlyTheWhitelistedRecords` /
`TestNotifierPolicyIsManagerPolicyPlusExactlyTwoDeltas`、`tests/e2e/notify_*`（G1/G3/G4/G8）、
`testenv/fakes.go`——一字不改（e2e 起真 manager 二进制，生产路径 B3 后不变，见 §2 表）。

## 8. 不变量

| # | 不变量 | 验证 |
|---|---|---|
| N1 | 装了视图的进程：六类型解析结果与 B2 逐位相同（factory 同一份闭包） | 7.1 改写后的等价性测试 + e2e G1/G3/G4/G8 零修改通过 |
| N2 | 错误文本 `unknown channel type %q` / `built an unnamed sender` 一字不改 | 7.2 文本断言保留 |
| N3 | `notify` 包内无第二个注册表 | 7.3 源级钉子 |
| N4 | 硬约束零改动（`kind.go` / `datasource.go` / `sources_plugin.go` / 告警派发 / `plugins/` / manifest schema / kernel） | `git diff --name-only` |
| N5 | `notify` 不新增 kernel / pluginmanifest / domain import（B2 的 C3 继续成立，本 change 反而少了 `sync` / `sort`） | `make module-check` |

## 9. 闸门

`make pi-plugin-guard`；`make module-check`；`go test ./scripts/ledgercheck`（若
`core/base/pkg/notify` 或 `core/manager` 行数读数变化须同提交更新台账）；五模块全量
`go test ./...`；`make deadcode-ratchet-check`（本 change 是**净删除**，预期读数下降，
若需下调基线同一提交写明理由）；`openspec validate --all --no-interactive`；
`scripts/audit_open_source.py`；e2e `-tags e2e` G1/G3/G4/G8。

## 10. 风险

| 级 | 风险 | 缓解 |
|---|---|---|
| 中 | 下游以库形式 import `notify` 且不装视图 | 本仓零此用法；doc.go 定位宿主传输层；CHANGELOG BREAKING 条目点名 `BuildChannel` 语义与两个删除的导出符号 |
| 低 | 测试遗漏"未装视图"路径的新语义 | 7.2 替代用例 + 7.3 钉子 |
| 低 | deadcode 棘轮读数变化 | 净删除，方向向下；同一提交调整并写明 |
| 低 | `Set.Logger()` 删除影响 `Wire` | `Wire` 不调用它；编译器兜底 |
