# API 文档：git-artifact 制品溯源

> **范围**：从运行时事件（PG query / Redis cmd / K8s image / HTTP route）反查到精确 commit + file:line + author
> **协议版本**：v0 / v1（向后兼容至少 2 个版本）
> **关联**：
> - Spec：[openspec/specs/git-artifact-linker/spec.md](../../openspec/specs/git-artifact-linker/spec.md)
> - 协议：[openspec/changes/archive/2026-07-13-unified-platform-base-selection/protocols/git-artifact-v0.md](../../openspec/changes/archive/2026-07-13-unified-platform-base-selection/protocols/git-artifact-v0.md)
> - 实现：commit 69aceb8

---

## 一、协议头

所有请求必带：

```
X-GitArtifact-Version: v1   # v0 / v1；缺省 v0
Authorization: Bearer <JWT>
```

---

## 二、上报制品

### 2.1 v0 协议

```
POST /api/v1/git-artifacts
Content-Type: application/json
X-GitArtifact-Version: v0
```

**Body**：

```json
{
  "repo_url": "https://github.com/example/order-svc",
  "commit": "abc123def456...",
  "artifact_url": "s3://builds/order-svc/v1.2.3.tar.gz",
  "meta": {
    "build_id": "ci-build-12345",
    "branch": "main",
    "author": "@alice"
  },
  "symbols": [
    {
      "type": "pg_query",
      "value": "SELECT * FROM orders WHERE status = 'pending'",
      "file_path": "src/db/queries.go",
      "line_start": 142,
      "line_end": 158
    },
    {
      "type": "redis_cmd",
      "value": "GET user:profile:42",
      "file_path": "src/cache/user.go",
      "line_start": 88,
      "line_end": 92
    },
    {
      "type": "k8s_image",
      "value": "registry.example.com/order-svc:v1.2.3",
      "file_path": "deploy/k8s/deployment.yaml",
      "line_start": 23,
      "line_end": 27
    },
    {
      "type": "http_route",
      "value": "GET /orders/{id}",
      "file_path": "src/api/orders.go",
      "line_start": 45,
      "line_end": 60,
      "function_name": "GetOrder"
    }
  ]
}
```

**响应（v0）**：

```json
{
  "code": 0,
  "message": "ok",
  "data": {
    "artifact_id": "ga-abc123",
    "indexed_commits": 1,
    "async_rebuild": true
  }
}
```

### 2.2 v1 协议

```
POST /api/v1/git-artifacts
X-GitArtifact-Version: v1
```

**新增字段**（v1 扩展）：

```json
{
  "...": "...",  // 必填字段同 v0
  "sbom": {
    "format": "spdx",
    "url": "s3://builds/order-svc/v1.2.3.spdx.json"
  },
  "provenance": {
    "type": "slsa-provenance-v1",
    "url": "s3://builds/order-svc/v1.2.3.intoto.jsonl"
  },
  "signatures": [
    {
      "key_id": "cosign-key-123",
      "signature": "base64..."
    }
  ]
}
```

### 2.3 必填字段校验

缺任一必填字段返回 **400 Bad Request**：

```json
{
  "code": 4000,
  "message": "缺 meta.build_id 字段",
  "data": {
    "missing_fields": ["meta.build_id"]
  }
}
```

必填字段：

- `repo_url`
- `commit`
- `artifact_url`
- `meta.build_id`

---

## 三、运行时反查

### 3.1 通用反查接口

```
POST /api/v1/runtime-link
X-GitArtifact-Version: v0
```

**Body（按 symbol type 区分）**：

#### PG query 反查

```json
{
  "type": "pg_query",
  "value": "SELECT * FROM orders WHERE status = 'pending'",
  "tenant_id": 42,        // 可选，缺省用 ctx tenant
  "confidence_threshold": 0.7  // 阈值过滤
}
```

**响应**：

```json
{
  "code": 0,
  "data": {
    "tenant_id": 42,
    "matched": [
      {
        "commit": "abc123def456...",
        "repo": "https://github.com/example/order-svc",
        "file_path": "src/db/queries.go",
        "line_start": 142,
        "line_end": 158,
        "author": "@alice",
        "commit_message": "fix: optimize pending order query",
        "confidence": 0.95,
        "match_type": "exact"
      }
    ],
    "needs_human_review": false
  }
}
```

#### Redis cmd 反查

```json
{
  "type": "redis_cmd",
  "value": "GET user:profile:42"
}
```

#### K8s image 反查

```json
{
  "type": "k8s_image",
  "value": "registry.example.com/order-svc:v1.2.3"
}
```

confidence 通常 > 0.9（image tag 显式映射）。

#### HTTP route 反查

```json
{
  "type": "http_route",
  "value": "GET /orders/{id}",
  "method": "GET",
  "path": "/orders/{id}"
}
```

confidence 取决于模糊匹配程度。

### 3.2 低置信度处理

`confidence < 0.7` 时：

```json
{
  "code": 0,
  "data": {
    "matched": [...],
    "needs_human_review": true,
    "reason": "confidence 0.45 < threshold 0.7"
  }
}
```

UI 标注"待人工确认"；Coordinator 在 RCA 报告中标记"代码位置不明确"。

---

## 四、租户隔离

### 4.1 tenant_id 来源

优先级：

1. URL Query `?tenant_id=N`（仅 superuser 可跨租户查询）
2. Request Body `tenant_id` 字段（与 ctx tenant 不一致时 **403**）
3. JWT 中的 `default_tenant_id`
4. HTTP Header `X-Tenant-ID`

### 4.2 scoped index

内部索引 key = `"<tenant_id>\x00<symbol>"`，物理隔离。

- **同租户查询**：先查 `scoped_key` → 命中
- **跨租户查询（superuser）**：fallback 到 `0\x00<symbol>` 全局索引
- **跨租户查询（非 superuser）**：403 拒绝

### 4.3 模糊匹配隔离

模糊匹配（如 PG query 文本模糊）同样先 tenant 范围，再 fallback 全局。避免跨租户泄漏（Task 3.5 修复）。

---

## 五、版本兼容矩阵

| Client Version | Server 支持 | 行为 |
|---|---|---|
| v0 | ✅ | 按 v0 解析；不识别 v1 字段（忽略）|
| v1 | ✅ | 按 v1 解析；v1 字段（SBOM / provenance / signatures）入库 |
| v2 | ⏸ future | v2 协议尚未发布；如发现 v2 header，返回 400 + 提示升级 |

---

## 六、错误码

| HTTP | code | 含义 |
|---|---|---|
| 200 | 0 | 成功 |
| 400 | 4000 | 请求参数错误（如缺 build_id）|
| 401 | 4001 | 未认证 |
| 403 | 4003 | 跨租户查询拒绝（非 superuser）|
| 404 | 4004 | 无匹配 |
| 409 | 4009 | 制品冲突（同 commit + artifact_url 已存在）|
| 422 | 4022 | 协议版本不识别 |
| 500 | 5000 | 服务端错误 |
| 503 | 5003 | 索引器暂不可用（重试）|

---

## 七、异步索引

上报后增量重建索引，异步执行：

```bash
# 查询索引进度
GET /api/v1/git-artifacts/{artifact_id}/index-status
```

响应：

```json
{
  "code": 0,
  "data": {
    "artifact_id": "ga-abc123",
    "status": "indexing",  // indexing / completed / failed
    "indexed_symbols": 42,
    "total_symbols": 100,
    "eta_s": 30
  }
}
```

性能目标：**1000 commit / 5 min**（spec §Requirement "反向索引自动重建"）。

---

## 八、相关

- Spec：[openspec/specs/git-artifact-linker/spec.md](../../openspec/specs/git-artifact-linker/spec.md)
- 协议：[openspec/changes/archive/2026-07-13-unified-platform-base-selection/protocols/git-artifact-v0.md](../../openspec/changes/archive/2026-07-13-unified-platform-base-selection/protocols/git-artifact-v0.md)
- Harness 评测：[docs/harness-guide.md](../harness-guide.md)
- Middleware API：[docs/api/middleware.md](middleware.md)
