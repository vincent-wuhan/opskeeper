# Docker 一键部署 AgentTeams Plugin 链路

## 一、目的

让运维在**单台 Linux/Mac 机器**上一键拉起:

- opskeeper Manager (opskeeper 二进制) — 提供 `/v1/plugins/*` REST API
- 2 个 qwenpaw worker (mock FastAPI runtime) — 注册 opskeeper-teamharness 4 个 HTTP 端点
- (可选) AgentTeams Controller — 切换 `controller-discovery` mode 自动发现 worker

无需 Controller、无需 K8s、无需真 qwenpaw,完整跑通 Dashboard upload → push → worker install 链路。

## 二、快速开始

### 2.1 启动栈

```bash
cd opskeeper-v2

# 启动 opskeeper-manager + 2 个 worker
docker compose -f deploy/docker-compose.agentteams.yml up -d --build

# 等待 healthcheck 通过(约 30s)
docker compose -f deploy/docker-compose.agentteams.yml ps
# 期望 opskeeper-manager / worker-opskeeper-alerter / worker-opskeeper-investigator 全是 healthy
```

### 2.2 端到端验证

```bash
# 1. opskeeper Manager liveness
curl -s http://localhost:8080/healthz
# 期望 200 OK

# 2. 列出已注册 plugin(空)
curl -s http://localhost:8080/v1/plugins
# 期望 {"plugins": []}

# 3. Worker 端 install-plugin/health
curl -s http://localhost:8088/api/opskeeper-teamharness/install-plugin/health
# 期望 {"ok": true, "qwenpaw": "/usr/local/bin/qwenpaw", "maxBytes": 33554432}
```

### 2.3 完整 install + push 链路

```bash
# Step 1: Build worker plugin zip (与 AgentTeams 兼容的 v1alpha1 包)
cd plugins/opskeeper-teamharness
zip -r dist/opskeeper-teamharness.zip plugin.yaml prompts skills mcp adapters scripts loongsuite examples dashboard >/dev/null

# Step 2: 上传到 opskeeper Manager
curl -s -X POST http://localhost:8080/v1/plugins/install \
  -H "Authorization: Bearer ${HIGRESS_ADMIN_PASSWORD:-change-me}" \
  -F "file=@dist/opskeeper-teamharness.zip"

# 期望:
# {"id":"opskeeper-teamharness","version":"1.0.1","description":"...", ...}

# Step 3: Push 到 worker
curl -s -X POST http://localhost:8080/v1/plugins/opskeeper-teamharness/push \
  -H "Authorization: Bearer ${HIGRESS_ADMIN_PASSWORD:-change-me}"

# 期望:
# {"plugin_id":"opskeeper-teamharness","pushed":true,"bytes":75984}

# Step 4: 验证 status
curl -s http://localhost:8080/v1/plugins/opskeeper-teamharness \
  -H "Authorization: Bearer ${HIGRESS_ADMIN_PASSWORD:-change-me}" | jq .status
# 期望 "enabled"

# Step 5: 查看 worker 端日志确认 qwenpaw plugin install 触发
docker compose -f deploy/docker-compose.agentteams.yml logs worker-opskeeper-alerter
# 期望看到 "[install-plugin] exitCode=0 ..." 或 mock qwenpaw 收到 zip
```

## 三、配置矩阵

| 模式 | env vars | 何时使用 |
|---|---|---|
| `stub` (默认) | 无 | 单机模式 / 评审演示 / 本地 dev |
| `worker-http` (Docker 推荐) | `OPSKEEPER_PLUGIN_SYNC_MODE=worker-http` + `OPSKEEPER_PLUGIN_SYNC_ENDPOINTS_FILE=/etc/opskeeper/plugin-sync-endpoints.json` | Docker compose / 静态 endpoint 列表 |
| `controller-discovery` (K8s 推荐) | `OPSKEEPER_PLUGIN_SYNC_MODE=controller-discovery` + `AGENTTEAMS_CONTROLLER_URL=http://agentteams-controller:8080` | K8s + AgentTeams Controller 自动发现 |

### 3.1 切换到 controller-discovery 模式

编辑 `deploy/docker-compose.agentteams.yml` 里 opskeeper-manager service 的 environment:

```yaml
OPSKEEPER_PLUGIN_SYNC_MODE: controller-discovery
AGENTTEAMS_CONTROLLER_URL: http://agentteams-controller:8080
AGENTTEAMS_CONTROLLER_BEARER: <your-controller-bearer>
```

然后取消注释 `agentteams-controller` service 并:

```bash
docker compose -f deploy/docker-compose.agentteams.yml up -d --build
```

opskeeper-manager 会调 `GET ${AGENTTEAMS_CONTROLLER_URL}/api/v1/workers` 自动拉 worker 列表,
5 分钟缓存 + stale-while-revalidate。

## 四、目录结构

```
deploy/
├── docker-compose.agentteams.yml           # 本文件演示栈
├── Dockerfile.opskeeper-worker            # worker 镜像构建脚本
├── worker-entrypoint.py                   # mock qwenpaw 启动器
└── examples/
    ├── plugin-sync-endpoints.json         # worker-http mode 的 endpoint 配置
    └── README-docker.md                    # 本文档

plugins/opskeeper-teamharness/             # worker 端 v1alpha1 plugin
├── plugin.yaml                             # AgentTeams 兼容清单
├── adapters/qwenpaw/
│   ├── plugin.py                           # register(api) + 4 个 HTTP 端点
│   └── test_install_endpoint.py            # 15 个 pytest
└── ...

plugins/agentteams-plugin-installer/        # Dashboard UI 插件
├── dashboard/src/extensions/
│   ├── api.js                              # 7 个 REST client
│   └── route.jsx                           # 完整管理 UI + Push 按钮
└── scripts/
    └── demo-install-opskeeper-teamharness.sh  # 端到端 demo 脚本
```

## 五、安全 / 运维注意

- **Higress GatewayKey**:生产部署必须替换 `${HIGRESS_ADMIN_PASSWORD:-change-me}` 为真实密钥,
  并配置 Higress AI Gateway 路径规则 `/api/opskeeper-teamharness/**` 网关鉴权
- **持久化**:plugin registry 在 `opskeeper_plugins` volume,容器删除后仍保留
  - 卸载整个栈:`docker compose down -v` (会清空 volume)
  - 仅重启:`docker compose restart`
- **plugin sync bearer**:worker 端 `HIGRESS_PLUGIN_BEARER` 与 manager 端 `OPSKEEPER_PLUGIN_SYNC_BEARER`
  必须一致,否则 push 请求会被 worker 401
- **subprocess 行为**:worker mock 用真实 `install_via_subprocess` 调用 `qwenpaw plugin install <path> --force`。
  - 真 qwenpaw → 实际安装到 qwenpaw runtime
  - mock qwenpaw → binary 不存在 → exit 127 → 返回 503 Service Unavailable
    (符合 install_via_subprocess 的 FileNotFoundError 处理路径)

## 六、调试

```bash
# 看 manager 日志(plugin sync / push 行为)
docker compose -f deploy/docker-compose.agentteams.yml logs -f opskeeper-manager

# 看 worker 日志(register + install 调用)
docker compose -f deploy/docker-compose.agentteams.yml logs -f worker-opskeeper-alerter

# 进入 manager shell 检查 plugin registry
docker exec -it opskeeper-manager sh -c 'ls -la /var/lib/opskeeper/plugins/'

# 进入 worker shell 检查 qwenpaw
docker exec -it worker-opskeeper-alerter sh -c 'which qwenpaw || echo qwenpaw missing'

# manager → worker 网络连通性
docker exec opskeeper-manager curl -s http://worker-opskeeper-alerter:8088/health
```

## 七、生产 vs 演示

| 维度 | 本演示栈 | 生产 K8s |
|---|---|---|
| Worker runtime | FastAPI mock 复用真实 plugin 代码 | 真 qwenpaw + opskeeper-teamharness plugin |
| Discovery | 静态 JSON 文件 | Controller GET /api/v1/workers (5min cache) |
| TLS | 无 | Higress AI Gateway GatewayKey |
| Persistent storage | Docker volume | K8s PVC / S3 |
| Healthcheck | wget /healthz | k8s livenessProbe / readinessProbe |
| Restart policy | unless-stopped | K8s Deployment.replicas |

升级到 K8s 只需把 `docker-compose.agentteams.yml` 翻译成 Helm values,
无需修改任何 plugin 代码。
