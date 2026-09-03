# opskeeper-migrate-runtime — 数据库 schema 版本化迁移 CLI

> **状态**：v1.0.0-dev（路径 A Task 3.4）
> **关联包**：[internal/migrator](../../internal/migrator/)
> **替代方案**：GORM `AutoMigrate`（隐式 / 无版本控制 / 无回滚）

opskeeper 数据库 schema 的**生产级版本化迁移**工具。提供显式版本号 + Up/Down 对称 + 幂等 + 回滚 + 锁 + 干跑。

---

## 一、为什么需要版本化迁移

| 维度 | GORM AutoMigrate | migrator 运行时 |
|---|---|---|
| 版本号 | ❌ 无 | ✅ 时间戳（YYYYMMDDHHMMSS）|
| 幂等 | ⚠️ schema 幂等，history 无 | ✅ schema_migrations 表记录 |
| 回滚 | ❌ 不支持 | ✅ Down() + 历史保留 |
| 锁 | ❌ 无 | ✅ MySQL GET_LOCK |
| 干跑 | ❌ 无 | ✅ DryRun 跳过所有写入 |
| 步数限制 | ❌ 全量 | ✅ Steps=N 限制 |
| 审计 | ❌ 无 | ✅ schema_migrations 表 |
| 失败恢复 | ❌ 启动失败 | ✅ 单步失败回滚后续 |

**何时仍用 AutoMigrate**：开发期 / 测试环境 / 简单项目。**生产**：版本化迁移。

---

## 二、子命令速查

| 子命令 | 用途 | 关键标志 |
|---|---|---|
| `up` | 应用所有 pending 迁移 | `--dsn` `--dry-run` |
| `down N` | 回滚 N 步（默认 1）| `--dsn` `--steps` `--dry-run` |
| `status` | 显示 applied / pending 状态 | `--dsn` |
| `create` | 创建新迁移骨架 | `--name` |

全局帮助：`opskeeper-migrate-runtime --help`

---

## 三、典型工作流

### 3.1 首次部署（应用所有 pending）

```bash
opskeeper-migrate-runtime up \
  --dsn "postgres://${DB_USER}:${DB_PASSWORD}@localhost:5432/opskeeper?sslmode=disable"

# 输出：
# ✅ up 完成
#    应用: 12
#      20260101000001
#      20260115000002
#      ...
```

### 3.2 干跑（仅报告）

```bash
opskeeper-migrate-runtime up \
  --dsn "postgres://..." \
  --dry-run

# 输出：
# 🧪 [DRY-RUN] up 完成
#    应用: 3
#      20260701000001
#      ...
```

### 3.3 查看状态

```bash
opskeeper-migrate-runtime status \
  --dsn "postgres://..."

# 输出：
# === Migration Status ===
# Total registered: 12
# Applied: 10
# Pending: 2
#
# Applied:
#   20260101000001 | applied | 2026-01-15T10:00:00Z | 230ms
#   ...
#
# Pending:
#   20260701000001 | add_harness_scores_table
#   20260713000001 | add_git_artifacts_indexes
```

### 3.4 回滚 1 步

```bash
opskeeper-migrate-runtime down \
  --dsn "postgres://..." \
  --steps 1

# 注意：会执行对应迁移的 Down() 方法
# 若 Down() 返回 ErrIrreversible，拒绝回滚
```

### 3.5 创建新迁移

```bash
opskeeper-migrate-runtime create \
  --name add_harness_scores_table

# 输出：
# ✅ 创建迁移: migrations/20260713150000_add_harness_scores_table.go
#    Version: 20260713150000
#    接下来：编辑 migrations/... 实现 Up/Down，
#    然后通过 import _ "<your-pkg>" 注册到 opskeeper-migrate-runtime
```

骨架文件内容：

```go
package migrations

import (
    "context"
    "github.com/vincent-wuhan/opskeeper/internal/migrator"
    "gorm.io/gorm"
)

type Migration_add_harness_scores_table struct{}

func (m *Migration_add_harness_scores_table) Version() string {
    return "20260713150000"
}

func (m *Migration_add_harness_scores_table) Description() string {
    return "add_harness_scores_table"
}

func (m *Migration_add_harness_scores_table) Up(ctx context.Context, db *gorm.DB) error {
    // TODO: 实现 Up 逻辑
    return db.WithContext(ctx).AutoMigrate(&HarnessScore{})
}

func (m *Migration_add_harness_scores_table) Down(ctx context.Context, db *gorm.DB) error {
    return db.WithContext(ctx).Migrator().DropTable(&HarnessScore{})
}

func init() {
    migrator.MustRegister(&Migration_add_harness_scores_table{})
}
```

---

## 四、数据库驱动

驱动自动从 `--dsn` 前缀推断：

| DSN 前缀 | 驱动 |
|---|---|
| `postgres://...` / `postgresql://...` / 含 `host=` | PostgreSQL |
| `mysql://...` | MySQL |
| `.db` / `.sqlite` / `.sqlite3` 扩展 | SQLite |

显式指定：`--driver postgres|mysql|sqlite`

---

## 五、注册迁移

### 5.1 业务包内注册（推荐）

```go
// internal/harness/store/migrations.go
package store

import (
    "github.com/vincent-wuhan/opskeeper/internal/migrator"
)

func init() {
    migrator.MustRegister(&harnessMigrations...)
}
```

### 5.2 CLI 入口导入

```go
// cmd/opskeeper-migrate-runtime/main.go
import (
    _ "github.com/vincent-wuhan/opskeeper/internal/harness/store"  // 触发 init
)
```

### 5.3 命名约定

- 文件：`migrations/<version>_<name>.go`
- Version：时间戳（YYYYMMDDHHMMSS，14 位）
- 包名：`migrations`

---

## 六、并发安全

- 执行期间持 MySQL `GET_LOCK(100420260713, 10)` 锁
- 10 秒超时；超时返回错误而非阻塞
- 多副本部署：只有第一个持锁者执行，其他等待 → 锁释放后看到 history 已更新，幂等跳过

**PG 适配**：v1 仅支持 MySQL GET_LOCK；v2 计划支持 PG advisory lock。

---

## 七、schema_migrations 表

每次运行自动创建（v1 用 AutoMigrate，v2 计划改固定 SQL）：

```sql
CREATE TABLE schema_migrations (
    version      VARCHAR(32) PRIMARY KEY,
    description  VARCHAR(255),
    applied_at   DATETIME,
    duration_ms  BIGINT,
    status       VARCHAR(16)  -- applied / rolled_back / failed
);
```

- 同一 version 多次应用：基于 PRIMARY KEY 去重
- rollback 不删除记录：标 `status=rolled_back`（保留审计）
- 失败迁移：标 `status=failed`（保留现场）

---

## 八、退出码

| 退出码 | 含义 |
|---|---|
| 0 | 成功 |
| 1 | 一般错误（DB 连接 / 迁移失败）|
| 2 | 子命令未知 |

---

## 九、CI/CD 集成

### 9.1 部署前 hook

```yaml
# .github/workflows/deploy.yml
- name: Apply DB migrations
  run: |
    ./opskeeper-migrate-runtime up \
      --dsn "${{ secrets.PROD_DB_DSN }}" \
      --steps 1   # 仅应用最新一步
```

### 9.2 Dry-run 验证

```yaml
- name: Validate migrations (dry-run)
  run: |
    ./opskeeper-migrate-runtime up \
      --dsn "$TEST_DB_DSN" \
      --dry-run
```

### 9.3 PR 检查（推荐）

```yaml
- name: Check migration status
  run: |
    ./opskeeper-migrate-runtime status \
      --dsn "$TEST_DB_DSN"
    # 若 Pending > 0，警告
```

---

## 十、相关

- migrator 包：[internal/migrator/](../../internal/migrator/)
- 单元测试：[internal/migrator/migrator_test.go](../../internal/migrator/migrator_test.go)
- 集成指南：[docs/integration-guide.md](../../docs/integration-guide.md)
- 数据迁移 CLI：[cmd/opskeeper-migrate/README.md](../opskeeper-migrate/README.md)
