import * as React from 'react';

// Toolbar button — 一键跳转到 Opskeeper 诊断 + 健康检查提示。
export default function OneClickRcaButton({ api }) {
  const [busy, setBusy] = React.useState(false);

  async function onClick() {
    setBusy(true);
    try {
      // 跳到主页（route.jsx 已经内置选择最高优先级事故触发 RCA）
      api.dashboard.navigate('plugin-route:opskeeper-teamharness/home');
      api.dashboard.toast('已跳转 Opskeeper 诊断', 'info');
    } finally {
      // 简单 250ms 防抖，防止用户连点
      setTimeout(() => setBusy(false), 250);
    }
  }

  return (
    <button
      onClick={onClick}
      disabled={busy}
      title="跳转到 Opskeeper 7 阶段 RCA 诊断"
      style={{
        display: 'inline-flex', alignItems: 'center', gap: 6,
        padding: '6px 12px', borderRadius: 4, fontSize: 12,
        border: '1px solid var(--border)',
        background: 'var(--primary)',
        color: 'var(--primary-foreground)',
        cursor: busy ? 'wait' : 'pointer',
      }}
    >
      <span style={{ fontSize: 14 }}>⚡</span>
      <span>{busy ? '跳转中…' : '一键 RCA'}</span>
    </button>
  );
}
