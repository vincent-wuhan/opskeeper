// opskeeper-teamharness Dashboard plugin entry
// apiVersion: dashboard.agentteams/v1
//
// activate(api) is called once by the Dashboard host at plugin install.
// 5 extension points are registered; each component lives in ./extensions/<point>.jsx.
// Components consume the shared opskeeperApi helper to read opskeeper backend state.

import * as React from 'react';
import SidebarMenu from './extensions/sidebar-menu.jsx';
import OpskeeperRoute from './extensions/route.jsx';
import OpskeeperInstallView from './extensions/install-view.jsx';
import OpskeeperStatsWidget from './extensions/dashboard-widget.jsx';
import WorkerOpsBlock from './extensions/detail-panel.jsx';
import OneClickRcaButton from './extensions/toolbar.jsx';

export const PLUGIN_ID = 'opskeeper-teamharness';
export const OPSKEEPER_API_BASE = '/api/opskeeper';

export function activate(api) {

  api.registerMenuItem({
    id: 'opskeeper-home',
    label: 'Opskeeper 诊断',
    icon: 'gauge',
    target: { type: 'plugin-route', routeId: 'home' },
    order: 10,
  });

  api.registerMenuItem({
    id: 'opskeeper-install',
    label: 'opskeeper 插件',
    icon: 'package',
    target: { type: 'plugin-route', routeId: 'install' },
    order: 11,
  });

  api.registerRoute({
    id: 'home',
    title: 'Opskeeper 7 阶段 RCA',
    component: () => React.createElement(OpskeeperRoute, { api }),
  });

  api.registerRoute({
    id: 'install',
    title: 'opskeeper 插件安装',
    component: () => React.createElement(OpskeeperInstallView, { api }),
  });

  api.registerWidget({
    id: 'opskeeper-stats',
    title: 'Opskeeper 概览',
    component: () => React.createElement(OpskeeperStatsWidget, { api }),
    size: 'md',
    order: 5,
  });

  api.registerDetailBlock({
    id: 'opskeeper-history',
    entity: 'worker',
    component: (props) => React.createElement(WorkerOpsBlock, { api, ...props }),
  });

  api.registerToolbarButton({
    id: 'one-click-rca',
    label: '一键 RCA',
    icon: 'zap',
    onClick: () => api.dashboard.navigate('plugin-route:opskeeper-teamharness/home'),
  });

  api.log.info('OpsKeeper TeamHarness Dashboard plugin activated', { plugin: PLUGIN_ID });

  return {
    PLUGIN_ID,
    OPSKEEPER_API_BASE,
  };
}

export function deactivate() {
  // Dashboard host cleans up registered extensions and store on deactivate.
}

const plugin = { activate, deactivate };
export default plugin;
