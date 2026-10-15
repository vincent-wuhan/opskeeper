import { AlertTriangle, CheckCircle2, Pause, Siren } from 'lucide-react';
import { cn } from '@/lib/cn';
import { useI18n } from '@/i18n/locale';

// Incident severity + status chips.
//
// Both were copy-pasted between the alerts list and the incident detail
// page, and both printed their raw token, so a Chinese console showed
// 「级别 critical」「状态 acknowledged」. Severity stays a code (it is a
// scale, and the rule editor lists the same tokens) but the status — a
// state, not a level — now reads as a word in the operator's language.

const SEVERITY_STYLE: Record<string, string> = {
  critical: 'bg-red-500/15 text-red-300 ring-red-500/40',
  warning: 'bg-amber-500/10 text-amber-300 ring-amber-500/30',
};

export function SeverityBadge({ severity, className }: { severity: string; className?: string }) {
  const style = SEVERITY_STYLE[severity] ?? 'bg-zinc-800 text-zinc-300 ring-zinc-700';
  return (
    <span
      className={cn(
        'inline-flex items-center gap-1 rounded-md px-1.5 py-0.5 text-[11px] font-medium ring-1 ring-inset',
        style,
        className,
      )}
    >
      {severity === 'critical' ? <Siren size={11} /> : <AlertTriangle size={11} />}
      {severity}
    </span>
  );
}

const STATUS_STYLE: Record<string, string> = {
  open: 'bg-red-500/10 text-red-300 ring-red-500/30',
  acknowledged: 'bg-blue-500/10 text-blue-300 ring-blue-500/30',
  silenced: 'bg-zinc-700 text-zinc-300 ring-zinc-600',
  resolved: 'bg-emerald-500/10 text-emerald-300 ring-emerald-500/30',
};

const STATUS_LABEL: Record<string, { zh: string; en: string }> = {
  open: { zh: '未确认', en: 'open' },
  acknowledged: { zh: '已确认', en: 'acknowledged' },
  silenced: { zh: '已静默', en: 'silenced' },
  resolved: { zh: '已解决', en: 'resolved' },
};

export function IncidentStatusBadge({ status, className }: { status: string; className?: string }) {
  const { tr } = useI18n();
  const label = STATUS_LABEL[status];
  return (
    <span
      className={cn(
        'inline-flex items-center gap-1 rounded-md px-1.5 py-0.5 text-[11px] font-medium ring-1 ring-inset',
        STATUS_STYLE[status] ?? STATUS_STYLE.silenced,
        className,
      )}
    >
      {status === 'resolved' ? (
        <CheckCircle2 size={11} />
      ) : status === 'silenced' ? (
        <Pause size={11} />
      ) : (
        <span className="h-1.5 w-1.5 rounded-full bg-current" />
      )}
      {label ? tr(label.zh, label.en) : status}
    </span>
  );
}
