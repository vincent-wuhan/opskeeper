# 4.8 observability / readonly / middleware 插件业务缺口盘点与补齐方案

> 状态：**方案 v0.1（待评审）**。对应 `tasks.md` 4.8；评审通过前不勾选 4.8（与 4.1 / 4.3–4.7 的处理一致）。
>
> 基线：`main@07499880`（PR #109 之后）；旧 release `release-v2026.09.20-patch.2`（commit `8774f7b3`）。
>
> 范围：**仅盘点与方案产出**。本文档回答"旧 release 的运维工具现在恢复到什么程度、哪些该补、补到哪个包、怎么测、怎么回滚"，并给出实现切片建议（不在本任务内合入）。任何对 `plugins/pig-ops/`、`core/manager/biz/aiops/tools/`、`core/floor/pluginmanifest/` 的真实改动必须另开 change 并独立评审。

## 1. 目标与边界

### 1.1 目标
对照旧 release 的运维工具清单与当前 5 包 94 工具（readonly 20 / observability 12 / middleware 56 / repair 5 / autonomy 1），逐项判定恢复状态，并对"未恢复且归本任务"的项给出补齐方案。已知至少含 `query_knowledge`（4.2 归"迁移为插件（候选）→ 4.8"）。

### 1.2 与 4.1 §7、4.2 的一致边界
- **告警规则 / 草稿 / 降噪归 4.12**：本任务不动告警规则体系；readonly 包内既有的 `query_alert_rules`（只读查询）维持现状，不扩展写路径。
- **4.2 四分类**：知识检索 `query_knowledge` 归"迁移为插件（候选）→ 4.8"；技能（`install_skill`）与外部 MCP（`mcp__*`）归"暂缓迁移"，本任务不触碰；demo 归"淘汰"。
- **修复插件强化归 4.9**：中间件写类工具的审批 / 回滚证据不在本任务。
- **拓扑 / CMDB / Git 第三方 adapter 归 4.4**；出站通知归 4.5；不重复不越界。
- **内核契约**：不在 `core/floor/kernel/kind.go` 新增 Kind；`tool` 扩展点的注册形态（manifest `spec.tools` + 节点 gate allow-list）不变。

### 1.3 非目标
- 不实现代码合并（实施另批，见 §10 切片建议）。
- 不做告警规则 / 草稿（4.12）；不动 provider / notifier / collector（4.5–4.7 已有方案）。
- 不改内核；不改 `datasource.go` / `sources.go` 主流程。
- 不改 manager 侧 chat / MCP 面的既有行为——本任务只补"节点插件面"的缺口，manager 面的等价能力均已存在。

## 2. 盘点口径与证据链

### 2.1 三个"面"
旧 release 与 main 的同一工具名可能出现在不同表面。盘点把"现状"拆成三列，避免把一个面的恢复误读成另一个面的恢复：

| 面 | 定义 | main 证据 |
|---|---|---|
| **节点插件面** | 5 个 `pig-ops` 包 manifest `spec.tools` 声明、节点 gate 放行的工具（节点 agent 可调用） | `plugins/pig-ops/*/pig-ops.yaml`，共 94 条 |
| **manager 编译内面** | `aiops/tools` 注册表内的工具（manager chat / MCP / 流程面可调用） | `core/manager/biz/aiops/tools/registry.go` + `registry_basetool.go` |
| **节点上呼面** | manager 经 `agent.tool` 隧道方法**实际可服务**的工具（节点 agent 经 broker 上呼到达的闭包注册表） | `cmd/opskeeper/main.go` `agentToolUpcall` → `a.reg.Invoke`；闭包注册表共 **17** 个工具（`NewRegistry` 15 + `SetPluginConfigLister` 2） |

### 2.2 判定类别
- **已恢复**：节点插件面有同名工具（manager 面同时保留）。
- **等价恢复**：节点插件面无同名工具，但存在语义等价的新工具（列出对应关系）。
- **未恢复（归本任务）**：节点插件面缺失、且判定应补 readonly / observability / middleware 之一。
- **归属其他任务**：4.4 / 4.5 / 4.9 / 4.12 / `runtime-management-gap` / TeamHarness 等已有归属。
- **按设计不补 / 淘汰**：与既有工具重复、违反窄权边界、或按 `docs/opskeeper2-architecture.md` §九 宿主保留（写明理由）。
- **暂缓**：4.2 §3 已明确暂缓（技能、MCP）。

### 2.3 证据命令
```
git show release-v2026.09.20-patch.2:internal/manager/biz/aiops/tools/registry.go          # 旧闭包注册
git show release-v2026.09.20-patch.2:internal/manager/biz/aiops/tools/registry_basetool.go # 旧 BaseTool 袋
git grep -n 'ToolName[A-Za-z]* = "' release-v2026.09.20-patch.2 -- internal/manager/biz/aiops/tools
git grep -nE 'makeTool\("[a-z0-9_.]+"' release-v2026.09.20-patch.2 -- internal/middleware/adapter
grep -oE '\{ name: [a-zA-Z0-9_.]+,' plugins/pig-ops/*/pig-ops.yaml                          # 现 manifest
grep -n 'Name:.*ToolName' core/manager/biz/aiops/tools/registry.go                           # 现上呼闭包注册
```

## 3. 盘点对照表

### 3.1 manager 侧 aiops 具名工具（旧 release 38 个具名 + `mcp__*` 动态族）

"上呼"列 = 节点上呼面是否可服务；"包"列 = 所在插件包。

| 旧工具 | class | 包（节点插件面） | 上呼 | 判定 |
|---|---|---|---|---|
| get_host_load | read | observability | ✓ | 已恢复 |
| query_promql | read | observability | ✓ | 已恢复 |
| list_metric_catalog | read | observability | ✓ | 已恢复 |
| list_database_sources | read | observability | ✓ | 已恢复 |
| analyze_database_status | read | observability | ✓ | 已恢复 |
| query_logql | read | observability | ✓ | 已恢复 |
| query_traceql | read | observability | ✓ | 已恢复 |
| list_repo_sources | read | observability | **✗** | 已上 manifest，上呼无 dispatch —— **G0**（§4） |
| read_source | read | observability | **✗** | 同上，**G0** |
| grep_source | read | observability | **✗** | 同上，**G0** |
| query_change_events | read | observability | **✗** | 同上，**G0** |
| get_edge_summary | read | observability | ✓ | 已恢复 |
| get_topology | read | readonly | ✓ | 已恢复 |
| find_outlier_edges | read | readonly | ✓ | 已恢复 |
| expand_topology | read | readonly | **✗** | 已上 manifest，上呼无 dispatch —— **G0** |
| find_topology_node | read | readonly | **✗** | 同上，**G0** |
| query_alert_rules | read | readonly | ✓ | 已恢复（写路径归 4.12，不动） |
| get_host_processes | read | （无） | ✓ | **未恢复 → 归本任务**（§5.3） |
| query_devices | read | （无） | ✓ | **未恢复 → 归本任务**（§5.4） |
| rank_edges | read | （无） | ✓ | **不补**：与 readonly `find_outlier_edges`（z-score 离群）+ `query_promql` 能力重叠，补齐即近重复工具（§7） |
| query_incidents | read | （无） | ✓ | **未恢复 → 归本任务**（§5.2） |
| get_incident_detail | read | （无） | ✓ | **未恢复 → 归本任务**（§5.2） |
| correlate_incident | read | （无） | ✓ | **不补**：组合面归 agent 编排（investigator persona / loop），四个输入工具均已在 observability 包内（§7） |
| query_knowledge | read | （无） | **✗** | **未恢复 → 归本任务**（§5.1；4.2 已归 4.8） |
| host_find_large_files | read | （无，等价 host_file_inventory） | ✗ | **等价恢复**：readonly `host_file_inventory` / `host_grep_file` / `host_tail_file` 覆盖空间定位与读取场景；manager 面编译内保留 |
| host_du_summary | read | （无，等价 host_file_inventory） | ✗ | **等价恢复**：同上 |
| host_stat_file | read | （无） | ✗ | **等价恢复（部分）**：单文件 stat 语义由 `host_file_inventory` + `host_grep_file` 近似；精确 inode/权限查询低频，不为此单开工具（§7） |
| draft_config_change | read | repair | ✗（走 repair 审批通道） | 已恢复（repair 包，L2） |
| apply_config_change | write | repair | 同上 | 已恢复 |
| host_restart_service | write | repair | 同上 | 已恢复 |
| recovery.execute | write | repair | 同上 | 已恢复 |
| verify_recovery | read | repair | 同上 | 已恢复（4.9 再强化） |
| host_bash | read→write | （无） | ✗ | **按设计不补**：通用 shell 通道与窄权修复边界冲突；写路径已被 `host_restart_service` / `recovery.execute` 收敛，人工通道由 WebSSH（宿主保留）承担（§7） |
| cloud_bash | write | （无） | ✗ | **按设计不补**：同类理由；云上结构化操作归 4.4 adapter 候选（§7） |
| chat_to_query | read | （无） | ✗ | **按设计不补（插件面）**：节点 agent 本身就是 LLM 调用者，二级"自然语言→查询语言"翻译工具在插件面冗余；manager 面编译内保留（决策 350 已接线） |
| send_im_message | write | （无） | ✗ | **归属 4.5**（notifier 出站渠道插件化） |
| serve_page | write | （无） | ✗ | **宿主保留**（4.1 §① 工作台；托管页面是控制台能力） |
| install_skill | write | （无） | ✗ | **暂缓**（4.2 §3：技能宿主内管理） |
| AgentTool / SendMessage / TaskStop | meta | TeamHarness（包资源） | ✗ | **已由新架构承接**：7 角色 Worker 已包化（`plugins/opskeeper-teamharness`）；任务停等控制面归 `runtime-management-gap` 2.4 |
| ToolSearch | meta | （无） | ✗ | **不补**：节点包工具集小而静态，manifest 即清单；meta 检索属 manager 大菜单问题（§7） |
| `mcp__*`（动态族） | 混合 | （无） | ✗ | **暂缓**（4.2 §3：MCP 宿主内注册） |

### 3.2 中间件 adapter 工具（旧 64 → 现节点包 56 / 现 adapter 注册表 102）

现 `core/manager/middleware/adapter` 注册 102 个（git 8 / host 11 / k8s 24 / kafka 6 / mq 6 / pg 23 / rabbitmq 5 / redis 19），节点 `opskeeper-sre-middleware` 包声明其中 **56 个只读/诊断类**。逐族对照：

| 族 | 旧（64 中） | 包内现状 | 未上包的旧工具 | 判定 |
|---|---|---|---|---|
| git | 8 | 1/8（`git.find_runtime_link`） | `git.connect` `git.list_repos` `git.commit_history` `git.file_at_commit` `git.blame` `git.diff` `git.search_code` | **不补（重复）**：读仓库内容的语义已由 observability 包 `list_repo_sources` / `read_source` / `grep_source`（gitartifact 同源克隆）覆盖；同仓两套工具名会造成双实现漂移。结构化 git 历史（blame/diff/commit）作为 4.4 git adapter 第三方包的能力评审 |
| k8s | 13 | 9/13 旧全上 + 5 个新增（`k8s.connect` `pvc_list` `pvc_usage` `rollout_status` `top_pods`） | `k8s.scale` `k8s.rollout_undo` `k8s.cordon` `k8s.exec_into_pod`（均写/执行类） | **归属 4.9**：写类工具按设计不经 L1 中间件包直通；`agentToolUpcall.runMiddlewareTool` 显式拒写（"reachable through the approved remediation path"）。是否成为 `recovery.execute` 预留动作由 4.9 裁决 |
| pg | 18 | 14/18 旧读全上 + `pg.replication_status` 新增 | `pg.analyze_table` `pg.cancel_query` `pg.kill_session` `pg.vacuum_table`（写类） | **归属 4.9**（同上；loop 的 `RegistryInvoker` 已是这些写工具的审批派发路径） |
| redis | 15 | 13/15 | `redis.config_set` `redis.flushdb`（写/破坏类） | **归属 4.9** |
| kafka | 5 | 5/5 | — | 已恢复 |
| rabbitmq | 5 | 4/5 | `rabbitmq.purge_queue`（破坏类） | **归属 4.9** |

**未上包的 18 个旧工具全部是写 / 执行 / 破坏类或与既有读工具重复**，没有一个只读缺口遗留——中间件只读面相对旧 release 无未解释遗漏。main 新增的 38 个 adapter 工具（host 11、k8s 11、mq 6、pg 5、redis 4、kafka 1）中 10 个已上包，其余 28 个同属写类 / 待 4.9 或后续 host 族评审（host 族为 main 新增能力，非旧 release 缺口，不计入本盘点缺口）。

### 3.3 edge 执行面支撑（旧 release 的 RPC handler）

| 旧 handler | 支撑的旧工具 | main 对应 | 判定 |
|---|---|---|---|
| `edgeagent/bash` | host_bash | `core/edge/bash` | 随 host_bash 判定（按设计不补节点面；宿主保留） |
| `edgeagent/host_files`（3 方法） | host_find_large_files / du_summary / stat_file | `core/edge/host_files` + readonly `host_file_inventory` 等 | 等价恢复（见 §3.1） |
| `edgeagent/restart_service` | host_restart_service | `core/edge/restart_service` + repair 包 | 已恢复 |
| `edgeagent/webshell` | （人工通道） | `core/edge/webshell` | 宿主保留（高权限通道，4.1 §③） |

节点本地探针族（`host_dmesg` / `host_lsof` / `host_strace` / `host_probe_*` / `host_read_journal` / `host_sosreport` / `host_mtr` / `host_traceroute` / `host_netns_inspect` / `host_grep_file` / `host_tail_file` / `host_top_processes` 共 15 个）为 **main 新增能力**（旧 release 无），已随 readonly 包交付，不构成缺口。

### 3.4 盘点结果汇总

| 口径 | 数量 |
|---|---|
| 旧 release 运维工具（aiops 具名 38 + 中间件 64 + edge handler 面） | 102+ |
| 已恢复（同名上包） | aiops 16 + 中间件 46 = 62 |
| 等价恢复 | 3（host 文件三件套） |
| 未恢复且归本任务 | **5 个工具**：`query_knowledge`、`query_incidents`、`get_incident_detail`、`get_host_processes`、`query_devices` |
| 盘点新发现的交付缺口 | **G0 双向对账缺口**：6 上manifest未服务 + 6 已服务未上manifest（§4） |
| 归属其他任务 | 4.4（git 结构化 / 云）、4.5（send_im_message）、4.9（中间件写类 18 + main 新增写类）、4.12（告警写路径）、runtime-management-gap 2.4（TaskStop 族）、TeamHarness（AgentTool 族） |
| 按设计不补 / 淘汰 / 暂缓 | rank_edges、correlate_incident、host_bash、cloud_bash、chat_to_query、ToolSearch、serve_page（宿主保留）、install_skill、`mcp__*`（暂缓） |

**无未解释遗漏**：旧 release 每个运维工具都落在上表某一判定内。

## 4. 缺口 G0：节点上呼通道的双向对账缺口（盘点新发现）

### 4.1 现象
节点插件面的控制面读工具走"broker → 上呼 `agent.tool` → manager `a.reg.Invoke`"链路（`cmd/opskeeper-edge/tools.go` 本地 miss 即上呼）。而 `agentToolUpcall` 的 aiops 分支只查**闭包注册表**（17 个，`registry.go` `NewRegistry` + `SetPluginConfigLister`）；`BuildBaseTools` 产出的 BaseTool 袋只喂 manager chat / MCP / 流程面，**不进上呼通道**。于是：

| 方向 | 工具 | 后果 |
|---|---|---|
| **manifest 已声明、上呼无 dispatch**（6） | `query_change_events` `list_repo_sources` `read_source` `grep_source`（observability）；`expand_topology` `find_topology_node`（readonly） | 节点 agent 调用 → manager 返回 "no such tool"。模型看得见、永远调不通 |
| **上呼可服务、manifest 未声明**（6） | `query_devices` `rank_edges` `query_incidents` `get_incident_detail` `correlate_incident` `get_host_processes` | 节点 gate 在每一轮拒绝（allow-list 即 manifest），dispatch 白白可达 |

这与决策 350（"标着已交付却从未装上的工具"）同类：**清单与接线各说各话**。四道既有闸门都没拦住它：sdk registry 校验查"扩展声明 ⊆ manifest"；toolset 漂移测试查"tools.go = registry Info()"；profile 组合测试查"包 ⊆ profile"；没有一个查"manifest ⊆ manager 可服务名 ∪ 节点本地技能名"。

### 4.2 修复原则（并入 §5.5 切片）
1. **一个实现、两个面**：上呼通道增加第三个 surface（BaseTool 桥），复用 `BuildBaseTools` 的同一批工具与同一组 decorator（含 `MarkUntrustedOutput`），不在闭包注册表里手抄第二份实现——两份实现正是命名 / 语义漂移的温床。
2. **对账闸门测试**：新增"manifest 工具名 ⊆（节点本地技能名 ∪ manager 上呼可服务名）"的静态对账测试（负例：删掉任一侧即红），与既有 `TestTheObservabilityProfilesDeclaredToolsAreExactlyWhatItShipsToTheModel` 形成四向对账。
3. **审计面**：`agentToolSurface` 增加 `"basetool"` 归类，审计行继续区分"回答 fleet 问题的工具"与"伸进客户系统的工具"。

## 5. 补齐方案（归本任务的 5 项）

### 5.0 通用生成链（所有补齐项共用）
1. manifest：`plugins/pig-ops/<pkg>/pig-ops.yaml` 增 `spec.tools` 条目（保持排序、逐条注释）；`metadata.version` 0.2.0 → **0.3.0**（菜单新增即 schema 增量，`install.strategy: rolling` 语义不变）。
2. 工具清单：observability / middleware 走生成链 `OPSKEEPER_UPDATE_TOOLSET=1 go test ./core/manager/biz/aiops/tools/ -run Toolset` → `scripts/sync-pig-ops.sh`；readonly 的 `core/pig/extensions/opskeeper-sre-readonly/tools.go` 为手工清单，按 `Info()` 原样抄录 schema（漂移由 `TestThePackagedToolsetMatchesTheCanonicalSource` + sdk 校验兜住）。
3. profile / 测试同步：`core/floor/pluginmanifest/{observability,profile}_test.go` 的计数断言（"twelve tools"）、persona SKILL 覆盖说明同步更新。
4. 闸门命令：`make pi-plugin-guard && make module-check && go test ./scripts/ledgercheck`（目标名以 Makefile 为准）。

### 5.1 `query_knowledge` → observability 包
- **归属理由**：知识检索是控制面数据读（Qdrant + knowledge biz 在 manager），与 observability 包"回答来自控制面"的 charter 一致；该包已承载同源的仓库读三件套（`list_repo_sources` / `read_source` / `grep_source`，gitartifact 与 KB 同属知识域）。候选对比见 §6.1。
- **工具签名**：沿用 `core/manager/biz/aiops/tools/query_knowledge_basetool.go` 的 `Info()`（query / path_prefix / tags / max_results，默认 5；预览每条 ≤800 runes）。class=read。
- **scope 对照**：新增 `knowledge.read`（`core/domain/plugin.go` `ScopeKnowledgeRO`），manifest `required_scopes += knowledge.read`（与既有 `observability.read / db.read / alert.read` 并列）。**宿主 Policy 增量**：`profiles.go` 的 finance 与 SaaS 两个 profile `Granted` 都必须加 `knowledge.read`，否则 admission 拒绝（`core/edge/pluginhost/host.go` 记录过"not granted"的真实节点教训）；只读 scope，不触碰两个 profile 的 NotGranted 清单语义。
- **manifest 增量**：observability `pig-ops.yaml`：tools += `{ name: query_knowledge, class: read, limits: {output_bytes: 262144, timeout_seconds: 60} }`（KB 检索结果受 max_results 与预览上限双重约束）；version 0.3.0。
- **dispatch 增量**：上呼通道需能服务（走 §5.5 BaseTool 桥；`r.knowledge` 为 nil 的部署按既定降级语义返回 "knowledge service not configured"）。
- **安全标注**：KB 内容是外部文本，上呼桥必须保留 `promptguard.KindSource` 标注（`untrusted_sources.go` 表已有该工具）。
- **测试**：见 §8；回滚见 §8.3。

### 5.2 `query_incidents` + `get_incident_detail` → readonly 包
- **归属理由**：事件读与既在 readonly 的 `query_alert_rules` 同域（"what is firing / what happened"），且 readonly 已持有 `alert.read` scope；observability 的 change-events 先例不改变"事件时间线属告警域"的划分（差异在 §6.3 备选说明）。
- **工具签名**：沿用 `alerting.NewQueryIncidentsTool` / `NewGetIncidentDetailTool` 的 `Info()`（列表：severity / status / edge / 时间窗 / limit；详情：incident_id 全量时间线）。class=read。
- **scope 对照**：`alert.read` ⊆ readonly 既有 `required_scopes`（host.read / topology.read / alert.read）⊆ finance 与 SaaS profile Granted——**零 profile 增量**。
- **manifest 增量**：readonly `pig-ops.yaml`：tools += 两条（`query_incidents`、`get_incident_detail`，class read，`limits: {output_bytes: 262144}`）；version 0.3.0；`core/pig/extensions/opskeeper-sre-readonly/tools.go` 手工同步两条 spec。
- **测试 / 回滚**：见 §8。

### 5.3 `get_host_processes` → observability 包
- **归属理由**：与既在 observability 的 `get_host_load` 成对（"任一主机的负载 / 进程"，参数带 device_id 上呼目标主机）；readonly 的 `host_top_processes` 只答本机，语义不覆盖跨主机清单。
- **工具签名**：沿用 `host.NewGetProcessListTool` 的 `Info()`（device_ids[] / 过滤 / limit）。class=read。
- **scope 对照**：`observability.read` ⊆ 既有 required_scopes——**零增量**。
- **manifest 增量**：observability tools += 1 条（class read，`limits: {output_bytes: 262144, timeout_seconds: 60}`）；version 0.3.0；生成链自动带出。

### 5.4 `query_devices` → readonly 包
- **归属理由**：设备清单是拓扑域的入口读（topology 三件套 + `get_edge_summary` 都需要 device_id 先行）；readonly 已持有 `topology.read`。
- **工具签名**：沿用 `NewQueryEdgesTool` 的 `Info()`（role / status / last_seen_within_minutes / name_contains / ip_contains / limit；返回 device_id 数组并在描述中约束后续查询用 device_id 而非 edge_id）。class=read。
- **scope 对照**：`topology.read` ⊆ readonly 既有 required_scopes——**零增量**。
- **manifest 增量**：readonly tools += 1 条（class read，`limits: {output_bytes: 262144}`）；version 0.3.0；tools.go 手工同步。

### 5.5 G0 对账闸门 + BaseTool 上呼桥（S-1，先于 5.1–5.4）
- **内容**：§4.2 三条原则的落地切片。先修 6 个"已声明未服务"工具的 dispatch，再补 5.1–5.4 的 manifest（顺序保证"补一个、通一个"，新工具不再带着 G0 上线）。
- **测试**：新增对账测试（manifest 名单 vs 可服务名单双向断言 + 负例）；`agenttool_audit` 的 surface 断言扩展 `"basetool"`。
- **回滚**：桥是加法（新 surface 分支），不改 17 个闭包工具的既有路径；出问题按包版本回退（§8.3）。

## 6. 候选方案对比与取舍（ADR）

### 6.1 `query_knowledge` 的归属（决策项 D-1）
- **背景 / 约束**：知识检索归 `tool` 扩展点（4.2）；目标包限定 readonly / observability / middleware（本任务范围）；scope 必须是宿主 profile 已授予集合的子集，新 scope 需 profile 增量。
- **方案 A（采用）：observability + 新 `knowledge.read` scope**。控制面数据读与其 charter 一致；与仓库读三件套同包同域。代价：observability 的 scope 集从 3 个变 4 个，`TestTheObservabilityProfileAsksForReadScopesAndNothingWider` 的哲学注释需要同步（knowledge.read 与 alert.read 同为"答案所解释的参照系"类读）。
- **方案 B（放弃）：新建 `opskeeper-sre-knowledge` L1 包**。边界最纯（一个 scope 一个包），但为 1 个工具开第 6 个包：组合 profile、persona、courier 扩展、市场条目全套治理成本，收益只在对 knowledge 有独立授予需求的部署——当前无此用例。**被放弃原因：包增殖成本 > 边界收益**（元认知检查：不把"分类学上的纯净"当成"架构上的必要"）。
- **方案 C（放弃）：不补，仅 manager 面保留**。节点 investigator 是知识回流的主要消费者（复盘 SKILL 明确先查 KB），不补则节点面每次都要回退通用诊断，与"闭环知识回流"的目标相悖。4.2 已将其归 4.8，无"暂缓"依据。

### 6.2 G0 修法（决策项 D-2）
- **方案 A（采用）：上呼通道加 BaseTool 桥 surface**。一个实现两个面，decorator（审计 / 超时 / 限流 / untrusted 标注）复用。代价：`agentToolUpcall` 多一个依赖（bag 的构造时机在 tunnel 之后，需在 `main.go` 装配根补接线——决策 350 的教训正是这类"最后二十行"）。
- **方案 B（放弃）：闭包注册表逐个补 `r.Register`**。改动小但制造第二份实现，schema / 降级语义 / untrusted 标注三处都会漂移；`registry.go` 的闭包路径本来就是被 BaseTool 收编中的历史面。

### 6.3 事件读归属（决策项 D-3）
- **方案 A（采用）：readonly**（`query_alert_rules` 先例、alert.read 已持有）。
- **方案 B（放弃）：observability**（`query_change_events` 先例）。两个先例本身已分裂；本方案按"告警 / 事件时间线归 readonly，历史遥测归 observability"收敛，不再扩大分裂面。**留待评审**：若评审认为应统一迁到 observability，5.2 与 5.4 的目标包对调即可，其余不变。

## 7. 判定不补清单（理由，非硬凑）

| 工具 | 理由 | 反悔条件 |
|---|---|---|
| rank_edges | 与 `find_outlier_edges` + `query_promql` 组合能力重叠；补齐即维护两个近同义工具 | 出现"排序清单"高频用法且模型组合失败率可测量时再评 |
| correlate_incident | 组合编排属 agent 层（investigator persona / loop），四个输入工具均已在 observability；固化进工具层会双实现漂移 | 节点面多工具往返的 token / 时延成本有实测数据时，作为 observability v2 的组合读再评 |
| host_bash / cloud_bash | 通用 shell 违反窄权修复边界（L2 的 `max_blast_radius: pod` 与"每个动作只作用于一个具名目标"的包承诺容不下任意命令）；人工通道 WebSSH 宿主保留 | 4.9 若引入"预声明命令白名单"式修复动作，按 repair 包新工具单独评审 |
| chat_to_query（插件面） | 节点 agent 即 LLM 调用者，二级翻译冗余；manager 面保留 | 无 |
| ToolSearch | 节点包菜单小而静态（≤32/包），manifest 即清单 | 包菜单规模接近 manager 面（30+）时再评 |
| host_stat_file | 精确单文件元数据低频，`host_file_inventory` + `host_grep_file` 近似覆盖 | 有真实用例（如权限漂移巡检）再单开 |
| git.connect 等 7 个读工具 | 与 observability 仓库读三件套语义重复 | 4.4 git adapter 第三方包评审时按结构化历史能力决定 |
| serve_page / send_im_message / install_skill / `mcp__*` | 宿主保留 / 4.5 / 暂缓（4.2 §3） | 随归属任务 |

## 8. 风险分级与测试策略

### 8.1 风险分级
| 级 | 风险 | 缓解 |
|---|---|---|
| **高** | G0 桥接线重复决策 350（"装了没接"） | S-1 首切片即建对账闸门测试（负例真红）；deadcode 闸门已修方法分支，setter 无调用方会被抓 |
| **高** | `knowledge.read` profile 增量遗漏导致全包拒绝 | `TestEveryComposedPackageIsActuallyAdmittedByItsProfile` 已有；新增 scope 同步进两个 profile 的断言 |
| **中** | 事件读归属（D-3）评审推翻 | 方案按目标包对调即可重构，工具签名 / 测试不变 |
| **中** | readonly 菜单从 20 → 23 触发模型选择质量下降 | persona SKILL 更新使用边界；菜单仍远低于 deferral 阈值 30 的量级；E2E 用假模型断言 advertised 数 |
| **低** | 版本 0.3.0 与滚动策略的组合让旧节点新节点菜单不一致 | 全部为 L1 只读新增，旧节点只是"少一个工具"，无兼容破坏；canary 波语义照常 |

### 8.2 测试策略
| 层 | 用例 | 通过条件 |
|---|---|---|
| Unit（pluginmanifest） | manifest ↔ tools.go ↔ registry Info() 三向一致（生成链 + 既有漂移测试） | 新工具名三处一致 |
| Unit（新增） | G0 对账：每个 manifest 控制面工具 ∈ 上呼可服务名 ∪ 本地技能名；负例（移除一侧）真红 | 全覆盖 |
| Unit（domain/profiles） | `knowledge.read` 进两 profile Granted；composition 测试绿 | finance / SaaS 均可装 |
| Integration（edge→manager） | fake tunnel 下 broker 上呼 5 个新工具 + 6 个 G0 修复工具 | 返回非空 JSON、审计行 surface 正确 |
| E2E | 沿用 `tests/e2e/node_agent_tool_call_test.go` 形态，脚本化调用 `query_knowledge` 与 `query_incidents` | 工具结果回传模型；无 REFUSED |
| 回归 | `make pi-plugin-guard && make module-check && go test ./scripts/ledgercheck` | 全过 |

### 8.3 回滚方案（按包）
1. **回滚粒度**：包级（readonly / observability 各自独立），不依赖代码回退——插件发布走既有 marketplace / release 通道。
2. **步骤**：发布 0.2.0 回退版本（或停用 0.3.0 release）→ 节点 reload 后 manifest 回到 20 / 12 工具，gate 对新工具名自然拒绝（fail-closed 是默认行为，无需清理）。
3. **G0 桥**：若 BaseTool 桥本身出问题，回退 manager 二进制即回到 17 个闭包工具的现状面（桥是加法分支）；已写审计行保留。
4. **`knowledge.read` scope**：包回退后 scope 成无人消费者，profile 中保留无害；若要清理，随下一版 profile 收敛（scope 授予是加法，单独回退无副作用）。
5. **不变量**：回退不破坏 0.2.0 的任何行为；manager 面编译内工具全程不动。

## 9. 与 4.1 / 4.2 一致性自查表

| 自查项 | 出处 | 本方案是否一致 | 证据 |
|---|---|---|---|
| 告警规则 / 草稿归 4.12 | 4.1 §7 结论 5 | ✓ 只读 `query_alert_rules` 维持现状，不扩写路径 | §1.2 / §3.1 |
| 知识检索工具归 4.8 | 4.2 §2（候选 → 4.8） | ✓ 5.1 为首要补齐项 | §5.1 |
| 嵌入模型本期编译内 | 4.1 §7 结论 3 | ✓ 不动 embedding；query_knowledge 复用既有 KB 服务 | §5.1 |
| 技能 / MCP 暂缓 | 4.2 §3 | ✓ install_skill / `mcp__*` 不触碰 | §3.1 |
| 拓扑工具接入待盘点 | 4.1 §6 | ✓ expand/find_topology_node 判 G0（声明在、dispatch 缺），修复归本任务 | §4 / §5.5 |
| 六扩展点封闭 | kernel kind 六元集合 | ✓ 不新增 Kind，全部走 `tool` 既有形态 | §1.2 |
| scope ⊆ 宿主 Policy GrantedScopes | 4.3–4.7 纪律 | ✓ 4 项零增量；1 项（knowledge.read）显式给 profile 增量 | §5.1–5.4 |
| manifest 实测闸门 | 4.6 / 4.7 纪律 | ✓ 新增对账闸门有负例；闸门命令沿用 Makefile 真实目标名 | §5.5 / §8.2 |
| 修复强化归 4.9 | 4.1 §4⑥ | ✓ 中间件写类 18 个全部归 4.9，不在本任务 | §3.2 |
| provider / notifier / collector 不越界 | 4.5–4.7 | ✓ 未触碰 | §1.3 |

## 10. 实现切片建议（不在本任务合入）
- **S-1（先行）**：G0 BaseTool 上呼桥 + 对账闸门测试 + 6 个已声明工具的 dispatch 修复。
- **S-2**：readonly 0.3.0（query_devices / query_incidents / get_incident_detail）+ persona 更新。
- **S-3**：observability 0.3.0（query_knowledge / get_host_processes）+ `knowledge.read` scope 与两 profile 增量 + persona 更新。
- S-2 / S-3 可并行；S-3 依赖 S-1（query_knowledge 的 dispatch）。每个切片独立 PR、独立回滚（§8.3）。

## 11. 验收清单（对应任务描述）
- [x] 盘点表覆盖旧 release 全部运维工具，无未解释遗漏（§3，含 edge 执行面与 `mcp__*` 族）
- [x] 每个补齐项有 manifest 增量、scope 对照、测试与回滚（§5 各项 + §8）
- [x] 与 4.1 §7（告警归 4.12 等）、4.2 结论一致（§9 自查表）
- [x] 判定不该补的项写明理由而非硬凑（§7，含反悔条件）
- [x] 盘点产出新发现缺口 G0 并给出修复切片（§4 / §10）
- [ ] 实施另批（§10 切片），评审通过后勾选 `tasks.md` 4.8

## 12. 提交与执行规范
- **落盘路径**：本文档 `openspec/changes/plugin-architecture-recovery-plan/plugin-gap-plan.md`。
- **提交语言**：本任务 PR 使用中文注释 / 中文提交说明（按计划任务 7.4）。
- **关联**：OpenSpec change `plugin-architecture-recovery-plan` 任务 4.8；执行任务编号只记录在 PR 描述与评审评论中，**不写入受审计文件**（`scripts/audit_open_source.py` 的 internal task ID 规则会拒绝 `BENY-` / `LUM-` 形态）。
- **勾选状态**：方案 v0.1，评审通过前不勾选 4.8。
