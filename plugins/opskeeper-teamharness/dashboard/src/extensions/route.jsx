import * as React from 'react';
import { opskeeperDarkPanelStyle, opskeeperPluginThemeStyle } from './plugin-theme.js';
import { normalizeIncidentList } from './runtime.js';
import { buildInvestigationRequest, opskeeperApi } from './api.js';
import { normalizeRepairPreviewSummary } from './archive.js';
import {
  extractKnowledgeFromReport,
  formatSimilarity,
  normalizeKBHitList,
  normalizePostmortemRefList,
} from './knowledge.js';

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

function normalizeRootReport(report) {
  if (!report || typeof report !== 'object') {
    return { root: {}, chain: [], evidence: [], confidence: null, embeddedHits: [], embeddedWrites: [] };
  }
  const obj = report.root_cause_object && typeof report.root_cause_object === 'object'
    ? report.root_cause_object
    : null;
  const root = obj || report.root_cause || report.rootCause || {};
  const chain = report.causal_chain || report.causalChain || (obj && Array.isArray(obj.causal_chain) ? obj.causal_chain : []);
  const evidence = report.evidence_chain || report.evidence || (obj && Array.isArray(obj.evidence_chain) ? obj.evidence_chain : []);
  const confidence = report.confidence ?? report.confidence_score
    ?? (obj && typeof obj.confidence === 'number' ? obj.confidence : null);
  const knowledge = extractKnowledgeFromReport(report);
  return {
    root,
    chain,
    evidence,
    confidence,
    embeddedHits: knowledge.hits,
    embeddedWrites: knowledge.writes,
  };
}

function KnowledgePanel({ report, incidentId, embeddedHits = [], embeddedWrites = [], rootSummary = '' }) {
  // 引用知识库：优先用 report 自带的 kb_hits / knowledge_refs，回退到独立查询。
  // 输出知识库：优先用 report 自带的 knowledge_writes / postmortem_refs，回退到 archive 端点。
  const [hits, setHits] = React.useState(() => (embeddedHits.length ? embeddedHits : null));
  const [writes, setWrites] = React.useState(() => (embeddedWrites.length ? embeddedWrites : null));
  const [hitsError, setHitsError] = React.useState(null);
  const [hitsNotice, setHitsNotice] = React.useState(null);
  const [writesError, setWritesError] = React.useState(null);
  const [hitsLoading, setHitsLoading] = React.useState(embeddedHits.length === 0);
  const [writesLoading, setWritesLoading] = React.useState(Boolean(incidentId) && embeddedWrites.length === 0);

  React.useEffect(() => {
    setHits(embeddedHits.length ? embeddedHits : null);
    setHitsError(null);
    setHitsNotice(null);
    setHitsLoading(embeddedHits.length === 0);
  }, [report, embeddedHits]);

  React.useEffect(() => {
    setWrites(embeddedWrites.length ? embeddedWrites : null);
    setWritesError(null);
    setWritesLoading(Boolean(incidentId) && embeddedWrites.length === 0);
  }, [incidentId, embeddedWrites]);

  React.useEffect(() => {
    if (embeddedHits.length > 0) return undefined;
    if (!rootSummary) {
      setHitsLoading(false);
      return undefined;
    }
    let cancelled = false;
    setHitsLoading(true);
    setHitsError(null);
    setHitsNotice(null);
    opskeeperApi.queryKnowledge({ query: rootSummary, top_k: 5 })
      .then((res) => {
        if (cancelled) return;
        const list = normalizeKBHitList(res);
        setHits(list);
      })
      .catch((e) => {
        if (cancelled) return;
        if (e?.status === 404) {
          setHits([]);
          setHitsNotice('知识库查询接口暂未开放；当前展示 RCA 证据链中的引用记录。');
          return;
        }
        setHitsError(e?.message || '引用知识库查询失败');
      })
      .finally(() => {
        if (!cancelled) setHitsLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [rootSummary, embeddedHits]);

  React.useEffect(() => {
    if (embeddedWrites.length > 0) return undefined;
    if (!incidentId) {
      setWritesLoading(false);
      return undefined;
    }
    let cancelled = false;
    setWritesLoading(true);
    setWritesError(null);
    opskeeperApi.getIncidentArchive(incidentId)
      .then((res) => {
        if (cancelled) return;
        const list = normalizePostmortemRefList(res);
        setWrites(list);
      })
      .catch((e) => {
        if (cancelled) return;
        if (e?.status === 404) {
          setWrites([]);
          return;
        }
        setWritesError(e?.message || '输出知识库查询失败');
      })
      .finally(() => {
        if (!cancelled) setWritesLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [incidentId, embeddedWrites]);

  const hitList = hits || [];
  const writeList = writes || [];

  return (
    <div style={{
      ...opskeeperDarkPanelStyle,
      padding: 14, borderRadius: 8, border: '1px solid var(--border)',
      display: 'flex', flexDirection: 'column', gap: 12,
    }}>
      <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
        <span style={{ fontSize: 13, fontWeight: 600 }}>📚 知识库贡献</span>
        <span style={{
          fontSize: 10, padding: '2px 8px', borderRadius: 999,
          background: 'rgba(99,102,241,0.18)', color: '#a5b4fc',
        }}>
          引用 {hitList.length} · 输出 {writeList.length}
        </span>
      </div>
      <div style={{
        fontSize: 11, color: 'var(--muted-foreground)',
        borderLeft: '3px solid #6366f1', padding: '4px 8px',
        background: 'rgba(99,102,241,0.06)', borderRadius: 4,
      }}>
        OpsKeeper 知识库（pgvector + BM25 双索引）记录每次调查命中的 incident_pattern，
        闭环后由 postmortem 阶段写入 vault。下方面板展示本次 RCA 的引用 / 输出。
      </div>

      {/* 引用知识库 */}
      <section>
        <div style={{
          fontSize: 12, color: 'var(--muted-foreground)',
          display: 'flex', alignItems: 'center', gap: 6, marginBottom: 6,
        }}>
          <span>🔗 引用知识库</span>
          {hitsLoading && <span style={{ fontSize: 10 }}>查询中…</span>}
        </div>
        {hitsError && (
          <div style={{
            padding: 8, fontSize: 11, color: '#f59e0b',
            background: 'rgba(245,158,11,0.08)', borderRadius: 4,
          }}>
            查询失败：{hitsError}
          </div>
        )}
        {hitsNotice && (
          <div style={{
            padding: 8, fontSize: 11, color: 'var(--muted-foreground)',
            background: 'rgba(99,102,241,0.06)', borderRadius: 4, marginBottom: 6,
          }}>
            {hitsNotice}
          </div>
        )}
        {!hitsLoading && !hitsError && hitList.length === 0 && (
          <div style={{
            padding: 10, fontSize: 12, color: 'var(--muted-foreground)',
            border: '1px dashed var(--border)', borderRadius: 4,
          }}>
            暂未命中既有 incident_pattern；本次 RCA 将作为新样本由 postmortem 阶段回填。
          </div>
        )}
        {hitList.length > 0 && (
          <ul style={{ margin: 0, padding: 0, listStyle: 'none', display: 'flex', flexDirection: 'column', gap: 6 }}>
            {hitList.map((hit) => (
              <li key={hit.id} style={{
                padding: 8, borderRadius: 4, fontSize: 12,
                border: '1px solid var(--border)',
                background: 'rgba(99,102,241,0.06)',
                display: 'flex', flexDirection: 'column', gap: 4,
              }}>
                <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
                  <code style={{ fontSize: 10, color: '#a5b4fc' }}>{hit.source}</code>
                  <strong style={{ flex: 1 }}>{hit.summary}</strong>
                  <span style={{
                    fontSize: 10, padding: '2px 6px', borderRadius: 999,
                    background: 'rgba(99,102,241,0.18)', color: '#a5b4fc',
                  }}>
                    相似度 {formatSimilarity(hit.similarity)}
                  </span>
                </div>
                {(hit.symptom || hit.rootCause) && (
                  <div style={{ fontSize: 11, color: 'var(--muted-foreground)' }}>
                    {hit.symptom && <span>症状：{hit.symptom}</span>}
                    {hit.symptom && hit.rootCause && <span> · </span>}
                    {hit.rootCause && <span>根因：{hit.rootCause}</span>}
                  </div>
                )}
                {(hit.hitCount > 0 || hit.postmortemId) && (
                  <div style={{ fontSize: 10, color: 'var(--muted-foreground)' }}>
                    历史命中 {hit.hitCount} 次
                    {hit.postmortemId && <span> · 关联复盘 {hit.postmortemId}</span>}
                  </div>
                )}
              </li>
            ))}
          </ul>
        )}
      </section>

      {/* 输出知识库 */}
      <section>
        <div style={{
          fontSize: 12, color: 'var(--muted-foreground)',
          display: 'flex', alignItems: 'center', gap: 6, marginBottom: 6,
        }}>
          <span>📤 输出知识库</span>
          {writesLoading && <span style={{ fontSize: 10 }}>查询中…</span>}
        </div>
        {writesError && (
          <div style={{
            padding: 8, fontSize: 11, color: '#f59e0b',
            background: 'rgba(245,158,11,0.08)', borderRadius: 4,
          }}>
            查询失败：{writesError}
          </div>
        )}
        {!writesLoading && !writesError && writeList.length === 0 && (
          <div style={{
            padding: 10, fontSize: 12, color: 'var(--muted-foreground)',
            border: '1px dashed var(--border)', borderRadius: 4,
          }}>
            尚无知识库输出。等待修复验证 → postmortem 阶段会自动写入本次事故的复盘文档。
          </div>
        )}
        {writeList.length > 0 && (
          <ul style={{ margin: 0, padding: 0, listStyle: 'none', display: 'flex', flexDirection: 'column', gap: 6 }}>
            {writeList.map((ref) => (
              <li key={ref.id} style={{
                padding: 8, borderRadius: 4, fontSize: 12,
                border: '1px solid var(--border)',
                background: 'rgba(16,185,129,0.06)',
                display: 'flex', flexDirection: 'column', gap: 4,
              }}>
                <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
                  <code style={{ fontSize: 10, color: '#10b981' }}>vault:postmortem</code>
                  <strong style={{ flex: 1 }}>{ref.rootCause}</strong>
                </div>
                <div style={{ fontSize: 10, color: 'var(--muted-foreground)' }}>
                  {ref.confirmedBy && <span>由 {ref.confirmedBy} 写入</span>}
                  {ref.confirmedBy && ref.confirmedAt && <span> · </span>}
                  {ref.confirmedAt && <span>{ref.confirmedAt}</span>}
                </div>
              </li>
            ))}
          </ul>
        )}
      </section>
    </div>
  );
}

function RepairPreviewGate({ incidentId }) {
  const [summary, setSummary] = React.useState(null);
  const [loading, setLoading] = React.useState(true);
  const [error, setError] = React.useState(null);

  React.useEffect(() => {
    let cancelled = false;
    setLoading(true);
    setError(null);
    setSummary(null);
    opskeeperApi.getIncidentRepairPreviewSummary(incidentId)
      .then((response) => {
        if (!cancelled) setSummary(normalizeRepairPreviewSummary(response));
      })
      .catch((requestError) => {
        if (!cancelled) setError(requestError?.message || '修复预演摘要读取失败');
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => { cancelled = true; };
  }, [incidentId]);

  const boundary = summary?.isolationBoundary
    || 'Controlled fixed-workload reconstruction in disposable preview-pg; original active sessions are not copied.';

  return (
    <section
      aria-label="修复预演审批门禁"
      style={{
        ...opskeeperDarkPanelStyle,
        padding: 14, borderRadius: 8, border: '1px solid var(--border)', minWidth: 0,
      }}
    >
      <div style={{ display: 'flex', flexWrap: 'wrap', gap: 8, alignItems: 'center', marginBottom: 10 }}>
        <div style={{ fontSize: 12, color: 'var(--muted-foreground)' }}>修复预演审批门禁</div>
        {loading && <span style={{ fontSize: 11 }}>读取中…</span>}
        {!loading && summary?.runId && (
          <span style={{ fontSize: 11, color: 'var(--muted-foreground)', overflowWrap: 'anywhere' }}>
            Run：{summary.runId}
          </span>
        )}
      </div>

      {error && (
        <div style={{ padding: 10, fontSize: 11, color: '#f59e0b', background: 'rgba(245,158,11,.1)', borderRadius: 6, overflowWrap: 'anywhere' }}>
          修复预演摘要暂不可用：{error}
        </div>
      )}
      {!error && !loading && !summary?.runId && (
        <div style={{ padding: 10, fontSize: 12, color: 'var(--muted-foreground)', border: '1px dashed var(--border)', borderRadius: 6 }}>
          暂无修复预演摘要；需要先完成候选修复实测，才可进入人工审批。
        </div>
      )}

      {summary?.runId && (
        <>
          <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(min(100%, 220px), 1fr))', gap: 8 }}>
            <PreviewMetricCard
              title="Baseline 重放"
              decision={summary.baseline?.decision || 'PASS'}
              primary={formatPreviewLatency(summary.baseline?.average_latency_ms)}
              secondary={summary.controlledLoad ? '受控固定负载' : '负载标记缺失'}
            />
            <PreviewMetricCard
              title="Candidate A"
              decision={summary.passing?.decision || 'PASS'}
              primary={formatPreviewLatency(summary.passing?.average_latency_ms)}
              secondary="PASS / eligible for human approval"
              pass
            />
            <PreviewMetricCard
              title="Candidate B"
              decision={summary.rejected?.decision || 'FAIL'}
              primary={formatPreviewLatency(summary.rejected?.average_latency_ms ?? summary.rejected?.averageLatencyMs)}
              secondary={summary.rejected?.rejection_reason || 'blocked before human approval'}
              fail
            />
          </div>
          <div style={{ marginTop: 8, fontSize: 11, color: 'var(--muted-foreground)', overflowWrap: 'anywhere' }}>
            Workload fingerprint：<span style={{ color: 'var(--foreground)' }}>{summary.workloadFingerprint || '未记录'}</span>
          </div>
          <div style={{ marginTop: 4, fontSize: 11, color: 'var(--muted-foreground)', overflowWrap: 'anywhere' }}>{boundary}</div>
          <div style={{ marginTop: 6, fontSize: 11, color: 'var(--muted-foreground)' }}>
            PASS 仅代表预演资格通过，人工审批前不改变生产数据。
          </div>
        </>
      )}
    </section>
  );
}

function PreviewMetricCard({ title, decision, primary, secondary, pass = false, fail = false }) {
  const color = pass ? '#10b981' : fail ? '#ef4444' : 'var(--foreground)';
  return (
    <div style={{
      minWidth: 0, padding: 10, borderRadius: 6,
      border: `1px solid ${pass ? 'rgba(16,185,129,.45)' : fail ? 'rgba(239,68,68,.45)' : 'var(--border)'}`,
      background: pass ? 'rgba(16,185,129,.08)' : fail ? 'rgba(239,68,68,.08)' : 'rgba(31,41,55,.28)',
    }}>
      <div style={{ display: 'flex', alignItems: 'center', gap: 6, minWidth: 0 }}>
        <strong style={{ fontSize: 11 }}>{title}</strong>
        <span style={{ marginLeft: 'auto', fontSize: 10, fontWeight: 700, color, whiteSpace: 'nowrap' }}>{decision}</span>
      </div>
      <div style={{ marginTop: 6, fontSize: 16, fontWeight: 700 }}>{primary}</div>
      <div style={{ marginTop: 3, fontSize: 10, color: 'var(--muted-foreground)', overflowWrap: 'anywhere' }}>{secondary}</div>
    </div>
  );
}

function formatPreviewLatency(value) {
  return typeof value === 'number' && Number.isFinite(value) ? `${value.toFixed(1)} ms` : '—';
}

function ReportViewer({ report, incidentId }) {
  if (!report) return null;
  const normalized = normalizeRootReport(report);
  const { root, chain, evidence, confidence, embeddedHits, embeddedWrites } = normalized;

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
      {/* Root cause */}
      <div style={{
        ...opskeeperDarkPanelStyle,
        padding: 14, borderRadius: 8, border: '1px solid var(--border)',
      }}>
        <div style={{ fontSize: 12, color: 'var(--muted-foreground)', marginBottom: 4 }}>根因</div>
        <div style={{ fontSize: 14, fontWeight: 600 }}>
          {root.summary || root.description || report.summary || '—'}
        </div>
        {root.entity && (
          <div style={{ fontSize: 12, color: 'var(--muted-foreground)', marginTop: 4 }}>
            实体：{root.entity.type} = {root.entity.id}
          </div>
        )}
        {root.kind && (
          <div style={{ fontSize: 12, color: 'var(--muted-foreground)', marginTop: 4 }}>
            类型：<code>{root.kind}</code>
          </div>
        )}
        {root.detail && typeof root.detail === 'object' && (
          <div style={{ fontSize: 12, color: 'var(--muted-foreground)', marginTop: 4 }}>
            {Object.entries(root.detail).map(([k, v]) => (
              <div key={k}><span style={{ color: 'var(--muted-foreground)' }}>{k}:</span> {String(v)}</div>
            ))}
          </div>
        )}
        {confidence !== null && (
          <div style={{ fontSize: 12, color: 'var(--muted-foreground)', marginTop: 4 }}>
            置信度：<strong style={{ color: confidence >= 0.7 ? '#10b981' : '#f59e0b' }}>
              {(confidence * 100).toFixed(0)}%
            </strong>
          </div>
        )}
      </div>

      {/* Causal chain */}
      {chain.length > 0 && (
        <div style={{
          ...opskeeperDarkPanelStyle,
          padding: 14, borderRadius: 8, border: '1px solid var(--border)',
        }}>
          <div style={{ fontSize: 12, color: 'var(--muted-foreground)', marginBottom: 8 }}>因果链</div>
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
                  <span style={{ fontSize: 10, color: 'var(--muted-foreground)' }}>
                    {step.entity.type}:{step.entity.id}
                  </span>
                )}
                {i < chain.length - 1 && (
                  <span style={{ color: 'var(--muted-foreground)', marginLeft: 4 }}>↓</span>
                )}
              </div>
            ))}
          </div>
        </div>
      )}
      {/* Evidence */}
      {evidence.length > 0 && (
        <div style={{
          ...opskeeperDarkPanelStyle,
          padding: 14, borderRadius: 8, border: '1px solid var(--border)',
        }}>
          <div style={{ fontSize: 12, color: 'var(--muted-foreground)', marginBottom: 8 }}>
            证据 ({evidence.length} 条)
          </div>
          <ul style={{ margin: 0, paddingLeft: 18, fontSize: 12 }}>
            {evidence.map((e, i) => {
              const source = e.source || e.type || 'evidence';
              const detail = e.signal || e.snippet || e.value || '';
              const at = e.timestamp || e.observed_at;
              return (
                <li key={i} style={{ marginBottom: 6, color: '#e5e7eb' }}>
                  <code style={{ fontSize: 11, color: 'var(--muted-foreground)' }}>{source}</code>
                  {detail && <span> — <span style={{ color: '#e5e7eb' }}>{String(detail).slice(0, 240)}</span></span>}
                  {at && <span style={{ color: 'var(--muted-foreground)' }}> @ {at}</span>}
                </li>
              );
            })}
          </ul>
        </div>
      )}

      {/* Phase progress */}
      {report.phase && (
        <div style={{
          ...opskeeperDarkPanelStyle,
          padding: 14, borderRadius: 8, border: '1px solid var(--border)',
        }}>
          <div style={{ fontSize: 12, color: 'var(--muted-foreground)', marginBottom: 4 }}>
            7 阶段进度 — 当前阶段：<strong>{report.phase}</strong>
          </div>
          <PhaseProgress phase={report.phase} />
        </div>
      )}

      {/* Knowledge base contributions */}
      <KnowledgePanel
        report={report}
        incidentId={incidentId}
        embeddedHits={embeddedHits}
        embeddedWrites={embeddedWrites}
        rootSummary={root.summary || report.summary || ''}
      />

      {/* Controlled repair preview gate: after RCA, before human repair approval. */}
      <RepairPreviewGate incidentId={incidentId} />

      {/* Raw JSON fallback */}
      <details style={{ fontSize: 11, color: 'var(--muted-foreground)' }}>
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
      setIncidents(normalizeIncidentList(data));
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
    if (running) return;
    setSelected(incident);
    setRunning(true);
    setReportError(null);
    setReport(null);
    try {
      const r = await opskeeperApi.investigate(buildInvestigationRequest(incident));
      const payload = r.data || r.report || r;
      setReport(payload);
      api?.eventBus?.emit?.('dashboard:rca-finished', payload);
      api?.dashboard?.toast?.(`RCA 完成：${incident.id}`, 'success');
    } catch (e) {
      setReportError(e.message);
      api.dashboard.toast(`RCA 失败：${e.message}`, 'error');
    } finally {
      setRunning(false);
    }
  }

  const filtered = incidents.filter((i) =>
    !filter || String(i.id ?? '').includes(filter) || String(i.summary ?? '').includes(filter)
  );

  return (
    <div style={{ ...opskeeperPluginThemeStyle, padding: 24, display: 'flex', gap: 16, alignItems: 'flex-start' }}>
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
            padding: 24, textAlign: 'center', fontSize: 12, color: 'var(--muted-foreground)',
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
            onClick={() => {
              if (!running) triggerRCA(i);
            }}
            style={{
              padding: 12, borderRadius: 6, fontSize: 12,
              cursor: running ? 'wait' : 'pointer',
              opacity: running ? 0.6 : 1,
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
            padding: 32, textAlign: 'center', fontSize: 13, color: 'var(--muted-foreground)',
            border: '1px dashed var(--border)', borderRadius: 8,
          }}>
            ← 选择左侧事故触发 7 阶段 RCA
          </div>
        )}

        {selected && running && (
          <div style={{
            padding: 24, fontSize: 13, color: 'var(--muted-foreground)',
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

        {selected && !running && report && (
          <ReportViewer
            report={report}
            incidentId={selected.labels?.incident_id || selected.id}
          />
        )}
      </div>
    </div>
  );
}
