import Link from 'next/link';
import { Github, BookOpen, ShieldCheck, GitBranch } from 'lucide-react';
import { BrandMark } from '@/components/brand-mark';
import { SITE } from '@/lib/site';

const cols: { title: string; links: { label: string; href: string }[] }[] = [
  {
    title: 'Product',
    links: [
      { label: 'Platform', href: '/platform' },
      { label: 'Worker roles', href: '/workers' },
      { label: 'Closed loop', href: '/platform#closed-loop' },
      { label: 'Live demo', href: '/demo' },
      { label: 'Safety boundary', href: '/security' },
      { label: 'Roadmap', href: '/roadmap' },
      { label: 'Changelog', href: '/changelog' },
    ],
  },
  {
    title: 'Docs',
    links: [
      { label: 'Getting started', href: '/docs/getting-started' },
      { label: 'Architecture', href: '/docs/architecture' },
      { label: 'Deployment', href: '/docs/deployment' },
      { label: 'Operations', href: '/docs/operations' },
      { label: 'API reference', href: '/docs/api' },
      { label: 'Integrations', href: '/docs/integrations' },
    ],
  },
  {
    title: 'Project',
    links: [
      { label: 'GitHub', href: SITE.repo },
      { label: 'Brand assets', href: '/brand' },
      { label: 'Security policy', href: '/security' },
      { label: 'Trademark', href: '/trademark' },
      { label: 'License', href: 'https://github.com/vincent-wuhan/opskeeper/blob/main/LICENSE' },
      { label: 'Code of conduct', href: 'https://github.com/vincent-wuhan/opskeeper/blob/main/CODE_OF_CONDUCT.md' },
    ],
  },
];

export function SiteFooter() {
  return (
    <footer className="border-t border-white/5 bg-ink-950">
      <div className="mx-auto max-w-7xl px-4 sm:px-6 lg:px-8">
        <div className="grid gap-10 py-14 md:grid-cols-4">
          <div>
            <BrandMark />
            <p className="mt-4 max-w-xs text-sm leading-relaxed text-ink-400">
              {SITE.description}
            </p>
            <div className="mt-6 flex items-center gap-2">
              <Link
                href={SITE.repo}
                aria-label="GitHub"
                className="inline-flex h-9 w-9 items-center justify-center rounded-md border border-white/10 bg-white/5 text-ink-200 hover:bg-white/10 hover:text-white"
              >
                <Github className="h-4 w-4" />
              </Link>
              <Link
                href="/docs"
                aria-label="Documentation"
                className="inline-flex h-9 w-9 items-center justify-center rounded-md border border-white/10 bg-white/5 text-ink-200 hover:bg-white/10 hover:text-white"
              >
                <BookOpen className="h-4 w-4" />
              </Link>
              <Link
                href="/security"
                aria-label="Security"
                className="inline-flex h-9 w-9 items-center justify-center rounded-md border border-white/10 bg-white/5 text-ink-200 hover:bg-white/10 hover:text-white"
              >
                <ShieldCheck className="h-4 w-4" />
              </Link>
              <Link
                href="/changelog"
                aria-label="Changelog"
                className="inline-flex h-9 w-9 items-center justify-center rounded-md border border-white/10 bg-white/5 text-ink-200 hover:bg-white/10 hover:text-white"
              >
                <GitBranch className="h-4 w-4" />
              </Link>
            </div>
          </div>
          {cols.map((c) => (
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
          <div>© {new Date().getFullYear()} OpsKeeper Authors. Released under the {SITE.license} License.</div>
          <div className="flex items-center gap-4">
            <span>Build {SITE.version}</span>
            <span aria-hidden>·</span>
            <Link href="/trademark" className="hover:text-white">Trademark</Link>
            <span aria-hidden>·</span>
            <Link href="/brand" className="hover:text-white">Brand</Link>
          </div>
        </div>
      </div>
    </footer>
  );
}
