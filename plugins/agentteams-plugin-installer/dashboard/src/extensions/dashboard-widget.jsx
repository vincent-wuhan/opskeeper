import * as React from 'react';
import { pluginApi } from './api.js';

// Dashboard overview widget — 概览卡片：显示已装 plugin 数、enabled 数、tools 总数。
export default function PluginStatsWidget({ api }) {
  const [stats, setStats] = React.useState({ total: 0, enabled: 0, tools: 0 });
  const [loading, setLoading] = React.useState(true);

  React.useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        const data = await pluginApi.list();
        if (cancelled) return;
        const plugins = data.plugins || [];
        const enabled = plugins.filter((p) => p.status === 'enabled').length;
        const tools = plugins.reduce((acc, p) => acc + (p.tool_count || 0), 0);
        setStats({ total: plugins.length, enabled, tools });
      } catch {
        /* widget must never crash host */
      } finally {
        if (!cancelled) setLoading(false);
      }
    })();
    return () => {
      cancelled = true;
    };
  }, []);

  return React.createElement(
    'div',
    {
      style: {
        padding: 16,
        border: '1px solid var(--border)',
        borderRadius: 8,
        background: 'var(--card)',
        color: 'var(--card-foreground)',
        cursor: 'pointer',
      },
      onClick: () => api.dashboard.navigate('plugin-route:agentteams-plugin-installer/home'),
      title: '跳转到 AgentTeams Plugin 管理',
    },
    React.createElement(
      'div',
      { style: { display: 'flex', alignItems: 'center', gap: 6, marginBottom: 8 } },
      React.createElement('span', { style: { fontSize: 13, fontWeight: 600 } }, 'AgentTeams Plugins'),
      React.createElement(
        'span',
        {
          style: {
            marginLeft: 'auto',
            padding: '1px 6px',
            borderRadius: 4,
            fontSize: 10,
            background: 'var(--muted)',
            color: 'var(--muted-foreground)',
          },
        },
        'plugin'
      )
    ),
    loading
      ? React.createElement('div', { style: { fontSize: 12, color: 'var(--muted)' } }, '加载中…')
      : React.createElement(
          'div',
          { style: { display: 'flex', gap: 12, fontSize: 12 } },
          React.createElement(
            'div',
            null,
            React.createElement('div', { style: { fontSize: 18, fontWeight: 600 } }, String(stats.total)),
            React.createElement('div', { style: { color: 'var(--muted)' } }, '已装')
          ),
          React.createElement(
            'div',
            null,
            React.createElement('div', { style: { fontSize: 18, fontWeight: 600, color: 'var(--success, #10b981)' } }, String(stats.enabled)),
            React.createElement('div', { style: { color: 'var(--muted)' } }, '启用')
          ),
          React.createElement(
            'div',
            null,
            React.createElement('div', { style: { fontSize: 18, fontWeight: 600 } }, String(stats.tools)),
            React.createElement('div', { style: { color: 'var(--muted)' } }, 'tools')
          )
        )
  );
}
