import * as React from 'react';
import { opskeeperApi } from './api.js';
import { buildRuntimeSnapshot } from './runtime.js';

// Dashboard overview widget — 概览卡：active / open / closed / avg-RCA / 阶段通过率。
//
// 数据源：opskeeperApi.listIncidents({ limit: 50 })。从最近 50 个事故聚合：
//   - active = status ∈ {open, in_progress, investigating}
//   - open   = status ∈ {open, in_progress}
//   - closed = status = resolved
//   - avg_rca_seconds = resolved 事故的 RCA 耗时平均值（started_at → resolved_at）
export default function OpskeeperStatsWidget({ api }) {
  const [stats, setStats] = React.useState({
    active: 0, open: 0, closed: 0, total: 0, avgRcaSeconds: null,
    overallStatus: 'unknown', checkedAt: null, meanLocalizationSeconds: null,
    auditEvidenceCompleteness: null,
  });
  const [loading, setLoading] = React.useState(true);

  const refresh = React.useCallback(async () => {
    setLoading(true);
    try {
      const [incidentsResult, healthResult, metricsResult] = await Promise.allSettled([
        opskeeperApi.listIncidents({ limit: 50 }),
        opskeeperApi.getSystemHealth(),
        opskeeperApi.getIncidentMetrics(),
      ]);
      const data = incidentsResult.status === 'fulfilled' ? incidentsResult.value : null;
      const incidents = data?.incidents || data?.data || [];
      const ACTIVE = new Set(['open', 'in_progress', 'investigating', 'repairing', 'verifying']);
      let active = 0, openCount = 0, closed = 0;
      const rcaDurations = [];
      for (const i of incidents) {
        const s = (i.status || '').toLowerCase();
        if (ACTIVE.has(s)) active++;
        if (s === 'open' || s === 'in_progress') openCount++;
        if (s === 'resolved' || s === 'closed') {
          closed++;
          const t0 = Date.parse(i.started_at || i.created_at || '');
          const t1 = Date.parse(i.resolved_at || i.ended_at || '');
          if (Number.isFinite(t0) && Number.isFinite(t1) && t1 > t0) {
            rcaDurations.push((t1 - t0) / 1000);
          }
        }
      }
      const avgRca = rcaDurations.length > 0
        ? rcaDurations.reduce((a, b) => a + b, 0) / rcaDurations.length
        : null;
      const snapshot = buildRuntimeSnapshot({
        health: healthResult.status === 'fulfilled' ? healthResult.value : null,
        metrics: metricsResult.status === 'fulfilled' ? metricsResult.value : null,
        incidents,
      });
      setStats({
        active,
        open: openCount,
        closed,
        total: incidents.length,
        avgRcaSeconds: avgRca,
        overallStatus: snapshot.overallStatus,
        checkedAt: snapshot.health?.checkedAt || null,
        meanLocalizationSeconds: snapshot.metrics?.meanLocalizationSeconds ?? null,
        auditEvidenceCompleteness: snapshot.metrics?.auditEvidenceCompleteness ?? null,
      });
    } catch {
      // widget must never crash host
    } finally {
      setLoading(false);
    }
  }, []);

  React.useEffect(() => {
    refresh();
    const id = setInterval(refresh, 30000);
    return () => clearInterval(id);
  }, [refresh]);

  function fmtDuration(seconds) {
    if (seconds == null) return '—';
    if (seconds < 60) return `${seconds.toFixed(0)}s`;
    if (seconds < 3600) return `${(seconds / 60).toFixed(1)}m`;
    return `${(seconds / 3600).toFixed(1)}h`;
  }

  return (
    <div
      style={{
        padding: 16, border: '1px solid var(--border)', borderRadius: 8,
        background: 'var(--card)', color: 'var(--card-foreground)', cursor: 'pointer',
      }}
      onClick={() => api.dashboard.navigate('plugin-route:opskeeper-teamharness/home')}
      title="跳转到 Opskeeper 诊断"
    >
      <div style={{ display: 'flex', alignItems: 'center', gap: 6, marginBottom: 10 }}>
        <span style={{ fontSize: 13, fontWeight: 600 }}>Opskeeper</span>
        <span style={{
          width: 7, height: 7, borderRadius: '50%',
          background: statusColor(stats.overallStatus),
        }} />
        <span style={{
          marginLeft: 'auto', padding: '1px 6px', borderRadius: 4, fontSize: 10,
          background: 'var(--muted)', color: 'var(--muted-foreground)',
        }}>
          7 阶段 RCA
        </span>
      </div>
      {loading ? (
        <div style={{ fontSize: 12, color: 'var(--muted)' }}>加载中…</div>
      ) : (
        <div style={{ display: 'flex', gap: 14, fontSize: 12 }}>
          <div>
            <div style={{
              fontSize: 22, fontWeight: 600,
              color: stats.active > 0 ? '#ef4444' : 'var(--card-foreground)',
            }}>{stats.active}</div>
            <div style={{ color: 'var(--muted)', fontSize: 11 }}>进行中</div>
          </div>
          <div>
            <div style={{ fontSize: 22, fontWeight: 600 }}>{stats.open}</div>
            <div style={{ color: 'var(--muted)', fontSize: 11 }}>待处理</div>
          </div>
          <div>
            <div style={{ fontSize: 22, fontWeight: 600, color: '#10b981' }}>{stats.closed}</div>
            <div style={{ color: 'var(--muted)', fontSize: 11 }}>已闭环</div>
          </div>
          <div style={{ marginLeft: 'auto', textAlign: 'right' }}>
            <div style={{ fontSize: 14, fontWeight: 500 }}>
              {fmtDuration(stats.avgRcaSeconds)}
            </div>
            <div style={{ color: 'var(--muted)', fontSize: 11 }}>平均 RCA</div>
          </div>
        </div>
      )}
      <div style={{
        marginTop: 10, paddingTop: 8, borderTop: '1px solid var(--border)',
        fontSize: 10, color: 'var(--muted)',
        display: 'flex', justifyContent: 'space-between',
      }}>
        <span>定位 {fmtDuration(stats.meanLocalizationSeconds)} · 审计 {fmtPercent(stats.auditEvidenceCompleteness)}</span>
        <span>{stats.checkedAt ? new Date(stats.checkedAt).toLocaleTimeString() : '30s 自动刷新'}</span>
      </div>
    </div>
  );
}

function statusColor(status) {
  if (status === 'ok') return '#10b981';
  if (status === 'degraded') return '#f59e0b';
  if (status === 'failed') return '#ef4444';
  return '#a1a1aa';
}

function fmtPercent(value) {
  return value == null ? '—' : `${(value * 100).toFixed(0)}%`;
}
