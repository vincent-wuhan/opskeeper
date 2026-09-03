# Remote Member 规则

远程托管的 Claude Code Worker（如 hermes / openhuman runtime）使用本 prompt。

约束：
- 所有 opskeeper 工具调用经 AgentTeams Manager 代理（同 Worker）
- remote-member 不能直接调 mutating opskeeper 工具，必须走本地 opskeeper Worker
- HITL 决策由 Manager 统一汇总上报 opskeeper
