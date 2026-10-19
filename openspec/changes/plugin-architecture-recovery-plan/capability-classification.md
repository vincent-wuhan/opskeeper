# 4.2 编译内能力四分类盘点

> 状态：**已确认（2026-10-19）**。对应 `tasks.md` 4.2。
>
> 输入：`business-domain-mapping.md` §4「归属初判」+ §7 评审结论（2026-10-19），交叉核对 `core/manager`、`core/domains`、`core/base/pkg`、`core/edge`、`plugins/` 实际包清单（`main@1203be78`）。
>
> 四类定义：
> - **保留在 core**：按 `docs/opskeeper2-architecture.md` §九 属宿主职责，或平台自身基础设施，不插件化。
> - **迁移为插件**：应挂六扩展点之一；标注「已完成（一方）」「进行中」「候选」。
> - **暂缓迁移**：可插件化但本期不做（含 §7 结论明确暂缓项）。
> - **淘汰**：建议移除；正式移除前需线上确认无依赖。

## 1. 保留在 core

| 能力 | 载体 | 依据 |
|---|---|---|
| Web 控制台、宿主路由、托管页面 | `web/*`、`cmd/opskeeper/hosted_page_routes.go` | §九 宿主职责 |
| Agent 内核（PiG 引入、agentkernel） | `core/pig/*`、`aiops/agentkernel` | §九；PiG 适配归 `pig-runtime-adapter` |
| 会话 Chat / @诊断入口 | `aiops/chatruntime`、`biz/chatdiagnose` | 闭环主体，消费 `tool` |
| 事件闭环主体（incident、loop、proposal、approval、hitl、critic、investigator、crystallize） | `core/domains/control/incident`、`biz/{loop,approval,hitl}`、`aiops/{proposal,critic,investigator,crystallize}` | §九：提案/审批工作流不可插件化；闭环编排保留 |
| 修复预览 | `core/domains/control/repairpreview` | 保留 core；任务生命周期接入归 `runtime-management-gap` 2.4 |
| 设备纳管、Edge 注册/升级、WebSSH | `biz/{device,edge,webshell}`、`core/edge/webshell` | 设备域保留 core；云/CMDB 同步走 adapter 插件（4.4）；WebSSH 高权限通道 |
| 知识库 / 检索 / 向量存储 | `biz/knowledge`、`core/base/pkg/{vecstore,qdrantx}` | §九：RAG 不在 PiG 范畴 |
| 嵌入模型 | `core/base/pkg/embedding` | §7 结论 3：本期维持编译内 |
| IM 机器人（入站） | `biz/imbridge` | 入站渠道，不属六扩展点；出站见 4.5 |
| 插件 / 技能市场治理面 | `biz/{marketplace,pluginimport}`、`core/domains/service/plugin`、`sdk` | 治理面归宿主 |
| IAM / 审计 / DataGuard | `core/manager/iam`、`core/domains/biz/audit`、`core/manager/dataguard` | §九 平台治理 |
| 系统健康 / 升级 / 版本 / 集群 / 集成配置 / 凭证 / 设置 | `core/domains/server/{systemupgrade,version,cluster,integration,secret,setting}`、`core/domains/biz/{secret,setting}` | 已独立成域，宿主保留 |
| 观测域（指标/日志/链路查询、监控面板、Prometheus 写入） | `core/domains/biz/{metric,monitor,promwrite}`、`core/domains/server/{logs,traces,prometheus}` | 已独立成域；查询后端经 `datasource` 插件（4.3） |
| 中间件适配宿主侧 | `core/manager/middleware`（toolset、pluginhost） | 插件宿主本身 |
| AI 网关 / 节点模型端点 | `core/manager/higress`、`core/domains/server/llmgw` | 网关面；候选 provider 归 4.6 评估 |
| manager 自身可观测 | `core/manager/observability` | 平台基础设施 |
| 平台调度 | `core/domains/biz/scheduler` | 平台基础设施 |
| 数据迁移 | `core/manager/{migrate,migrator}` | 平台基础设施 |
| 基础库（dbx、auth、tracing、logquery/tracequery/promquery 查询构造等） | `core/base/pkg/*`（除 notify、embedding、llm、mcpclient，见下） | 基础设施，被插件经 SDK 消费而非自身成为插件 |
| Runtime 管理 | `biz/pigruntime`、`core/manager/model/runtimemgmt`、`biz/agentteams` | 归 `runtime-management-gap`、`pig-runtime-adapter`，非插件化范畴 |
| 评测 Harness / 故障注入 | `core/harness`、`core/faults` | 独立模块，不插件化 |
| 规划外新增（节点 AI Agent、多集群联邦、L3 有限自治治理面） | `core/domains/biz/{nodeagent,nodefleet,federation}`、`core/edge/autonomy` | 保留 core；L3 本计划不启用 |

## 2. 迁移为插件

| 能力 | 载体 | 扩展点 | 进度 | 后续任务 |
|---|---|---|---|---|
| 运维工具集（SRE 工具） | `plugins/pig-ops` 5 包 / 94 工具 | `tool` | 进行中 | 4.8、4.9 |
| 诊断 / RCA 只读工具 | `opskeeper-sre-readonly`（20 工具） | `tool` | 已完成 | `pig-runtime-adapter` 3.11 E2E |
| 可观测查询工具 | `opskeeper-sre-observability`（12 工具） | `tool` | 已完成 | 4.8 |
| 中间件工具 | `opskeeper-sre-middleware`（56 工具） | `tool` + `adapter`（一方 8 源已注册） | 已完成（一方） | 4.4（第三方源） |
| 修复执行 / 独立验证 | `opskeeper-sre-repair`（L2，5 工具）+ Verifier persona | `tool` | 已完成 | 4.9（预览/验证/回滚强化） |
| L3 有限自治 | `opskeeper-sre-autonomy`（1 工具） | `tool` | 已完成（默认受限） | 非目标：本计划不启用 |
| 数据源后端（prom/loki/tempo） | `aiops/datasource`（一方 3 后端经内核注册） | `datasource` | 已完成（一方） | 4.3（VictoriaMetrics / ES / OpenSearch） |
| LLM 模型供应商 | `core/pig/pigmodel`（7 provider id，宿主工厂注册表） | `provider` | 未接内核 | 4.6 |
| 出站通知渠道 | `core/base/pkg/notify`（6 渠道，宿主注册表） | `notifier` | 未接内核 | 4.5 |
| 边缘采集器 | `core/edge/plugins/{hostmetrics,databasemetrics,procmetrics,logs,traces,custommetrics}` | `collector` | 通道就绪，0 贡献 | 4.7 |
| 7 角色 Worker / persona | `plugins/opskeeper-teamharness`、PiG 包资源 | `tool`（包资源） | 已包化 | `pig-runtime-adapter` 3.x |
| 拓扑 / CMDB | `biz/topology`（编译内） | `tool` + `adapter` | 候选 | 4.4 |
| Grafana 自动配置 | `core/domains/biz/grafana` | `adapter` | 候选 | 4.4 |
| GitArtifact（知识侧） | `core/manager/knowledge/gitartifact` | `adapter`（git 源已注册） | 部分 | 4.4 |
| 知识检索工具 `query_knowledge` | `aiops/tools` 编译内 | `tool` | 候选 | 4.8 |
| 告警规则 / 草稿 / 降噪 | `biz/alert`、`aiops/{alertconfig,alertdraft}` | `tool`（extension tool + command） | 候选 | 4.12（§7 结论 5） |
| ITSM / CMDB / 云资源 / Paperclip 审批桥 | 未实现 | `adapter` + `tool` | 未开始 | 4.4、5.5–5.9 |

## 3. 暂缓迁移

| 能力 | 载体 | 扩展点 | 依据 | 后续 |
|---|---|---|---|---|
| 技能 Skill | `core/floor/skill`、`biz/skill` | 可执行部分 `tool` | §7 结论 4：本期宿主内管理 | 4.2 后保持待定；内核化需求出现时再评 |
| 外部 MCP 服务 | `biz/mcp`、`core/base/pkg/mcpclient` | `tool`（basetool 暴露） | §7 结论 4；容器 `.mcp.json` 明确 `mcp_unsupported` | 同上 |
| 报告 / 演示场景中非演示部分（report 引擎） | `biz/report` | — | 宿主保留，见 §1 | — |

## 4. 淘汰

| 能力 | 载体 | 依据 | 动作 |
|---|---|---|---|
| 演示场景 demo | `biz/demo` | 旧 release 演示用途，非生产业务；4.1 初稿即标淘汰候选 | **待线上确认无依赖后移除**；移除前不阻塞其他任务 |

## 5. 遗漏核对

包清单交叉核对结果（相对 4.1 映射表）：新识别并归入 §1 的有 `core/domains/biz/scheduler`（平台调度）、`core/manager/observability`（自身可观测）、`biz/pigruntime`（#100 新增，归 runtime 面）。`core/domains/biz/{nodeagent,nodefleet,federation}` 属 §5 已标注的「规划外新增」。无未解释的编译内能力缺口。

## 6. 统计口径

- 包清单：`ls core/manager/biz`、`ls core/domains/{biz,control,server}`、`ls core/base/pkg`、`ls core/edge/plugins`、`ls plugins/`（`main@1203be78`）。
- 归类依据以 4.1 映射表「归属初判」为基线，§7 结论覆盖待定项。
