## Why

4.8 方案（`plugin-gap-plan.md`）记录的 G0 是这一轮里唯一一条**代码存在、包声明了、运行时却没有**的能力缺口：

- `plugins/pig-ops/opskeeper-sre-observability/pig-ops.yaml` 声明了 `query_change_events`、`list_repo_sources`、`read_source`、`grep_source`；
- `plugins/pig-ops/opskeeper-sre-readonly/pig-ops.yaml` 声明了 `expand_topology`、`find_topology_node`；
- 这 6 个工具在管理端**确实有实现**——它们是 `core/manager/biz/aiops/tools` 里的 BaseTool，由 `Registry.BuildBaseTools()` 装配，节点侧的 PiG 扩展 schema 也是从这份 `Info()` 生成的；
- 但节点 agent 上呼的落点是 `cmd/opskeeper` 的 `agentToolUpcall.dispatchAgentTool`，那里只有两条去路：中间件 family（前缀路由）与闭包注册表 `Registry.Invoke`。闭包注册表里没有这 6 个名字。

于是这 6 个名字在节点侧被 manifest 允许、在 schema 里可见、在调用时统一落到 "no such tool"。这不是"还没写"，是**写好的那一半没有被接上**——G0 的准确形状。

G0 之所以能一直存在，是因为现有检查问的都是"这个工具在不在"，没有一个问"这个包声明的工具**有没有人服务**"。声明与可服务之间的空档正好落在两套互不相干的机制上：包声明在 `pig-ops.yaml`，可服务面在 Go 侧的路由里，两边没有任何一处同时看它们。

§6.2 的 D-2 裁决已经给出结论：**用 BaseTool 桥**（一实现两面）。桥不是第四套工具实现，而是把已经存在的那一套（`BuildBaseTools()` + decorator 链）接到上呼通道上——闭包注册表与 BaseTool 是同一批业务能力的两种装配形态，节点侧的 schema 来自后者，那么上呼就必须落到后者，否则节点拿到的 schema 与实际参数形状会分叉。

本切片同时补上缺口之所以能长期存在的那个原因：**对账闸门**。修好 G0 是一件事，让 G0 下次不能悄悄回来是另一件，而后者的成本只是一个可执行的断言。

## What Changes

- 新增 `cmd/opskeeper/basetool_upcall.go`：`baseToolUpcall` 把管理端的 BaseTool 桥接到节点上呼通道。
  - 桥从**独立的** `Registry.BuildBaseTools()` 实例取工具（不复用 MCP/对话用的那两个 bag），因此桥只承载注册表自身的工具面，`host_*` 这类节点本地技能面不会因为复用 bag 而被抬成上呼可服务。
  - 收敛口径是 `Info().Class == "read"`，与 `runMiddlewareTool` 对中间件面的做法一致：上呼通道没有审批队列，只读是这条通道的**前提**而不是事后过滤。
  - 非读类工具与显式保留名单（`host_bash`、`ToolSearch`、`chat_to_query`、`draft_config_change`）进入"知道但不服务"集合，被问到时给出**为什么**而不是 "no such tool"。
  - 复用 `decorators.Wrap` 的同一条装饰器链（timeout / ratelimit / metric），`Deps.Audit` 留空——本通道的账本由 `agentToolAudit` 写，再写一次 `chat_tool_calls` 是同一件事记两遍；`ReviewSpawner` 不接，因此链上没有审批闸门（本通道只读）。
  - `MarkUntrustedOutput` 由 `BuildBaseTools()` 内部完成，桥不再包第二层。
- 修改 `cmd/opskeeper/main.go`：
  - 桥在 `toolWiring.apply(toolsReg)` **之后**装配——早于此处，`BuildBaseTools()` 看不到后置 setter 注入的依赖，桥会静默少一批工具。
  - `dispatchAgentTool` 改为按 `中间件 family → 桥 → 闭包注册表` 的顺序分派，并把**实际服务了这次调用的面**返回给审计。
  - 同名工具以**桥（BaseTool）优先、闭包兜底**：节点侧 schema 由 BaseTool 的 `Info()` 生成，闭包与 BaseTool 的参数形状已经分叉（闭包取 `edge_name`，BaseTool 取 `device_ids[]`），若上呼落到闭包，节点发来的参数会被按另一种形状解析。
- 修改 `cmd/opskeeper/agenttool_audit.go` 与 `agenttool_audit_test.go`：审计行的 `surface` 增加 `basetool` 一类；`surface` 的取值改为由分派结果给出，而不是由工具名推断。
- 新增对账闸门测试（`cmd/opskeeper/plugin_gap_gate_test.go`）：
  - 断言形式：**每个包的 manifest 工具名 ∈（上呼可服务名 ∪ 节点本地技能名）**。
  - 两侧名单都来自真实构造（上呼侧由全依赖 fake 下 `BuildBaseTools()` + 桥装配得到；本地侧由 `skill.All()` 得到），**禁止手抄字面量**——手抄出来的名单正是 G0 本身的成因。
  - 负例真红：把桥从可服务集合里拿掉（即复原 G0 前的状态），闸门必须报出且只报出那 6 个工具。
- 新增 fake tunnel 集成测试：6 个 G0 工具各自经上呼成功返回非空 JSON，且审计行 `surface=basetool`。
- 新增 `openspec/changes/plugin-gap-slice-s1/`，本 change 即 §5.5 切片 S-1 的实施记录。

### 明确不在本切片内

- **不改任何 `pig-ops.yaml`**。5.1–5.4 的 manifest 增量属于 S-2/S-3；本切片只保证"已经声明的能服务"，不新增声明。
- **不做反向断言**（"上呼可服务 ⊆ manifest 声明"）。桥按只读类收敛，管理端还有一批只读工具不在任何包的声明里；反向断言会把"管理端有工具"错误地判成违规。§7 的 `rank_edges`、`correlate_incident` 同样不因此进入任何 manifest。
- **不改闭包注册表**。桥不改 `Registry.Invoke` 的任何行为，闭包仍是它自己的那套分派。
- **不实现"已声明但依赖缺失"的降级**（如 `r.knowledge == nil` 时 `query_knowledge` 整体不在 bag 里）。那是 S-3 的"就地降级"改造；本切片只保证桥的**措辞**不把"这里没有"说成"不存在"——见 design.md 的「找不到 vs 没配置」一节。
- 不新增 `host_*` 的上呼路径：主机探针是节点本地技能面，由节点自己服务。

## Capabilities

### New Capabilities

- `plugin-gap-slice-s1`：节点上呼通道的 BaseTool 可服务面契约（只读收敛、保留名单、同名优先级、审计 surface 取值），以及包声明与可服务面的双向对账闸门。

### Modified Capabilities

- 无。本仓库尚无归档的主规格（archived main specs），本切片修复既有实现之间的空档，不改变已归档能力的对外语义。

## Impact

- `cmd/opskeeper/basetool_upcall.go`：新增文件，桥的构造、分派与名单。
- `cmd/opskeeper/main.go`：`agentToolUpcall` 增加 `basetool` 面；`dispatchAgentTool` 返回实际服务面；装配点后移依赖 `toolWiring.apply`。
- `cmd/opskeeper/agenttool_audit.go`：`surface` 取值集合与注释。
- `cmd/opskeeper/agenttool_audit_test.go`、`cmd/opskeeper/basetool_upcall_test.go`、`cmd/opskeeper/plugin_gap_gate_test.go`：新增/更新测试。
- 模块图不变：`cmd/opskeeper` 与 `core/manager`、`core/floor` 的依赖关系没有新增边。
- 无数据迁移、无部署迁移、无配置项变更。回滚即 revert 单个 PR；回滚后 6 个工具回到 "no such tool"，其余行为不变。
