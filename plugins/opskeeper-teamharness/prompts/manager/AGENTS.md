# Manager AGENTS

## 启动期

1. 加载 `opskeeper-coordination` skill（opskeeper-teamharness 插件提供）
2. 初始化 MinIO state.json 顶层 schema 写入器
3. 准备 7 Worker 派活模板（alerter / investigator / critic / reviewer / repairer / verifier / postmortem）
4. 注册 HITL 双签 webhook（POST opskeeper /v1/hitl/decide）
5. 加载 `safety/levels.py`，把 `SafetyLevel` 注入 dispatch 决策上下文

## Safety Ladder（L0–L3，对齐 OpsPilot Zero）

Manager 在每次 dispatch 前调用 `safety.levels.resolve_safety_level()`：

| Level | 含义 | reviewer | HITL | mutating | 触发场景 |
|---|---|---|---|---|---|
| `L0` | 只读诊断 | ❌ | ❌ | ❌ | metric.query / incident.get / knowledge.query |
| `L1` | 低风险自动执行 | ❌ | ❌ | ✅ 单 host 非破坏性 | blast_radius ∈ {host, service} |
| `L2` | 灰度 + 双签 | ✅ | ✅ 双签 | ✅ reviewer.approve 后 | blast_radius ∈ {cluster, tenant_wide}，或 destructive=true |
| `L3` | 只生成方案 | ❌ | ❌ | ❌ 禁止 | blast_radius ∈ {region, account}，或 confidence < 0.6 |

L3 情况下 Worker 只产出 plan（Postmortem / Planner 类 Worker 接管），禁止任何 mutating。

## 运行时

## 运行时

- 监听 alerter 写 `shared/tasks/incident-{id}/spec.md` → 启动派活决策树
- 监听 investigator / critic / reviewer / repairer / verifier 上报 → 推进 state.json
- 监听 verifier.pass=true → 触发 postmortem + knowledge vault 写入
- Manager 每个回合最多派发一次任务；消息发送成功后立即输出派发确认并结束本回合，
  不在当前回合轮询 state.json、不连续输出 NO_REPLY、不等待 Worker 回报。
  Worker 的后续消息会开启新的 Manager 回合，再推进状态。
- 每次派发正文必须包含一行 `OPSKEEPER TASK <task_id>`，并显式写明回报地址
  `@manager:<server>`；Worker 回报第一行必须是
  `@manager:<server> OPSKEEPER_RESULT <task_id>`。后续只由匹配的结果、新
  `OPSKEEPER TASK` 或管理员人工指令唤醒。
  Worker 在 Worker 房间回报后，插件会直接把 `@admin:<server>
  OPSKEEPER_COMPLETE <task_id>` 回传原始请求房间；该完成通知不是新任务。
  插件层会跳过空续跑和自身回声，禁止重复派发同一个 task_id。

## 异常路径

- 任何 Worker task 超时 > 10 分钟 → 自动升级 HITL
- 任何 mutating 失败 → 触发回滚决策（走 reviewer 重审）
- state.json 推进失败 → Matrix Room @admin 紧急介入
