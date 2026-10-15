// ClosedLoopTimeline — 闭环时间线页面。
// 路由：`/incidents/:id/loop`（沿用 IncidentDetail 的 incidentId 命名空间）。
//
// 设计要点：
//   - 顶部 4 个 rubric 指标卡：rca_accuracy / time_to_remediate /
//     approval_rate / recovery_pass_rate（spec loop-harness-rubric）
//   - 7 阶段时间线复用 ProcessTimeline（DPO 组件）
//   - 默认展开 failed / running 阶段（智能折叠，避免满屏展开让用户淹没
//     在细节里）
//   - 数据加载：首次请求 `/api/v1/loops/{id}/timeline`；后续用 usePoll
//     5s 轮询，遵循"fail-open"——后端无 WebSocket 时降级轮询即可
//   - 不依赖 wsfanout（项目内目前无 wsfanout 库，仅有 usePoll）
//
// 更新：
//   - 时间线 endpoint 现在返回 phases + rubric + chain，由后端
//     timeline_aggregate.go 聚合。前端不再二次解析 event log。
//   - 顶部新增 chain 覆盖徽章 + recovery_signal / closed 标签，
//     让审阅者快速判断证据完整度。
//   - 每阶段新增 audit（dispatch/approval/execution/verification/
//     close）+ worker_role / skill_version，让 Manager 派发、诊断
//     依据、审批绑定目标、fallback、执行身份在 Element 内连续可见。
import { useCallback, useEffect, useState } from 'react';
import {
  AlertTriangle,
  CheckCircle2,
  GitBranch,
  ListChecks,
  Shield,
  User as UserIcon,
} from 'lucide-react';
import { useParams } from 'react-router-dom';
import { request } from '@/api/client';
import { PageHeader } from '@/components/ui/PageHeader';
import { Card } from '@/components/ui/Card';
import {
  ProcessTimeline,
  type TimelinePhase,
} from '@/components/dpo';
import { useI18n } from '@/i18n/locale';
import { usePoll } from '@/lib/usePoll';
import { cn } from '@/lib/cn';

interface LoopRubric {
  rca_accuracy: number;
  time_to_remediate: string;
  approval_rate: number;
  recovery_pass_rate: number;
  phase_count: number;
  event_count: number;
  has_recovery_signal: boolean;
  has_closure: boolean;
}

interface TimelineAuditRow {
  kind: string;
  actor?: string;
  actor_role?: string;
  bound_target?: string;
  bound_params?: string;
  bound_scope?: string;
  action?: string;
  fallback?: string;
  fallback_cause?: string;
  at?: string;
  trace_id?: string;
  evidence_ref?: string;
  note?: string;
}

interface ChainMeta {
  phases_observed: number;
  phases_expected: number;
  coverage_pct: number;
  current_phase: string;
  final_phase: string;
  recovery_signal: boolean;
  closed: boolean;
  trace_ids?: string[];
}

interface TimelineResponse {
  phases: TimelinePhase[];
  rubric: LoopRubric | null;
  chain?: ChainMeta;
}

const POLL_MS = 5_000;

export default function ClosedLoopTimelinePage() {
  const { tr } = useI18n();
  const { id = '' } = useParams<{ id: string }>();
  const [phases, setPhases] = useState<TimelinePhase[]>([]);
  const [rubric, setRubric] = useState<LoopRubric | null>(null);
  const [chain, setChain] = useState<ChainMeta | null>(null);
  const [loading, setLoading] = useState(true);
  const [err, setErr] = useState<string | null>(null);

  const loadTimeline = useCallback(async () => {
    if (!id) return;
    try {
      const data = await request<TimelineResponse>(
        'GET',
        `/loops/${encodeURIComponent(id)}/timeline`,
      );
      setPhases(Array.isArray(data.phases) ? data.phases : []);
      setRubric(data.rubric ?? null);
      setChain(data.chain ?? null);
      setErr(null);
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setLoading(false);
    }
  }, [id]);

  useEffect(() => {
    void loadTimeline();
  }, [loadTimeline]);

  usePoll(loadTimeline, POLL_MS, !!id);

  // 智能折叠：默认展开 failed / running 阶段
  const defaultExpanded = phases
    .filter((p) => p.status === 'failed' || p.status === 'running')
    .map((p) => p.phase);

  return (
    <div className="px-6 py-5 max-w-5xl">
      <PageHeader
        title={tr('闭环时间线', 'Closed Loop Timeline')}
        subtitle={
          <span className="font-mono">
            incident_id: {id}
          </span>
        }
        actions={
          <div className="flex items-center gap-2">
            <button
              type="button"
              className="px-2.5 py-1 text-xs text-zinc-400 hover:text-zinc-100 border border-zinc-800/60 rounded transition-colors"
            >
              {tr('导出 Postmortem', 'Export Postmortem')}
            </button>
            <button
              type="button"
              className="px-2.5 py-1 text-xs text-white bg-indigo-600 hover:bg-indigo-500 rounded transition-colors"
            >
              {tr('强制终止', 'Force Stop')}
            </button>
          </div>
        }
      />

      {/* Chain footer: coverage + recovery_signal + closed */}
      {chain && (
        <ChainFooter chain={chain} />
      )}

      {/* rubric 4 指标 */}
      {rubric && (
        <div className="grid grid-cols-2 md:grid-cols-4 gap-3 mb-4">
          <RubricCard metric="rca_accuracy" value={rubric.rca_accuracy.toFixed(2)} />
          <RubricCard metric="time_to_remediate" value={rubric.time_to_remediate} />
          <RubricCard metric="approval_rate" value={rubric.approval_rate.toFixed(2)} />
          <RubricCard metric="recovery_pass_rate" value={rubric.recovery_pass_rate.toFixed(2)} />
        </div>
      )}

      {loading && phases.length === 0 ? (
        <Card className="text-center text-xs text-zinc-500">
          <div className="py-6">{tr('加载中…', 'Loading…')}</div>
        </Card>
      ) : err ? (
        <Card className="text-center text-xs text-red-400">
          <div className="py-6">
            {tr('加载失败', 'Failed to load')}：{err}
          </div>
        </Card>
      ) : (
        <ProcessTimeline phases={phases} defaultExpanded={defaultExpanded} />
      )}
    </div>
  );
}

function ChainFooter({ chain }: { chain: ChainMeta }) {
  const { tr } = useI18n();
  const coverageTone =
    chain.coverage_pct >= 0.99
      ? 'border-emerald-700/40 bg-emerald-500/10 text-emerald-300'
      : chain.coverage_pct >= 0.5
        ? 'border-amber-700/40 bg-amber-500/10 text-amber-300'
        : 'border-zinc-700 bg-zinc-800/40 text-zinc-300';
  return (
    <div className="flex flex-wrap items-center gap-2 mb-4">
      <span
        className={cn(
          'flex items-center gap-1.5 px-2 py-1 text-[11px] font-mono rounded border',
          coverageTone,
        )}
      >
        <GitBranch className="h-3 w-3" />
        {tr('覆盖', 'coverage')} {chain.phases_observed}/{chain.phases_expected}{' '}
        ({(chain.coverage_pct * 100).toFixed(0)}%)
      </span>
      {chain.recovery_signal && (
        <span className="flex items-center gap-1.5 px-2 py-1 text-[11px] font-mono rounded border border-emerald-700/40 bg-emerald-500/10 text-emerald-300">
          <Shield className="h-3 w-3" />
          recovery_signal=true
        </span>
      )}
      {chain.closed && (
        <span className="flex items-center gap-1.5 px-2 py-1 text-[11px] font-mono rounded border border-sky-700/40 bg-sky-500/10 text-sky-300">
          <CheckCircle2 className="h-3 w-3" />
          {tr(`已闭环 · ${chain.final_phase}`, `closed via ${chain.final_phase}`)}
        </span>
      )}
      {!chain.recovery_signal && chain.phases_observed > 0 && (
        <span className="flex items-center gap-1.5 px-2 py-1 text-[11px] font-mono rounded border border-zinc-700 bg-zinc-800/40 text-zinc-300">
          <AlertTriangle className="h-3 w-3" />
          {tr('尚未观测到恢复', 'recovery not yet observed')}
        </span>
      )}
      {chain.trace_ids && chain.trace_ids.length > 0 && (
        <span className="flex items-center gap-1.5 px-2 py-1 text-[11px] font-mono rounded border border-zinc-700 bg-zinc-800/40 text-zinc-300">
          <ListChecks className="h-3 w-3" />
          {chain.trace_ids.length} trace_id
          {chain.trace_ids.length === 1 ? '' : 's'}
        </span>
      )}
    </div>
  );
}

// The four rubric keys come straight off the API, so the tiles used to be
// captioned `rca_accuracy` / `time_to_remediate` in an otherwise Chinese
// page. Map them to words and keep the machine key as the tooltip.
const RUBRIC_LABEL: Record<string, { zh: string; en: string; hintZh: string; hintEn: string }> = {
  rca_accuracy: {
    zh: '根因准确率',
    en: 'RCA accuracy',
    hintZh: '诊断结论与最终确认的根因一致的占比',
    hintEn: 'Share of incidents whose diagnosis matched the confirmed root cause',
  },
  time_to_remediate: {
    zh: '平均修复耗时',
    en: 'Time to remediate',
    hintZh: '从告警触发到恢复完成的中位时长',
    hintEn: 'Median time from alert firing to recovery complete',
  },
  approval_rate: {
    zh: '审批通过率',
    en: 'Approval rate',
    hintZh: '提交审批后被批准的占比',
    hintEn: 'Share of submitted approvals that were approved',
  },
  recovery_pass_rate: {
    zh: '恢复通过率',
    en: 'Recovery pass rate',
    hintZh: '恢复动作通过校验的占比',
    hintEn: 'Share of recovery actions that passed verification',
  },
};

function RubricCard({ metric, value }: { metric: string; value: string }) {
  const { tr } = useI18n();
  const meta = RUBRIC_LABEL[metric];
  return (
    <div className="rounded-md border border-border-soft bg-card px-3 py-2.5">
      <div
        className="text-[11px] uppercase tracking-wider text-zinc-500"
        title={meta ? `${tr(meta.hintZh, meta.hintEn)} · ${metric}` : metric}
      >
        {meta ? tr(meta.zh, meta.en) : metric}
      </div>
      <div className="mt-0.5 text-base font-semibold text-zinc-100 font-mono tabular-nums">
        {value}
      </div>
    </div>
  );
}

// Re-export TimelineAuditRow + UserIcon so existing DPO
// tooling can import them. The audit rows themselves are rendered
// inline by ProcessTimeline; this file only needs the type for the
// future audit drawer.
export type { TimelineAuditRow, UserIcon };
