'use client';

import Link from 'next/link';
import { usePathname } from 'next/navigation';
import { cn } from '@/lib/utils';

type Item = { title: string; href: string };
type Section = { title: string; items: Item[] };

const sections: Section[] = [
  {
    title: '快速上手',
    items: [
      { title: '简介', href: '/zh/docs' },
      { title: '快速开始', href: '/zh/docs/getting-started' },
      { title: '架构', href: '/zh/docs/architecture' },
    ],
  },
  {
    title: '运维',
    items: [
      { title: '部署', href: '/zh/docs/deployment' },
      { title: '运维手册', href: '/zh/docs/operations' },
      { title: '安全模型', href: '/zh/docs/security-model' },
    ],
  },
  {
    title: '扩展',
    items: [
      { title: '插件', href: '/zh/docs/plugins' },
      { title: '集成', href: '/zh/docs/integrations' },
      { title: 'API 参考', href: '/zh/docs/api' },
    ],
  },
  {
    title: '团队协作',
    items: [
      { title: '工作流目录', href: '/zh/docs/workflow-catalog' },
      { title: 'Harness 指南', href: '/zh/docs/harness-guide' },
      { title: '迁移', href: '/zh/docs/migration' },
    ],
  },
];

export function DocsSidebarZh() {
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
