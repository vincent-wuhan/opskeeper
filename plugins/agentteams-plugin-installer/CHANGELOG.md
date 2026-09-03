# Changelog

All notable changes to `agentteams-plugin-installer` are documented here.

## [1.0.0] - 2026-08-25

### Added
- **Backend** (`internal/manager/server/agentteams/plugin_http.go`):
  - `GET /v1/plugins` — list installed
  - `GET /v1/plugins/{id}` — get details
  - `POST /v1/plugins/install` — multipart zip upload (10MB cap)
  - `DELETE /v1/plugins/{id}` — uninstall
  - `POST /v1/plugins/{id}/enable` — enable
  - `POST /v1/plugins/{id}/disable` — disable
  - `POST /v1/plugins/{id}/sync` — trigger worker reload (stub LoggingSyncClient)
- **Backend** (`internal/manager/server/agentteams/registry.go`):
  - `PluginRegistry` — filesystem-based plugin index
  - `${OPSKEEPER_PLUGINS_DIR}/<id>/{plugin.yaml, manifest.json, .installed_at, .status}`
  - v1alpha1 manifest parser + 计数 tools/skills/prompts/adapters
  - zip-slip / 大小 / 文件数 / id 格式校验
- **Backend** (`internal/agentteams/plugin_sync.go`):
  - `LoggingSyncClient` — stub for sync, log-only
- **Backend tests** (`plugin_http_test.go`):
  - 7 个 test cases，覆盖 install/list/get/delete/enable/disable/sync/duplicate/invalid/zip-slip

- **Dashboard plugin** (`plugins/agentteams-plugin-installer/dashboard/`):
  - `public/plugin.json` — Dashboard manifest (apiVersion: dashboard.agentteams/v1)
  - `src/main.jsx` — activate/deactivate，注册 5 个 extension points
  - `src/extensions/api.js` — HTTP helper (list/get/install/uninstall/enable/disable/sync)
  - `src/extensions/route.jsx` — Plugin 管理主页面（list + 上传 + 操作）
  - `src/extensions/dashboard-widget.jsx` — overview 卡片（已装 / 启用 / tools 数）
  - `src/extensions/detail-panel.jsx` — Worker 详情页内嵌（per-worker sync 按钮）
  - `src/extensions/sidebar-menu.jsx` — 侧边栏菜单
  - `src/extensions/toolbar.jsx` — toolbar 按钮
  - `vite.config.mjs` — vite lib build
  - `vite-plugin-host-react.mjs` — Dashboard host React 共享 shim
  - `package.json` — react 19 + vite 6
  - `dist/main.js` — 构建产物 (60 KB)

- **Tools**:
  - `scripts/self_check.py` — 31 项 check 全 PASS
  - `scripts/build-and-zip.sh` — `npm install + build + zip` 一键打包
  - `scripts/demo-install-opskeeper-teamharness.sh` — 端到端演示脚本
  - `Makefile` — `make build / plugin-zip / self-check / verify / clean`

### Verified
- `go test ./internal/manager/server/agentteams/` PASS (7/7)
- `npm run build` 成功（dist/main.js 60.34 KB / gzip 15.02 KB）
- `python3 scripts/self_check.py` PASS（31/31）
- `python3 plugins/opskeeper-teamharness/scripts/self_check.py` PASS（既有 17 项不回归）

### Limits / Future
- `PluginSyncClient` 当前是 stub（记日志），不真触发 Worker reload
- Worker 端 reload endpoint 由 QwenPaw 自身提供，opskeeper 通过 Controller webhook 转发
- 见 ADR-020 §7 后续扩展点
