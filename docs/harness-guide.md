# Harness 评测指南

> **面向**：opskeeper 开发者、平台 SRE、CI 维护者
> **目的**：用 golden case 评测 Agent 决策质量，用 judge 模型打分，用 leaderboard 跟踪回归
> **关联**：
> - ADR：[docs/superpowers/decisions/2026-07-13-harness-judge-models.md](superpowers/decisions/2026-07-13-harness-judge-models.md)
> - 集成指南：[docs/integration-guide.md](integration-guide.md)
> - 运维手册：[docs/operations-manual.md](operations-manual.md)

---

## 一、概念

| 概念 | 说明 |
|---|---|
| **golden case** | 标准化的事故场景（YAML 描述），含注入 / 期望 / rubric |
| **fault-injector** | 在隔离环境注入故障（PG 长事务 / Redis 大 key / K8s pod OOM / 主机磁盘满）|
| **judge** | LLM 评分模型（默认 Claude Sonnet 4 + GPT-4o 双模型取均值）|
| **leaderboard** | 评分历史 + 回归基线 + 排名 |
| **regression baseline** | 历史评分基线，新评分对比基线判断是否下降 |

## 二、CLI 速查（`cmd/opskeeper-eval`）

```bash
opskeeper-eval --help
opskeeper-eval list-cases                           # 列出全部 golden case
opskeeper-eval list-cases --severity P0             # 按严重度
opskeeper-eval run --case pg/long-running-tx        # 跑单个 case
opskeeper-eval run --suite middleware-baseline      # 跑一组
opskeeper-eval run --suite full --env staging       # 全量回归
opskeeper-eval inject --case k8s/pod-oom --env staging  # 仅注入不评分
opskeeper-eval judge --case pg/long-running-tx --response agent-response.json  # 外部 judge
opskeeper-eval leaderboard                          # 查看排行榜
opskeeper-eval leaderboard --since 30d              # 近 30 天
```

---

## 三、Golden Case 编写规范

### 3.1 目录结构

```
internal/harness/cases/<resource>/<case-name>/case.yaml
```

例：

```
internal/harness/cases/host/disk-full/case.yaml
internal/harness/cases/pg/long-running-tx/case.yaml
internal/harness/cases/redis/big-key/case.yaml
```

### 3.2 YAML 字段规范

```yaml
id: pg/long-running-tx                   # 必填，格式 <resource>/<case-name>
description: 模拟 PG 长事务阻塞          # 必填，一句话
severity: P0                             # 必填，P0/P1/P2/P3
tags: [pg, lock, performance]            # 可选，用于筛选

prerequisites:                           # 可选，前置条件列表
  - pg.test_db_seeded
  - pg.test_user_with_privilege

inject:                                  # 必填，注入步骤
  - type: pg.open_long_tx                # 注入器类型
    duration: 300s                       # 持续时间
    params:                              # 注入参数
      sql: "BEGIN; SELECT pg_sleep(60);"
      sessions: 10

expect:                                  # 必填，期望行为
  time_to_detect: 30                     # 秒，期望 Agent 多快发现
  time_to_remediate: 120                 # 秒，期望 Agent 多快修复
  root_cause_lines:                      # 期望根因工具
    - pg.pg_stat_activity
    - pg.pg_locks
    - pg.top_queries
  remediation_options:                   # 期望修复方案（多选一即可）
    - pg.kill_session
    - pg.cancel_query

rubric:                                  # 必填，评分维度
  rca_accuracy: 0.85                     # 根因准确度阈值
  time_to_remediate: 120                 # 修复时长阈值
  no_collateral_damage: true             # 是否要求零副作用

metadata:                                # 可选，元信息
  owner: "@opskeeper-oncall"
  created_at: "2026-07-13"
  references:
    - "https://wiki.opskeeper.io/runbook/pg-long-tx"
```

### 3.3 完整示例：PG 长事务 case

```yaml
id: pg/long-running-tx
description: 模拟 PG 长事务导致锁等待和性能下降
severity: P0
tags: [pg, lock, performance, long-tx]
prerequisites:
  - pg.test_db_with_orders_table
  - pg.test_user_with_kill_privilege

inject:
  - type: pg.open_long_tx
    duration: 600s
    params:
      sql: "BEGIN; LOCK TABLE orders IN ACCESS EXCLUSIVE MODE; SELECT pg_sleep(120); COMMIT;"
      tx_id: tx-12345

expect:
  time_to_detect: 30
  time_to_remediate: 120
  root_cause_lines:
    - pg.pg_stat_activity
    - pg.pg_locks
    - pg.top_queries
  remediation_options:
    - pg.kill_session
    - pg.cancel_query

rubric:
  rca_accuracy: 0.85
  time_to_remediate: 120
  no_collateral_damage: true

metadata:
  owner: "@opskeeper-oncall"
  created_at: "2026-07-13"
  references:
    - "https://wiki.postgresql.org/wiki/Lock_Monitoring"
```

### 3.4 JSON Schema 校验

`internal/harness/cases/schema.json` 是权威 schema。新增 case 自动校验：

```bash
opskeeper-eval validate --case pg/long-running-tx
# → Validation passed
```

校验失败的常见原因：
- 缺 `id` / `description` / `severity` / `inject` / `expect` / `rubric`
- `time_to_detect` > `time_to_remediate`（逻辑错误）
- `inject.type` 未在 fault-injector 注册表中
- `rubric.*` 超出合理范围（`rca_accuracy` 应在 [0, 1]）

---

## 四、故障注入器（fault-injector）

### 4.1 内置注入器

| 类型 | 适用资源 | 关键参数 |
|---|---|---|
| `host.fill_disk` | host | path, target_percent, duration |
| `host.cpu_spike` | host | cpu_percent, duration, processes |
| `pg.open_long_tx` | postgres | sql, duration |
| `pg.lock_table` | postgres | table, lock_mode, duration |
| `pg.create_bloat` | postgres | table, bloat_factor |
| `redis.big_key` | redis | key, size_mb |
| `redis.slow_cmd` | redis | cmd, sleep_ms |
| `mq.backlog` | rabbitmq / kafka | queue/topic, message_count |
| `k8s.pod_oom` | k8s | namespace, deployment, memory_limit |
| `k8s.node_notready` | k8s | node_name, duration |

### 4.2 环境限制

- **staging / dev**：默认允许
- **prod**：必须 `--confirm-prod` + 双人审批
- **注入时间窗**：默认 5 分钟（`--max-duration` 可调，但不超过 10 分钟）

### 4.3 自动清理

注入器在 `duration` 到期后自动回滚（kill session / release lock / delete key）。若回滚失败，强制告警并人工介入。

---

## 五、Judge 模型

### 5.1 默认配置：双模型取均值

| 模型 | 用途 | 备注 |
|---|---|---|
| Claude Sonnet 4 | 主评分 | 中文 / 代码 / 推理强 |
| GPT-4o | 副评分 | 通用推理 / 多语言 |

### 5.2 评分维度

每个 case 由 judge 在以下 5 维度独立打分（0-1）：

1. **rca_accuracy** — 根因工具是否用对
2. **time_to_detect** — 检测时长（vs rubric 阈值）
3. **time_to_remediate** — 修复时长（vs rubric 阈值）
4. **collateral_damage** — 副作用（kill 错 session 等）
5. **rubric_compliance** — 与 case 定义的一致性

### 5.3 一致性校验

两模型评分差异 > 0.2 时标记为 `Flagged`，进入人工 rubric 复评队列。一致率目标：

```
一致率（差异 < 0.1）>= 80%
```

详见 [ADR：judge 模型选型](superpowers/decisions/2026-07-13-harness-judge-models.md)。

### 5.4 缓存 + 增量

- **缓存**：相同 `(case_id, agent_response_hash)` 复用评分结果
- **增量**：PR 修改的 case 跑全量，其余 case 复用上次结果
- **限速**：每分钟最多 60 次 judge 调用

### 5.5 评分降级

若双模型一致率持续 < 70%（持续 4 周），降级到单模型（仅 Claude Sonnet 4）。详见 ADR "回滚条件"。

---

## 六、Leaderboard 与回归基线

### 6.1 Leaderboard 查看

```bash
opskeeper-eval leaderboard
```

输出示例：

```
Harness Leaderboard — 最近 30 天
┌──────────────────────────────┬─────────┬────────┬──────────┐
│ Case                         │ Score   │ Δ vs   │ Status   │
│                              │ (avg)   │ base   │          │
├──────────────────────────────┼─────────┼────────┼──────────┤
│ pg/long-running-tx           │ 0.92    │ +0.02  │ ✅ pass   │
│ redis/big-key                │ 0.88    │ -0.03  │ ⚠ warn  │
│ k8s/pod-oom                  │ 0.85    │ -0.08  │ ❌ fail   │
│ host/disk-full               │ 0.94    │ +0.01  │ ✅ pass   │
└──────────────────────────────┴─────────┴────────┴──────────┘
Overall: 0.90 (baseline 0.91, Δ -0.01)
```

### 6.2 回归基线规则

| 评分下降幅度 | 行为 |
|---|---|
| < 5% | 静默（log only） |
| 5%-15% | **告警**（Slack / 钉钉） |
| > 15% | **CI 阻断**（merge 拒绝） |

### 6.3 基线更新

```bash
# 把当前评分设为新基线（每月一次）
opskeeper-eval leaderboard --lock-baseline

# 查看历史基线
opskeeper-eval leaderboard --baselines
```

---

## 七、CI 集成

### 7.1 GitHub Actions 示例

```yaml
# .github/workflows/harness.yml
name: Harness Eval
on:
  pull_request:
    paths:
      - 'internal/manager/**'
      - 'internal/harness/**'
      - 'cmd/opskeeper-eval/**'

jobs:
  eval:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: '1.25'
      - name: Build opskeeper-eval
        run: go build -o opskeeper-eval ./cmd/opskeeper-eval
      - name: Run harness suite
        env:
          OPSKEEPER_LLM_ANTHROPIC_KEY: ${{ secrets.ANTHROPIC_API_KEY }}
          OPSKEEPER_LLM_OPENAI_KEY: ${{ secrets.OPENAI_API_KEY }}
        run: |
          ./opskeeper-eval run --suite pr-baseline --env staging --report eval-report.json
      - name: Check regression
        run: |
          ./opskeeper-eval leaderboard --check-regression --report eval-report.json
      - name: Upload report
        if: always()
        uses: actions/upload-artifact@v4
        with:
          name: harness-report
          path: eval-report.json
```

### 7.2 REST API（CI 集成）

```bash
# 异步触发评测
curl -X POST https://ops.example.com/api/v1/harness/runs \
  -H "Authorization: Bearer $JWT" \
  -H "Content-Type: application/json" \
  -d '{
    "suite": "middleware-baseline",
    "env": "staging",
    "webhook_url": "https://ci.example.com/callback"
  }'
# → {"run_id": "hr-abc123"}

# 查询进度
curl https://ops.example.com/api/v1/harness/runs/hr-abc123 \
  -H "Authorization: Bearer $JWT"
```

详见 [docs/api/harness.md](api/harness.md)。

---

## 八、生产保护

### 8.1 Prod 环境注入拦截

```bash
# 拒绝：prod 环境 + 未确认
$ opskeeper-eval inject --case k8s/pod-oom --env prod
ERROR: prod environment requires --confirm-prod and 2-person approval

# 通过：显式确认 + 审批人
$ opskeeper-eval inject --case k8s/pod-oom --env prod --confirm-prod \
    --approver "@alice" --approver "@bob"
✅ approved, injecting in 30s
```

### 8.2 时间窗限制

```bash
# 默认 5 分钟
$ opskeeper-eval inject --case pg/lock-table --env staging
duration=300s

# 调整上限（最大 600s = 10 分钟）
$ opskeeper-eval inject --case pg/lock-table --env staging --max-duration 600
duration=600s

# 超过限制被拒
$ opskeeper-eval inject --case pg/lock-table --env staging --max-duration 1200
ERROR: max-duration cannot exceed 600s
```

### 8.3 审计

所有 inject / run / judge 操作必审计：

```bash
logcli query '{app="opskeeper-eval"} |= "inject"' --since=24h
```

---

## 九、Case 库扩展

### 9.1 贡献流程

1. 在 `internal/harness/cases/<resource>/<new-case>/case.yaml` 写新 case
2. `opskeeper-eval validate --case <new-case>` 校验 schema
3. 在 staging 跑一次：`opskeeper-eval run --case <new-case> --env staging`
4. PR review + 合并
5. 纳入下月回归基线

### 9.2 案例库覆盖目标（v1.0）

| 资源 | 当前 | 目标 | 缺口 |
|---|---|---|---|
| PG | 6 | 20 | 14（真空闲事务 / 慢查询 / 复制延迟 / vacuum stuck / 索引膨胀 / autovacuum 失效 / etc.）|
| Redis | 4 | 12 | 8 |
| MQ | 4 | 10 | 6 |
| K8s | 4 | 12 | 8 |
| Host | 2 | 6 | 4 |
| **总计** | **20** | **60** | **40** |

每 case 估时 2-3 天（含开发 + review + 跑测）。

---

## 十、相关文档

- ADR：[docs/superpowers/decisions/2026-07-13-harness-judge-models.md](superpowers/decisions/2026-07-13-harness-judge-models.md)
- Spec：[openspec/specs/harness-eval-platform/spec.md](../openspec/specs/harness-eval-platform/spec.md)
- 集成指南：[docs/integration-guide.md](integration-guide.md)
- 运维手册：[docs/operations-manual.md](operations-manual.md)
- API 文档：[docs/api/harness.md](api/harness.md)
