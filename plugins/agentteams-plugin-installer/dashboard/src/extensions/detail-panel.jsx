import * as React from 'react';
import { pluginApi } from './api.js';

// Worker 详情页嵌入区块 — 列出此 worker 上加载的 plugin。
// 当前 worker entity 不带 loaded_plugins 字段，所以列出**所有**已装 plugin 作为参考，
// 并在每条上加 "request sync to this worker" 按钮 — Dashboard host 调用 worker id 透传。
export default function PluginDetailPanel({ entity, api }) {
  const workerName = entity && entity.name;
  const [plugins, setPlugins] = React.useState([]);
  const [loading, setLoading] = React.useState(true);
  const [busy, setBusy] = React.useState(null);

  const refresh = React.useCallback(async () => {
    try {
      const data = await pluginApi.list();
      setPlugins(data.plugins || []);
    } catch {
      /* block never crashes host */
    } finally {
      setLoading(false);
    }
  }, []);

  React.useEffect(() => {
    refresh();
  }, [refresh]);

  async function syncTo(p) {
    setBusy(p.id);
    try {
      const result = await pluginApi.sync(p.id);
      api.dashboard.toast(
        `已请求 sync ${p.id} → worker ${workerName || '?'}: ${result.synced ? 'OK' : result.reason || 'stub'}`,
        result.synced ? 'success' : 'info'
      );
    } catch (e) {
      api.dashboard.toast(`sync 失败：${e.message}`, 'error');
    } finally {
      setBusy(null);
    }
  }

  return React.createElement(
    'div',
    {
      style: {
        padding: 12,
        border: '1px solid var(--border)',
        borderRadius: 8,
        background: 'var(--card)',
      },
    },
    React.createElement(
      'div',
      { style: { display: 'flex', alignItems: 'center', marginBottom: 8 } },
      React.createElement('strong', { style: { fontSize: 13 } }, 'Loaded AgentTeams Plugins'),
      React.createElement(
        'span',
        { style: { marginLeft: 8, fontSize: 11, color: 'var(--muted)' } },
        workerName ? `worker: ${workerName}` : 'worker: -'
      ),
      React.createElement(
        'button',
        {
          onClick: refresh,
          style: {
            marginLeft: 'auto',
            padding: '2px 8px',
            fontSize: 11,
            borderRadius: 4,
            border: '1px solid var(--border)',
            background: 'transparent',
            color: 'var(--muted)',
            cursor: 'pointer',
          },
        },
        '刷新'
      )
    ),
    loading
      ? React.createElement('div', { style: { fontSize: 12, color: 'var(--muted)' } }, '加载中…')
      : plugins.length === 0
        ? React.createElement(
            'div',
            { style: { fontSize: 12, color: 'var(--muted)' } },
            '集群暂无 AgentTeams plugin — 到 Plugin 管理页面上传 zip'
          )
        : React.createElement(
            'div',
            { style: { display: 'flex', flexDirection: 'column', gap: 4 } },
            plugins.map((p) =>
              React.createElement(
                'div',
                {
                  key: p.id,
                  style: {
                    display: 'flex',
                    alignItems: 'center',
                    gap: 6,
                    padding: '4px 8px',
                    fontSize: 12,
                    borderRadius: 4,
                    background: 'var(--muted-bg, rgba(127,127,127,0.06))',
                  },
                },
                React.createElement('span', null, p.name || p.id),
                React.createElement('span', { style: { color: 'var(--muted)' } }, 'v' + p.version),
                React.createElement(
                  'span',
                  {
                    style: {
                      padding: '1px 5px',
                      borderRadius: 3,
                      fontSize: 10,
                      background: p.status === 'enabled' ? '#10b981' : p.status === 'error' ? '#ef4444' : '#888',
                      color: '#fff',
                    },
                  },
                  p.status || 'installed'
                ),
                React.createElement(
                  'button',
                  {
                    onClick: () => syncTo(p),
                    disabled: busy === p.id,
                    style: {
                      marginLeft: 'auto',
                      padding: '1px 6px',
                      fontSize: 10,
                      borderRadius: 3,
                      border: '1px solid var(--border)',
                      background: 'transparent',
                      color: 'var(--muted)',
                      cursor: busy === p.id ? 'wait' : 'pointer',
                    },
                  },
                  busy === p.id ? '…' : 'sync'
                )
              )
            )
          )
  );
}
