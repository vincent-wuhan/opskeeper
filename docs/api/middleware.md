# API 文档：中间件 Adapter

> **范围**：PG / Redis / RabbitMQ / Kafka / K8s Cluster / Git Repository 6 类资源的 REST API
> **鉴权**：所有端点 JWT 必填，多租户隔离强制 `tenant_id`
> **关联**：
> - Spec：[openspec/specs/middleware-adapter/spec.md](../../openspec/specs/middleware-adapter/spec.md)
> - 集成指南：[docs/integration-guide.md](../integration-guide.md)

---

## 一、通用约定

### 1.1 响应格式

```json
{
  "code": 0,
  "message": "ok",
  "data": { ... }
}
```

- `code = 0` 成功；非 0 失败
- `message` 中文（与 AGENTS.md 一致）
- `data` 业务数据（可为 null）

### 1.2 错误码

| HTTP | code | 含义 |
|---|---|---|
| 200 | 0 | 成功 |
| 400 | 4000 | 请求参数错误 |
| 401 | 4001 | 未认证（JWT 缺失 / 无效）|
| 403 | 4003 | 无权限（Casbin 拒绝 / tenant 不匹配）|
| 404 | 4004 | 资源不存在 |
| 409 | 4009 | 资源冲突（已存在）|
| 422 | 4022 | 业务校验失败（凭据错误 / 连接失败）|
| 429 | 4029 | 限流（按 tenant + endpoint）|
| 500 | 5000 | 服务端错误 |
| 503 | 5003 | Adapter 不可用（resource down）|

### 1.3 鉴权

```bash
curl -H "Authorization: Bearer $JWT" \
     -H "X-Tenant-ID: 42" \
     https://ops.example.com/api/v1/middleware/...
```

- JWT 必须包含 `sub`（user_id）、`tenant_ids`（可访问租户列表）
- `X-Tenant-ID` 可选；不填则用 JWT 的默认 tenant

### 1.4 限流

默认每租户 1000 req/min，超出返回 429。`--rate-limit` Helm 配置可调。

---

## 二、资源管理

### 2.1 列出中间件资源

```
GET /api/v1/middleware
```

**Query**：

| 参数 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `type` | string | 否 | postgres / redis / rabbitmq / kafka / k8s / git |
| `status` | string | 否 | healthy / degraded / down |
| `page` | int | 否 | 页码（默认 1）|
| `size` | int | 否 | 每页条数（默认 20，最大 100）|

**响应**：

```json
{
  "code": 0,
  "data": {
    "items": [
      {
        "id": "mw-123",
        "type": "postgres",
        "name": "prod-pg-1",
        "host": "pg-prod-1.example.com",
        "port": 5432,
        "status": "healthy",
        "created_at": "2026-07-13T10:00:00Z",
        "updated_at": "2026-07-13T10:30:00Z"
      }
    ],
    "total": 42,
    "page": 1,
    "size": 20
  }
}
```

### 2.2 创建中间件资源

```
POST /api/v1/middleware
```

**Body**：

```json
{
  "type": "postgres",
  "name": "prod-pg-1",
  "host": "pg-prod-1.example.com",
  "port": 5432,
  "database": "orders",
  "username": "monitor_user",
  "password": "<encrypted_secret_ref>",
  "ssl_mode": "require",
  "tags": { "env": "prod", "team": "data" }
}
```

**响应**：

```json
{
  "code": 0,
  "data": {
    "id": "mw-123",
    "status": "healthy",
    "verify_message": "Connection OK, 3 databases found"
  }
}
```

**错误**：

- 422：凭据错误 / 连接超时
- 409：同名资源已存在

### 2.3 查看资源详情

```
GET /api/v1/middleware/{id}
```

**响应**：

```json
{
  "code": 0,
  "data": {
    "id": "mw-123",
    "type": "postgres",
    "name": "prod-pg-1",
    "host": "pg-prod-1.example.com",
    "port": 5432,
    "database": "orders",
    "username": "monitor_user",
    "password_masked": "********1234",   // 始终 mask
    "ssl_mode": "require",
    "status": "healthy",
    "last_healthcheck": "2026-07-13T10:30:00Z",
    "tags": { "env": "prod", "team": "data" },
    "created_at": "2026-07-13T10:00:00Z"
  }
}
```

> 凭据字段始终 mask；运行时进程内解密使用。

### 2.4 更新资源

```
PATCH /api/v1/middleware/{id}
```

部分更新，所有字段可选。

### 2.5 删除资源

```
DELETE /api/v1/middleware/{id}
```

需要 Casbin 权限 + 二次确认。

---

## 三、诊断操作（只读）

### 3.1 PG 诊断

```
POST /api/v1/middleware/{id}/diagnose/pg
```

**Body**：

```json
{
  "action": "top_queries",
  "params": { "limit": 20, "min_duration_ms": 1000 }
}
```

**响应**：

```json
{
  "code": 0,
  "data": {
    "queries": [
      {
        "query": "SELECT * FROM orders WHERE ...",
        "calls": 1234,
        "avg_duration_ms": 1520,
        "total_duration_ms": 1874880
      }
    ]
  }
}
```

支持的 `action`：

- `top_queries` — TOP 慢查询
- `pg_stat_activity` — 当前活动 session
- `pg_locks` — 锁等待
- `table_bloat` — 表膨胀
- `vacuum_status` — vacuum 进度
- `replication_lag` — 复制延迟

### 3.2 Redis 诊断

```
POST /api/v1/middleware/{id}/diagnose/redis
```

**Body**：

```json
{
  "action": "big_keys",
  "params": { "scan_count": 1000 }
}
```

**响应**：

```json
{
  "code": 0,
  "data": {
    "big_keys": [
      { "key": "user:profile:42", "size_mb": 8.4, "type": "hash" }
    ]
  }
}
```

支持的 `action`：

- `big_keys` — 大 key
- `hot_keys` — 热 key
- `slow_cmd` — 慢命令
- `memory_info` — 内存详情
- `cluster_info` — 集群拓扑

### 3.3 MQ 诊断 / 3.4 K8s 诊断 / 3.5 Git 诊断

类似结构，详见 OpenAPI schema。

---

## 四、执行操作（写，必审批）

### 4.1 PG kill_session

```
POST /api/v1/middleware/{id}/execute/pg
```

**Body**：

```json
{
  "action": "kill_session",
  "params": { "pid": 12345, "mode": "fast" }
}
```

**响应（审批拦截）**：

```json
{
  "code": 0,
  "data": {
    "approval_ticket_id": "at-abc123",
    "status": "pending_approval",
    "preview": {
      "session_info": {
        "pid": 12345,
        "user": "app_user",
        "query": "SELECT pg_sleep(60)",
        "duration_s": 245,
        "state": "idle in transaction"
      }
    }
  }
}
```

**审批通过后执行**：

```json
{
  "code": 0,
  "data": {
    "approval_ticket_id": "at-abc123",
    "status": "executed",
    "exit_code": 0,
    "output": "session killed"
  }
}
```

### 4.2 Redis flushdb（强保护）

需要 **双人审批** + 强告警（影响所有 key）：

```json
{
  "code": 0,
  "data": {
    "approval_ticket_id": "at-xyz789",
    "status": "pending_dual_approval",
    "approvers_required": 2,
    "approvers_so_far": ["@alice"],
    "alert_sent": ["slack:#opskeeper-alerts"]
  }
}
```

### 4.3 审批回调

```
POST /api/v1/middleware/approvals/{ticket_id}/decide
```

**Body**：

```json
{
  "decision": "approve",
  "comment": "已确认影响面"
}
```

`decision`：`approve` / `reject`。

---

## 五、租户隔离

所有端点强制 `tenant_id` 过滤：

- 资源创建：自动绑定调用者 tenant
- 资源查询：`WHERE tenant_id = ?` 强制 SQL 过滤（SQL 注入防护）
- 资源执行：审批工单同样带 tenant，跨租户审批无效

**superuser**（tenant_id = 0）可跨租户查询，但写操作仍受 tenant 限制（除非显式 `--cross-tenant`，需特殊权限）。

---

## 六、Webhook（可选）

资源状态变化时可注册 webhook：

```json
POST /api/v1/middleware/{id}/webhooks
{
  "url": "https://my-app.example.com/opskeeper-webhook",
  "events": ["status_change", "approval_pending"],
  "secret": "<encrypted_secret_ref>"
}
```

---

## 七、相关

- Spec：[openspec/specs/middleware-adapter/spec.md](../../openspec/specs/middleware-adapter/spec.md)
- 集成指南：[docs/integration-guide.md](../integration-guide.md)
- Harness 评测：[docs/harness-guide.md](../harness-guide.md)
- git-artifact API：[docs/api/git-artifact.md](git-artifact.md)
