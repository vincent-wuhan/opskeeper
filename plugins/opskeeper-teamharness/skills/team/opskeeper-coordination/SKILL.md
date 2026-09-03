---
name: opskeeper-coordination
description: "Manager 派活决策树。基于 incident severity / blast_radius / confidence 硬编码派活规则，不依赖 LLM 推断派给谁。"
---

# opskeeper Coordination — 派活决策树

Manager 在收到 opskeeper 告警或 investigator 输出时，按以下决策树硬编码派发到 7 个 Worker。不依赖 LLM 推断（避免幻觉派活）。

## 决策表

| 触发条件 | 派活目标 | Safety Level | 备注 |
|---|---|---|---|
| `incident.severity ∈ {warning, critical}` AND `phase=detected` | `opskeeper-alerter` | L0 | 先聚合 dedup，再写 incident |
| `alerter.completed` AND `rca 未启动` | `opskeeper-investigator` | L0 | 调 `loop.investigate` |
| `investigator.completed` AND `confidence < 0.6` | `opskeeper-critic` | L0 | 后置审计 |
| `critic.audit.completed` AND `needs_correction=true` | `opskeeper-investigator`（重派，携 critic issues） | L0 | 重派计数 +1，超 2 次升级 HITL |
| `repair_plan.proposed` AND `blast_radius ∈ {cluster, tenant_wide}` OR `destructive=true` | `opskeeper-reviewer`（异步 background=true） | L2 | 等 reviewer.approve |
| `repair_plan.approved` AND `blast_radius ∈ {cluster, tenant_wide}` | **HITL** → `POST opskeeper /v1/hitl/decide`（含 `safety_level=L2`） | L2 | 等 Matrix Room @admin 双签 |
| `hitl.approved` OR `blast_radius ∈ {host}` | `opskeeper-repairer` | L1 / L2 | 调 mutating 工具 |
| `repairer.completed` | `opskeeper-verifier` | L0 | 调 `recovery.verify` |
| `verifier.pass=true` | `opskeeper-postmortem`（v1.0.2 新增 Worker） | L0 | Manager 写 state.json 推进 phase=postmortem |

> Level 解析统一调用 `safety/levels.py::resolve_safety_level(blast_radius, confidence, destructive)`。
> L3（blast_radius ∈ {region, account} 或 confidence < 0.6）→ Manager 直接派 postmortem，不走修复链。

## 状态推进

派发消息和 Worker 回报必须携带同一个任务标记：

```text
OPSKEEPER TASK incident-123-investigator
@manager:matrix.example.com OPSKEEPER_RESULT incident-123-investigator {...}
```

`OPSKEEPER_RESULT` 到达前，Manager 不轮询、不重复派发、不响应空续跑。Worker
必须在结果行同一行 @ Manager，否则群聊消息只进入历史缓存；管理员人工指令和
新 `OPSKEEPER TASK` 可以优先唤醒。
Worker 房间的结果唤醒 Manager 后，Manager 必须向原始请求房间回发
`@admin:<server> OPSKEEPER_COMPLETE <task_id>`；完成通知不触发新派发。

每次派活决策后必须 `state.put`：

```json
{
  "task_id": "incident-123",
  "phase": "rca",
  "status": "in_progress",
  "assigned_to": "opskeeper-investigator",
  "retry_count": 0,
  "blast_radius": "cluster",
  "audit": [{"event": "dispatch", "from": "alerter", "to": "investigator", "at": "..."}]
}
```

## PostgreSQL 连接池耗尽分支

当 `fault_family=capacity/connection_pool`、`resource_type=pg`，或初始证据包含
`probe failed`、`waiters>0`、`active_connections>=capacity` 时，按以下闭环推进：

1. **alerter**：只聚合同一 `incident_id`、`pool_manifest_id` 和时间窗口内的超时、
   probe 失败与连接池饱和证据；不得把共享 PostgreSQL 的其他告警合并进该事故。
2. **investigator**：必查 `postgres.analyze_status`、`metric.query`、`incident.get`
   与 `loop.investigate`。RootCauseJSON 必须区分应用连接池耗尽与共享 PostgreSQL
   容量不足，并给出 `pool_manifest_id`、active/capacity、waiters、probe 延迟、
   `pg_stat_activity` 证据。禁止直接修改 incident 状态。
3. **critic / reviewer**：审查证据链是否覆盖容量、等待者、probe 失败与数据库侧
   反证；置信度低于 0.6 时回派 investigator。修复提案只允许
   `command=resize_pool`、`target=pg:pool-fixture`、`resource_type=pg`，且
   `pool_manifest_id` 必须属于该 incident。
4. **HITL**：向用户展示动作、目标 pool、容量变化、blast radius、回滚方式和证据。
   未获批准前禁止执行。AgentTeams caller 不能设置 `skip_audit`。
5. **repairer**：仅在运行时管理员显式配置 `OPSKEEPER_PERMISSION_MODE=standard`
   且拿到已批准 `proposal_id` 后调用 `recovery.execute`。参数只能是
   `incident_id`、`pool_manifest_id`、`reason`；禁止重启共享 PostgreSQL、kill
   无关连接、执行 shell/browser 或写业务文件。
6. **verifier**：以修复后 probe 成功、active/capacity 恢复、waiters 下降、请求
   延迟回落为通过条件；命令执行成功本身不等于恢复成功。
7. **postmortem**：只有在 verifier 通过后输出复盘并写入 knowledge vault，记录
   根因、审批、恢复动作、VerifiedDelta 与防复发建议。

该分支的初始故障可以通过 `pool-fixture` 注入，但所有阶段推进必须由 Manager
调度 Worker 并通过 MCP 工具完成；不得用脚本直接改状态或代替 Worker 回报。

## HITL 双签

`blast_radius ∈ {cluster, tenant_wide}` 且 `destructive=true` 时，按 ADR-019 走 opskeeper-admin + opskeeper-observer 双签。opskeeper `/v1/hitl/decide` 验证签名后推进 state.json。

## 异常路径

- investigator 重派 > 2 次 → 自动升级到 opskeeper-reviewer + Matrix Room @admin
- critic / reviewer 拒绝 → 回派 investigator 或取消修复
- verifier.fail → 回派 repairer
- 所有失败均写 audit + 触发 postmortem
