import { StrictMode } from 'react';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { http, HttpResponse, delay } from 'msw';
import { DeliverableCard, DeliverableSequence, extractHtmlTitle, matchDeliverable } from './DeliverableCard';
import { server } from '@/test/msw-server';
import { stubIntersectionObserver, triggerVisibleAt, unstubIntersectionObserver } from '@/test/mockIO';

// getLocale() 在 localStorage 为空时走自动探测,这里固定 zh-CN(既有约定)。
localStorage.setItem('opskeeper-locale', 'zh-CN');

const HEX24 = 'a3f9c2d81b7e4056c9d0e1f2';
const UUID = '7b2c1a9e-3f4d-4c5b-8e9f-0a1b2c3d4e5f';
const PAGE_HTML = '<!doctype html><html><head><title>巡检日报页</title></head><body><h1>ok</h1></body></html>';
const PAGE_HTML_NO_TITLE = '<!doctype html><html><head></head><body></body></html>';

const pageInfo = { type: 'page' as const, id: HEX24, href: `/pages/${HEX24}` };
const reportInfo = { type: 'report' as const, id: UUID, href: `/reports/${UUID}` };

// 最小 ReportDetail 就绪响应。
const REPORT_READY = {
  id: UUID, title: '10月8日日报', kind: 'daily', status: 'ready', summary: '',
  period_start: '', period_end: '', generated_at: '2026-10-08T09:00:00Z',
  created_at: '2026-10-08T09:00:00Z', content_md: '', timezone: 'Asia/Shanghai',
};
const REPORT_GENERATING = { ...REPORT_READY, status: 'generating', generated_at: undefined, title: '' };

let pageHits = 0;
beforeEach(() => {
  pageHits = 0;
  server.use(
    http.get('/api/pages/:id', () => { pageHits++; return HttpResponse.text(PAGE_HTML); }),
    http.get('/api/v1/reports/:id', () => HttpResponse.json(REPORT_READY)),
  );
});
afterEach(() => {
  unstubIntersectionObserver();
  server.resetHandlers();
});

describe('matchDeliverable', () => {
  it('recognizes hosted pages and reports by real artifact id shapes', () => {
    expect(matchDeliverable(`/pages/${HEX24}`)).toEqual({ type: 'page', id: HEX24, href: `/pages/${HEX24}` });
    expect(matchDeliverable(`/reports/${UUID}`)).toEqual({ type: 'report', id: UUID, href: `/reports/${UUID}` });
  });
  it('rejects legacy numeric ids, /view subroutes, non-hex segments and foreign paths', () => {
    expect(matchDeliverable('/pages/12')).toBeNull();
    expect(matchDeliverable('/reports/3')).toBeNull();
    expect(matchDeliverable(`/pages/${HEX24}/view`)).toBeNull();
    expect(matchDeliverable('/pages/settings')).toBeNull();
    expect(matchDeliverable('/settings')).toBeNull();
    expect(matchDeliverable('https://example.com')).toBeNull();
    // FIX 2:报表 id 必须是小写 UUID(8-4-4-4-12)。收紧前 `[0-9a-f-]{16,64}` 会把全连字符
    // 的串也当成卡(如 16 个 '-'),那绝不可能是真实 id(uuid.NewString 不会产出)。
    expect(matchDeliverable('/reports/----------------')).toBeNull();
    expect(matchDeliverable('/reports/7b2c1a9e-3f4d-4c5b-8e9f')).toBeNull(); // 段数不对
  });
  // 回归锁定(spec 7.5「断言第三方链接不渲染 iframe」的白名单另一半):白名单的判据
  // 是 href 以 `/pages/<hex>` / `/reports/<id>` **开头**的站内相对路径,不是「路径里
  // 有这么一段」。第三方域 `https://evil.example.com/pages/<hex24>` 的 path 形状与
  // 托管页完全一样;若 PAGE_RE / REPORT_RE 丢掉 `^`(或任何让 exec 能在中段命中的
  // 改动),它就会被当成交付物,消息里出现一个指向外部站点的卡片 + sandbox iframe。
  // 这里两条都必须返回 null,否则上面的「recognizes」用例仍然绿 —— 只有本条会红。
  it('rejects third-party absolute URLs whose path matches the whitelist shape', () => {
    expect(matchDeliverable(`https://evil.example.com/pages/${HEX24}`)).toBeNull();
    expect(matchDeliverable(`https://evil.example.com/reports/${UUID}`)).toBeNull();
  });
});

describe('extractHtmlTitle', () => {
  it('parses the page <title>', () => {
    expect(extractHtmlTitle('<!doctype html><title>巡检日报页</title><body></body>')).toBe('巡检日报页');
  });
  it('trims whitespace around the title', () => {
    expect(extractHtmlTitle('<title>  spaced  </title>')).toBe('spaced');
  });
  it('returns empty string when there is no title (metadata 留空)', () => {
    expect(extractHtmlTitle('<!doctype html><body></body>')).toBe('');
  });
});

describe('DeliverableCard thumbnails', () => {
  it('opens the deliverable in a new tab (exit preserved on every state)', () => {
    const open = vi.spyOn(window, 'open').mockImplementation(() => null);
    render(<DeliverableCard info={pageInfo} />);
    screen.getByRole('button', { name: '新窗口打开' }).click();
    expect(open).toHaveBeenCalledWith(`/pages/${HEX24}`, '_blank');
    open.mockRestore();
  });

  it('does not fetch before entering the viewport', () => {
    render(<DeliverableCard info={pageInfo} />);
    expect(pageHits).toBe(0);
    expect(document.querySelector('iframe')).toBeNull();
  });

  it('renders the real sandboxed thumbnail after visibility', async () => {
    stubIntersectionObserver();
    render(<DeliverableCard info={pageInfo} />);
    triggerVisibleAt(0);
    await waitFor(() => expect(document.querySelector('iframe')).not.toBeNull());
    const iframe = document.querySelector('iframe')!;
    expect(iframe.getAttribute('sandbox')).toBe('');
    expect(iframe.getAttribute('srcdoc')).toContain('ok');
    expect(screen.getByText('巡检日报页')).toBeInTheDocument(); // <title> 诚实标题
    expect(pageHits).toBe(1);
  });

  it('shows a placeholder-sized loading pane while fetching', async () => {
    server.use(http.get('/api/pages/:id', async () => { await delay(50); return HttpResponse.text(PAGE_HTML); }));
    stubIntersectionObserver();
    render(<DeliverableCard info={pageInfo} />);
    triggerVisibleAt(0);
    const pane = screen.getByTestId('deliverable-thumb');
    expect(pane).toBeInTheDocument();
    expect(pane.className).toContain('h-40'); // idle/loading 期保持占位尺寸
    await waitFor(() => expect(document.querySelector('iframe')).not.toBeNull());
  });

  it('renders a typed report placeholder with kind + generated time, never an img/iframe', async () => {
    stubIntersectionObserver();
    render(<DeliverableCard info={reportInfo} />);
    triggerVisibleAt(0);
    await waitFor(() => expect(screen.getByText('日报')).toBeInTheDocument());
    expect(screen.getByText('2026-10-08T09:00:00Z')).toBeInTheDocument();
    expect(document.querySelector('iframe')).toBeNull();
    expect(document.querySelector('img')).toBeNull();
  });

  it('shows the generating placeholder for pending/generating reports (not failed)', async () => {
    server.use(http.get('/api/v1/reports/:id', () => HttpResponse.json(REPORT_GENERATING)));
    stubIntersectionObserver();
    render(<DeliverableCard info={reportInfo} />);
    triggerVisibleAt(0);
    await waitFor(() => expect(screen.getByText('报告生成中…')).toBeInTheDocument());
    expect(screen.queryByText('加载失败')).not.toBeInTheDocument();
  });

  it('degrades to a typed placeholder on page fetch failure and keeps the exit', async () => {
    server.use(http.get('/api/pages/:id', () => HttpResponse.text('', { status: 404 })));
    const open = vi.spyOn(window, 'open').mockImplementation(() => null);
    stubIntersectionObserver();
    render(<DeliverableCard info={pageInfo} />);
    triggerVisibleAt(0);
    await waitFor(() => expect(screen.getByText('加载失败')).toBeInTheDocument());
    expect(document.querySelector('iframe')).toBeNull();
    screen.getByRole('button', { name: '新窗口打开' }).click();
    expect(open).toHaveBeenCalledWith(`/pages/${HEX24}`, '_blank');
    open.mockRestore();
  });

  it('degrades to a typed placeholder on report fetch failure and keeps the exit', async () => {
    server.use(http.get('/api/v1/reports/:id', () => HttpResponse.json({}, { status: 500 })));
    stubIntersectionObserver();
    render(<DeliverableCard info={reportInfo} />);
    triggerVisibleAt(0);
    await waitFor(() => expect(screen.getByText('加载失败')).toBeInTheDocument());
    expect(screen.getByRole('button', { name: '新窗口打开' })).toBeInTheDocument();
  });

  it('leaves the title blank when the HTML has no <title> (不编造)', async () => {
    server.use(http.get('/api/pages/:id', () => HttpResponse.text(PAGE_HTML_NO_TITLE)));
    stubIntersectionObserver();
    render(<DeliverableCard info={pageInfo} />);
    triggerVisibleAt(0);
    await waitFor(() => expect(document.querySelector('iframe')).not.toBeNull());
    expect(screen.getByText('托管页')).toBeInTheDocument(); // 类型 chip 仍在
    // 断言整条头部的确切文本而不是「某个标题不存在」:只断言 queryByText('巡检日报页')
    // 会放过任何别的编造标题。头部此刻必须是 类型 chip + href + 出口按钮,chip 与
    // href 之间没有标题节点 —— 头部文本一变就失败,这就是「不编造」的约束本身。
    expect(screen.getByTestId('deliverable-card-header').textContent).toBe(
      `托管页${pageInfo.href}新窗口打开`,
    );
  });
});

describe('DeliverableSequence thumbnail cap', () => {
  const infos = [
    'a3f9c2d81b7e4056c9d0e1f2',
    'b4e0d3f92c8a5167d0f2a3b4',
    'c5f1e40a83d9b6278e103b4c5',
    'd602f51b94eac7389f214c5d6',
    'e713a62cab5fd849a0325d6e7',
  ].map((id) => ({ type: 'page' as const, id, href: `/pages/${id}` }));

  // 回归防护:StrictMode 会把组件渲染两次。若序号用「首次渲染自增计数」领取,
  // 第二次渲染时卡片的 useRef 被重建回初值,每张卡会再领一次,3 张卡的序号变成
  // [1,3,5],THUMB_CAP=3 只剩 1 个窗格 —— 实际表现是每条消息只有第一张卡有缩略。
  // 必须包 StrictMode 测,否则普通单次渲染下序号式 claim 看起来完全正常。
  it('caps at 3 panes for 5 cards under StrictMode (double render must not burn slots)', () => {
    render(
      <StrictMode>
        <DeliverableSequence>
          {infos.map((info) => (
            <DeliverableCard key={info.href} info={info} />
          ))}
        </DeliverableSequence>
      </StrictMode>,
    );
    expect(screen.getAllByTestId('deliverable-thumb')).toHaveLength(3);
  });

  it('gives a card with no provider the same index-0 thumbnail as before', () => {
    render(<DeliverableCard info={infos[0]} />);
    expect(screen.getAllByTestId('deliverable-thumb')).toHaveLength(1);
  });
});

describe('DeliverableCard in-place preview', () => {
  it('expands the page preview in place with the shared renderer, without navigating', async () => {
    const open = vi.spyOn(window, 'open').mockImplementation(() => null);
    stubIntersectionObserver();
    render(<DeliverableCard info={pageInfo} />);
    triggerVisibleAt(0);
    await waitFor(() => expect(document.querySelector('iframe')).not.toBeNull()); // 缩略 ready
    fireEvent.click(screen.getByTestId('deliverable-card-header'));
    const preview = screen.getByTestId('deliverable-preview');
    expect(preview).toBeInTheDocument();
    const iframe = preview.querySelector('iframe')!;
    expect(iframe).not.toBeNull();
    expect(iframe.getAttribute('sandbox')).toBe(''); // 预览 iframe 同样收紧
    expect(open).not.toHaveBeenCalled(); // 就地展开,不导航
    open.mockRestore();
  });

  it('collapses the preview on second click', async () => {
    stubIntersectionObserver();
    render(<DeliverableCard info={pageInfo} />);
    triggerVisibleAt(0);
    await waitFor(() => expect(document.querySelector('iframe')).not.toBeNull());
    fireEvent.click(screen.getByTestId('deliverable-card-header'));
    expect(screen.getByTestId('deliverable-preview')).toBeInTheDocument();
    fireEvent.click(screen.getByTestId('deliverable-card-header'));
    expect(screen.queryByTestId('deliverable-preview')).not.toBeInTheDocument();
  });

  it('expands the report preview with the shared bounded renderer (480px)', async () => {
    server.use(http.get('/api/v1/reports/:id', () => HttpResponse.json({
      ...REPORT_READY,
      content: {
        version: '1', hero: [], narrative: { headline: '集群平稳' },
        resource: { available: false, cpu_avg: 0, cpu_peak: 0, mem_avg: 0, mem_peak: 0, disk_avg: 0, disk_peak: 0 },
        fleet: { total: 3, online: 2 },
        actions_summary: { mutating_total: 0, mutating_approved: 0, safe_total: 1 },
        assets: { new_agents: 0, new_skills: 0, new_repos: 0 },
        usage: { sessions: 1, prompt_tokens: 10, completion_tokens: 5 },
      },
    })));
    stubIntersectionObserver();
    render(<DeliverableCard info={reportInfo} />);
    triggerVisibleAt(0);
    await waitFor(() => expect(screen.getByText('日报')).toBeInTheDocument());
    fireEvent.click(screen.getByTestId('deliverable-card-header'));
    await waitFor(() => expect(screen.getByTestId('deliverable-preview')).toBeInTheDocument());
    const preview = screen.getByTestId('deliverable-preview');
    expect(screen.getByText('集群平稳')).toBeInTheDocument(); // ReportContentView 内容
    const scroller = preview.querySelector('div[style]') as HTMLElement | null;
    expect(scroller?.style.maxHeight).toBe('480px'); // 不撑破消息流
  });

  it('shows the generating placeholder in the preview while the report has no content', async () => {
    server.use(http.get('/api/v1/reports/:id', () => HttpResponse.json(REPORT_GENERATING)));
    stubIntersectionObserver();
    render(<DeliverableCard info={reportInfo} />);
    triggerVisibleAt(0);
    await waitFor(() => expect(screen.getByText('报告生成中…')).toBeInTheDocument());
    fireEvent.click(screen.getByTestId('deliverable-card-header'));
    const preview = screen.getByTestId('deliverable-preview');
    expect(preview.textContent).toContain('报告生成中…');
    expect(preview.querySelector('iframe')).toBeNull();
  });

  it('failed preview keeps the 新窗口打开 exit (no dead end)', async () => {
    server.use(http.get('/api/pages/:id', () => HttpResponse.text('', { status: 404 })));
    const open = vi.spyOn(window, 'open').mockImplementation(() => null);
    stubIntersectionObserver();
    render(<DeliverableCard info={pageInfo} />);
    triggerVisibleAt(0);
    await waitFor(() => expect(screen.getByText('加载失败')).toBeInTheDocument());
    fireEvent.click(screen.getByTestId('deliverable-card-header'));
    expect(screen.getByTestId('deliverable-preview').textContent).toContain('加载失败');
    // 必须用 fireEvent:原生 .click() 的 React 状态更新不会在 act() 之外 flush,
    // 下面那条 preview 断言会对着旧树求值,删掉 stopPropagation 也照样通过。
    fireEvent.click(screen.getByRole('button', { name: '新窗口打开' }));
    expect(open).toHaveBeenCalledWith(`/pages/${HEX24}`, '_blank');
    // 锁死 stopPropagation:删掉它,出口按钮的 click 会冒泡到卡头并收起就地预览,
    // 而上面的 open 断言照样成立 —— 没有这条断言,spec 里「出口不打扰预览」是裸的。
    expect(screen.getByTestId('deliverable-preview')).toBeInTheDocument();
    open.mockRestore();
  });

  it('keeps the exit usable by keyboard: Enter opens a new tab and does not collapse the preview', async () => {
    const open = vi.spyOn(window, 'open').mockImplementation(() => null);
    stubIntersectionObserver();
    render(<DeliverableCard info={pageInfo} />);
    triggerVisibleAt(0);
    await waitFor(() => expect(document.querySelector('iframe')).not.toBeNull());
    fireEvent.click(screen.getByTestId('deliverable-card-header'));
    expect(screen.getByTestId('deliverable-preview')).toBeInTheDocument();

    // jsdom 不会把 keyDown 合成为 click,这里显式补上浏览器的真实序列:按钮上
    // Enter keydown(其默认行为就是激活)→ click。回归锁定点:keydown 从按钮冒泡
    // 到卡头,若卡头仍无条件 preventDefault + 切换,下面的 preview 断言会挂 ——
    // 键盘用户唯一的出口会静默失效。
    const exit = screen.getByRole('button', { name: '新窗口打开' });
    fireEvent.keyDown(exit, { key: 'Enter' });
    fireEvent.click(exit);

    expect(open).toHaveBeenCalledWith(`/pages/${HEX24}`, '_blank');
    expect(screen.getByTestId('deliverable-preview')).toBeInTheDocument();
    open.mockRestore();
  });

  // FIX 1 回归:卡片只露出一角时 IO 以 isIntersecting=false 回执,visible 恒 false。用户点开
  // 卡头是一次显式展开,必须无条件取数;修前 `!visible` 短路 → state 停在 idle → 预览永久 spinner。
  it('fetches and previews on expand even when the card never reported intersecting (visible stays false)', async () => {
    stubIntersectionObserver();
    render(<DeliverableCard info={pageInfo} />);
    triggerVisibleAt(0, false); // 交集比 < 0.1:负回执,some() 不翻转
    expect(pageHits).toBe(0); // 缩略仍懒挂载:未可见、未展开时不取数
    expect(screen.queryByTestId('deliverable-preview')).not.toBeInTheDocument();

    fireEvent.click(screen.getByTestId('deliverable-card-header'));

    await waitFor(() => expect(pageHits).toBe(1)); // 显式展开无条件取数
    const preview = screen.getByTestId('deliverable-preview');
    await waitFor(() => expect(preview.querySelector('iframe')).not.toBeNull()); // 落地内容,不是 spinner
  });

  // FIX 3 回归:报表 status:'ready' 但 content 为空 —— 取数成功但无内容。修前落到 failed 占位,
  // 把一次成功的取数谎报成「加载失败」;必须给诚实的空态,且不得移除「新窗口打开」出口。
  it('shows an honest empty state when a ready report has no content (not "加载失败")', async () => {
    stubIntersectionObserver();
    render(<DeliverableCard info={reportInfo} />);
    triggerVisibleAt(0);
    await waitFor(() => expect(screen.getByText('日报')).toBeInTheDocument()); // 缩略 ready
    fireEvent.click(screen.getByTestId('deliverable-card-header'));
    const preview = screen.getByTestId('deliverable-preview');
    expect(preview.textContent).toContain('报告暂无内容');
    expect(preview.textContent).not.toContain('加载失败');
    expect(screen.getByRole('button', { name: '新窗口打开' })).toBeInTheDocument(); // 出口仍在
  });
});

// phase4-ui-deepening:D1 一致性可见契约 —— 类型徽标走 Chip 原语、卡面圆角走 rk
// 阶梯 token。徽标由手写 pill 改为 <Chip> 后,形态与全站 chip 一致(rounded-full);
// 缩略/预览的边框从裸 zinc-800 收敛到 border-border token。
describe('DeliverableCard chrome consistency', () => {
  it('renders the type badge through the Chip primitive (rounded-full, not a hand-rolled pill)', () => {
    render(<DeliverableCard info={pageInfo} />);
    const badge = screen.getByText('托管页');
    expect(badge.className).toContain('rounded-full'); // Chip 原语形态
    expect(badge.className).not.toContain('rounded-rk-'); // 不再是手写 pill
  });

  it('uses rk radius tokens and token borders on the card surfaces', () => {
    render(<DeliverableCard info={pageInfo} />);
    expect(screen.getByTestId('deliverable-card').className).toContain('rounded-rk-md');
    const thumb = screen.getByTestId('deliverable-thumb');
    expect(thumb.className).toContain('rounded-rk-sm');
    expect(thumb.className).toContain('border-border'); // token 边框
    expect(thumb.className).not.toContain('border-zinc-800'); // 不再是裸 zinc
  });
});
