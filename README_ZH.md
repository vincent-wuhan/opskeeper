# OpsKeeper

OpsKeeper 是一个面向可审计多 Agent 事件响应的运维平台，把告警接入、证据采集、根因分析、人工审批、精确授权恢复、独立验证和复盘沉淀连接成一个闭环。

## 核心能力

- **多 Agent 协同**：Manager 式路由与 alerter、investigator、critic、reviewer、repairer、verifier、postmortem 七类运维角色。
- **AgentTeams 插件集成**：`agentteams-plugin-installer` 负责 Dashboard 插件安装；`opskeeper-teamharness` 连接 Worker 与 OpsKeeper MCP 工具。
- **安全边界**：默认只读诊断；变更动作必须绑定待审批 proposal，并匹配 incident、manifest、目标、命令与 payload hash。
- **事件控制面**：持久事件时间线、恢复信号、proposal 状态、审计事件与可重放指标。
- **运维上下文**：PostgreSQL 事件记忆、Qdrant 向量召回、关键词召回、RRF 排序与候选决策留痕。
- **可观测性**：OpenTelemetry trace context、Prometheus、Loki、Tempo 与 Grafana。
- **Web 控制台**：React/Vite 实现的事件、工作流、审批、审计与运行观察界面。

## 构建与验证

```bash
# 后端
go build ./...
go test ./... -count=1
make build

# Web
cd web
pnpm install --frozen-lockfile
pnpm typecheck
pnpm test
pnpm run build

# 插件
make -C plugins/agentteams-plugin-installer plugin-zip
bash plugins/opskeeper-teamharness/scripts/build-package.sh
python3 -m unittest discover -s plugins/opskeeper-teamharness -p 'test_*.py'

# 开源发布审计
python3 scripts/audit_open_source.py
```

## 合成事件示例

`deploy/incident-events/` 提供连接池耗尽、锁等待、磁盘 I/O 饱和与副本回放延迟的 PostgreSQL 示例。数据均为合成数据，不包含客户标识、生产遥测、凭据、私有端点或真实事件证据。示例使用中性租户 `opskeeper-demo` 和确定性 ID，便于验证时间线、指标、审批状态与审计行为。

## 安全模型

1. 诊断工具默认只读。
2. 变更动作必须匹配同一个已批准 incident、manifest、资源、命令与 payload hash。
3. 修复与验证角色分离。
4. 审批、拒绝、执行、失败、恢复信号与复盘事件均保留。
5. 未知工具、shell/browser 逃逸、绕过审计与跨资源目标默认拒绝。

## 文档

- [AgentTeams 插件安装指南](docs/guides/agentteams-plugin-installation.md)
- [集成指南](docs/integration-guide.md)
- [运维手册](docs/operations-manual.md)
- [公开路线图](ROADMAP_PUBLIC.md)

## 致谢与许可证

感谢 GoAI AgentTeams 与 AgentTeams Dashboard 在多 Agent 协同、插件扩展和可观测交互上的启发。完整归因、非派生与非背书边界见 [docs/ACKNOWLEDGMENTS.md](docs/ACKNOWLEDGMENTS.md)。

OpsKeeper 按 Apache License 2.0 分发。详见 [LICENSE](LICENSE)、[NOTICE.md](NOTICE.md) 与 [TRADEMARK.md](TRADEMARK.md)。
