import * as React from 'react';
import OpskeeperRoute from './route.jsx';
import OpskeeperRuntimeRoute from './runtime-route.jsx';
import OpskeeperInstallView from './install-view.jsx';
import { OPSKEEPER_TABS, normalizeOpskeeperTab } from './tabs.js';

export default function OpskeeperUnifiedRoute({ api, initialTab = 'diagnostics' }) {
  const [tab, setTab] = React.useState(() => normalizeOpskeeperTab(initialTab));

  return (
    <div>
      <header style={{
        display: 'flex',
        alignItems: 'center',
        gap: 12,
        padding: '18px 24px 0',
      }}>
        <span style={{ fontSize: 24 }}>🛡️</span>
        <div>
          <h1 style={{ margin: 0, fontSize: 20 }}>OpsKeeper</h1>
          <p style={{ margin: '3px 0 0', fontSize: 12, color: 'var(--muted)' }}>
            AgentTeams 协同入口：诊断闭环、运行时读back 与插件安装统一管理。
          </p>
        </div>
      </header>
      <nav style={{
        display: 'flex',
        gap: 6,
        padding: '14px 24px 18px',
        borderBottom: '1px solid var(--border)',
      }}>
        {OPSKEEPER_TABS.map((item) => {
          const active = item.id === tab;
          return (
            <button
              key={item.id}
              type="button"
              title={item.description}
              onClick={() => setTab(item.id)}
              style={{
                padding: '6px 13px',
                borderRadius: 999,
                fontSize: 12,
                fontWeight: active ? 600 : 400,
                border: `1px solid ${active ? 'var(--primary)' : 'var(--border)'}`,
                background: active ? 'var(--primary)' : 'transparent',
                color: active ? 'var(--primary-foreground)' : 'var(--card-foreground)',
                cursor: 'pointer',
              }}
            >
              {item.label}
            </button>
          );
        })}
      </nav>
      {tab === 'diagnostics' && <OpskeeperRoute api={api} />}
      {tab === 'runtime' && <OpskeeperRuntimeRoute api={api} />}
      {tab === 'plugins' && <OpskeeperInstallView api={api} />}
    </div>
  );
}
