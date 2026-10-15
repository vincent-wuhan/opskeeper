// Discover 统一壳测试 —— 一个壳层聚合技能 / 插件市场 / 自愈结晶三张既有页面,
// tab 状态完全落在 `?tab=` 查询参数里,所以每个入口(sidebar、重定向、书签)
// 都只是一条普通链接。
//
// 三张内嵌页面各自都会在挂载时发请求;共享 msw server(`src/test/msw-server.ts`)
// 是空的,`setup.ts` 又是 onUnhandledRequest:'error',所以缺一个 handler 会当众
// 炸,而不是静默挂起——这里按每个用例实际挂载的内嵌页把 handler 配齐。
import type { ReactElement } from 'react';
import { fireEvent, render, screen, within, waitFor } from '@testing-library/react';
import { MemoryRouter, Route, Routes, useLocation } from 'react-router-dom';
import { http, HttpResponse } from 'msw';
import { beforeEach, describe, expect, it, vi } from 'vitest';

import { CrystalsRedirect, Discover, PluginsRedirect, SkillsRedirect } from './Discover';
import { server } from '@/test/msw-server';

// SkillsPage 读 `useAuth((s) => s.role)`(Skills.tsx:31);
// PluginMarketplace / Crystallized 读 `usePermissions()`。
// 两个 store 都 mock 掉,避免依赖真实 store 的持久化状态。
vi.mock('@/store/auth', () => ({
  useAuth: Object.assign(
    <T,>(selector: (s: { role: string }) => T): T => selector({ role: 'admin' }),
    { getState: () => ({ logout: () => {} }) },
  ),
  getToken: () => null,
  getRefreshToken: () => null,
}));
vi.mock('@/store/me', () => ({
  usePermissions: () => ({ isAdmin: false, canMutate: false, role: 'user' }),
}));

function renderAt(entry: string) {
  return render(
    <MemoryRouter initialEntries={[entry]}>
      <Routes>
        <Route path="/discover" element={<Discover />} />
      </Routes>
    </MemoryRouter>,
  );
}

// The redirect cases mount a legacy path plus the /discover shell, then read
// where the router ended up. MemoryRouter keeps history in memory and never
// touches window.location, so a probe reading window.location would stay
// pinned at jsdom's default and the assertion could never pass — useLocation()
// reads the router's real state. Precedent: src/pages/Agents.test.tsx:55
// (LocationProbe).
function renderRedirect(entry: string, legacyPath: string, node: ReactElement) {
  const at = { value: '' };
  function Probe() {
    const loc = useLocation();
    at.value = loc.pathname + loc.search;
    return null;
  }
  render(
    <MemoryRouter initialEntries={[entry]}>
      <Routes>
        <Route path={legacyPath} element={node} />
        <Route
          path="/discover"
          element={
            <>
              <Discover />
              <Probe />
            </>
          }
        />
      </Routes>
    </MemoryRouter>,
  );
  return at;
}

describe('Discover', () => {
  beforeEach(() => {
    // 断言的 tab 名正则中英都能命中,但仍按仓库既有约定 pin 住 locale,
    // 免得别的用例改了全局 locale 后这里出现假阴性。
    localStorage.setItem('opskeeper-locale', 'zh-CN');
    server.use(
      // SkillsPage 挂载时拉技能 + MCP flow-tools。
      http.get('/api/v1/skills', () => HttpResponse.json({ items: [], total: 0 })),
      http.get('/api/v1/flow-tools', () => HttpResponse.json({ items: [] })),
      // 未知 tab 的回落到 skills 后,admin 会命中 SkillsPage 的 install 子 tab
      // (深链语义),那里的 InstallChatBar 会拉模型目录。
      http.get('/api/v1/aiops/models', () => HttpResponse.json({ providers: [], default: null })),
      // PluginMarketplacePage 挂载时拉节点清单与发布列表。
      http.get('/api/v1/edges', () => HttpResponse.json({ items: [], total: 0 })),
      http.get('/api/v1/plugins/releases', () => HttpResponse.json({ items: [] })),
      // CrystallizedPage 挂载时拉账本。
      http.get('/api/v1/loops/crystallized', () =>
        HttpResponse.json({ items: [], policy: null, observing_since: null }),
      ),
    );
  });

  // Each case asserts BOTH halves of the shell: the tab that reads as selected
  // AND the panel it swaps in. `aria-selected={tab === t.id}` and
  // `{tab === 'x' && <XPage/>}` are two independent derivations from the same
  // `tab` value, so a case that only checked `aria-selected` would stay green
  // even if the panel branch regressed (always render <SkillsPage/>, or drop
  // the panel lines entirely — every page's handlers are registered, so nothing
  // fails to mount). The panel's `<h1>` comes from the embedded page's own
  // PageHeader (PageHeader.tsx:32); the shell has none, so it is unambiguous
  // within the panel — scoping through `role="tabpanel"` keeps the skills case
  // from colliding with the 技能 tab button's text.
  it('defaults to the skills tab', () => {
    renderAt('/discover');
    expect(screen.getByRole('tab', { name: /技能|Skills/ }).getAttribute('aria-selected')).toBe('true');
    expect(
      within(screen.getByRole('tabpanel')).getByRole('heading', { level: 1 }),
    ).toHaveTextContent('技能');
  });

  it('honours ?tab=plugins', () => {
    renderAt('/discover?tab=plugins');
    expect(screen.getByRole('tab', { name: /插件|Plugins/ }).getAttribute('aria-selected')).toBe('true');
    expect(
      within(screen.getByRole('tabpanel')).getByRole('heading', { level: 1 }),
    ).toHaveTextContent('插件市场');
  });

  it('honours ?tab=crystals', () => {
    renderAt('/discover?tab=crystals');
    expect(screen.getByRole('tab', { name: /自愈结晶|Crystals/ }).getAttribute('aria-selected')).toBe('true');
    // 内嵌页标题是「自愈规则 / Crystallised Runbooks」,故意与 Discover 的
    // tab 标签「自愈结晶」不同——这样 h1 断言不可能撞上 tab 文字。
    expect(
      within(screen.getByRole('tabpanel')).getByRole('heading', { level: 1 }),
    ).toHaveTextContent('自愈规则');
  });

  it('falls back to skills for ?tab=install and lands on the admin install sub-surface', () => {
    renderAt('/discover?tab=install');
    // 未知值(tab=install)回落到 skills:tab 亮起、技能面板挂载。
    expect(screen.getByRole('tab', { name: /技能|Skills/ }).getAttribute('aria-selected')).toBe('true');
    expect(
      within(screen.getByRole('tabpanel')).getByRole('heading', { level: 1 }),
    ).toHaveTextContent('技能');
    // 值留在 URL,且 auth mock 的 role 是 admin(Skills.tsx:32 需要 admin),
    // 所以内嵌的 SkillsPage 真的切到了 install 子界面 —— 那条安装 composer
    // 的 placeholder 只在 install 子 tab 渲染,是稳定的证据。
    expect(screen.getByPlaceholderText(/贴个技能源/)).toBeInTheDocument();
  });

  // phase4-ui-deepening:标签条键盘可达(左右方向键)+ 标签/面板关联。
  it('associates each tab with its panel and moves with arrow keys', async () => {
    renderAt('/discover');
    const skills = screen.getByRole('tab', { name: /技能|Skills/ });
    expect(skills).toHaveAttribute('aria-controls', 'discover-panel-skills');
    expect(screen.getByRole('tabpanel')).toHaveAttribute('id', 'discover-panel-skills');
    // roving tabindex:仅活动标签可 Tab 聚焦
    expect(skills).toHaveAttribute('tabindex', '0');
    expect(screen.getByRole('tab', { name: /插件|Plugins/ })).toHaveAttribute('tabindex', '-1');

    skills.focus();
    fireEvent.keyDown(skills, { key: 'ArrowRight' });
    await waitFor(() =>
      expect(screen.getByRole('tab', { name: /插件|Plugins/ }).getAttribute('aria-selected')).toBe('true'),
    );
    expect(screen.getByRole('tabpanel')).toHaveAttribute('aria-labelledby', 'discover-tab-plugins');
  });

  // Legacy-route redirects. `/skills?tab=install&q=web` must keep BOTH its
  // meaningful `tab=install` (the only way to reach SkillsPage's install
  // sub-surface) and its unrelated `q=web` — the redirect only supplies a
  // default tab when one is absent, it never overwrites an existing value.
  it('redirects /skills to /discover preserving the original query', async () => {
    const at = renderRedirect('/skills?tab=install&q=web', '/skills', <SkillsRedirect />);
    await waitFor(() => expect(at.value).toContain('/discover'));
    expect(at.value).toContain('tab=install');
    expect(at.value).toContain('q=web');
  });

  it('redirects /plugins to /discover?tab=plugins', async () => {
    const at = renderRedirect('/plugins', '/plugins', <PluginsRedirect />);
    await waitFor(() => expect(at.value).toContain('/discover'));
    expect(at.value).toContain('tab=plugins');
  });

  it('redirects /crystallized to /discover?tab=crystals', async () => {
    const at = renderRedirect('/crystallized', '/crystallized', <CrystalsRedirect />);
    await waitFor(() => expect(at.value).toContain('/discover'));
    expect(at.value).toContain('tab=crystals');
  });
});
