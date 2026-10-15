// Pages 页面测试 — Task 3 抽出共享 HostedPageView 后，Pages.tsx 出现两个新消费点：
//   1) 列表卡片的 PageThumb：取数留在本页，渲染交给共享组件；
//   2) 预览弹窗：加载中走 spinner 分支，就绪后走全尺寸 HostedPageView。
// 之前这两个消费点没有组件级覆盖（HostedPageView 有自己的单测，但"页面是否正确
// 消费它"无人保证），这里补上——尤其是弹窗 iframe 的动态 title，它在抽出共享
// 组件时曾被写死成固定的 "page preview"，是一处真实的无障碍回归。
import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter } from 'react-router-dom';
import { http, HttpResponse } from 'msw';
import { beforeEach, describe, expect, it, vi } from 'vitest';

import PagesPage from './Pages';
import { ConfirmProvider } from '@/components/ui';
import { server } from '@/test/msw-server';

vi.mock('@/store/auth', () => ({
  useAuth: Object.assign(
    <T,>(selector: (s: { role: string }) => T): T => selector({ role: 'admin' }),
    { getState: () => ({ logout: () => {} }) },
  ),
  getToken: () => null,
  getRefreshToken: () => null,
}));

const PAGE_HTML = '<!doctype html><html><body><h1>季度营收报告</h1></body></html>';

// 列表接口走 request() → /api/v1/pages；HTML 走 fetchPageHTML() 的裸 fetch
// → /api/pages/<id>（注意没有 /v1 前缀，这是两条不同的路由）。
const listURL = '/api/v1/pages';

function page(id = 'p1', title = '季度营收报告') {
  return { id, title, created_at: '2026-10-01T00:00:00Z', url: `/api/pages/${id}` };
}

// 手写闸门代替 delay('infinity')：让 HTML 请求真正挂起，才能确定性地断言
// "pending 时没有 iframe" 这条分支，而不是靠竞态碰运气。
function makeGate() {
  let release: () => void = () => {};
  const promise = new Promise<void>((res) => {
    release = res;
  });
  return { promise, release: () => release() };
}

function renderPages() {
  return render(
    <MemoryRouter>
      <ConfirmProvider>
        <PagesPage />
      </ConfirmProvider>
    </MemoryRouter>,
  );
}

describe('PagesPage — PageThumb 消费共享 HostedPageView', () => {
  let gate: ReturnType<typeof makeGate>;

  beforeEach(() => {
    localStorage.setItem('opskeeper-locale', 'zh-CN');
    gate = makeGate();
    server.use(
      http.get(listURL, () => HttpResponse.json({ items: [page()], total: 1 })),
      http.get('/api/pages/:id', async () => {
        await gate.promise;
        return HttpResponse.html(PAGE_HTML);
      }),
    );
  });

  it('HTML 拉取未完成时只有占位容器，不渲染 iframe', async () => {
    renderPages();
    // 列表已就绪 → PageThumb 已挂载 → 但 HTML 请求仍在闸门里挂着
    await screen.findByText('季度营收报告');
    expect(document.querySelector('iframe')).toBeNull();
  });

  it('HTML 就绪后渲染共享组件的沙箱 iframe，srcdoc 是取回的页面内容', async () => {
    renderPages();
    await screen.findByText('季度营收报告');
    gate.release();

    const iframe = await waitFor(() => {
      const el = document.querySelector('iframe');
      expect(el).not.toBeNull();
      return el!;
    });
    // 消费的是 HostedPageView 的缩略模式：sandbox 收紧 + 固定无障碍名 + 等比缩放
    expect(iframe.getAttribute('sandbox')).toBe('');
    expect(iframe.getAttribute('title')).toBe('thumbnail');
    expect(iframe.getAttribute('srcdoc')).toContain('季度营收报告');
    expect(iframe.style.transform).toContain('scale(');
  });
});

describe('PagesPage — 预览弹窗消费全尺寸 HostedPageView', () => {
  let gate: ReturnType<typeof makeGate>;

  beforeEach(() => {
    localStorage.setItem('opskeeper-locale', 'zh-CN');
    gate = makeGate();
    server.use(
      http.get(listURL, () => HttpResponse.json({ items: [page()], total: 1 })),
      http.get('/api/pages/:id', async () => {
        await gate.promise;
        return HttpResponse.html(PAGE_HTML);
      }),
    );
  });

  it('previewHtml 为 null 时走 spinner 分支，就绪后渲染全尺寸 iframe 并带上动态 title', async () => {
    renderPages();
    await userEvent.click(await screen.findByTitle('预览'));

    // 加载中：spinner 文案在，弹窗内没有 iframe
    const dialog = await screen.findByRole('dialog');
    expect(within(dialog).getByText(/加载中/)).toBeInTheDocument();
    expect(dialog.querySelector('iframe')).toBeNull();

    gate.release();

    const iframe = await waitFor(() => {
      const el = dialog.querySelector('iframe');
      expect(el).not.toBeNull();
      return el!;
    });
    // 回归防护：弹窗 iframe 的无障碍名称是页面标题，不是写死的 "page preview"。
    // 只在弹窗内查 iframe —— 背后的列表卡片还挂着一张缩略 iframe。
    expect(iframe.getAttribute('title')).toBe('季度营收报告');
    expect(iframe.getAttribute('sandbox')).toBe('');
    expect(iframe.getAttribute('srcdoc')).toContain('季度营收报告');
    expect(iframe.style.height).toBe('60vh');
    // 全尺寸模式不缩放
    expect(iframe.style.transform).toBe('');
    expect(within(dialog).queryByText(/加载中/)).not.toBeInTheDocument();
  });

  it('无标题的页面回落到 title="page"', async () => {
    server.use(
      http.get(listURL, () => HttpResponse.json({ items: [page('p2', '')], total: 1 })),
      http.get('/api/pages/:id', async () => {
        await gate.promise;
        return HttpResponse.html(PAGE_HTML);
      }),
    );
    renderPages();
    await userEvent.click(await screen.findByTitle('预览'));

    const dialog = await screen.findByRole('dialog');
    gate.release();

    const iframe = await waitFor(() => {
      const el = dialog.querySelector('iframe');
      expect(el).not.toBeNull();
      return el!;
    });
    expect(iframe.getAttribute('title')).toBe('page');
  });
});