import { useCallback, useEffect, useRef, useState } from 'react';
import { ShieldCheck, RefreshCw, Check, X, ChevronDown, ChevronRight, Hourglass } from 'lucide-react';
import { listApprovals, approveApproval, rejectApproval, type Approval } from '@/api/approvals';
import { ApiError } from '@/api/client';
import { useI18n } from '@/i18n/locale';
import { Button, Chip, DependencyNotice, EmptyState, LoadingState, PageHeader } from '@/components/ui';
import { Modal } from '@/components/Modal';
import { parseSigners, dualSignState, signerWording } from '@/lib/approvalSigners';
import { blastRadiusLabel, riskClassLabel, riskClassTone } from '@/lib/riskClass';

// Approvals inbox (HLD-017 propose-confirm). Dangerous actions proposed by
// the agent (or a flow approval node) wait here; an admin approves (→ runs)
// or rejects. Default view = pending.

const STATUSES = ['pending', 'approved', 'executed', 'rejected', 'failed'] as const;

// 会话级批准结果。审批默认视图过滤 pending,批准后不能用重载列表来体现结果
// (重载会让已裁决行凭空消失)。按返回行 status 分流并把结果就地留驻,由 outcomes 承载。
type RowOutcome = { phase: 'waiting' | 'executed' | 'failed'; result?: string };

export default function ApprovalsPage() {
  const { tr } = useI18n();
  const [items, setItems] = useState<Approval[]>([]);
  const [status, setStatus] = useState<string>('pending');
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState('');
  const [err, setErr] = useState<string | null>(null);
  const [expanded, setExpanded] = useState<Record<string, boolean>>({});
  const [outcomes, setOutcomes] = useState<Record<string, RowOutcome>>({});
  const [confirmKind, setConfirmKind] = useState<'approve' | 'reject' | null>(null);
  const [confirmTarget, setConfirmTarget] = useState<Approval | null>(null);
  const [rejectReason, setRejectReason] = useState('');
  const cancelRef = useRef<HTMLButtonElement | null>(null);

  const openConfirm = useCallback((kind: 'approve' | 'reject', a: Approval) => {
    setConfirmKind(kind);
    setConfirmTarget(a);
    setRejectReason('');
  }, []);
  // 稳定引用:Modal 的聚焦 effect 依赖 onClose,若每次渲染都换新函数会重跑 effect,
  // 打字时(受控 textarea 每次输入都触发重渲染)把焦点反复抢回「取消」。
  const closeConfirm = useCallback(() => {
    setConfirmKind(null);
    setConfirmTarget(null);
    setRejectReason('');
  }, []);

  const load = useCallback(async () => {
    setLoading(true);
    try {
      const r = await listApprovals(status);
      setItems(r.items ?? []);
      setErr(null);
    } catch (e) {
      setErr(e instanceof ApiError ? e.message : (e as Error).message);
    } finally {
      setLoading(false);
    }
  }, [status]);

  useEffect(() => {
    void load();
  }, [load]);

  const onApprove = (a: Approval) => openConfirm('approve', a);

  const doApprove = async (a: Approval) => {
    setBusy(a.id);
    try {
      const row = await approveApproval(a.id);
      // 就地替换为返回行,不调用 load()——默认视图过滤 pending,重载会让
      // 已裁决行凭空消失。按返回行 status 分流,结果留驻可见。
      setItems((prev) => prev.map((it) => (it.id === a.id ? row : it)));
      const phase: RowOutcome['phase'] =
        row.status === 'executed' ? 'executed' : row.status === 'failed' ? 'failed' : 'waiting';
      setOutcomes((prev) => ({ ...prev, [a.id]: { phase, result: row.result } }));
      if (phase !== 'waiting') setExpanded((e) => ({ ...e, [a.id]: true }));
    } catch (e) {
      setErr(e instanceof ApiError ? e.message : (e as Error).message);
    } finally {
      setBusy('');
      closeConfirm();
    }
  };

  const onReject = (a: Approval) => openConfirm('reject', a);

  const doReject = async (a: Approval) => {
    setBusy(a.id);
    try {
      await rejectApproval(a.id, rejectReason);
      // 就地更新为已拒绝并展开,让原因可见;不调用 load()。
      setItems((prev) =>
        prev.map((it) => (it.id === a.id ? { ...it, status: 'rejected', reason: rejectReason } : it)),
      );
      setExpanded((e) => ({ ...e, [a.id]: true }));
    } catch (e) {
      setErr(e instanceof ApiError ? e.message : (e as Error).message);
    } finally {
      setBusy('');
      closeConfirm();
    }
  };

  return (
    <main className="anim-fade flex flex-1 flex-col overflow-hidden">
      <PageHeader
        title={tr('待确认', 'Approvals')}
        subtitle={tr('Agent / 工作流提交的危险操作，需人工批准后才执行', 'Dangerous actions proposed by agents / flows — execute only after a human approves')}
        actions={
          <button
            type="button"
            onClick={() => void load()}
            className="inline-flex items-center gap-1.5 rounded-md border border-zinc-700 px-2.5 py-1.5 text-[12px] text-zinc-300 hover:bg-zinc-800"
          >
            <RefreshCw size={13} />
            {tr('刷新', 'Refresh')}
          </button>
        }
        extra={
          <div className="-mb-2 flex items-center gap-1">
            {STATUSES.map((s) => (
              <button
                key={s}
                type="button"
                onClick={() => setStatus(s)}
                className={`rounded-md px-2.5 py-1 text-[12px] transition-colors ${
                  status === s ? 'bg-zinc-800 text-zinc-100' : 'text-zinc-500 hover:text-zinc-300'
                }`}
              >
                {statusLabel(s, tr)}
              </button>
            ))}
          </div>
        }
      />

      <div className="flex-1 overflow-auto px-6 py-4">
        {err && <DependencyNotice className="mb-3" message={err} />}
        {loading ? (
          <LoadingState />
        ) : items.length === 0 ? (
          <EmptyState
            icon={ShieldCheck}
            title={status === 'pending' ? tr('没有待确认的操作', 'No actions awaiting approval') : tr('暂无记录', 'Nothing here')}
            hint={
              status === 'pending'
                ? tr(
                    'Agent / 工作流提交的危险操作会先落到这里，人工批准后才执行。',
                    'Risky operations submitted by agents / workflows land here first and only run after a human approves.',
                  )
                : tr('换个状态页签看看，或等新的操作提交上来。', 'Try another status tab, or wait for new operations to come in.')
            }
          />
        ) : (
          <div className="space-y-2">
            {items.map((a) => {
              const outcome = outcomes[a.id];
              return (
                <div key={a.id} className="surface-card rounded-2xl p-3">
                  <div className="flex items-start gap-3">
                    <button
                      type="button"
                      onClick={() => setExpanded((e) => ({ ...e, [a.id]: !e[a.id] }))}
                      className="mt-0.5 text-zinc-500 hover:text-zinc-300"
                    >
                      {expanded[a.id] ? <ChevronDown size={16} /> : <ChevronRight size={16} />}
                    </button>
                    <div className="min-w-0 flex-1">
                      <div className="flex items-center gap-2">
                        <span className="font-medium text-zinc-100">{a.title}</span>
                        <StatusChip status={a.status} tr={tr} />
                        <span className="rounded bg-zinc-800 px-1.5 py-0.5 font-mono text-[10px] text-zinc-500">{a.kind}</span>
                      </div>
                      {a.summary && <div className="mt-1 whitespace-pre-wrap text-[12px] text-zinc-400">{a.summary}</div>}
                      {(a.blast_radius || a.risk_class || a.target) && (
                        <div className="mt-1 flex flex-wrap items-center gap-1.5">
                          {a.blast_radius && <Chip tone="warning" dense>{tr('影响面', 'Blast radius')}: {blastRadiusLabel(a.blast_radius, tr)}</Chip>}
                          {a.risk_class && <Chip tone={riskClassTone(a.risk_class)} dense>{tr('风险等级', 'Risk')}: {riskClassLabel(a.risk_class, tr)}</Chip>}
                          {a.target && <span className="font-mono text-[11px] text-zinc-400">{a.target}</span>}
                        </div>
                      )}
                      <div className="mt-1 text-[11px] text-zinc-600">
                        {tr('来源', 'source')}: {a.source}
                        {a.session_id ? ` · ${a.session_id.slice(0, 8)}` : ''} · {new Date(a.created_at).toLocaleString()}
                      </div>
                      <SignerProgress approval={a} />
                      {outcome?.phase === 'waiting' && (
                        <div className="mt-1.5 flex items-start gap-1.5 text-[11px] text-amber-400">
                          <Hourglass size={12} className="mt-0.5 shrink-0" />
                          <span>{tr('你的签名已记录,等待第二位批准人', 'Your signature is recorded — waiting for a second approver')}</span>
                        </div>
                      )}
                      {outcome?.phase === 'executed' && (
                        <div className="mt-1.5 text-[11px] text-emerald-400">{tr('已执行', 'Executed')}</div>
                      )}
                      {outcome?.phase === 'failed' && (
                        <div className="mt-1.5 text-[11px] text-red-400">{tr('执行失败', 'Failed')}</div>
                      )}
                      {expanded[a.id] && (
                        <div className="mt-2 space-y-1">
                          <div className="text-[11px] text-zinc-500">{tr('操作内容', 'Action payload')}</div>
                          <pre className="max-h-56 overflow-auto whitespace-pre-wrap break-all rounded bg-zinc-950 p-2 text-[10px] text-zinc-400">{prettify(a.payload)}</pre>
                          {a.result && (
                            <>
                              <div className="text-[11px] text-zinc-500">{tr('执行结果', 'Result')}</div>
                              <pre className="max-h-56 overflow-auto whitespace-pre-wrap break-all rounded bg-zinc-950 p-2 text-[10px] text-zinc-400">{prettify(a.result)}</pre>
                            </>
                          )}
                          {a.reason && <div className="text-[11px] text-amber-400/80">{tr('原因', 'reason')}: {a.reason}</div>}
                        </div>
                      )}
                    </div>
                    {a.status === 'pending' && (
                      <div className="flex shrink-0 items-center gap-1.5">
                        {outcome?.phase === 'waiting' ? (
                          <button
                            type="button"
                            disabled
                            className="inline-flex items-center gap-1 rounded-md border border-emerald-700 bg-emerald-950/30 px-2 py-1 text-[12px] text-emerald-300 disabled:opacity-40"
                          >
                            <Check size={13} />
                            {tr('已签署，等待第二位批准人', 'Signed — awaiting second approver')}
                          </button>
                        ) : (
                          <button
                            type="button"
                            onClick={() => void onApprove(a)}
                            disabled={busy === a.id}
                            className="inline-flex items-center gap-1 rounded-md border border-emerald-700 bg-emerald-950/30 px-2 py-1 text-[12px] text-emerald-300 hover:bg-emerald-900/40 disabled:opacity-40"
                          >
                            <Check size={13} />
                            {tr('批准', 'Approve')}
                          </button>
                        )}
                        <button
                          type="button"
                          onClick={() => void onReject(a)}
                          disabled={busy === a.id}
                          className="inline-flex items-center gap-1 rounded-md border border-zinc-700 px-2 py-1 text-[12px] text-zinc-400 hover:border-red-800 hover:text-red-400 disabled:opacity-40"
                        >
                          <X size={13} />
                          {tr('拒绝', 'Reject')}
                        </button>
                      </div>
                    )}
                  </div>
                </div>
              );
            })}
          </div>
        )}
      </div>

      <Modal
        open={confirmKind !== null}
        onClose={closeConfirm}
        title={confirmTarget?.title}
        size="sm"
        initialFocusRef={cancelRef}
        footer={
          <>
            <Button ref={cancelRef} onClick={closeConfirm}>
              {tr('取消', 'Cancel')}
            </Button>
            {confirmKind === 'approve' ? (
              <Button
                variant="primary"
                disabled={busy === confirmTarget?.id}
                onClick={() => confirmTarget && void doApprove(confirmTarget)}
              >
                {tr('确认批准并执行', 'Confirm approve & run')}
              </Button>
            ) : (
              <Button
                variant="danger"
                disabled={busy === confirmTarget?.id}
                onClick={() => confirmTarget && void doReject(confirmTarget)}
              >
                {tr('确认拒绝', 'Confirm reject')}
              </Button>
            )}
          </>
        }
      >
        {confirmKind === 'approve' ? (
          <div className="space-y-2 text-[13px] text-zinc-300">
            <p>{tr('批准后将立即执行下列操作，请确认影响范围。', 'Approving runs the action immediately — confirm the impact below.')}</p>
            <div className="flex flex-wrap items-center gap-1.5">
              {confirmTarget?.blast_radius && (
                <Chip tone="warning" dense>
                  {tr('影响面', 'Blast radius')}: {blastRadiusLabel(confirmTarget.blast_radius, tr)}
                </Chip>
              )}
              {confirmTarget?.risk_class && (
                <Chip tone={riskClassTone(confirmTarget.risk_class)} dense>
                  {tr('风险等级', 'Risk')}: {riskClassLabel(confirmTarget.risk_class, tr)}
                </Chip>
              )}
              {confirmTarget?.target && (
                <span className="font-mono text-[11px] text-zinc-400">{confirmTarget.target}</span>
              )}
            </div>
          </div>
        ) : (
          <label className="block text-[13px] text-zinc-300">
            <span className="mb-1 block">{tr('拒绝原因（可选）', 'Reject reason (optional)')}</span>
            <textarea
              value={rejectReason}
              onChange={(e) => setRejectReason(e.target.value)}
              placeholder={tr('拒绝原因（可选）', 'Reject reason (optional)')}
              className="h-24 w-full resize-none rounded-md border border-zinc-700 bg-zinc-950 px-2 py-1.5 text-[12px] text-zinc-200 outline-none focus:border-zinc-500"
            />
          </label>
        )}
      </Modal>
    </main>
  );
}

function statusLabel(s: string, tr: (zh: string, en: string) => string): string {
  switch (s) {
    case 'pending':
      return tr('待确认', 'Pending');
    case 'approved':
      return tr('已批准', 'Approved');
    case 'executed':
      return tr('已执行', 'Executed');
    case 'rejected':
      return tr('已拒绝', 'Rejected');
    case 'failed':
      return tr('失败', 'Failed');
    default:
      return s;
  }
}

// console-reskin delta spec：状态标签用 rounded-full 胶囊 + 呼吸点。
// 呼吸点复用 tailwind.config.ts 里既有的 pulse-dot keyframes（纯 opacity 明暗，
// 不是缩放/位移），并用内置 motion-safe 变体卡在 prefers-reduced-motion 之外——
// 开了「减少动态效果」的用户看到的是一个静止的圆点，而不是被动画反复闪。
// 圆点颜色与所在状态的语义色同系，不另引入色板。
function StatusChip({ status, tr }: { status: string; tr: (zh: string, en: string) => string }) {
  const cls =
    status === 'pending'
      ? 'bg-amber-900/40 text-amber-300'
      : status === 'executed' || status === 'approved'
        ? 'bg-emerald-900/40 text-emerald-300'
        : status === 'failed'
          ? 'bg-red-900/40 text-red-300'
          : 'bg-zinc-800 text-zinc-400';
  const dot =
    status === 'pending'
      ? 'bg-amber-400'
      : status === 'executed' || status === 'approved'
        ? 'bg-emerald-400'
        : status === 'failed'
          ? 'bg-red-400'
          : 'bg-zinc-500';
  return (
    <span className={`inline-flex items-center gap-1 rounded-full px-1.5 py-0.5 text-[10px] font-medium ${cls}`}>
      <span className={`h-1 w-1 shrink-0 rounded-full motion-safe:animate-pulse-dot ${dot}`} aria-hidden="true" />
      {statusLabel(status, tr)}
    </span>
  );
}

function prettify(s: string): string {
  try {
    return JSON.stringify(JSON.parse(s), null, 2);
  } catch {
    return s;
  }
}

// SignerProgress 呈现一行审批的双签进度,取自共享基元 approvalSigners。
// decided(非 pending) 不渲染;unknown 作中性提示,不禁用任何操作按钮。
function SignerProgress({ approval }: { approval: Approval }) {
  const { tr } = useI18n();
  const state = dualSignState(approval);
  if (state === 'decided') return null;
  const { signers } = parseSigners(approval.signers);
  const w = signerWording(state, signers.length);
  return (
    <div className="mt-1.5 space-y-0.5 text-[11px]">
      <div className={state === 'partial' ? 'text-amber-400/90' : 'text-zinc-500'}>
        {tr(w.zh, w.en)}
      </div>
      {state === 'partial' && (
        <ul className="flex flex-wrap gap-x-3 gap-y-0.5 text-zinc-500">
          {signers.map((s, i) => (
            <li key={`${s.user_id}-${i}`} className="font-mono">
              {s.role ?? String(s.user_id)}
              {s.at ? ` · ${new Date(s.at).toLocaleString()}` : ''}
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}
