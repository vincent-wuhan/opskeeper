// 结晶模式 — the review surface for cost crystallisation.
//
// The ledger promotes a (fault, fix) pattern after it has verified cleanly
// several times in a row, and renders the exact pig-ops.yaml a node would
// install. This page is where an operator *sees* that claim: "the platform
// intends to run this fix, on this target, with this exact argv, without a
// model in the path". Before it existed the promotion was reachable only from
// tests.
//
// What this page deliberately does NOT do is approve. There is no "install"
// button, because installing is the release console's job and it carries the
// same review and signature channel every other package travels. 落盘 is the
// furthest this page goes: it writes the draft into the manager's review root
// (`OPSKEEPER_PLUGIN_IMPORT_DIR`) so the operator can read it as a file and
// take it to release. The distinction matters — a button that installs from
// this page would be the platform deciding on the operator's behalf.
import { useCallback, useEffect, useState } from 'react';
import { AlertTriangle, FileText, Loader2, RefreshCw, Rocket, ShieldAlert, Sparkles } from 'lucide-react';
import { Link } from 'react-router-dom';

import { ApiError } from '@/api/client';
import {
  getCrystallized,
  listCrystallized,
  promoteCrystallized,
  type CrystallizedPolicy,
  type CrystallizedPattern,
} from '@/api/crystallized';
import { Button, Card, Chip, EmptyState, PageHeader } from '@/components/ui';
import { useI18n } from '@/i18n/locale';
import { usePermissions } from '@/store/me';

export default function CrystallizedPage() {
  const { tr } = useI18n();
  const { isAdmin } = usePermissions();
  const [items, setItems] = useState<CrystallizedPattern[]>([]);
  const [policy, setPolicy] = useState<CrystallizedPolicy | null>(null);
  const [observingSince, setObservingSince] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  const [err, setErr] = useState<string | null>(null);
  const [selected, setSelected] = useState<CrystallizedPattern | null>(null);

  const load = useCallback(async () => {
    setLoading(true);
    try {
      const r = await listCrystallized();
      setItems(r.items ?? []);
      setPolicy(r.policy ?? null);
      setObservingSince(r.observing_since ?? null);
      setErr(null);
    } catch (e) {
      // 503 with not-wired is a state, not a failure: this manager never
      // wired a ledger. Saying so beats an empty list that reads as
      // "nothing has been promoted anywhere".
      const msg = e instanceof ApiError ? e.message : (e as Error).message;
      setErr(msg);
      setItems([]);
      setObservingSince(null);
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    void load();
  }, [load]);

  return (
    <main className="flex h-full flex-col overflow-hidden">
      <PageHeader
        title={tr('自愈规则', 'Crystallised Runbooks')}
        subtitle={tr(
          '平台在多次验证通过后，准备在没有模型介入的情况下自己执行的修复。只读——安装仍走发布控制台',
          'Fixes the platform intends to run with no model in the path, after they verified cleanly several times. Read-only — installation still goes through the release console'
        )}
        actions={
          <>
            <Button variant="ghost" onClick={() => void load()} disabled={loading}>
              <RefreshCw className={loading ? 'h-3.5 w-3.5 animate-spin' : 'h-3.5 w-3.5'} />
              {tr('刷新', 'Refresh')}
            </Button>
            <Link to="/admin/plugins">
              <Button variant="ghost">
                <Rocket className="h-3.5 w-3.5" />
                {tr('发布控制台', 'Release console')}
              </Button>
            </Link>
          </>
        }
      />

      <div className="flex-1 overflow-auto p-6">
        {policy ? (
          <p className="mb-4 text-[11px] text-zinc-500">
            {tr(
              `晋升门槛：连续 ${policy.min_clean_streak} 次一次通过 · TTL 上限 ${Math.round(policy.max_ttl_seconds / 60)} 分钟 · 包前缀 ${policy.package_prefix}`,
              `Promotion: ${policy.min_clean_streak} consecutive first-try verifications · TTL cap ${Math.round(policy.max_ttl_seconds / 60)} min · prefix ${policy.package_prefix}`
            )}
          </p>
        ) : null}

        {err ? (
          <Card className="mb-4 flex items-start gap-2 border-amber-900/50 bg-amber-950/20">
            <AlertTriangle className="mt-0.5 h-4 w-4 shrink-0 text-amber-400" />
            <div className="text-xs text-amber-200/90">
              {err.includes('not wired') || err.includes('not-wired')
                ? tr(
                    '本管理面未接入成本结晶——没有账本可读。这不是“没有任何模式被晋升”。',
                    'Cost crystallisation is not wired on this manager: there is no ledger to read. This is not "nothing has been promoted".'
                  )
                : err}
            </div>
          </Card>
        ) : null}

        {!loading && items.length === 0 && !err ? (
          observingSince ? (
            <EmptyState
              icon={Sparkles}
              title={tr(
                `自 ${observingSince} 起还没有模式被晋升`,
                `No pattern has been promoted since ${observingSince}`
              )}
              hint={tr(
                '这是这个窗口的读数，不是全部历史的读数：结晶账本在内存里，管理面重启会清空它，' +
                  '已经攒下的连续通过次数不会跨重启保留。',
                'This is a reading of that window, not of all history: the crystallisation ledger is ' +
                  'in-memory, a manager restart empties it, and a streak does not survive one.'
              )}
            />
          ) : (
            <EmptyState
              icon={Sparkles}
              title={tr('还没有模式被晋升', 'No pattern has earned a runbook yet')}
              hint={tr(
                '同一个修复需要连续多次一次通过才会晋升；单次成功是轶事，不是规则',
                'A fix has to verify cleanly several times in a row; one success is an anecdote, not a rule'
              )}
            />
          )
        ) : null}

        <div className="space-y-3">
          {items.map((p) => (
            <PatternCard
              key={p.name}
              pattern={p}
              isAdmin={isAdmin}
              onInspect={() => setSelected(p)}
            />
          ))}
        </div>
      </div>

      {selected ? <DetailDrawer name={selected.name} onClose={() => setSelected(null)} /> : null}
    </main>
  );
}

function PatternCard({
  pattern,
  isAdmin,
  onInspect,
}: {
  pattern: CrystallizedPattern;
  isAdmin: boolean;
  onInspect: () => void;
}) {
  const { tr } = useI18n();
  const [busy, setBusy] = useState(false);
  const [note, setNote] = useState<string | null>(null);

  const onPromote = useCallback(async () => {
    setBusy(true);
    setNote(null);
    try {
      const r = await promoteCrystallized(pattern.name);
      setNote(tr(`草稿已写入 ${r.dir}，可在发布控制台送审`, `Draft written to ${r.dir}; take it to the release console`));
    } catch (e) {
      const msg = e instanceof ApiError ? e.message : (e as Error).message;
      setNote(
        e instanceof ApiError && e.status === 409
          ? tr('已存在同名草稿，未覆盖——请先处理上一份', 'A draft already exists; nothing was overwritten')
          : msg
      );
    } finally {
      setBusy(false);
    }
  }, [pattern.name, tr]);

  const safetyTone = pattern.safety_level === 'L3' ? 'danger' : pattern.safety_level === 'L2' ? 'warning' : 'default';

  return (
    <Card className="space-y-3">
      <div className="flex flex-wrap items-start justify-between gap-2">
        <div className="min-w-0">
          <div className="flex flex-wrap items-center gap-2">
            <span className="font-mono text-xs text-zinc-100">{pattern.name}</span>
            <Chip tone={safetyTone}>{pattern.safety_level}</Chip>
            <Chip>{pattern.class}</Chip>
            <Chip tone="success">
              {tr(`${pattern.streak} 连通过`, `${pattern.streak} clean`)}
            </Chip>
          </div>
          <div className="mt-1 text-[11px] text-zinc-500">
            {tr('故障', 'fault')}: <span className="text-zinc-300">{pattern.fault_kind}</span>
            {pattern.family ? ` (${pattern.family})` : ''} · {tr('目标', 'target')}:{' '}
            <span className="text-zinc-300">{pattern.target}</span>
          </div>
        </div>
        <div className="flex shrink-0 items-center gap-1.5">
          <Button variant="ghost" onClick={onInspect}>
            <FileText className="h-3.5 w-3.5" />
            {tr('查看声明', 'View declaration')}
          </Button>
          <Button variant="primary" onClick={() => void onPromote()} disabled={busy || !isAdmin}>
            {busy ? <Loader2 className="h-3.5 w-3.5 animate-spin" /> : <Rocket className="h-3.5 w-3.5" />}
            {tr('落盘送审', 'Write for review')}
          </Button>
        </div>
      </div>

      <div>
        <div className="text-[10px] uppercase tracking-wide text-zinc-600">{tr('将执行的命令', 'Command that will run')}</div>
        <pre className="mt-1 overflow-auto rounded bg-zinc-950 p-2 font-mono text-[11px] text-emerald-300/90">
          {pattern.argv.join(' ')}
        </pre>
      </div>

      <div className="flex flex-wrap items-center gap-x-4 gap-y-1 text-[11px] text-zinc-500">
        <span>
          {tr('触发', 'trigger')}:{' '}
          <span className="text-zinc-300">
            {pattern.trigger.metric} {pattern.trigger.kind === 'metric_above' ? '>' : pattern.trigger.kind}{' '}
            {pattern.trigger.threshold}
          </span>
        </span>
        <span>
          {tr('影响范围', 'blast radius')}: <span className="text-zinc-300">{pattern.blast_radius}</span>
        </span>
        <span>
          {tr('TTL', 'TTL')}: <span className="text-zinc-300">{pattern.ttl_seconds}s</span>
        </span>
        <span>
          {tr('晋升于', 'promoted')}: {pattern.promoted_at ? new Date(pattern.promoted_at).toLocaleString() : '—'}
        </span>
      </div>

      {pattern.evidence && pattern.evidence.length > 0 ? (
        <div className="text-[11px] text-zinc-600">
          {tr('依据的运行', 'Runs behind it')}: <span className="font-mono text-zinc-500">{pattern.evidence.join(', ')}</span>
        </div>
      ) : null}

      {note ? <div className="text-[11px] text-zinc-400">{note}</div> : null}
      {!isAdmin ? (
        <div className="text-[11px] text-zinc-600">
          {tr('只有管理员可以落盘送审。', 'Admin only.')}
        </div>
      ) : null}
    </Card>
  );
}

function DetailDrawer({ name, onClose }: { name: string; onClose: () => void }) {
  const { tr } = useI18n();
  const [yaml, setYaml] = useState<string | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    let alive = true;
    void (async () => {
      try {
        const r = await getCrystallized(name);
        if (alive) setYaml(r.yaml);
      } catch (e) {
        if (alive) setErr(e instanceof ApiError ? e.message : (e as Error).message);
      } finally {
        if (alive) setLoading(false);
      }
    })();
    return () => {
      alive = false;
    };
  }, [name]);

  return (
    <div className="fixed inset-0 z-40 flex justify-end bg-black/50" onClick={onClose}>
      <div
        className="flex h-full w-full max-w-2xl flex-col border-l border-zinc-800 bg-zinc-950"
        onClick={(e) => e.stopPropagation()}
      >
        <div className="flex items-center justify-between border-b border-zinc-800 px-4 py-3">
          <div className="flex items-center gap-2">
            <ShieldAlert className="h-4 w-4 text-zinc-400" />
            <span className="font-mono text-xs text-zinc-200">{name}</span>
          </div>
          <Button variant="ghost" onClick={onClose}>
            {tr('关闭', 'Close')}
          </Button>
        </div>
        <div className="flex-1 overflow-auto p-4">
          {loading ? (
            <div className="flex items-center gap-2 text-xs text-zinc-500">
              <Loader2 className="h-3.5 w-3.5 animate-spin" />
              {tr('加载中…', 'Loading…')}
            </div>
          ) : err ? (
            <div className="text-xs text-amber-300">{err}</div>
          ) : (
            <>
              <p className="mb-2 text-[11px] text-zinc-500">
                {tr(
                  '这是送审的那份 pig-ops.yaml 原文，含来源注释。审批在发布控制台完成，本页不安装。',
                  'The exact pig-ops.yaml that would be reviewed, provenance comments included. Approval happens in the release console; this page does not install.'
                )}
              </p>
              <pre className="whitespace-pre-wrap break-all rounded bg-zinc-900 p-3 font-mono text-[11px] text-zinc-300">
                {yaml}
              </pre>
            </>
          )}
        </div>
      </div>
    </div>
  );
}
