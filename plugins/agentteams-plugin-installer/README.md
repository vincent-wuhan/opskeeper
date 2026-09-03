# AgentTeams Plugin Installer (Dashboard Plugin)

OpsKeeper 控制台扩展，让 OpsKeeper 真正成为 **AgentTeams plugin 的运维控制台**。

## 架构

```
┌───────────────────────────────────────────────────────────────┐
│ AgentTeams Dashboard (agentteams-dashboard, Next.js)         │
│                                                                │
│  Settings → 插件                                              │
│    → 上传 agentteams-plugin-installer.zip                     │
│    → 自动激活 5 个 extension points                           │
│                                                                │
│  ┌─────────────────────────────────────────────────────────┐  │
│  │ agentteams-plugin-installer (本插件)                    │  │
│  │  ├─ sidebar-menu  → "Plugin 管理"                       │  │
│  │  ├─ route         → PluginListRoute                     │  │
│  │  ├─ dashboard-widget → PluginStatsWidget                │  │
│  │  ├─ detail-panel  → Worker loaded plugins 块           │  │
│  │  └─ toolbar       → "+ Plugin" 按钮                     │  │
│  └─────────────────────────────────────────────────────────┘  │
│         │                                                       │
│         │ fetch /v1/plugins/* (Higress Bearer)                  │
│         ▼                                                       │
│  opskeeper backend (cmd/opskeeper)                                │
│    /v1/plugins               GET  列表                          │
│    /v1/plugins/{id}          GET  详情                          │
│    /v1/plugins/install       POST multipart zip                 │
│    /v1/plugins/{id}          DELETE 卸载                       │
│    /v1/plugins/{id}/enable   POST  启用                         │
│    /v1/plugins/{id}/disable  POST  停用                         │
│    /v1/plugins/{id}/sync     POST 触发 worker reload            │
│    /v1/plugins/{id}/push     POST 推送 zip 到 worker install      │
│         │                                                       │
│         ▼                                                       │
│  PluginRegistry (filesystem-based, /var/lib/opskeeper/plugins/) │
│    <id>/plugin.yaml                                             │
│    <id>/manifest.json                                           │
│    <id>/.installed_at                                           │
│    <id>/.status                                                 │
│    <id>/.payload.zip          ← 持久化原始 zip (push 用)         │
└───────────────────────────────────────────────────────────────┘
```

## 安装步骤

```bash
# 1. 构建本插件
cd plugins/agentteams-plugin-installer/dashboard
npm install
npm run build
# → dist/main.js (ES module, lib build)

# 2. 打包成 zip（vite lib 已构建，dist/ + plugin.json 一起）
cd ..
zip -j dist/agentteams-plugin-installer.zip \
  dashboard/public/plugin.json \
  dashboard/dist/main.js

# 3. 上传到 Dashboard
# Settings → 插件 → 选择 zip → 自动激活
```

## 后端依赖

opskeeper 启动时设置环境变量：

```bash
export OPSKEEPER_PLUGINS_DIR=/var/lib/opskeeper/plugins   # 可选，默认值
export HIGRESS_ADMIN_PASSWORD=...                        # Bearer 鉴权必需
```

后端 7 个 endpoint 由 `internal/manager/server/agentteams/plugin_http.go` 提供：
- list / get / install / uninstall / enable / disable / sync / **push**(zip → worker qwenpaw install)
Bearer 中间件复用 `cmd/opskeeper/auth_agentteams.go`。

## 演示流程

1. 启动 opskeeper: `go run ./cmd/opskeeper`
2. 打开 Dashboard: `http://localhost:3000`
3. Settings → 插件 → 上传 `agentteams-plugin-installer.zip`
4. 侧边栏出现 "Plugin 管理" 入口
5. 进入 Plugin 管理 → 上传 `opskeeper-teamharness-v1.0.0.zip`
6. 列表显示 opskeeper-teamharness v1.0.0（含 tools/skills/prompts 计数）
7. 点击 "Push" → opskeeper 把 zip multipart 上传到 worker 的
   `POST /api/opskeeper-teamharness/install-plugin` 端点
   (worker 内部 `qwenpaw plugin install <path> --force` 子进程 + plugin.register(api) 重执行)
8. 点击 "Sync" → 触发 worker 端 in-memory 配置热重载
9. Dashboard overview 卡片显示 "AgentTeams Plugins: 1 已装 / 1 启用 / 14 tools"
10. （K8s 验证）worker pod 内 `kubectl exec ... -- qwenpaw plugin list` 看到 opskeeper-teamharness active

## 安全约束

- **zip 上传限制**：10 MB（压缩）/ 50 MB（解压）/ 1000 文件
- **zip-slip 防护**：拒绝 `../` / `\\` / `\0`
- **plugin id 正则**：`^[a-z][a-z0-9-_]{0,63}$`
- **manifest 校验**：apiVersion=v1alpha1, kind=AgentTeamPlugin, name/version 必填
- **Bearer 鉴权**：复用 opskeeper-teamharness Higress consumer
- **filesystem 隔离**：每个 plugin 独立子目录，卸载 = `rm -rf <id>`

## 端到端数据流

Dashboard upload → opskeeper Manager → Controller discovery → Worker HTTP → qwenpaw subprocess

```
Dashboard UI
   │ upload opskeeper-teamharness.zip
   ▼
POST /v1/plugins/install (multipart)
   │ → PluginRegistry.Install  (.payload.zip 持久化)
   │ → PluginSyncClient.InstallPlugin
   ▼
GET /api/v1/workers  (AgentTeams Controller)
   │ → worker endpoint 列表
   ▼
POST<worker>/api/opskeeper-teamharness/install-plugin (multipart zip)
   │ → _extract_plugin_zip (zip-slip / oversize / empty 防护)
   │ → qwenpaw plugin install <path> --force  (subprocess)
   │ → plugin.register(api) 重新执行
   ▼
qwenpaw runtime active
```

## 已知限制

- **Worker `/install-plugin` 端点需 Higress GatewayKey 鉴权**（与 `/sync` 同）。
  当前 worker qwenpaw 默认 console_port = 8088，需要 cluster 内 NetworkPolicy 或
  Higress AI Gateway 暴露路径 `/api/<plugin>/**` 给 opskeeper Manager。
- **install 后建议显式触发 sync**（opskeeper-teamharness 不会自动重读 prompt）。
  用户需在 Dashboard 点 "Sync" 触发 in-memory 配置热重载。
- **plugin hash / 数字签名** 未做（P2）；当前仅 sha256 内容校验。
