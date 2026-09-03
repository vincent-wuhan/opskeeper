import * as React from 'react';
import { opskeeperApi } from './api.js';

// OpskeeperInstallView — Dashboard surface that lists currently-installed
// opskeeper plugins and lets the operator upload a new plugin zip.
//
// Wire:
//   GET  /api/opskeeper/plugins        — list (via Dashboard proxy)
//   POST /api/opskeeper/plugins/install — upload zip (multipart/form-data)
//
// The Dashboard proxy in turn calls Manager → worker
// /api/opskeeper-teamharness/install-plugin → qwenpaw plugin install. The
// whole chain can take 30s+, so we surface progress + a busy state.
export default function OpskeeperInstallView({ api }) {
  const [plugins, setPlugins] = React.useState([]);
  const [loading, setLoading] = React.useState(true);
  const [error, setError] = React.useState(null);
  const [selectedFile, setSelectedFile] = React.useState(null);
  const [installing, setInstalling] = React.useState(false);
  const [installProgress, setInstallProgress] = React.useState(0);
  const [installResult, setInstallResult] = React.useState(null);

  const refresh = React.useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const data = await opskeeperApi.listPlugins();
      setPlugins(data.plugins || data.data || []);
    } catch (e) {
      setError(e.message);
      setPlugins([]);
    } finally {
      setLoading(false);
    }
  }, []);

  React.useEffect(() => {
    refresh();
  }, [refresh]);

  function onPickFile(ev) {
    const f = ev.target.files && ev.target.files[0];
    setSelectedFile(f || null);
    setInstallResult(null);
  }

  async function onInstall() {
    if (!selectedFile || installing) return;
    setInstalling(true);
    setInstallProgress(0);
    setInstallResult(null);
    try {
      const result = await opskeeperApi.installPlugin(selectedFile, {
        onProgress: ({ loaded, total }) => {
          const pct = total ? Math.min(100, Math.round((loaded / total) * 100)) : 0;
          setInstallProgress(pct);
        },
      });
      setInstallResult({ ok: true, data: result });
      api?.dashboard?.toast?.('插件安装成功', 'success');
      setSelectedFile(null);
      await refresh();
    } catch (e) {
      setInstallResult({ ok: false, error: e.message, body: e.body });
      api?.dashboard?.toast?.(`安装失败：${e.message}`, 'error');
    } finally {
      setInstalling(false);
    }
  }

  return (
    <div style={{ padding: 24, display: 'flex', flexDirection: 'column', gap: 16 }}>
      <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between' }}>
        <h2 style={{ margin: 0, fontSize: 18 }}>opskeeper 插件管理</h2>
        <button
          onClick={refresh}
          disabled={loading}
          style={{
            padding: '4px 10px', fontSize: 12, borderRadius: 4,
            border: '1px solid var(--border)', background: 'var(--card)',
            color: 'var(--card-foreground)', cursor: loading ? 'wait' : 'pointer',
          }}
        >
          {loading ? '…' : '刷新'}
        </button>
      </div>

      {/* Installed plugin list */}
      <div style={{
        padding: 14, borderRadius: 8, border: '1px solid var(--border)',
        background: 'var(--card)',
      }}>
        <div style={{ fontSize: 12, color: 'var(--muted)', marginBottom: 8 }}>
          已安装插件
        </div>
        {error && (
          <div style={{
            padding: 10, fontSize: 12, borderRadius: 6,
            background: 'rgba(220,38,38,0.1)', color: '#ef4444',
            border: '1px solid #ef4444', marginBottom: 8,
          }}>
            加载失败：{error}
          </div>
        )}
        {!loading && plugins.length === 0 && (
          <div style={{ fontSize: 12, color: 'var(--muted)' }}>
            暂无已安装插件
          </div>
        )}
        {plugins.map((p) => (
          <div
            key={p.id}
            style={{
              padding: 10, borderRadius: 6, marginBottom: 6,
              border: '1px solid var(--border)', background: 'var(--background)',
              display: 'flex', flexDirection: 'column', gap: 4,
            }}
          >
            <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
              <strong style={{ fontSize: 13 }}>{p.id}</strong>
              <span style={{
                fontSize: 10, padding: '1px 6px', borderRadius: 3,
                background: 'var(--muted)', color: 'var(--muted-foreground)',
              }}>
                v{p.version}
              </span>
              <span style={{ marginLeft: 'auto', fontSize: 11, color: 'var(--muted)' }}>
                {p.status}
              </span>
            </div>
            <div style={{ fontSize: 11, color: 'var(--muted)' }}>
              {p.description?.split('\n')[0].slice(0, 200)}
            </div>
            <div style={{ display: 'flex', gap: 12, fontSize: 11, color: 'var(--muted)' }}>
              <span>skills: {p.skill_count ?? '—'}</span>
              <span>tools: {p.tool_count ?? '—'}</span>
              <span>prompts: {p.prompt_count ?? '—'}</span>
              <span>adapters: {(p.adapter_ids || []).join(', ') || '—'}</span>
            </div>
          </div>
        ))}
      </div>

      {/* Upload + install */}
      <div style={{
        padding: 14, borderRadius: 8, border: '1px solid var(--border)',
        background: 'var(--card)',
      }}>
        <div style={{ fontSize: 12, color: 'var(--muted)', marginBottom: 8 }}>
          上传并安装新插件
        </div>
        <input
          type="file"
          accept=".zip"
          onChange={onPickFile}
          disabled={installing}
          style={{
            display: 'block', width: '100%', padding: 8,
            fontSize: 12, borderRadius: 4,
            border: '1px solid var(--border)', background: 'var(--background)',
            color: 'var(--card-foreground)',
            marginBottom: 8,
          }}
        />
        {selectedFile && (
          <div style={{ fontSize: 11, color: 'var(--muted)', marginBottom: 8 }}>
            已选：<code>{selectedFile.name}</code>
            {' '}({(selectedFile.size / 1024).toFixed(1)} KiB)
          </div>
        )}
        <button
          onClick={onInstall}
          disabled={!selectedFile || installing}
          style={{
            padding: '6px 14px', fontSize: 12, borderRadius: 4,
            border: 'none',
            background: !selectedFile || installing ? 'var(--muted)' : 'var(--primary)',
            color: 'var(--primary-foreground)',
            cursor: !selectedFile || installing ? 'not-allowed' : 'pointer',
          }}
        >
          {installing ? `上传中… ${installProgress}%` : '上传并安装'}
        </button>

        {installing && (
          <div style={{
            marginTop: 10, height: 6, borderRadius: 3,
            background: 'var(--muted)', overflow: 'hidden',
          }}>
            <div style={{
              width: `${installProgress}%`, height: '100%',
              background: 'var(--primary)',
              transition: 'width 0.2s ease',
            }} />
          </div>
        )}

        {installResult?.ok && (
          <div style={{
            marginTop: 10, padding: 10, borderRadius: 6, fontSize: 12,
            background: 'rgba(16,185,129,0.1)', color: '#10b981',
            border: '1px solid #10b981',
          }}>
            ✅ 安装成功 — v{installResult.data?.version ?? '?'}
            {installResult.data?.skills && `, ${installResult.data.skills} skills`}
            {installResult.data?.tools && `, ${installResult.data.tools} tools`}
          </div>
        )}

        {installResult && !installResult.ok && (
          <div style={{
            marginTop: 10, padding: 10, borderRadius: 6, fontSize: 12,
            background: 'rgba(220,38,38,0.1)', color: '#ef4444',
            border: '1px solid #ef4444',
          }}>
            ❌ 安装失败：{installResult.error}
            {installResult.body?.detail && (
              <pre style={{
                marginTop: 6, padding: 8, fontSize: 10,
                background: '#0a0a0a', color: '#eee', borderRadius: 4,
                overflow: 'auto', maxHeight: 200,
              }}>
                {typeof installResult.body.detail === 'string'
                  ? installResult.body.detail
                  : JSON.stringify(installResult.body.detail, null, 2)}
              </pre>
            )}
          </div>
        )}
      </div>
    </div>
  );
}