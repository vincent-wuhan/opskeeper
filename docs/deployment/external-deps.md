# 外部依赖接入

> Platform-base-ha：外部 MySQL + Redis 配置指南

## 外部 MySQL

opskeeper manager 需要 MySQL 8.0+ 作为主数据库。HA 部署必须使用外部 MySQL（不能依赖 embedded sqlite 或 PVC）。

### 接入步骤

```bash
# 1. 在 MySQL 中创建数据库和用户
CREATE DATABASE opskeeper CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;
CREATE USER 'opskeeper'@'%' IDENTIFIED BY '<password>';
GRANT ALL PRIVILEGES ON opskeeper.* TO 'opskeeper'@'%';
FLUSH PRIVILEGES;

# 2. 创建 K8s Secret 存储密码
kubectl create secret generic opskeeper-db-password \
  --from-literal=password='<password>'

# 3. Helm 安装
helm install opskeeper ./deploy/helm \
  --set database.host=mysql.internal \
  --set database.port=3306 \
  --set database.user=opskeeper \
  --set database.db=opskeeper \
  --set database.passwordSecret=opskeeper-db-password \
  --set database.sslmode=require
```

### SSL/TLS

| sslmode | 行为 |
|---|---|
| `disable` | 不加密（仅开发环境） |
| `require` | 加密但不验证证书 |
| `verify-ca` | 加密 + 验证 CA |
| `verify-full` | 加密 + 验证 CA + hostname |

生产环境建议 `require` 或 `verify-ca`。

### 连接池调优

| 参数 | 默认 | 说明 |
|---|---|---|
| `database.pool.maxOpen` | 25 | 最大连接数 |
| `database.pool.maxIdle` | 5 | 最大空闲连接 |
| `database.pool.connMaxLifetime` | 30m | 连接最大生命周期 |

多副本部署建议：`maxOpen = (DB max_connections / replicaCount) - safety_margin`。

## 外部 Redis

opskeeper manager 需要 Redis 作为分布式锁和 leader 选举的协调后端。

### 接入步骤

```bash
# 1. 确保 Redis 实例可达（HA 推荐 Sentinel 或 Cluster 模式）
redis-cli -h redis.internal -a <password> ping
# 期望: PONG

# 2. 创建 K8s Secret
kubectl create secret generic opskeeper-redis-password \
  --from-literal=password='<password>'

# 3. Helm 安装
helm install opskeeper ./deploy/helm \
  --set redis.addr=redis.internal:6379 \
  --set redis.passwordSecret=opskeeper-redis-password
```

### 连接池调优

| 参数 | 默认 | 说明 |
|---|---|---|
| `redis.pool.maxActive` | 50 | 最大连接数 |
| `redis.pool.maxIdle` | 10 | 最大空闲连接 |
| `redis.pool.dialTimeout` | 5s | 连接超时 |

### Redis 可用性要求

- leader 选举的 TTL 为 15s，renew 间隔 5s
- Redis 短暂不可达（< 15s）不影响现有 leader
- Redis 长时间不可达（> 15s）会触发 leader 失锁 → failover
- 恢复后 leader 会自动重新获选

## 凭据管理最佳实践

1. **绝不**在 values.yaml 或 git 中明文存储密码
2. 使用 K8s Secret + `passwordSecret` 字段引用
3. 生产环境用 External Secrets Operator 或 Sealed Secrets 管理
4. 定期轮换密码（需同时更新 Secret + rolling restart manager）

## DSN 直连（兼容模式）

如不使用 Helm，可直接设置环境变量：

```bash
OPSKEEPER_DB_HOST=mysql.internal \
OPSKEEPER_DB_PORT=3306 \
OPSKEEPER_DB_USER=opskeeper \
OPSKEEPER_DB_PASSWORD=<password> \
OPSKEEPER_DB_NAME=opskeeper \
OPSKEEPER_DB_SSLMODE=require \
OPSKEEPER_REDIS_ADDR=redis.internal:6379 \
OPSKEEPER_REDIS_PASSWORD=<password> \
OPSKEEPER_LEADER_ENABLED=true \
./manager
```

或使用 DSN 直连（绕过离散字段）：

```bash
OPSKEEPER_DB_DSN="opskeeper:<password>@tcp(mysql.internal:3306)/opskeeper?parseTime=true&charset=utf8mb4&loc=Local&tls=require" \
./manager
```
