# Leader 角色规则（占位）

opskeeper-teamharness 暂不强制使用 Leader 角色。当前架构是 Manager + 6 Worker 平铺。
Leader 仅在用户显式启用 DAG/Loop Project Work 时出现（参考 teamharness team-coordination SKILL）。

不要使用 Leader 角色做 opskeeper 派活决策——所有派活走 opskeeper-coordination SKILL。
