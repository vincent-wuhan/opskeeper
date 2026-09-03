// ContractPanel — 阶段 contract 详细面板。
// 阶段间 contract（RootCauseJSON / CritiqueScore / ApprovalDecision /
// VerifiedDelta / PostmortemDoc）的结构化展示，用于 ProcessTimeline 展开
// 后 "看一眼就知道这一阶段产出了什么"。
//
// 设计要点：
//   - 用普通 div 而非 Card（避免双层 rounded-xl 视觉冗余）
//   - 字段以 key: value 行展示，值若是对象则 JSON.stringify
//   - 顶部带 phase 名 + 字段计数，方便审计
import { useI18n } from '@/i18n/locale';
import { cn } from '@/lib/cn';

interface ContractPanelProps {
  /** contract 字段表（key → value） */
  data: Record<string, unknown>;
  /** 阶段 ID，仅用于表头 */
  phase: string;
  className?: string;
}

function formatValue(v: unknown): string {
  if (v === null) return 'null';
  if (v === undefined) return '—';
  if (typeof v === 'string') return v;
  if (typeof v === 'number' || typeof v === 'boolean') return String(v);
  try {
    return JSON.stringify(v);
  } catch {
    return String(v);
  }
}

export function ContractPanel({ data, phase, className }: ContractPanelProps) {
  const { tr } = useI18n();
  const entries = Object.entries(data);
  return (
    <div
      className={cn(
        'rounded-md border border-zinc-800/60 bg-zinc-900/60',
        className,
      )}
    >
      <div className="px-3 py-2 text-[11px] font-mono text-zinc-500 border-b border-zinc-800/60 flex items-center justify-between">
        <span>
          {phase}{' '}
          <span className="text-zinc-600">·</span>{' '}
          {tr('contract', 'contract')}
        </span>
        <span>
          {entries.length} {tr('字段', 'fields')}
        </span>
      </div>
      <div className="p-3 space-y-1.5">
        {entries.length === 0 ? (
          <div className="text-[11px] text-zinc-500 italic">
            {tr('无字段', 'no fields')}
          </div>
        ) : (
          entries.map(([key, value]) => (
            <div key={key} className="flex items-start gap-2 text-[11px]">
              <span className="text-zinc-500 font-mono min-w-[140px] shrink-0">
                {key}:
              </span>
              <span className="text-zinc-300 font-mono break-all flex-1">
                {formatValue(value)}
              </span>
            </div>
          ))
        )}
      </div>
    </div>
  );
}
