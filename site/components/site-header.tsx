'use client';

import Link from 'next/link';
import { usePathname } from 'next/navigation';
import { useState } from 'react';
import { Menu, X, Github } from 'lucide-react';
import { PRIMARY_NAV, SITE } from '@/lib/site';
import { BrandMark } from '@/components/brand-mark';
import { cn } from '@/lib/utils';

export function SiteHeader() {
  const pathname = usePathname();
  const [open, setOpen] = useState(false);

  return (
    <header className="sticky top-0 z-40 border-b border-white/5 bg-ink-950/80 backdrop-blur-md">
      <div className="mx-auto flex h-16 max-w-7xl items-center justify-between px-4 sm:px-6 lg:px-8">
        <div className="flex items-center gap-8">
          <BrandMark />
          <nav className="hidden md:flex items-center gap-1 text-sm">
            {PRIMARY_NAV.map((item) => {
              const active = pathname === item.href || pathname.startsWith(item.href + '/');
              return (
                <Link
                  key={item.href}
                  href={item.href}
                  className={cn(
                    'rounded-md px-3 py-2 transition-colors',
                    active
                      ? 'text-white bg-white/5'
                      : 'text-ink-300 hover:text-white hover:bg-white/5',
                  )}
                >
                  {item.label}
                </Link>
              );
            })}
          </nav>
        </div>
        <div className="hidden md:flex items-center gap-2">
          <Link
            href={SITE.repo}
            className="inline-flex items-center gap-2 rounded-md border border-white/10 bg-white/5 px-3 py-1.5 text-sm text-ink-100 hover:bg-white/10"
          >
            <Github className="h-4 w-4" />
            <span>{SITE.repoShort}</span>
          </Link>
          <Link
            href="/docs/getting-started"
            className="inline-flex items-center rounded-md bg-white px-3 py-1.5 text-sm font-medium text-ink-950 hover:bg-ink-100"
          >
            Get started
          </Link>
        </div>
        <button
          aria-label="Toggle navigation"
          onClick={() => setOpen((v) => !v)}
          className="md:hidden inline-flex items-center justify-center rounded-md border border-white/10 bg-white/5 p-2 text-ink-100"
        >
          {open ? <X className="h-5 w-5" /> : <Menu className="h-5 w-5" />}
        </button>
      </div>
      {open && (
        <div className="md:hidden border-t border-white/5 px-4 py-3">
          <nav className="flex flex-col gap-1 text-sm">
            {PRIMARY_NAV.map((item) => (
              <Link
                key={item.href}
                href={item.href}
                onClick={() => setOpen(false)}
                className="rounded-md px-3 py-2 text-ink-200 hover:bg-white/5 hover:text-white"
              >
                {item.label}
              </Link>
            ))}
            <div className="mt-2 flex items-center gap-2">
              <Link
                href={SITE.repo}
                className="inline-flex items-center gap-2 rounded-md border border-white/10 bg-white/5 px-3 py-1.5 text-sm text-ink-100"
              >
                <Github className="h-4 w-4" />
                GitHub
              </Link>
              <Link
                href="/docs/getting-started"
                className="inline-flex items-center rounded-md bg-white px-3 py-1.5 text-sm font-medium text-ink-950"
              >
                Get started
              </Link>
            </div>
          </nav>
        </div>
      )}
    </header>
  );
}
