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
import { useCallback, useEffect, useState } from 'react';
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
}

interface TimelineResponse {
  phases: TimelinePhase[];
  rubric: LoopRubric | null;
}

const POLL_MS = 5_000;

export default function ClosedLoopTimelinePage() {
  const { tr } = useI18n();
  const { id = '' } = useParams<{ id: string }>();
  const [phases, setPhases] = useState<TimelinePhase[]>([]);
  const [rubric, setRubric] = useState<LoopRubric | null>(null);
  const [loading, setLoading] = useState(true);
  const [err, setErr] = useState<string | null>(null);

  const loadTimeline = useCallback(async () => {
    if (!id) return;
    try {
      const data = await request<TimelineResponse>(
        'GET',
        `/loops/${encodeURIComponent(id)}/timeline`
      );
      setPhases(Array.isArray(data.phases) ? data.phases : []);
      setRubric(data.rubric ?? null);
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

      {/* rubric 4 指标 */}
      {rubric && (
        <div className="grid grid-cols-2 md:grid-cols-4 gap-3 mb-4">
          <RubricCard
            label="rca_accuracy"
            value={rubric.rca_accuracy.toFixed(2)}
          />
          <RubricCard
            label="time_to_remediate"
            value={rubric.time_to_remediate}
          />
          <RubricCard
            label="approval_rate"
            value={rubric.approval_rate.toFixed(2)}
          />
          <RubricCard
            label="recovery_pass_rate"
            value={rubric.recovery_pass_rate.toFixed(2)}
          />
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

function RubricCard({ label, value }: { label: string; value: string }) {
  return (
    <div
      className={cn(
        'bg-zinc-900/40 border border-zinc-800/60 rounded-md px-3 py-2.5',
      )}
    >
      <div className="text-[10px] uppercase tracking-wider text-zinc-500">
        {label}
      </div>
      <div className="mt-0.5 text-base font-semibold text-zinc-100 font-mono tabular-nums">
        {value}
      </div>
    </div>
  );
}
