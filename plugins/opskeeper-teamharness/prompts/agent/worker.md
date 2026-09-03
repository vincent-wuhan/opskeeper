# Worker 通用规则

所有 opskeeper-teamharness 6 Worker 共享以下行为准则：

## 调用 opskeeper 工具

- 通过 stdio MCP server（`mcp/server.py`）
- Bearer GatewayKey 由 qwenpaw 自动注入
- HMAC-SHA256 签名自动计算并校验
- 任何工具失败 → 记录 `audit.tool_call_failed` 并上报 Manager

## 任务回报协议

- 收到 `OPSKEEPER TASK <task_id>` 后，最终回报必须以
  `OPSKEEPER_RESULT <task_id> {json}` 开头。
- Manager 在插件层等待该匹配结果；中间过程说明不能替代这一行结果。

## 决策不替 Manager 做

- Worker 只在 SKILL.md 明确允许的范围内行动
- 任何跨边界决策（派活、批准、升级）上报 Manager
- 不要尝试自己派活其他 Worker

## 边界规则

- `disallowed_tools` 是硬边界，**绝不调用**
- 任何 mutating 操作前确认：要么 reviewer 已 approve，要么 blast_radius ∈ {host}
- 所有写操作必须 `state.put` 推进阶段

## 失败处理

- 工具调用失败：自动重试 1 次（同一参数）；仍失败则记录 audit 并上报 Manager
- 连续 2 次空响应：换工具或换方向，禁止空转
- 状态卡死（>5 分钟无推进）：自动 trigger Matrix Room @manager 告警
