// Discover — one shell over the three extension surfaces (D6).
// Tab state lives entirely in `?tab=` so every entry point (sidebar,
// redirects, bookmarks) is a plain link.
//
// Unknown values fall back to `skills` but are LEFT IN the URL: the legacy
// `/skills?tab=install` deep link therefore lands on the skills panel, where
// the embedded SkillsPage reads the same `?tab=` param (Skills.tsx:32) and
// shows its install subtab — but only for an admin; a non-admin sees the
// catalog regardless of the param.
//
// No PageHeader and no <main> at this level: each embedded page renders its
// own (Skills.tsx:47 / PluginMarketplace.tsx:50 / Crystallized.tsx). A header
// here would stack two title bars, and a <main> here would nest a <main>
// inside a <main>. The root is therefore a plain <div>, leaving the embedded
// page's <main> as the page's only one.
import { Navigate, useSearchParams } from 'react-router-dom';
import type { KeyboardEvent } from 'react';
import { tr } from '@/i18n/locale';
import { cn } from '@/lib/cn';
import SkillsPage from './Skills';
import PluginMarketplacePage from './PluginMarketplace';
import CrystallizedPage from './Crystallized';

export type DiscoverTab = 'skills' | 'plugins' | 'crystals';

const TABS: Array<{ id: DiscoverTab; zh: string; en: string }> = [
  { id: 'skills', zh: '技能', en: 'Skills' },
  { id: 'plugins', zh: '插件', en: 'Plugins' },
  { id: 'crystals', zh: '自愈结晶', en: 'Crystals' },
];

export function Discover() {
  const [searchParams, setSearchParams] = useSearchParams();
  const raw = searchParams.get('tab');
  const tab: DiscoverTab = raw === 'plugins' || raw === 'crystals' ? raw : 'skills';

  const select = (next: DiscoverTab) => {
    setSearchParams(
      (prev) => {
        const p = new URLSearchParams(prev);
        p.set('tab', next);
        return p;
      },
      { replace: true },
    );
  };

  // 左右方向键在标签间移动(标准 tablist 键盘契约),焦点跟随选中项。
  const onKeyDown = (e: KeyboardEvent<HTMLDivElement>) => {
    if (e.key !== 'ArrowRight' && e.key !== 'ArrowLeft') return;
    e.preventDefault();
    const i = TABS.findIndex((t) => t.id === tab);
    const delta = e.key === 'ArrowRight' ? 1 : -1;
    const next = TABS[(i + delta + TABS.length) % TABS.length].id;
    select(next);
    e.currentTarget.querySelector<HTMLButtonElement>(`#discover-tab-${next}`)?.focus();
  };

  return (
    <div className="flex flex-1 flex-col overflow-hidden">
      {/* 标签条对齐控制台页边距(PageHeader 用 px-6);self-start 保持 pill 宽度自适应 */}
      <div
        role="tablist"
        aria-label={tr('扩展面', 'Extensions')}
        onKeyDown={onKeyDown}
        className="ml-6 mt-4 flex gap-1 self-start rounded-rk-md bg-zinc-900/60 p-1"
      >
        {TABS.map((t) => (
          <button
            key={t.id}
            id={`discover-tab-${t.id}`}
            role="tab"
            type="button"
            aria-selected={tab === t.id}
            aria-controls={`discover-panel-${t.id}`}
            tabIndex={tab === t.id ? 0 : -1}
            onClick={() => select(t.id)}
            className={cn(
              'rounded-full px-3 py-1.5 text-xs transition-colors',
              tab === t.id ? 'bg-zinc-100 text-zinc-900' : 'text-zinc-400 hover:text-zinc-200',
            )}
          >
            {tr(t.zh, t.en)}
          </button>
        ))}
      </div>
      <div
        id={`discover-panel-${tab}`}
        role="tabpanel"
        aria-labelledby={`discover-tab-${tab}`}
        className="flex flex-1 flex-col overflow-hidden"
      >
        {tab === 'skills' && <SkillsPage />}
        {tab === 'plugins' && <PluginMarketplacePage />}
        {tab === 'crystals' && <CrystallizedPage />}
      </div>
    </div>
  );
}

// Legacy-route redirects: /skills, /plugins and /crystallized now fold into the
// single /discover shell. Each only supplies a DEFAULT tab when the incoming
// URL has none. A legacy deep link that already carries a meaningful `?tab=`
// must keep it: `/skills?tab=install` is the only entry to SkillsPage's install
// sub-surface (InstallTab is mounted inside Skills.tsx and is hidden from the
// visible nav), so overwriting the tab would make that surface unreachable.
// Discover's own reader falls back to the skills panel for values it does not
// recognise, so an unknown incoming tab still lands somewhere sensible.
function redirectTo(tab: DiscoverTab) {
  return function DiscoverRedirect() {
    const [sp] = useSearchParams();
    const params = new URLSearchParams(sp);
    if (!params.get('tab')) params.set('tab', tab);
    return <Navigate to={{ pathname: '/discover', search: `?${params.toString()}` }} replace />;
  };
}

export const SkillsRedirect = redirectTo('skills');
export const PluginsRedirect = redirectTo('plugins');
export const CrystalsRedirect = redirectTo('crystals');
