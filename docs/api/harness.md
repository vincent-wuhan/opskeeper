# API 文档：Harness 评测平台

> **范围**：golden case 评测 + 双模型 judge 评分 + leaderboard 回归
> **关联**：
> - Spec：[openspec/specs/harness-eval-platform/spec.md](../../openspec/specs/harness-eval-platform/spec.md)
> - ADR：[docs/superpowers/decisions/2026-07-13-harness-judge-models.md](../superpowers/decisions/2026-07-13-harness-judge-models.md)
> - 使用指南：[docs/harness-guide.md](../harness-guide.md)

---

## 一、case 管理

### 1.1 列出 case

```
GET /api/v1/harness/cases
```

**Query**：

| 参数 | 类型 | 说明 |
|---|---|---|
| `severity` | string | P0 / P1 / P2 / P3 |
| `tag` | string | 按 tag 过滤 |
| `resource` | string | pg / redis / mq / k8s / host |

**响应**：

```json
{
  "code": 0,
  "data": {
    "items": [
      {
        "id": "pg/long-running-tx",
        "description": "模拟 PG 长事务导致锁等待和性能下降",
        "severity": "P0",
        "tags": ["pg", "lock", "performance"],
        "resource": "pg",
        "rubric": {
          "rca_accuracy": 0.85,
          "time_to_remediate": 120
        }
      }
    ],
    "total": 60
  }
}
```

### 1.2 校验 case

```
POST /api/v1/harness/cases/validate
```

**Body**：单个 case YAML 或 JSON。

**响应**：

```json
{
  "code": 0,
  "data": {
    "valid": true,
    "warnings": ["inject.duration=600s 接近 prod 限制 300s"]
  }
}
```

---

## 二、运行评测

### 2.1 同步运行单个 case

```
POST /api/v1/harness/runs
```

**Body**：

```json
{
  "case_id": "pg/long-running-tx",
  "env": "staging",
  "timeout_s": 300
}
```

**响应**：

```json
{
  "code": 0,
  "data": {
    "run_id": "hr-abc123",
    "status": "running",
    "case_id": "pg/long-running-tx",
    "started_at": "2026-07-13T10:00:00Z"
  }
}
```

### 2.2 同步运行 suite

```json
{
  "suite": "middleware-baseline",
  "env": "staging",
  "judge_models": ["claude-sonnet-4", "gpt-4o"]
}
```

### 2.3 异步运行 + webhook

```json
{
  "case_id": "pg/long-running-tx",
  "env": "staging",
  "async": true,
  "webhook_url": "https://ci.example.com/callback",
  "webhook_secret_ref": "<encrypted_secret_ref>"
}
```

执行完成时 POST 到 webhook：

```json
{
  "run_id": "hr-abc123",
  "status": "completed",
  "score": {
    "overall": 0.92,
    "rca_accuracy": 0.95,
    "time_to_detect_s": 18,
    "time_to_remediate_s": 85,
    "collateral_damage": false,
    "rubric_compliance": 0.92
  }
}
```

---

## 三、查询 run

### 3.1 run 详情

```
GET /api/v1/harness/runs/{run_id}
```

**响应**：

```json
{
  "code": 0,
  "data": {
    "run_id": "hr-abc123",
    "case_id": "pg/long-running-tx",
    "env": "staging",
    "status": "completed",  // running / completed / failed / flagged
    "started_at": "2026-07-13T10:00:00Z",
    "completed_at": "2026-07-13T10:02:30Z",
    "duration_s": 150,
    "judge_results": [
      {
        "model": "claude-sonnet-4",
        "score": 0.93,
        "rubric_breakdown": { ... }
      },
      {
        "model": "gpt-4o",
        "score": 0.91,
        "rubric_breakdown": { ... }
      }
    ],
    "final_score": 0.92,
    "flagged": false,
    "agent_response": { ... },   // Agent 的实际响应
    "injection_logs": [ ... ]   // fault-injector 日志
  }
}
```

### 3.2 run 列表

```
GET /api/v1/harness/runs?since=24h&case_id=pg/long-running-tx
```

---

## 四、注入操作

### 4.1 注入故障

```
POST /api/v1/harness/inject
```

**Body**：

```json
{
  "case_id": "k8s/pod-oom",
  "env": "staging",
  "max_duration_s": 300,
  "params": {
    "namespace": "default",
    "deployment": "order-svc",
    "memory_limit": "256Mi"
  }
}
```

**响应（默认 staging）**：

```json
{
  "code": 0,
  "data": {
    "inject_id": "inj-xyz789",
    "status": "injecting",
    "started_at": "2026-07-13T10:00:00Z",
    "auto_rollback_at": "2026-07-13T10:05:00Z"
  }
}
```

### 4.2 prod 环境拦截

请求 `env=prod` 时：

```json
{
  "code": 0,
  "data": {
    "status": "requires_dual_approval",
    "approvers_required": 2,
    "approvers_so_far": [],
    "approval_ticket_id": "at-inj-prod-001"
  }
}
```

需要 2 名审批人通过后才会执行。

### 4.3 手动停止注入

```
POST /api/v1/harness/inject/{inject_id}/stop
```

---

## 五、Leaderboard

### 5.1 查看 leaderboard

```
GET /api/v1/harness/leaderboard?since=30d
```

**响应**：

```json
{
  "code": 0,
  "data": {
    "since": "30d",
    "total_runs": 240,
    "overall_score": 0.90,
    "baseline_score": 0.91,
    "delta": -0.01,
    "cases": [
      {
        "case_id": "pg/long-running-tx",
        "score_avg": 0.92,
        "delta_vs_baseline": +0.02,
        "runs": 30,
        "status": "pass"
      },
      {
        "case_id": "redis/big-key",
        "score_avg": 0.88,
        "delta_vs_baseline": -0.03,
        "runs": 25,
        "status": "warn"
      }
    ]
  }
}
```

### 5.2 锁定基线

```
POST /api/v1/harness/leaderboard/lock
```

**Body**：

```json
{
  "version": "v1.0",
  "comment": "release v1.0 基线"
}
```

锁定后所有评分与此对比。每月一次。

### 5.3 检查回归

```
POST /api/v1/harness/leaderboard/check-regression
```

**Body**：

```json
{
  "run_results": [...]  // 来自 /runs 的响应
}
```

**响应**：

```json
{
  "code": 0,
  "data": {
    "passed": true,
    "max_drop_pct": 3.2,
    "alerts": [],
    "blocks": []
  }
}
```

`blocks` 非空时返回 422：

```json
{
  "code": 4022,
  "message": "回归阻断：评分下降 > 15%",
  "data": {
    "blocks": [
      { "case_id": "k8s/pod-oom", "drop_pct": 18.4 }
    ]
  }
}
```

---

## 六、Judge 模型

### 6.1 列出可用模型

```
GET /api/v1/harness/judge/models
```

### 6.2 单 case 外部 judge

```
POST /api/v1/harness/judge
```

**Body**：

```json
{
  "case_id": "pg/long-running-tx",
  "agent_response": { ... },
  "models": ["claude-sonnet-4", "gpt-4o"]
}
```

用于本地调试或第三方评测集成。

### 6.3 一致性指标

```
GET /api/v1/harness/judge/consistency?since=30d
```

返回两模型一致率（spec §Requirement "judge 双模型取均值"目标 ≥ 80%）。

---

## 七、错误码

| HTTP | code | 含义 |
|---|---|---|
| 200 | 0 | 成功 |
| 400 | 4000 | 请求参数错误 |
| 401 | 4001 | 未认证 |
| 403 | 4003 | prod 环境注入未授权 / 跨租户 |
| 404 | 4004 | case / run 不存在 |
| 409 | 4009 | run 冲突（同 case_id + env 已有 running）|
| 422 | 4022 | 回归阻断 / schema 校验失败 |
| 429 | 4029 | judge 限速（> 60/min）|
| 500 | 5000 | 服务端错误 |
| 503 | 5003 | 注入器暂不可用（资源忙）|

---

## 八、多租户隔离

所有 run / inject / leaderboard 强制 tenant 隔离：

- run 创建：自动绑定调用者 tenant
- leaderboard：返回当前 tenant 的统计
- 跨租户查看：仅 superuser 可访问（`tenant_id=0`）

---

## 九、相关

- Spec：[openspec/specs/harness-eval-platform/spec.md](../../openspec/specs/harness-eval-platform/spec.md)
- ADR：[docs/superpowers/decisions/2026-07-13-harness-judge-models.md](../superpowers/decisions/2026-07-13-harness-judge-models.md)
- 使用指南：[docs/harness-guide.md](../harness-guide.md)
- Middleware API：[docs/api/middleware.md](middleware.md)
- git-artifact API：[docs/api/git-artifact.md](git-artifact.md)
