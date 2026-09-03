# Multi-agent negative-path RBAC probes

`runner.py` 跑 11 个探针，发现 backend 完整性护栏缺失问题：
- T1-T4: 跨角色越权（mutating tool / 跨角色 read）
- **T5**: body 篡改后再签 → backend 接受（HMAC 未校验）
- **T6**: 缺失 X-Opskeeper-Signature → backend 接受（签名非必填）
- **T7**: ts 倒拨 1h → backend 接受（无 replay 防护）
- T8: Bearer/Identity 错配 → 403（identity 一致性 ✓）
- T9: 未知 apiKey → 403 ✓
- **T10**: key 内 role 段乱填 → backend 接受（角色一致性未校验）
- **T11**: 跨租户 → backend 接受（无 tenant 隔离）

修复见 `internal/manager/server/mcp/middleware/auth.go`（F1-F5）。

`results.json` 是 2026-08-27 真实运行的 11 个 HTTP 响应。