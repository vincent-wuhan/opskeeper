# 4.1 旧 release 业务板块 → 六扩展点映射表（初稿）

> 状态：**初稿，待评审**。对应 `tasks.md` 4.1；评审通过前不勾选 4.1。
>
> 基线：旧 release `release-v2026.09.20-patch.2`（commit `8774f7b3`）对比当前 `main`（commit `86dff884`，2026-10-18）。
>
> 本表只回答两件事：旧 release 每个业务板块的能力在当前 `main` 落在哪里，以及它该挂到哪个扩展点（或按设计留在宿主）。“保留在 core / 迁移为插件 / 暂缓迁移 / 淘汰”的正式四分类由 4.2 负责，本表的“归属初判”一栏只作为 4.2 的输入。

## 1. 结论摘要

| 层面 | 现状 | 依据 |
|---|---|---|
| 业务入口 | 全部保留并扩充 | 前端路由 71 → 77；`web/src/pages` 页面组件 47 → 52（新增 Discover、NodeAgents、PluginMarketplace、Crystallized、PluginReleases），无删除 |
| 代码迁移 | 完成 | `internal/` 已清空；735 个非测试 Go 文件中 656 个可按重命名追溯到 `core/*` |
| 业务域独立 | 约 1/3 | release 的 27 个 `biz` 包中 9 个迁入 `core/domains`；事件闭环主体（aiops、alert、loop、approval、knowledge 等）仍在 `core/manager`。同口径文件数 550 → 594，未变小 |
| 扩展点接入 | 3/6 有真实贡献 | `tool`、`adapter`、`datasource` 已有注册；`collector` 通道就绪但无贡献；`provider`、`notifier` 各有宿主内工厂注册表，未接内核 |
| 治理与企业闭环 | 早期 | 本计划 6/76、`runtime-management-gap` 0/41、`pig-runtime-adapter` 6/33、Paperclip 17/27、团队记忆 3/14、日报 12/15 |

## 2. 旧 release 业务架构（还原）

旧 release 没有单独的业务域规划文档。下图由以下证据还原：

- release 侧边栏分组（`web/src/components/Sidebar.tsx`）：Agent、知识库、设备、监控告警、日常、审批、管理；
- `README_ZH.md` 的“响应闭环”与 7 个 Worker 角色；
- `cmd/opskeeper/main.go` 中 `/api/v1` 的路由分组；
- 本计划 4.1 列出的八个板块：工作台、观测、事件、修复、审批、知识、协同、企业集成。

```text
┌──────────────────────────────────────────────────────────────────────────────────────────────────┐
│ OpsKeeper release-v2026.09.20-patch.2 · 业务架构（业务域规划）                                   │
├──────────────────────────────────────────────────────────────────────────────────────────────────┤
│ 【用户与渠道】 SRE/值班 · 管理员 · 审批人                                                        │
│               Web 控制台 · IM 机器人(飞书/钉钉) · AgentTeams Dashboard · 告警 Webhook · MCP/API  │
├──────────────────────────────────────────────────────────────────────────────────────────────────┤
│ 【核心价值流：事件响应闭环】                                                                     │
│  告警接入 → 降噪分派 → 只读调查 → 根因定位 → 修复提案 → HITL 双签                                │
│     → 窄权修复 → 独立验证 → 审计/复盘 → 知识回流                                                 │
├──────────────────────────────────────────────────────────────────────────────────────────────────┤
│ 【业务域】                                                                                       │
│ ┌─① 工作台────────────────────┐ ┌─② 智能体协同────────────────┐ ┌─③ 资产与拓扑────────────────┐  │
│ │ 首页 / 仪表盘               │ │ 助理 + 7 角色 Worker        │ │ 设备纳管 主机/存储/DB/网络  │  │
│ │ 会话 Chat · @诊断入口       │ │ 调查·评审·修复·验证·复盘…   │ │ Edge 注册 · 批量升级        │  │
│ │ 日常：任务 · 产物 · 报告    │ │ 工作流 Flow 编排            │ │ 拓扑(节点/关系/类型)        │  │
│ │ 托管页面 · 演示场景         │ │ 技能 Skill · MCP · 市场     │ │ WebSSH 会话                 │  │
│ └─────────────────────────────┘ └─────────────────────────────┘ └─────────────────────────────┘  │
│ ┌─④ 可观测────────────────────┐ ┌─⑤ 告警与事件────────────────┐ ┌─⑥ 修复──────────────────────┐  │
│ │ 指标查询 · 监控面板         │ │ 告警规则 · 语义降噪 · SLI   │ │ 修复提案 Proposal           │  │
│ │ 日志(Loki) · 链路(Tempo)    │ │ Incident 事件/指标/Runbook  │ │ 修复预览 repair-preview     │  │
│ │ Prometheus 写入/代理        │ │ 闭环 Loop 编排              │ │ Repairer 窄权执行           │  │
│ │ Grafana 自动配置            │ │ Chat 诊断 · RCA 报告        │ │ Verifier 独立验证           │  │
│ └─────────────────────────────┘ └─────────────────────────────┘ └─────────────────────────────┘  │
│ ┌─⑦ 审批──────────────────────┐ ┌─⑧ 知识──────────────────────┐ ┌─⑨ 企业集成──────────────────┐  │
│ │ 审批收件箱(propose-confirm) │ │ 知识库 · 检索(BM25+向量)    │ │ IM 桥接 · 集成配置          │  │
│ │ HITL 高危操作双签           │ │ 代码仓库 GitArtifact        │ │ 凭证库 · 系统设置           │  │
│ │ 审批结果回写闭环            │ │ 复盘沉淀 · 知识回流         │ │ AI 网关 Higress             │  │
│ │                             │ │                             │ │ 中间件 pg/redis/k8s/mq/git  │  │
│ └─────────────────────────────┘ └─────────────────────────────┘ └─────────────────────────────┘  │
├──────────────────────────────────────────────────────────────────────────────────────────────────┤
│ 【平台治理】 IAM 用户/组织/Casbin · 审计日志 · 数据分级 DataGuard · 系统健康/升级/集群/版本      │
│              Runtime 视图 · ops-keeper 数据迁移                                                  │
├──────────────────────────────────────────────────────────────────────────────────────────────────┤
│ 【执行面 Edge Agent】(经 Tunnel 反连控制面)                                                      │
│   采集插件 主机/DB/进程/日志/链路/自定义 · 变更监听 · 命令策略 cmdpolicy                         │
│   本地技能 bash / host-files / restart-service · WebShell                                        │
├──────────────────────────────────────────────────────────────────────────────────────────────────┤
│ 【质量保障】 评测 Harness(黄金用例/裁判/故障注入/排行榜) · Demo 场景 · 测试夹具                  │
├──────────────────────────────────────────────────────────────────────────────────────────────────┤
│ 【基础设施】 PostgreSQL · Redis · Qdrant · Prometheus · Loki · Tempo · Grafana                   │
│              LLM(Higress / OpenAI 兼容) · AgentTeams Controller                                  │
└──────────────────────────────────────────────────────────────────────────────────────────────────┘
```

板块对照：

| 4.1 原文板块 | 图中业务域 | 说明 |
|---|---|---|
| 工作台 | ① 工作台 | — |
| 协同 | ② 智能体协同 | 本稿把“协同”解释为**智能体协同 + 团队协同**（见 §7 待评审问题 1） |
| 观测 | ④ 可观测 | — |
| 事件 | ⑤ 告警与事件 | — |
| 修复 | ⑥ 修复 | — |
| 审批 | ⑦ 审批 | — |
| 知识 | ⑧ 知识 | — |
| 企业集成 | ⑨ 企业集成 | — |
| *（4.1 原文未列出）* | ③ 资产与拓扑 | release 侧边栏“设备”组，独立存在；按 8.4“无未解释的旧业务板块缺口”补入 |
| *（4.1 原文未列出）* | 平台治理、Edge 执行面、质量保障 | 横切能力，按设计宿主保留或独立模块，补入以便核对 |

## 3. 六扩展点现状（main@86dff884）

定义见 `core/floor/kernel/kind.go`。

| 扩展点 | 宿主侧 | 当前贡献 | 状态 | 证据 |
|---|---|---|---|---|
| `tool` | Manager + Edge | 按 manifest `spec.tools` 注册；`plugins/pig-ops` 5 个包共 94 个工具（readonly 20 / observability 12 / middleware 56 / repair 5 / autonomy 1） | **已接入** | `core/manager/middleware/pluginhost/host.go`、`core/edge/pluginhost/host.go` |
| `adapter` | Manager | 8 个一方源作为预装插件经内核注册：pg、redis、k8s、mq、kafka、rabbitmq、git、host | **已接入（仅一方）** | `core/manager/middleware/toolset/sources.go`、`sources_plugin.go` |
| `datasource` | Manager | 3 个一方后端作为预装插件经内核注册：prom、loki、tempo | **已接入（仅一方）** | `core/manager/biz/aiops/datasource/datasource.go` |
| `collector` | Edge | `CollectorContribution` 与 supervisor sink 已打通，但生产代码无 `RegisterActivator` 调用；内置采集器仍编译内运行 | **通道就绪，0 贡献** | `core/edge/pluginhost/host.go`、`core/edge/plugins/*` |
| `provider` | Manager | `core/pig/pigmodel` 自有 `providerFactories`（openai、anthropic、zhipu、gemini、deepseek、kimi、custom），未接内核 | **未接内核** | `core/pig/pigmodel/providers.go`、`core/domain/provider.go` |
| `notifier` | Manager | `core/base/pkg/notify` 自有 `ChannelFactory` 注册表（webhook、slack、feishu、dingtalk、wecom、telegram），未接内核 | **未接内核** | `core/base/pkg/notify/registry.go` |

## 4. 业务板块 → 扩展点映射表

图例：
- **扩展点**一栏：`—` 表示不挂扩展点。
- **现状**一栏：“已接入”指经内核注册；“宿主保留”指按 `docs/opskeeper2-architecture.md` §九 判定不插件化。
- **归属初判**一栏：仅作 4.2 的输入。

### ① 工作台

| 能力 | release 载体 | main 载体 | 扩展点 | 现状 | 归属初判 | 后续任务 |
|---|---|---|---|---|---|---|
| 首页 / 仪表盘 / 日报 | Dashboard 页 | Home、Dashboard 页；`ops-daily-brief` | — | 宿主保留 | 保留 core | `ops-daily-brief`、5.1 |
| 会话 Chat、@诊断入口 | `biz/aiops`、`biz/chatdiagnose` | `core/manager/biz/aiops/chatruntime`、`core/manager/biz/chatdiagnose` | —（消费 `tool`） | 宿主保留 | 保留 core | — |
| 任务 / 产物 / 报告 / 托管页面 | `biz/report`、`/v1/pages` | `core/manager/biz/report`、`cmd/opskeeper/hosted_page_routes.go` | — | 宿主保留 | 保留 core | — |
| 演示场景 | `biz/demo` | `core/manager/biz/demo` | — | 原样搬迁 | 待 4.2 评估（淘汰候选） | 4.2 |

### ② 协同（智能体协同 + 团队协同）

| 能力 | release 载体 | main 载体 | 扩展点 | 现状 | 归属初判 | 后续任务 |
|---|---|---|---|---|---|---|
| Agent 内核 | `biz/aiops/graph`（eino） | `core/pig/*`、`core/manager/biz/aiops/agentkernel` | — | 宿主保留（PiG 仅 `core/pig` 引入） | 保留 core | — |
| 运维工具集 | `biz/aiops/tools` | `aiops/tools` 子包 + `toolregistry`；`plugins/pig-ops` 5 包 | `tool` | 已接入 | 迁移为插件（进行中） | 4.8、4.9 |
| 7 角色 Worker / persona | `agents/*.md`、`plugins/opskeeper-teamharness` | TeamHarness 插件；PiG 包 `agents`/`skills` 资源 | —（包资源，非内核 kind） | 已包化 | 迁移为插件 | `pig-runtime-adapter` 3.x |
| 工作流 Flow | `biz/flow` | `core/domains/biz/flow` | —（消费 `tool`） | 宿主保留，已独立成域 | 保留 core | — |
| 技能 Skill | `internal/skill` | `core/floor/skill`；Discover 页 | 可执行部分走 `tool` | 部分 | 待 4.2 | 4.2 |
| 外部 MCP 服务 | `biz/mcp` | `core/manager/biz/mcp` | `tool`（经 basetool 暴露） | 宿主内注册，未经内核 | 待 4.2 | 4.2 |
| 插件 / 技能市场 | `biz/marketplace` | `marketplace`、`pluginimport`、`core/domains/service/plugin`、`sdk`；插件市场与插件发布页 | —（治理面） | 宿主保留 | 保留 core | 4.10、4.11 |
| AgentTeams Worker 运行时 | `internal/agentteams` | `core/manager/agentteams` | —（runtime adapter，非 kind） | 未纳入统一运行时 | 保留 core | `runtime-management-gap` 1.3、2.5 |
| 团队记忆 / 事件群聊 / 值班 | release 无 | 未实现 | — | 未开始 | 保留 core | 5.10–5.13 |

### ③ 资产与拓扑（补充板块）

| 能力 | release 载体 | main 载体 | 扩展点 | 现状 | 归属初判 | 后续任务 |
|---|---|---|---|---|---|---|
| 设备纳管 | `biz/device` | `core/manager/biz/device` | `adapter` 候选（云资源 / CMDB 同步） | 宿主内，未插件化 | 保留 core + 候选插件 | 4.4 |
| Edge 注册 / 升级 | `biz/edge`、`server/edgeauth` | `core/manager/biz/edge`、`core/domains/server/edgeauth`、`core/domains/biz/nodefleet` | — | 宿主保留 | 保留 core | `runtime-management-gap` 1.3 |
| 拓扑 | `biz/topology` | `core/manager/biz/topology` | `tool`（§九：可插件化）；CMDB 走 `adapter` | 未接入 | 迁移为插件（候选） | 4.4 |
| WebSSH | `biz/webshell`、`edgeagent/webshell` | `core/manager/biz/webshell`、`core/edge/webshell` | — | 宿主保留（高权限通道） | 保留 core | — |

### ④ 观测

| 能力 | release 载体 | main 载体 | 扩展点 | 现状 | 归属初判 | 后续任务 |
|---|---|---|---|---|---|---|
| 指标查询 / 监控面板 | `biz/metric`、`biz/monitor` | `core/domains/biz/{metric,monitor}` | `datasource`：prom | 已接入（一方） | 迁移为插件（已完成一方） | 4.3 |
| 日志 / 链路 | `server/{logs,traces}`、`pkg/{logquery,tracequery}` | `core/domains/server/{logs,traces}` | `datasource`：loki、tempo | 已接入（一方） | 同上 | 4.3（VictoriaMetrics / Elasticsearch / OpenSearch） |
| Prometheus 写入 / 代理 | `biz/promwrite`、`server/prometheus` | `core/domains/biz/promwrite`、`core/domains/server/prometheus` | — | 宿主保留，已独立成域 | 保留 core | — |
| Grafana 自动配置 | `biz/grafana` | `core/domains/biz/grafana` | `adapter` 候选 | 未接入 | 待 4.2 | 4.2 |
| 边缘采集 | `edgeagent/plugins/*` | `core/edge/plugins/{hostmetrics,databasemetrics,procmetrics,logs,traces,custommetrics}` | `collector` | 通道就绪，0 贡献 | 迁移为插件（候选） | 4.7 |
| 可观测查询工具 | `aiops/tools` 中的 promql / logql / traceql | `opskeeper-sre-observability`（12 工具） | `tool` | 已接入 | 迁移为插件（已完成） | 4.8 |

### ⑤ 事件

| 能力 | release 载体 | main 载体 | 扩展点 | 现状 | 归属初判 | 后续任务 |
|---|---|---|---|---|---|---|
| 告警规则 / 草稿 / 降噪 | `biz/alert`、`aiops/{alertconfig,alertdraft}` | `core/manager/biz/alert`、`core/manager/biz/aiops/{alertconfig,alertdraft}` | `tool`（§九：可插件化为 extension tool + command） | 未接入 | 迁移为插件（候选） | 需在权威 change 补任务 |
| 告警外发通知 | `pkg/notify`（硬编码分支） | `core/base/pkg/notify`（`ChannelFactory` 注册表） | `notifier` | 宿主注册表已有，未接内核 | 迁移为插件 | 4.5 |
| Incident 事件 / 指标 / Runbook | `control/incident`、`server/incident` | `core/domains/control/incident`、`core/domains/server/incident`、`shared/incident-command` | — | 宿主保留，已独立成域；新增 Incident Command UI | 保留 core | 5.3（Paperclip 只读同步） |
| 闭环 Loop 编排 | `biz/loop` | `core/manager/biz/loop` | —（消费 `tool` / `adapter`） | 宿主保留 | 保留 core | `runtime-management-gap` 2.4 |
| 诊断 / RCA | `aiops/{investigator,critic}`、`biz/chatdiagnose` | 同名包；`opskeeper-sre-readonly`（20 工具） | `tool` | 已接入（只读工具） | 迁移为插件（已完成） | `pig-runtime-adapter` 3.11 |

### ⑥ 修复

| 能力 | release 载体 | main 载体 | 扩展点 | 现状 | 归属初判 | 后续任务 |
|---|---|---|---|---|---|---|
| 修复提案 | `aiops/proposal` | `core/manager/biz/aiops/proposal` | — | 宿主保留（§九：提案 / 审批工作流不可插件化） | 保留 core | — |
| 修复预览 | `control/repairpreview` | `core/domains/control/repairpreview` | — | 宿主保留；未接共享任务生命周期 | 保留 core | `runtime-management-gap` 2.4、4.9 |
| 修复执行 | `edgeagent/{bash,restart_service}`、`aiops/tools` | `opskeeper-sre-repair`（L2，5 工具）+ `core/edge/toolbroker` | `tool` | 已接入 | 迁移为插件（已完成） | 4.9 |
| 有限自治 | release 无 | `opskeeper-sre-autonomy`（L3，1 工具）+ `core/edge/autonomy` | `tool` | 已接入，默认受限 | 迁移为插件（已完成） | 非目标：本计划不启用 L3 |
| 独立验证 | Verifier 角色 | Verifier persona + 只读工具 | `tool` | 部分 | 迁移为插件 | 4.9 |

### ⑦ 审批

| 能力 | release 载体 | main 载体 | 扩展点 | 现状 | 归属初判 | 后续任务 |
|---|---|---|---|---|---|---|
| 审批收件箱 | `biz/approval`（1 文件） | `core/manager/biz/approval`、`core/manager/server/approval`（重写）；审批治理 UI | — | 宿主保留（§九） | 保留 core | 5.14 |
| HITL 高危双签 | `biz/hitl` | `core/manager/biz/hitl`、`cmd/opskeeper/dualsigngate.go` | — | 宿主保留 | 保留 core | 5.14 |
| 节点侧裁决 | `edgeagent/cmdpolicy` | `core/edge/{policygate,gatesocket}`、`core/pig/extensions/opskeeper-gate` | —（gate 信使是策略扩展，非 kind） | 宿主保留 | 保留 core | — |
| 外部审批桥（Paperclip） | release 无 | 未实现（`approvalBridgeEnabled=false`） | `adapter` + `tool` 候选 | 未开始 | 迁移为插件（候选） | 5.5–5.9 |

### ⑧ 知识

| 能力 | release 载体 | main 载体 | 扩展点 | 现状 | 归属初判 | 后续任务 |
|---|---|---|---|---|---|---|
| 知识库 / 检索 | `biz/knowledge`、`pkg/{qdrantx,embedding}` | `core/manager/biz/knowledge`、`core/base/pkg/{vecstore,qdrantx,embedding}` | —（§九：RAG 不在 PiG 范畴，可包成 MCP 暴露） | 宿主保留；入口并入“日常” | 保留 core | — |
| 代码仓库 GitArtifact | `internal/knowledge/gitartifact` | `core/manager/knowledge/gitartifact` | `adapter`：git（git 源已注册） | 部分 | 待 4.2 | 4.4 |
| 知识检索工具 | `query_knowledge` basetool | `aiops/tools` 内编译 | `tool` | 编译内，未包化 | 待 4.2 | 4.8 |
| 复盘沉淀 / 结晶 | `biz/report` | `core/manager/biz/aiops/crystallize`、Crystallized 页 | — | 宿主保留（main 新增） | 保留 core | — |
| 嵌入模型 | `pkg/embedding` | `core/base/pkg/embedding` | 待定（`provider` 当前仅定义为对话补全） | 未接入 | 待定 | 见 §7 问题 3 |

### ⑨ 企业集成

| 能力 | release 载体 | main 载体 | 扩展点 | 现状 | 归属初判 | 后续任务 |
|---|---|---|---|---|---|---|
| LLM 模型供应商 | `pkg/llm` | `core/pig/pigmodel`（7 个 provider id） | `provider` | 宿主注册表已有，未接内核 | 迁移为插件 | 4.6 |
| AI 网关 / 节点模型端点 | `internal/higress` | `core/manager/higress`、`core/domains/server/llmgw` | `provider` 候选 | 宿主保留 | 保留 core | 4.6 |
| 中间件适配 | `internal/middleware/adapter`（git、k8s、mq、postgres、redis） | `core/manager/middleware/adapter/{postgres,redis,k8s,mq,git,host}`；`opskeeper-sre-middleware`（56 工具） | `adapter` + `tool` | 已接入（一方 8 源） | 迁移为插件（已完成一方） | 4.4 |
| IM 机器人（入站） | `biz/imbridge`（feishu、slack、telegram） | `core/manager/biz/imbridge` | —（入站渠道，不属六扩展点） | 宿主保留 | 保留 core | 出站部分见 4.5 |
| 集成配置 | `server/integration` | `core/domains/server/integration` | —（`datasource` / `provider` 的配置面） | 宿主保留，已独立成域 | 保留 core | — |
| 凭证库 / 系统设置 | `biz/{secret,setting}` | `core/domains/biz/{secret,setting}` | — | 宿主保留，已独立成域 | 保留 core | 4.6（凭据作用域） |
| ITSM / CMDB / 云资源 / Paperclip | release 无 | 未实现 | `adapter` | 未开始 | 迁移为插件（候选） | 4.4、5.2–5.9 |

### 补充：平台治理、Edge 执行面、质量保障

| 能力 | main 载体 | 扩展点 | 现状 | 后续任务 |
|---|---|---|---|---|
| IAM 用户 / 组织 / 权限 | `core/manager/iam` | — | 宿主保留（§九） | — |
| 审计 | `core/domains/biz/audit`、`core/base/pkg/audit`、`core/edge/auditlog` | — | 宿主保留；release 的 1 个文件重写为域内 + 节点本地账本 | 3.9 |
| 数据分级 DataGuard | `core/manager/dataguard` | — | 宿主保留 | — |
| 系统健康 / 升级 / 版本 / 集群 | `core/domains/server/{systemupgrade,version,cluster}` | — | 宿主保留，已独立成域 | 6.1 |
| Runtime 视图 | `web/src/pages/admin/Runtime.tsx`、`core/manager/model/runtimemgmt` | — | 页面空值已修（#97）；任务生命周期模型已落（#98）；inventory / health 未做 | 1.2、1.3、2.x |
| 数据迁移 | `core/manager/{migrate,migrator}` | — | 原样搬迁 | — |
| Edge 执行面 | `core/edge/*`（60 → 80 文件） | `tool`、`collector` | PiG 监督、policygate、toolbroker、gatesocket、离线自治已落地 | 4.7 |
| 评测 Harness / 故障注入 | `core/harness`、`core/faults` | — | 独立模块，不插件化 | — |

## 5. 实现进度叠加图

```text
┌──────────────────────────────────────────────────────────────────────────────────────────────────┐
│ main@86dff884 对上图的实现进度（✓ 已落地  ~ 部分  ✗ 未落地  — 按设计宿主保留）                   │
├──────────────────────────────────────────────────────────────────────────────────────────────────┤
│ 【渠道】 ✓ 71 条旧路由全保留 → 77 条；导航改为 对话/Agent/发现/运维/日常/审批/管理               │
│ 【价值流】 ✓ 闭环各环节代码均已迁移；~ 统一任务生命周期仅有模型层(#98)                           │
├──────────────────────────────────────────────────────────────────────────────────────────────────┤
│ ┌─① 工作台 [高]───────────────┐ ┌─② 智能体协同 [中]───────────┐ ┌─③ 资产与拓扑 [中]───────────┐  │
│ │ ✓ 入口 +/home +日报         │ │ ✓ +发现 +节点Agent +插件市场│ │ ✓ 入口并入「运维」          │  │
│ │ ~ report/demo 留 manager    │ │ ✓ 内核换 PiG(core/pig 单点) │ │ ~ device/topo 留 manager    │  │
│ │ — Web 控制台宿主保留        │ │ ✓ tool 扩展点 5 包/94 工具  │ │ ~ 拓扑/CMDB 未插件化        │  │
│ │ 缺：ops-daily-brief 12/15   │ │ 缺：Runtime 0/41 PiG 6/33   │ │ 缺：K8s/云/CMDB adapter     │  │
│ └─────────────────────────────┘ └─────────────────────────────┘ └─────────────────────────────┘  │
│ ┌─④ 可观测 [高]───────────────┐ ┌─⑤ 告警与事件 [中]───────────┐ ┌─⑥ 修复 [中]─────────────────┐  │
│ │ ✓ 整域迁入 core/domains     │ │ ✓ +Incident Command UI      │ │ ✓ repairpreview→domains     │  │
│ │ ✓ datasource Prom/Loki/Tempo│ │ ~ incident→domains          │ │ ✓ repair L2 5 工具+自治 L3  │  │
│ │ ~ collector 通道就绪 0 贡献 │ │ ~ alert/loop 留 manager     │ │ ~ 未接共享任务生命周期      │  │
│ │ 缺：VM/ES 等新数据源        │ │ 缺：Paperclip 同步/群聊     │ │ 缺：预览/验证/回滚强化      │  │
│ └─────────────────────────────┘ └─────────────────────────────┘ └─────────────────────────────┘  │
│ ┌─⑦ 审批 [中高]───────────────┐ ┌─⑧ 知识 [中]─────────────────┐ ┌─⑨ 企业集成 [中低]───────────┐  │
│ │ ✓ 升为一级菜单+治理 UI      │ │ ~ 入口降级并入「日常」      │ │ ✓ secret/setting→domains    │  │
│ │ ✓ 重写 + 节点 policygate    │ │ ~ manager 内原样搬迁        │ │ ✓ adapter 8 源 + 56 工具    │  │
│ │ — 裁决权宿主保留            │ │ ✓ +结晶 Crystallized        │ │ ~ provider/notifier 未接内核│  │
│ │ 缺：Paperclip 审批桥/hash   │ │ 缺：团队记忆 3/14           │ │ 缺：Paperclip 17/27 ITSM    │  │
│ └─────────────────────────────┘ └─────────────────────────────┘ └─────────────────────────────┘  │
├──────────────────────────────────────────────────────────────────────────────────────────────────┤
│ 【平台治理 中高】 ✓ IAM 原样 · ✓ 审计重写(域内+节点本地账本) · ✓ DataGuard · ✓ 升级/版本→domains │
│                   ~ Runtime 页空值已修(#97)，统一 runtime inventory/health 未做                  │
│ 【执行面 Edge 高】 ✓ 60→80 文件：PiG 监督·policygate·toolbroker·gatesocket·离线自治              │
│ 【质量保障 高】   ✓ harness 扩展 axes/projection · ✓ 故障注入拆为独立模块 core/faults            │
├──────────────────────────────────────────────────────────────────────────────────────────────────┤
│ 【规划外新增】 节点 AI Agent(NodeFleet/llmgw) · 多集群联邦 · 插件市场/发布/导入 + sdk            │
│                运行时管理模型(runtimemgmt) · PiG 进程驱动(pigruntime) · L3 有限自治              │
└──────────────────────────────────────────────────────────────────────────────────────────────────┘
```

板块评级（高 / 中 / 低）是根据入口、代码落位、扩展点接入和权威 change 勾选情况做的判断，不是测量值。

## 6. 扩展点缺口与后续任务归属

| 缺口 | 影响板块 | 归属 |
|---|---|---|
| `provider` 未接内核，模型供应商只能编译内增加 | 企业集成、协同 | 4.6 |
| `notifier` 未接内核，通知渠道只能编译内增加 | 事件、企业集成 | 4.5 |
| `collector` 无贡献，内置采集器仍编译内 | 观测、Edge | 4.7 |
| `adapter` / `datasource` 只有一方贡献，没有第三方包 | 观测、企业集成 | 4.3、4.4 |
| 告警规则 / 草稿、拓扑、知识检索仍以编译内工具存在 | 事件、资产与拓扑、知识 | 4.8；告警部分需在权威 change 补任务 |
| 修复预览、只读诊断未接共享任务生命周期 | 修复、事件 | `runtime-management-gap` 2.4 |
| 外部审批桥、ITSM / CMDB 均未开始 | 审批、企业集成 | 4.4、5.5–5.9 |
| 团队记忆与事件协同未开始 | 协同 | 5.10–5.13 |

另有两项与映射无关、但影响本计划读数的问题：

- `runtime-management-gap` 的 2.1（任务生命周期八类模型）已在 #98 落入 `core/manager/model/runtimemgmt`，但该 change 的 `tasks.md` 仍为 0/41 未勾选。
- 旧 release tag 内 `VERSION` 为 `v2026.09.14-rc4`，与 tag 名不一致，属于 1.5 已列出的版本元数据漂移。

## 7. 待评审问题

1. 4.1 原文的“协同”指智能体协同、团队协同，还是两者都包含？本稿按两者都包含处理。
2. “资产与拓扑”在 4.1 原文中缺席，是否作为第九个板块纳入？本稿已补入。
3. 嵌入模型是否归入 `provider`？当前 `KindProvider` 只定义为对话补全。
4. 外部 MCP 服务、技能（Skill）是否需要经内核注册，还是继续由宿主内注册表管理？
5. 告警规则 / 草稿插件化尚无权威 change 承接，需要新开 change，还是并入既有 change？

## 附：统计口径

- 路由数：`App.tsx` 中 `path=` 出现次数；页面数：`web/src/pages` 下非 `*.test.tsx` 的 `.tsx` 文件。
- 文件数：非测试 `.go` 文件；“同口径”指 release 的 `internal/{manager,iam,agentteams,dataguard,higress,knowledge,middleware,migrate,migrator,observability}` 对应 main 的 `core/manager`。
- 重命名追溯：`git diff -M50% release-v2026.09.20-patch.2 main`。
- 扩展点注册：`grep -rn 'kernel\.Kind' core cmd --include=*.go`（排除测试和 `core/floor/kernel` 自身）。
- 工具数：各 `plugins/pig-ops/*/pig-ops.yaml` 的 `spec.tools` 条目数。
- 勾选数：各 change `tasks.md` 中 `- [x]` 与 `- [ ]` 的计数。
