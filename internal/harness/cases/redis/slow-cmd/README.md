# redis/slow-cmd — Harness case

Redis 慢命令阻塞（KEYS * / 大集合操作）— 端到端 Demo 候选 case。

## 注入步骤

1. 启动 Redis test 实例
2. 通过 case.go Inject 注入 `redis-cli DEBUG SLEEP <N>` 或大集合 KEYS / SMEMBERS 命令
3. 触发告警链路（外部 webhook → opskeeper alerter）
4. 验证 6 阶段状态推进：detected → rca → critic_audit → review → repair_execution → verification → postmortem

## 期望行为（AgentTeams 协同路径）

| Agent | 工具调用（plugin name） | 触发条件 |
|---|---|---|
| opskeeper-alerter | `metric.query` (PromQL `redis_commands_duration_seconds`) | latency p99 > 100ms |
| opskeeper-investigator | `loop.investigate` + `host.get_processes` | alerter.completed AND rca 未启动 |
| opskeeper-critic | `loop.investigate` (重读) | investigator.confidence < 0.6 |
| opskeeper-reviewer | `incident.get` | repair_plan.proposed AND destructive=true |
| opskeeper-repairer | `host.restart_service` | reviewer.approve OR blast_radius=host |
| opskeeper-verifier | `recovery.verify` | repairer.completed |

## 与 pg/long-running-tx 的差异

| 维度 | pg/long-running-tx | redis/slow-cmd |
|---|---|---|
| blast_radius | cluster（PG 锁链） | host（单实例重启） |
| HITL 双签 | 走（tenant_wide） | 跳过 |
| 修复手段 | `pg.kill_session`（注入后端清理） | `host.restart_service`（plugin 工具） |
| 检测信号 | `postgres.long_running_tx`（plugin 原生） | `metric.query` (PromQL `redis_*`) |
| 知识依赖 | low | medium（需查 redis slowlog 配置） |

## 评分（在 pg case 基础上加）

- rca_accuracy >= 0.85（RootCauseJSON 含 "slow command" + 命令名 + 客户端 IP）
- time_to_detect <= 90s
- time_to_remediate <= 180s
- state_progression 完整 7 阶段
- worker_spawn_count = 6（6 Worker 实际拉起）

## AgentTeams 集成扩展

详见 `case.yaml` 末尾 `agentteams:` 字段。包含：
- 6 Worker 派活决策表
- 7 阶段 state_progression
- plugin 工具集 vs backend 工具名映射（NAME_REMAP）
- rubric_addons（state_progression_complete / worker_spawn_count / hitl_path_skipped）

## 文件

- `case.yaml` — Harness case（含 agentteams 扩展）
- `README.md` — 本文件
