// PhaseIndicator — 单阶段指示器（chip 形态）。
// 由 ProcessTimeline 调用：渲染「状态点 + 阶段名 + 状态 chip + 时长」。
//
// 设计要点：
//   - 阶段名走 i18n（detected/correlated/... 中英映射）
//   - 状态 chip 复用项目内 opskeeper/ui Chip（tone 6 选 1）
//   - 时长走 monospace + zinc-500（小一号：11px），保持 "满屏正常态灰字"
//     哲学（AGENTS.md 红线）
import { useI18n } from '@/i18n/locale';
import { Chip } from '@/components/ui/Chip';
import { cn } from '@/lib/cn';
import { PhaseStatusDot, type PhaseStatus } from './PhaseStatusDot';

const PHASE_NAME_ZH: Record<string, string> = {
  detected: '检测到',
  correlated: '关联',
  investigated: '调查',
  critiqued: '评审',
  approved: '审批',
  recovered: '恢复',
  postmortem: '复盘',
};

const PHASE_NAME_EN: Record<string, string> = {
  detected: 'Detected',
  correlated: 'Correlated',
  investigated: 'Investigated',
  critiqued: 'Critiqued',
  approved: 'Approved',
  recovered: 'Recovered',
  postmortem: 'Postmortem',
};

const STATUS_TONE: Record<PhaseStatus, 'default' | 'success' | 'danger' | 'info'> = {
  pending: 'default',
  running: 'info',
  success: 'success',
  failed: 'danger',
  skipped: 'default',
};

interface PhaseIndicatorProps {
  /** 阶段 ID（detected / correlated / ...），用于查表得到展示名 */
  phase: string;
  status: PhaseStatus;
  /** 形如 "+0ms · 12ms" 或 "28s" 的相对耗时（可选） */
  duration?: string;
  className?: string;
}

export function PhaseIndicator({ phase, status, duration, className }: PhaseIndicatorProps) {
  const { tr } = useI18n();
  const label = PHASE_NAME_ZH[phase] ?? phase;
  const labelEn = PHASE_NAME_EN[phase] ?? phase;

  return (
    <div className={cn('flex items-center gap-2', className)}>
      <PhaseStatusDot status={status} />
      <span className="text-sm font-medium text-zinc-100">{tr(label, labelEn)}</span>
      <Chip tone={STATUS_TONE[status]} dense>
        {status}
      </Chip>
      {duration && (
        <span className="text-[11px] text-zinc-500 font-mono tabular-nums">{duration}</span>
      )}
    </div>
  );
}
