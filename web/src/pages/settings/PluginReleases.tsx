import { useCallback, useEffect, useState } from 'react';
import {
  AlertTriangle,
  CheckCircle2,
  Clock,
  Loader2,
  RefreshCw,
  RotateCcw,
  Rocket,
  ShieldAlert,
  XCircle,
} from 'lucide-react';
import { ApiError } from '@/api/client';
import {
  advanceRelease,
  getRelease,
  haltRelease,
  listReleases,
  rollbackRelease,
  startRelease,
  type ReleaseStatus,
} from '@/api/pluginReleases';
import { Modal } from '@/components/Modal';
import { Button, Card, Chip, EmptyState, PageHeader } from '@/components/ui';
import { useI18n } from '@/i18n/locale';
import { usePermissions } from '@/store/me';

// Plugin releases — the console half of /v1/plugins/releases.
//
// A release puts new code, including L2 tools that can restart services,
// onto hosts. Three things about it shape this page:
//
//   1. It is a job, not a request. So there is no "done" spinner; the page
//      polls, and the operator drives the waves.
//   2. `pending` is not `failed`. A node the wave has not heard from blocks
//      the release and must look different from a node that refused. One is
//      "we do not know yet"; the other is an answer.
//   3. halt and rollback are separate buttons, because stopping a release
//      and taking it back off are separate decisions and an operator
//      watching a canary go bad wants the first one immediately.

const inputClass =
  'w-full rounded-md border border-zinc-800 bg-zinc-950/60 px-2.5 py-1.5 text-[13px] text-zinc-100 placeholder:text-zinc-600 focus:border-zinc-600 focus:outline-none disabled:cursor-not-allowed disabled:opacity-60';

function statusChip(s: ReleaseStatus) {
  if (s.rolled_back) return <Chip tone="danger">已回滚</Chip>;
  if (s.halted) return <Chip tone="warning">已停止</Chip>;
  if ((s.pending?.length ?? 0) > 0) return <Chip tone="info">等待节点回音</Chip>;
  if ((s.failed?.length ?? 0) > 0) return <Chip tone="danger">有节点失败</Chip>;
  if (s.wave >= s.waves && s.waves > 0) return <Chip tone="success">全部波次已发</Chip>;
  return <Chip tone="accent">进行中</Chip>;
}

export default function PluginReleasesPage() {
  const { tr } = useI18n();
  const { isAdmin } = usePermissions();

  const [items, setItems] = useState<ReleaseStatus[]>([]);
  const [loading, setLoading] = useState(true);
  const [err, setErr] = useState<string | null>(null);
  const [busy, setBusy] = useState<string | null>(null);
  const [detail, setDetail] = useState<ReleaseStatus | null>(null);
  const [haltFor, setHaltFor] = useState<ReleaseStatus | null>(null);
  const [haltReason, setHaltReason] = useState('');
  const [rollbackFor, setRollbackFor] = useState<ReleaseStatus | null>(null);
  const [startOpen, setStartOpen] = useState(false);

  const load = useCallback(async () => {
    try {
      setItems(await listReleases());
      setErr(null);
    } catch (e) {
      // 503 is "the tunnel is not wired on this manager", not a failure of
      // the release. Saying so saves a log dive.
      if (e instanceof ApiError && e.status === 503) {
        setErr(tr('插件发布通道未接线（manager 未挂载 tunnel）', 'The plugin release transport is not wired on this manager'));
      } else {
        setErr(e instanceof Error ? e.message : String(e));
      }
    } finally {
      setLoading(false);
    }
  }, [tr]);

  useEffect(() => {
    void load();
  }, [load]);

  // Poll while anything is in flight. A release is a job; a page that only
  // fetched on mount would show a canary frozen at wave 1 forever.
  useEffect(() => {
    const anyLive = items.some((s) => !s.halted && !s.rolled_back && (s.wave < s.waves || (s.pending?.length ?? 0) > 0));
    if (!anyLive) return;
    const t = window.setInterval(() => void load(), 4000);
    return () => window.clearInterval(t);
  }, [items, load]);

  const run = useCallback(
    async (name: string, fn: () => Promise<unknown>) => {
      setBusy(name);
      try {
        await fn();
        await load();
        setErr(null);
      } catch (e) {
        setErr(e instanceof Error ? e.message : String(e));
      } finally {
        setBusy(null);
      }
    },
    [load]
  );

  const openDetail = useCallback(async (name: string) => {
    try {
      setDetail(await getRelease(name));
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    }
  }, []);

  return (
    <div className="flex h-full flex-col">
      <PageHeader
        title="插件发布"
        subtitle="把插件包推到节点舰队。一次发布是一条作业：逐波推进，随时停止，随时收回。"
        actions={
          <>
            <Button variant="ghost" onClick={() => void load()} disabled={loading}>
              {loading ? <Loader2 className="h-3.5 w-3.5 animate-spin" /> : <RefreshCw className="h-3.5 w-3.5" />}
              刷新
            </Button>
            {isAdmin && (
              <Button onClick={() => setStartOpen(true)}>
                <Rocket className="h-3.5 w-3.5" />
                新建发布
              </Button>
            )}
          </>
        }
      />

      <div className="flex-1 overflow-y-auto px-6 py-4">
        {err && (
          <Card className="mb-3 border-red-900/60 bg-red-950/30">
            <div className="flex items-start gap-2 text-[13px] text-red-200">
              <AlertTriangle className="mt-0.5 h-4 w-4 shrink-0" />
              <span className="break-all">{err}</span>
            </div>
          </Card>
        )}

        {!loading && items.length === 0 && (
          <EmptyState
            icon={Rocket}
            title={tr('没有进行中的发布', 'No releases in flight')}
            hint={tr(
              '插件包由节点自行拉取、验签、审核后才激活。这个页面只负责调度，不接触包内容。',
              'Nodes fetch, verify and review a package themselves before activating it. This page only schedules.'
            )}
          />
        )}

        <div className="space-y-3">
          {items.map((s) => (
            <Card key={s.plugin} interactive className="p-4">
              <div className="flex items-start justify-between gap-4">
                <div className="min-w-0">
                  <div className="flex items-center gap-2">
                    <span className="truncate text-[13px] font-medium text-zinc-100">{s.plugin}</span>
                    <Chip dense>{s.version}</Chip>
                    {statusChip(s)}
                  </div>
                  <div className="mt-1.5 flex flex-wrap items-center gap-x-3 gap-y-1 text-[11px] text-zinc-500">
                    <span className="inline-flex items-center gap-1">
                      <Clock className="h-3 w-3" />
                      波次 {s.wave}/{s.waves}
                    </span>
                    {(s.pending?.length ?? 0) > 0 && (
                      <span className="inline-flex items-center gap-1 text-sky-300">
                        <Loader2 className="h-3 w-3 animate-spin" />
                        {s.pending!.length} 个节点未回音（不前进）
                      </span>
                    )}
                    {(s.failed?.length ?? 0) > 0 && (
                      <span className="inline-flex items-center gap-1 text-red-300">
                        <XCircle className="h-3 w-3" />
                        {s.failed!.length} 个节点失败 · {s.failed!.join(', ')}
                      </span>
                    )}
                    {s.summary && <span className="truncate">{s.summary}</span>}
                  </div>
                  {s.reason && <div className="mt-1 text-[11px] text-amber-300/80">{s.reason}</div>}
                </div>

                <div className="flex shrink-0 items-center gap-2">
                  <Button variant="ghost" onClick={() => void openDetail(s.plugin)}>
                    详情
                  </Button>
                  {isAdmin && !s.halted && !s.rolled_back && (
                    <>
                      <Button
                        variant="ghost"
                        disabled={busy === s.plugin || (s.pending?.length ?? 0) > 0}
                        title={
                          (s.pending?.length ?? 0) > 0
                            ? '当前波还有节点未回音；发布不会前进'
                            : '发送下一波'
                        }
                        onClick={() => void run(s.plugin, async () => {
                          const r = await advanceRelease(s.plugin);
                          if (!r.moved) {
                            throw new Error('当前波尚未全部回音，发布没有前进');
                          }
                        })}
                      >
                        推进
                      </Button>
                      <Button variant="ghost" onClick={() => { setHaltFor(s); setHaltReason(''); }}>
                        停止
                      </Button>
                    </>
                  )}
                  {isAdmin && !s.rolled_back && (
                    <Button variant="ghost" onClick={() => setRollbackFor(s)}>
                      <RotateCcw className="h-3.5 w-3.5" />
                      回滚
                    </Button>
                  )}
                </div>
              </div>
            </Card>
          ))}
        </div>
      </div>

      {startOpen && (
        <StartReleaseDialog
          onClose={() => setStartOpen(false)}
          onStarted={() => {
            setStartOpen(false);
            void load();
          }}
        />
      )}

      {detail && (
        <Modal open title={`发布详情 · ${detail.plugin}`} onClose={() => setDetail(null)}>
          <pre className="max-h-[60vh] overflow-auto rounded-md bg-zinc-950/60 p-3 text-[11px] leading-relaxed text-zinc-300">
            {JSON.stringify(detail, null, 2)}
          </pre>
        </Modal>
      )}

      {haltFor && (
        <Modal open title={`停止发布 · ${haltFor.plugin}`} onClose={() => setHaltFor(null)}>
          <p className="mb-3 text-[12px] text-zinc-400">
            停止会拦住后续波次，但<strong className="text-zinc-200">不会</strong>把已经装上的包摘下来。
            要摘下来请用「回滚」——那是另一次调用，因为「现在别继续」和「把已经做的撤掉」是两个决定。
          </p>
          <input
            className={inputClass}
            placeholder="原因（会写进审计行，读的人通常不是写的人）"
            value={haltReason}
            onChange={(e) => setHaltReason(e.target.value)}
          />
          <div className="mt-3 flex justify-end gap-2">
            <Button variant="ghost" onClick={() => setHaltFor(null)}>取消</Button>
            <Button
              onClick={() =>
                void run(haltFor.plugin, async () => {
                  await haltRelease(haltFor.plugin, haltReason);
                  setHaltFor(null);
                })
              }
            >
              停止
            </Button>
          </div>
        </Modal>
      )}

      {rollbackFor && (
        <Modal open title={`回滚 · ${rollbackFor.plugin}`} onClose={() => setRollbackFor(null)}>
          <div className="flex items-start gap-2 text-[12px] text-amber-200">
            <ShieldAlert className="mt-0.5 h-4 w-4 shrink-0" />
            <span>
              回滚把节点上的包还原到发布前的版本。没被这一波触达过的节点不会收到删除请求——
              那会摘掉运维自己装的东西。
            </span>
          </div>
          <div className="mt-3 flex justify-end gap-2">
            <Button variant="ghost" onClick={() => setRollbackFor(null)}>取消</Button>
            <Button
              onClick={() =>
                void run(rollbackFor.plugin, async () => {
                  try {
                    await rollbackRelease(rollbackFor.plugin);
                  } catch (e) {
                    // A partial rollback returns 502 with the status naming
                    // the nodes that still hold the package. That list is
                    // the whole point of the answer — surface it rather
                    // than showing only the error string.
                    if (e instanceof ApiError && e.payload && typeof e.payload === 'object') {
                      const payload = e.payload as { status?: ReleaseStatus };
                      if (payload.status) setDetail(payload.status);
                    }
                    throw e;
                  }
                  setRollbackFor(null);
                })
              }
            >
              回滚
            </Button>
          </div>
        </Modal>
      )}
    </div>
  );
}

function StartReleaseDialog({
  onClose,
  onStarted,
}: {
  onClose: () => void;
  onStarted: () => void;
}) {
  const [plugin, setPlugin] = useState('');
  const [version, setVersion] = useState('');
  const [url, setURL] = useState('');
  const [sha256, setSHA] = useState('');
  const [signature, setSignature] = useState('');
  const [keyID, setKeyID] = useState('');
  const [strategy, setStrategy] = useState<'rolling' | 'pin'>('rolling');
  const [nodes, setNodes] = useState('');
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  const submit = async () => {
    setBusy(true);
    try {
      const parsed = nodes
        .split(',')
        .map((s) => s.trim())
        .filter(Boolean)
        .map((s) => Number(s));
      if (parsed.some((n) => !Number.isFinite(n) || n <= 0)) {
        throw new Error('节点列表里有不是正整数的一项');
      }
      await startRelease({
        plugin,
        version,
        url,
        sha256,
        signature,
        key_id: keyID || undefined,
        strategy,
        nodes: parsed.length ? parsed : undefined,
      });
      onStarted();
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Modal open title="新建插件发布" onClose={onClose}>
      <div className="space-y-2.5">
        {err && (
          <div className="flex items-start gap-2 rounded-md border border-red-900/60 bg-red-950/30 p-2 text-[12px] text-red-200">
            <AlertTriangle className="mt-0.5 h-3.5 w-3.5 shrink-0" />
            <span className="break-all">{err}</span>
          </div>
        )}
        <div className="grid grid-cols-2 gap-2.5">
          <label className="space-y-1">
            <span className="text-[11px] text-zinc-500">包名</span>
            <input className={inputClass} value={plugin} onChange={(e) => setPlugin(e.target.value)} placeholder="opskeeper-sre-repair" />
          </label>
          <label className="space-y-1">
            <span className="text-[11px] text-zinc-500">版本</span>
            <input className={inputClass} value={version} onChange={(e) => setVersion(e.target.value)} placeholder="0.1.0" />
          </label>
        </div>
        <label className="block space-y-1">
          <span className="text-[11px] text-zinc-500">包地址（节点会自行拉取）</span>
          <input className={inputClass} value={url} onChange={(e) => setURL(e.target.value)} placeholder="https://…/pkg.tar.gz" />
        </label>
        <label className="block space-y-1">
          <span className="text-[11px] text-zinc-500">sha256</span>
          <input className={inputClass} value={sha256} onChange={(e) => setSHA(e.target.value)} />
        </label>
        <label className="block space-y-1">
          <span className="text-[11px] text-zinc-500">签名（ed25519，对整个包目录）</span>
          <input className={inputClass} value={signature} onChange={(e) => setSignature(e.target.value)} />
        </label>
        <label className="block space-y-1">
          <span className="text-[11px] text-zinc-500">key_id（可选）</span>
          <input className={inputClass} value={keyID} onChange={(e) => setKeyID(e.target.value)} />
        </label>
        <div className="grid grid-cols-2 gap-2.5">
          <label className="space-y-1">
            <span className="text-[11px] text-zinc-500">策略（必填，不默认）</span>
            <select className={inputClass} value={strategy} onChange={(e) => setStrategy(e.target.value as 'rolling' | 'pin')}>
              <option value="rolling">rolling — 金丝雀逐波</option>
              <option value="pin">pin — 单波，不自动升级</option>
            </select>
          </label>
          <label className="space-y-1">
            <span className="text-[11px] text-zinc-500">节点（留空 = 全舰队）</span>
            <input className={inputClass} value={nodes} onChange={(e) => setNodes(e.target.value)} placeholder="1,2,3" />
          </label>
        </div>
        <div className="flex items-start gap-2 rounded-md bg-zinc-950/50 p-2 text-[11px] text-zinc-500">
          <CheckCircle2 className="mt-0.5 h-3.5 w-3.5 shrink-0 text-zinc-600" />
          <span>
            审核顺序是 <span className="text-zinc-300">签名 → 清单 → 准入 → 版本</span>，全部在节点上执行。
            控制面只负责调度，读不到包内容，也改不了节点的裁决。
          </span>
        </div>
      </div>
      <div className="mt-3 flex justify-end gap-2">
        <Button variant="ghost" onClick={onClose}>取消</Button>
        <Button onClick={() => void submit()} disabled={busy || !plugin || !version || !url || !sha256 || !signature}>
          {busy ? <Loader2 className="h-3.5 w-3.5 animate-spin" /> : <Rocket className="h-3.5 w-3.5" />}
          开始发布
        </Button>
      </div>
    </Modal>
  );
}
