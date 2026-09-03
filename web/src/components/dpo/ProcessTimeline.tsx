// ProcessTimeline — 闭环时间线主组件。
// 渲染 7 阶段（detected → correlated → investigated → critiqued →
// approved → recovered → postmortem）的纵向时间线，含：
//   - 左侧状态点 + 连接线
//   - 阶段主体：PhaseIndicator + 合同摘要
//   - 展开后：工具调用块（ToolCallBlock）+ 阶段 contract（ContractPanel）
//   - critiqued 阶段的子阶段（replay / depth / consistency）
//
// 设计要点：
//   - 整组阶段统一在一张 Card 内，用 `divide-y` 分行（AGENTS.md 红线）
//   - 行高 12px（py-3），不让满屏堆叠挤压
//   - ChevronRight 用 lucide-react，旋转 90° 表达展开态
//   - URL state 同步：默认展开 failed / running 阶段（智能折叠）
//   - a11y：role="button" + aria-expanded + Enter/Space 键盘可达
import { useCallback, useMemo, useState } from 'react';
import { ChevronRight } from 'lucide-react';
import { Card } from '@/components/ui/Card';
import { cn } from '@/lib/cn';
import { PhaseStatusDot, type PhaseStatus } from './PhaseStatusDot';
import { PhaseIndicator } from './PhaseIndicator';
import { ToolCallBlock } from './ToolCallBlock';
import { ContractPanel } from './ContractPanel';

export interface TimelineToolCall {
  name: string;
  args?: string;
  result?: string;
  status: PhaseStatus;
  latencyMs?: number;
}

export interface TimelineSubPhase {
  name: string;
  status: PhaseStatus;
}

export interface TimelinePhase {
  phase: string;
  status: PhaseStatus;
  /** 形如 "+50ms · 28s" 的相对耗时 */
  duration?: string;
  /** 阶段开始时间（ISO） */
  startedAt?: string;
  /** contract 摘要，单行文本，默认可见 */
  contractSummary?: string;
  /** contract 详情，展开后渲染为 ContractPanel */
  contractDetail?: Record<string, unknown>;
  /** 工具调用列表 */
  toolCalls?: TimelineToolCall[];
  /** 子阶段列表（仅 critiqued 用） */
  subPhases?: TimelineSubPhase[];
}

interface ProcessTimelineProps {
  phases: TimelinePhase[];
  /** 默认展开的 phase name 集合 */
  defaultExpanded?: string[];
  /** 点击阶段时回调（用于 URL state 同步） */
  onPhaseClick?: (phase: string) => void;
  className?: string;
}

export function ProcessTimeline({
  phases,
  defaultExpanded = [],
  onPhaseClick,
  className,
}: ProcessTimelineProps) {
  const [expanded, setExpanded] = useState<Set<string>>(
    () => new Set(defaultExpanded),
  );

  const toggle = useCallback(
    (phase: string) => {
      setExpanded((prev) => {
        const next = new Set(prev);
        if (next.has(phase)) next.delete(phase);
        else next.add(phase);
        return next;
      });
      onPhaseClick?.(phase);
    },
    [onPhaseClick],
  );

  // 按 mockup 顺序固定阶段 ID 列表，便于渲染稳定的连接线
  const orderedPhases = useMemo(
    () =>
      phases.map((p, idx) => ({
        ...p,
        __isLast: idx === phases.length - 1,
      })),
    [phases],
  );

  if (phases.length === 0) {
    return (
      <Card className={cn('text-center text-xs text-zinc-500', className)}>
        <div className="py-6">—</div>
      </Card>
    );
  }

  return (
    <Card className={cn('divide-y divide-zinc-800/60', className)}>
      {orderedPhases.map((p) => {
        const isExpanded = expanded.has(p.phase);
        return (
          <div
            key={p.phase}
            data-phase={p.phase}
            className={cn(
              'px-4 py-3 flex items-start gap-3 transition-colors',
              isExpanded && 'bg-zinc-900/60',
            )}
          >
            {/* 左侧状态点 + 连接线 */}
            <div className="mt-1 flex flex-col items-center">
              <PhaseStatusDot status={p.status} />
              {!p.__isLast && <span className="w-px h-6 bg-zinc-800 mt-1" />}
            </div>

            {/* 阶段主体 */}
            <div className="flex-1 min-w-0">
              <div
                className="flex items-center justify-between gap-2 cursor-pointer select-none"
                onClick={() => toggle(p.phase)}
                onKeyDown={(e) => {
                  if (e.key === 'Enter' || e.key === ' ') {
                    e.preventDefault();
                    toggle(p.phase);
                  }
                }}
                role="button"
                tabIndex={0}
                aria-expanded={isExpanded}
                aria-controls={`phase-detail-${p.phase}`}
              >
                <div className="flex items-center gap-2 flex-wrap min-w-0">
                  <PhaseIndicator
                    phase={p.phase}
                    status={p.status}
                    duration={p.duration}
                  />
                  {(() => {
                    const subs = p.subPhases;
                    if (!subs || subs.length === 0) return null;
                    return (
                      <div className="flex items-center gap-1.5 ml-1 flex-wrap">
                        {subs.map((sp, i) => (
                          <span key={sp.name} className="flex items-center gap-1.5">
                            <span
                              className={cn(
                                'text-[10px] px-1.5 py-0.5 rounded font-mono',
                                sp.status === 'success' && 'bg-emerald-500/10 text-emerald-300',
                                sp.status === 'running' && 'bg-sky-500/10 text-sky-300',
                                sp.status === 'failed' && 'bg-red-500/10 text-red-300',
                                sp.status === 'pending' && 'bg-zinc-800 text-zinc-400',
                                sp.status === 'skipped' && 'bg-zinc-800/60 text-zinc-500',
                              )}
                            >
                              {sp.name}
                            </span>
                            {i < subs.length - 1 && (
                              <span className="text-zinc-700">→</span>
                            )}
                          </span>
                        ))}
                      </div>
                    );
                  })()}
                </div>
                <ChevronRight
                  className={cn(
                    'w-3.5 h-3.5 text-zinc-500 transition-transform shrink-0',
                    isExpanded && 'rotate-90',
                  )}
                />
              </div>

              {/* 阶段 contract 摘要（默认可见） */}
              {p.contractSummary && (
                <div className="mt-1 text-xs text-zinc-400 break-words">
                  {p.contractSummary}
                </div>
              )}

              {/* 展开后的详情 */}
              {isExpanded && (
                <div
                  id={`phase-detail-${p.phase}`}
                  className="mt-2.5 space-y-2.5"
                >
                  {p.toolCalls && p.toolCalls.length > 0 && (
                    <div className="bg-zinc-900/60 border border-zinc-800/60 rounded-md divide-y divide-zinc-800/60">
                      {p.toolCalls.map((tc, i) => (
                        <ToolCallBlock key={`${tc.name}-${i}`} {...tc} />
                      ))}
                    </div>
                  )}

                  {p.contractDetail && Object.keys(p.contractDetail).length > 0 && (
                    <ContractPanel data={p.contractDetail} phase={p.phase} />
                  )}
                </div>
              )}
            </div>
          </div>
        );
      })}
    </Card>
  );
}
