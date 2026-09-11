import Link from 'next/link';
import { Github, BookOpen, ShieldCheck, GitBranch } from 'lucide-react';
import { BrandMark } from '@/components/brand-mark';
import { SITE_ZH, FOOTER_COLS_ZH } from '@/lib/site-zh';

export function SiteFooterZh() {
  return (
    <footer className="border-t border-white/5 bg-ink-950">
      <div className="mx-auto max-w-7xl px-4 sm:px-6 lg:px-8">
        <div className="grid gap-10 py-14 md:grid-cols-4">
          <div>
            <BrandMark />
            <p className="mt-4 max-w-xs text-sm leading-relaxed text-ink-400">
              {SITE_ZH.description}
            </p>
            <div className="mt-6 flex items-center gap-2">
              <Link
                href={SITE_ZH.repo}
                aria-label="GitHub"
                className="inline-flex h-9 w-9 items-center justify-center rounded-md border border-white/10 bg-white/5 text-ink-200 hover:bg-white/10 hover:text-white"
              >
                <Github className="h-4 w-4" />
              </Link>
              <Link
                href="/zh/docs"
                aria-label="文档"
                className="inline-flex h-9 w-9 items-center justify-center rounded-md border border-white/10 bg-white/5 text-ink-200 hover:bg-white/10 hover:text-white"
              >
                <BookOpen className="h-4 w-4" />
              </Link>
              <Link
                href="/zh/security"
                aria-label="安全"
                className="inline-flex h-9 w-9 items-center justify-center rounded-md border border-white/10 bg-white/5 text-ink-200 hover:bg-white/10 hover:text-white"
              >
                <ShieldCheck className="h-4 w-4" />
              </Link>
              <Link
                href="/zh/changelog"
                aria-label="更新日志"
                className="inline-flex h-9 w-9 items-center justify-center rounded-md border border-white/10 bg-white/5 text-ink-200 hover:bg-white/10 hover:text-white"
              >
                <GitBranch className="h-4 w-4" />
              </Link>
            </div>
          </div>
          {FOOTER_COLS_ZH.map((c) => (
            <div key={c.title}>
              <div className="text-xs font-semibold uppercase tracking-wider text-ink-400">
                {c.title}
              </div>
              <ul className="mt-4 space-y-2 text-sm">
                {c.links.map((l) => (
                  <li key={l.href}>
                    <Link
                      href={l.href}
                      className="text-ink-200 transition-colors hover:text-white"
                    >
                      {l.label}
                    </Link>
                  </li>
                ))}
              </ul>
            </div>
          ))}
        </div>
        <div className="flex flex-col gap-3 border-t border-white/5 py-6 text-xs text-ink-400 md:flex-row md:items-center md:justify-between">
          <div>© {new Date().getFullYear()} OpsKeeper Authors. 基于 {SITE_ZH.license} 协议发布。</div>
          <div className="flex items-center gap-4">
            <span>版本 {SITE_ZH.version}</span>
            <span aria-hidden>·</span>
            <Link href="/zh/trademark" className="hover:text-white">商标</Link>
            <span aria-hidden>·</span>
            <Link href="/zh/brand" className="hover:text-white">品牌</Link>
          </div>
        </div>
      </div>
    </footer>
  );
}
