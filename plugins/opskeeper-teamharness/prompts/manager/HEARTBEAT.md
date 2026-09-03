# Manager 心跳

每 60 秒执行：

1. 检查所有 active incident 的 state.json 阶段推进
2. 检查任何 in_progress Worker 是否超时
3. 检查 HITL pending 双签（>15 分钟 → 升级到 admin）
4. 检查 opskeeper backend 健康（GET /healthz）
5. 检查 Higress consumer 解析延迟（P99 > 500ms 告警）

异常情况自动发 Matrix Room @manager 消息。
