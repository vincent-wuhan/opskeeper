import { Loader2 } from 'lucide-react';
import { cn } from '@/lib/cn';
import { tr } from '@/i18n/locale';

// LoadingState — the single loading indicator for list/panel bodies.
// Replaces the five hand-rolled variants the audit found (`py-16 text-center`,
// `h-40 …`, `h-60 …`, `Loader2 + text-xs`, `<Card h-24 …>`). `role="status"`
// + `aria-live` so assistive tech announces it; the spinner is motion-safe.
type Props = {
  /** Override the default "加载中… / Loading…" copy. */
  label?: string;
  /** Override the default height (h-40) for cramped or full-page contexts. */
  className?: string;
};

export function LoadingState({ label, className }: Props) {
  return (
    <div
      role="status"
      aria-live="polite"
      className={cn(
        'flex h-40 items-center justify-center gap-2 text-sm text-zinc-500',
        className,
      )}
    >
      <Loader2 size={16} className="motion-safe:animate-spin" />
      <span>{label ?? tr('加载中…', 'Loading…')}</span>
    </div>
  );
}
