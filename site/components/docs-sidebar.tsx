'use client';

import Link from 'next/link';
import { usePathname } from 'next/navigation';
import { cn } from '@/lib/utils';

type Item = { title: string; href: string };
type Section = { title: string; items: Item[] };

const sections: Section[] = [
  {
    title: 'Get started',
    items: [
      { title: 'Introduction', href: '/docs' },
      { title: 'Getting started', href: '/docs/getting-started' },
      { title: 'Architecture', href: '/docs/architecture' },
    ],
  },
  {
    title: 'Operate',
    items: [
      { title: 'Deployment', href: '/docs/deployment' },
      { title: 'Operations manual', href: '/docs/operations' },
      { title: 'Security model', href: '/docs/security-model' },
    ],
  },
  {
    title: 'Build',
    items: [
      { title: 'Plugins', href: '/docs/plugins' },
      { title: 'Integrations', href: '/docs/integrations' },
      { title: 'API reference', href: '/docs/api' },
    ],
  },
  {
    title: 'Run as a team',
    items: [
      { title: 'Workflow catalog', href: '/docs/workflow-catalog' },
      { title: 'Harness guide', href: '/docs/harness-guide' },
      { title: 'Migration', href: '/docs/migration' },
    ],
  },
];

export function DocsSidebar() {
  const pathname = usePathname();
  return (
    <nav className="rounded-2xl border border-white/10 bg-white/[0.02] p-4">
      {sections.map((s) => (
        <div key={s.title} className="mb-4 last:mb-0">
          <div className="px-2 pb-2 text-xs font-semibold uppercase tracking-wider text-ink-400">
            {s.title}
          </div>
          <ul className="space-y-0.5">
            {s.items.map((it) => {
              const active = pathname === it.href;
              return (
                <li key={it.href}>
                  <Link
                    href={it.href}
                    className={cn(
                      'block rounded-md px-2 py-1.5 text-sm transition-colors',
                      active
                        ? 'bg-accent-500/10 text-white'
                        : 'text-ink-200 hover:bg-white/5 hover:text-white',
                    )}
                  >
                    {it.title}
                  </Link>
                </li>
              );
            })}
          </ul>
        </div>
      ))}
    </nav>
  );
}
