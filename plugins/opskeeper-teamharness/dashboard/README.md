# opskeeper-teamharness Dashboard Plugin

Dashboard UI extension for the `opskeeper-teamharness` AgentTeams plugin.
Same package ships both `plugin.yaml` (AgentTeams runtime) and `dashboard/plugin.json` (Dashboard UI) — no AgentTeams fork required.

## 5 Extension Points Registered

| Extension | Purpose |
|---|---|
| `sidebar-menu` | "Opskeeper 诊断" menu item → diagnose route |
| `route` | 7-stage RCA report page (raw RootCauseJSON viewer) |
| `dashboard-widget` | Active incidents, service health, localization, and audit completeness on Overview |
| `detail-panel` | Worker detail panel → "Opskeeper 历史" block |
| `toolbar` | "一键 RCA" top-bar button |

All extensions read opskeeper backend state via the Dashboard's `/api/opskeeper/*` proxy (see Dashboard `proxy-helper.ts`).

## Runtime Readback

The `OpsKeeper Runtime` route reads `/v1/system/health`, `/v1/version`, `/v1/incidents/metrics`, and the incident list through the Dashboard proxy. It provides a bypass execution view while AgentTeams remains the collaboration-task source of truth. Runtime incidents are correlated with collaboration work by `incident_id`, `task_id`, and `trace_id`; the plugin does not mutate native task-board state.

## Local Development

```bash
cd plugins/opskeeper-teamharness/dashboard
npm install
npm run dev   # serves http://localhost:5173/plugin.json
```

In Dashboard Settings → Plugins → 填入 `http://localhost:5173/plugin.json`,点击安装。

环境变量注入（开发模式）：
```bash
NEXT_PUBLIC_PLUGIN_DEV_URLS=http://localhost:5173/plugin.json \
  npm run dev   # in agentteams-dashboard repo
```

## Architecture Notes

- Uses Dashboard's `window.__AGENTTEAMS_DASHBOARD_HOST__` for React instance (no double React, hooks/context cross-boundary OK).
- Each extension component is wrapped in its own error boundary by Dashboard host — single-plugin failure does not break main app.
- State isolated via `api.store` (Zustand, namespaced per plugin id).

## Compatibility

- AgentTeams Dashboard ≥ 0.2.0
- Plugin `apiVersion`: `dashboard.agentteams/v1`
- Same package is also a valid `agentteams.agentteam/v1alpha1` AgentTeams plugin (see `../plugin.yaml`).

## Build for Production

```bash
npm run build
# output: dist/
# Dashboard loads via URL or copies into public/plugins/
```
