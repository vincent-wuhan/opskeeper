# opskeeper-teamharness Plugin

AgentTeams 插件，让 6 ops Worker（alerter/investigator/critic/reviewer/repairer/verifier）通过 stdio MCP proxy 调用 opskeeper-v2 后端能力（RCA / 修复 / 验证 / postmortem）。

## 架构

```
┌─────────────────────────────────────────────────────────────────┐
│ AgentTeams Worker (qwenpaw runtime)                              │
│   plugin.py (sanitizer + audit hook)                             │
│     └─ stdio MCP server (mcp/server.py) ──── HTTP /v1/mcp ────┐ │
│                                              Bearer GWKey      │
│                                                               ▼
│                                       ┌─────────────────────────────┐
│                                       │ opskeeper-v2 backend         │
│                                       │   middleware/auth.go         │
│                                       │   loop/mcp_adapter.go         │
│                                       │   agentteams/state.go        │
│                                       └─────────────────────────────┘
```

## qwenpaw HTTP router 端点（v1.0.1+）

通过 `register_http_router(prefix="/opskeeper-teamharness")` 暴露 4 个 FastAPI 端点，
由 opskeeper Manager 通过 Controller discovery 调用:

| 端点 | 方法 | 用途 | 调用方 |
|---|---|---|---|
| `/api/opskeeper-teamharness/health` | GET | liveness | opskeeper `/v1/plugins/{id}/sync` 前置检查 |
| `/api/opskeeper-teamharness/sync` | POST | in-memory 重载 prompt / skill / MCP | PluginSyncClient.SyncPlugin |
| `/api/opskeeper-teamharness/install-plugin/health` | GET | qwenpaw 二进制路径 + 容量上限 | 运维调试 |
| `/api/opskeeper-teamharness/install-plugin` | POST multipart | 上传 zip + `qwenpaw plugin install --force` | PluginSyncClient.InstallPlugin |

端点鉴权：同 teamharness `/api/teamharness/sync`，经 Higress AI Gateway GatewayKey 鉴权。
Worker 端 subprocess：`subprocess.run(["qwenpaw", "plugin", "install", path, "--force"], timeout=120)`，
exit_code ≠ 0 返回 500 + stderr，调用方按 partial failure 处理。
详见 ADR-020 §9 与 `adapters/qwenpaw/test_install_endpoint.py` (12 个 pytest 覆盖)。

## 目录结构

```
opskeeper-teamharness/
├── plugin.yaml                            # v1alpha1 base manifest
├── prompts/{team,agent,manager}/*.md      # Prompt overrides
├── skills/
│   ├── agent/opskeeper-{alerter,investigator,critic,reviewer,repairer,verifier}/SKILL.md
│   └── team/opskeeper-coordination/SKILL.md
├── mcp/
│   ├── server.py                          # stdio MCP proxy → opskeeper HTTP
│   ├── tools.py                           # 14 tools catalog
│   ├── names.py                           # plugin ↔ backend name remap + plugin native routing
│   ├── auth.py                            # Bearer GatewayKey + HMAC-SHA256 + trace context
│   └── test_alignment.py                  # 22 个端到端 + 名字对齐测试
├── adapters/
│   ├── qwenpaw/                           # qwenpaw runtime adapter
│   │   ├── plugin.json
│   │   ├── plugin.py
│   │   ├── task_trace.py                  # lifecycle hook → state.json audit
│   │   ├── install.sh + uninstall.sh
│   │   ├── scripts/{build,validate}-qwenpaw-plugin.rb
│   │   ├── test_task_trace.py             # 5 个 lifecycle 集成测试
│   │   └── README.md
│   └── claude-code/                       # placeholder（AgentTeams 原生也是 placeholder）
├── scripts/
│   ├── install.sh + uninstall.sh          # Lifecycle entrypoints
│   └── self_check.py                      # CI 自检脚本（7 checks）
├── loongsuite/agents.d/opskeeper-teamharness.json   # LoongSuite compat
├── examples/
│   ├── higress-setup.sh                   # Higress MCP proxy 注册
│   └── review-and-run.sh                  # Worker CR mcpServers patch
├── README.md
└── CHANGELOG.md
```

## 安装（运维侧 3 步）

### 1. 安装插件到 qwenpaw runtime

```bash
# 在 Worker 容器内（或 Manager 主机），执行：
bash /path/to/opskeeper-teamharness/scripts/install.sh

# 或直接用 qwenpaw：
bash /path/to/opskeeper-teamharness/adapters/qwenpaw/install.sh
```

### 2. 注册 opskeeper 为 Higress MCP proxy

```bash
# 在 Manager 主机（能调 setup-mcp-proxy.sh 的地方），执行：
OPSKEEPER_BACKEND_HOST=opskeeper.example.com:8443 \
OPSKEEPER_BOOTSTRAP_TOKEN=xxx \
HIGRESS_COOKIE_FILE=/tmp/higress.cookie \
bash /path/to/opskeeper-teamharness/examples/higress-setup.sh
```

### 3. 为 6 个 Worker CR 注入 mcpServers

```bash
OPSKEEPER_BACKEND_HOST=opskeeper.example.com:8443 \
AGENTTEAMS_NAMESPACE=agentteams \
bash /path/to/opskeeper-teamharness/examples/review-and-run.sh --execute
```

## 验证

### 0. 构建插件包

```bash
bash /path/to/opskeeper-teamharness/scripts/build-package.sh
```

产物统一输出到 `opskeeper-teamharness/dist/`：

- `opskeeper-teamharness-1.0.5-plugin-manager.tar.gz`：plugin-manager 标准上传包
- `opskeeper-teamharness-qwenpaw-1.0.5.zip`：QwenPaw 诊断 / Worker 安装包

### Worker 权限边界

QwenPaw Worker 默认运行在 `read_only` 模式：内置文件写入、Shell、浏览器等变更工具，以及 OpsKeeper 状态写入、恢复执行等逻辑变更工具都会在执行前被拒绝。只允许查询、观测、验证与消息类只读工具。

确需执行变更的 Worker 必须由运行时管理员显式设置：

```bash
OPSKEEPER_PERMISSION_MODE=standard
```

该配置不能由任务提示词或模型自行授予。只读验收任务不应设置该值。

### 1. 插件 validate

```bash
# TeamHarness base
ruby /path/to/AgentTeams/plugins/scripts/validate-plugin.rb \
  /path/to/opskeeper-teamharness/plugin.yaml

# QwenPaw zip（先 build）
ruby /path/to/opskeeper-teamharness/adapters/qwenpaw/scripts/build-qwenpaw-plugin.rb \
  /path/to/opskeeper-teamharness/plugin.yaml
# 解压后：
ruby /path/to/opskeeper-teamharness/adapters/qwenpaw/scripts/validate-qwenpaw-plugin.rb \
  /tmp/opskeeper-teamharness-qwenpaw-1.0.0
```

### 2. stdio MCP server 自检

```bash
OPSKEEPER_GATEWAY_KEY=test \
echo '{"jsonrpc":"2.0","method":"tools/list","id":1}' | \
  python3 /path/to/opskeeper-teamharness/mcp/server.py
```

### 3. 端到端

```bash
mcporter list opskeeper --schema
mcporter call opskeeper.loop.investigate --args '{"incident_id":"inc-123"}'
```

## 配置（Worker 容器 env）

| Env | 必需 | 来源 |
|---|---|---|
| `OPSKEEPER_BACKEND_URL` | 是 | AgentTeams Worker CR spec.env 或 Helm values |
| `OPSKEEPER_GATEWAY_KEY` | 是 | agentteams-controller credentials.go 自动注入 |
| `OPSKEEPER_TENANT_ID` | 否 | default |
| `OPSKEEPER_TIMEOUT` | 否 | 30s |
| `OPSKEEPER_LOG_LEVEL` | 否 | INFO |

## 工具命名空间对齐（plugin ↔ backend）

plugin 端给 Worker LLM 暴露的 14 个工具名（type.method 命名空间友好），通过 `mcp/names.py`
NAME_REMAP 改写到 backend `/v1/mcp` 实际工具名（TaskNameXxx 形式）：

| plugin name | backend name | route |
|---|---|---|
| `metric.query` | `query_promql` | /v1/mcp |
| `incident.list` | `query_incidents` | /v1/mcp |
| `incident.get` | `get_incident_detail` | /v1/mcp |
| `postgres.analyze_status` | `analyze_database_status` | /v1/mcp |
| `host.get_load` / `host.get_processes` | `get_host_load` / `get_host_processes` | /v1/mcp |
| `knowledge.query` | `query_knowledge` | /v1/mcp |
| `loop.investigate` / `loop.correlate` / `recovery.verify` / `host.restart_service` | (透传) | /v1/mcp |
| `hitl.decide` / `state.put` / `state.get` | (plugin native) | POST `/v1/hitl/decide` / PUT,GET `/v1/state/{task_id}` |

11 个被删除的 plugin 工具（如 `metric.query_range` / `k8s.*` / `audit.{list,search}` / `incident.update_status`）不在 backend 暴露，
Worker 改用 backend 既有 REST 端点。详见 Worker SKILL.md 的「Tools Removed in This Revision」章节。

## LoongSuite Trace 透传

plugin stdio MCP server 从 Worker 容器 env 读 W3C `TRACEPARENT`（优先）或
`LOONG_TRACE_ID`+`LOONG_SPAN_ID`（回落），注入 `traceparent` / `X-Trace-Id` 头。

backend mcp middleware `ExtractTrace` 解析后写入 ctx；`/v1/state/{task_id}` PUT 把它
写入 `state.json.trace_id` + `span_id`；`/v1/hitl/decide` 写 `state.audit[].trace_id`。
这样 Worker → backend → LoongSuite / Tempo 可按 trace_id 关联。

CI 自检脚本：`scripts/self_check.py`（7 checks）覆盖 plugin 全链路一致性。

## 协议版本

- AgentTeams v1alpha1（apiVersion: agentteams.agentteam/v1alpha1）
- QwenPaw 2.0.1+（adapter plugin.json min_version）
- opskeeper /v1/mcp v1（X-Opskeeper-Version 头）

## 参考

- [AgentTeams Plugin Schema](https://github.com/agentscope-ai/AgentTeams/blob/main/plugins/schemas/plugin.schema.json)
- [AgentTeams teamharness 插件](https://github.com/agentscope-ai/AgentTeams/tree/main/plugins/teamharness)
- `openspec/changes/agentteams-opskeeper-integration/` 设计文档
- `docs/superpowers/decisions/2026-08-25-opskeeper-as-agentteams-plugin.md` ADR-020
