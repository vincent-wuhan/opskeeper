import type { PreviewDecisionSummary } from '@/lib/demo-types';
import type { DemoLocale } from '@/lib/demo-locale';
import { cn } from '@/lib/utils';

export function PreviewDecisionCard({
  decision,
  locale,
}: {
  decision: PreviewDecisionSummary | null;
  locale: DemoLocale;
}) {
  const comparable = Boolean(
    decision &&
      decision.replay_profile_id &&
      !decision.boundary_text.toUpperCase().includes('NOT COMPARABLE'),
  );

  return (
    <section className="rounded-xl border border-white/10 bg-white/[0.03] p-5" aria-labelledby="preview-decision-title">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h3 id="preview-decision-title" className="text-lg font-semibold text-white">
            {locale === 'zh' ? '修复预演决策' : 'Repair preview decision'}
          </h3>
          <p className="mt-1 text-sm text-ink-300">
            {locale === 'zh'
              ? '只展示进入人工审批前的紧凑证据；完整对比表在事故关闭后由档案页回看。'
              : 'Compact evidence before HITL; the full comparison is available in the archive after closure.'}
          </p>
        </div>
        {decision && (
          <span
            className={cn(
              'rounded-full border px-3 py-1 font-mono text-xs',
              comparable
                ? 'border-accent-500/30 bg-accent-500/10 text-accent-200'
                : 'border-amber-400/30 bg-amber-400/10 text-amber-200',
            )}
          >
            {locale === 'zh' ? (comparable ? '可对比' : '不可对比') : comparable ? 'Comparable' : 'Not comparable'}
          </span>
        )}
      </div>

      <p className="mt-4 rounded-lg border border-white/10 bg-white/[0.04] p-3 text-sm leading-relaxed text-ink-200">
        {locale === 'zh' ? '受控负载边界：' : 'Controlled workload boundary: '}
        {decision?.boundary_text || (locale === 'zh'
          ? 'preview-pg 只重放固定负载，不复制原实例活动会话。'
          : 'preview-pg replays a fixed workload and never copies active sessions from the source instance.')}
      </p>

      <dl className="mt-4 grid gap-3 text-sm sm:grid-cols-2">
        <div className="rounded-lg border border-white/10 bg-white/[0.02] p-4">
          <dt className="text-xs text-ink-400">Candidate A</dt>
          <dd className="mt-2 font-mono text-white">{decision?.candidate_a || (locale === 'zh' ? '未获得可用候选' : 'No usable candidate')}</dd>
          <p className={cn('mt-2 text-xs', decision?.eligible_for_hitl ? 'text-accent-200' : 'text-ink-400')}>
            {decision?.eligible_for_hitl
              ? locale === 'zh' ? 'PASS · 仅获得人工审批资格' : 'PASS · Eligible for human approval only'
              : locale === 'zh' ? '未获得人工审批资格' : 'Not eligible for human approval'}
          </p>
        </div>
        <div className="rounded-lg border border-white/10 bg-white/[0.02] p-4">
          <dt className="text-xs text-ink-400">Candidate B</dt>
          <dd className="mt-2 font-mono text-white">{decision?.candidate_b || (locale === 'zh' ? '未返回' : 'Not returned')}</dd>
          <p className="mt-2 text-xs text-rose-200">{locale === 'zh' ? '预演拒绝 · 不进入人工审批' : 'Preview rejected · No human approval'}</p>
        </div>
      </dl>

      {decision?.replay_profile_id && (
        <p className="mt-3 break-all font-mono text-xs text-ink-400">
          replay_profile_id: {decision.replay_profile_id}
        </p>
      )}
    </section>
  );
}
