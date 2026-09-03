import * as React from 'react';
import { pluginApi } from './api.js';

// ─── 设计常量 ──────────────────────────────────────────────────────────────
// 色板基于 CSS 变量；fallback 让插件在没注入 Dashboard 主题色时仍可读。
const TONE = {
  primary: 'var(--primary, #2563eb)',
  primaryFg: 'var(--primary-foreground, #ffffff)',
  border: 'var(--border, rgba(15,23,42,0.18))',
  card: 'var(--card, #ffffff)',
  cardFg: 'var(--card-foreground, #0f172a)',
  muted: 'var(--muted, #475569)',
  mutedBg: 'var(--muted-bg, rgba(15,23,42,0.06))',
  destructive: 'var(--destructive, #dc2626)',
  destructiveBg: 'var(--destructive-bg, rgba(220,38,38,0.14))',
  success: 'var(--success, #059669)',
  warning: 'var(--warning, #d97706)',
  info: 'var(--info, #0891b2)',
  surface: 'var(--surface, #f8fafc)',
};

const STATUS_META = {
  installed: { tone: 'zinc',   label: '已安装' },
  enabled:   { tone: 'success', label: '已启用' },
  disabled:  { tone: 'warning', label: '已停用' },
  system:    { tone: 'info',    label: '内置' },
  dashboard: { tone: 'info',    label: '仅面板' },
};

const SYNC_META = {
  in_sync:           { tone: 'success',  label: '已对齐' },
  absent_on_worker:  { tone: 'warning',  label: 'worker 缺失' },
  system_plugin:     { tone: 'info',     label: 'worker 内置' },
  dashboard_only:    { tone: 'info',     label: '仅面板' },
  worker_unreachable:{ tone: 'destructive', label: 'worker 不可达' },
};

const SOURCE_META = {
  manager:   { tone: 'info',     label: 'opskeeper 管理', tip: '已纳入 plugin-manager DB，可被 Push/Sync/卸载' },
  worker:    { tone: 'zinc',     label: 'worker 已加载', tip: '在 AgentTeams worker (QwenPaw) 上实际加载' },
  dashboard: { tone: 'violet',   label: '面板已部署',  tip: '通过 Dashboard 静态文件 (/plugins/{id}/) 提供' },
};

const ACTION_META = {
  install:   { label: '安装',   icon: '⬇',  tone: 'info' },
  sync:      { label: '同步',   icon: '⟳',  tone: 'info' },
  push:      { label: '强推',   icon: '⤴',  tone: 'warning' },
  register:  { label: '导入',   icon: '＋',  tone: 'info' },
  enable:    { label: '启用',   icon: '✓',  tone: 'success' },
  disable:   { label: '停用',   icon: '◯',  tone: 'warning' },
  uninstall: { label: '卸载',   icon: '✕',  tone: 'destructive' },
};

const STATUS_BADGE_TONE = {
  success:     TONE.success,
  warning:     TONE.warning,
  destructive: TONE.destructive,
  info:        TONE.info,
  zinc:        '#6b7280',
  violet:      '#7c3aed',
};

// ─── 小工具 ────────────────────────────────────────────────────────────────
function statusLabel(s) {
  if (!s) return '未知';
  if (s.startsWith('error:')) return '错误';
  return STATUS_META[s]?.label ?? s;
}

function fmtDate(iso) {
  if (!iso) return '—';
  try {
    const d = new Date(iso);
    const pad = (n) => String(n).padStart(2, '0');
    return `${d.getFullYear()}-${pad(d.getMonth()+1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`;
  } catch {
    return iso;
  }
}

function fmtAgo(iso) {
  if (!iso) return '—';
  try {
    const ms = Date.now() - new Date(iso).getTime();
    if (ms < 1000) return '刚刚';
    if (ms < 60_000) return `${Math.floor(ms / 1000)} 秒前`;
    if (ms < 3_600_000) return `${Math.floor(ms / 60_000)} 分钟前`;
    if (ms < 86_400_000) return `${Math.floor(ms / 3_600_000)} 小时前`;
    return `${Math.floor(ms / 86_400_000)} 天前`;
  } catch {
    return iso;
  }
}

// ─── UI 元件 ───────────────────────────────────────────────────────────────
function Badge({ tone = 'zinc', children, title }) {
  const bg = STATUS_BADGE_TONE[tone] || STATUS_BADGE_TONE.zinc;
  return React.createElement(
    'span',
    {
      title,
      style: {
        padding: '2px 8px',
        borderRadius: 4,
        fontSize: 12,
        fontWeight: 500,
        background: bg,
        color: '#fff',
        whiteSpace: 'nowrap',
        cursor: title ? 'help' : 'default',
      },
    },
    children
  );
}

function SourceChips({ presentIn, source }) {
  const items = Array.isArray(presentIn) && presentIn.length > 0
    ? presentIn
    : [source || 'manager'];
  return React.createElement(
    'div',
    { style: { display: 'flex', gap: 4, flexWrap: 'wrap' } },
    items.map((k) => {
      const m = SOURCE_META[k] || { tone: 'zinc', label: k, tip: k };
      return React.createElement(Badge, { key: k, tone: m.tone, title: m.tip }, m.label);
    })
  );
}

function KpiCard({ label, value, tone = 'info', sub }) {
  return React.createElement(
    'div',
    {
      style: {
        flex: '1 1 0',
        minWidth: 140,
        background: TONE.card,
        border: `1px solid ${TONE.border}`,
        borderRadius: 8,
        padding: '14px 16px',
        display: 'flex',
        flexDirection: 'column',
        gap: 4,
      },
    },
    React.createElement(
      'div',
      { style: { fontSize: 13, color: TONE.muted, letterSpacing: 0.4, fontWeight: 500 } },
      label
    ),
    React.createElement(
      'div',
      {
        style: {
          fontSize: 24,
          fontWeight: 600,
          color: STATUS_BADGE_TONE[tone] || TONE.cardFg,
          lineHeight: 1.2,
        },
      },
      value
    ),
    sub &&
      React.createElement(
        'div',
        { style: { fontSize: 12, color: TONE.muted, marginTop: 2 } },
        sub
      )
  );
}

function ToolbarButton({ label, onClick, variant, disabled, title }) {
  let bg = TONE.card, fg = TONE.cardFg;
  if (variant === 'primary') { bg = TONE.primary; fg = TONE.primaryFg; }
  if (variant === 'danger')  { bg = TONE.destructive; fg = '#fff'; }
  if (variant === 'ghost')   { bg = 'transparent'; }
  return React.createElement(
    'button',
    {
      onClick,
      disabled,
      title,
      style: {
        padding: '5px 12px',
        fontSize: 12,
        fontWeight: 500,
        borderRadius: 6,
        border: `1px solid ${variant === 'danger' ? TONE.destructive : TONE.border}`,
        background: disabled ? TONE.mutedBg : bg,
        color: disabled ? TONE.muted : fg,
        cursor: disabled ? 'not-allowed' : 'pointer',
        whiteSpace: 'nowrap',
      },
    },
    label
  );
}

function Tabs({ value, onChange, items }) {
  return React.createElement(
    'div',
    {
      style: {
        display: 'flex',
        gap: 4,
        borderBottom: `1px solid ${TONE.border}`,
        padding: '0 24px',
        background: TONE.card,
      },
    },
    items.map((it) => {
      const active = it.value === value;
      return React.createElement(
        'button',
        {
          key: it.value,
          onClick: () => onChange(it.value),
          style: {
            padding: '10px 16px',
            background: 'transparent',
            border: 'none',
            borderBottom: active ? `2px solid ${TONE.primary}` : '2px solid transparent',
            color: active ? TONE.primary : TONE.muted,
            fontWeight: active ? 600 : 500,
            fontSize: 13,
            cursor: 'pointer',
            marginBottom: -1,
          },
        },
        it.label,
        it.badge != null &&
          React.createElement(
            'span',
            {
              style: {
                marginLeft: 6,
                padding: '0 6px',
                background: active ? TONE.primary : TONE.mutedBg,
                color: active ? TONE.primaryFg : TONE.muted,
                borderRadius: 8,
                fontSize: 10,
                fontWeight: 600,
              },
            },
            it.badge
          )
      );
    })
  );
}

function Field({ label, children }) {
  return React.createElement(
    'label',
    { style: { display: 'flex', flexDirection: 'column', gap: 4, fontSize: 13, color: TONE.muted } },
    React.createElement('span', null, label),
    children
  );
}

function inputStyle(w) {
  return {
    padding: '6px 10px',
    borderRadius: 6,
    border: `1px solid ${TONE.border}`,
    background: TONE.card,
    color: TONE.cardFg,
    width: w,
    fontSize: 13,
  };
}

function cellStyle() {
  return { padding: '10px 12px', verticalAlign: 'middle' };
}

function Modal({ title, onClose, children, footer }) {
  return React.createElement(
    'div',
    {
      style: {
        position: 'fixed',
        inset: 0,
        background: 'rgba(0,0,0,0.55)',
        display: 'flex',
        alignItems: 'center',
        justifyContent: 'center',
        zIndex: 1000,
      },
      onClick: onClose,
    },
    React.createElement(
      'div',
      {
        style: {
          background: TONE.card,
          color: TONE.cardFg,
          padding: 20,
          borderRadius: 10,
          minWidth: 480,
          maxWidth: 760,
          border: `1px solid ${TONE.border}`,
          boxShadow: '0 12px 40px rgba(0,0,0,0.25)',
        },
        onClick: (e) => e.stopPropagation(),
      },
      React.createElement(
        'div',
        { style: { display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 12 } },
        React.createElement('h3', { style: { margin: 0, fontSize: 16 } }, title),
        React.createElement(
          'button',
          {
            onClick: onClose,
            style: {
              background: 'transparent',
              border: 'none',
              fontSize: 18,
              cursor: 'pointer',
              color: TONE.muted,
            },
          },
          '×'
        )
      ),
      children,
      footer &&
        React.createElement(
          'div',
          { style: { marginTop: 16, textAlign: 'right', display: 'flex', gap: 8, justifyContent: 'flex-end' } },
          footer
        )
    )
  );
}

// ─── 主组件 ────────────────────────────────────────────────────────────────
export default function PluginListRoute({ api }) {
  const [tab, setTab] = React.useState('plugins');

  return React.createElement(
    'div',
    { style: { display: 'flex', flexDirection: 'column', background: TONE.surface, minHeight: '100%' } },
    React.createElement(Tabs, {
      value: tab,
      onChange: setTab,
      items: [
        { value: 'plugins', label: '插件列表' },
        { value: 'logs', label: '操作日志' },
        { value: 'system', label: '系统信息' },
      ],
    }),
    tab === 'plugins' && React.createElement(PluginsTab, { api }),
    tab === 'logs' && React.createElement(OperationLogTab, { api }),
    tab === 'system' && React.createElement(SystemInfoTab, { api }),
  );
}

// ─── 插件列表 Tab ───────────────────────────────────────────────────────────
function PluginsTab({ api }) {
  const [plugins, setPlugins] = React.useState([]);
  const [workerState, setWorkerState] = React.useState('unknown');
  const [loading, setLoading] = React.useState(true);
  const [error, setError] = React.useState(null);
  const [uploading, setUploading] = React.useState(false);
  const [filter, setFilter] = React.useState('');
  const [statusFilter, setStatusFilter] = React.useState('all');
  const [syncFilter, setSyncFilter] = React.useState('all');
  const [sourceFilter, setSourceFilter] = React.useState('all');
  const [sortBy, setSortBy] = React.useState('id');
  const [sortDir, setSortDir] = React.useState('asc');
  const [healthModal, setHealthModal] = React.useState(null);
  const [busy, setBusy] = React.useState({});
  const fileRef = React.useRef(null);

  const refresh = React.useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const data = await pluginApi.list();
      setPlugins(data.plugins || []);
      setWorkerState(data.worker_state || 'unknown');
    } catch (e) {
      setError(e.message);
    } finally {
      setLoading(false);
    }
  }, []);

  React.useEffect(() => { refresh(); }, [refresh]);

  function setSort(col) {
    if (sortBy === col) setSortDir(sortDir === 'asc' ? 'desc' : 'asc');
    else { setSortBy(col); setSortDir('asc'); }
  }

  async function withBusy(action, id, fn) {
    const key = `${action}:${id}`;
    setBusy((b) => ({ ...b, [key]: true }));
    try {
      const result = await fn();
      await refresh();
      return result;
    } catch (e) {
      api.dashboard.toast(`${actionLabel(action)}失败：${e.message}`, 'error');
      throw e;
    } finally {
      setBusy((b) => ({ ...b, [key]: false }));
    }
  }

  function actionLabel(a) {
    return ACTION_META[a]?.label || a;
  }

  async function handleUpload(e) {
    const file = e.target.files && e.target.files[0];
    if (!file) return;
    setUploading(true);
    setError(null);
    try {
      await pluginApi.install(file);
      api.dashboard.toast(`插件「${file.name}」已安装`, 'success');
      await refresh();
    } catch (err) {
      api.dashboard.toast(`安装失败：${err.message}`, 'error');
      setError(err.message);
    } finally {
      setUploading(false);
      if (fileRef.current) fileRef.current.value = '';
    }
  }

  async function handleHealth(p) {
    try {
      const h = await pluginApi.health(p.id);
      setHealthModal({ plugin: p, health: h });
    } catch (e) {
      api.dashboard.toast(`查看健康状态失败：${e.message}`, 'error');
    }
  }

  async function handleRegister(p) {
    if (!confirm(`把「${p.id}」导入到 opskeeper 管理？\n\n导入后 opskeeper 能对它做启用/停用/移除，但 Sync/Push 不可用（无 tarball）。`)) return;
    try {
      const result = await pluginApi.register(p.id);
      api.dashboard.toast(`已导入 ${p.id}（${result.status || 'ok'}）`, 'success');
      await refresh();
    } catch (e) {
      api.dashboard.toast(`导入失败：${e.message}`, 'error');
    }
  }

  async function handleSync(p) {
    try {
      const result = await pluginApi.sync(p.id);
      if (result.status === 'in_sync') {
        api.dashboard.toast(`${p.id} 已在 worker 对齐：${result.reason || ''}`, 'info');
      } else {
        api.dashboard.toast(`已同步 ${p.id}`, 'success');
      }
      await refresh();
    } catch (e) {
      api.dashboard.toast(`同步失败：${e.message}`, 'error');
    }
  }

  async function handlePush(p) {
    if (!confirm(`将强制覆盖 worker 上「${p.id}」的内容，继续？`)) return;
    try {
      const result = await pluginApi.push(p.id, { force: true });
      api.dashboard.toast(`已强推 ${p.id}（${result.payload_bytes ?? 0} bytes）到 worker`, 'success');
      await refresh();
    } catch (e) {
      api.dashboard.toast(`强推失败：${e.message}`, 'error');
    }
  }

  async function handleEnable(p)    { await withBusy('enable', p.id,    () => pluginApi.enable(p.id)).then(() => api.dashboard.toast(`已启用 ${p.id}`, 'success')); }
  async function handleDisable(p)   { await withBusy('disable', p.id,   () => pluginApi.disable(p.id)).then(() => api.dashboard.toast(`已停用 ${p.id}`, 'info')); }
  async function handleUninstall(p) {
    if (!confirm(`确认卸载「${p.id}@${p.version}」？worker 上也将同步卸载。`)) return;
    await withBusy('uninstall', p.id, () => pluginApi.uninstall(p.id, { notifyWorker: true }));
    api.dashboard.toast('已卸载（含 worker）', 'success');
  }

  function renderActions(p) {
    const present = Array.isArray(p.present_in) ? p.present_in : [p.source || 'manager'];
    const inManager = present.includes('manager');
    const inDashboard = present.includes('dashboard');
    const hasTarball = p.has_tarball !== false;
    const isBusy = (a) => !!busy[`${a}:${p.id}`];

    if (!inManager && (present.includes('worker') || inDashboard)) {
      return React.createElement(
        'div',
        { style: { display: 'flex', gap: 4, flexWrap: 'wrap' } },
        React.createElement(ToolbarButton, { label: '健康', onClick: () => handleHealth(p), variant: 'ghost' }),
        React.createElement(ToolbarButton, { label: '导入', onClick: () => handleRegister(p), variant: 'primary' })
      );
    }
    if (inManager && !hasTarball) {
      return React.createElement(
        'div',
        { style: { display: 'flex', gap: 4, flexWrap: 'wrap' } },
        React.createElement(ToolbarButton, { label: '健康', onClick: () => handleHealth(p), variant: 'ghost' }),
        p.status === 'disabled'
          ? React.createElement(ToolbarButton, { label: '启用', onClick: () => handleEnable(p), variant: 'primary', disabled: isBusy('enable') })
          : React.createElement(ToolbarButton, { label: '停用', onClick: () => handleDisable(p), disabled: isBusy('disable') }),
        React.createElement(ToolbarButton, { label: '移除', onClick: () => handleUninstall(p), variant: 'danger', disabled: isBusy('uninstall') })
      );
    }
    return React.createElement(
      'div',
      { style: { display: 'flex', gap: 4, flexWrap: 'wrap' } },
      React.createElement(ToolbarButton, { label: '健康', onClick: () => handleHealth(p), variant: 'ghost' }),
      React.createElement(ToolbarButton, { label: '同步', onClick: () => handleSync(p), disabled: isBusy('sync'), title: 'POST /sync（幂等）' }),
      React.createElement(ToolbarButton, { label: '强推', onClick: () => handlePush(p), variant: 'primary', disabled: isBusy('push'), title: 'POST /sync?force=true（覆盖）' }),
      p.status === 'disabled'
        ? React.createElement(ToolbarButton, { label: '启用', onClick: () => handleEnable(p), variant: 'primary', disabled: isBusy('enable') })
        : React.createElement(ToolbarButton, { label: '停用', onClick: () => handleDisable(p), disabled: isBusy('disable') }),
      React.createElement(ToolbarButton, { label: '卸载', onClick: () => handleUninstall(p), variant: 'danger', disabled: isBusy('uninstall') })
    );
  }

  // KPI 统计
  const total = plugins.length;
  const managed = plugins.filter((p) => p.managed).length;
  const syncErrors = plugins.filter((p) => (p.sync_state || '').startsWith('stale:') || p.sync_state === 'worker_unreachable').length;
  const unmanaged = total - managed;

  const filtered = plugins
    .filter((p) => !filter || p.id.includes(filter) || (p.name || '').includes(filter))
    .filter((p) => statusFilter === 'all' || p.status === statusFilter)
    .filter((p) => {
      if (syncFilter === 'all') return true;
      if (syncFilter === 'stale')    return (p.sync_state || '').startsWith('stale:');
      if (syncFilter === 'absent')   return p.sync_state === 'absent_on_worker';
      if (syncFilter === 'in_sync')  return p.sync_state === 'in_sync';
      if (syncFilter === 'unreach')  return p.sync_state === 'worker_unreachable';
      return true;
    })
    .filter((p) => {
      if (sourceFilter === 'all') return true;
      const present = Array.isArray(p.present_in) ? p.present_in : [p.source || 'manager'];
      return present.includes(sourceFilter);
    })
    .sort((a, b) => {
      const av = a[sortBy] || '';
      const bv = b[sortBy] || '';
      const cmp = String(av).localeCompare(String(bv));
      return sortDir === 'asc' ? cmp : -cmp;
    });

  const COLUMNS = [
    { key: 'id',           label: '插件',     w: '24%', align: 'left' },
    { key: 'version',      label: '版本',     w: '8%',  align: 'left' },
    { key: 'status',       label: '状态',     w: '10%', align: 'left' },
    { key: 'source',       label: '来源',     w: '14%', align: 'left' },
    { key: 'sync_state',   label: '同步',     w: '11%', align: 'left' },
    { key: 'worker',       label: 'worker',   w: '12%', align: 'left' },
    { key: 'installed_at', label: '安装时间', w: '11%', align: 'left' },
    { key: 'actions',      label: '操作',     w: '10%', align: 'right' },
  ];

  return React.createElement(
    'div',
    { style: { padding: '20px 24px', display: 'flex', flexDirection: 'column', gap: 16 } },

    // 顶部 KPI
    React.createElement(
      'div',
      { style: { display: 'flex', gap: 12, flexWrap: 'wrap' } },
      React.createElement(KpiCard, { label: '插件总数', value: total, tone: 'info', sub: `含 ${unmanaged} 个外部插件` }),
      React.createElement(KpiCard, { label: 'opskeeper 管理', value: managed, tone: 'success', sub: '可被 Sync / Push / 卸载' }),
      React.createElement(KpiCard, { label: '需关注', value: syncErrors, tone: syncErrors > 0 ? 'warning' : 'zinc', sub: 'stale 或 worker 不可达' }),
      React.createElement(KpiCard, {
        label: 'worker 状态',
        value: workerState.startsWith('reachable') ? '已连接' : '离线',
        tone: workerState.startsWith('reachable') ? 'success' : 'destructive',
        sub: workerState,
      })
    ),

    // 操作栏
    React.createElement(
      'div',
      { style: { display: 'flex', justifyContent: 'space-between', alignItems: 'center', gap: 8 } },
      React.createElement(
        'div',
        { style: { display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap' } },
        React.createElement('input', {
          ref: fileRef,
          type: 'file',
          onChange: handleUpload,
          style: { display: 'none' },
        }),
        React.createElement(ToolbarButton, {
          label: uploading ? '上传中…' : '上传 zip / tar.gz',
          onClick: () => fileRef.current && fileRef.current.click(),
          variant: 'primary',
          disabled: uploading,
        }),
        React.createElement(ToolbarButton, {
          label: '刷新',
          onClick: refresh,
          variant: 'ghost',
          disabled: loading,
        })
      ),
      React.createElement(
        'div',
        { style: { fontSize: 12, color: TONE.muted, fontWeight: 500 } },
        `最后刷新：${fmtAgo(new Date().toISOString())}`
      )
    ),

    // 过滤
    React.createElement(
      'div',
      { style: { display: 'flex', gap: 8, alignItems: 'flex-end', flexWrap: 'wrap' } },
      React.createElement(Field, { label: '搜索' },
        React.createElement('input', {
          type: 'text',
          placeholder: '按 id 或名称过滤…',
          value: filter,
          onChange: (e) => setFilter(e.target.value),
          style: inputStyle(220),
        })
      ),
      React.createElement(Field, { label: '状态' },
        React.createElement(
          'select',
          { value: statusFilter, onChange: (e) => setStatusFilter(e.target.value), style: inputStyle(140) },
          React.createElement('option', { value: 'all' }, '全部'),
          ...Object.entries(STATUS_META).map(([k, v]) =>
            React.createElement('option', { key: k, value: k }, v.label)
          )
        )
      ),
      React.createElement(Field, { label: '同步' },
        React.createElement(
          'select',
          { value: syncFilter, onChange: (e) => setSyncFilter(e.target.value), style: inputStyle(140) },
          React.createElement('option', { value: 'all' }, '全部'),
          React.createElement('option', { value: 'in_sync' }, '已对齐'),
          React.createElement('option', { value: 'absent' }, 'worker 缺失'),
          React.createElement('option', { value: 'stale' }, '已过期'),
          React.createElement('option', { value: 'unreach' }, 'worker 不可达')
        )
      ),
      React.createElement(Field, { label: '来源' },
        React.createElement(
          'select',
          { value: sourceFilter, onChange: (e) => setSourceFilter(e.target.value), style: inputStyle(160) },
          React.createElement('option', { value: 'all' }, '全部'),
          ...Object.entries(SOURCE_META).map(([k, v]) =>
            React.createElement('option', { key: k, value: k }, v.label)
          )
        )
      )
    ),

    error &&
      React.createElement(
        'div',
        {
          style: {
            padding: 12,
            borderRadius: 6,
            background: TONE.destructiveBg,
            color: TONE.destructive,
            border: `1px solid ${TONE.destructive}`,
            fontSize: 13,
          },
        },
        '错误：' + error
      ),

    // 表格
    React.createElement(
      'div',
      {
        style: {
          border: `1px solid ${TONE.border}`,
          borderRadius: 8,
          overflow: 'hidden',
          background: TONE.card,
        },
      },
      React.createElement(
        'table',
        { style: { width: '100%', borderCollapse: 'collapse', fontSize: 13 } },
        React.createElement(
          'thead',
          null,
          React.createElement(
            'tr',
            { style: { background: TONE.mutedBg } },
            COLUMNS.map((col) =>
              React.createElement(
                'th',
                {
                  key: col.key,
                  onClick: () => col.key !== 'actions' && setSort(col.key),
                  style: {
                    width: col.w,
                    padding: '10px 12px',
                    textAlign: col.align,
                    fontWeight: 600,
                    cursor: col.key === 'actions' ? 'default' : 'pointer',
                    userSelect: 'none',
                    borderBottom: `1px solid ${TONE.border}`,
                    color: TONE.cardFg,
                    fontSize: 13,
                    letterSpacing: 0.2,
                  },
                },
                col.label,
                sortBy === col.key &&
                  React.createElement(
                    'span',
                    { style: { marginLeft: 4, fontSize: 10 } },
                    sortDir === 'asc' ? '▲' : '▼'
                  )
              )
            )
          )
        ),
        React.createElement(
          'tbody',
          null,
          loading &&
            React.createElement(
              'tr',
              null,
              React.createElement(
                'td',
                { colSpan: COLUMNS.length, style: { padding: 28, textAlign: 'center', color: TONE.muted, fontSize: 14 } },
                '加载中…'
              )
            ),
          !loading && filtered.length === 0 &&
            React.createElement(
              'tr',
              null,
              React.createElement(
                'td',
                { colSpan: COLUMNS.length, style: { padding: 32, textAlign: 'center', color: TONE.muted, fontSize: 14 } },
                plugins.length === 0 ? '暂无插件 — 请上传 tar.gz / zip' : '没有匹配的插件'
              )
            ),
          filtered.map((p) =>
            React.createElement(
              'tr',
              { key: p.id, style: { borderBottom: `1px solid ${TONE.border}` } },
              React.createElement(
                'td',
                { style: { ...cellStyle(), fontWeight: 500 } },
                React.createElement('div', null, p.name || p.id),
                p.description &&
                  React.createElement(
                    'div',
                    { style: { fontSize: 12, color: TONE.muted, marginTop: 2 } },
                    p.description.slice(0, 100) + (p.description.length > 100 ? '…' : '')
                  )
              ),
              React.createElement('td', { style: { ...cellStyle(), fontFamily: 'monospace', fontSize: 13 } }, 'v' + p.version),
              React.createElement(
                'td',
                { style: cellStyle() },
                React.createElement(Badge, { tone: STATUS_META[p.status]?.tone || 'zinc' }, statusLabel(p.status))
              ),
              React.createElement(
                'td',
                { style: cellStyle() },
                React.createElement(SourceChips, { presentIn: p.present_in, source: p.source })
              ),
              React.createElement(
                'td',
                { style: cellStyle() },
                (() => {
                  const ss = p.sync_state || '';
                  const meta = SYNC_META[ss] || (ss.startsWith('stale:') ? { tone: 'destructive', label: '已过期' } : { tone: 'zinc', label: ss || '—' });
                  return React.createElement(Badge, { tone: meta.tone, title: ss }, meta.label);
                })()
              ),
              React.createElement(
                'td',
                { style: { ...cellStyle(), fontSize: 12, color: TONE.muted } },
                p.worker_info
                  ? `v${p.worker_info.version} · ${p.worker_info.enabled ? '已启用' : '未启用'}`
                  : '—'
              ),
              React.createElement(
                'td',
                { style: { ...cellStyle(), fontSize: 12, color: TONE.muted, fontFamily: 'monospace' } },
                (Array.isArray(p.present_in) ? !p.present_in.includes('manager') : p.source === 'worker' || p.source === 'dashboard')
                  ? '未管理'
                  : fmtDate(p.installed_at)
              ),
              React.createElement('td', { style: { ...cellStyle(), textAlign: 'right' } }, renderActions(p))
            )
          )
        )
      )
    ),

    // 健康详情
    healthModal &&
      React.createElement(
        Modal,
        {
          title: `健康详情 · ${healthModal.plugin.id}`,
          onClose: () => setHealthModal(null),
          footer: React.createElement(ToolbarButton, { label: '关闭', onClick: () => setHealthModal(null) }),
        },
        React.createElement(
          'pre',
          {
            style: {
              fontSize: 12,
              maxHeight: 420,
              overflow: 'auto',
              padding: 14,
              background: TONE.mutedBg,
              borderRadius: 6,
              margin: 0,
              fontFamily: 'monospace',
            },
          },
          JSON.stringify(healthModal.health, null, 2)
        )
      )
  );
}

// ─── 操作日志 Tab ───────────────────────────────────────────────────────────
function OperationLogTab({ api }) {
  const [entries, setEntries] = React.useState([]);
  const [loading, setLoading] = React.useState(true);
  const [error, setError] = React.useState(null);
  const [autoRefresh, setAutoRefresh] = React.useState(true);
  const [filter, setFilter] = React.useState('');
  const [actionFilter, setActionFilter] = React.useState('all');
  const [statusFilter, setStatusFilter] = React.useState('all');
  const [selected, setSelected] = React.useState(null);

  const refresh = React.useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const data = await pluginApi.operations({ tail: 200 });
      setEntries(data.entries || []);
    } catch (e) {
      setError(e.message);
    } finally {
      setLoading(false);
    }
  }, []);

  React.useEffect(() => { refresh(); }, [refresh]);
  React.useEffect(() => {
    if (!autoRefresh) return;
    const t = setInterval(refresh, 3000);
    return () => clearInterval(t);
  }, [autoRefresh, refresh]);

  const filtered = entries.filter((e) => {
    if (actionFilter !== 'all' && e.action !== actionFilter) return false;
    if (statusFilter !== 'all' && e.status !== statusFilter) return false;
    if (filter) {
      const f = filter.toLowerCase();
      if (!(e.plugin_id || '').toLowerCase().includes(f) &&
          !(e.detail || '').toLowerCase().includes(f) &&
          !(e.error || '').toLowerCase().includes(f)) {
        return false;
      }
    }
    return true;
  });

  // 统计
  const counts = entries.reduce((acc, e) => {
    acc[e.status] = (acc[e.status] || 0) + 1;
    return acc;
  }, {});

  return React.createElement(
    'div',
    { style: { padding: '20px 24px', display: 'flex', flexDirection: 'column', gap: 16 } },

    // KPI
    React.createElement(
      'div',
      { style: { display: 'flex', gap: 12, flexWrap: 'wrap' } },
      React.createElement(KpiCard, { label: '总操作数', value: entries.length, tone: 'info' }),
      React.createElement(KpiCard, { label: '成功', value: counts.ok || 0, tone: 'success' }),
      React.createElement(KpiCard, { label: '警告', value: counts.warn || 0, tone: 'warning' }),
      React.createElement(KpiCard, { label: '失败', value: counts.error || 0, tone: (counts.error || 0) > 0 ? 'destructive' : 'zinc' })
    ),

    // 过滤
    React.createElement(
      'div',
      { style: { display: 'flex', gap: 8, alignItems: 'flex-end', flexWrap: 'wrap' } },
      React.createElement(Field, { label: '搜索' },
        React.createElement('input', {
          type: 'text',
          placeholder: '搜索插件 id / detail / error…',
          value: filter,
          onChange: (e) => setFilter(e.target.value),
          style: inputStyle(240),
        })
      ),
      React.createElement(Field, { label: '动作' },
        React.createElement(
          'select',
          { value: actionFilter, onChange: (e) => setActionFilter(e.target.value), style: inputStyle(140) },
          React.createElement('option', { value: 'all' }, '全部'),
          ...Object.entries(ACTION_META).map(([k, v]) =>
            React.createElement('option', { key: k, value: k }, v.label)
          )
        )
      ),
      React.createElement(Field, { label: '结果' },
        React.createElement(
          'select',
          { value: statusFilter, onChange: (e) => setStatusFilter(e.target.value), style: inputStyle(120) },
          React.createElement('option', { value: 'all' }, '全部'),
          React.createElement('option', { value: 'ok' }, '成功'),
          React.createElement('option', { value: 'warn' }, '警告'),
          React.createElement('option', { value: 'error' }, '失败'),
          React.createElement('option', { value: 'skip' }, '跳过')
        )
      ),
      React.createElement(
        'div',
        { style: { flex: 1 } }
      ),
      React.createElement(ToolbarButton, {
        label: autoRefresh ? '暂停自动刷新' : '启动自动刷新',
        onClick: () => setAutoRefresh(!autoRefresh),
        variant: autoRefresh ? 'primary' : 'ghost',
      }),
      React.createElement(ToolbarButton, { label: '刷新', onClick: refresh, variant: 'ghost', disabled: loading })
    ),

    error &&
      React.createElement(
        'div',
        { style: {
          padding: 12, borderRadius: 6, background: TONE.destructiveBg,
          color: TONE.destructive, border: `1px solid ${TONE.destructive}`, fontSize: 13,
        } },
        '错误：' + error
      ),

    // 时间线
    React.createElement(
      'div',
      {
        style: {
          border: `1px solid ${TONE.border}`,
          borderRadius: 8,
          background: TONE.card,
          maxHeight: 600,
          overflow: 'auto',
        },
      },
      loading && entries.length === 0 &&
        React.createElement('div', { style: { padding: 28, textAlign: 'center', color: TONE.muted, fontSize: 14 } }, '加载中…'),
      !loading && filtered.length === 0 &&
        React.createElement('div', { style: { padding: 32, textAlign: 'center', color: TONE.muted, fontSize: 14 } }, '没有匹配的操作记录'),
      filtered.map((e) => {
        const meta = ACTION_META[e.action] || { label: e.action, icon: '·', tone: 'zinc' };
        const isError = e.status === 'error';
        const isWarn = e.status === 'warn';
        const accentColor = isError ? TONE.destructive : (isWarn ? TONE.warning : TONE.border);
        return React.createElement(
          'div',
          {
            key: e.id,
            onClick: () => setSelected(e),
            style: {
              display: 'flex',
              gap: 12,
              padding: '10px 14px',
              borderBottom: `1px solid ${TONE.border}`,
              borderLeft: `3px solid ${accentColor}`,
              cursor: 'pointer',
              background: selected && selected.id === e.id ? TONE.mutedBg : 'transparent',
            },
          },
          React.createElement(
            'div',
            {
              style: {
                fontFamily: 'monospace',
                fontSize: 12,
                color: TONE.muted,
                fontWeight: 500,
                minWidth: 140,
              },
            },
            fmtDate(e.started_at)
          ),
          React.createElement(
            'div',
            { style: { minWidth: 70 } },
            React.createElement(Badge, { tone: meta.tone }, `${meta.icon} ${meta.label}`)
          ),
          React.createElement(
            'div',
            { style: { minWidth: 180, fontFamily: 'monospace', fontSize: 13 } },
            e.plugin_id || '—'
          ),
          React.createElement(
            'div',
            { style: { flex: 1, fontSize: 14, color: isError ? TONE.destructive : TONE.cardFg } },
            e.detail || e.error || '—'
          ),
          React.createElement(
            'div',
            { style: { fontSize: 12, color: TONE.muted, minWidth: 70, textAlign: 'right', fontWeight: 500 } },
            `${e.duration_ms} ms`
          )
        );
      })
    ),

    // 详情
    selected &&
      React.createElement(
        Modal,
        {
          title: `操作详情 · ${ACTION_META[selected.action]?.label || selected.action} · ${selected.plugin_id || '—'}`,
          onClose: () => setSelected(null),
          footer: React.createElement(ToolbarButton, { label: '关闭', onClick: () => setSelected(null) }),
        },
        React.createElement(
          'div',
          { style: { display: 'flex', flexDirection: 'column', gap: 8, fontSize: 14 } },
          React.createElement('div', null, React.createElement('strong', null, '开始时间：'), ' ', fmtDate(selected.started_at)),
          React.createElement('div', null, React.createElement('strong', null, '耗时：'), ' ', selected.duration_ms, ' ms'),
          React.createElement('div', null, React.createElement('strong', null, '状态：'), ' ',
            React.createElement(Badge, {
              tone: { ok: 'success', warn: 'warning', error: 'destructive', skip: 'zinc' }[selected.status] || 'zinc',
            }, selected.status)
          ),
          selected.detail &&
            React.createElement('div', null, React.createElement('strong', null, '描述：'), ' ', selected.detail),
          selected.error &&
            React.createElement(
              'div',
              { style: { padding: 10, background: TONE.destructiveBg, borderRadius: 6, color: TONE.destructive, fontFamily: 'monospace', fontSize: 13, fontWeight: 500 } },
              selected.error
            )
        )
      )
  );
}

// ─── 系统信息 Tab ───────────────────────────────────────────────────────────
function SystemInfoTab({ api }) {
  const [data, setData] = React.useState(null);
  const [ops, setOps] = React.useState(null);
  const [error, setError] = React.useState(null);

  const refresh = React.useCallback(async () => {
    setError(null);
    try {
      const [list, op] = await Promise.all([
        pluginApi.list(),
        pluginApi.operations({ tail: 1 }),
      ]);
      setData(list);
      setOps(op);
    } catch (e) {
      setError(e.message);
    }
  }, []);
  React.useEffect(() => { refresh(); }, [refresh]);

  const rows = [
    ['Worker 状态', data?.worker_state || '未知'],
    ['插件总数', String(data?.plugins?.length ?? '—')],
    ['Opskeeper 管理', String(data?.plugins?.filter((p) => p.managed).length ?? '—')],
    ['操作日志条数', String(ops?.size ?? '—')],
    ['后端', 'agentteams-plugin-manager (独立 microservice)'],
    ['API 路径', '/api/v1/plugins · /api/v1/operations'],
  ];

  return React.createElement(
    'div',
    { style: { padding: '20px 24px', display: 'flex', flexDirection: 'column', gap: 16, maxWidth: 720 } },

    React.createElement(
      'div',
      { style: { display: 'flex', gap: 8, alignItems: 'center' } },
      React.createElement('h3', { style: { margin: 0, fontSize: 16 } }, '运行时信息'),
      React.createElement(ToolbarButton, { label: '刷新', onClick: refresh, variant: 'ghost' })
    ),

    error &&
      React.createElement(
        'div',
        { style: {
          padding: 12, borderRadius: 6, background: TONE.destructiveBg,
          color: TONE.destructive, border: `1px solid ${TONE.destructive}`, fontSize: 13,
        } },
        '错误：' + error
      ),

    React.createElement(
      'div',
      {
        style: {
          border: `1px solid ${TONE.border}`,
          borderRadius: 8,
          background: TONE.card,
          overflow: 'hidden',
        },
      },
      React.createElement(
        'table',
        { style: { width: '100%', borderCollapse: 'collapse', fontSize: 13 } },
        React.createElement(
          'tbody',
          null,
          rows.map(([k, v], i) =>
            React.createElement(
              'tr',
              { key: k, style: { borderBottom: i < rows.length - 1 ? `1px solid ${TONE.border}` : 'none' } },
              React.createElement(
                'td',
                {
                  style: {
                    padding: '10px 14px',
                    color: TONE.muted,
                    background: TONE.mutedBg,
                    fontWeight: 500,
                    width: 180,
                  },
                },
                k
              ),
              React.createElement('td', { style: { padding: '10px 14px', fontFamily: k === 'API 路径' ? 'monospace' : 'inherit' } }, v)
            )
          )
        )
      )
    ),

    React.createElement(
      'div',
      { style: { fontSize: 12, color: TONE.muted } },
      '此面板仅显示 agentteams-plugin-manager 暴露的运行时信息；worker 与 controller 的深度指标请到对应 dashboard。'
    )
  );
}
