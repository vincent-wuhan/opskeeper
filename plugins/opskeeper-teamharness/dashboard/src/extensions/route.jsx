import * as React from 'react';
import { opskeeperApi } from './api.js';

// 7 阶段 RCA orchestrator 阶段定义（来自 opskeeper 7 阶段 RCA loop）
const STAGES = [
  { id: 'collect', name: '告警采集', emoji: '1️⃣' },
  { id: 'correlate', name: '关联聚类', emoji: '2️⃣' },
  { id: 'investigate', name: '深度调查', emoji: '3️⃣' },
  { id: 'critic', name: '自批评审', emoji: '4️⃣' },
  { id: 'reviewer', name: '修复提案', emoji: '5️⃣' },
  { id: 'repairer', name: '执行修复', emoji: '6️⃣' },
  { id: 'verifier', name: '效果验证', emoji: '7️⃣' },
];

const SEVERITY_COLORS = {
  critical: '#ef4444',
  high: '#f59e0b',
  medium: '#eab308',
  low: '#10b981',
  info: '#6b7280',
};

function severityTone(s) {
  return SEVERITY_COLORS[(s || 'info').toLowerCase()] || SEVERITY_COLORS.info;
}

function StatusBadge({ status }) {
  const isOpen = !status || status === 'open' || status === 'in_progress';
  const color = isOpen ? '#ef4444' : '#10b981';
  return (
    <span style={{
      padding: '2px 8px', borderRadius: 4, fontSize: 11, fontWeight: 500,
      background: color, color: '#fff',
    }}>
      {status || 'open'}
    </span>
  );
}

function StageRow({ stage, idx, total }) {
  const passed = idx < total;
  const current = idx === total;
  return (
    <div style={{
      display: 'flex', alignItems: 'center', gap: 8, padding: '6px 0',
      opacity: passed ? 1 : 0.4,
    }}>
      <span style={{ fontSize: 16, width: 28, textAlign: 'center' }}>
        {passed ? '✅' : current ? '⏳' : '⚪'}
      </span>
      <span style={{ fontSize: 12, flex: 1 }}>{stage.emoji} {stage.name}</span>
      {current && <span style={{ fontSize: 10, color: '#f59e0b' }}>进行中</span>}
    </div>
  );
}

function PhaseProgress({ phase }) {
  const total = STAGES.length;
  const idx = STAGES.findIndex((s) => s.id === phase);
  return (
    <div style={{ marginTop: 8 }}>
      {STAGES.map((s, i) => (
        <StageRow key={s.id} stage={s} idx={i} total={idx >= 0 ? idx : 0} />
      ))}
    </div>
  );
}

function ReportViewer({ report }) {
  if (!report) return null;
  const root = report.root_cause || report.rootCause || {};
  const chain = report.causal_chain || report.causalChain || [];
  const evidence = report.evidence || [];
  const confidence = report.confidence ?? report.confidence_score ?? null;

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
      {/* Root cause */}
      <div style={{
        padding: 14, borderRadius: 8, border: '1px solid var(--border)',
        background: 'var(--card)', color: 'var(--card-foreground)',
      }}>
        <div style={{ fontSize: 12, color: 'var(--muted)', marginBottom: 4 }}>根因</div>
        <div style={{ fontSize: 14, fontWeight: 600 }}>
          {root.summary || root.description || report.summary || '—'}
        </div>
        {root.entity && (
          <div style={{ fontSize: 12, color: 'var(--muted)', marginTop: 4 }}>
            实体：{root.entity.type} = {root.entity.id}
          </div>
        )}
        {confidence !== null && (
          <div style={{ fontSize: 12, color: 'var(--muted)', marginTop: 4 }}>
            置信度：<strong style={{ color: confidence >= 0.7 ? '#10b981' : '#f59e0b' }}>
              {(confidence * 100).toFixed(0)}%
            </strong>
          </div>
        )}
      </div>

      {/* Causal chain */}
      {chain.length > 0 && (
        <div style={{
          padding: 14, borderRadius: 8, border: '1px solid var(--border)',
          background: 'var(--card)',
        }}>
          <div style={{ fontSize: 12, color: 'var(--muted)', marginBottom: 8 }}>因果链</div>
          <div style={{ display: 'flex', flexDirection: 'column', gap: 6 }}>
            {chain.map((step, i) => (
              <div key={i} style={{ display: 'flex', alignItems: 'center', gap: 8, fontSize: 12 }}>
                <span style={{
                  display: 'inline-flex', alignItems: 'center', justifyContent: 'center',
                  width: 22, height: 22, borderRadius: '50%',
                  background: 'var(--muted)', color: 'var(--muted-foreground)',
                  fontSize: 11, fontWeight: 600,
                }}>{i + 1}</span>
                <span style={{ flex: 1 }}>{step.event || step.description}</span>
                {step.entity && (
                  <span style={{ fontSize: 10, color: 'var(--muted)' }}>
                    {step.entity.type}:{step.entity.id}
                  </span>
                )}
                {i < chain.length - 1 && (
                  <span style={{ color: 'var(--muted)', marginLeft: 4 }}>↓</span>
                )}
              </div>
            ))}
          </div>
        </div>
      )}

      {/* Evidence */}
      {evidence.length > 0 && (
        <div style={{
          padding: 14, borderRadius: 8, border: '1px solid var(--border)',
          background: 'var(--card)',
        }}>
          <div style={{ fontSize: 12, color: 'var(--muted)', marginBottom: 8 }}>
            证据 ({evidence.length} 条)
          </div>
          <ul style={{ margin: 0, paddingLeft: 18, fontSize: 12 }}>
            {evidence.map((e, i) => (
              <li key={i} style={{ marginBottom: 4 }}>
                <code style={{ fontSize: 11, color: 'var(--muted)' }}>{e.source || 'evidence'}</code>
                {e.signal && <span> — {e.signal}</span>}
                {e.timestamp && <span style={{ color: 'var(--muted)' }}> @ {e.timestamp}</span>}
              </li>
            ))}
          </ul>
        </div>
      )}

      {/* Phase progress */}
      {report.phase && (
        <div style={{
          padding: 14, borderRadius: 8, border: '1px solid var(--border)',
          background: 'var(--card)',
        }}>
          <div style={{ fontSize: 12, color: 'var(--muted)', marginBottom: 4 }}>
            7 阶段进度 — 当前阶段：<strong>{report.phase}</strong>
          </div>
          <PhaseProgress phase={report.phase} />
        </div>
      )}

      {/* Raw JSON fallback */}
      <details style={{ fontSize: 11, color: 'var(--muted)' }}>
        <summary style={{ cursor: 'pointer' }}>原始 JSON</summary>
        <pre style={{
          marginTop: 8, padding: 12, background: '#0a0a0a', color: '#eee',
          borderRadius: 6, overflow: 'auto', fontSize: 11,
        }}>
          {JSON.stringify(report, null, 2)}
        </pre>
      </details>
    </div>
  );
}

export default function OpskeeperRoute({ api }) {
  const [incidents, setIncidents] = React.useState([]);
  const [loadingIncidents, setLoadingIncidents] = React.useState(true);
  const [incidentsError, setIncidentsError] = React.useState(null);
  const [filter, setFilter] = React.useState('');
  const [selected, setSelected] = React.useState(null);
  const [report, setReport] = React.useState(null);
  const [running, setRunning] = React.useState(false);
  const [reportError, setReportError] = React.useState(null);

  const refresh = React.useCallback(async () => {
    setLoadingIncidents(true);
    setIncidentsError(null);
    try {
      const data = await opskeeperApi.listIncidents({ limit: 50 });
      setIncidents(data.incidents || data.data || []);
    } catch (e) {
      setIncidentsError(e.message);
      setIncidents([]);
    } finally {
      setLoadingIncidents(false);
    }
  }, []);

  React.useEffect(() => {
    refresh();
  }, [refresh]);

  async function triggerRCA(incident) {
    if (!incident) return;
    setSelected(incident);
    setRunning(true);
    setReportError(null);
    setReport(null);
    try {
      const r = await opskeeperApi.investigate({ incident_id: incident.id });
      const payload = r.data || r.report || r;
      setReport(payload);
      api.eventBus.emit('dashboard:rca-finished', payload);
      api.dashboard.toast(`RCA 完成：${incident.id}`, 'success');
    } catch (e) {
      setReportError(e.message);
      api.dashboard.toast(`RCA 失败：${e.message}`, 'error');
    } finally {
      setRunning(false);
    }
  }

  const filtered = incidents.filter((i) =>
    !filter || (i.id || '').includes(filter) || (i.summary || '').includes(filter)
  );

  return (
    <div style={{ padding: 24, display: 'flex', gap: 16, alignItems: 'flex-start' }}>
      {/* Left: incident list */}
      <div style={{ flex: '0 0 360px', display: 'flex', flexDirection: 'column', gap: 12 }}>
        <div style={{
          display: 'flex', alignItems: 'center', justifyContent: 'space-between',
        }}>
          <h2 style={{ margin: 0, fontSize: 18 }}>事故列表</h2>
          <button
            onClick={refresh}
            disabled={loadingIncidents}
            style={{
              padding: '4px 10px', fontSize: 12, borderRadius: 4,
              border: '1px solid var(--border)', background: 'var(--card)',
              color: 'var(--card-foreground)', cursor: loadingIncidents ? 'wait' : 'pointer',
            }}
          >
            {loadingIncidents ? '…' : '刷新'}
          </button>
        </div>

        <input
          type="text"
          placeholder="按 id 或 summary 过滤…"
          value={filter}
          onChange={(e) => setFilter(e.target.value)}
          style={{
            padding: '6px 10px', borderRadius: 6, fontSize: 12,
            border: '1px solid var(--border)', background: 'var(--card)',
            color: 'var(--card-foreground)',
          }}
        />

        {incidentsError && (
          <div style={{
            padding: 10, borderRadius: 6, fontSize: 12,
            background: 'rgba(220,38,38,0.1)', color: '#ef4444',
            border: '1px solid #ef4444',
          }}>
            错误：{incidentsError}
          </div>
        )}

        {!loadingIncidents && filtered.length === 0 && (
          <div style={{
            padding: 24, textAlign: 'center', fontSize: 12, color: 'var(--muted)',
            border: '1px dashed var(--border)', borderRadius: 6,
          }}>
            {incidents.length === 0
              ? '暂无事故 — 等待 alerter 派发'
              : '无匹配项'}
          </div>
        )}

        {filtered.map((i) => (
          <div
            key={i.id}
            onClick={() => triggerRCA(i)}
            style={{
              padding: 12, borderRadius: 6, fontSize: 12, cursor: 'pointer',
              border: '1px solid var(--border)',
              background: selected?.id === i.id ? 'var(--primary)' : 'var(--card)',
              color: selected?.id === i.id ? 'var(--primary-foreground)' : 'var(--card-foreground)',
              display: 'flex', flexDirection: 'column', gap: 4,
            }}
          >
            <div style={{ display: 'flex', alignItems: 'center', gap: 6 }}>
              <span style={{
                width: 8, height: 8, borderRadius: '50%',
                background: severityTone(i.severity),
              }} />
              <strong style={{ fontSize: 12 }}>{i.id}</strong>
              <StatusBadge status={i.status} />
              <span style={{ marginLeft: 'auto', fontSize: 10, opacity: 0.7 }}>
                {i.severity || '—'}
              </span>
            </div>
            <div style={{ fontSize: 11, opacity: 0.85 }}>
              {i.summary || '(no summary)'}
            </div>
            {i.started_at && (
              <div style={{ fontSize: 10, opacity: 0.6 }}>{i.started_at}</div>
            )}
          </div>
        ))}
      </div>

      {/* Right: RCA report */}
      <div style={{ flex: 1, minWidth: 0, display: 'flex', flexDirection: 'column', gap: 12 }}>
        <h2 style={{ margin: 0, fontSize: 18 }}>RCA 报告</h2>

        {!selected && (
          <div style={{
            padding: 32, textAlign: 'center', fontSize: 13, color: 'var(--muted)',
            border: '1px dashed var(--border)', borderRadius: 8,
          }}>
            ← 选择左侧事故触发 7 阶段 RCA
          </div>
        )}

        {selected && running && (
          <div style={{
            padding: 24, fontSize: 13, color: 'var(--muted)',
            display: 'flex', alignItems: 'center', gap: 8,
          }}>
            <span style={{
              display: 'inline-block', width: 14, height: 14,
              border: '2px solid var(--muted)', borderTopColor: 'transparent',
              borderRadius: '50%', animation: 'spin 0.8s linear infinite',
            }} />
            正在为 <code style={{ marginLeft: 4 }}>{selected.id}</code> 执行 7 阶段 RCA…
          </div>
        )}

        {selected && !running && reportError && (
          <div style={{
            padding: 16, borderRadius: 8, fontSize: 13,
            background: 'rgba(220,38,38,0.1)', color: '#ef4444',
            border: '1px solid #ef4444',
          }}>
            <strong>RCA 失败</strong>
            <div style={{ marginTop: 4, fontSize: 12 }}>{reportError}</div>
            <button
              onClick={() => triggerRCA(selected)}
              style={{
                marginTop: 8, padding: '4px 12px', fontSize: 12, borderRadius: 4,
                border: '1px solid #ef4444', background: 'transparent',
                color: '#ef4444', cursor: 'pointer',
              }}
            >
              重试
            </button>
          </div>
        )}

        {selected && !running && report && <ReportViewer report={report} />}
      </div>
    </div>
  );
}
