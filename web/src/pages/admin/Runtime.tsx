// Runtime — deployment version page.
import { useCallback, useEffect, useState } from 'react';
import {
  Activity,
  AlertTriangle,
  Bot,
  Boxes,
  CheckCircle2,
  Database,
  GitBranch,
  Loader2,
  RefreshCw,
  Server,
  ShieldCheck,
  XCircle,
} from 'lucide-react';
import { Card } from '@/components/ui/Card';
import { PageHeader } from '@/components/ui/PageHeader';
import { cn } from '@/lib/cn';
import { useI18n } from '@/i18n/locale';
import { usePermissions } from '@/store/me';
import {
  getVersionDeployment,
  type DeploymentResponse,
  type DeployDependency,
  type HealthSummary,
  type RecoveryOperation,
  type SkillVersion,
  type TimelinePhaseLite,
} from '@/api/versionDeployment';
import {
  getRuntimeInventory,
  type InventoryResponse,
  type RuntimeView,
} from '@/api/runtimeInventory';
import { ApiError } from '@/api/client';

const POLL_MS = 10_000;

const STATUS_TONE: Record<string, string> = {
  ok: 'bg-emerald-500/15 text-emerald-300 border-emerald-700/40',
  success: 'bg-emerald-500/15 text-emerald-300 border-emerald-700/40',
  degraded: 'bg-amber-500/15 text-amber-300 border-amber-700/40',
  warning: 'bg-amber-500/15 text-amber-300 border-amber-700/40',
  failed: 'bg-red-500/15 text-red-300 border-red-700/40',
  error: 'bg-red-500/15 text-red-300 border-red-700/40',
  pending: 'bg-zinc-700/30 text-zinc-300 border-zinc-700',
  running: 'bg-sky-500/15 text-sky-300 border-sky-700/40',
  skipped: 'bg-zinc-700/30 text-zinc-400 border-zinc-700',
};

export default function RuntimePage() {
  const { tr } = useI18n();
  const { isAdmin } = usePermissions();
  const [resp, setResp] = useState<DeploymentResponse | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  const [refreshing, setRefreshing] = useState(false);
  const [inventory, setInventory] = useState<InventoryResponse | null>(null);
  const [inventoryErr, setInventoryErr] = useState<string | null>(null);
  const [inventoryLoading, setInventoryLoading] = useState(true);

  const load = useCallback(async (silent = false) => {
    try {
      if (silent) setRefreshing(true);
      else setLoading(true);
      const data = await getVersionDeployment();
      setResp(data);
      setErr(null);
    } catch (e) {
      setErr(e instanceof ApiError ? e.message : (e as Error).message);
    } finally {
      setLoading(false);
      setRefreshing(false);
    }
  }, []);

  // Inventory polls independently: it is the authoritative runtime list
  // (registration + live projections), while the deployment view below is
  // a different, version-composition readback. One failing must not blank
  // the other, which is why each keeps its own error state.
  const loadInventory = useCallback(async (silent = false) => {
    try {
      if (!silent) setInventoryLoading(true);
      const data = await getRuntimeInventory();
      setInventory(data);
      setInventoryErr(null);
    } catch (e) {
      setInventoryErr(e instanceof ApiError ? e.message : (e as Error).message);
    } finally {
      setInventoryLoading(false);
    }
  }, []);

  useEffect(() => { void load(false); }, [load]);
  useEffect(() => { void loadInventory(false); }, [loadInventory]);
  useEffect(() => {
    const id = window.setInterval(() => {
      void load(true);
      void loadInventory(true);
    }, POLL_MS);
    return () => window.clearInterval(id);
  }, [load, loadInventory]);

  const refreshAll = useCallback(() => {
    void load(false);
    void loadInventory(false);
  }, [load, loadInventory]);

  if ((loading && !resp) || (inventoryLoading && !inventory)) {
    return (
      <div className="px-6 py-5">
        <PageHeader title={tr('运行时版本', 'Runtime / Version')} />
        <Card className="text-center text-xs text-zinc-500">
          <div className="py-8">
            <Loader2 className="inline h-4 w-4 animate-spin mr-2" />
            {tr('加载中…', 'Loading…')}
          </div>
        </Card>
      </div>
    );
  }

  return (
    <div className="px-6 py-5 max-w-5xl">
      <PageHeader
        title={tr('运行时版本与部署', 'Runtime / Version')}
        subtitle={(
          <span className="text-xs text-zinc-500">
            {tr('运行时清单（注册 + 实时投影）+ Manager / Worker / 插件 / 服务端组合 + 健康检查 + 部署依赖 + 一次完整恢复操作',
                'Runtime inventory (registry + live projections) + Manager / Worker / plugin / server composition + health + deploy deps + one worked recovery')}
          </span>
        )}
        actions={(
          <button
            type="button"
            className="flex items-center gap-1 px-2.5 py-1 text-xs text-zinc-300 border border-zinc-700 rounded hover:bg-zinc-800"
            onClick={refreshAll}
          >
            <RefreshCw className={cn('h-3.5 w-3.5', refreshing && 'animate-spin')} />
            {tr('刷新', 'Refresh')}
          </button>
        )}
      />

      {inventoryErr && (
        <Card className="text-center text-xs text-red-400 mb-4">
          <div className="py-3">{tr('运行时清单加载失败', 'Failed to load runtime inventory')}: {inventoryErr}</div>
        </Card>
      )}

      {inventory && <InventoryCard resp={inventory} isAdmin={isAdmin} />}

      {err && (
        <Card className="text-center text-xs text-red-400 mb-4">
          <div className="py-3">{tr('加载失败', 'Failed to load')}: {err}</div>
        </Card>
      )}

      {resp && (
        <>
          <DeploymentGrid resp={resp} />
          <HealthCard health={resp.health} />
          <DependenciesCard deps={resp.dependencies} />
          <RecoveryExampleCard rec={resp.recovery_example} />
          <SkillsCard skills={resp.skills} />
        </>
      )}
    </div>
  );
}

// ---- Runtime inventory (authoritative list, spec first requirement) ----

// The five health states the backend may derive, plus unknown for a
// projection that honestly cannot tell (e.g. edge presence unreadable).
const HEALTH_BADGE: Record<string, { zh: string; en: string; tone: string }> = {
  starting: { zh: '启动中', en: 'starting', tone: 'bg-sky-500/15 text-sky-300 border-sky-700/40' },
  online: { zh: '在线', en: 'online', tone: 'bg-emerald-500/15 text-emerald-300 border-emerald-700/40' },
  degraded: { zh: '降级', en: 'degraded', tone: 'bg-amber-500/15 text-amber-300 border-amber-700/40' },
  recently_lost: { zh: '最近失联', en: 'recently_lost', tone: 'bg-orange-500/15 text-orange-300 border-orange-700/40' },
  offline: { zh: '离线', en: 'offline', tone: 'bg-red-500/15 text-red-300 border-red-700/40' },
  unknown: { zh: '未知', en: 'unknown', tone: 'bg-zinc-700/30 text-zinc-400 border-zinc-700' },
};
const HEALTH_FALLBACK = HEALTH_BADGE.unknown;

function InventoryCard({ resp, isAdmin }: { resp: InventoryResponse; isAdmin: boolean }) {
  const { tr } = useI18n();
  const items = resp.items ?? [];
  return (
    <Card className="mb-4">
      <div className="flex items-center gap-2 mb-2">
        <Boxes className="h-4 w-4 text-indigo-400" />
        <div className="text-sm font-medium">{tr('运行时清单', 'Runtime inventory')}</div>
        <span className="ml-auto rounded border border-border-soft px-1.5 py-0.5 text-[10px] uppercase tracking-wider text-zinc-500">
          {tr('只读', 'read-only')} · {resp.total}
        </span>
      </div>
      {items.length === 0 ? (
        <div className="px-2 py-4 rounded border border-zinc-800/60 bg-zinc-900/40 text-xs text-zinc-500">
          {tr('暂无已注册或可投影的运行时', 'No registered or projectable runtimes')}
        </div>
      ) : (
        <table className="w-full text-xs">
          <thead>
            <tr className="border-b border-zinc-800/60 bg-zinc-950/40 text-left text-[11px] uppercase tracking-wider text-zinc-500">
              <th className="text-left py-1">{tr('运行时', 'Runtime')}</th>
              <th className="text-left py-1">{tr('健康', 'Health')}</th>
              <th className="text-left py-1">{tr('能力', 'Capabilities')}</th>
              <th className="text-left py-1">{tr('容量', 'Capacity')}</th>
              <th className="text-left py-1">{tr('版本', 'Version')}</th>
              <th className="text-left py-1">digest</th>
              {isAdmin && <th className="text-left py-1">{tr('治理', 'Governance')}</th>}
            </tr>
          </thead>
          <tbody>
            {items.map((rt) => (
              <RuntimeRow key={rt.runtime_id} rt={rt} isAdmin={isAdmin} />
            ))}
          </tbody>
        </table>
      )}
      <div className="mt-2 text-[11px] text-zinc-500">
        {tr('registry 行持有心跳时戳，projection 行是观察者实时投影；两者按 runtime_id 去重，注册行优先。',
            'registry rows carry heartbeat timestamps, projection rows are live observer snapshots; deduped by runtime_id with the registry row winning.')}
      </div>
    </Card>
  );
}

function RuntimeRow({ rt, isAdmin }: { rt: RuntimeView; isAdmin: boolean }) {
  const { tr } = useI18n();
  const badge = HEALTH_BADGE[rt.health] ?? HEALTH_FALLBACK;
  const caps = Object.entries(rt.capabilities ?? {});
  const hasCapacity = rt.total_slots > 0 || rt.free_slots > 0 || rt.active_tasks > 0;
  const version = rt.observed_version
    ? `${rt.observed_version}${rt.version_source ? ` (${rt.version_source})` : ''}`
    : '—';
  return (
    <tr className="border-t border-zinc-800/60 align-top">
      <td className="py-1.5 font-mono">
        <div className="text-zinc-200">{rt.runtime_id || '—'}</div>
        <div className="text-[10px] text-zinc-500">
          {rt.kind || '—'}
          {rt.source && <span className="ml-1 text-zinc-600">· {rt.source}</span>}
        </div>
      </td>
      <td className="py-1.5">
        <span
          className={cn('inline-block px-1.5 py-0.5 text-[10px] rounded border', badge.tone)}
          title={rt.health_reason || rt.health || 'unknown'}
        >
          {tr(badge.zh, badge.en)}
        </span>
        {rt.health_reason && (
          <div className="mt-0.5 max-w-40 truncate text-[10px] text-amber-400/80" title={rt.health_reason}>
            {truncate(rt.health_reason, 48)}
          </div>
        )}
        {rt.last_heartbeat_at && (
          <div className="mt-0.5 text-[10px] text-zinc-600">{rt.last_heartbeat_at}</div>
        )}
      </td>
      <td className="py-1.5">
        {caps.length === 0 ? (
          <span className="text-zinc-600">—</span>
        ) : (
          <div className="flex flex-wrap gap-1 max-w-56">
            {caps.map(([k, v]) => (
              <span
                key={k}
                className="px-1.5 py-0.5 text-[10px] rounded border border-zinc-700 font-mono text-zinc-400"
                title={`${k}: ${String(v)}`}
              >
                {k}: {String(v)}
              </span>
            ))}
          </div>
        )}
      </td>
      <td className="py-1.5 font-mono text-zinc-300 whitespace-nowrap">
        {hasCapacity ? (
          <>
            {tr('槽位', 'slots')} {rt.free_slots}/{rt.total_slots}
            <span className="ml-1 text-zinc-500">· {tr('当前任务', 'tasks')} {rt.active_tasks}</span>
          </>
        ) : (
          <span className="text-zinc-600">—</span>
        )}
      </td>
      <td className="py-1.5 font-mono text-zinc-300">{version}</td>
      <td className="py-1.5 font-mono text-zinc-500">
        {rt.artifact_digest ? <span title={rt.artifact_digest}>{truncate(rt.artifact_digest, 14)}</span> : '—'}
      </td>
      {isAdmin && (
        <td className="py-1.5">
          <div className="text-[11px] text-zinc-400">
            {rt.owner && <div>owner: {rt.owner}</div>}
            {rt.deployment_location && <div>loc: {rt.deployment_location}</div>}
          </div>
          {Object.keys(rt.labels ?? {}).length > 0 && (
            <div className="mt-0.5 flex flex-wrap gap-1 max-w-40">
              {Object.entries(rt.labels).map(([k, v]) => (
                <span key={k} className="px-1 py-0.5 text-[10px] rounded border border-zinc-800 font-mono text-zinc-500" title={`${k}: ${v}`}>
                  {k}={v}
                </span>
              ))}
            </div>
          )}
        </td>
      )}
    </tr>
  );
}

function DeploymentGrid({ resp }: { resp: DeploymentResponse }) {
  return (
    <div className="grid grid-cols-1 md:grid-cols-2 gap-3 mb-4">
      <Card>
        <div className="flex items-center gap-2 mb-2">
          <Server className="h-4 w-4 text-indigo-400" />
          <div className="text-sm font-medium">Manager</div>
          <span className="ml-auto border-b border-zinc-800/60 bg-zinc-950/40 text-left text-[11px] uppercase tracking-wider text-zinc-500">runtime</span>
        </div>
        <Row k="version" v={resp.manager_version || '—'} mono />
        <Row k="go" v={resp.go_version} mono />
        <Row k="os/arch" v={resp.os_arch} mono />
        {resp.build_at && <Row k="build_at" v={resp.build_at} mono />}
      </Card>
      <Card>
        <div className="flex items-center gap-2 mb-2">
          <GitBranch className="h-4 w-4 text-emerald-400" />
          <div className="text-sm font-medium">TeamHarness Plugin</div>
          <span className="ml-auto border-b border-zinc-800/60 bg-zinc-950/40 text-left text-[11px] uppercase tracking-wider text-zinc-500">disk</span>
        </div>
        <Row k="id" v={resp.plugin.id} />
        <Row k="name" v={resp.plugin.name} />
        <Row k="version" v={resp.plugin.version} mono />
        {resp.plugin.entry?.dashboard && <Row k="dashboard" v={resp.plugin.entry.dashboard} mono />}
        {resp.plugin.description && <Row k="description" v={truncate(resp.plugin.description, 80)} />}
      </Card>
    </div>
  );
}

// Health-card vocabulary. The backend hands back the same status keys the
// settings → health page uses (ok/degraded/error/pending…); a missing key
// means "not probed yet", which used to render as a bare em-dash inside a
// bordered box — indistinguishable from a broken panel.
const HEALTH_KEYS = ['db', 'prom', 'logs', 'traces', 'llm'] as const;
const HEALTH_LABEL: Record<string, { zh: string; en: string }> = {
  db: { zh: '数据库', en: 'Database' },
  prom: { zh: 'Prometheus', en: 'Prometheus' },
  logs: { zh: 'Loki 日志', en: 'Loki logs' },
  traces: { zh: 'Tempo 链路', en: 'Tempo traces' },
  llm: { zh: 'LLM 推理', en: 'LLM' },
};
const HEALTH_STATUS: Record<string, { zh: string; en: string; tone: string }> = {
  ok: { zh: '正常', en: 'OK', tone: 'text-emerald-400' },
  success: { zh: '正常', en: 'OK', tone: 'text-emerald-400' },
  degraded: { zh: '降级', en: 'Degraded', tone: 'text-amber-400' },
  warning: { zh: '降级', en: 'Degraded', tone: 'text-amber-400' },
  failed: { zh: '异常', en: 'Failed', tone: 'text-red-400' },
  error: { zh: '异常', en: 'Error', tone: 'text-red-400' },
  pending: { zh: '待检查', en: 'Pending', tone: 'text-zinc-400' },
  running: { zh: '运行中', en: 'Running', tone: 'text-sky-400' },
  skipped: { zh: '未启用', en: 'Not enabled', tone: 'text-zinc-500' },
};
const HEALTH_UNKNOWN = { zh: '未知', en: 'Unknown', tone: 'text-zinc-500' };

function HealthCard({ health }: { health: HealthSummary }) {
  const { tr } = useI18n();
  const overallTone = STATUS_TONE[health.overall] || STATUS_TONE.pending;
  const overallLabel = HEALTH_STATUS[health.overall] ?? HEALTH_UNKNOWN;
  return (
    <Card className="mb-4">
      <div className="flex items-center gap-2 mb-3">
        <Activity className="h-4 w-4 text-sky-400" />
        <div className="text-sm font-medium">{tr('健康检查', 'Health check')}</div>
        <span
          className={cn('ml-auto px-2 py-0.5 text-[10px] uppercase tracking-wider rounded border', overallTone)}
          title={health.overall || 'unknown'}
        >
          {tr(overallLabel.zh, overallLabel.en)}
        </span>
      </div>
      <div className="grid grid-cols-2 gap-2 md:grid-cols-5">
        {HEALTH_KEYS.map((k) => {
          const raw = health[k];
          const meta = HEALTH_STATUS[raw ?? ''] ?? HEALTH_UNKNOWN;
          return (
            <div
              key={k}
              className="rounded-lg border border-border-soft bg-card px-2.5 py-2 text-center"
              title={raw || tr('尚未探测', 'Not probed yet')}
            >
              <div className="text-[10px] uppercase tracking-wider text-zinc-500">
                {tr(HEALTH_LABEL[k].zh, HEALTH_LABEL[k].en)}
              </div>
              <div className={cn('mt-0.5 text-xs font-medium', meta.tone)}>
                {tr(meta.zh, meta.en)}
              </div>
            </div>
          );
        })}
      </div>
      {health.note && <div className="mt-2 text-[11px] text-zinc-500">{health.note}</div>}
    </Card>
  );
}

function DependenciesCard({ deps }: { deps: DeployDependency[] }) {
  const { tr } = useI18n();
  return (
    <Card className="mb-4">
      <div className="flex items-center gap-2 mb-2">
        <Database className="h-4 w-4 text-amber-400" />
        <div className="text-sm font-medium">{tr('部署依赖', 'Deploy dependencies')}</div>
        <span className="ml-auto rounded border border-border-soft px-1.5 py-0.5 text-[10px] uppercase tracking-wider text-zinc-500">
          {tr('环境变量', 'env')}
        </span>
      </div>
      <table className="w-full text-xs">
        <thead>
          <tr className="border-b border-zinc-800/60 bg-zinc-950/40 text-left text-[11px] uppercase tracking-wider text-zinc-500">
            <th className="text-left py-1">{tr('名称', 'Name')}</th>
            <th className="text-left py-1">{tr('已配置', 'Configured')}</th>
            <th className="text-left py-1">{tr('必需', 'Required')}</th>
            <th className="text-left py-1">{tr('说明', 'Note')}</th>
          </tr>
        </thead>
        <tbody>
          {deps.length === 0 ? (
            <tr>
              <td colSpan={4} className="py-4 text-center text-zinc-500">
                {tr('暂无部署依赖数据', 'No deployment dependencies')}
              </td>
            </tr>
          ) : deps.map((d) => (
            <tr key={d.name} className="border-t border-zinc-800/60">
              <td className="py-1.5 font-mono">{d.name}</td>
              <td className="py-1.5">
                {d.configured ? (
                  <span className="inline-flex items-center gap-1 text-emerald-400">
                    <CheckCircle2 className="h-3 w-3" />
                    {tr('是', 'yes')}
                  </span>
                ) : (
                  <span className="inline-flex items-center gap-1 text-zinc-500">
                    <XCircle className="h-3 w-3" />
                    {tr('否', 'no')}
                  </span>
                )}
              </td>
              <td className="py-1.5">
                {d.required ? (
                  <span className="text-amber-400">{tr('必需', 'required')}</span>
                ) : (
                  <span className="text-zinc-500">{tr('可选', 'optional')}</span>
                )}
              </td>
              <td className="py-1.5 text-zinc-400">{d.note || ''}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </Card>
  );
}

function RecoveryExampleCard({ rec }: { rec: RecoveryOperation }) {
  const { tr } = useI18n();
  const audit = rec.audit_kinds || [];
  return (
    <Card className="mb-4">
      <div className="flex items-center gap-2 mb-2">
        <ShieldCheck className="h-4 w-4 text-emerald-400" />
        <div className="text-sm font-medium">{tr('恢复演练', 'Recovery example')}</div>
        <span className="ml-auto rounded border border-border-soft px-1.5 py-0.5 text-[10px] uppercase tracking-wider text-zinc-500">
          {tr('主场景', 'Main scenario')}
        </span>
      </div>
      <div className="grid grid-cols-2 md:grid-cols-4 gap-2 mb-3">
        <KPI label="incident" value={rec.incident_id} />
        <KPI label="fault_family" value={rec.fault_family} />
        <KPI label="phases" value={`${rec.phases_observed}/${rec.phases_expected}`} tone={rec.phases_observed === rec.phases_expected ? 'ok' : 'pending'} />
        <KPI label="closed" value={rec.closed ? 'yes' : 'no'} tone={rec.closed ? 'ok' : 'warning'} />
      </div>
      <div className="text-[11px] text-zinc-500 mb-2">
        Worked incident: {rec.title}
      </div>
      <div className="flex flex-wrap items-center gap-1 mb-3">
        {(audit || []).map((k) => (
          <span key={k} className={cn('px-1.5 py-0.5 text-[10px] rounded border font-mono', STATUS_TONE.success)}>{k}</span>
        ))}
        {rec.recovery_signal && (
          <span className={cn('px-1.5 py-0.5 text-[10px] rounded border font-mono', STATUS_TONE.success)}>recovery_signal=true</span>
        )}
      </div>
      <table className="w-full text-xs">
        <thead>
          <tr className="border-b border-zinc-800/60 bg-zinc-950/40 text-left text-[11px] uppercase tracking-wider text-zinc-500">
            <th className="text-left py-1">phase</th>
            <th className="text-left py-1">role</th>
            <th className="text-left py-1">skill_ver</th>
            <th className="text-left py-1">duration</th>
            <th className="text-left py-1">summary</th>
          </tr>
        </thead>
        <tbody>
          {(rec.phases || []).length === 0 ? (
            <tr>
              <td colSpan={5} className="py-4 text-center text-zinc-500">
                {tr('暂无恢复阶段数据', 'No recovery phases')}
              </td>
            </tr>
          ) : (rec.phases || []).map((p: TimelinePhaseLite) => (
            <tr key={p.phase} className="border-t border-zinc-800/60">
              <td className="py-1.5 font-mono">
                <span className="inline-flex items-center gap-1">
                  <StatusDot status={p.status} />{p.phase}
                </span>
              </td>
              <td className="py-1.5 font-mono text-zinc-300">{p.worker_role}</td>
              <td className="py-1.5 font-mono text-zinc-400">{p.skill_version}</td>
              <td className="py-1.5 font-mono text-zinc-400">{p.duration || '—'}</td>
              <td className="py-1.5 text-zinc-300">{truncate(p.summary || '', 80)}</td>
            </tr>
          ))}
        </tbody>
      </table>
      {rec.evidence && Object.keys(rec.evidence).length > 0 && (
        <div className="mt-3">
          <div className="border-b border-zinc-800/60 bg-zinc-950/40 text-left text-[11px] uppercase tracking-wider text-zinc-500 mb-1">evidence_refs</div>
          <div className="flex flex-wrap gap-1">
            {Object.entries(rec.evidence).map(([k, v]) => (
              <span key={k} className="px-1.5 py-0.5 text-[10px] rounded border border-zinc-700 font-mono text-zinc-400">{k}: {v}</span>
            ))}
          </div>
        </div>
      )}
    </Card>
  );
}

function SkillsCard({ skills }: { skills: SkillVersion[] }) {
  const { tr } = useI18n();
  return (
    <Card className="mb-4">
      <div className="flex items-center gap-2 mb-2">
        <Bot className="h-4 w-4 text-fuchsia-400" />
        <div className="text-sm font-medium">Worker Skills</div>
        <span className="ml-auto border-b border-zinc-800/60 bg-zinc-950/40 text-left text-[11px] uppercase tracking-wider text-zinc-500">skill_meta.yaml</span>
      </div>
      <div className="grid grid-cols-1 md:grid-cols-2 gap-1">
        {skills.length === 0 ? (
          <div className="px-2 py-4 rounded border border-zinc-800/60 bg-zinc-900/40 text-xs text-zinc-500">
            {tr('暂无 Worker Skills 数据', 'No worker skills')}
          </div>
        ) : skills.map((s) => (
          <div key={s.id} className="flex items-center gap-2 px-2 py-1 rounded border border-zinc-800/60 bg-zinc-900/40">
            <span className="font-mono text-xs">{s.id}</span>
            <span className="ml-auto font-mono text-[11px] text-zinc-400">{s.version || '—'}</span>
          </div>
        ))}
      </div>
    </Card>
  );
}

function Row({ k, v, mono = false }: { k: string; v: string; mono?: boolean }) {
  return (
    <div className="flex items-center justify-between py-1 text-xs border-t border-zinc-800/60 first:border-0">
      <span className="text-zinc-500">{k}</span>
      <span className={cn('text-zinc-200', mono && 'font-mono')}>{v}</span>
    </div>
  );
}

function KPI({ label, value, tone }: { label: string; value: string; tone?: string }) {
  return (
    <div className="px-2 py-1.5 rounded border border-zinc-800/60 bg-zinc-900/40">
      <div className="border-b border-zinc-800/60 bg-zinc-950/40 text-left text-[11px] uppercase tracking-wider text-zinc-500">{label}</div>
      <div className={cn('mt-0.5 text-sm font-mono', tone && STATUS_TONE[tone] ? '' : 'text-zinc-100')}>{value}</div>
    </div>
  );
}

function StatusDot({ status }: { status: string }) {
  if (status === 'ok' || status === 'success') return <CheckCircle2 className="h-3 w-3 text-emerald-400" />;
  if (status === 'failed' || status === 'error') return <AlertTriangle className="h-3 w-3 text-red-400" />;
  const cls = STATUS_TONE[status] || STATUS_TONE.pending;
  return <span className={cn('inline-block w-2 h-2 rounded-full border', cls)} />;
}

function truncate(s: string, max: number) {
  if (!s) return '';
  return s.length <= max ? s : s.slice(0, max - 1) + '…';
}
