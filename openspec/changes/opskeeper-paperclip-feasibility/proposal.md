# Why

OpsKeeper 当前以 AgentTeams/Matrix 作为多 agent 协作与人工审批入口，已经具备事故、证据、RCA、修复预演、审批、执行和独立验证的闭环能力。Paperclip 定位为开源 AI agent 工作控制台，提供公司/项目/任务、agent 组织、审批、预算、执行心跳、插件、webhook 和治理模型。两者概念高度互补，但直接把 OpsKeeper 改造成 Paperclip adapter，或用 Paperclip 替换 AgentTeams 协作面，都会破坏现有安全边界并引入双重状态源。

本变更只做可行性调研和推荐路线设计，回答三个问题：

1. OpsKeeper 与 Paperclip 的领域模型、API 和扩展机制能否映射。
2. 最小可落地的集成形态是什么。
3. 哪些能力必须保留在 OpsKeeper 权威边界内，不能迁移到 Paperclip 插件。

# What Changes

- 增加 OpsKeeper 适配 Paperclip 的可行性调研与决策文档。
- 明确推荐路线：优先实现 Paperclip 侧 `OpsKeeper Connector Plugin`，以只读事故同步和工作台视图作为第一阶段。
- 定义后续三个阶段：
  - Phase A：只读同步与 Paperclip 工作台展示。
  - Phase B：人工审批桥接，OpsKeeper 继续作为审批与授权权威。
  - Phase C：受控只读 agent tools，让 Paperclip 内 agent 可查询事故、证据与归档。
- 定义明确不做的第一阶段内容：
  - 不把 OpsKeeper 七角色 worker 改造为 Paperclip adapter。
  -不用 Paperclip 替换 AgentTeams/Matrix 房间协作。
  - 不把修复执行、shell、MCP 变更或跨资源操作暴露给 Paperclip 插件。
  - 不在 Paperclip 插件中复制 OpsKeeper 审批账本。
- 记录许可、架构、插件成熟度、安全、数据映射和实施风险。
- 输出后续如果要实施连接器时的任务拆分和验收标准。

# Capabilities

## New Capabilities

- `paperclip-integration-feasibility`: Document the feasible integration boundary, phased architecture, data mapping, security model, and go/no-go criteria for connecting OpsKeeper with Paperclip.

## Modified Capabilities

- None. This change is research and specification only.

# Impact

- 新增 OpenSpec change 文档，不修改 Go 后端、Web 控制台、AgentTeams 插件或线上环境。
- 后续实施建议位于独立 Paperclip 插件包或 `integrations/paperclip-connector/`，不在本次变更中落地。
- 需要以 Paperclip `master@0f14d261233c545aa6a8a38ec253c498a5130fff` 的插件规范为基准，并在实施前复核上游变化。
- 需要为 OpsKeeper 设计 scoped service account；现有管理员会话或高权限 token 不应直接配置给 Paperclip 插件。
- 审批桥接必须保持 OpsKeeper proposal id、resource、command 和 payload hash 的精确匹配，不允许 Paperclip 侧生成第二套授权事实。
