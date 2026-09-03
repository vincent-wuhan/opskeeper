# K8s 部署指南：opskeeper v1.0 Helm chart

> **面向**：K8s 运维、平台 SRE、私有化部署工程师
> **Chart 版本**：1.0.0（appVersion: v1.0.0）
> **最低 K8s 版本**：1.24
> **关联**：
> - Helm chart：[deploy/helm/](../helm/)
> - 集成指南：[docs/integration-guide.md](../integration-guide.md)
> - 运维手册：[docs/operations-manual.md](../operations-manual.md)
> - 落地计划：[docs/superpowers/plans/2026-07-13-unified-platform-path-a.md](../superpowers/plans/2026-07-13-unified-platform-path-a.md) Task 3.2

---

## 一、Chart 概览

opskeeper Helm chart 是路径 A 落地后的统一部署入口，包含：

- **核心组件**：manager（主进程）/ edge-agent（DaemonSet）/ web（前端）
- **可选依赖**：qdrant（向量库）/ prometheus / loki / tempo / grafana（可观测性栈）
- **存储**：默认 PVC + 可选 external storage class
- **Ingress**：nginx 默认，支持 cert-manager 自动 TLS
- **RBAC**：内置 ServiceAccount + ClusterRole（最小权限）
- **HPA**：可选 HorizontalPodAutoscaler

### 1.1 资源拓扑

```
                    ┌────────────────┐
                    │   Ingress      │
                    │ (nginx + TLS)  │
                    └────────┬───────┘
                             │
              ┌──────────────┼──────────────┐
              │              │              │
        ┌─────▼─────┐  ┌─────▼─────┐  ┌─────▼─────┐
        │  Web SPA  │  │  Manager  │  │  Manager  │
        │  (nginx)  │  │  Pod 1    │  │  Pod 2    │
        │  1-3 pods │  │  500m/1Gi │  │  (HPA)    │
        └───────────┘  └─────┬─────┘  └───────────┘
                             │
              ┌──────────────┼──────────────┐
              │              │              │
        ┌─────▼─────┐  ┌─────▼─────┐  ┌─────▼─────┐
        │  Postgres │  │  Qdrant   │  │  Redis    │
        │  (10Gi)   │  │  (5Gi)    │  │  (1Gi)    │
        └───────────┘  └───────────┘  └───────────┘

        Edge Agent (DaemonSet — 每节点 1 个)
        ┌─────────┐  ┌─────────┐  ┌─────────┐
        │ Node 1  │  │ Node 2  │  │ Node 3  │
        │ edge    │  │ edge    │  │ edge    │
        └─────────┘  └─────────┘  └─────────┘
```

### 1.2 Chart 依赖

| 依赖 | 版本 | 用途 | 是否必须 |
|---|---|---|---|
| PostgreSQL | 12+ | 元数据 / 任务 / 告警 / 审计 | ✅ 必须（自带 or 外部） |
| Redis | 6+ | 缓存 / 队列 / 限流 | ✅ 必须（自带 or 外部） |
| Qdrant | 1.5+ | 向量库 / 知识库 RAG | ⏸ 可选（helm dep） |
| Prometheus | 25+ | 指标 | ⏸ 可选（helm dep） |
| Loki | 5+ | 日志 | ⏸ 可选（helm dep） |
| Tempo | 1.5+ | Trace | ⏸ 可选（helm dep） |
| Grafana | 7+ | 可视化 | ⏸ 可选（helm dep） |
| cert-manager | 1.10+ | 自动 TLS | ⏸ 推荐 |
| nginx-ingress | 1.8+ | Ingress Controller | ⏸ 推荐 |

---

## 二、快速安装

### 2.1 前置准备

```bash
# 1. K8s 集群（minikube / EKS / GKE / 自建均可，最低 3 节点 / 4 CPU / 8Gi）
kubectl version  # >= v1.24

# 2. Helm 3
helm version  # >= v3.10

# 3. （推荐）nginx-ingress + cert-manager
helm repo add ingress-nginx https://kubernetes.github.io/ingress-nginx
helm repo add jetstack https://charts.jetstack.io
helm repo update
helm install ingress-nginx ingress-nginx/ingress-nginx --namespace ingress-nginx --create-namespace
helm install cert-manager jetstack/cert-manager --namespace cert-manager --create-namespace --set installCRDs=true

# 4. 添加 opskeeper helm repo
helm repo add opskeeper https://charts.opskeeper.io
helm repo update
```

### 2.2 最小安装（仅核心组件 + 外部 PG/Redis）

```bash
# 1. 创建 namespace
kubectl create namespace opskeeper

# 2. 准备 values.yaml
cat > values-minimal.yaml <<'EOF'
global:
  imageRegistry: ghcr.io/opskeeper-removed

manager:
  enabled: true
  replicaCount: 1
  config:
    # 外部 PG / Redis（如使用自带则省略）
    postgres:
      host: postgres.example.com
      port: 5432
      database: opskeeper
      userSecretRef:
        name: opskeeper-pg-credentials
        key: username
      passwordSecretRef:
        name: opskeeper-pg-credentials
        key: password
    redis:
      addr: redis.example.com:6379
      passwordSecretRef:
        name: opskeeper-redis-credentials
        key: password
    llm:
      primary:
        provider: anthropic
        apiKeySecretRef:
          name: opskeeper-llm-credentials
          key: anthropic_key

edgeAgent:
  enabled: true  # DaemonSet

web:
  enabled: true
  replicaCount: 1

ingress:
  enabled: true
  className: nginx
  hosts:
    - host: ops.example.com
      paths:
        - path: /
          pathType: Prefix
  tls:
    - secretName: opskeeper-tls
      hosts:
        - ops.example.com

# 可观测性栈：先关掉（用外部）
qdrant:
  enabled: false
prometheus:
  enabled: false
loki:
  enabled: false
tempo:
  enabled: false
grafana:
  enabled: false
EOF

# 3. 创建 secrets
kubectl create secret generic -n opskeeper opskeeper-pg-credentials \
  --from-literal=username=opskeeper --from-literal=password='YOUR-PG-PASSWORD'
kubectl create secret generic -n opskeeper opskeeper-redis-credentials \
  --from-literal=password='YOUR-REDIS-PASSWORD'
kubectl create secret generic -n opskeeper opskeeper-llm-credentials \
  --from-literal=anthropic_key='sk-ant-xxx'

# 4. 安装
helm install opskeeper opskeeper/opskeeper \
  --namespace opskeeper \
  --values values-minimal.yaml
```

### 2.3 完整安装（含可观测性栈）

```bash
# 1. 使用同一份 values.yaml 但开启可观测性栈
cat > values-full.yaml <<'EOF'
global:
  imageRegistry: ghcr.io/opskeeper-removed

manager:
  enabled: true
  replicaCount: 2
  config:
    # 同上（PG / Redis / LLM）...

edgeAgent:
  enabled: true

web:
  enabled: true
  replicaCount: 2

ingress:
  enabled: true
  className: nginx
  hosts:
    - host: ops.example.com
      paths:
        - path: /
          pathType: Prefix
  tls:
    - secretName: opskeeper-tls
      hosts:
        - ops.example.com

# 启用可观测性栈
qdrant:
  enabled: true
  persistence:
    size: 10Gi
prometheus:
  enabled: true
  server:
    persistentVolume:
      enabled: true
      size: 50Gi
    retention: 30d
loki:
  enabled: true
  persistence:
    enabled: true
    size: 30Gi
tempo:
  enabled: true
  persistence:
    enabled: true
    size: 20Gi
grafana:
  enabled: true
  persistence:
    enabled: true
    size: 5Gi
  adminPassword: 'YOUR-GRAFANA-PASSWORD'
EOF

helm install opskeeper opskeeper/opskeeper \
  --namespace opskeeper \
  --values values-full.yaml
```

### 2.4 验证

```bash
# 检查 pods
kubectl get pods -n opskeeper
# NAME                              READY   STATUS    RESTARTS
# opskeeper-edge-xxxxx                 1/1     Running   0
# opskeeper-edge-yyyyy                 1/1     Running   0
# opskeeper-manager-aaaaa              1/1     Running   0
# opskeeper-manager-bbbbb              1/1     Running   0
# opskeeper-web-cccccc                 1/1     Running   0
# qdrant-0                          1/1     Running   0
# prometheus-server-0               2/2     Running   0
# loki-0                            1/1     Running   0
# tempo-0                           1/1     Running   0
# grafana-xxxxxxxxxx-yyy            1/1     Running   0

# 健康检查
kubectl exec -n opskeeper deploy/opskeeper-manager -- wget -qO- http://localhost:8080/healthz
# {"status":"ok"}

# 端口转发访问 Web（开发用）
kubectl port-forward -n opskeeper svc/opskeeper-web 8080:80
# 访问 http://localhost:8080

# 或通过 ingress
curl -k https://ops.example.com/healthz
```

---

## 三、生产配置

### 3.1 高可用（HA）

```yaml
# values-ha.yaml
manager:
  replicaCount: 3
  autoscaling:
    enabled: true
    minReplicas: 3
    maxReplicas: 10
    targetCPUUtilizationPercentage: 70
  config:
    # 必须使用外部 PG + Redis（多副本不能共享本地存储）
    postgres:
      host: postgres-rw.example.com
      port: 5432
      # ... 同上
    redis:
      addr: redis-cluster.example.com:6379
      # ... 同上

web:
  replicaCount: 3
  autoscaling:
    enabled: true
    minReplicas: 3
    maxReplicas: 10

# 启用 PodDisruptionBudget
podDisruptionBudget:
  enabled: true
  minAvailable: 2

# Pod topology spread constraints
topologySpreadConstraints:
  - maxSkew: 1
    topologyKey: kubernetes.io/hostname
    whenUnsatisfiable: ScheduleAnyway
    labelSelector:
      matchLabels:
        app.kubernetes.io/name: opskeeper
        app.kubernetes.io/component: manager
```

### 3.2 持久化存储

```yaml
manager:
  persistence:
    enabled: true
    size: 100Gi
    storageClass: gp3  # AWS / 可替换
    accessModes:
      - ReadWriteOnce

qdrant:
  persistence:
    storageClass: gp3
    size: 50Gi

prometheus:
  server:
    persistentVolume:
      storageClass: gp3
      size: 200Gi
```

### 3.3 资源限制

```yaml
manager:
  resources:
    requests:
      cpu: 1000m    # 提请求
      memory: 2Gi
    limits:
      cpu: 4000m
      memory: 8Gi

edgeAgent:
  resources:
    requests:
      cpu: 100m
      memory: 256Mi
    limits:
      cpu: 500m
      memory: 512Mi

web:
  resources:
    requests:
      cpu: 200m
      memory: 256Mi
    limits:
      cpu: 1000m
      memory: 512Mi
```

### 3.4 Ingress + TLS

```yaml
ingress:
  enabled: true
  className: nginx
  annotations:
    cert-manager.io/cluster-issuer: letsencrypt-prod
    nginx.ingress.kubernetes.io/proxy-body-size: "50m"  # 大文件上传
    nginx.ingress.kubernetes.io/proxy-read-timeout: "300"
  hosts:
    - host: ops.example.com
      paths:
        - path: /
          pathType: Prefix
  tls:
    - secretName: opskeeper-tls
      hosts:
        - ops.example.com
```

### 3.5 NetworkPolicy（可选加固）

```yaml
networkPolicy:
  enabled: true
  ingress:
    # 仅允许从 ingress controller 访问
    - from:
        - namespaceSelector:
            matchLabels:
              name: ingress-nginx
      ports:
        - protocol: TCP
          port: 8080
  egress:
    # 允许访问 PG / Redis / 外部 LLM API
    - to:
        - namespaceSelector: {}
      ports:
        - protocol: TCP
          port: 5432
        - protocol: TCP
          port: 6379
    # 允许出站 HTTPS（LLM / git-artifact webhook）
    - to:
        - ipBlock:
            cidr: 0.0.0.0/0
            except:
              - 10.0.0.0/8
              - 172.16.0.0/12
              - 192.168.0.0/16
      ports:
        - protocol: TCP
          port: 443
```

### 3.6 ServiceMonitor（Prometheus 自动发现）

```yaml
serviceMonitor:
  enabled: true
  interval: 30s
  scrapeTimeout: 10s
  labels:
    release: prometheus  # match Prometheus selector
```

---

## 四、多租户配置

opskeeper v1.0 强制多租户隔离。配置方式：

```yaml
manager:
  config:
    multiTenancy:
      enabled: true
      isolationLevel: strict  # strict / shared
      defaultTenant: 1  # admin 租户
```

- `strict` 模式：每个 tenant 独立数据库 schema
- `shared` 模式：单数据库，按 `tenant_id` SQL 过滤（默认）

详见 [docs/operations-manual.md §六 安全基线](../operations-manual.md)。

---

## 五、配置 LLM Provider

### 5.1 多 provider 配置

```yaml
manager:
  config:
    llm:
      primary:
        provider: anthropic
        model: claude-sonnet-4-20250514
        apiKeySecretRef:
          name: opskeeper-llm
          key: anthropic_key
      secondary:
        provider: openai
        model: gpt-4o
        apiKeySecretRef:
          name: opskeeper-llm
          key: openai_key
      tertiary:
        provider: deepseek
        model: deepseek-chat
        apiKeySecretRef:
          name: opskeeper-llm
          key: deepseek_key
        baseURL: https://api.deepseek.com/v1
      routing:
        # 简单路由：primary 失败 → secondary → tertiary
        strategy: fallback
        # 智能路由：按成本 + 性能
        # strategy: cost-optimized
```

### 5.2 Secret 准备

```bash
kubectl create secret generic -n opskeeper opskeeper-llm \
  --from-literal=anthropic_key='sk-ant-xxx' \
  --from-literal=openai_key='sk-xxx' \
  --from-literal=deepseek_key='sk-xxx'
```

---

## 六、CI/CD 集成

### 6.1 GitOps（ArgoCD）

```yaml
# argocd-app.yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: opskeeper
  namespace: argocd
spec:
  project: default
  source:
    repoURL: https://charts.opskeeper.io
    chart: opskeeper
    targetRevision: 1.0.0
    helm:
      valueFiles:
        - values-prod.yaml
  destination:
    server: https://kubernetes.default.svc
    namespace: opskeeper
  syncPolicy:
    automated:
      prune: true
      selfHeal: true
    syncOptions:
      - CreateNamespace=true
```

### 6.2 Helmfile（多环境）

```yaml
# helmfile.yaml
environments:
  default:
    values:
      - environment: dev
  prod:
    values:
      - environment: prod

releases:
  - name: opskeeper
    namespace: opskeeper
    chart: opskeeper/opskeeper
    version: 1.0.0
    values:
      - values.yaml
      - values-{{ .Environment.Name }}.yaml
```

---

## 七、升级与回滚

### 7.1 升级

```bash
# 1. 备份数据库
kubectl exec -n opskeeper deploy/opskeeper-manager -- \
  opskeeper-manager --export > backup-v$(cat VERSION).json

# 2. Helm 升级
helm upgrade opskeeper opskeeper/opskeeper \
  --namespace opskeeper \
  --values values.yaml \
  --set image.tag=v1.0.0 \
  --wait

# 3. 运行 migration
kubectl exec -n opskeeper deploy/opskeeper-manager -- \
  opskeeper-manager --migrate up

# 4. 验证
helm test opskeeper -n opskeeper
```

### 7.2 回滚

```bash
# 1. 查看历史
helm history opskeeper -n opskeeper
# 1. 2026-07-13  deployed  opskeeper-1.0.0
# 2. 2026-07-13  superseded opskeeper-1.0.1

# 2. 回滚到指定版本
helm rollback opskeeper 1 -n opskeeper

# 3. 数据库回滚（如 migration 已执行）
kubectl exec -n opskeeper deploy/opskeeper-manager -- \
  opskeeper-manager --migrate down --to backup-v1.0.0.json
```

---

## 八、测试集群验证（minikube）

### 8.1 minikube 启动

```bash
# 启动 minikube（≥ 8Gi 内存）
minikube start --memory=8192 --cpus=4 --driver=docker

# 启用 ingress addon
minikube addons enable ingress
minikube addons enable metrics-server

# 安装 cert-manager（可选）
kubectl apply -f https://github.com/cert-manager/cert-manager/releases/download/v1.13.0/cert-manager.yaml

# 添加 hosts 记录
echo "$(minikube ip) ops.minikube.local" | sudo tee -a /etc/hosts
```

### 8.2 安装 + 验证

```bash
# 安装（开发模式，无 TLS）
helm install opskeeper ./deploy/helm \
  --namespace opskeeper --create-namespace \
  --set ingress.tls=null \
  --set ingress.hosts[0].host=ops.minikube.local

# 验证
kubectl get pods -n opskeeper
helm test opskeeper -n opskeeper

# 访问
open https://ops.minikube.local   # macOS
# 或
xdg-open https://ops.minikube.local  # Linux
```

### 8.3 端到端 smoke test

```bash
# 1. 健康检查
curl -k https://ops.minikube.local/healthz
# {"status":"ok"}

# 2. Web UI 加载
curl -k -I https://ops.minikube.local/
# HTTP/2 200

# 3. Prometheus 指标
kubectl port-forward -n opskeeper svc/prometheus-server 9090:80
open http://localhost:9090/graph?g0.expr=up

# 4. Grafana 仪表盘
kubectl port-forward -n opskeeper svc/grafana 3000:80
# 登录 admin / YOUR-GRAFANA-PASSWORD
open http://localhost:3000
```

---

## 九、故障排查

### 9.1 Pod 启动失败

```bash
# 查看 pod 详情
kubectl describe pod -n opskeeper <pod-name>

# 查看日志
kubectl logs -n opskeeper <pod-name> --previous

# 常见错误：
# - ImagePullBackOff：检查 imagePullSecrets / 网络代理
# - CrashLoopBackOff：检查 config / 环境变量 / 数据库连接
# - Pending：检查 PVC / nodeSelector / 资源配额
```

### 9.2 数据库连接失败

```bash
# 测试 secret 内容
kubectl get secret -n opskeeper opskeeper-pg-credentials -o jsonpath='{.data.password}' | base64 -d

# 测试从 pod 内连接
kubectl exec -n opskeeper deploy/opskeeper-manager -- \
  psql -h postgres.example.com -U opskeeper -d opskeeper -c "SELECT 1"
```

### 9.3 Ingress 502

```bash
# 检查 ingress controller 日志
kubectl logs -n ingress-nginx -l app.kubernetes.io/name=ingress-nginx --tail=100

# 检查 backend service
kubectl get svc -n opskeeper
kubectl get endpoints -n opskeeper opskeeper-web
```

### 9.4 性能问题

```bash
# 检查 HPA 状态
kubectl get hpa -n opskeeper

# 触发手动扩容
kubectl scale -n opskeeper deploy/opskeeper-manager --replicas=5

# 查看 Prometheus 关键指标
kubectl port-forward -n opskeeper svc/prometheus-server 9090:80
# 查询 opskeeper_investigator_duration_seconds histogram
# 查询 opskeeper_manager_cpu_usage gauge
```

详见 [docs/operations-manual.md §五 应急响应](../operations-manual.md)。

---

## 十、相关文档

- 集成指南：[docs/integration-guide.md](../integration-guide.md)
- 运维手册：[docs/operations-manual.md](../operations-manual.md)
- Harness 评测指南：[docs/harness-guide.md](../harness-guide.md)
- API 文档：[docs/api/](../api/)
- Helm chart README：[deploy/helm/README.md](../../deploy/helm/README.md)
- Release notes v1.0：[docs/releases/v1.0.0.md](../releases/v1.0.0.md)
- 落地计划：[docs/superpowers/plans/2026-07-13-unified-platform-path-a.md](../superpowers/plans/2026-07-13-unified-platform-path-a.md) Task 3.2

---

## 附录：完整 values.yaml 模板

```yaml
global:
  imageRegistry: ghcr.io/opskeeper-removed
  imagePullSecrets: []
  storageClass: ""

manager:
  enabled: true
  replicaCount: 1
  autoscaling:
    enabled: false
    minReplicas: 1
    maxReplicas: 10
    targetCPUUtilizationPercentage: 70
  image:
    repository: opskeeper/manager
    tag: ""  # 默认 appVersion
    pullPolicy: IfNotPresent
  resources:
    requests: { cpu: 500m, memory: 1Gi }
    limits: { cpu: 2000m, memory: 4Gi }
  service:
    type: ClusterIP
    port: 8080
  persistence:
    enabled: true
    size: 20Gi
    storageClass: ""
  config:
    logLevel: info
    logFormat: json
    enableHarness: true
    enableMiddleware: true
    enableGitArtifact: true
  podDisruptionBudget:
    enabled: false
  nodeSelector: {}
  tolerations: []
  affinity: {}

edgeAgent:
  enabled: true
  resources:
    requests: { cpu: 100m, memory: 256Mi }
    limits: { cpu: 500m, memory: 512Mi }
  # ... 同上结构

web:
  enabled: true
  replicaCount: 1
  image:
    repository: opskeeper/web
    tag: ""
  resources:
    requests: { cpu: 100m, memory: 128Mi }
    limits: { cpu: 500m, memory: 512Mi }
  # ...

ingress:
  enabled: true
  className: nginx
  annotations: {}
  hosts: []
  tls: []

serviceAccount:
  create: true
  name: ""

networkPolicy:
  enabled: false

serviceMonitor:
  enabled: false

# 可观测性栈（按需启用）
qdrant: { enabled: false }
prometheus: { enabled: false }
loki: { enabled: false }
tempo: { enabled: false }
grafana: { enabled: false }
```

