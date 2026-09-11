import Link from 'next/link';
import { ArrowRight } from 'lucide-react';
import { cn } from '@/lib/utils';

type Variant = 'primary' | 'secondary' | 'ghost';

const variantClasses: Record<Variant, string> = {
  primary:
    'bg-white text-ink-950 hover:bg-ink-100 shadow-[0_0_0_1px_rgba(255,255,255,0.05)]',
  secondary:
    'bg-white/5 text-white border border-white/10 hover:bg-white/10',
  ghost: 'bg-transparent text-ink-100 hover:bg-white/5',
};

export function Button({
  href,
  children,
  variant = 'primary',
  className,
  trailingIcon = true,
  external = false,
}: {
  href: string;
  children: React.ReactNode;
  variant?: Variant;
  className?: string;
  trailingIcon?: boolean;
  external?: boolean;
}) {
  const cls = cn(
    'inline-flex items-center justify-center gap-2 rounded-md px-4 py-2 text-sm font-medium transition-colors',
    variantClasses[variant],
    className,
  );
  if (external) {
    return (
      <a href={href} className={cls} target="_blank" rel="noreferrer noopener">
        {children}
        {trailingIcon && <ArrowRight className="h-4 w-4" />}
      </a>
    );
  }
  return (
    <Link href={href} className={cls}>
      {children}
      {trailingIcon && <ArrowRight className="h-4 w-4" />}
    </Link>
  );
}
