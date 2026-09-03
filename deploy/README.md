# demo 部署说明（仓库根 `docker-compose.yml`）

> 本 README 服务对象：**仓库根 `docker-compose.yml`**（新增）。
> 本地全栈开发仍走 `deploy/docker-compose.yml`（MySQL + Prometheus/Grafana）；
> 正式 release 用 `deploy/install/` 或 K8s `deploy/helm/`。
> **本 compose 仅供演示环境 + 内部联调，不要直接上生产。**

## 1. 服务拓扑

```
postgres (5432, pgvector) ──┐
                             ├─→ opskeeper (8080/9100) ──→ agentteams-controller (8090)
redis (6379) ───────────────┘            │
                                          ├─→ hitl-monitor → element-web (8081)
                                          └─→ 内部 cmd/agentteams-state (HITL state)
```

## 2. 一键启动

```bash
cd <repo-root>                                 # 切到仓库根（compose 文件所在目录）
cp deploy/demo.env.example .env                           # 按需修改密码 / escalation URL
docker compose up -d --build                   # 首次启动会构建 opskeeper:dev 镜像
docker compose ps                              # 等待所有 healthcheck 通过（约 60-90s）
docker compose logs -f opskeeper               # 跟日志确认启动正常
```

启动顺序（依赖图强制）：

1. `postgres` + `redis` 通过 healthcheck
2. `opskeeper` 依赖上面两者通过 → 自动跑 GORM `AutoMigrate` 建表
3. `agentteams-controller` 依赖 opskeeper healthy
4. `hitl-monitor` 依赖 opskeeper healthy → 开始扫 pending HITL
5. `element-web` 独立启动，admin 登录用 Element Web UI

> 首次启动后 `postgres_data` 卷已写入，再次 `up` 时 `deploy/postgres-init.sql` 不会再执行（官方镜像设计）。

## 3. 端口映射

| 服务 | 容器端口 | 主机端口（可改） | 用途 |
|---|---|---|---|
| opskeeper | 8080 | `${OPSKEEPER_HTTP_PORT:-8080}` | HTTP API + Swagger UI |
| opskeeper | 9100 | `${OPSKEEPER_METRICS_PORT:-9100}` | Prometheus `/metrics` |
| agentteams-controller | 8090 | `${AGENTTEAMS_CONTROLLER_PORT:-8090}` | AgentTeams Manager |
| element-web | 80 | `${ELEMENT_WEB_PORT:-8081}` | Admin 审批 UI |
| postgres | 5432 | `${POSTGRES_HOST_PORT:-5432}` | PostgreSQL + pgvector |
| redis | 6379 | `${REDIS_HOST_PORT:-6379}` | 缓存 / 分布式锁 |
| qdrant | 6333 | `${QDRANT_HOST_PORT:-6333}` | Knowledge RAG 向量索引 |

## 4. 验证步骤

### 4.1 容器健康

```bash
docker compose ps                            # 所有服务 state=healthy
docker compose logs --tail 50 postgres       # 看 init SQL 是否执行成功
```

### 4.2 数据库 + pgvector

```bash
docker compose exec postgres psql -U opskeeper -d opskeeper -c '\dx'
# 期望看到 `vector` 扩展
docker compose exec postgres psql -U opskeeper -d agentteams -c '\dx'
# 同样能看到 `vector`
docker compose exec postgres psql -U opskeeper -c '\l'
# 期望：opskeeper + agentteams 两个 DB
```

### 4.3 opskeeper HTTP

```bash
curl -sf http://localhost:8080/healthz | jq           # ok
curl -sf http://localhost:8080/metrics | head -20     # prom 指标暴露
```

Trace 采集端默认读取 `OPSKEEPER_OTEL_ENDPOINT`（OTLP HTTP，`host:port`）。可将其指向
Tempo、LoongSuite 或任意 OTLP 兼容 collector；collector 未就绪时 opskeeper 记录
警告并继续提供 API 与 metrics。

### 4.3a T16 case-owned CPU fixture

`host-fixture` 是 demo 专用独立服务，只允许终止自己创建的 2-4 个 CPU 进程；
manifest 不含 PID，`recovery.execute(command=kill_process)` 也不接受 PID。启动前先生成运行时 token：

```bash
export HOST_FIXTURE_TOKEN="$(openssl rand -hex 32)"
docker compose up -d --build host-fixture
curl -sf http://localhost:8091/healthz | jq
curl -sf -H "Authorization: Bearer ${HOST_FIXTURE_TOKEN}" \
  -H 'X-Opskeeper-Version: v1' \
  http://localhost:8091/metrics
```

OpsKeeper 侧通过 `OPSKEEPER_HOST_FIXTURE_URL=http://host-fixture:8091` 与同一运行时 token 注入；
手工或脚本不得替代 exact approved Proposal 调用终止接口。

KB 权限升级说明：启动器会自动把旧内置 vault 点补上 `tenant_scopes=["global"]`。
升级前已存在的手工 / 上传 / 租户仓库文档因为没有历史租户元数据，会被安全过滤；
请由对应租户重新上传或重新 Sync 一次以写入 `tenant:<id>` scope。全新 demo 环境不受影响。

### 4.4 AgentTeams 控制器连通

```bash
curl -sf http://localhost:8090/healthz | jq
```

### 4.5 HITL monitor

```bash
docker compose logs hitl-monitor --tail 50
# 期望首行：`[hitl-monitor <ISO时间>] starting; interval=60s timeout=900s opskeeper=http://opskeeper:8080`
docker compose exec hitl-monitor bash -c '
  /usr/local/bin/opskeeper-hitl-monitor.sh --help
'
```

### 4.6 Element Web UI

浏览器打开 <http://localhost:8081>，使用 `OPSKEEPER_ADMIN_EMAIL` / `OPSKEEPER_ADMIN_PASSWORD` 登录。

## 5. Demo 触发命令（pg/long-running-tx 剧本）

```bash
# 5.1 注入 long-running tx（demo helper，模拟生产事故）
docker compose exec postgres bash -c '
  psql -U opskeeper -d opskeeper <<SQL
    CREATE TABLE IF NOT EXISTS accounts (id int PRIMARY KEY, balance int);
    INSERT INTO accounts VALUES (1, 0) ON CONFLICT DO NOTHING;
    BEGIN;
    UPDATE accounts SET balance = balance + 1 WHERE id = 1;
    SELECT pg_sleep(120);     -- 持锁 2 分钟
    COMMIT;
SQL
' &

# 5.2 等待 opskeeper MCP `loop.investigate` 自动派 investigator
sleep 30
curl -sf http://localhost:8080/api/agentteams/incidents | jq

# 5.3 blast_radius 命中 @admin → Element Web 房间出现审批请求
#     浏览器打开 http://localhost:8081 → 进入事故房间 → approve / reject

# 5.4 15 分钟不响应 → hitl-monitor 自动升级
docker compose logs hitl-monitor --tail 100
# 期望看到 escalation POST 日志
```

## 6. 清理 / 复盘

```bash
docker compose down                  # 停容器，保留卷
docker compose down -v               # 同时清空 postgres_data / redis_data
docker system prune -f               # 清理悬空镜像（含 opskeeper:dev）
```

## 7. 故障排查

| 症状 | 排查 |
|---|---|
| `opskeeper` 起不来 / 连不上 postgres | `docker compose logs postgres` 看 healthcheck；确认 `POSTGRES_PASSWORD` 与 `.env` 一致 |
| `pgvector` 找不到 | 数据卷已存在 → `docker compose down -v` 后再 `up`；或手动 `psql -c 'CREATE EXTENSION vector;'` |
| `hitl-monitor` 报 `permission denied` | `chmod +x scripts/opskeeper-hitl-monitor.sh` 后 `docker compose restart hitl-monitor` |
| `element-web` 502 | opskeeper 还没就绪；等 `docker compose ps` 全 healthy 后浏览器硬刷 |
| `agentteams-controller` 镜像拉不到 | 占位镜像 `ghcr.io/agentteams/controller:dev` 需自行 publish；临时换 `AGENTTEAMS_CONTROLLER_IMAGE=...` |
| `AutoMigrate` 失败 | 看 `docker compose logs opskeeper`；多为 DSN 拼错或密码含特殊字符未做 URL encode |

## 8. 与现有 deploy/ 资产的关系

| 资产 | 用途 | 是否生产 |
|---|---|---|
| `docker-compose.yml`（仓库根，本 README 服务对象） | demo：opskeeper + AgentTeams + pgvector + Element Web | ❌ demo only |
| `deploy/docker-compose.yml` | 本地开发：MySQL + Prometheus/Grafana 全栈 | ❌ local dev |
| `deploy/install/` | 正式 release：TLS + 备份 + 监控告警 | ✅ |
| `deploy/helm/` | K8s 部署 | ✅ |
| `deploy/postgres-init.sql`（本目录新增） | demo 的 pgvector + agentteams DB 初始化 | ❌ demo only |
| `docker-compose.yml` 内 `qdrant` 服务 | KB SOP / incident pattern 向量索引；BM25 候选来自业务库 | ❌ demo only |
| `scripts/opskeeper-hitl-monitor.sh`（新增） | HITL 超时监控占位脚本 | ❌ demo only |
| `Dockerfile.dev`（仓库根新增） | `build context .` 对应的 demo 镜像 | ❌ demo only |
| `cmd/host-fixture` + `host-fixture` compose 服务 | T16 case-owned CPU 进程与受控 `kill_process` 执行端 | ❌ demo only |

## 9. .env 变量速查

详见仓库根 `deploy/demo.env.example`。要点：

- `POSTGRES_PASSWORD` / `OPSKEEPER_DB_PASSWORD` —— 必须一致（前者喂官方镜像，后者喂 opskeeper DSN）
- `OPSKEEPER_JWT_SECRET` —— 32 字符以上随机串
- `OPSKEEPER_HITL_ESCALATION_URL` —— demo 默认 `http://element-web:80`；线上请换飞书 / PagerDuty webhook
- `OPSKEEPER_HITL_TIMEOUT` —— demo 默认 `15m`；与 agentteams-controller `/api/agentteams/hitl/pending` 解析一致
- `OPSKEEPER_OTEL_ENDPOINT` —— OTLP HTTP collector；用于 Tempo / LoongSuite 兼容 Trace 接入
- `HOST_FIXTURE_TOKEN` —— 32 字符以上运行时随机值；同时注入 host-fixture 与 opskeeper，不落盘提交
- `OPSKEEPER_EMBEDDING_API_KEY` —— 配置后启用 KB 向量检索；BM25 始终可用并自动降级
