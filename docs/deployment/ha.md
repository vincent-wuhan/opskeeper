# opskeeper HA 部署指南

> Platform-base-ha：多实例 manager 与外部依赖

## 架构总览

```
                    K8s Service: opskeeper-manager (ClusterIP / Ingress)
                                 │
             ┌───────────────────┼───────────────────┐
             │ HTTP              │ HTTP              │ HTTP
        ┌────▼────┐         ┌────▼────┐         ┌────▼────┐
        │ manager │         │ manager │         │ manager │
        │  pod-A  │         │  pod-B  │         │  pod-N  │
        │ (leader)│         │(follow) │         │  ...    │
        └────┬────┘         └─────────┘         └─────────┘
             │
             │  ┌──────────────────────────────────────────┐
             │  │          Process Internals              │
             │  │  HTTP Server (active-active)            │
             │  │  leader.Manager (Redis-based election)  │
             │  │  Leader-only workers:                   │
             │  │    scheduler:flow / scheduler:report    │
             │  │    harness:runner / upgrade:checker     │
             │  └──────────────────────────────────────────┘
             │
        ┌────▼────────────────┬──────────────────────┐
        │   External MySQL    │   External Redis     │
        │   (active-active)   │   (locks + election) │
        └─────────────────────┴──────────────────────┘
```

**核心原则**：
- **HTTP API active-active**：所有副本同时处理请求
- **Leader-only worker**：状态机 / 定时任务只在 leader 副本运行
- **Redis 分布式锁**：leader election 走 Redis，不走 K8s leases

## 部署要求

| 组件 | 要求 |
|---|---|
| K8s | ≥ 1.24（PodDisruptionBudget policy/v1） |
| 副本数 | ≥ 2（Helm 默认值） |
| 节点数 | ≥ 2（podAntiAffinity 跨节点分散） |
| Redis | 外部实例（HA 推荐 Sentinel / Cluster 模式） |
| MySQL | 外部实例（HA 推荐主从 / Galera / RDS） |

## 快速部署

```bash
# 1. 创建 Redis 密码 Secret
kubectl create secret generic opskeeper-redis-password \
  --from-literal=password=your-redis-password

# 2. 创建 DB 密码 Secret
kubectl create secret generic opskeeper-db-password \
  --from-literal=password=your-db-password

# 3. Helm 安装（HA 模式）
helm install opskeeper ./deploy/helm \
  --set database.host=mysql.internal \
  --set database.passwordSecret=opskeeper-db-password \
  --set redis.addr=redis.internal:6379 \
  --set redis.passwordSecret=opskeeper-redis-password

# 4. 验证
kubectl get pods -l app.kubernetes.io/component=manager
# 应看到 2 个 Running pod
```

## 验证 leader 选举

```bash
# 查询集群状态（需 admin token）
TOKEN=$(kubectl exec deploy/opskeeper-manager -- \
  printenv OPSKEEPER_JWT_SECRET)
# 或通过 UI 获取 admin JWT

curl -s -H "Authorization: Bearer <admin-jwt>" \
  http://opskeeper-manager:8080/api/v1/cluster/status | jq .

# 期望输出：
# {
#   "instance_id": "manager-pod-a-xxxx",
#   "role": "leader",
#   "leader_instance_id": "manager-pod-a-xxxx",
#   "workers": {
#     "scheduler:flow": {"running": true},
#     "scheduler:report": {"running": true},
#     "harness:runner": {"running": true},
#     "upgrade:checker": {"running": true}
#   }
# }
```

## 故障演练

### leader failover

```bash
# 1. 找到 leader pod
LEADER=$(curl -s -H "Authorization: Bearer $TOKEN" \
  http://opskeeper-manager:8080/api/v1/cluster/status | jq -r .instance_id)

# 2. 删除 leader pod
kubectl delete pod $LEADER

# 3. 验证 5s 内新 leader 接管
sleep 5
curl -s -H "Authorization: Bearer $TOKEN" \
  http://opskeeper-manager:8080/api/v1/cluster/status | jq '.role, .leader_instance_id'
```

### DB 不可达

```bash
# 当 DB 不可达时，/readyz 返回 503，K8s 摘除该副本
curl http://opskeeper-manager:8080/readyz
# {"ready":false,"checks":{"db":{"ok":false,...}}}
```

## 关键配置

| 参数 | 默认值 | 说明 |
|---|---|---|
| `manager.replicaCount` | `2` | 副本数（Breaking change: 1→2） |
| `manager.terminationGracePeriodSeconds` | `60` | SIGTERM 后最长存活时间 |
| `manager.podDisruptionBudget.minAvailable` | `1` | 至少 1 个可用 |
| `leader.enabled` | `true` | 开启 leader 选举 |
| `leader.ttl` | `15s` | leader 锁 TTL |
| `leader.renewInterval` | `5s` | renew 间隔 |

## 单副本降级

如不需要 HA（开发 / 测试环境）：

```bash
helm install opskeeper ./deploy/helm \
  --set manager.replicaCount=1 \
  --set leader.enabled=false
```
