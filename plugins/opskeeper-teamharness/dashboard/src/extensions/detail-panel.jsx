import * as React from 'react';
import { opskeeperApi } from './api.js';

// Worker 详情页嵌入区块 — 列出该 worker 最近 5 次 RCA 报告 + 单 worker re-trigger 按钮。
//
// 限制：当前 opskeeper /v1/incidents 不支持按 worker_id 过滤（无 worker_id 字段）。
// 因此这里拉取最近 50 个事故 → 在客户端按 entity.worker_id / tags 过滤 → 显示前 5 条。
// 当 worker entity 提供了 metadata.worker_id 则更精确，否则按 summary 中的 worker 名字匹配。

function workerMatches(incident, worker) {
  if (!worker) return true;
  const wid = worker.id || worker.name;
  if (!wid) return true;
  const haystack = JSON.stringify(incident).toLowerCase();
  return haystack.includes(String(wid).toLowerCase());
}

function StatusDot({ status }) {
  const s = (status || 'open').toLowerCase();
  const color = s === 'resolved' || s === 'closed'
    ? '#10b981'
    : s === 'open' || s === 'in_progress'
      ? '#ef4444'
      : '#888';
  return (
    <span style={{
      display: 'inline-block', width: 6, height: 6, borderRadius: '50%',
      background: color, marginRight: 6, verticalAlign: 'middle',
    }} />
  );
}

export default function WorkerOpsBlock({ entity, api }) {
  const workerName = entity && (entity.name || entity.id);
  const [incidents, setIncidents] = React.useState([]);
  const [loading, setLoading] = React.useState(true);
  const [busy, setBusy] = React.useState(null);
  const [error, setError] = React.useState(null);

  const refresh = React.useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const data = await opskeeperApi.listIncidents({ limit: 50 });
      const all = data.incidents || data.data || [];
      const filtered = all.filter((i) => workerMatches(i, entity)).slice(0, 5);
      setIncidents(filtered);
    } catch (e) {
      setError(e.message);
    } finally {
      setLoading(false);
    }
  }, [entity]);

  React.useEffect(() => {
    refresh();
  }, [refresh]);

  async function reTrigger(i) {
    if (!i?.id) return;
    setBusy(i.id);
    try {
      await opskeeperApi.investigate({ incident_id: i.id });
      api.dashboard.toast(`已重新触发 RCA：${i.id}`, 'success');
      await refresh();
    } catch (e) {
      api.dashboard.toast(`触发失败：${e.message}`, 'error');
    } finally {
      setBusy(null);
    }
  }

  return (
    <div style={{
      padding: 12, border: '1px solid var(--border)', borderRadius: 8,
      background: 'var(--card)', color: 'var(--card-foreground)',
    }}>
      <div style={{
        display: 'flex', alignItems: 'center', marginBottom: 8, gap: 8,
      }}>
        <strong style={{ fontSize: 13 }}>Opskeeper 历史</strong>
        <span style={{ fontSize: 11, color: 'var(--muted)' }}>
          {workerName ? `worker: ${workerName}` : 'worker: -'}
        </span>
        <span style={{
          marginLeft: 'auto', padding: '1px 6px', borderRadius: 3, fontSize: 10,
          background: 'var(--muted)', color: 'var(--muted-foreground)',
        }}>
          最近 {incidents.length} 条
        </span>
        <button
          onClick={refresh}
          style={{
            padding: '2px 8px', fontSize: 11, borderRadius: 4,
            border: '1px solid var(--border)', background: 'transparent',
            color: 'var(--muted)', cursor: 'pointer',
          }}
        >
          刷新
        </button>
      </div>

      {error && (
        <div style={{
          padding: 8, borderRadius: 4, fontSize: 11,
          background: 'rgba(220,38,38,0.1)', color: '#ef4444', marginBottom: 8,
        }}>
          {error}
        </div>
      )}

      {loading ? (
        <div style={{ fontSize: 12, color: 'var(--muted)' }}>加载中…</div>
      ) : incidents.length === 0 ? (
        <div style={{ fontSize: 12, color: 'var(--muted)', padding: 8 }}>
          暂无该 worker 的 RCA 历史
        </div>
      ) : (
        <div style={{ display: 'flex', flexDirection: 'column', gap: 4 }}>
          {incidents.map((i) => (
            <div
              key={i.id}
              style={{
                display: 'flex', alignItems: 'center', gap: 8,
                padding: '6px 8px', fontSize: 12, borderRadius: 4,
                background: 'rgba(127,127,127,0.06)',
              }}
            >
              <StatusDot status={i.status} />
              <code style={{ fontSize: 11 }}>{i.id}</code>
              <span style={{ flex: 1, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
                {i.summary || '(no summary)'}
              </span>
              <span style={{ fontSize: 10, color: 'var(--muted)' }}>{i.severity || '—'}</span>
              <button
                onClick={() => reTrigger(i)}
                disabled={busy === i.id}
                title="重新触发 RCA"
                style={{
                  padding: '1px 8px', fontSize: 10, borderRadius: 3,
                  border: '1px solid var(--border)', background: 'transparent',
                  color: 'var(--muted)', cursor: busy === i.id ? 'wait' : 'pointer',
                }}
              >
                {busy === i.id ? '…' : 're-RCA'}
              </button>
            </div>
          ))}
        </div>
      )}

      <div style={{
        marginTop: 10, paddingTop: 8, borderTop: '1px solid var(--border)',
        display: 'flex', gap: 6, fontSize: 10, color: 'var(--muted)',
      }}>
        <button
          onClick={() => api.dashboard.navigate('plugin-route:opskeeper-teamharness/home')}
          style={{
            padding: '3px 10px', fontSize: 10, borderRadius: 3,
            border: '1px solid var(--border)', background: 'transparent',
            color: 'var(--muted)', cursor: 'pointer',
          }}
        >
          打开完整诊断 →
        </button>
      </div>
    </div>
  );
}
