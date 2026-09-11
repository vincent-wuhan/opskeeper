import Link from 'next/link';
import { cn } from '@/lib/utils';

export function BrandMark({ className }: { className?: string }) {
  return (
    <Link
      href="/"
      className={cn(
        'group inline-flex items-center gap-2 text-ink-100 hover:text-white transition-colors',
        className,
      )}
    >
      <span
        aria-hidden
        className="relative inline-flex h-7 w-7 items-center justify-center rounded-md bg-gradient-to-br from-accent-400 to-accent-700 shadow-glow"
      >
        <span className="absolute inset-[3px] rounded-[5px] bg-ink-950/85" />
        <span className="relative font-mono text-[11px] font-semibold text-accent-300">OK</span>
      </span>
      <span className="text-[15px] font-semibold tracking-tight">OpsKeeper</span>
    </Link>
  );
}
