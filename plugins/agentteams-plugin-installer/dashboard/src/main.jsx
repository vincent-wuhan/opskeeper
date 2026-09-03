// agentteams-plugin-installer — Dashboard plugin entry
// apiVersion: dashboard.agentteams/v1
//
// 在 Dashboard host 中被 activate(api) 调用一次，注册 5 个 extension points。
// 5 个组件位于 ./extensions/<point>.jsx，统一通过 ./extensions/api.js 调
// agentteams-plugin-manager 后端（独立 microservice，零 opskeeper 依赖）。
//
// BASE path: /api/v1/plugins
//   - Dashboard wrapper.js 把 /api/v1/plugins/* 转发到 agentteams-plugin-manager:8095
//   - wrapper.js 注入 Bearer SA token（与 controller 共享同一份 secret）
//   - plugin-manager 验签通过后返回 plugin 列表 / 详情 / 安装 / 卸载 / sync

import * as React from 'react';
import SidebarMenu from './extensions/sidebar-menu.jsx';
import PluginListRoute from './extensions/route.jsx';
import PluginStatsWidget from './extensions/dashboard-widget.jsx';
import PluginDetailPanel from './extensions/detail-panel.jsx';
import QuickInstallButton from './extensions/toolbar.jsx';

export const PLUGIN_ID = 'agentteams-plugin-installer';
export const OPSKEEPER_PLUGINS_API = '/api/v1/plugins';

export function activate(api) {
  const { http } = api;

  // 1. 侧边栏菜单
  api.registerMenuItem({
    id: 'agentteams-plugin-installer-home',
    label: 'Plugin 管理',
    icon: 'package',
    target: { type: 'plugin-route', routeId: 'home' },
    order: 100,
  });

  // 2. 主路由 — list + 上传 + 安装 + 操作
  api.registerRoute({
    id: 'home',
    title: 'AgentTeams Plugin 管理',
    component: () => React.createElement(PluginListRoute, { api, http }),
  });

  // 3. dashboard widget — 显示已装 plugin 数量
  api.registerWidget({
    id: 'plugin-stats',
    title: 'AgentTeams Plugins',
    component: () => React.createElement(PluginStatsWidget, { api, http }),
    size: 'sm',
    order: 95,
  });

  // 4. detail panel — 在 Worker 详情页显示已装 plugin 列表
  api.registerDetailBlock({
    id: 'worker-loaded-plugins',
    entity: 'worker',
    component: (props) => React.createElement(PluginDetailPanel, { api, http, ...props }),
  });

  // 5. toolbar — 一键跳到 plugin 管理页
  api.registerToolbarButton({
    id: 'quick-plugin-install',
    label: 'Plugin 管理',
    icon: 'package-plus',
    onClick: () => api.dashboard.navigate(`plugin-route:${PLUGIN_ID}/home`),
  });

  api.log.info('AgentTeams Plugin Installer activated');
}

export function deactivate() {
  // Dashboard host 自动清理所有 register* 副作用（store / events）
}

const plugin = { activate, deactivate };
export default plugin;
