# Manager 可用工具

## 派活类

- spawn_worker(role, task_payload) — 派发 opskeeper 6 Worker
- dispatch_decision_tree(incident) — 执行 opskeeper-coordination 决策表（硬编码，不靠 LLM）

派发后规则：一次 `message send` 成功即结束当前回合；状态读取只能由下一回合的
Worker 回报事件触发，禁止同回合轮询。
派发消息必须携带 `OPSKEEPER TASK <task_id>` 与 `@manager:<server>` 回报地址；
在收到匹配的 `@manager:<server> OPSKEEPER_RESULT <task_id>` 前，不响应空续跑，
也不重复发送同一 task。
收到来自 Worker 房间的匹配结果后，插件会直接向原始请求房间发送
`@admin:<server> OPSKEEPER_COMPLETE <task_id>` 摘要；直接发送失败时才由 Manager
按 fallback 指令补发。

## 状态类

- state.get(task_id) — 读 MinIO state.json
- state.put(task_id, state) — 写 MinIO state.json（带对象锁 CAS）
- state.list() — 列当前所有 incident 顶层状态

## HITL 类

- hitl.request(task_id, blast_radius, signers_required) — 创建 HITL 双签请求
- hitl.decide(task_id, decision, signers, reason) — 上报 HITL 决策（opskeeper /v1/hitl/decide）

## 审计类

- audit.list(resource) — 读 opskeeper audit
- audit.search(action, actor) — 搜 opskeeper audit

## 不允许 Manager 直接调

- 任何 opskeeper 业务工具（metric.query / incident.update / postgres.*）— 派 Worker 去做
- 任何 mutating 工具 — 必须经 reviewer + HITL
