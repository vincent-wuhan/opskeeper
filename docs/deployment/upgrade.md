# 升级策略

> Platform-base-ha：零停机滚动升级 / 蓝绿 / 金丝雀

## 滚动升级（推荐）

opskeeper HA 部署默认支持零停机滚动升级。K8s 的 `maxUnavailable=0` + `maxSurge=1` 配合 `/readyz` probe 确保升级期间始终有可用副本。

### 操作

```bash
helm upgrade opskeeper ./deploy/helm \
  --set manager.image.tag=v1.1.0
```

### 升级序列（每个副本）

1. K8s 启动新副本，等待 `/readyz` = 200
2. 新副本完成 leader 注册（等 leader 锁可用）
3. K8s 发送 SIGTERM 到旧副本
4. 旧副本 MarkDraining → `/readyz` = 503
5. K8s 从 Service endpoints 摘除旧副本
6. 旧副本 HTTP drain（30s）→ ResignAll（25s）→ 关闭 DB/Redis
7. 旧副本退出

### 验证零停机

```bash
# 升级期间持续 curl（应全程 200）
while true; do
  curl -s -o /dev/null -w "%{http_code}\n" http://opskeeper-manager:8080/healthz
  sleep 1
done
```

### K8s Deployment 策略

```yaml
strategy:
  type: RollingUpdate
  rollingUpdate:
    maxUnavailable: 0    # 不允许同时减少可用副本
    maxSurge: 1          # 允许临时多 1 个副本
```

## 蓝绿部署

```bash
# 1. 部署新版本（green）
helm install opskeeper-green ./deploy/helm \
  --set manager.image.tag=v2.0.0

# 2. 验证 green 副本健康
kubectl get pods -l app.kubernetes.io/instance=opskeeper-green

# 3. 切换 Service selector
kubectl patch svc opskeeper-manager -p \
  '{"spec":{"selector":{"app.kubernetes.io/instance":"opskeeper-green"}}}'

# 4. 观察流量切换，确认无误后删除 blue
helm uninstall opskeeper-blue
```

## 金丝雀发布

```bash
# 1. 主版本保持不变
helm install opskeeper ./deploy/helm --set manager.image.tag=v1.0.0

# 2. 金丝雀副本（10% 流量）
helm install opskeeper-canary ./deploy/helm \
  --set manager.image.tag=v2.0.0 \
  --set manager.replicaCount=1

# 3. 通过 Ingress 权重或 Service Mesh 路由 10% 流量到 canary
# 4. 观察 30 分钟后决定全量或回滚
```

## 回滚

```bash
# Helm 回滚到上一版本
helm rollback opskeeper 0

# 或指定版本
helm rollback opskeeper <REVISION_NUMBER>

# 查看历史版本
helm history opskeeper
```

## 升级前检查清单

- [ ] `go test -race ./...` 全通过
- [ ] `helm lint ./deploy/helm` 通过
- [ ] DB migration 已在 staging 验证（expand-contract）
- [ ] Redis 兼容性确认（版本 + maxmemory policy）
- [ ] terminationGracePeriodSeconds ≥ 60s
- [ ] PDB minAvailable ≥ 1
- [ ] 至少 2 个节点满足 podAntiAffinity
