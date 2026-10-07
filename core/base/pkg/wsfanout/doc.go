// Package wsfanout 提供 WebSocket / 流式会话的跨副本协调能力。
//
// 设计动机：platform-base-ha 实现了 HTTP active-active + leader-only worker，
// 但 WebSocket / SSE / SSH 隧道等长连接仍绑死单副本。当客户端调 stop / kill
// API 路由到非 owning pod 时，跨副本请求静默失败。
//
// 本包给 AIOps chat SSE、WebShell 等场景提供：
//   - 跨副本会话注册表（Redis Hash + TTL）：任意副本可查 session 的 owning pod
//   - 跨副本控制消息总线（Redis Pub/Sub）：异步向 owning pod 发送 stop / kill
//   - 降级行为：Redis 不可达时不阻塞业务，仅记 metric
//
// 设计依据：docs/superpowers/specs/2026-07-15-websocket-fanout-design.md
package wsfanout
