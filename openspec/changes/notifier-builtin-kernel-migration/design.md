# Design — notifier 六渠道经内核迁移（B1 + B2）

本文件是实现依据。每一节回答"为什么是这个形状"，并给出一处必须由评审确认的偏差。
方案来源：`plugin-architecture-recovery-plan/notifier-plugin-plan.md` §5.1 / §5.2 / §5.3；
前置切片：`notifier-kernel-first-slice`（S-1 per-kind ceiling、S-4 响应体判定钩子，已合入）。

## 1. 范围

| 切片 | 内容 | 在本 change |
|---|---|---|
| B1 | 六个内置渠道成为编译内预装插件（包名常量、贡献体、合成 manifest、Backends、Activate、Resolve、Set） | ✓ |
| B2 | 装配层 `ph.Load` 之后激活；`ErrDuplicate` 记 Info 跳过；`BuildChannel` 先读内核、未命中回落旧 map | ✓ |
| B3 | 删除旧 map 与回落；`RegisterChannel` 收编 | ✗（动 `registry.go`，独立评审） |

非目标：候选插件包（S-8）、渠道类型枚举扩展（S-5）、Router 实例名重名（S-6）、
`ChannelSpec` 凭证模型（S-7）、`install.strategy` 的灰度语义。

## 2. 裁定 D-1：内核胶水落在 `core/manager/biz/notify/`，不落在 `core/base/pkg/notify/kernel.go`

方案 §5.3 的骨架草图（与本任务描述同源）把 `Plugin* / channelPlugin / manifestFor /
Backends / Activate / Resolve / Set` 画在 `core/base/pkg/notify/kernel.go`。**这个落点
过不了仓库自己的边界闸门**，实测（`main` 干净树上加一个只 import `core/domain` 的探针
文件后跑 `make module-check`）：

```text
core/base/pkg/notify/zz_scratch_probe.go: shared_pkg imports
github.com/vincent-wuhan/opskeeper/core/domain (oxcore_domain), which no rule
in .go-arch-lint.yml permits
```

原因不是巧合，是规则：`.go-arch-lint.yml` 的 `shared_pkg`（`core/base/pkg/**`）
`mayDependOn = [shared_pkg, oxcore_ports, oxfloor]`，**不含 `oxcore_domain`**；而
`manifestFor` 必须用 `domain.SafetyL3` / `domain.ClassDestructive` /
`domain.ScopeAlertWrite` / `domain.TargetManager` 构造清单，`Policy()` 必须用
`domain.Scopes`。`core/base` 全模块今天对 `core/domain` 的生产 import 数为 **0**。

两个出路，取舍如下：

| 候选 | 做法 | 代价 / 失败模式 | 判定 |
|---|---|---|---|
| A. 放宽规则 | 给 `shared_pkg.mayDependOn` 加 `oxcore_domain` | 底座（45 个 domain 都压在它上面）从此认识治理词表；下次有人在 `core/base/pkg` 里引用 `domain.Alert` 时，这条授权已经在了——白名单的死条款比缺少白名单更坏 | 拒绝 |
| **B. 挪落点** | 胶水整体放 `core/manager/biz/notify/`，`notify` 只加一个读取视图 | 多一个小包；六个 factory 的**包装闭包**（每条 3 行）从 `notify` 包里挪到外面写 | **采用** |
| C. 留在装配根 | 全部写进 `cmd/opskeeper/notifier_wiring.go` | 与决策 326 的方向相反：住在 package main 的代码对两头测试不可见，而这块恰恰要测试钉住 | 拒绝 |

采用 B 的另外两条理由：

1. `core/manager/biz/notify/` 与 `core/manager/biz/aiops/datasource/` 同位（方案 §5.3
   自己给出的第二个选项），而 `manager_biz` 的 `mayDependOn` 同时含 `oxfloor` /
   `oxcore_domain` / `shared_pkg`——datasource 已经在用同一条授权。
2. 只有落在 `manager_biz` 里，`Policy()` 才能**从 `aiopsdatasource.ManagerPolicy()`
   派生**而不是抄一份（见 §4）；`shared_middleware`（`core/manager/middleware/**`）
   的白名单不含 `manager_biz`，放那边反而派生不了。

`core/base/pkg/notify` 因此只增加一个文件，内容是一个读取视图（§6），**不新增任何
import**——`make module-check` 是这条的验收。

### 2.1 关于"包级注入视图"的预先辩护

`core/floor/skill` 与 `core/base/pkg` 曾经因为包级 `Set*Resolver` 反向拿到
`manager/biz/setting` 的实现被 arch-lint 点名（`.go-arch-lint.yml` 头部注释）。本 change
在 `notify` 里加的 `InstallChannelResolver` 形状相似，三条区别写在这里，免得被当成同一
个问题的复发：

1. **方向相反**：那次是底座向上够到一个 BC 的实现；这次是装配根向下注入一个查找函数，
   接口声明在消费方（`notify.ChannelResolver`），`notify` 不知道也不 import 实现方。
2. **生命周期相反**：那次是运行期可换的服务定位器；这次是启动期一次性安装，进程存活
   期间不换，`InstallChannelResolver(nil)` 是回滚杠杆不是运行期开关。
3. **终点相反**：B3 删掉旧 map 之后，这个视图就是 `notify` 里**唯一**的渠道来源，不再
   是第二套注册表面是第一套的替身。

## 3. 裁定 D-2：六个 factory 是"复制"，不是"搬走"

任务描述与方案 §5.1 B1 都写"从 `registry.go` 的 map 原样搬进 `Backends`"。按字面执行
（把 map 清空）会让 B1+B2 期间的行为**变**，理由有三：

1. `BuildChannel` 的回落路径读的就是那个 map；`ph.Load` 之前、`Activate` 之前、以及
   任何没装配 notifier 的二进制（全部既有单测、`tests/e2e` 里不经过装配根的部分）都靠它。
2. `RegisteredChannels()` 从 map 派生；`registry_test.go` 的
   `TestRegisteredChannelsCoversBuiltins` / `TestRegisterChannelRefusesDuplicate` 断言
   六个内置类型在 map 里。
3. 本切片的硬约束本来就有"`registry.go` 一字不改"（前置切片的 C2，B3 才动它）。

所以 B1 期间六个 factory **在两处各有一份**：`registry.go` 的 map 原样不动，
`biz/notify` 的 `Backends` 里是逐字复制。代价是六条 3 行闭包短暂双写一个切片，B3 删
map 时一并消失；等价性测试（§8 的 I2）钉住两份必须产出同一行为，双写期间不会静默分叉。

## 4. 裁定 D-3：`Policy()` 必须在本 change 落地，且必须走放宽入口

这是本 change 最容易被漏掉的一块：**没有它，B2 在默认部署上直接启动失败。**

六个内置渠道的 manifest 是 L3（方案 §4.2 用 `safety.go` 原文论证过，评审已确认），
而 manager 宿主今天的 policy 是 `aiopsdatasource.ManagerPolicy()`：全局 `MaxSafetyLevel:
L2`、无 per-kind 条目。`Host.Admit` 用 `WidestSafetyCeiling()`（= L2）判包 → 六个 L3
manifest 全部被拒 → `Activate` 返回错误 → 按 §5 的裁定这不是 `ErrDuplicate`，是致命错误
→ `os.Exit(1)`。

所以 policy 派生不是 B2 的"可选增强"，是前置。形状：

```go
// biz/notify/policy.go
func Policy() kernel.Policy {
	p := aiopsdatasource.ManagerPolicy()
	scopes := make(domain.Scopes, 0, len(p.GrantedScopes)+1)
	scopes = append(scopes, p.GrantedScopes...)
	scopes = append(scopes, domain.ScopeAlertWrite)
	p.GrantedScopes = scopes
	return p.WithWiderKindCeiling(kernel.KindNotifier, domain.SafetyL3)
}
```

三个必须写死的细节：

1. **入口必须是 `WithWiderKindCeiling`，不是 `WithKindCeiling`。** 全局 L2 之上给
   notifier L3 是一次**放宽**；S-1 把写入方向拆成两个入口、写反方向 panic（
   `notifier-kernel-first-slice/design.md` §2.1 的裁定）。方案 §4.6 草图里那句
   `.WithKindCeiling(KindNotifier, L3)` 照抄会在启动时 panic——这不是笔误提醒，是
   语义裁定落地的直接后果。
2. **恰好两个 delta，写进测试**：`Policy()` 与 `ManagerPolicy()` 的差必须**只有**
   `KindCeilings[KindNotifier] = L3` 与 `GrantedScopes` 多一个 `alert.write`，
   别的字段（Target / MaxSafetyLevel / MaxBlastRadius / 其余四个 scope）逐位相等。
   `TestNotifierPolicyIsManagerPolicyPlusExactlyTwoDeltas` 把它钉住：派生面不许漂。
3. **为什么必须加 `alert.write`**：六个 manifest 的 `required_scopes` 是
   `[alert.read, alert.write]`（已评审的 `notifier-manifests/builtin-channels.yaml`），
   `ManagerPolicy()` 只授予 `alert.read`，缺这个 scope `Admit` 直接拒。

**实测**（干净树上的一次性探针，未入库）：拿一份 L3 / `[alert.read, alert.write]` 的
`opskeeper-notifier-slack` 合成 manifest，分别过三种 policy——

```text
ManagerPolicy()（今天）            → pig-ops.yaml: spec.required_scopes: not granted: alert.write
ManagerPolicy().WithKindCeiling(    → panic: WithKindCeiling("notifier", L3) widens that
  KindNotifier, L3)                    kind past L2; use WithWiderKindCeiling
本文的 Policy()（两个 delta）      → Admit 通过；KindNotifier/slack 激活成功
```

第一条说明**缺 scope 比缺 ceiling 先挡住**——只放宽安全级别不改 scope，六个包照样全灭；
第二条说明方案 §4.6 草图那行代码今天连编译后的第一行都跑不过；第三条是唯一活路。

### 4.1 两条治理后果，必须写进 PR 描述

policy 是**一个宿主一份**，不是 notifier 一份。所以这两条 delta 的作用面是整个 manager
宿主，不是六个通知包：

- **`alert.write` 变成宿主级授予**：一个 manager 目标的 L2 工具包今天申请 `alert.write`
  会被拒，改后会被授予。内核没有 per-kind scope 机制（S-1 只做了 per-kind **安全级别**），
  要收窄就得再来一次内核改动。本 change 接受这个面，理由：宿主早已授予 `db.read`，这
  不是第一份写权限；且六个清单的 ask 是评审定下的。把它记为开放问题（§11 Q2），
  触发条件是"真有第三方 manager 包来申请 alert.write"。
- **包级闸门从 L2 抬到 L3**：`Admit` 用 `WidestSafetyCeiling()`，notifier 条目让最宽值
  变成 L3。后果是 L3 包**过了包级闸门**、在 `Activate` 的 per-kind 检查处被拒。两种
  拒绝都不致命、都进 `Report.Refused`，运维看到的结局相同；差别只在拒绝发生在哪一关、
  报错文本是哪一条。这一点必须有测试（§9.3）钉住：**L3 包在其他 Kind 上依旧进不来。**

### 4.2 实测：`ErrDuplicate` 与非重复拒绝的分界

两类激活失败（一类是预期的让位、一类是必须致命的漂移），实测错误文本如下——它们是 §5 那条
"只有 `ErrDuplicate` 跳过"的依据：

```text
第三方已持有 notifier/slack，内置 slack 再激活：
  kernel: activating "opskeeper-notifier-slack": kernel: name already registered
  for kind: notifier/slack (held by plugin "opskeeper-notifier-slack-blocks",
  "opskeeper-notifier-slack" tried to take it)        → errors.Is(ErrDuplicate) == true

L3 包往 KindTool 落条目（widened policy 下）：
  kernel: activating "opskeeper-l3-tool": kernel: entry tool/shell: plugin is L3,
  policy ceiling for kind "tool" allows at most L2     → errors.Is(ErrDuplicate) == false
                                                       → 整包已回滚，IsLive == false
```

## 5. 裁定 D-4：装配顺序与 `ErrDuplicate` 的精确判定

```go
// cmd/opskeeper/main.go —— ph.Load 块之后，无条件执行
notifySet, nErr := notifybiz.Wire(ph.Kernel(), notifybiz.Builtins(), pluginLog)
if nErr != nil {
	log.Error("notification channel activation", slog.Any("err", nErr))
	os.Exit(1)
}
```

顺序是**语义**，不是风格：`ph.Load`（第三方包）先、一方包后。第三方变体（如
`opskeeper-notifier-slack-blocks` 注册 `slack`）先占住类型键，一方包撞上 `ErrDuplicate`
让位；反过来的顺序会让一方包永远赢，"配置选增强变体"这条出路就不存在了。

`ErrDuplicate` 的判定与处置，四条全部写死：

1. **用 `errors.Is(err, kernel.ErrDuplicate)`**。`Host.Activate` 把贡献体注册的错误包成
   `kernel: activating %q: %w`，`errors.Is` 仍能命中；字符串匹配不行。
2. **跳过时记 Info，不是 Warn**：这是预期的运维选择结果（配置选了变体），不是故障。
   日志必须点名让位方与持有方——`ErrDuplicate` 的文本自带持有者包名
   （`held by plugin %q, %q tried to take it`），照抄进 Info 的 err 字段即可。
3. **只有 `ErrDuplicate` 跳过，其余一律致命**。一个 per-kind ceiling 拒绝、一个 scope
   拒绝、一个"plugin is already active"，都意味着我们自己的 manifest 或 policy 漂了，
   静默跳过等于六个渠道里少了一个而启动日志看着正常——这正是本方案要消灭的那种静默
   故障。`os.Exit(1)`。
4. **跳过只影响那一个类型**：六个类型是六个独立包，各自 `Activate`，互不回滚。

已知且**不改**的一条：变体部署上，内核会对让位的一方包打一条
`plugin activation failed; rolled back` 的 **Warn**（`kernel/host.go` Activate 的回滚
路径）。那不是本 change 的日志，改它要动内核；变体部署不是默认部署，不影响 §7 的日志
契约。写在这里是为了防止有人后来"顺手修掉"它。

`Wire` 的内部顺序：逐类型 `Activate`（按 §5 的判定）→ `Resolve(reg)` →
`notify.InstallChannelResolver(set)` → 一条 Info 汇总。**安装放在最后**：任何一步出错
都不该留下一个装了一半的全局视图。

## 6. 裁定 D-5：`BuildChannel` 的读取顺序

`core/base/pkg/notify` 新增（新文件，`registry.go` 不动）：

```go
type ChannelResolver interface{ Factory(typ string) (ChannelFactory, bool) }
func InstallChannelResolver(r ChannelResolver)   // nil = 卸载，回到迁移前路径
```

`BuildChannel(spec, log)` 的读取顺序与日志：

| 状态 | 行为 | 日志 |
|---|---|---|
| 未安装视图 | 只读旧 map | **无**（与迁移前逐位相同——这是全部既有单测与非装配二进制的路径） |
| 已安装、类型命中 | 用内核视图的 factory | 无 |
| 已安装、类型未命中 | 回落旧 map | **Warn 一条**，每个类型只打一次（`slog` 结构化字段 `type` + `hint`，hint 说明这是 B3 要删的回落路径） |
| 两边都没有 | `unknown channel type %q` | 文本与今天**一字不差** |

两个必须处理的细节：

- **`log` 可能为 nil**。生产调用方 `BuildSenderFromChannel` 传的就是 `nil`
  （`core/manager/biz/alert/usecase.go:1497`）。回落 Warn 因此**不能**走 `BuildChannel`
  的参数 logger，要走视图自带的 logger（`Wire` 装进 `Set` 的那个）；没有视图就没有
  Warn，所以永远不会有 nil 解引用。
- **`Set` 是启动期快照**。`Resolve` 在 `Wire` 里读一次，`BuildChannel` 之后不再查
  registry。理由：manager 的插件集是启动期一次性的（`ph.Load` 只跑一次，没有重载路径），
  每次投递都过一遍 registry 锁是热路径上的无谓开销。边界写清楚：**运行期 `Deactivate`
  一个 notifier 包不会改变 `BuildChannel` 的答案，要重启才生效。** 哪天真出现运行期
  重载，这里要跟着改（记入 §11 Q3）。

`Set` 的形状（`Holder` 是给日志与诊断用的，`Factory` 是给 `BuildChannel` 用的）：

```go
type Set struct{ /* typ -> {factory, plugin} */ }
func (s Set) Factory(typ string) (notify.ChannelFactory, bool)
func (s Set) Holder(typ string) (plugin string, ok bool)
func (s Set) Types() []string   // 排序
```

`Set` 结构上满足 `notify.ChannelResolver`，所以 `InstallChannelResolver(set)` 不需要
适配器，也不需要 `notify` 认识 `biz/notify`。

## 7. 裁定 D-6：默认部署启动日志的精确契约

任务描述的验收写"默认部署启动日志与改动前一致"。**按字面它不可满足**：把六个包激活进
内核，内核就会为每个包打一条 `plugin activated`（`kernel/host.go`），这六条是新增的。
把验收写成一句不可满足的话，实现者只有两条路——要么做不到，要么为了做到而不打日志，
后者更糟。所以这里把它改写成**可判定的白名单**：

> 默认部署（`OPSKEEPER_MANAGER_PLUGINS_DIR` 为空或不存在）的启动日志：**改动前出现的
> 每一行，改动后逐字节再次出现、顺序不变、属性不变**；新增行**只有**白名单里的
> 7 条 Info（6 条内核 `plugin activated` + 1 条装配层 `notification channels
> activated`），**没有新增 Warn / Error**。

顺带一条**不会**发生的变化，写清楚免得被当成遗漏：既有的
`control-plane plugin capabilities` 那条日志打在 `ph.Load` **之前**（main.go 现序），
notifier 激活在其后，所以那一行的 `plugins` / `kind_counts` **不含** notifier 条目、
逐字节不变。运维要看 notifier 拥有情况，读新增的那条汇总（它按类型给出持有包，比
inventory 的计数更可读）。

验收方式两条，缺一不可：

1. **测试钉白名单**：`Wire` 用一个捕获型 `slog.Handler` 跑一遍，断言产生的记录恰好是
   1 条 Info（`notification channels activated`）+ 6 条来自内核的 Info
   `plugin activated`，且 `Level >= Warn` 的记录数为 0。
2. **手工对照一次**：改动前后各启动一次 manager（无插件目录），`diff` 两份日志，把
   结果贴进 PR 描述。这一步不能被测试替代，因为它守的是"既有行不变"那一半。

## 8. 七条不变量与验证

方案 §5.2 的六条 + 补充一条，共七条。每条要么有测试，要么有论证，要么两者都有：

| # | 不变量 | 验证 |
|---|---|---|
| I1 | **配置面不变**：`specsFromConfig` 读的还是 `config.NotificationConfig` 四个子结构，产出 `[]ChannelSpec`；DB 渠道行的 `channel_type` 取值不变 | 论证（这两处一行不改）+ 既有 `TestNewFromConfigBuildsTheSameChannels` 零修改通过 |
| I2 | **工厂实现不变**：六个 factory 逐字复制，payload / 签名 / header 一处不改 | `TestTheSixBuiltinChannelsResolveIdenticallyThroughTheKernel`（§9.1） |
| I3 | **解析错误语义不变**：未知类型 `unknown channel type %q`、空名 sender `built an unnamed sender` 两条文本一字不改，只是来源从 map 查不到变成视图查不到 | 新测试：同一条非法 spec 分别走三条路径（未装视图 / 装了视图命中 / 装了视图未命中），断言错误文本全等 |
| I4 | **部分失败不拖垮整体**：`NewRouterFromSpecs` 的跳过并 Warn、逐渠道独立 timeout、`errors.Join` 聚合不动 | 既有 `TestUnknownChannelTypeIsSkippedNotFatal` 零修改通过 + 装了视图后的同形用例 |
| I5 | **投递留痕不动**：`notification_deliveries` 仍由宿主写，`RetryWorker` 不碰，插件不能伪造也不能抑制 | 论证（`core/manager/biz/alert` 一行不改，`git diff --name-only` 兜底）+ 既有 channel_sender / retry 测试零修改通过 |
| I6 | **既有回归零修改**：`notify` 三个测试文件、`tests/e2e/notify_*`、`testenv/fakes.go` 一字不改仍全绿 | `git diff --name-only` 守卫 + 全量测试 |
| I7 | **派生面与既有构造点零改动**：`ManagerPolicy()` / `adapterPolicy()` / edge `Profile.Policy()` 逐位不变；`notifier.Policy()` 与 `ManagerPolicy()` 恰好差 §4 的两个 delta | 既有三个"逐位不变"测试零修改通过 + `TestNotifierPolicyIsManagerPolicyPlusExactlyTwoDeltas` |

第八条不算进七条（它不是"不变"，是"新增的边界"），但同样有闸门：
**`core/base/pkg/notify` 不新增 `core/floor/kernel` / `core/floor/pluginmanifest` /
`core/domain` 的 import**——`make module-check` 就是它的验收。

## 9. 测试设计

### 9.1 等价性测试（I2 的锚点，任务点名要的名字）

`TestTheSixBuiltinChannelsResolveIdenticallyThroughTheKernel`，放在
`core/manager/biz/notify/`。对六个类型各跑一次：

1. 旧路径：`notify.BuildChannel(spec, nil)`（视图未安装，走 map）。
2. 新路径：`Builtins()` 经 `Wire` 进一个真 kernel host，再 `BuildChannel`（视图已安装）。
3. 两个 Sender 打到**同一个**捕获型 `httptest.Server`，断言全等：`Name()`、HTTP 方法、
   路径、**请求体字节**、签名相关 header（飞书在 body 顶层、钉钉在 URL query、generic
   webhook 的 `X-OpsKeeper-Signature`）。
4. 收尾 `InstallChannelResolver(nil)`，保证不污染同包其他用例。

签名断言直接复用 G4 e2e 已有的口径，不另发明一套。

### 9.2 变体让位（本切片的语义核心）

`TestAThirdPartyVariantOwningSlackWinsOverTheBuiltinAndBootStillSucceeds`：

1. 先往同一个 kernel host 注册一个名为 `opskeeper-notifier-slack-blocks` 的假第三方包，
   占住 `KindNotifier/slack`（贡献体是一个可识别的假 factory）。
2. 再 `Wire(Builtins())`：**不得**返回错误；`slack` 一条 Info 让位，其余五个激活。
3. `BuildChannel({Type:"slack"})` 拿到的是**假第三方**的 factory——内核优先于旧 map，
   这正是迁移的意义；不装视图时同一个 spec 拿到的是内置实现，两者必须有断言区分。
4. 让位那条日志是 Info 且点名持有者；`Level >= Warn` 的新增记录只允许内核自己的那条
   回滚 Warn（§5 第 4 条）。

### 9.3 其余用例

| 用例 | 断言 |
|---|---|
| `TestNotifierPolicyIsManagerPolicyPlusExactlyTwoDeltas` | §4 的两个 delta，其余逐位相等 |
| `TestAnL3PackageIsStillRefusedInOtherKindsUnderTheNotifierPolicy` | L3 包过包级闸门、在 KindTool 上被拒、整包回滚、`ph.Load` 报 Refused 而不是致命 |
| `TestWireLogsExactlyTheWhitelistedRecords` | §7 的白名单 |
| `TestBuildChannelFallsBackToTheLegacyMapOncePerType` | 装了视图、类型只在旧 map：能建出来 + Warn 恰好一条（重复调用不重复打） |
| `TestInstallNilRestoresThePreMigrationPath` | `InstallChannelResolver(nil)` 后行为与未装视图逐位相同（回滚杠杆可用） |
| 既有 e2e（G1 / G3 / G4 / G8） | 零修改通过；G4 的签名断言继续钉"请求形状没变" |

## 10. 硬约束（C1–C8）

| # | 约束 | 验 |
|---|---|---|
| C1 | `core/floor/kernel/kind.go` 一字不改 | `git diff --name-only` |
| C2 | `core/base/pkg/notify/registry.go` 一字不改 | 同上 |
| C3 | `core/base/pkg/notify` 不新增 kernel / pluginmanifest / domain import | `make module-check` |
| C4 | `datasource.go`（含 `ManagerPolicy()`）、`sources_plugin.go`（含 `adapterPolicy()`）一字不改 | 同上 + 既有逐位不变测试 |
| C5 | 告警派发主流程（`MaybeNotify` / `FinishDelivery` / `RetryWorker`）不改 | `git diff --name-only` |
| C6 | `plugins/` 不动；不产任何插件包 | 同上 |
| C7 | 不新增 Kind；manifest schema 不加字段 | `git diff --name-only` |
| C8 | 既有 notify / e2e 测试文件零修改（I6） | `git diff --name-only` |

## 11. 风险与开放问题

| 级 | 风险 | 缓解 |
|---|---|---|
| 中 | 双写期两份 factory 分叉（D-2） | I2 等价性测试；B3 删 map 收口，两份并存只有一个切片的窗口 |
| 中 | `alert.write` 宿主级授予被第三方 manager 包申请（§4.1） | 记为 Q2；触发时再议 per-kind scope，不为尚无用例扩内核 ABI |
| 低 | `Set` 快照与运行期 `Deactivate` 不一致（§6） | manager 无重载路径；重启生效；记为 Q3 |
| 低 | 新增符号触发 deadcode 棘轮 | `make deadcode-ratchet-check`，需要抬基线时在同一提交写明理由（前置切片已有先例） |
| 低 | 变体部署上内核的回滚 Warn 被误读为故障（§5） | 文档写明"预期"；不改内核 |

**Q1（需评审确认）**：D-1 的落点偏差——与任务描述和方案 §5.3 草图不一致，依据是
`make module-check` 的实测拒绝。方案 §5.3 自己标了"骨架建议（不在本任务合入）"，且给了
`core/manager/biz/notify/` 这个同位选项，所以这更像草图没跑过闸门，而不是推翻已评审
结论。**若评审坚持原落点，必须同时给 `shared_pkg` 加 `oxcore_domain` 授权并写明理由，
那是架构边界变更，不是本 change 的实现细节。**

**Q2**：`alert.write` 是否需要 per-kind scope 收窄（§4.1）。现在不做。

**Q3**：运行期插件重载出现时，`Set` 快照要改成活查询（§6）。现在不做。

## 12. 与方案文字的偏差清单

| # | 方案 / 任务原文 | 本 change | 理由 |
|---|---|---|---|
| 1 | §5.3 草图：胶水在 `core/base/pkg/notify/kernel.go` | `core/manager/biz/notify/` | §2（module-check 实测拒绝）；方案自己给了这个备选落点 |
| 2 | §5.1 B1"从 registry.go 的 map 原样**搬**进" | 复制，map 不动 | §3（否则 B1+B2 期间行为必变） |
| 3 | §4.6 草图 `.WithKindCeiling(KindNotifier, L3)` | `WithWiderKindCeiling` | §4（S-1 的方向裁定，写反 panic） |
| 4 | §4.6 草图 `NotifierPolicy()` 是独立构造 | 从 `ManagerPolicy()` 派生 + 恰好两个 delta 的测试 | §4（不抄字段，派生面不漂） |
| 5 | 任务验收"启动日志与改动前一致" | 白名单式契约（§7） | 字面不可满足；白名单可判定、可测试 |
