// PhaseStatusDot — 6px 状态原子点。
// 闭环时间线 7 阶段（detected → correlated → investigated → critiqued →
// approved → recovered → postmortem）共用同一组语义色：pending / running /
// success / failed / skipped。
//
// 设计要点：
//   - 仅 6px（w-1.5 h-1.5），与 mockup 01 锁定一致
//   - `running` 状态允许 animate-pulse（AGENTS.md 红线：仅圆点）
//   - role="status" + aria-label 让屏幕阅读器朗读阶段语义
//   - 颜色走 AGENTS.md 配色：zinc(待/跳) / sky(运行) / emerald(成功) / red(失败)
import { cn } from '@/lib/cn';

export type PhaseStatus = 'pending' | 'running' | 'success' | 'failed' | 'skipped';

interface PhaseStatusDotProps {
  status: PhaseStatus;
  className?: string;
  /** Override the default aria-label (otherwise it follows `status`). */
  ariaLabel?: string;
}

const STATUS_COLOR: Record<PhaseStatus, string> = {
  pending: 'bg-zinc-700',
  running: 'bg-sky-500',
  success: 'bg-emerald-500',
  failed: 'bg-red-500',
  skipped: 'bg-zinc-600',
};

const STATUS_LABEL_ZH: Record<PhaseStatus, string> = {
  pending: '待执行',
  running: '运行中',
  success: '成功',
  failed: '失败',
  skipped: '已跳过',
};

const STATUS_LABEL_EN: Record<PhaseStatus, string> = {
  pending: 'Pending',
  running: 'Running',
  success: 'Success',
  failed: 'Failed',
  skipped: 'Skipped',
};

export function PhaseStatusDot({ status, className, ariaLabel }: PhaseStatusDotProps) {
  // Minimal locale peek — keeps this leaf component from depending on
  // the i18n React context (so it can render in tests / non-React
  // call sites). The chip/page consumers handle full reactivity via
  // useI18n() — see PhaseIndicator / ProcessTimeline.
  const isZh = ((): boolean => {
    if (typeof localStorage === 'undefined') return true;
    const stored = localStorage.getItem('opskeeper-locale');
    if (stored === 'en-US' || stored === 'zh-CN') return stored === 'zh-CN';
    if (typeof navigator === 'undefined') return true;
    const tz = Intl.DateTimeFormat().resolvedOptions().timeZone || '';
    if (
      ['Asia/Shanghai', 'Asia/Chongqing', 'Asia/Urumqi', 'Asia/Harbin', 'Asia/Hong_Kong', 'Asia/Macau'].includes(tz)
    ) {
      return true;
    }
    return !(navigator.language || '').toLowerCase().startsWith('zh');
  })();
  const label = ariaLabel ?? (isZh ? STATUS_LABEL_ZH[status] : STATUS_LABEL_EN[status]);

  return (
    <span
      className={cn(
        'inline-block w-1.5 h-1.5 rounded-full',
        STATUS_COLOR[status],
        status === 'running' && 'animate-pulse',
        className,
      )}
      role="status"
      aria-label={label}
    />
  );
}
