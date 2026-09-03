# opskeeper 协同规约

本规约适用于使用 opskeeper-teamharness 插件的所有 AgentTeams 团队。

## 7 职能 Worker + Manager 拓扑

```
┌────────────────────────────────────────────────────────────┐
│ Manager（agentteams 自带，qwenpaw runtime）               │
│   - 派活决策：按 opskeeper-coordination SKILL 硬编码规则    │
│   - 状态追踪：写 MinIO shared/opskeeper/tasks/{id}/state.json│
│   - HITL：通过 opskeeper /v1/hitl/decide 上报决策          │
│   - Safety：派发前 resolve SafetyLevel（L0–L3）           │
└────────────────────────────────────────────────────────────┘
        │  spawn（按 opskeeper-coordination 决策表）
        ▼
┌────────────────────────────────────────────────────────────┐
│ 7 Worker（每 Worker 一个 qwenpaw 实例 + opskeeper 插件）    │
│ alerter → investigator → critic ↺ investigator             │
│                       ↓                                   │
│                     reviewer（异步 background=true）        │
│                       ↓                                   │
│                  [HITL L2] cluster / tenant_wide          │
│                       ↓                                   │
│                    repairer（L1/L2）                       │
│                       ↓                                   │
│                    verifier → postmortem（v1.0.2 新增）    │
└────────────────────────────────────────────────────────────┘
```

## 协同规则

1. **派活决策硬编码**：Manager 不靠 LLM 推断派给谁；按 opskeeper-coordination SKILL 的决策表。
2. **状态分层**：AgentTeams state.json 持顶层 6 阶段元状态；opskeeper loop 引擎 7 阶段对调用方不可见。
3. **工具白名单**：每个 Worker SKILL.md 的 `disallowed_tools` 强制边界；opskeeper 服务端 cmdpolicy + Casbin 二次校验。
4. **审计全留痕**：所有 mutating 操作 + HITL 决策 + 状态推进写 opskeeper audit。
5. **复盘驱动改进**：postmortem 自动写 knowledge vault + 更新 RCA 模式。
6. **标记等待**：Manager 派发携带 `OPSKEEPER TASK <task_id>` 与回报地址，Worker 用
   `@manager:<server> OPSKEEPER_RESULT <task_id>` 唤醒后续推进；无 @ Manager 的
   群聊回报只进入历史缓存，不保证唤醒 Manager。
7. **结果回流**：Worker 房间中的结果唤醒 Manager 后，Manager 必须向原始请求房间
   回发 `@admin:<server> OPSKEEPER_COMPLETE <task_id>`，保证入口会话可见闭环。
