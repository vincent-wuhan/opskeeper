import * as React from 'react';
import { opskeeperApi } from './api.js';
import {
  normalizeArchiveIncidentList,
  normalizeArchiveResponse,
  normalizeIncidentSummary,
} from './archive.js';
import { formatBeijingTime as formatTime } from './time-format.js';

const PREVIEW_URL = 'https://opskeeper.yueming.xin/preview/';
const summaryGridStyle = {
  display: 'grid',
  gridTemplateColumns: 'repeat(auto-fit, minmax(min(100%, 180px), 1fr))',
  gap: 12,
  marginBottom: 12,
};
const contentGridStyle = {
  display: 'grid',
  gridTemplateColumns: 'repeat(auto-fit, minmax(min(100%, 360px), 1fr))',
  gap: 12,
};
const wrapAnywhereStyle = {
  minWidth: 0,
  overflowWrap: 'anywhere',
  wordBreak: 'break-word',
};

export default function OpskeeperArchiveRoute({ api }) {
  const [incidents, setIncidents] = React.useState([]);
  const [incidentId, setIncidentId] = React.useState('');
  const [archive, setArchive] = React.useState(null);
  const [incidentSummary, setIncidentSummary] = React.useState(null);
  const [loadingIncidents, setLoadingIncidents] = React.useState(true);
  const [loadingArchive, setLoadingArchive] = React.useState(false);
  const [incidentError, setIncidentError] = React.useState('');
  const [archiveError, setArchiveError] = React.useState('');

  const loadIncidents = React.useCallback(async () => {
    setLoadingIncidents(true);
    try {
      // 演示/决赛场景需要看到最近触发的事故（含尚未走完 RCA 闭环的），
      // 所以从 /v1/incidents 拉取全量；闭环档案由 /incidents/<id>/archive 二次拉取。
      const items = normalizeArchiveIncidentList(await opskeeperApi.listIncidents({ limit: 100 }));
      setIncidents(items);
      setIncidentError('');
      setIncidentId((current) => current || items[0]?.id || '');
    } catch (error) {
      setIncidents([]);
      setIncidentError(error?.message || '事故列表读取失败');
    } finally {
      setLoadingIncidents(false);
    }
  }, []);

  const loadArchive = React.useCallback(async (selectedIncidentId) => {
    const targetIncidentId = String(selectedIncidentId ?? incidentId ?? '').trim();
    if (!targetIncidentId) {
      setArchiveError('请输入或选择事故 ID');
      setIncidentSummary(null);
      return;
    }
    setLoadingArchive(true);
    setIncidentSummary(null);
    try {
      setArchive(normalizeArchiveResponse(await opskeeperApi.getIncidentArchive(targetIncidentId)));
      setArchiveError('');
    } catch (error) {
      setArchive(null);
      if (error?.status === 404) {
        // 闭环档案还没生成（告警刚触发或 RCA 未走到闭环），回退到 incident 详情
        // 让页面至少能展示 alert/rule/label，避免 demo 时一片空白。
        try {
          const summary = normalizeIncidentSummary(
            await opskeeperApi.getIncident(targetIncidentId),
          );
          setIncidentSummary(summary);
          setArchiveError('该事故尚未生成闭环档案；以下为告警触发时刻的事实快照，RCA 闭环后会写入档案。');
        } catch (innerError) {
          setArchiveError(innerError?.message || error?.message || '事故档案读取失败');
        }
      } else {
        setArchiveError(error?.message || '事故档案读取失败');
      }
    } finally {
      setLoadingArchive(false);
    }
  }, [incidentId]);

  React.useEffect(() => {
    loadIncidents();
  }, [loadIncidents]);

  const requiredEventTypes = archive?.required_event_types || [];
  const missingEventTypes = new Set(archive?.missing_event_types || []);
  const timeline = React.useMemo(
    () => [...(archive?.timeline || [])].sort((left, right) => new Date(left.occurred_at) - new Date(right.occurred_at)),
    [archive],
  );

  return (
    <div style={{ padding: 24, minWidth: 0, color: 'var(--card-foreground)' }}>
      <header style={{ display: 'flex', flexWrap: 'wrap', alignItems: 'center', gap: 12, marginBottom: 18 }}>
        <div style={{ minWidth: 0, flex: '1 1 280px' }}>
          <h2 style={{ margin: 0, fontSize: 18 }}>OpsKeeper 事故档案</h2>
          <p style={{ margin: '4px 0 0', fontSize: 12, color: 'var(--muted-foreground)' }}>
            Manager 保留权威证据与权限；插件仅做只读回看，不复制控制面事实源。
          </p>
        </div>
        <button
          type="button"
          onClick={() => { loadIncidents(); if (incidentId) loadArchive(incidentId); }}
          disabled={loadingIncidents || loadingArchive}
          style={buttonStyle(loadingIncidents || loadingArchive)}
        >
          {loadingIncidents || loadingArchive ? '刷新中…' : '刷新'}
        </button>
      </header>

      <Panel>
        <form
          onSubmit={(event) => { event.preventDefault(); loadArchive(incidentId); }}
          style={{ display: 'flex', flexWrap: 'wrap', gap: 8, alignItems: 'center', minWidth: 0 }}
        >
          <select
            value={incidents.some((incident) => incident.id === incidentId) ? incidentId : ''}
            onChange={(event) => setIncidentId(event.target.value)}
            disabled={loadingIncidents || incidents.length === 0}
            style={inputStyle()}
          >
            <option value="">{loadingIncidents ? '加载事故中…' : incidents.length ? '选择事故（闭环优先）' : '暂无可回看事故'}</option>
            {incidents.map((incident) => (
              <option key={incident.id} value={incident.id}>
                {`${incident.summary} · ${incident.eventCount}事件 · ${incident.status}${incident.evidenceComplete ? ' · 档案完整' : ''}`}
              </option>
            ))}
          </select>
          <input
            value={incidentId}
            onChange={(event) => setIncidentId(event.target.value)}
            placeholder="事故 ID"
            style={{ ...inputStyle, minWidth: 260 }}
          />
          <button type="submit" disabled={loadingArchive} style={buttonStyle(loadingArchive)}>
            {loadingArchive ? '查询中…' : '查询档案'}
          </button>
        </form>
        {incidentError && <ErrorState text={incidentError} />}
        {archiveError && <ErrorState text={archiveError} />}
      </Panel>

      {archive && (
        <>
          <section style={summaryGridStyle}>
            <SummaryCard label="证据完整性" value={archive.evidence_complete ? '完整' : '缺失'} hint={`${archive.event_count || 0} 条事件`} color={archive.evidence_complete ? '#16a34a' : '#dc2626'} />
            <SummaryCard label="恢复确认" value={archive.recovery_observed ? '已观测' : '未观测'} color={archive.recovery_observed ? '#16a34a' : '#f59e0b'} />
            <SummaryCard label="定位耗时" value={formatSeconds(archive.localization_seconds)} hint="告警 → 根因" />
            <SummaryCard label="恢复耗时" value={formatSeconds(archive.recovery_seconds)} hint="执行 → 恢复" />
          </section>

          <section style={contentGridStyle}>
            <Panel title="反向证据链">
              <div style={{ display: 'flex', flexWrap: 'wrap', gap: 6, marginBottom: 12 }}>
                {requiredEventTypes.map((eventType) => (
                  <span key={eventType} style={stageChipStyle(missingEventTypes.has(eventType))}>
                    {eventType}
                  </span>
                ))}
              </div>
              {timeline.map((event) => (
                <div key={event.id || `${event.event_type}-${event.occurred_at}`} style={eventRowStyle()}>
                  <div style={{ flex: '1 1 220px', ...wrapAnywhereStyle }}>
                    <div style={{ fontSize: 12, fontWeight: 600 }}>{event.event_type}</div>
                    <div style={{ marginTop: 3, fontSize: 11, color: 'var(--muted-foreground)' }}>
                      {event.phase || '未记录阶段'} · {event.actor_type || 'unknown'} / {event.actor || 'unknown'} · {event.status || 'unknown'}
                    </div>
                  </div>
                  <div style={{ flex: '1 1 180px', textAlign: 'right', fontSize: 11, color: 'var(--muted-foreground)', ...wrapAnywhereStyle }}>
                    <div>{formatTime(event.occurred_at)}</div>
                    {event.evidence_ref && <div style={{ marginTop: 3 }}>{event.evidence_ref}</div>}
                    {event.trace_id && <div style={{ marginTop: 3 }}>trace: {event.trace_id}</div>}
                  </div>
                </div>
              ))}
              {timeline.length === 0 && <EmptyState text="暂无事件" />}
            </Panel>

            <div style={{ display: 'flex', flexDirection: 'column', gap: 12, minWidth: 0 }}>
              <Panel title="闭环状态">
                <MetricRow label="事故已关闭" value={archive.closed ? '是' : '否'} />
                <MetricRow label="Trace" value={archive.trace_ids?.length || 0} />
                <MetricRow label="缺失事件" value={archive.missing_event_types?.length || 0} />
                <MetricRow label="最近事件" value={formatTime(archive.last_event_at)} />
              </Panel>

              <Panel title="同类历史事故">
                {archive.similar_incidents?.map((incident) => (
                  <button key={incident.incident_id} type="button" onClick={() => { setIncidentId(incident.incident_id); loadArchive(incident.incident_id); }} style={linkRowStyle()}>
                    <span>{incident.incident_id}</span>
                    <span style={{ color: incident.closed ? '#16a34a' : 'var(--muted)' }}>{incident.closed ? '已关闭' : '未关闭'}</span>
                  </button>
                ))}
                {archive.similar_incidents?.length === 0 && <EmptyState text="暂无可反查历史事故" />}
              </Panel>

              <Panel title="复盘与修复预演">
                {archive.postmortem_refs?.map((reference) => (
                  <div key={reference.id || reference.incident_id} style={{ padding: '7px 0', borderBottom: '1px solid var(--border)', fontSize: 12 }}>
                    <div>{reference.root_cause || '未记录根因'}</div>
                    <div style={{ marginTop: 3, fontSize: 11, color: 'var(--muted-foreground)' }}>
                      {reference.confirmed_by || 'unknown'} · {formatTime(reference.confirmed_at)}
                    </div>
                  </div>
                ))}
                {archive.postmortem_refs?.length === 0 && <EmptyState text="暂无复盘引用" />}
                <div style={{ marginTop: 8, fontSize: 11, color: 'var(--muted-foreground)' }}>
                  完整候选修复对比见下方 Manager 权威档案投影。
                </div>
              </Panel>
            </div>
          </section>

          <RepairPreviewArchive runs={archive.repair_previews} incidentId={archive.incident_id} />
        </>
      )}

      {!archive && !incidentSummary && !archiveError && !loadingArchive && (
        <Panel>
          <EmptyState text="选择或输入事故 ID 后查询证据档案" />
        </Panel>
      )}

      {!archive && incidentSummary && (
        <>
          <section style={summaryGridStyle}>
            <SummaryCard label="告警状态" value={incidentSummary.status} color={incidentSummary.status === 'resolved' ? '#16a34a' : '#f59e0b'} />
            <SummaryCard label="事件数量" value={String(incidentSummary.eventCount)} hint="已落库" />
            <SummaryCard label="规则" value={incidentSummary.ruleKey || '—'} hint={incidentSummary.ruleName} />
            <SummaryCard label="触发时间" value={formatTime(incidentSummary.firedAt)} hint={formatTime(incidentSummary.resolvedAt) !== '未采集' ? `恢复：${formatTime(incidentSummary.resolvedAt)}` : '尚未恢复'} />
          </section>

          <section style={contentGridStyle}>
            <Panel title="告警事实快照">
              <div style={{ fontSize: 13, fontWeight: 600, marginBottom: 8 }}>{incidentSummary.summary || '未提供 summary'}</div>
              <MetricRow label="严重级别" value={incidentSummary.severity || '—'} />
              <MetricRow label="Dedupe Key" value={incidentSummary.dedupeKey || '—'} />
              <MetricRow label="Target Type" value={incidentSummary.targetType || '—'} />
              <MetricRow label="指标值" value={incidentSummary.value ?? '—'} />
              {Object.keys(incidentSummary.labels || {}).length > 0 && (
                <div style={{ marginTop: 10 }}>
                  <div style={{ fontSize: 11, color: 'var(--muted-foreground)', marginBottom: 4 }}>Labels</div>
                  <div style={{ display: 'flex', flexWrap: 'wrap', gap: 6 }}>
                    {Object.entries(incidentSummary.labels).map(([key, value]) => (
                      <span key={key} style={stageChipStyle(false)}>{key}={String(value)}</span>
                    ))}
                  </div>
                </div>
              )}
            </Panel>

            <Panel title="后续动作">
              <div style={{ fontSize: 12, color: 'var(--muted-foreground)' }}>
                闭环档案（包含 alert/根因/修复/恢复 7 阶段事件）由 Manager 在 RCA 闭环后写入；当前展示的是告警事实快照，不复制控制面数据。
              </div>
              <a href={PREVIEW_URL} target="_blank" rel="noreferrer" style={{ display: 'inline-block', marginTop: 10, fontSize: 12 }}>
                打开 preview-pg 修复对比 →
              </a>
            </Panel>
          </section>
        </>
      )}
    </div>
  );
}

function RepairPreviewArchive({ runs, incidentId }) {
  const previews = runs || [];

  return (
    <section
      aria-label="候选修复完整对比"
      style={{
        padding: 14, marginBottom: 12, borderRadius: 8,
        border: '1px solid var(--border)', background: 'var(--card)',
        minWidth: 0, overflow: 'hidden',
      }}
    >
      <div style={{ display: 'flex', flexWrap: 'wrap', gap: 8, alignItems: 'center', marginBottom: 10 }}>
        <div style={{ fontSize: 13, fontWeight: 600 }}>候选修复完整对比</div>
        <span style={{ fontSize: 11, color: 'var(--muted-foreground)' }}>
          Manager Archive 权威数据 · 只读投影
        </span>
        <a
          href={PREVIEW_URL}
          target="_blank"
          rel="noreferrer"
          style={{ marginLeft: 'auto', fontSize: 11, color: 'var(--primary)', overflowWrap: 'anywhere' }}
        >
          辅助深链：preview-pg →
        </a>
      </div>
      {previews.length === 0 && (
        <EmptyState text={`事故 ${incidentId || '未知'} 暂无修复预演记录（历史事故可能未启用候选修复预演）`} />
      )}
      {previews.map((run) => (
        <div
          key={run.run_id || run.id}
          style={{
            minWidth: 0, padding: 12, marginBottom: 12,
            border: '1px solid var(--border)', borderRadius: 8, background: 'var(--background)',
          }}
        >
          <div style={{ display: 'flex', flexWrap: 'wrap', gap: 8, marginBottom: 8, fontSize: 11 }}>
            <span style={{ fontWeight: 600, overflowWrap: 'anywhere' }}>Run：{run.run_id || run.id}</span>
            <span style={{ color: 'var(--muted-foreground)', overflowWrap: 'anywhere' }}>
              Workload fingerprint：{run.workload_fingerprint || run.workloadFingerprint || '未记录'}
            </span>
            <span style={{ color: 'var(--muted-foreground)', overflowWrap: 'anywhere' }}>
              Seed：{run.seed_fingerprint || run.seedFingerprint || '未记录'}
            </span>
          </div>
          <div style={{ fontSize: 11, color: 'var(--muted-foreground)', marginBottom: 8, overflowWrap: 'anywhere' }}>
            {run.isolation_boundary || run.isolationBoundary || 'Controlled fixed-workload reconstruction in disposable preview-pg; original active sessions are not copied.'}
          </div>
          <div style={{ minWidth: 0, overflowX: 'auto', WebkitOverflowScrolling: 'touch' }}>
            <table style={{ width: '100%', minWidth: 1160, tableLayout: 'fixed', borderCollapse: 'collapse', fontSize: 11 }}>
              <thead>
                <tr>
                  {['候选 / 动作', '一致性', '平均延迟', 'P95 延迟', '吞吐 TPS', '写入影响', '存储增量', '业务探针', '决策 / 原因'].map((label) => (
                    <th key={label} style={previewHeaderCellStyle()}>{label}</th>
                  ))}
                </tr>
              </thead>
              <tbody>
                {run.candidates.map((candidate, candidateIndex) => (
                  <tr key={candidate.id || candidate.candidate_id || candidateIndex}>
                    <td style={previewCellStyle('left')}>
                      <div style={{ fontWeight: 600, overflowWrap: 'anywhere' }}>
                        {candidate.name || candidate.candidate_id || '未知候选'}
                      </div>
                      <div style={{ marginTop: 3, overflowWrap: 'anywhere' }}>
                        {candidate.action || candidate.change_summary || '未记录动作'}
                      </div>
                    </td>
                    <td style={previewCellStyle()}>{formatBoolean(candidate.consistent)}</td>
                    <td style={previewCellStyle()}>{formatMilliseconds(candidate.average_latency_ms)}</td>
                    <td style={previewCellStyle()}>{formatMilliseconds(candidate.p95_latency_ms)}</td>
                    <td style={previewCellStyle()}>{formatNumber(candidate.tps)}</td>
                    <td style={previewCellStyle()}>{candidate.write_impact || '—'}</td>
                    <td style={previewCellStyle()}>{formatBytes(candidate.storage_delta_bytes)}</td>
                    <td style={previewCellStyle()}>{formatBoolean(candidate.business_probe_pass)}</td>
                    <td style={previewCellStyle('left')}>
                      <span style={previewDecisionStyle(candidate.decision)}>{candidate.decision || 'UNKNOWN'}</span>
                      {candidate.rejection_reason && (
                        <div style={{ marginTop: 4, color: 'var(--muted-foreground)', overflowWrap: 'anywhere' }}>
                          {candidate.rejection_reason}
                        </div>
                      )}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          {run.candidates.length === 0 && <EmptyState text="该预演 Run 未返回候选数据" />}
        </div>
      ))}
    </section>
  );
}

function Panel({ title, children }) {
  return (
    <div style={{ padding: 14, marginBottom: 12, borderRadius: 8, border: '1px solid var(--border)', background: 'var(--card)', minWidth: 0 }}>
      {title && <div style={{ fontSize: 12, color: 'var(--muted-foreground)', marginBottom: 8 }}>{title}</div>}
      {children}
    </div>
  );
}

function SummaryCard({ label, value, hint, color }) {
  return (
    <div style={{ padding: 14, borderRadius: 8, border: '1px solid var(--border)', background: 'var(--card)', minWidth: 0 }}>
      <div style={{ fontSize: 11, color: 'var(--muted-foreground)' }}>{label}</div>
      <div style={{ marginTop: 6, fontSize: 16, lineHeight: 1.35, fontWeight: 600, color: color || 'inherit', ...wrapAnywhereStyle }}>{value}</div>
      {hint && <div style={{ marginTop: 4, fontSize: 11, lineHeight: 1.35, color: 'var(--muted-foreground)', ...wrapAnywhereStyle }}>{hint}</div>}
    </div>
  );
}

function MetricRow({ label, value }) {
  return (
    <div style={{ display: 'flex', alignItems: 'flex-start', gap: 8, padding: '7px 0', borderBottom: '1px solid var(--border)', fontSize: 12, minWidth: 0 }}>
      <span style={{ flex: '0 0 auto' }}>{label}</span>
      <span style={{ marginLeft: 'auto', textAlign: 'right', fontWeight: 600, ...wrapAnywhereStyle }}>{value}</span>
    </div>
  );
}

function ErrorState({ text }) {
  return <div style={{ marginTop: 10, padding: 9, borderRadius: 6, background: 'rgba(220,38,38,.12)', color: '#dc2626', fontSize: 12 }}>{text}</div>;
}

function EmptyState({ text }) {
  return <div style={{ padding: 16, textAlign: 'center', fontSize: 12, color: 'var(--muted-foreground)' }}>{text}</div>;
}

function inputStyle() {
  return {
    padding: '6px 9px', borderRadius: 6, fontSize: 12, flex: '1 1 220px', minWidth: 'min(100%, 220px)',
    border: '1px solid var(--border)', background: 'var(--background)', color: 'inherit',
  };
}

function buttonStyle(disabled) {
  return {
    padding: '6px 12px', borderRadius: 6, fontSize: 12,
    border: '1px solid var(--border)', background: 'var(--card)', color: 'inherit',
    cursor: disabled ? 'wait' : 'pointer',
  };
}

function stageChipStyle(missing) {
  return {
    padding: '3px 8px', borderRadius: 999, fontSize: 11,
    maxWidth: '100%', overflowWrap: 'anywhere', wordBreak: 'break-word',
    border: `1px solid ${missing ? '#dc2626' : 'var(--border)'}`,
    color: missing ? '#dc2626' : 'inherit',
    background: missing ? 'rgba(220,38,38,.1)' : 'transparent',
  };
}

function eventRowStyle() {
  return {
    display: 'flex', flexWrap: 'wrap', justifyContent: 'space-between', gap: 12, minWidth: 0,
    padding: '9px 0', borderBottom: '1px solid var(--border)',
  };
}

function linkRowStyle() {
  return {
    display: 'flex', flexWrap: 'wrap', justifyContent: 'space-between', gap: 8, width: '100%', textAlign: 'left', minWidth: 0,
    padding: '7px 0', border: 0, borderBottom: '1px solid var(--border)',
    background: 'transparent', color: 'inherit', fontSize: 12, cursor: 'pointer',
  };
}

function previewHeaderCellStyle() {
  return {
    padding: '8px 7px', textAlign: 'left', fontWeight: 600, whiteSpace: 'nowrap',
    borderBottom: '1px solid var(--border)', background: 'var(--card)',
  };
}

function previewCellStyle(alignment = 'right') {
  return {
    padding: '8px 7px', textAlign: alignment, verticalAlign: 'top',
    overflowWrap: 'anywhere', minWidth: 88, maxWidth: 180,
    borderBottom: '1px solid var(--border)',
  };
}

function previewDecisionStyle(decision) {
  const normalized = String(decision || '').toUpperCase();
  const color = normalized === 'PASS' ? '#16a34a'
    : normalized === 'REJECTED_BY_PREVIEW' ? '#dc2626'
      : normalized === 'FAIL' ? '#d97706' : 'inherit';
  return {
    display: 'inline-block', padding: '2px 7px', borderRadius: 999,
    fontSize: 10, fontWeight: 700, color,
    border: `1px solid ${color === 'inherit' ? 'var(--border)' : color}`,
    whiteSpace: 'nowrap',
  };
}

function formatBoolean(value) {
  if (typeof value !== 'boolean') return '—';
  return value ? 'PASS' : 'FAIL';
}

function formatMilliseconds(value) {
  return typeof value === 'number' && Number.isFinite(value) ? `${value.toFixed(1)} ms` : '—';
}

function formatNumber(value) {
  return typeof value === 'number' && Number.isFinite(value) ? value.toFixed(2) : '—';
}

function formatBytes(value) {
  if (typeof value !== 'number' || !Number.isFinite(value)) return '—';
  if (value < 1024) return `${value} B`;
  if (value < 1024 * 1024) return `${(value / 1024).toFixed(1)} KiB`;
  return `${(value / (1024 * 1024)).toFixed(1)} MiB`;
}
function formatSeconds(value) {
  if (value == null) return '—';
  if (value < 60) return `${value.toFixed(0)}s`;
  if (value < 3600) return `${(value / 60).toFixed(1)}m`;
  return `${(value / 3600).toFixed(1)}h`;
}
