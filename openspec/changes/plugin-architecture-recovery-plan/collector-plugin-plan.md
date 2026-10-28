# 4.7 collector 边缘采集插件方案（闸门审计 + 内置六采集器迁移 + 资源治理）

> 状态：**方案 v0.1（待评审）**。对应 `tasks.md` 4.7；评审通过前不勾选 4.7。
>
> **评审裁决（2026-10-28，用户确认）**：D1 选 **D1-a**（采集器包自带 endpoint_kind，resolver 按包名→kind→系统设置三层解析，保留 loki.url 自动重定向；D1-b 仅作落地前过渡）；D2 选**收窄**——用已合并的 WithKindCeiling 机制（PR #106）给 edge 宿主 `KindCollector` 收窄到 L1，消除 saas 档放行 L3 采集器的治理失真；D3（spec.collector schema）维持独立 change 推迟。
>
> 基线：`main@483783b4`（本任务打开时的 HEAD，即 4.5 已合并的 `notifier-plugin-plan.md` 之后）；与 4.1 `business-domain-mapping.md`、4.2 结论保持口径一致。
>
> 范围：**仅方案产出**。本文档回答三件事：edge 侧第三方 collector 的真实装配路径上有哪些闸门（§3）、六个内置采集器怎么在默认部署行为不变的前提下变成编译内预装插件（§5）、进程模型与资源上限怎么同时有 manifest 声明位和宿主执行点（§6）。§10 的实现切片建议**不**在本任务内合入；任何对 `core/floor/kernel/kind.go`、`core/edge/pluginhost/host.go` 主流程、`core/edge/plugins/` 结构、`core/domain/plugin.go` 的真实改动，必须另开 change 并独立评审。

## 1. 目标与边界

### 1.1 目标

| # | 目标 | 判定 |
|---|---|---|
| A | 把"`collector` 通道就绪但 0 贡献"这句话拆开：逐道核实第三方包从落盘到真正跑起来要过的每一道闸门，每道写清代码位置、后果、修复归属 | §3 |
| B | 六个内置采集器从"编译内直接注册"迁到"编译内预装插件、经同一道门 admission"，默认部署行为不变 | §5 |
| C | 进程模型、CPU/内存/文件句柄上限、日志采样与脱敏同时落到 manifest 声明位与宿主执行点，保持"可审查" | §6 |
| D | 第一批第三方候选 1–2 个，每候选 manifest / 注册 / 测试 / 回滚齐备 | §7、§8 |

### 1.2 一个必须先说清的框架判断：这里"第三方插件"是什么

`design.md` 的 Non-Goals 写明：**不引入运行时加载第三方 Go 代码的机制；实现仍通过 manifest 声明与编译内贡献注册**。`core/manager/middleware/pluginhost/host.go` 的包注释把这个模型说得更直白：manifest 从磁盘上准入（可评审、可签名），贡献体按包名在一个**装配层填好的注册表**里查（运维选择编译进去的代码）。

因此 collector 扩展点上"可装第三方采集器"的准确含义是：**准入、清单、回滚、配置面是按部署、按节点动态的；代码是编译期注册的**。`RegisterActivator(name, func() kernel.Contribution)` 就是那个编译期接缝——节点二进制带一组按包名索引的采集器构造器，目录里的 manifest 决定这一部署承认哪些。

这个判断是 §3 全部闸门审计的前置：它决定了"注册后不可见"在 edge 侧的形态与 adapter 侧**不同**（§3.1），也决定了资源治理的执行点只能是宿主代码而不是某种沙箱（§6）。

### 1.3 与 4.1 §3 / §4 ④ / §6、4.2 的边界

- **4.1 §3**：`collector` 状态"通道就绪，0 贡献"——证据 `core/edge/pluginhost/host.go`（`CollectorContribution` 与 sink 已打通）与 `core/edge/plugins/*`（内置采集器仍编译内运行）。本方案的内容就是消掉这一行的状态。
- **4.1 §4 ④ 可观测"边缘采集"行**：`迁移为插件（候选）→ 4.7`。本方案不改这一行的归属。
- **4.1 §6 缺口表**："`collector` 无贡献，内置采集器仍编译内 → 观测、Edge → 4.7"。本方案收口该行。
- **4.1 §4 补充表"Edge 执行面"行**：`tool`、`collector` → 4.7。本方案只做其中 `collector` 半边；`core/edge/changewatcher`（变更监听）同属 Edge 执行面但 4.1 未把它映射到 collector，本方案按"不扩映射"处理，只在 §9.3 记一笔。
- **4.2 四分类**：collector 归"迁移为插件（候选）"。本方案只推进"迁移"分支，不触碰"保留在 core / 暂缓迁移 / 淘汰"三类。
- **内核契约**：`core/floor/kernel/kind.go` 的 `KindCollector`（"edge telemetry collector… the edge half of `KindDatasource`"）复用，**不新增字符串**，六元封闭集合保持。

### 1.4 非目标

- 不写实现代码；§10 只给切片与禁项。
- 不改 `core/floor/kernel/kind.go`（六 Kind 封闭集合）。
- 不改 `core/edge/pluginhost/host.go` 的 `Load` 主流程（§5.3 说明迁移如何用一个**新文件**加方法，避免触碰它）。
- 不动 `core/edge/plugins/` 目录结构与六个采集器的实现体。
- 不做 datasource / adapter / notifier / provider（4.3–4.6）。
- 不引入运行时加载第三方 Go 代码（见 §1.2）。
- 不把 `core/edge/collector`（`CollectAll` 用的 embedded/scrape 采集器）纳入本次迁移——那是 `edgebiz.Collector` 接口的另一条链路，与 `plugins.Plugin` 无关，混进来会把两个不同的"collector"字面量当成一个。

## 2. 现状证据：edge 采集链路全貌

```text
节点上的采集器有两条互不相交的链路，本方案只动第一条：

  ① plugins.Plugin 链（本方案对象）
     cmd/opskeeper-edge/main.go:345  registered := []edgeplugins.Plugin{ logs, traces,
                                    metrics, custommetrics, databasemetrics,
                                    hostmetrics, procmetrics }   ← 编译内字面量切片
        │  supervisor.Register(p)（main.go:391，逐个直接注册，不经任何 admission）
        ▼
     core/edge/plugins/supervisor.go  Supervisor.Run → reconcile
        │  desired ← ConfigFetcher.Fetch()
        │  （启动/停止/重配置，崩溃退避在 SubprocessPlugin 内部）
        ▼
     core/edge/plugins/subprocess.go  子进程采集器：exec + O_APPEND 日志 + 退避重启
        或  plugins.Plugin 的进程内实现（goroutine，main.go 的 metrics/custommetrics）
        ▼
     PluginHealth → 心跳上送（main.go:436 agent.SetPluginHealthFn）

  ② edgebiz.Collector 链（不属于本方案）
     main.go:109 buildCollector → core/edge/collector{Embedded,Scraper,Composite}
        → edgesvc.RegisterWithCollector → HostInfo / GetProcessList / CollectAll

  ③ 应当存在而目前断开的第三条：第三方包经 pluginhost
     cmd/opskeeper-edge/main.go:286 edgePluginHost := edgepluginhost.New(dir, policy, sink, log)
        → Load(ctx) 遍历 OPSKEEPER_EDGE_PLUGINS_DIR
        → Admit → RegisterActivator 查表 → CollectorContribution → KindCollector
        → sink（deferredCollectorSink，main.go:395 才指向 supervisor）
     这条链的每一环都写好了，但生产代码没有任何一行调用 RegisterActivator，
     所以它从未携带过一个真实的采集器（§3.1）。
```

配置面的另一半在 manager：`edge_plugin_configs` 表 + `get_plugin_configs` tunnel RPC + `EndpointResolver`。它是 §3.3 / §3.4 两道闸门的所在。

### 2.1 现状事实清单

| # | 事实 | 证据 |
|---|---|---|
| F1 | 节点注册了 **7** 个编译内采集器，不是 6 个：issue 点名的六个加上 `metrics`（进程内 scraper，走 `push_prom_samples` tunnel RPC） | `cmd/opskeeper-edge/main.go:345-372` |
| F2 | `plugins.Plugin` 接口的进程模型是二元的：子进程（`SubprocessPlugin`，每采集器一个子进程）或进程内 goroutine | `core/edge/plugins/plugin.go:21-24`、`subprocess.go` |
| F3 | `CollectorContribution`、`CollectorSink`、sink 注册尾段、`Report.NoActivator` 全部已实现且有测试 | `core/edge/pluginhost/host.go:89-105, 212-220`、`host_test.go` |
| F4 | 子进程采集器的资源约束现状：**无 CPU 上限、无内存上限、无文件句柄上限、无日志轮转/采样/脱敏、子进程继承父进程全部环境变量** | `subprocess.go:229-277`（`runOnce` 无任何 `SysProcAttr`/rlimit/裁剪） |
| F5 | 本仓对"声明了却不执行的上限字段"有**测试强制**的反对纪律：`TestTheToolLimitVocabularyIsClosed` | `core/floor/pluginmanifest/limits_test.go:84-131` |
| F6 | 采集器子进程的本地日志文件 `O_APPEND` 无上限增长，落盘路径含渲染出的凭据 | `subprocess.go:237-244`、`core/edge/plugins/logs/render.go:33-40` |
| F7 | manager 侧配置面的插件名是**闭集**：`IsKnownPluginName` 只认 8 个名字 | `core/manager/model/edge/plugin_config.go:68-73` |

## 3. 现状闸门审计（本方案的首要产出）

参照 4.4 `adapter-plugin-plan.md` §3 的 G1/G2 方法：先判定"第三方注册后是否真的可见 / 可装配"，再判定 scope ⊆ 宿主 Policy。edge 侧的答案是**五道闸门**，其中两道（C1、C3）合起来构成"装得上也跑不起来"。

先给结论表，再逐道展开：

| 闸门 | 一句话 | 位置 | 严重度 | 修复归属 |
|---|---|---|---|---|
| **C1** | 生产代码零 `RegisterActivator` 调用：第三方采集器包**没有任何路径**能产生 `KindCollector` 条目 | `core/edge/pluginhost/host.go:192-204` + `cmd/opskeeper-edge/main.go:286` | **阻塞** | edge 装配层（`main.go`），宿主代码本身不需要改 |
| **C2** | 六个内置采集器直接 `supervisor.Register`，**不经内核**：`Inventory()` 里没有 `collector` 计数，回滚没有包名可依 | `cmd/opskeeper-edge/main.go:345-391` | 高（治理缺口，非功能故障） | 新增 `preinstalled` 装配切片（§5.3） |
| **C3** | 配置面两侧都是闭集：manager 拒绝未知名字，edge 把不在名单里的采集器**强制合成为 disabled** | `core/manager/model/edge/plugin_config.go:68`、`core/manager/biz/edge/plugin_config.go:150,270,185`、`core/edge/plugins/config_tunnel.go:88-95` | **阻塞**（修了 C1 也在 60 秒内被停掉） | manager 配置面 + edge 装配层各一半 |
| **C4** | `EndpointResolver` 只认 `logs` / `traces` 两路：推（push）型第三方采集器拿不到数据面 endpoint | `cmd/opskeeper/main.go:4307-4333` | 中（只挡 push 型，不挡 pull 型） | manager 装配层 |
| **C5** | 资源与日志：**声明位与执行点双双不存在**，且子进程继承含 `OPSKEEPER_EDGE_SECRET_KEY` 的环境 | `core/edge/plugins/subprocess.go:229-277`、`core/domain/plugin.go`（无 collector 块） | 高（issue 的硬要求落在这一道） | `core/domain` schema + `SubprocessPlugin` 执行点（§6） |

### 3.1 C1：`RegisterActivator` 缺失——edge 版的"注册后不可见"

4.4 的 G1 是"`Activate` 成功 ≠ 可用"：`Sources()` 读了注册表却只按一方字面量过滤，第三方条目进了表也出不来。edge 侧的形态**不同且更早**：

`core/edge/pluginhost/host.go:192-204` 的读回分支：

```go
act, ok := activators[name]
if !ok {
    // No registered activator. A package that declares tools still
    // has something to contribute — its tool declarations...
    if len(p.Manifest.Spec.Tools) == 0 {
        rep.NoActivator = append(rep.NoActivator, name)
        continue
    }
    act = toolPackageActivator(p)
}
```

生产代码里 `RegisterActivator`（`host.go:144`）**一次都没有被调用**——全仓检索只有 `core/manager/middleware/pluginhost` 与 `core/edge/pluginhost` 的测试文件调用它。于是第三方采集器包在 `OPSKEEPER_EDGE_PLUGINS_DIR` 里的命运只有两种：

- 不声明 `spec.tools` → 进 `Report.NoActivator`，被跳过，内核表里什么都没有；
- 声明了 `spec.tools` → 被当成**工具包**激活，注册的是 `KindTool` 条目，`KindCollector` 依然为空。

两种都不产生采集器。`host.go` 包注释那句 "Now a collector is a package a node admits through the same gate a tool package already passes" 描述的是**已备好的机制**，不是已接通的路径——与 4.4 §3.1 里 `sources_plugin.go:87-88` 那句注释的处境同构：注释写的是意图，代码里没有对应的调用方。

**与 adapter G1 的差别要写清，因为它决定修复归属不同**：adapter 的闸门在**宿主读回逻辑**里（读到了但被过滤），修复要动 `toolset` 包；edge 的闸门在**装配层**里（宿主逻辑正确，只是没人往表里填），修复不动 `pluginhost` 包，落在 `cmd/opskeeper-edge/main.go` 的装配段——这正是本方案把 C1 的修复归属判给装配层、并把 §10 的 S-1 切片放在那里的原因。

**后果链**（与 4.4 §3.1 的表格同形）：

| 消费者 | 位置 | 后果 |
|---|---|---|
| `sink` 注册尾段 | `host.go:212-220` | 循环体从不执行，supervisor 收不到第三方采集器 |
| `Inventory()` / `/v1/plugin-capabilities` | `cmd/opskeeper-edge/main.go:305-325` | `kind_counts` 永远没有 `collector` 键 |
| `Deactivate`（按包名回滚） | `core/floor/kernel/host.go` | 无条目可摘，第三方采集器没有包级回滚路径 |
| `ErrDuplicate` 互斥 | `core/floor/kernel/registry.go:73-75` | 从未有机会触发——同信号双注册的保护从未被走到 |

### 3.2 C2：内置六采集器编译内装配，与内核注册无关

`main.go:345-372` 构造七个采集器，`main.go:391` 逐个 `supervisor.Register(p)`。这条路径**完全不经过** `kernel.Host.Admit` / `Activate`：

- `edgePluginHost.Inventory()` 因此**永远不包含任何 collector 条目**。节点本地的 `/v1/plugin-capabilities` 端点（`main.go:305`）存在的目的就是回答"这个节点能做什么"，而采集器——节点上唯一真正长期跑着的遥测面——在它的答案里缺席。这与 `pluginhost` 包注释宣称的对称性（"the third-party path is exercised on every boot"）不符：工具包路径每次启动都走（目录里有 `pig-ops` 包时），采集器路径一次都没走过。
- 回滚没有名字。`Host.Deactivate(ctx, name)` 按包名摘条目；编译内采集器的唯一控制杆是 manager 侧的启用开关（`edge_plugin_configs` 行），那是配置回滚，不是包回滚，两者在审计里语义不同。
- 安全级别、scope、能力声明**对内置采集器根本不存在**。今天"logs 采集器可以读节点上任意文件"这件事没有任何一份可评审的声明记录它——它只存在于 `render.go` 的模板里。C2 因此不只是"没接上"的问题：它是 §5 迁移方案存在理由的直接证据。

**一个刻意的边界澄清**：这不是"内置采集器做错了"。它们的实现体（`SubprocessPlugin` / `CollectorSpec` 声明）是本方案明确不动、且迁移后继续原样使用的东西。C2 指出的是**装配路径**缺一道 admission，不是实现有缺陷。

### 3.3 C3：配置面两侧都是闭集——"进得了注册表，过不了第一个 reconcile"

这是五道闸门里最锋利的一道，因为它让 C1 的修复**单独无效**。

**manager 侧**（两处）：

- `core/manager/model/edge/plugin_config.go:68-73` 的 `IsKnownPluginName` 是 8 个名字的 switch（含一个并无 edge 实现的 `profiles`）。`PluginConfigUC.Set` 在 `core/manager/biz/edge/plugin_config.go:185` 用它拒未知名字：`unknown plugin %q`。**运维无法为一个第三方采集器创建启用行。**
- `ListForUI`（`:150`）与 `FetchForEdge`（`:270`）都遍历**硬编码的** `knownPlugins` 字面量切片。就算表里 somehow 存在第三方名字的行，它也不会出现在 UI 列表，更不会被送上网线。

**edge 侧**（决定性的一击）：`core/edge/plugins/config_tunnel.go:85-95`

```go
for name, entry := range resp.Configs {
    if !known[name] { continue }        // ← 名单外的条目直接丢弃
    out[name] = PluginConfig{...}
}
// Plugins manager didn't mention default to disabled
for _, name := range t.knownPlugins {
    if _, ok := out[name]; !ok {
        out[name] = PluginConfig{Enabled: false, EdgeID: edgeID}   // ← 合成 disabled
    }
}
```

`knownPlugins` 来自 `main.go:383` 的 `NewTunnelConfigFetcher(client, pluginNames)`，而 `pluginNames` 来自 `main.go:345` 那个编译内切片。注意第二段循环的方向：**名单内的名字缺行会被合成 `Enabled=false`**，而名单外的名字在第一段就被丢弃——于是第三方采集器拿到的是"没有配置"，`supervisor.reconcile`（`supervisor.go:180`）按 `!hasCfg || !desCfg.Enabled` 分支停掉它。

合起来的后果链是：假设 C1 已修好、第三方采集器已经注册进 supervisor——它会在**最多一个 reconcile 周期（60s 安全网轮询）内被停止**，且 `PluginHealth` 里只有 `stopped`、没有原因。这比"不可见"更糟：不可见至少在 `/v1/plugin-capabilities` 里是诚实的空，而这个状态是**注册表里有、运行时里死、健康面上无解释**。

**修复归属要拆两半**：manager 侧的名字闭集是治理面（`IsKnownPluginName` 换成"已准入的 edge 包名字集"需要一个来源，本方案判给 manager 配置面 + 一个新的"节点已装包清单"上报，见 §10 S-4）；edge 侧的 `knownPlugins` 过滤则是防御性正确的（防 manager 幽灵返回），**不应该简单删掉**，而应该把名单来源从"编译内切片"换成"supervisor 实际注册的采集器名"——这个来源 supervisor 自己就有（`plugins` map），换成它之后过滤语义保留、覆盖面自动正确。这一半归 edge 装配层。

**附带一个好消息**（写下来是为了避免误读）：`pluginDefaultEnabled`（`plugin_config.go:130-138`）只对五个一方名字给默认开启，第三方名字天然落 `false`。这是正确的 fail-closed，C3 的修复**不需要**动它。

### 3.4 C4：`EndpointResolver` 只认两路——push 型候选被挡

`cmd/opskeeper/main.go:4307-4333` 的 `pluginEndpointResolver.Endpoint` 是一个只认 `"logs"` 和 `"traces"` 的 switch，其余名字一律返回空串。`PluginConfig.Endpoint` 因此对任何第三方 push 型采集器为空。

- **不影响的形态**：pull 型（hostmetrics / procmetrics 这类在本机暴露端口、由 manager 侧 Prometheus 抓取的）不需要 endpoint。这直接影响了 §7 候选 1 的选型——它是刻意的 pull 型。
- **影响的形态**：push 型（要往 `/loki/api/v1/push`、`/v1/traces` 或任何数据面写的）。§8 候选 2 是 push 型，所以它是琥珀，需要决策 D1。
- **一个不需要 schema 变更的缓解**：`PluginConfig.Spec` 是自由格式 map，endpoint 可以作为部署参数由运维填进 spec（候选 2 的 manifest 注释里写了这条折中）。它的代价是 endpoint 出现在配置行而不是系统设置里——`loki.url` / `tempo.url` 在 Integrations UI 被改后自动重定向所有边缘的那条链路（`pluginEndpointResolver` 的两层解析本来就是为了这个）不会覆盖到它。D1 裁决时要带着这条代价一起裁。

**修复归属**：manager 装配层。正确形状不是给 switch 加 case，而是让每个采集器包**自带 endpoint 声明**（要哪个数据面、哪条路径），resolver 按包名查表——这与 C3 的"名字来源"是同一个来源，建议在同一个切片里做。

### 3.5 C5：资源与日志——声明位与执行点双双不存在

issue 的硬要求是"保持进程模型、日志和资源上限**可审查**"。现状是四个"不存在"：

| 维度 | 现状 | 证据 |
|---|---|---|
| **CPU** | 无上限。子进程继承 edge 进程的调度权 | `subprocess.go:231`（`exec.CommandContext`，无任何约束） |
| **内存** | 无上限。全仓唯一的内存约束讨论在 `core/domain/plugin.go:254-275`，结论是**当前隔离粒度下无法按 tool 执行，所以不加字段** | 同上 |
| **文件句柄** | 无上限，且未设 `Pdeathsig`——edge 崩溃时子进程会成孤儿 | `subprocess.go:231-235` |
| **日志** | `O_APPEND` 无上限增长，无轮转、无采样、无脱敏；宿主日志与子进程日志是两个不相通的世界 | `subprocess.go:237-244` |
| **环境** | 子进程继承父进程**全部**环境变量，含 `OPSKEEPER_EDGE_ACCESS_KEY` / `SECRET_KEY` | `exec.CommandContext` 默认行为；对照 `core/edge/cmdpolicy/sandbox.go:31`（工具执行会洗成 PATH/LANG/LC_ALL） |

最后一条值得单独说：一个从未声明 `host.read` 的第三方采集器，可以从自己的环境里读到 edge 的隧道凭据。这与 manifest 的 scope 承诺直接矛盾——scope 说"宿主只注入声明的 scope 的凭据"（`core/domain/plugin.go:52-54`），而子进程继承环境这条路径绕过了那句话。**对工具执行，本仓已经洗环境；对长期运行的采集器子进程，没有。** 这是 C5 里最具体的一条暴露面，也是 §6.4 的第一项。

还有一个现成的礼物：`subprocess.go:303-307` 有一段被 `var _ =` 压住的 `captureLine`，注释明说它是"future 'tail subprocess output to opskeeper-edge structured log' 的 plumbing point"。**宿主已经预留了把子进程输出接进宿主结构化日志的那个点**——§6.5 的采样与脱敏正好落在它上面，不需要发明新机制。

### 3.6 审计小结：为什么是这五道而不是三道

issue 点名要核实三个问题，本方案给出五道闸门，多出来的两道不是扩张，是三个点名问题的**依赖**：

- "supervisor sink 的消费端是否只认一方"（点名）→ 答案是 sink 本身**不认一方**（它接受任何 `plugins.Plugin`），真正只认一方的是 sink **上游的配置面**——所以 C3 拆成 manager / edge 两半，而不是笼统一道。
- "进程模型、日志和资源上限可审查"（点名，作为硬要求）→ 在现状审计里必须先有 C5 的四个"不存在"，§6 的设计才有对照面；否则"资源治理方案"看起来像在解决一个不存在的问题。

## 4. 进程模型：现状盘点与必须先做的边界判定

资源治理的一切都挂在进程模型上，所以先把现状写死，再做两个判定。

### 4.1 现状：二元进程模型

| 形态 | 实现 | 数量 | 可约束性 |
|---|---|---|---|
| 子进程 | `SubprocessPlugin`（`subprocess.go`）：一采集器一子进程，SIGTERM + 10s `WaitDelay` 兜底 SIGKILL，崩溃指数退避 1s→5min 封顶，`RestartCount` 单调 | logs / traces / hostmetrics / procmetrics / databasemetrics（内部再起 exporter 子进程） | **有可施加对象**：一个真实的 OS 进程，有 PID，可以被 rlimit / cgroup / 杀掉 |
| 进程内 goroutine | 各自实现 `plugins.Plugin` 的 `Start` 起 goroutine | metrics / custommetrics（以及 hostmetrics 的补充 textfile producer） | **无可施加对象**：与 edge 二进制共享地址空间，宿主无法从外部限制它的内存 |

`databasemetrics` 是混合形态：它自己是进程内插件，但**每个数据库源**再起一个 exporter 子进程（`databasemetrics/plugin.go:220-270`）——严格说它的资源边界是 N+1 个进程，这一点在 §6 的执行点设计里必须显式覆盖，否则"每采集器一个 cgroup"的假设在它身上不成立。

### 4.2 判定一：第三方采集器 v1 只允许子进程形态

这是本方案最重的一条边界判定，理由链：

1. 进程内形态对宿主**不可约束**（§4.1 右列）。一个进程内第三方采集器可以 OOM 整个节点代理——而节点代理上还挂着 tool broker、policygate、离线自治，它死了不是一个采集面死了。
2. `core/domain/plugin.go:254-275` 已经为"不可执行的声明"立过案：per-tool 内存字段缺席是**判定的结果**，理由就是"同一扩展里的八个工具共享地址空间，字段没有可以施加它的对象"。
3. 子进程形态的对象天然存在，且崩溃语义已经正确：子进程被 OOM-kill 后 `runLoop` 把它记成 `StateCrashed` 并退避重启——**对资源上限的违例在现有机制下表现为一次普通的崩溃重启**，这正是我们想要的失败模式（可观测、可退避、不传染）。

因此 manifest 的进程模型声明位是一个**枚举**而非自由文本，且 `in-process` 对第三方包是加载期拒绝的值。一方包不受此限（`metrics` / `custommetrics` 保持进程内，它们随二进制一起评审）。

### 4.3 判定二：执行点选 cgroup v2 为主、`/proc` 看门狗为底

CPU / 内存 / 句柄三类上限，宿主侧真实可用的执行点有三档，本方案选前两档并用第三档兜底：

| 执行点 | 能执行什么 | 代价 / 前提 | 判定 |
|---|---|---|---|
| **cgroup v2**（`memory.max` / `cpu.max` / `pids.max`） | 内存硬上限（超限 OOM-kill 仅该子进程）、CPU 配额、并发进程数 | 需要 cgroup v2 可写。边缘通常以 root 或 systemd 单元运行，可在单元下建 `opskeeper-edge-collectors` 子组；只影响采集器子进程 | **主执行点** |
| **`/proc/<pid>` 看门狗**（周期读 `VmRSS` / `fd` 目录大小） | 内存软上限（超限由宿主 SIGTERM）、句柄数（超限 SIGTERM） | 无权限前提，任何节点可用；代价是采样间隔内可超调 | **兜底执行点**，且是 cgroup 不可用时的唯一路径 |
| **`setrlimit` 预执行钩子** | `RLIMIT_NOFILE` / `RLIMIT_AS` 在 exec 前设好 | **Go 的 `os/exec` 没有预执行钩子**，`syscall.SysProcAttr` 不携带 rlimit；要用它必须经 `prlimit` / `systemd-run` 包装器 | 不选（写明原因，避免后人当成漏项补上） |

第三档的否决理由必须写出来，因为它看起来像最标准的答案：Go 标准库不给 pre-exec 钩子，引入包装器二进制等于给每个节点多一个要分发、要版本化、要审计的产物，换来的收益（句柄硬上限）`pids.max` + `/proc` 看门狗已经覆盖到可接受的程度。

**对 `databasemetrics` 的混合形态**：cgroup 组挂在"采集器"粒度上（一个叶子组内含 N 个 exporter 子进程），上限按组算——这恰好是该形态的正确语义（运维关心的是"这个采集面总共吃多少"，不是"每个 exporter 吃多少"）。

## 5. 内置六采集器的兼容迁移方案

### 5.1 迁移什么、不迁移什么

| 对象 | 处理 | 理由 |
|---|---|---|
| logs / traces / hostmetrics / procmetrics / custommetrics / databasemetrics（issue 点名的六个） | 迁移：编译内 → **编译内预装插件**，经 `manifestFor` 合成 admission | 即 §3.2 的 C2 |
| `metrics`（第 7 个） | **本期不迁**，仅记录 | 它是 `push_prom_samples` 的父管道（`plugins/metrics/plugin.go` 的包注释写明它是"the universal path"），迁移它牵动 push 链路与 `device_id` 注入语义，超出 4.7 的"采集器插件化"范围。它留在编译内直接注册，等 datasource/push 链路单独评审。**这条不做要写进评审**：它意味着迁移后 `Inventory()` 有 6 条 collector 而节点实际跑 7 个采集器，差异必须在 inventory 端点上可见（§5.3 的 S-2 验收里有一条专门的断言） |
| 六个采集器的**实现体** | 一行不动 | `CollectorSpec` 声明（logs/traces）、`NewSubprocess` 构造、hostmetrics 的 textfile producer、databasemetrics 的 secret 处理，全部原样复用 |
| 信号名 | 不改 | `hostmetrics` 等名字是 `edge_plugin_configs.plugin_name` 列、`IsKnownPluginName`、supervisor 注册表键、心跳 `PluginHealth.Name` 四处共用的一条名字链；迁移改 admission 路径，不改这条链 |

### 5.2 迁移形状：照抄 `datasource.manifestFor` 模式

新增 `core/edge/collectorkit`（**新包**，不动 `core/edge/plugins/` 结构），内容与 `core/manager/biz/aiops/datasource/datasource.go` 逐段同形：

```go
// 包名与信号名分开：回滚按包名，查找按信号名（datasource 的 PluginProm
// 与 NameProm 的关系，同样的理由）。
const (
    PluginHostMetrics     = "opskeeper-collector-hostmetrics"
    PluginProcMetrics     = "opskeeper-collector-procmetrics"
    PluginLogs            = "opskeeper-collector-logs"
    PluginTraces          = "opskeeper-collector-traces"
    PluginCustomMetrics   = "opskeeper-collector-custommetrics"
    PluginDatabaseMetrics = "opskeeper-collector-databasemetrics"
)

// Backends 的 nil 字段 = 该部署未配置该采集器，不激活（datasource.Backends
// 与 notifier 的 Backends 同形）。装配层照旧构造六个实现体，只是交给
// 这里过门，而不是直接 supervisor.Register。
type Backends struct {
    HostMetrics, ProcMetrics, Logs, Traces, CustomMetrics, DatabaseMetrics plugins.Plugin
}

// manifestFor 合成预装插件的治理 manifest（datasource.manifestFor 的镜像，
// 差别只有 targets=edge 和 per-signal 的 required_scopes）。
func manifestFor(plugin, signal string) pluginmanifest.Plugin
```

manifest 的参考 YAML 形态在 `collector-manifests/builtin-collectors.yaml`，六包的 `safety_level / capabilities / audit / approval / install` 完全一致，差异只在 `required_scopes`：

| 信号 | required_scopes | 多问的那个 scope 的理由 |
|---|---|---|
| hostmetrics / procmetrics | `host.read` + `observability.read` | 读 `/proc` 与本机端口 |
| logs | `host.read` + `observability.read` | **`spec.file_paths` 可指向节点任意文件并把内容送进 Loki——这是六包里唯一必须单独说明的一条读取面** |
| traces | `observability.read` | 只出数据面，不读本机敏感面 |
| custommetrics | `observability.read` | 抓运维给的 /metrics URL |
| databasemetrics | `db.read` + `host.read` + `observability.read` | **六包里唯一拿到数据库连接凭据的**（读 edge 本地受管凭据文件并拨号），多问的 `db.read` 就是评审能看见的那条面 |

六包 scope 全部 ⊆ 两个部署 profile 的授予集（finance 与 saas 都授予 `host.read` / `db.read` / `observability.read`），§7.1 已实测两档全绿，**不需要改任何 profile 或 policy**。这与 4.4 的两个琥珀候选形成对照，也是本方案不用开 D1/D2 之外决策的原因。

### 5.3 默认部署行为不变：论证与切片

迁移必须分三片，因为 `core/edge/pluginhost/host.go` 的主流程在本任务非目标清单里，而 `Host` 的 kernel host 是 `New` 内部造的、外部无法预填充。

| 切片 | 内容 | 触碰面 | 回滚 |
|---|---|---|---|
| **B1** | 新增 `core/edge/collectorkit`：包名常量、`manifestFor`、`Backends`、`Activate`。**不改任何现有文件** | 只新增 | 删包即回到现状，行为逐位不变 |
| **B2** | 新增 `core/edge/pluginhost/preinstalled.go`（**新文件**，在 `pluginhost` 包内给 `Host` 加一个导出方法 `ActivatePreinstalled`）：走同一套 `kern.Admit` → `Activate`，然后复用 `host.go:212-220` 同款的 sink 注册尾段。`main.go` 在构造 supervisor 后调它。**`host.go` 主流程一字不改** | 新文件 + `main.go` 两行 | 注释掉那两行调用；`Load` 路径与现状一致 |
| **B3** | `main.go` 的 `supervisor.Register` 循环改为"构造 → 交 `collectorkit.Activate` → 由 sink 注册"，删除直接注册 | `main.go` | git revert B3 即回到直接注册 |

B1+B2 期间双重注册不发生（B2 只过 admission，注册仍由原循环做）；B3 才切换注册来源。三片各自可独立回滚。

**默认行为不变的论证（与 notifier §5.2 同形）**：

| # | 不变量 | 论证 |
|---|---|---|
| 1 | **配置面不变** | `edge_plugin_configs` 行、`IsKnownPluginName` 的 8 个名字、`pluginDefaultEnabled` 的默认开启集、`PluginConfig` 结构、`FetchForEdge` 的 wire 形状全部不动。存量行零迁移 |
| 2 | **实现体不变** | 六个构造函数与 `CollectorSpec` 声明逐字不动；`Configure/Start/Stop/HealthSnapshot` 的行为由原实现决定，迁移只改"谁把它注册进 supervisor" |
| 3 | **信号名链不变** | 信号名 = 今天的插件名（§5.1 表末行），supervisor 注册表键、心跳 `PluginHealth.Name`、UI 插件名三处读到的字符串逐位相同 |
| 4 | **注册顺序不变** | sink 注册尾段按注册表名序（`host.go:212-215` 的既有语义，`TestCollectorSetIsStableOrder` 已锁）。`HealthSnapshots` 本来就按名排序（`supervisor.go:108`），下游读数不变 |
| 5 | **admission 失败不拖垮节点** | `ActivatePreinstalled` 对某一包 `Admit` 失败时记 Error 并**回落到直接注册**（与 notifier B2 的 `ErrDuplicate` 跳过语义同族，但方向相反：notifier 的竞争是预期运维选择，这里的一方包失败是不该杀节点的装配问题）。回落保证"迁移 PR 里有一份 manifest 写错"最坏退化为现状行为 |
| 6 | **既有测试零修改通过** | `core/edge/plugins/supervisor_test.go`、`collector_spec_test.go`、六个采集器各自的 `plugin_test.go` 全部不触碰被改的路径；新增 `TestTheSixBuiltinCollectorsResolveIdentically` 断言迁移前后 `supervisor.HealthSnapshots()` 的名字集合与顺序全等，作为等价性活锁 |

第 5 条要单独强调，因为它是三片里唯一有行为风险的：**回落路径必须打一条 Warn**，否则"迁移实际没生效"会变成一个静默状态——这正是 §3.1 批评过的那类注释与实现不符。

### 5.4 `ErrDuplicate` 与配置期单激活

`ActivatePreinstalled` 必须发生在 `edgePluginHost.Load(ctx)` **之前**（B2 的装配顺序）。这样第三方包若注册同名信号，`Activate` 在 `host.go` 的 `h.live` 检查处得到 "plugin is already active"，或 `Registry.Register` 处得到 `ErrDuplicate`（`registry.go:73-75`，错误信息带持有者包名）→ 记入 `rep.Refused`，节点继续用一方采集器。这是 4.3/4.4 的"配置期单激活"纪律在 edge 侧的落法：**想共存就换信号名，于是在 inventory、健康面、配置面三处都是第三个可见的名字，而不是藏在配置里的分支。**

反向顺序（第三方先、一方后）同样成立：一方包 `Admit`/`Activate` 失败走第 5 条回落。两个方向都不产生"后注册静默顶替"。

## 6. 资源治理：manifest 声明位 + 宿主执行点 + profile 上限

### 6.1 设计的三条腿

"可审查"拆开是三个必须同时成立的问题，缺一条就退化成声明或退化成黑箱：

| 腿 | 回答的问题 | 落点 |
|---|---|---|
| **声明位** | 这个包要多少资源、日志怎么处理？——**包作者写的、评审读的** | `pig-ops.yaml` 的 `spec.collector` 块（§6.2） |
| **上限** | 这个部署最多肯给多少？——**运维写的、profile 承载的** | `pluginmanifest.Profile` 新增 `CollectorCeiling`（§6.3） |
| **执行点** | 谁真的把它执行掉？——**宿主代码、不依赖包自觉** | `SubprocessPlugin` 的 cgroup + 看门狗 + 日志管道（§6.4/6.5） |

### 6.2 声明位：`spec.collector` 块（schema 提案，落地在 S-3）

```yaml
spec:
  collector:
    process: subprocess          # subprocess | in-process；第三方包写 in-process 为加载期拒绝（§4.2 判定一）
    cpu_max_percent: 25          # → cgroup v2 cpu.max（quota/period 换算）
    memory_max_bytes: 268435456  # → cgroup v2 memory.max；超限 = 该子进程 OOM = 既有崩溃退避语义
    pids_max: 16                 # → cgroup v2 pids.max；同时是 databasemetrics N 个 exporter 的组上限
    fd_max: 512                  # → /proc/<pid>/fd 看门狗（§4.3 判定二的兜底路径）
    log_level: warn              # → 宿主转发子进程输出时的最低级别
    log_max_bytes: 10485760      # → 本地原始日志的大小帽（见 §6.5 的两份日志设计）
    log_rate_lines_per_min: 120  # → 宿主侧采样：超过即丢弃并计数
    redact: [bearer, basic_auth, password, token, secret, private_key]
                                 # → 宿主侧模式脱敏，作用于"转发的那一份"，不作用于本地原始文件
```

**为什么是新块而不是复用 `spec.tools[].limits`**：`ToolLimits` 的词表是**闭合的**，且闭合有测试守卫（`TestTheToolLimitVocabularyIsClosed`，`limits_test.go:84-131`）。它的三个判据逐一对照：

1. **有可施加的对象吗** —— `ToolLimits` 加 `Memory` 的死因是"每插件一进程、八工具共享地址空间，字段没有对象"。采集器**相反**：§4.1 表里子进程形态有真实对象（一个 PID），所以 per-collector 的资源字段过了这一关。
2. **执行点在宿主侧、包跑之前可达吗** —— cgroup / 看门狗都在 `SubprocessPlugin` 启动子进程的路径上，admission 之后、`Start` 之前即可施加。
3. **名字是否与它实际能保证的粒度相符** —— 所以块叫 `collector`、字段粒度是**每采集器**（对 databasemetrics 是每采集器组），不叫 `memory` 挂在一个 per-tool 结构上。

三条全过。因此正确的做法是**新块**，并把 `TestTheToolLimitVocabularyIsClosed` 的同款守卫扩成 `TestTheCollectorLimitVocabularyIsClosed`——加字段的人必须让测试把"对象、执行点、粒度名"三件事一起说出来。

**省略即默认，不是省略即无限**（与 `ToolLimits.Limits` 的既有语义同族，`domain/plugin.go:213-215`）：每个字段都有宿主默认值，默认值由 profile 的 ceiling 兜底。**一个都不写 = 一份可运行的 manifest；写了一个超 ceiling 的值 = 拒绝。** 这条不对称是刻意的：前者让小包好写，后者让"多要"成为一次显式评审。

**必须如实写下的现状约束**：这个块今天**加载不了**。`sdk.Decode` 的 `KnownFields(true)`（`sdk/manifest.go:70`）会让含 `spec.collector` 的 manifest 在加载期被拒（§7.1 的负例实测里就是这个错误文本）。这不是缺陷而是 fail-closed 的正确表现——旧节点拒绝新 manifest 而不是忽略它。因此 `collector-manifests/*.yaml` 的三份草案**故意不含**这个块，它们在今天的加载器上是可加载的；块随 S-3 的 schema 变更一起进入，届时三份草案同步补块并重跑 §7.1 的闸门。

### 6.3 上限：`Profile.CollectorCeiling`

```go
// pluginmanifest.Profile 增加一个字段（profiles.go）：
type Profile struct {
    // ... 现有字段不动 ...
    // CollectorCeiling 是本部署肯给一个第三方采集器的资源上限。
    // manifest 的 spec.collector 是 ask，这里是 cap，宿主执行 min(ask, cap)。
    CollectorCeiling CollectorCeiling
}

type CollectorCeiling struct {
    CPUMaxPercent     int   // finance: 25, saas: 50
    MemoryMaxBytes    int64 // finance: 256 MiB, saas: 1 GiB
    PidsMax           int   // 两档都是 32
    FDMax             int   // 两档都是 1024
    LogMaxBytes       int64 // 两档都是 32 MiB
    LogRateLinesPerMin int  // finance: 60, saas: 240
}
```

放在 profile 而不是 `kernel.Policy` 的理由：`kernel.Policy` 的四个字段（Target / MaxSafetyLevel / MaxBlastRadius / GrantedScopes）全部是**权威性**判断（允不允许），而资源上限是**容量**判断（给多少）。把它们混进 `Policy` 会让 `Admit` 的语义从"这个包准不准装"漂移成"这个包装得下吗"——后者在 admission 时点（包还没跑）只能对着声明算，算出来的答案在运行期也是要被 cgroup 再执行一遍的。所以 admission 只做"ask ≤ cap"的**形式检查**，真实执行在 §6.4。

**两档的具体数字是提案不是结论**，留给评审改。它们来自一个可核对的参照：一方六个采集器里最重的是 otelcol-contrib（traces），上游对 contrib 发行版的常规内存占用在 100–200 MiB 量级；256 MiB 的 finance 上限给第三方留了余量但不至于让一个失控包吃掉节点代理的邻居。**如果评审能拿到真实的节点侧测量值，应以此处的提案为准被覆盖。**

**金融档（finance，L2 上限）的额外含义要写明**：finance profile 的 `Ceiling` 是 `SafetyL2`、`NotGranted` 明确点名 `k8s.exec, db.write, mq.write`。本方案六包 scope 只用 `host.read` / `db.read` / `observability.read`，两个候选只多一个 `k8s.read`——全部在 finance 的授予集内。也就是说**金融档 L2 上限对本方案的九份 manifest 全部放行**（§7.1 实测），不需要为 collector 单独放宽任何一档。资源 ceiling 才是金融档对采集器的真实收紧点：一个采集器再无害，也不能在交易主机上吃 4 GiB。

### 6.4 执行点一：进程与资源

落在 `core/edge/plugins/subprocess.go` 的三个具体位置（切片 S-4，**改的是 `SubprocessPlugin`，不是六个采集器**）：

| 位置 | 改动 | 语义 |
|---|---|---|
| `runOnce` 的 `exec.CommandContext`（`subprocess.go:231`） | 加 `cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}` | 边缘代理死亡时子进程随之退出，不再留孤儿采集器；`Setpgid` 让信号可以发给整组（databasemetrics 的 N 个 exporter 一起收） |
| 同处 | 加环境白名单：只传 `PATH` / `LANG` / `LC_ALL` / `TMPDIR` + 显式 ` collector.*` 前缀的变量 | 关掉 §3.5 的凭据暴露面；与 `cmdpolicy/sandbox.go:31` 的既有做法对齐 |
| `Start` 成功后 | 建 cgroup 叶子组 `<edge-group>/<collector-name>`，写 `memory.max` / `cpu.max` / `pids.max`，把子进程 PID 写入 `cgroup.procs`；cgroup 不可写时降级为 `/proc` 看门狗 | 主/兜底双路（§4.3 判定二）；降级必须打一条 Warn，否则"上限生效了吗"又变成一个不可回答的问题 |

`databasemetrics` 的混合形态：它的 exporter 子进程由它自己 `exec`，宿主无法逐个挂 cgroup——**挂组**（把整个插件进程树放进一个叶子组）由 `Setpgid` + cgroup 的 `cgroup.procs` 写父 PID 配合实现；这正好是"组上限"语义（§4.3 末段）。

**进程内形态**（`metrics` / `custommetrics` / hostmetrics 的 textfile producer）：不设内存上限，**如实声明不设**而不是设一个不执行的字段——这是 §6.2 引用 `TestTheToolLimitVocabularyIsClosed` 判据一的同一条纪律。进程内采集器的资源治理手段是：per-call timeout（`metrics` 的 scrape_timeout 已有）+ 推送体量帽（`push_prom_samples` 侧），以及"它是随二进制一起评审的一方代码"这一层评审兜底。

### 6.5 执行点二：日志的"两份"设计

现状（§3.5）是子进程输出写进一个 `O_APPEND` 无上限的本地文件，宿主日志与它互不相通。方案是把它显式拆成两份、责任分开：

| 份 | 内容 | 约束 | 谁执行 |
|---|---|---|---|
| **本地原始文件** `<workDir>/<name>.log` | 子进程 stdout/stderr 原样 | **大小帽**（`log_max_bytes`，默认 10 MiB）：到达后**截断重写**（保留头部提示"已轮转，丢弃 N 字节"），不再无限增长；权限维持 `0600` | `SubprocessPlugin.runOnce` 的日志文件句柄处 |
| **宿主转发流** | 经 `captureLine`（`subprocess.go:303-307`，宿主预留的 plumbing point）接进宿主结构化日志 | **采样**（`log_rate_lines_per_min`，超限丢弃并周期性打一条"已丢弃 N 行"）+ **脱敏**（按 `redact` 名单做模式替换）+ **级别过滤**（`log_level`） | 同处 |

两份的分工是这条设计的核心：**取证要原文，转发要安全**。本地那份永远不离开节点、有大小帽、0600，调试一个失控采集器时它是唯一可靠的现场；宿主转发那份才是会进日志聚合、可能被很多人读到的，所以脱敏和采样只作用在它上面。把脱敏作用在本地原文上会毁掉取证价值（你脱掉的可能正是那个把凭据打进日志的 bug 的证据），把本地原文无上限又会复刻 §3.5 的磁盘吃满。

`redact` 名单是**封闭词表**而非自由正则（`bearer` / `basic_auth` / `password` / `token` / `secret` / `private_key` / `dsn` / 自定义 namespaced 项），理由同 scope 词表：一个自由正则字段等于把"什么算敏感"的定义权交给了包作者，而包作者恰是被治理的一方。宿主按名单对常见凭据形态（`Authorization: Bearer …`、`password=…`、`postgres://user:pass@…`）做替换，替换计数随心跳上报——**脱敏发生了多少次**本身是一个值得上送的信号。

### 6.6 治理闭环：inventory 与心跳

"可审查"的最后一环是读数回得来：

- `/v1/plugin-capabilities`（`main.go:305`）在 B2 之后开始出现 `kind_counts.collector`——这是 C2 修复后运维第一次能在不读源码的情况下回答"这个节点跑着哪些采集器"。
- `PluginHealth` 增加资源读数字段（`cpu_percent` / `memory_bytes` / `fds` / `log_dropped_lines` / `redacted_count`），随既有心跳链路上送。**新增字段走 `tunnel.PluginHealthWire` 的向后兼容扩展**（新字段可选），不改 wire 的既有字段含义。

## 7. 候选 1：otelcol kubeletstats（绿）

### 7.1 选型理由

- **它补一个真空**：hostmetrics 给整机视角（`node_*`），procmetrics 给按 comm 分组的进程视角，`metrics` 抓一个指定 URL——**没有任何路径给出这台节点上 per-pod / per-container 的 CPU、内存、网络、卷指标**。"这个节点上哪个 pod 把 CPU 吃完了"是落到具体节点上诊断时的第一问，现在的答案只能是"上 manager 看 cAdvisor（如果有）"，与"边缘采集"这个扩展点的定位不符。
- **零新增二进制**：复用节点已带的 `otelcol-contrib`（traces 采集器用的同一个二进制），只换 receiver。这绕开了 C1 修复之外的另一个现实问题——第三方包的二进制怎么落到节点上：`OPSKEEPER_EDGE_PLUGINS_DIR` 只承载 manifest，二进制仍由 `OPSKEEPER_EDGE_PLUGIN_BIN_DIR` 分发，一个复用既有二进制的候选把分发问题推迟到第二个候选再解决（§8 正面处理它）。
- **它是 pull 型**：kubeletstats receiver 读 kubelet `/stats/summary`，Prometheus exporter 暴露在本机 `127.0.0.1:<port>`，由 manager 侧 Prometheus 沿 hostmetrics 已有的抓取路径来抓。**刻意不选 push 型**，因此不落 C4 那道闸门，也不需要新凭据注入通道——第一个候选选绿的是为了让"闸门清单是真的"这件事先被证明一次。
- **真正第三方**：CNCF otelcol-contrib，与一方 `traces` 不共享代码、不共享信号名（`kubeletstats` 与 `traces` 无前缀包含关系，不会撞 `ParseFamily` 式的误判）。

### 7.2 manifest / 注册 / 测试 / 回滚

**manifest**：`collector-manifests/otelcol-kubeletstats.yaml`。`targets: [edge]`、`safety_level: L1`、`capabilities: [read]`、`required_scopes: [k8s.read, observability.read]`、`install.strategy: rolling` + 双下限。scope 逐项对照见 §9.1。

**注册路径**（C1 修复后的形态，五步）：

1. 包目录 `OPSKEEPER_EDGE_PLUGINS_DIR/opskeeper-collector-otelcol-kubeletstats/` 带 `pig-ops.yaml`；
2. 节点二进制编译进该包的 `CollectorSpec` 声明（receiver 是配置不是代码：kubeletstats + prometheus exporter 的 otelcol 配置模板 + argv），装配层 `RegisterActivator("opskeeper-collector-otelcol-kubeletstats", …)` 用 `plugins.NewSubprocessCollector(spec, binDir, workDir, log)` 构造——**复用一方构造器**，因为 spec 是声明不是私有逻辑；
3. `Load(ctx)` → `Admit`（profile 渲染的 edge policy，§9.1 实测两档全绿）→ `Activate` → `Register(KindCollector, "kubeletstats", plugin)`；
4. sink 注册进 supervisor；
5. C3 修复后：manager 为该边缘建 `plugin_name=kubeletstats` 的启用行，`FetchForEdge` 带上它，`TunnelConfigFetcher` 不再把它过滤掉。

**与一方 `traces` 的协同边界**：同一个 otelcol 二进制跑两个实例、两个信号名、两份渲染配置、两个工作目录。互不干扰的前提是端口与工作目录分开——spec 声明里显式写死不同的 `--config` 与 exporter 端口，并在 §7.3 的测试里有一条专门的断言。

**测试方案**：

| 层 | 用例 | 通过条件 |
|---|---|---|
| Unit | kubelet URL / 认证形态 → otelcol 配置模板渲染 | 快照一致；认证材料不出现在渲染结果里 |
| Unit | 与一方 `traces` 的渲染产物互不重叠（端口、config 路径、工作目录） | 三项各自不同 |
| Unit | exporter 端口冲突（默认端口被占） | 返回错误，不静默换端口（静默换端口会让 manager 侧抓取配置失配） |
| Integration | 本地 fake kubelet `/stats/summary`（fixture 放 `core/harness/fixtures/collector/kubeletstats/`） | container/pod/volume 三类指标字段映射正确，`device_id` 由宿主注入而非包自报 |
| Integration | 与 hostmetrics 同节点并存 | 两套序列前缀不重叠（`kubeletstats_*` vs `node_*`） |
| Failure | kubelet 401 / 403 / 超时 / 端点不存在 | 错误进 `PluginHealth.LastError`，不吞错；退避重启有上限 |
| Contract | manifest 过 §9.1 闸门 | 两档 profile 全绿 |
| Resource | 人为让子进程内存超 `memory_max_bytes` | 子进程被杀、`RestartCount` +1、健康面出现 `crashed` + 原因，节点代理与其余采集器不受影响 |

**回滚方案**（因果链，与 4.4 §4.8 同一写法）：

1. manager 侧把该边缘的 `kubeletstats` 启用行置 `Enabled=false`（先断配置，采集器在一个 reconcile 内停止）；
2. 从 `OPSKEEPER_EDGE_PLUGINS_DIR` 移走包目录（admission 不再看到它）；
3. `Host.Deactivate(ctx, "opskeeper-collector-otelcol-kubeletstats")` 摘除内核条目（`registry.RemovePlugin` 按包名清掉它的全部条目）；
4. 重启节点代理；cgroup 叶子组随子进程退出回收（S-4 的清理路径），无残留组；
5. 验证：`/v1/plugin-capabilities` 的 `kind_counts.collector` 减一；一方六采集器 + `metrics` 的健康面逐条无变化；`make pi-plugin-guard && make module-check && go test ./scripts/ledgercheck` 全过。
   **不变量**：一方六采集器与 `metrics` 完全不受影响；kubelet 侧无任何被本包修改的状态（全只读），回滚无外部副作用需要清理。

## 8. 候选 2：otelcol filelog（琥珀，决策 D1）

### 8.1 选型理由

- **它补内置 logs 采集器的弱项**：promtail 是 `msg_type: text` 的搬运工；filelog receiver 有真正的解析器（json / regex / syslog）、多行合并（combine，栈追踪不再被拆散）、按操作符路由与结构化字段提升。应用日志里最有诊断价值的那部分——带 `trace_id` 的行、被拆散的异常——正是现状送进 Loki 后**没法查**的那部分。
- **它与候选 1 构成一对刻意对照**：一个 pull 一个 push、一个绿一个琥珀、一个要 `k8s.read` 一个要 `host.read`、一个 `rolling` 一个 `pin`。两个候选合起来把 §3 的五道闸门全部走到，而不是两次走同一道。

### 8.2 为什么是琥珀：闸门 C4

它是 push 型：要把日志推到 manager 的 `/loki/api/v1/push`。`pluginEndpointResolver.Endpoint`（`cmd/opskeeper/main.go:4307-4333`）只认 `"logs"` / `"traces"`，其余返回空串——装得上，拿不到 endpoint，跑不起来。

**决策 D1：`EndpointResolver` 如何对第三方采集器开放？**

| 选项 | 内容 | 代价 | 判定 |
|---|---|---|---|
| **D1-a** | 每个采集器包在 spec 里自带 `endpoint_kind`（如 `loki-push` / `otlp-traces` / `none`），resolver 按"包名 → endpoint_kind → 系统设置"三层解析；`loki.url` 在 Integrations UI 被改后自动重定向**所有**推 Loki 的采集器 | 需要一个"包 → endpoint_kind"的登记面（与 C3 的名字来源是同一个来源，建议同切片） | **推荐** |
| D1-b | endpoint 由运维填进每个边缘的 `spec.endpoint_override`，宿主不解析 | 零宿主改动，`PluginConfig.Spec` 免费承载；代价是失去 `loki.url` 的自动重定向链路，换 Loki 地址要逐边缘改 | 可作为 D1-a 未落地前的过渡（manifest 已按此写注释） |
| D1-c | 给 resolver 的 switch 加一个 `filelog → /loki/api/v1/push` 的 case | 最省事；每来一个 push 型候选就要改一次宿主 switch，闸门重新变回闭集 | 拒绝：把 C4 修成一个更细的 C4 |
| 不做 | 候选 2 不落地 | 候选 1 独木难支，且 push 型这条真实需求（应用日志）在扩展点上没有答案 | 拒绝 |

### 8.3 manifest / 注册 / 测试 / 回滚

**manifest**：`collector-manifests/filelog.yaml`。`required_scopes: [host.read, observability.read]`——**与被它替换的内置 logs 采集器逐字相同**，这是刻意的：一个第三方包要替换内置信号，它声明的授予面不该比被替换者更宽；更宽就必须在评审里被看见。`install.strategy: pin`（解析规则更换会改变已收日志的字段形状，查询侧要跟着变，不该按节点波次铺开）。

**信号名**：`filelog`（**不复用 `logs`**）。同一信号双注册会在装配期撞 `ErrDuplicate`（§5.4），这正是互斥语义在工作；想共存就用自己的名字，于是在 inventory、健康面、配置面三处都是可见的第三种选择。

**注册路径**：同 §7.2 五步，前置为 D1 裁决 + C3 修复。

**测试方案**：

| 层 | 用例 | 通过条件 |
|---|---|---|
| Unit | operator 解析器配置（json / regex / syslog / 多行 combine）→ otelcol 配置渲染 | 快照一致；`combine` 的多行窗口与超时有界 |
| Unit | label 基数：渲染结果里的 label 集合是**封闭名单**（`device_id` / `opskeeper_source` / `job` / 白名单 extra） | 包自报的任意 label 被拒——`device_id` 必须由宿主注入，这是 manager 侧 PromQL `by(device_id)` 的 join key，交给包自报等于把 join key 交给被治理方 |
| Unit | 凭据不落渲染文本（bearer 走环境注入而非写进配置文件） | 渲染产物 grep 不到凭据 |
| Integration | fake Loki 端点 + 固定日志 fixture（含多行栈追踪、含一行意外带 `password=` 的） | 多行合并成一条；带 `redact` 名单的转发流里 `password=` 被替换而本地原文保留 |
| Failure | Loki 429 / 5xx / 超时 | otelcol 自身退避；宿主侧 `PluginHealth.LastError` 可见，不无限重试 |
| Contract | manifest 过 §9.1 闸门 | 两档 profile 全绿（琥珀在 C4，不在 scope） |
| Rollout | 与内置 `logs` 同进程共存（不同信号名） | 两者同时跑、互不顶替；关掉内置后 `filelog` 独立工作 |

**回滚方案**：与 §7.2 同构（配置置 disabled → 移包 → `Deactivate` → 重启 → 验证），外加两条本候选特有的：

- **外部副作用不可撤销**：已经送进 Loki 的日志不会因回滚消失。这与其他扩展点的回滚语义不同（datasource 只读、adapter v1 只读），回滚文档第一句必须写它。
- **positions 状态**：filelog 的读取游标在包工作目录里；移包目录会连游标一起删，回滚后重新装回会**从头读**。回滚步骤里"保留工作目录"与"清掉工作目录"是两个不同决定，各自的行为要写进 runbook，不能默认。

## 9. 通用闸门与实测

### 9.1 manifest 闸门实测（`sdk.Decode → sdk.Validate → sdk.Admit`）

对 `collector-manifests/*.yaml` 逐份跑三段闸门，`Admit` 用部署 profile 渲染出的 edge policy（即 `core/edge/pluginhost.PolicyFrom(profile)` 的四个字段）：

| 文件 | name | level | scopes | finance（L2/pod） | saas（L3/ns） |
|---|---|---|---|---|---|
| `builtin-collectors.yaml` | `opskeeper-collector-hostmetrics` | L1 | `host.read, observability.read` | **通过** | **通过** |
| `otelcol-kubeletstats.yaml` | `opskeeper-collector-otelcol-kubeletstats` | L1 | `k8s.read, observability.read` | **通过** | **通过** |
| `filelog.yaml` | `opskeeper-collector-filelog` | L1 | `host.read, observability.read` | **通过** | **通过** |

（`Decode`/`Validate` 三份全过；`radius` 均为空、`strategy` 分别为 pin / rolling / pin。）

**负例对照**——闸门必须被证明是真的在比较，而不是恰好放行。同一工具跑了四组负例：

| 负例 | 结果 | 证明的事 |
|---|---|---|
| manifest 带 `spec.collector` 块 | `Decode` 拒：`field collector not found in type domain.PluginSpec` | **§6.2 的资源声明位今天不存在**，且 `KnownFields` 是 fail-closed 的（拒绝而非忽略）；块必须随 schema 变更一起落地 |
| manifest 要 `k8s.exec` | finance 拒：`not granted: k8s.exec`；saas 过 | finance 的 `NotGranted` 清单是真实闸门——一个要 `k8s.exec` 的"采集器"在金融档上装不上 |
| manifest 在 L1 下写 `max_blast_radius: pod` | `Validate` 拒：`safety_level L1 is read-only and cannot carry a blast radius` | `sdk.Validate` 的治理检查先于 `Admit` 生效 |
| manifest 声明 `L3` | finance 拒：`plugin is L3, target policy allows at most L2`；**saas 过** | 见下面的 D2——saas 档对 L3 采集器不设防 |
| 旧 edge policy 对照（L1 + 单 scope） | 全部拒：`not granted: …` | 复刻 `host.go:243-247` 注释记录的那个曾经拒掉全部随包的 policy，证明闸门真的在比 |
| manager policy 对照 | 全部拒：`targets [edge], host runs "manager"` | `targets` 检查独立于 scope 检查，一份 edge manifest 不会在 manager 宿主上被误装 |

（复核方式：把 `collector-manifests/` 目录与负例目录分别交给 `sdk.Decode` + `sdk.Validate` + `kernel.NewHost(reg, policy, nil).Admit`，policy 由 `pluginmanifest.ProfileForName` 渲染。这是本任务期间一次性跑过的验证，未在仓内留下测试代码——§10 S-5 建议把它固化成 `collector-manifests_test.go`，做法与 notifier 方案 §11.1 的说明一致。）

### 9.2 其余闸门（每候选必过）

| 闸门 | 内容 |
|---|---|
| **精确版本** | `metadata.version` semver 锁（第三方 `0.1.0`；合成的预装包 `1.0.0`，同 `datasource.manifestFor`）；不得 `latest` / `main` |
| **scope 最小化** | 每份 manifest 只声明该采集器真用的 scope；`db.read` 只有 databasemetrics 有，`k8s.read` 只有 kubeletstats 有。多写一个就该在评审被打回 |
| **scope ⊆ 宿主 Policy** | 逐候选对照 §9.1；本方案**九份 manifest（六预装 + 两候选 + 一参考）无一需要改 profile 或 policy**——这与 4.4 需要 D1/D2 扩 policy 形成对照，原因是采集器的 scope 全落在两个 profile 都授予的读集合里 |
| **安全级别** | 采集器一律 `L1 + [read]`；一个要写什么的采集器不是采集器。低报无收益（未识别等级按 L3 排名），高报会被 §9.1 的负例路径拦 |
| **进程模型声明** | `spec.collector.process` 对第三方包只接受 `subprocess`（§4.2）；`in-process` 是加载期拒绝值 |
| **资源声明 ≤ profile ceiling** | admission 做形式检查（ask ≤ cap），执行在 §6.4——**声明了不执行的路径不存在** |
| **命名空间** | 信号名不得与一方信号名或其前缀互相包含（`filelog` vs `logs` 是正例，`logs2` 是反例） |
| **兼容范围** | 第三方包写 `min_pig_version` / `min_edge_version`（0.4.0 / 0.8.0，与随包对齐）；合成的一方 manifest 不写（与二进制同构建，理由同 notifier `builtin-channels.yaml`） |
| **测试** | unit + integration + contract + failure 四层，外加本扩展点特有的 **resource 层**（§7.3 的内存超限用例）——资源上限没有测试就是 §6.2 批评过的"读起来像保证的字段" |
| **签名** | 经 `core/floor/pluginmanifest/signing.go` 通道；`Profile.Policy()` 已强制 `AllowUnsigned=false` |
| **回滚文档** | 卸载前 / 卸载后 / 不变量三段 + 精确因果链；push 型候选必须写明**外部副作用不可撤销** |
| **闸门命令** | `make pi-plugin-guard && make module-check && go test ./scripts/ledgercheck` 全过 |

### 9.3 明确不做与不建议（避免后续重复讨论）

- **`changewatcher`（变更监听）不迁**：同属 Edge 执行面（4.1 §4 补充表把 Edge 执行面指向 4.7），但它产出的是变更事件而非遥测信号，`plugins.Plugin` 的 `PluginConfig`/`PluginHealth` 形状也不匹配。把它塞进 collector 是用"六个 Kind 里最像的一个"代替"对的那个"。留待 4.8。
- **`metrics`（第 7 个采集器）本期不迁**：见 §5.1 表第二行的完整理由。
- **`profiles`（pyroscope）不在名单**：`IsKnownPluginName` 认它但节点上没有实现、二进制不在安装包里。加它等于为一个不存在的采集器占一个 admission 名额。
- **不把第三方客户端代码放进 `core/edge/plugins/`**：该目录是六采集器的家，与 4.4 §7.3"不要把第三方客户端放进 `core/manager/middleware/adapter/<name>`"同一条纪律。第三方包走 `RegisterActivator` + 包目录，不进主仓目录。

## 10. 实现切片建议（不在本任务合入）

| 切片 | 内容 | 触碰面 | 前置 | 验收 |
|---|---|---|---|---|
| **S-1** | C1 修复：装配层为第一批候选 `RegisterActivator`（构造走 `plugins.NewSubprocessCollector`，spec 是声明不是私有逻辑） | `cmd/opskeeper-edge/main.go` | — | `host_test.go` 的 `TestLoadActivatesCollectorAndRegistersIt` 在真装配路径上成立；`/v1/plugin-capabilities` 出现 `collector` 计数 |
| **S-2** | B1+B2：`core/edge/collectorkit` 新包 + `core/edge/pluginhost/preinstalled.go` 新文件 | 两个新文件 + `main.go` 两行 | S-1 | §5.3 六条不变量表逐条成立；新增等价性测试通过；`host.go` 主流程 diff 为零 |
| **S-3** | §6.2 schema：`domain.PluginSpec.Collector` 块 + `TestTheCollectorLimitVocabularyIsClosed` 守卫 + `Profile.CollectorCeiling` | `core/domain/plugin.go`、`core/floor/pluginmanifest/profiles.go` | S-2、独立 change（ABI 变更，`core/domain/plugin.go` 文件头明确常量属于插件 ABI） | 词表守卫在"加字段不加执行点"时失败；两份候选 manifest 补块后重跑 §9.1 全绿 |
| **S-4** | §6.4/6.5 执行点：`SubprocessPlugin` 的 `Setpgid`/`Pdeathsig`、环境白名单、cgroup + `/proc` 看门狗、日志大小帽、`captureLine` 接入宿主日志 + 采样 + 脱敏 | `core/edge/plugins/subprocess.go` | S-3 | §7.3 resource 层用例；一个失控子进程被杀且节点代理存活；`OPSKEEPER_EDGE_SECRET_KEY` 不在子进程环境里 |
| **S-5** | C3 + C4：manager 名字闭集改为"节点已装包清单"来源（心跳上送 inventory）；`knownPlugins` 过滤来源换成 supervisor 注册表；D1-a 的三层 endpoint 解析 | `core/manager/model/edge`、`core/manager/biz/edge`、`cmd/opskeeper/main.go`、`cmd/opskeeper-edge/main.go` | S-1、D1 裁决 | 第三方采集器从启用行到运行在 60s 内收敛；幽灵返回过滤仍生效（造一个 manager 返回未知名字的用例） |
| **S-6** | 候选包首切片（建议顺序：kubeletstats → filelog） | `OPSKEEPER_EDGE_PLUGINS_DIR` 外部目录 + 装配层 activator | S-1/S-4（kubeletstats）、S-5（filelog） | 每包独立 PR、独立版本、独立回滚 |
| **S-7** | `PluginHealth` 资源读数扩展（§6.6） | `core/edge/plugins`、`core/floor/tunnel` wire 可选字段 | S-4 | 心跳面能看到 cpu/mem/fd/丢弃行/脱敏次数 |

**顺序理由**：S-1 先于一切——它是唯一"零 schema 变更就能把闸门打通"的切片，先做它，后面每一步都能用 `/v1/plugin-capabilities` 的读数验证而不是用日志猜。S-3 必须独立 change：它动的是 ABI，且本方案 §6.2 的论证只有在评审接受后才是结论。S-4 在 S-3 之后：先有声明位再上执行点，反过来会出现"执行点存在但没人能声明"的空转。

### 10.1 不建议改动（禁项）

- 不在 `core/edge/plugins/` 下加第三方包或第三方客户端（§9.3）。
- 不在 `firstPartySources` 式的编译内切片里塞第三方条目——那是把第三方变成编译内依赖，恰好消灭扩展点（与 4.4 §7.3 方案 D 同一条否决）。
- 不给 collector 新增 Kind，不改 `kind.go` 六元集合。
- 不在 `subprocess.go` 里为某个具体采集器加 `if` 分支；差异化一律是新包。
- 不删 `TunnelConfigFetcher` 的 `known[name]` 过滤——它防的是 manager 幽灵返回，换来源而不是删检查（§3.3）。
- 不给进程内第三方采集器开口子。§4.2 判定一被推翻的正确路径是先建立进程内隔离手段，而不是先放开限制。

## 11. 与 4.1 / 4.2 一致性自查表

| 自查项 | 4.1 / 4.2 原文 | 本方案是否一致 | 证据 |
|---|---|---|---|
| collector 状态"通道就绪，0 贡献" | 4.1 §3 `collector` 行 | ✓ 本方案的内容就是消掉这一行：C1 是"0 贡献"的结构性原因，S-1 打通 | §3.1 / §10 |
| 4.1 §4 ④"边缘采集 → 迁移为插件（候选）→ 4.7" | 六采集器编译内 | ✓ §5 给六采集器迁移路径，§7/§8 给候选 | §5 / §7 / §8 |
| 4.1 §6 缺口行"collector 无贡献，内置采集器仍编译内 → 观测、Edge → 4.7" | | ✓ 收口：C1 + C2 分别对应两半 | §3.1 / §3.2 |
| 六扩展点封闭 | `kind.go` 六元封闭集合 | ✓ 复用 `KindCollector`，不新增字符串；`kind.go` 不在触碰清单 | §1.3 / §10.1 |
| `Activate` 是注册唯一入口 | 4.3 §2.2 关键不变量 | ✓ B2 的 `ActivatePreinstalled` 走同一套 `Admit → Activate`，无并行注册表 | §5.3 |
| 同信号互斥 `ErrDuplicate`、配置期单激活 | 4.3 §3.3 | ✓ §5.4 双向论证；候选 2 用独立信号名 `filelog` 而非复用 `logs` | §5.4 / §8.3 |
| 一方不重写 | 4.3 §1.2 / 4.4 §7.3 | ✓ 六采集器实现体一行不动；`core/edge/plugins/` 结构不动 | §5.1 / §1.4 |
| 不引入运行时加载第三方 Go | `design.md` Non-Goals | ✓ §1.2 把它作为前置框架判断，而非回避 | §1.2 |
| `required_scopes` ⊆ `core/domain/plugin.go` 常量集 | scope 硬约束 | ✓ 九份 manifest 只用 `host.read` / `db.read` / `k8s.read` / `observability.read` 四个既有常量，**零新增** | §5.2 / §9.1 |
| scope ⊆ 宿主（edge profile）Policy | 逐候选对照 | ✓ §9.1 两档 profile 实测全绿，无一需要扩 policy | §9.1 |
| 金融档 L2 上限约束写明 | 任务要求 | ✓ §6.3 末段；九份 manifest 全部 L1，L2 上限不是约束点，资源 ceiling 才是 | §6.3 / §9.1 |
| 任务硬要求"进程模型、日志和资源上限可审查" | tasks.md 4.7 原文 | ✓ 三条腿设计（§6.1）；C5 现状 + S-3/S-4 修复 | §3.5 / §6 |
| 不动 `core/edge/pluginhost/host.go` 主流程 | 任务非目标 | ✓ 用**新文件**加方法（B2），`Load` 主流程 diff 为零 | §5.3 |
| 与 `opskeeper-sre-*` 五包协同只描述接口 | 4.3 §8 同款 | ✓ 不改任何 `plugins/pig-ops` manifest；采集器不声明 `spec.tools`，与工具面无交集 | §9.2 |
| 受审计文件不含内部任务编号 | `scripts/audit_open_source.py` 规则 | ✓ 本文与 manifest 草案均不含；编号只在 PR 描述与评审评论 | §13 |
| 中文注释与中文提交说明 | 计划任务 7.4 | ✓ 文档与 manifest 注释为中文；提交说明为中文 | §13 |

## 12. 风险与待评审问题

### 12.1 风险分级

| 级 | 风险 | 触发条件 | 缓解 |
|---|---|---|---|
| **高** | S-3（`spec.collector` schema）被否决 | ABI 变更需独立评审未过 | 降级为 D3-b：资源上限只放 profile ceiling（fleet 级、无 per-package 声明），宿主执行点照做。代价是"这个包要多少"回到评审口头，失去声明面——**这是治理降级，需用户显式决策**，不能由实现者默认选 |
| **高** | C3 修复被做成"删过滤"而不是"换来源" | 实现者图省事 | §10.1 已列为禁项；S-5 验收里专门有一条"幽灵返回仍被过滤"的用例 |
| **中** | §6.3 的两档资源数字拍错（太紧杀正常采集器 / 太松形同虚设） | 落地时无真实测量 | 数字标注为提案；S-4 落地时先跑一方六采集器的实测占用再定稿，且 ceiling 是 profile 字段、改它不动 schema |
| **中** | cgroup 不可写（容器化节点、非 root） | 部署形态 | §6.4 的 `/proc` 看门狗兜底 + 降级 Warn；**降级必须可见**，否则"上限生效了吗"不可回答 |
| **中** | `metrics` 不迁导致 inventory 与实际采集器数不一致 | §5.1 的边界决定 | S-2 验收专门有一条断言，且 §5.1 表格写明理由供评审推翻 |
| **低** | 两个候选都是 otelcol，被读成"只会用 otelcol" | 阅读方式 | §8.1 说明配对是刻意的（pull/push、绿/琥珀、两种 scope、两种 strategy 各走一遍）；第三个候选应选非 otelcol（如 node_exporter 采集面的外部 exporter） |
| **低** | filelog 回滚后从头读日志 | positions 游标被清 | §8.3 把"保留 / 清掉工作目录"写成两个显式决定 |

### 12.2 待评审问题

1. **D1（阻塞候选 2）**：`EndpointResolver` 对第三方 push 型采集器怎么开放？推荐 D1-a（包声明 `endpoint_kind` + 三层解析），D1-b（spec 内 endpoint_override）作过渡，见 §8.2 表。
2. **D2（治理一致性，非安全堵洞）**：§9.1 负例实测发现 **saas profile（L3 上限）会放行一个声明 L3 的采集器**（finance 拒）。它不构成权限升级——贡献体是编译内代码，admission 是唯一动态闸门，而代码在编译期已被运维选择。但它构成**记录失真**：profile 的 ceiling 语句（"本部署肯承载什么"）与节点 inventory 的实际读数不一致。是否沿用 notifier 方案 S-1 提出的 `WithKindCeiling` 机制，给 edge 宿主的 `KindCollector` 收窄到 L1？**这是同一个内核机制服务两个扩展点，若 S-1 先落地，本项边际成本接近零。**
3. **D3（阻塞 §6.2）**：`spec.collector` 块是否接受为 ABI 变更？判据已按 `TestTheToolLimitVocabularyIsClosed` 的三条逐一对照（§6.2）；被否决时的降级路径见 12.1 第一行。
4. **Q4**：`metrics`（第 7 个采集器）本期不迁的边界是否接受？理由与代价在 §5.1 表第二行，含一条专门的 inventory 一致性断言。
5. **Q5**：`redact` 名单作为封闭词表（§6.5）是否够用？自定义 namespaced 项是否要进第一版，还是等第一个真实用例？

## 13. 提交与执行规范

- **落盘路径**：本文档 `openspec/changes/plugin-architecture-recovery-plan/collector-plugin-plan.md`；manifest 草案 `openspec/changes/plugin-architecture-recovery-plan/collector-manifests/`（`builtin-collectors.yaml`、`otelcol-kubeletstats.yaml`、`filelog.yaml`）。
- **提交语言**：本任务 PR 使用中文注释 / 中文提交说明（按计划任务 7.4）。
- **关联**：OpenSpec change `plugin-architecture-recovery-plan` 任务 4.7；执行任务编号只记录在 PR 描述与评审评论中，**不写入受审计文件**（`scripts/audit_open_source.py` 的 internal task ID 规则会拒绝 `BENY-\d+` / `LUM-\d+` 形态）。
- **勾选状态**：本文档为方案 v0.1，评审通过前不勾选 `tasks.md` 4.7（与 4.1 / 4.3 / 4.4 / 4.5 的处理一致）。
- **实现切片**（§10）**不**在本任务内合入；如要落地，请用户单独开 change，并先裁决 **D1**（endpoint 解析）、**D2**（collector per-kind 上限）、**D3**（`spec.collector` schema）。
