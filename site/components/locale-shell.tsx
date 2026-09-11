'use client';

import { useEffect } from 'react';
import { usePathname } from 'next/navigation';
import { SiteHeader } from '@/components/site-header';
import { SiteFooter } from '@/components/site-footer';
import { SiteHeaderZh } from '@/components/site-header-zh';
import { SiteFooterZh } from '@/components/site-footer-zh';

/**
 * LocaleShell decides which header/footer to render and keeps the
 * document-level `lang` attribute in sync with the URL prefix.
 * It must be rendered inside the root layout's <body>, because Next.js
 * App Router only allows a single <html> tag in the route tree.
 */
export function LocaleShell({ children }: { children: React.ReactNode }) {
  const pathname = usePathname() || '/';
  const isZh = pathname === '/zh' || pathname.startsWith('/zh/');

  useEffect(() => {
    if (typeof document !== 'undefined') {
      document.documentElement.lang = isZh ? 'zh-CN' : 'en';
    }
  }, [isZh]);

  return (
    <div className="relative isolate flex min-h-screen flex-col">
      <div className="pointer-events-none absolute inset-x-0 top-0 -z-10 h-[640px] bg-radial-fade" />
      {isZh ? <SiteHeaderZh /> : <SiteHeader />}
      <main className="flex-1">{children}</main>
      {isZh ? <SiteFooterZh /> : <SiteFooter />}
    </div>
  );
}
