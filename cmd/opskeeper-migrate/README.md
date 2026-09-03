# opskeeper-migrate — ops-keeper → opskeeper 数据迁移 CLI

> **状态**：v1.0.0-dev（路径 A Task 3.3）
> **关联**：[docs/integration-guide.md §四](../../docs/integration-guide.md) / [docs/migration/opskeeper-user-transition.md](../../docs/migration/opskeeper-user-transition.md)

ops-keeper 用户迁移到 opskeeper 的**自服务工具**。支持 9 类实体的导入导出 + 幂等 + 回滚 + 限速 + 多租户隔离。

---

## 一、安装

### 二进制

```bash
# 从源码构建
go build -o opskeeper-migrate ./cmd/opskeeper-migrate

# 或下载预编译（v1.1 发布后）
curl -fsSL https://get.opskeeper.io/install.sh | bash -s -- --cli migrate
```

### 依赖

- Go 1.25（构建时）
- 网络可达 ops-keeper（导出阶段）+ opskeeper（导入阶段）
- 凭据：ops-keeper API token + opskeeper JWT

---

## 二、子命令速查

| 子命令 | 用途 | 关键标志 |
|---|---|---|
| `export` | 拉 ops-keeper 数据到 snapshot | `--source` `--output` |
| `import` | 从 snapshot 导入到 opskeeper | `--source` `--target` `--tenant-mapping` |
| `rollback` | 一键回滚 | `--rollback-snapshot` `--target` |
| `verify` | 源/目标对比 | `--source` `--target` `--tenant-mapping` |
| `list-entities` | 列出 9 类支持实体 | — |

全局帮助：`opskeeper-migrate --help`

---

## 三、典型工作流

### 3.1 完整迁移（4 步）

```bash
# 1. 导出 ops-keeper 快照
opskeeper-migrate export \
  --source "http://ops-keeper.internal:3000" \
  --token "$OPSKEEPER_TOKEN" \
  --output snapshot-$(date +%Y%m%d).json

# 2. dry-run 校验（推荐先跑）
opskeeper-migrate import \
  --source snapshot-20260713.json \
  --target "https://ops.example.com" \
  --token "$OPSKEEPER_JWT" \
  --tenant-mapping "ops-proj-id=42:opskeeper-tenant=42,ops-proj=100:opskeeper=100" \
  --dry-run

# 3. 实际导入
opskeeper-migrate import \
  --source snapshot-20260713.json \
  --target "https://ops.example.com" \
  --token "$OPSKEEPER_JWT" \
  --tenant-mapping "..." \
  --rate 1000

# 4. 验证
opskeeper-migrate verify \
  --source snapshot-20260713.json \
  --target "https://ops.example.com" \
  --tenant-mapping "..." \
  --report verify-20260713.html
```

### 3.2 出问题回滚

```bash
# 1. 切流量回 ops-keeper（用户操作：改 DNS / Nginx upstream）

# 2. 导入 rollback snapshot 到 ops-keeper
opskeeper-migrate rollback \
  --rollback-snapshot rollback-snapshot-2026-XX-XX.json \
  --target "http://ops-keeper.internal:3000"

# 3. 验证
# （用户操作：人工 smoke test ops-keeper）
```

### 3.3 单实体迁移

```bash
# 仅迁移 PG connections（不含其他）
opskeeper-migrate export \
  --source "..." \
  --output snapshot-pg.json \
  --entity pg_connections

opskeeper-migrate import \
  --source snapshot-pg.json \
  --target "..." \
  --tenant-mapping "..." \
  --entity pg_connections
```

---

## 四、支持的实体（9 类）

| Entity | ops-keeper → opskeeper | 凭据加密 |
|---|---|---|
| `users` | users | — |
| `projects` | tenants | — |
| `pg_connections` | middleware_resources (type=postgres) | ✓ |
| `redis_connections` | middleware_resources (type=redis) | ✓ |
| `mq_connections` | middleware_resources (type=rabbitmq/kafka) | ✓ |
| `k8s_clusters` | middleware_resources (type=k8s) | ✓ |
| `git_repos` | middleware_resources (type=git) | ✓ |
| `inspection_schedules` | schedules | — |
| `alert_rules` | alert_rules | — |

查看完整列表：`opskeeper-migrate list-entities`

---

## 五、多租户隔离（强制）

每个 `--tenant-mapping` 必须显式声明 ops-keeper project_id → opskeeper tenant_id 的映射：

```bash
# 格式：ops_id=tenant_id，多个用逗号分隔
--tenant-mapping "42=1,100=2,200=3"
```

**约束**：
- ❌ 空映射 = 直接拒绝（避免跨租户写入）
- ❌ 未声明的 ops-keeper project_id = 拒绝（记录 failed）
- ❌ 与 opskeeper tenant 白名单不一致 = 拒绝（防御性检查）

---

## 六、限速

```bash
--rate 1000  # 默认 1000 行/秒
```

通过内部令牌桶限流，避免压垮 ops-keeper / opskeeper。生产环境建议保持默认或更低（500-1000）。

---

## 七、幂等

每条记录带 `source_id`（来自 ops-keeper），opskeeper 通过 `by-source-id/{id}` 端点查询：

- 已存在 → 跳过（`Skipped` 计数）
- 不存在 → 创建（`Imported` 计数）
- 失败 → 记录失败原因（`Failed` 计数 + 详情）

**重复运行相同命令是安全的**：第二次运行全部 `Skipped`，不创建重复数据。

---

## 八、Snapshot 格式

```json
{
  "header": {
    "version": "v1",
    "exported_at": "2026-07-13T10:00:00Z",
    "source": "http://ops-keeper.internal:3000",
    "opskeeper_ver": "v0.0.1",
    "tenant_mapping": [...]
  },
  "entities": {
    "users": [{"id": 1, "email": "...", ...}],
    "projects": [...],
    "pg_connections": [...]
  }
}
```

- 支持 `.json` 与 `.json.gz`（自动识别）
- v1 格式稳定，向后兼容至少 6 个月

---

## 九、回滚 Snapshot 命名约定

```
rollback-snapshot-{YYYY-MM-DDTHH-MM-SS}.json
```

示例：`rollback-snapshot-2026-07-13T10-30-00.json`

`opskeeper-migrate rollback` 自动从目录扫描此模式的文件。

---

## 十、退出码

| 退出码 | 含义 |
|---|---|
| 0 | 成功 |
| 1 | 一般错误（参数 / 网络 / IO）|
| 2 | 子命令未知 |

CI 中可通过 `if ! opskeeper-migrate import ...; then ...; fi` 捕获。

---

## 十一、故障排查

| 错误 | 原因 | 解决 |
|---|---|---|
| `tenant_mapping 不能为空` | 未传 `--tenant-mapping` | 显式声明映射 |
| `ops-keeper project_id X 未声明` | 记录含未在映射中的 project_id | 更新映射或排除该实体 |
| `opskeeper 返回 401` | JWT 无效或过期 | 刷新 token |
| `opskeeper 返回 404` | 端点路径不匹配 | 检查 opskeeper 版本（v1.0+）|
| `429 Too Many Requests` | 限速过严 | 调低 `--rate` |
| `getlock timeout` | 多个 migrate 并发 | 串行执行 |

---

## 十二、相关

- 集成指南：[docs/integration-guide.md](../../docs/integration-guide.md)
- 用户过渡计划：[docs/migration/opskeeper-user-transition.md](../../docs/migration/opskeeper-user-transition.md)
- API 文档：[docs/api/middleware.md](../../docs/api/middleware.md)
- 迁移包：[internal/migrate/](../../internal/migrate/)
- e2e 集成测试：[internal/migrate/integration_test.go](../../internal/migrate/integration_test.go)
