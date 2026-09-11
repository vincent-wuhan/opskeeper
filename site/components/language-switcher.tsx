'use client';

import Link from 'next/link';
import { usePathname } from 'next/navigation';
import { Languages } from 'lucide-react';

/**
 * Strips an optional `/zh` prefix from a pathname so we can compute the
 * "other locale" version of the same route. Root `/` and `/zh` map cleanly.
 */
function stripLocale(pathname: string): string {
  if (pathname === '/zh' || pathname === '/zh/') return '/';
  if (pathname.startsWith('/zh/')) return pathname.slice(3) || '/';
  return pathname;
}

export function LanguageSwitcher({ className }: { className?: string }) {
  const pathname = usePathname() || '/';
  const isZh = pathname.startsWith('/zh');
  const target = isZh ? stripLocale(pathname) : `/zh${stripLocale(pathname)}`;
  const label = isZh ? 'EN' : '中文';

  return (
    <Link
      href={target}
      aria-label={isZh ? 'Switch to English' : '切换到中文'}
      className={
        className ??
        'inline-flex items-center gap-1.5 rounded-md border border-white/10 bg-white/5 px-2.5 py-1.5 text-xs text-ink-100 hover:bg-white/10'
      }
    >
      <Languages className="h-3.5 w-3.5" />
      <span className="font-medium">{label}</span>
    </Link>
  );
}
