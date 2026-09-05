import * as React from 'react';
import { opskeeperApi } from './api.js';
import { buildRuntimeSnapshot } from './runtime.js';

const STATUS_COLORS = {
  ok: '#10b981',
  degraded: '#f59e0b',
  failed: '#ef4444',
  unknown: '#a1a1aa',
};

const STATUS_LABELS = {
  ok: '健康',
  degraded: '降级',
  failed: '异常',
  unknown: '未知',
};

export default function OpskeeperRuntimeRoute({ api }) {
  const [snapshot, setSnapshot] = React.useState(null);
  const [errors, setErrors] = React.useState({});
  const [loading, setLoading] = React.useState(true);

  const refresh = React.useCallback(async () => {
    setLoading(true);
    const [health, version, metrics, incidents] = await Promise.allSettled([
      opskeeperApi.getSystemHealth(),
      opskeeperApi.getVersion(),
      opskeeperApi.getIncidentMetrics(),
      opskeeperApi.listIncidents({ limit: 20 }),
    ]);
    const nextErrors = {
      health: errorText(health),
      version: errorText(version),
      metrics: errorText(metrics),
      incidents: errorText(incidents),
    };
    setErrors(nextErrors);
    setSnapshot(buildRuntimeSnapshot({
      health: valueOf(health),
      version: valueOf(version),
      metrics: valueOf(metrics),
      incidents: incidentsOf(valueOf(incidents)),
    }));
    setLoading(false);
  }, []);

  React.useEffect(() => {
    refresh();
  }, [refresh]);

  const health = snapshot?.health;
  const metrics = snapshot?.metrics;
  const groupedChecks = React.useMemo(() => groupBy(
    health?.checks || [],
    (check) => check.group || 'other',
  ), [health]);

  return (
    <div style={{ padding: 24, color: 'var(--card-foreground)' }}>
      <header style={{ display: 'flex', alignItems: 'center', gap: 12, marginBottom: 18 }}>
        <div>
          <h2 style={{ margin: 0, fontSize: 18 }}>OpsKeeper 运行时</h2>
          <p style={{ margin: '4px 0 0', fontSize: 12, color: 'var(--muted)' }}>
            AgentTeams 保留协同任务事实源；OpsKeeper 保留执行与证据事实源。
          </p>
        </div>
        <button
          type="button"
          onClick={refresh}
          disabled={loading}
          style={{
            marginLeft: 'auto', padding: '5px 12px', borderRadius: 6, fontSize: 12,
            border: '1px solid var(--border)', background: 'var(--card)', cursor: loading ? 'wait' : 'pointer',
          }}
        >
          {loading ? '刷新中…' : '刷新'}
        </button>
      </header>

      <section style={{ display: 'grid', gridTemplateColumns: 'repeat(4, minmax(180px, 1fr))', gap: 12 }}>
        <SummaryCard
          label="服务状态"
          value={STATUS_LABELS[health?.status] || '未知'}
          hint={formatTime(health?.checkedAt)}
          color={STATUS_COLORS[health?.status || 'unknown']}
        />
        <SummaryCard label="活跃事故" value={snapshot?.activeIncidentCount ?? '—'} hint={`累计 ${snapshot?.totalIncidentCount ?? '—'}`} />
        <SummaryCard
          label="平均定位"
          value={formatSeconds(metrics?.meanLocalizationSeconds)}
          hint={`${metrics?.incidentCount ?? '—'} 个事故`}
        />
        <SummaryCard
          label="审计完整度"
          value={formatPercent(metrics?.auditEvidenceCompleteness)}
          hint={`${metrics?.completeAuditEventCount ?? '—'}/${metrics?.auditRequiredEventCount ?? '—'} 事件`}
        />
      </section>

      <section style={{ display: 'grid', gridTemplateColumns: 'minmax(320px, 2fr) minmax(280px, 1fr)', gap: 12, marginTop: 16 }}>
        <Panel title="依赖检查">
          {Object.entries(groupedChecks).map(([group, checks]) => (
            <div key={group} style={{ marginBottom: 14 }}>
              <div style={{ fontSize: 11, color: 'var(--muted)', marginBottom: 6 }}>{group}</div>
              {checks.map((check) => (
                <div
                  key={check.id || check.label}
                  style={{
                    display: 'flex', alignItems: 'center', gap: 8, padding: '7px 0',
                    borderBottom: '1px solid var(--border)', fontSize: 12,
                  }}
                >
                  <span style={{ width: 8, height: 8, borderRadius: '50%', flex: '0 0 auto', background: STATUS_COLORS[check.status] || STATUS_COLORS.unknown }} />
                  <span>{check.label}</span>
                  <span style={{ marginLeft: 'auto', color: 'var(--muted)', fontSize: 11 }}>
                    {check.durationMs == null ? '' : `${check.durationMs}ms`}
                  </span>
                </div>
              ))}
            </div>
          ))}
          {!health && <EmptyState text={errors.health || '暂无健康数据'} />}
        </Panel>

        <div style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
          <Panel title="运行指标">
            <MetricRow label="错误闭环" value={metrics?.wrongClosureCount ?? '—'} />
            <MetricRow label="重复动作" value={metrics?.repeatedActionCount ?? '—'} />
            <MetricRow
              label="建议成功率"
              value={formatPercent(metrics?.recommendationSuccessRate)}
              hint={`${metrics?.recoveryConfirmedRecommendationCount ?? '—'}/${metrics?.approvedRecommendationCount ?? '—'}`}
            />
            <MetricRow label="Manager 版本" value={snapshot?.version?.managerVersion || '—'} />
          </Panel>

          <Panel title="最近事故">
            {(snapshot?.latestIncidents || []).map((incident) => (
              <button
                key={incident.id}
                type="button"
                onClick={() => api.dashboard.navigate('plugin-route:opskeeper-teamharness/home')}
                style={{
                  display: 'block', width: '100%', textAlign: 'left', padding: '7px 0', border: 0,
                  borderBottom: '1px solid var(--border)', background: 'transparent', cursor: 'pointer',
                  color: 'inherit', fontSize: 12,
                }}
              >
                <span>{incident.summary || incident.id || '未命名事故'}</span>
                <span style={{ float: 'right', color: 'var(--muted)', fontSize: 11 }}>{incident.status || '—'}</span>
              </button>
            ))}
            {snapshot?.latestIncidents?.length === 0 && <EmptyState text="暂无事故" />}
            {errors.incidents && <EmptyState text={errors.incidents} />}
          </Panel>
        </div>
      </section>
    </div>
  );
}

function SummaryCard({ label, value, hint, color }) {
  return (
    <div style={{ padding: 14, borderRadius: 8, border: '1px solid var(--border)', background: 'var(--card)' }}>
      <div style={{ fontSize: 11, color: 'var(--muted)' }}>{label}</div>
      <div style={{ fontSize: 20, fontWeight: 600, marginTop: 6, color: color || 'inherit' }}>{value}</div>
      <div style={{ fontSize: 11, color: 'var(--muted)', marginTop: 4 }}>{hint}</div>
    </div>
  );
}

function Panel({ title, children }) {
  return (
    <div style={{ padding: 14, borderRadius: 8, border: '1px solid var(--border)', background: 'var(--card)', minWidth: 0 }}>
      <div style={{ fontSize: 12, color: 'var(--muted)', marginBottom: 8 }}>{title}</div>
      {children}
    </div>
  );
}

function MetricRow({ label, value, hint }) {
  return (
    <div style={{ display: 'flex', alignItems: 'center', gap: 8, padding: '7px 0', borderBottom: '1px solid var(--border)', fontSize: 12 }}>
      <span>{label}</span>
      <span style={{ marginLeft: 'auto', fontWeight: 600 }}>{value}</span>
      {hint && <span style={{ color: 'var(--muted)', fontSize: 11 }}>{hint}</span>}
    </div>
  );
}

function EmptyState({ text }) {
  return <div style={{ padding: 16, textAlign: 'center', fontSize: 12, color: 'var(--muted)' }}>{text}</div>;
}

function valueOf(result) {
  return result.status === 'fulfilled' ? result.value : null;
}

function incidentsOf(response) {
  return response?.incidents || response?.data || [];
}

function errorText(result) {
  return result.status === 'rejected' ? result.reason?.message || '读取失败' : null;
}

function groupBy(items, keyFn) {
  return items.reduce((groups, item) => {
    const key = keyFn(item);
    groups[key] = groups[key] || [];
    groups[key].push(item);
    return groups;
  }, {});
}

function formatTime(value) {
  if (!value) return '未采集';
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? value : date.toLocaleString();
}

function formatSeconds(value) {
  if (value == null) return '—';
  if (value < 60) return `${value.toFixed(0)}s`;
  if (value < 3600) return `${(value / 60).toFixed(1)}m`;
  return `${(value / 3600).toFixed(1)}h`;
}

function formatPercent(value) {
  return value == null ? '—' : `${(value * 100).toFixed(0)}%`;
}
