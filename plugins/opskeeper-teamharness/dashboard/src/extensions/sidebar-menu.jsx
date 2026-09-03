import * as React from 'react';

// Sidebar menu item for opskeeper-teamharness.
// Renders a compact nav entry that links to the main plugin route via Dashboard host API.
export default function SidebarMenuItem({ api }) {
  return (
    <button
      onClick={() => api.dashboard.navigate('plugin-route:opskeeper-teamharness/home')}
      style={{
        display: 'flex', alignItems: 'center', gap: 8, width: '100%',
        padding: '8px 12px', borderRadius: 6,
        border: '1px solid transparent',
        background: 'transparent',
        color: 'var(--sidebar-foreground, var(--card-foreground))',
        cursor: 'pointer', fontSize: 13, textAlign: 'left',
      }}
      title="打开 Opskeeper 7 阶段 RCA 诊断"
    >
      <span style={{ fontSize: 16 }}>🛡️</span>
      <span>Opskeeper 诊断</span>
      <span style={{
        marginLeft: 'auto', fontSize: 9, opacity: 0.6,
        padding: '1px 5px', borderRadius: 3,
        background: 'var(--muted)', color: 'var(--muted-foreground)',
      }}>
        RCA
      </span>
    </button>
  );
}
