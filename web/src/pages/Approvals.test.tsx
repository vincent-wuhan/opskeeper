// Approvals 状态标签测试 —— 守住 console-reskin delta spec 的这一条:
// 「状态 pill 使用 rounded-full + 呼吸点」。
//
// 这条要求此前漏做:提交 6bca872 只把外层条目容器换成了 surface-card 大卡,
// 页内私有的 StatusChip 仍是老的 `rounded` 小圆角、且没有呼吸点。
// 下面是让这个漏做项不会再溜回去的护栏。
import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter } from 'react-router-dom';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import ApprovalsPage from './Approvals';
import { server } from '@/test/msw-server';

// @/api/client 会 import store/auth 的 getToken/getRefreshToken，mock 掉避免
// 依赖真实 store 的持久化状态。与 Agents.test.tsx / Discover.test.tsx 同款。
vi.mock('@/store/auth', () => ({
  useAuth: Object.assign(
    <T,>(selector: (s: { role: string }) => T): T => selector({ role: 'admin' }),
    { getState: () => ({ logout: () => {} }) },
  ),
  getToken: () => null,
  getRefreshToken: () => null,
}));

// 四态各一条，全部挂在一个固定 created_at 上 —— 页面底部会 toLocaleString()
// 这个时间戳，钉死它免得断言之外的东西随时区/语言漂移。
const FIXED_AT = '2026-01-02T03:04:05Z';

function approval(id: string, status: string, title: string) {
  return {
    id,
    kind: 'restart_service',
    title,
    summary: '',
    payload: '{}',
    source: 'agent',
    status,
    proposed_by: 1,
    created_at: FIXED_AT,
  };
}

const PENDING = approval('a-pending', 'pending', '重启数据库');
const APPROVED = approval('a-approved', 'approved', '扩容节点池');
const EXECUTED = approval('a-executed', 'executed', '清理磁盘');
const FAILED = approval('a-failed', 'failed', '关闭端口');

// 页面顶部的状态筛选 tabs 只有在切过去时才会拉到对应状态的数据。
// 这里让它对任意 status 都回全量四条，于是单次渲染就能覆盖四态 pill。
function renderApprovals() {
  return render(
    <MemoryRouter>
      <ApprovalsPage />
    </MemoryRouter>,
  );
}

// 呼吸点元素：class 里带 animate-pulse-dot 即可。用属性包含匹配而不是
// `.animate-pulse-dot` 类选择器 —— 实现上它被 motion-safe: 变体包了一层
// （class 字面量是 "motion-safe:animate-pulse-dot"，点号选择器匹配不到），
// 但「是不是脉冲动画」和「是不是被 motion-safe 门控」是两件要分别断言的事。
function pulseDotOf(pill: HTMLElement): HTMLElement | null {
  return pill.querySelector('[class*="animate-pulse-dot"]');
}

// pill 元素本身：StatusChip 的根 span。
//
// 「待确认」在页面上出现三次（PageHeader 标题、pending 筛选 tab、pending 标签），
// 直接 getByText 会报 found multiple。所以统一从**唯一**的条目标题定位到那张
// surface-card 大卡，再在卡内找状态标签 —— 顺带让断言落在条目语境里，
// 而不是全页搜索。
function pillFor(title: string, label: string): HTMLElement {
  const card = screen.getByText(title).closest('.surface-card');
  if (!card) throw new Error(`未找到条目卡片：${title}`);
  const el = within(card as HTMLElement).getByText(label).closest('span');
  if (!el) throw new Error(`未找到状态标签元素：${label}`);
  return el as HTMLElement;
}

describe('Approvals 状态标签 pill（console-reskin 规格）', () => {
  beforeEach(() => {
    // getLocale() 先读存储值再走 autoDetectLocale()；钉死后不再进时区分支，
    // 中文文案断言才稳定。
    localStorage.setItem('opskeeper-locale', 'zh-CN');

    server.use(
      http.get('/api/v1/approvals', () =>
        HttpResponse.json({ items: [PENDING, APPROVED, EXECUTED, FAILED] }),
      ),
    );
  });

  it('四态都渲染出状态标签', async () => {
    renderApprovals();

    await screen.findByText('重启数据库');
    const cases: Array<[string, string]> = [
      ['重启数据库', '待确认'],
      ['扩容节点池', '已批准'],
      ['清理磁盘', '已执行'],
      ['关闭端口', '失败'],
    ];
    for (const [title, label] of cases) {
      expect(pillFor(title, label), `${title} 缺少状态标签`).toBeInTheDocument();
    }
  });

  it('状态标签是 rounded-full 胶囊，而不是 rounded 小圆角', async () => {
    renderApprovals();

    await screen.findByText('重启数据库');
    for (const [title, label] of [
      ['重启数据库', '待确认'],
      ['扩容节点池', '已批准'],
      ['清理磁盘', '已执行'],
      ['关闭端口', '失败'],
    ]) {
      const cls = pillFor(title, label).className;
      expect(cls, `${label} 不是 rounded-full`).toContain('rounded-full');
      // rounded 与 rounded-full 同时出现时后者不一定覆盖，这里防一手。
      expect(cls, `${label} 仍残留 rounded`).not.toMatch(/(^|\s)rounded(\s|$)/);
    }
  });

  it('状态标签内有呼吸点（animate-pulse-dot 小圆点）', async () => {
    renderApprovals();

    await screen.findByText('重启数据库');
    const dot = pulseDotOf(pillFor('重启数据库', '待确认'));
    expect(dot, '待确认 缺少呼吸点').not.toBeNull();
    // 呼吸点同时要是小圆点，否则「有动画但没有点」也算跑偏。
    expect(dot?.className).toContain('rounded-full');
    // motion-safe 门控：开了「减少动态效果」的用户应该看到静止圆点。
    expect(dot?.className, '呼吸点未做 prefers-reduced-motion 门控').toContain('motion-safe:');
  });

  it('四态都带呼吸点，且圆点沿用各自状态的语义色', async () => {
    renderApprovals();

    await screen.findByText('重启数据库');
    const cases: Array<[string, string, string]> = [
      ['重启数据库', '待确认', 'bg-amber'],
      ['扩容节点池', '已批准', 'bg-emerald'],
      ['清理磁盘', '已执行', 'bg-emerald'],
      ['关闭端口', '失败', 'bg-red'],
    ];
    for (const [title, label, colorPrefix] of cases) {
      const dot = pulseDotOf(pillFor(title, label));
      expect(dot, `${label} 缺少呼吸点`).not.toBeNull();
      expect(dot?.className, `${label} 呼吸点语义色不符`).toContain(colorPrefix);
    }
  });

  it('文案仍走 statusLabel（换皮未丢失中文标签）', async () => {
    renderApprovals();

    await screen.findByText('重启数据库');
    // 四态标签的中文文案都还在，且没有被呼吸点圆点挤掉。
    expect(pillFor('重启数据库', '待确认')).toHaveTextContent('待确认');
    expect(pillFor('扩容节点池', '已批准')).toHaveTextContent('已批准');
    expect(pillFor('清理磁盘', '已执行')).toHaveTextContent('已执行');
    expect(pillFor('关闭端口', '失败')).toHaveTextContent('失败');
    // 顶部筛选 tab 仍由同一个 statusLabel 驱动。
    expect(screen.getByText('已拒绝')).toBeInTheDocument();
  });
});

describe('Approvals 双签进度（approval-governance 规格）', () => {
  beforeEach(() => {
    localStorage.setItem('opskeeper-locale', 'zh-CN');
  });

  function withSigners(id: string, title: string, signers?: string) {
    // 复用文件顶部的 approval() 形状,补上 signers 字段。
    return {
      id,
      kind: 'restart_service',
      title,
      summary: '',
      payload: '{}',
      source: 'agent',
      status: 'pending',
      signers,
      proposed_by: 1,
      created_at: FIXED_AT,
    };
  }

  function renderWith(rows: unknown[]) {
    server.use(http.get('/api/v1/approvals', () => HttpResponse.json({ items: rows })));
    return render(
      <MemoryRouter>
        <ApprovalsPage />
      </MemoryRouter>,
    );
  }

  it('部分签署: 显示 N 人已签 / 需 2 人,并列出签署人角色与时间', async () => {
    const signers = JSON.stringify([{ user_id: 1, role: 'admin', at: '2026-01-02T03:04:05Z' }]);
    renderWith([withSigners('a-partial', '重启数据库', signers)]);

    await screen.findByText('重启数据库');
    expect(screen.getByText('1 人已签 / 需 2 人')).toBeInTheDocument();
    // 签署人角色出现(admin 以 role 渲染)。
    expect(screen.getByText(/admin/)).toBeInTheDocument();
  });

  it('无签署(空数组): 显示签署要求,且不展示签署人', async () => {
    renderWith([withSigners('a-none', '扩容节点池', '[]')]);

    await screen.findByText('扩容节点池');
    expect(screen.getByText(/需 2 位批准人/)).toBeInTheDocument();
    // 空数组下没有签署人行,admin 不应出现在该卡内。
    expect(screen.queryByText(/admin/)).not.toBeInTheDocument();
  });

  it('不可解析: 中性显示「签署状态未知」,不显示人数,且按钮仍可用', async () => {
    renderWith([withSigners('a-unknown', '清理磁盘', '{bad json')]);

    await screen.findByText('清理磁盘');
    expect(screen.getByText('签署状态未知')).toBeInTheDocument();
    expect(screen.queryByText(/人已签/)).not.toBeInTheDocument();
    // 不阻塞操作:批准按钮存在且未禁用。
    expect(screen.getByRole('button', { name: '批准' })).toBeEnabled();
  });
});

describe('Approvals 部分签署诚实反馈（approval-governance 规格）', () => {
  beforeEach(() => {
    localStorage.setItem('opskeeper-locale', 'zh-CN');
  });

  afterEach(() => {
    vi.restoreAllMocks();
  });

  function pendingRow() {
    return {
      id: 'a-sign',
      kind: 'restart_service',
      title: '重启数据库',
      summary: '',
      payload: '{}',
      source: 'agent',
      status: 'pending',
      proposed_by: 1,
      created_at: FIXED_AT,
    };
  }

  it('202(pending): 显示等待第二位批准人,行不消失,批准按钮禁用,且不重载列表', async () => {
    const confirmSpy = vi.spyOn(window, 'confirm');
    let listCalls = 0;
    server.use(
      http.get('/api/v1/approvals', () => {
        listCalls += 1;
        return HttpResponse.json({ items: [pendingRow()] });
      }),
      http.post('/api/v1/approvals/a-sign/approve', () =>
        HttpResponse.json(
          {
            ...pendingRow(),
            status: 'pending',
            signers: JSON.stringify([{ user_id: 1, role: 'admin', at: '2026-01-02T03:04:05Z' }]),
          },
          { status: 202 },
        ),
      ),
    );

    render(
      <MemoryRouter>
        <ApprovalsPage />
      </MemoryRouter>,
    );
    await screen.findByText('重启数据库');
    await userEvent.click(screen.getByRole('button', { name: '批准' }));
    // 批准改走应用内确认:点击弹窗的「确认批准并执行」才真正发请求。
    await userEvent.click(await screen.findByRole('button', { name: /确认批准并执行/ }));

    expect(await screen.findByText(/你的签名已记录/)).toBeInTheDocument();
    // 该短语同时出现在「已签署，等待第二位批准人」禁用按钮上,故限定到横幅 span,
    // 断言等待横幅本身(而非按钮)存在。
    expect(screen.getByText(/等待第二位批准人/, { selector: 'span' })).toBeInTheDocument();
    // 就地留驻:行仍在,且没有触发第二次列表拉取。
    expect(screen.getByText('重启数据库')).toBeInTheDocument();
    expect(listCalls).toBe(1);
    // 批准按钮变为禁用的「已签署，等待第二位批准人」（全角逗号，与实现文案一致）。
    const signed = screen.getByRole('button', { name: /已签署，等待第二位批准人/ });
    expect(signed).toBeDisabled();
    // 不再使用浏览器原生确认对话框。
    expect(confirmSpy).not.toHaveBeenCalled();
  });

  it('200(executed): 就地呈现「已执行」+ 结果,且行不消失', async () => {
    const confirmSpy = vi.spyOn(window, 'confirm');
    server.use(
      http.get('/api/v1/approvals', () => HttpResponse.json({ items: [pendingRow()] })),
      http.post('/api/v1/approvals/a-sign/approve', () =>
        HttpResponse.json({
          ...pendingRow(),
          status: 'executed',
          result: JSON.stringify({ stdout: 'ok' }),
        }),
      ),
    );

    render(
      <MemoryRouter>
        <ApprovalsPage />
      </MemoryRouter>,
    );
    await screen.findByText('重启数据库');
    await userEvent.click(screen.getByRole('button', { name: '批准' }));
    await userEvent.click(await screen.findByRole('button', { name: /确认批准并执行/ }));

    // 行状态已变为 executed,StatusChip 与顶部筛选 tab 也会显示「已执行」;
    // 限定到横幅 div,断言就地留驻的执行横幅本身存在。
    expect(await screen.findByText('已执行', { selector: 'div' })).toBeInTheDocument();
    expect(screen.getByText(/ok/)).toBeInTheDocument();
    // 行未从 pending 列表消失。
    expect(screen.getByText('重启数据库')).toBeInTheDocument();
    expect(confirmSpy).not.toHaveBeenCalled();
  });

  it('failed: 就地呈现失败态 + 结果', async () => {
    const confirmSpy = vi.spyOn(window, 'confirm');
    server.use(
      http.get('/api/v1/approvals', () => HttpResponse.json({ items: [pendingRow()] })),
      http.post('/api/v1/approvals/a-sign/approve', () =>
        HttpResponse.json({
          ...pendingRow(),
          status: 'failed',
          result: 'boom',
        }),
      ),
    );

    render(
      <MemoryRouter>
        <ApprovalsPage />
      </MemoryRouter>,
    );
    await screen.findByText('重启数据库');
    await userEvent.click(screen.getByRole('button', { name: '批准' }));
    await userEvent.click(await screen.findByRole('button', { name: /确认批准并执行/ }));

    // 「失败」单独会命中 StatusChip 与顶部筛选 tab;收窄到横幅特有的「执行失败」。
    expect(await screen.findByText(/执行失败/)).toBeInTheDocument();
    expect(screen.getByText(/boom/)).toBeInTheDocument();
    expect(confirmSpy).not.toHaveBeenCalled();
  });
});

describe('Approvals 应用内确认（approval-governance 规格）', () => {
  beforeEach(() => {
    localStorage.setItem('opskeeper-locale', 'zh-CN');
  });

  function row() {
    return {
      id: 'a-confirm',
      kind: 'restart_service',
      title: '重启数据库',
      summary: '',
      payload: '{}',
      source: 'agent',
      status: 'pending',
      proposed_by: 1,
      created_at: FIXED_AT,
    };
  }

  it('批准: 出现应用内确认,焦点在「取消」,且不调用 window.confirm', async () => {
    const confirmSpy = vi.spyOn(window, 'confirm');
    server.use(http.get('/api/v1/approvals', () => HttpResponse.json({ items: [row()] })));

    render(
      <MemoryRouter>
        <ApprovalsPage />
      </MemoryRouter>,
    );
    await screen.findByText('重启数据库');
    await userEvent.click(screen.getByRole('button', { name: '批准' }));

    // 应用内确认出现(弹窗内「确认批准并执行」)。
    expect(await screen.findByRole('button', { name: /确认批准并执行/ })).toBeInTheDocument();
    // 默认焦点不在肯定动作上:焦点落在「取消」。
    expect(screen.getByRole('button', { name: '取消' })).toHaveFocus();
    // 不再使用浏览器原生对话框。
    expect(confirmSpy).not.toHaveBeenCalled();
    confirmSpy.mockRestore();
  });

  it('拒绝: 应用内 textarea 采集原因并在确认后提交,不调用 window.prompt', async () => {
    const promptSpy = vi.spyOn(window, 'prompt');
    let body: { reason?: string } | null = null;
    server.use(
      http.get('/api/v1/approvals', () => HttpResponse.json({ items: [row()] })),
      http.post('/api/v1/approvals/a-confirm/reject', async ({ request }) => {
        body = (await request.json()) as { reason?: string };
        return HttpResponse.json({ ok: true });
      }),
    );

    render(
      <MemoryRouter>
        <ApprovalsPage />
      </MemoryRouter>,
    );
    await screen.findByText('重启数据库');
    await userEvent.click(screen.getByRole('button', { name: '拒绝' }));

    const box = await screen.findByPlaceholderText('拒绝原因（可选）');
    await userEvent.type(box, '风险过高');
    await userEvent.click(screen.getByRole('button', { name: /确认拒绝/ }));

    await waitFor(() => expect(body).toEqual({ reason: '风险过高' }));
    expect(promptSpy).not.toHaveBeenCalled();
    promptSpy.mockRestore();
  });
});

describe('Approvals 危险动作确认防重复提交（approval-governance 规格）', () => {
  beforeEach(() => {
    localStorage.setItem('opskeeper-locale', 'zh-CN');
  });

  function row() {
    return {
      id: 'a-double',
      kind: 'restart_service',
      title: '重启数据库',
      summary: '',
      payload: '{}',
      source: 'agent',
      status: 'pending',
      proposed_by: 1,
      created_at: FIXED_AT,
    };
  }

  // 移除 window.confirm 后,确认动作改为应用内弹窗按钮。危险动作的批准是
  // 破坏性的、后端不保证幂等,而 doApprove 直到 finally 才关弹窗——请求在途时
  // 弹窗仍挂着、确认按钮仍可点。这里让 approve 请求悬停在途,验证确认按钮
  // 与行内按钮一样绑定 busy 守卫:快速双击只应发出一次 approve 请求。
  it('请求在途时确认按钮禁用,双击只发出一次 approve 请求', async () => {
    // approve 请求保持在途,直到测试放行,才能观察到「请求进行中」这一窗口。
    let releaseApprove!: () => void;
    const gate = new Promise<void>((resolve) => {
      releaseApprove = resolve;
    });
    let approveCalls = 0;
    server.use(
      http.get('/api/v1/approvals', () => HttpResponse.json({ items: [row()] })),
      http.post('/api/v1/approvals/a-double/approve', async () => {
        approveCalls += 1;
        await gate;
        return HttpResponse.json({ ...row(), status: 'executed', result: '{}' });
      }),
    );

    render(
      <MemoryRouter>
        <ApprovalsPage />
      </MemoryRouter>,
    );
    await screen.findByText('重启数据库');
    await userEvent.click(screen.getByRole('button', { name: '批准' }));

    const confirmBtn = await screen.findByRole('button', { name: /确认批准并执行/ });
    // 第一次点击:发起请求,但它被 gate 拦住仍在途。
    await userEvent.click(confirmBtn);
    // 请求在途时,确认按钮应绑定 busy 守卫而禁用(与行内「批准」按钮同款);
    // 未绑定时按钮仍可点,下面这一击会再发一次请求。
    await userEvent.click(confirmBtn);
    // 关键断言:危险动作只提交一次。
    expect(approveCalls).toBe(1);
    expect(confirmBtn).toBeDisabled();

    // 放行请求,收尾(避免悬空的未决 fetch 泄漏到后续测试)。
    releaseApprove();
    await waitFor(() => expect(screen.getByText('重启数据库')).toBeInTheDocument());
  });
});