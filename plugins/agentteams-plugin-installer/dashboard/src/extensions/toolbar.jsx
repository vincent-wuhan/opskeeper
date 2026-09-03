import * as React from 'react';

// Toolbar button — 直接触发跳转到 plugin 管理页。
// 实际 registerToolbarButton 已在 main.jsx 完成；这个组件作为 fallback 渲染。
export default function QuickInstallButton({ api }) {
  return React.createElement(
    'button',
    {
      onClick: () => api.dashboard.navigate('plugin-route:agentteams-plugin-installer/home'),
      style: {
        padding: '4px 12px',
        fontSize: 12,
        borderRadius: 4,
        border: '1px solid var(--border)',
        background: 'var(--primary)',
        color: 'var(--primary-foreground)',
        cursor: 'pointer',
      },
    },
    '+ Plugin'
  );
}
