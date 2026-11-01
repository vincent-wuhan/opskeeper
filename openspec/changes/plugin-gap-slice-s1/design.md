# Design: plugin-gap-slice-s1

本文件是 4.8 方案切片 S-1（G0 修复）的设计输入。方案依据：`plugin-gap-plan.md` §4（G0）、§5.5（S-1）、§6.2（D-2 裁决：BaseTool 桥，一实现两面）。

## 一、缺口的准确形状

三个事实叠在一起才是 G0，缺一个都造不出它：

1. **声明存在**：`plugins/pig-ops/opskeeper-sre-observability/pig-ops.yaml` 与 `plugins/pig-ops/opskeeper-sre-readonly/pig-ops.yaml` 里，6 个控制面工具在 `spec.tools` 中被声明。声明是节点侧的允许清单，节点的 broker 与 agent 进程各查一次。
2. **实现存在**：这 6 个工具是 `core/manager/biz/aiops/tools` 里的 BaseTool（`alerting.NewQueryChangeEventsTool`、`NewListRepoSourcesTool`、`NewReadSourceTool`、`NewGrepSourceTool`、`topology.NewExpandTopologyTool`、`topology.NewFindTopologyNodeTool`），由 `Registry.BuildBaseTools()` 装配；节点侧 PiG 扩展的 schema 也从同一批 `Info()` 生成（`core/pig/extensions/*/tools.go` 是 GENERATED 文件）。
3. **可服务面不存在**：节点上呼的落点是 `cmd/opskeeper` 的 `agentToolUpcall.dispatchAgentTool`，它只有两条去路——中间件 family（前缀路由）与闭包注册表 `Registry.Invoke`。闭包注册表（`core/manager/biz/aiops/tools/registry.go`）里没有这 6 个名字。

所以调用结果是确定的 "no such tool"，而不是"有时能有时不能"。**能力不缺，缺的是接线**。

这也解释了为什么现有闸门没有拦住它：仓库里所有工具相关的检查问的都是"这个工具在不在"（在 bag 里？在 manifest 里？在 schema 里？），没有一个问"这个包声明的工具**有没有人服务**"。声明在 YAML 里，可服务面在 Go 的路由里，两边从不同角度被看，中间那段空档没有视角。

## 二、实现约束（6 条，逐条落到代码）

### 2.1 只读闸门

`BuildBaseTools()` 产出的 bag 里既有只读工具，也有写工具与破坏性工具（`apply_config_change`、`host_restart_service`、`recovery.execute`、`send_im_message`、`serve_page`、`AgentTool`/`SendMessage`/`TaskStop`、`cloud_bash`、`install_skill`）。上呼通道**没有审批队列**：调用一到就被立即分派并归属到某个会话（`runMiddlewareTool` 的文档注释写明了这一点："a call that arrives on it is dispatched immediately and attributed to a conversation"）。

因此桥的收敛口径是 `Info().Class == "read"`，与中间件面用 `toolset.IsRead(spec.RiskLevel)` 收敛的做法同源：**类是从执行器重新推导的，不是从 manifest 抄来的**。manifest 是声明，声明能放宽一条通道就值得查两遍。

配套要求：桥必须**知道**那些非只读工具的名字。被问到时给出"这是一条只读通道"而不是"没有这个工具"——这两种回答对读日志的人意义完全不同：前者是策略，后者是拼错的名字。

`host_bash` 是个特例：它的 `Info().Class` 是 `"read"`（`core/manager/biz/aiops/tools/bash_basetool.go`），但它是 shell。只读类说的是"这个工具的**声明**效果是读"，而 shell 的实际效果由命令决定；它还带一条改命令走审批箱的路径。把一个 shell 挂到没有审批队列的通道上，是把审批从链路上摘掉。因此它进保留名单。

保留名单（4 项，逐项有理由）：

| 工具 | 理由 |
| --- | --- |
| `host_bash` | shell；类声明是读，实际效果由命令决定，且它自己带一条审批路径。节点本地已有自己的 bash 技能面。 |
| `ToolSearch` | bag 自己的"按需取 schema"工具（`bag.WithExtra` 挂上），是管理端模型循环的发现工具；节点侧的工具清单由 manifest 决定，不需要向管理端问 schema。 |
| `chat_to_query` | 自然语言转查询的助手功能，绑在管理端的对话会话上，不是节点会向控制面问的机群问题。 |
| `draft_config_change` | 配置草稿绑在管理端会话上，且它是 `apply_config_change`（写）的前半段；把写对的前半段挂到只读通道上，等于给写操作留了一条不需要审批的门。 |

### 2.2 同名优先级：桥优先，闭包兜底

闭包注册表与 BaseTool 是同一批业务能力的两套装配形态，**同名工具的参数形状已经分叉**：闭包取 `edge_name`，BaseTool 取 `device_ids[]`。

节点侧的 schema 由 BaseTool 的 `Info()` 生成（`core/pig/extensions/*/tools.go` 是 GENERATED 文件，且有测试比对生成物与执行器元数据）。所以节点发来的参数是按 BaseTool 的形状构造的。若上呼优先落到闭包，节点发来的 `device_ids[]` 会被一个期待 `edge_name` 的执行器解析——**G0 会在同名工具上原样复发**，而且这次更难察觉，因为工具"存在"。

因此：桥先、闭包兜底。

这与 `dispatchAgentTool` 里既有的"按前缀路由而不是按查找顺序"并不矛盾，两者管的是不同的命名空间：

- 中间件工具的名字带 family 前缀（`redis.info`、`pg.query`），闭包工具的名字不带。前缀是一个**类型标记**：带前缀的名字只可能是中间件工具，闭包工具不可能叫 `redis.info`。这里没有歧义可生，按前缀路由让两个面各自独立演进、各自被不同的人审。
- 桥与闭包是**同一命名空间**里的两个实现，重叠的名字指的是同一个能力。那就是"实现选谁"的问题，不是"类型判别"的问题。一个能力有两套装配形态时，**节点看得见的那一套**（BaseTool，因为 schema 从它生成）必须赢，否则节点看到的接口与调用的实现不是一回事。

### 2.3 独立的 bag 实例

桥调用 `Registry.BuildBaseTools()` **自己**取一份 bag，不复用 MCP 那份（`mcpBaseBag`）也不复用对话那份。

原因：`mcpBaseBag` 在构造之后被 `aiopshost.AppendHostFilesTools` 追加了 `host_*` 工具。那批工具是**节点本地技能面**——同一批能力在 `core/floor/skill/builtin/` 里注册，由节点自己服务（`cmd/opskeeper-edge/tools.go` 的 `runsOnThisNode` 让只读技能在本地跑）。如果桥复用这份 bag，`host_*` 就同时变成上呼可服务：节点会为一个自己能回答的问题绕一圈隧道问控制面。这是一条**反向缺口**——本切片修的是"声明了没人服务"，顺手制造一条"能本地服务却往上游问"，是把缺口换了个方向。

`BuildBaseTools()` 自身已经做了 `markUntrustedOutputs(out, promptguard.NewFencer())`，也就是 `MarkUntrustedOutput` 已在 bag 里。桥复用 bag 即可，不再包第二层——两层围栏会让同一个字符串被标记两次，也会让围栏的规则看起来像有两套。

### 2.4 装配时机

桥必须在 `toolWiring.apply(toolsReg)` **之后**装配，与 `mcpBaseBag` 同一块。

`toolWiring.apply` 是所有后置 setter 的唯一执行点（`knowledge` / `spawner` / `topologyGraph` / `auditLister` / `edgeChanges` / `pluginConfig` / `configManager` …）。在它之前调 `BuildBaseTools()`，`r.knowledge`、`r.topologyGraph`、`r.auditLister` 都还是 nil，于是：

- `list_repo_sources` / `read_source` / `grep_source` 不会进 bag（需要 `knowledge` 是 `CodeBrowser`）；
- `expand_topology` / `find_topology_node` 不会进 bag（需要 `topologyGraph`）；
- `query_change_events` 不会进 bag（需要 `auditLister`）。

也就是说，**6 个 G0 工具里有 6 个都依赖后置 setter**。装早了，桥建成一个空壳，而且不报错——这正是这类缺口的惯常形状。

### 2.5 surface 的单一事实源

`agentToolSurface(tool)` 是一个纯按名字推断的函数：有 family 前缀就是 `middleware`，否则是 `aiops`。桥接进来之后，这个推断开始说谎：`query_change_events` 没有前缀，按名字推断会说它是 `aiops` 面服务的，而它实际上是桥（`basetool`）服务的。审计行的用途是回答"控制面替这个节点跑了什么"，一个会说谎的 surface 比没有 surface 更糟。

因此 surface 改为**分派结果给出**：谁服务了这次调用，surface 就是谁。分派顺序是 `中间件 family → 桥 → 闭包注册表`，每一段都带着自己的 surface 往回传。

### 2.6 装饰器复用

桥沿用 `decorators.Wrap` 的同一条链（`tenant_bind → pause_point → review_gate → timeout → audit → ratelimit → metric`），`Deps` 与既有调用点同形：

- `Timeout: 15 * time.Second`——与 MCP 面、flow invoker 一致。
- `Limiter: NewTokenBucketLimiter(0)`——不限流。
- `Registerer`——metric 收集器照旧注册。
- `Deps.Audit` **留空**。本通道的账本由 `agentToolAudit` 写（`agent_tool_call` 事件）；装饰器链的 audit 写的是 `chat_tool_calls`。两处都写就是同一件事记两遍，而且两处的读者不同，会得出两个结论。
- `ReviewSpawner` **不接**，因此链上没有 `review_gate`。`review_gate` 只对写类工具有意义，而这条通道只走只读；接上它等于在一扇只开的门上装一把只对另一扇门有效的锁。
- `PauseCoordinator` 不接——同上，`WithPausePoint` 对 nil 是 noop。

## 三、测试策略

### 3.1 对账闸门

断言形式：**每个包的 manifest 控制面工具名 ∈（上呼可服务名 ∪ 节点本地技能名）**。

- **上呼可服务名**必须来自真实构造：全依赖 fake 下 `Registry.BuildBaseTools()` → 桥装配 → 桥的可服务名单。
- **节点本地技能名**必须来自真实构造：`skill.All()` 里每个 `Executor` 的 `Metadata().Key`。
- **禁止手抄字面量**。手抄出来的名单正是 G0 的成因本身——G0 就是"manifest 上写着 12 个名字，实现侧一个都没有，两边各自都是真的"。一份手抄的名单会把同样的错误搬进测试里，然后测试一直绿。

闸门的范围：本切片只覆盖 `opskeeper-sre-observability` 与 `opskeeper-sre-readonly` 两个包。理由写在测试的文档注释里：`opskeeper-sre-repair` 的两个只读工具（`draft_config_change` 绑在管理端会话上、`verify_recovery` 在 `cmd/opskeeper` 里独立构造、不在注册表 bag 内）与 `opskeeper-sre-middleware` 的工具分别属于"会话面"与"适配器面"，它们的可服务性与本切片修的这一类不同；把它们塞进同一个闸门会让闸门长期红，红的闸门等于没有闸门。

### 3.2 负例必须真红

闸门要能证明它不是空转：把桥从可服务集合里拿掉（也就是复原 G0 之前的状态），闸门必须报出**且只报出**那 6 个工具。

这一步是"真红"的判据：如果负例也绿，说明闸门没有在检查任何东西。证据（闸门在 G0 状态下报出的工具名）随 PR 附上。

### 3.3 不做反向断言

反向断言（"上呼可服务 ⊆ manifest 声明"）不做。桥按只读类收敛，管理端还有一批只读工具不在任何包的声明里（§7 的 `rank_edges`、`correlate_incident` 等），反向断言会把"管理端有工具"判成违规。§7 的处理方式是：**不因桥接通了就把它们写进 manifest**——manifest 增量是 S-2/S-3 的事，且要按包的语义挑，不按"现在能调了"挑。

### 3.4 fake tunnel 集成测试

6 个 G0 工具各自经上呼成功，返回非空 JSON，且审计行的 `surface` 是 `basetool`。

"fake tunnel"指节点侧那条隧道在测试里的替身：管理端这 6 个工具里，`expand_topology` / `find_topology_node` / `query_change_events` / 三个 `*_source` 工具各自有数据依赖（拓扑图、审计流水、代码库），测试用 fake 提供这些依赖，走的是与生产同一条 `RunAgentTool` → `dispatchAgentTool` → 桥 → 装饰器链的路径。断言的不是"函数返回了东西"，而是**审计行里的 surface 是 `basetool`**——这才是这条修复与"恰好能在某个部署上跑通"的区别。

## 四、找不到 vs 没配置

桥的 miss 有两种，措辞必须不同：

- **知道但不服务**（非只读类、保留名单）：给出**为什么**。"这条通道不承载写操作"与"这个名字在这里不服务"是两件事，前者是策略，后者是编排决定。
- **这里没有**：桥不宣称这个工具不存在——它只知道自己这一份 bag。桥没命中就继续往下问闭包注册表，两边都没有才说"没有这个工具"。

"已声明但依赖缺失"（如 `r.knowledge == nil` 时 `query_knowledge` 整体不在 bag 里）在本切片里**仍会落到"没有这个工具"**——运行时没有一份不靠手抄的清单能说出"这个工具存在，只是这个部署没配"。S-3 的"就地降级"改造会把这类工具改成带内降级，那时措辞才自然分成两句。本切片不提前实现 S-3，但桥的措辞已经按"只说自己知道的"写好，等 S-3 落地时不需要改这一层。

## 五、交付边界

- **先开 OpenSpec change**（本目录），再实现。
- **范围内**：桥 + 装配 + 对账闸门（负例真红）+ 审计 surface 增 `basetool` + 6 工具 dispatch 修复 + fake tunnel 用例。
- **范围外**：5.1–5.4 的 manifest 增量（S-2/S-3）；任何新的闭包注册表条目；反向断言。
- **PR**：中文说明 + `.github/pull_request_template.md` 的勾选项 + Conventional Commit（标题英文，正文可中文）。**内部任务编号只出现在 PR 说明与评审意见里，不进入受审计文件**（`scripts/audit_open_source.py` 会拒绝内部编号形态与本机绝对路径）。
