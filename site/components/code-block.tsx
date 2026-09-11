import { cn } from '@/lib/utils';

export function CodeBlock({
  language,
  title,
  children,
  className,
}: {
  language?: string;
  title?: string;
  children: React.ReactNode;
  className?: string;
}) {
  return (
    <div
      className={cn(
        'overflow-hidden rounded-xl border border-white/10 bg-ink-900/70 shadow-soft',
        className,
      )}
    >
      {(language || title) && (
        <div className="flex items-center justify-between border-b border-white/5 bg-white/[0.02] px-4 py-2 text-xs">
          <div className="flex items-center gap-2 text-ink-400">
            <span className="h-2 w-2 rounded-full bg-accent-500" />
            <span className="font-mono uppercase tracking-wider">{title ?? language}</span>
          </div>
          <div className="flex items-center gap-1.5">
            <span className="h-2.5 w-2.5 rounded-full bg-white/15" />
            <span className="h-2.5 w-2.5 rounded-full bg-white/15" />
            <span className="h-2.5 w-2.5 rounded-full bg-white/15" />
          </div>
        </div>
      )}
      <pre className="overflow-x-auto p-4 text-[13px] leading-relaxed text-ink-100">
        <code className="font-mono">{children}</code>
      </pre>
    </div>
  );
}
