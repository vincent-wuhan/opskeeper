import * as React from 'react';

// Sidebar menu is auto-rendered by Dashboard host; this file exists for parity
// with teamharness/workerflow patterns. The actual registerMenuItem call is
// in src/main.jsx. This component is exported in case host wants to embed a
// richer sub-tree (currently not used).

export default function SidebarMenu({ api }) {
  return React.createElement(
    'div',
    { style: { padding: '8px 12px', fontSize: 12, color: 'var(--muted)' } },
    'AgentTeams Plugin 管理 — opskeeper 是 AgentTeams plugin 的运维控制台。'
  );
}
