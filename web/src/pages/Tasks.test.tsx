// 任务页「创建每日值班简报」预设入口测试 —— 守住 spec「一键创建预设」:
// 点击预设 → 预填表单 → 经既有创建接口提交,请求体携带 daily/09:00/飞书渠道/三节模板。
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter } from 'react-router-dom';
import { http, HttpResponse } from 'msw';
import { describe, expect, it, vi } from 'vitest';

import TasksPage from './Tasks';
import { ConfirmProvider } from '@/components/ui';
import { server } from '@/test/msw-server';
import { DAILY_BRIEF_PROMPT } from '@/lib/dailyBrief';

// @/api/client 依赖 store/auth 的 token;mock 掉避免持久化状态。
vi.mock('@/store/auth', () => ({
  useAuth: Object.assign(
    <T,>(selector: (s: { role: string }) => T): T => selector({ role: 'admin' }),
    { getState: () => ({ logout: () => {} }) },
  ),
  getToken: () => null,
  getRefreshToken: () => null,
}));
// 页面用 usePermissions().canMutate 门控「新建任务」。
vi.mock('@/store/me', () => ({
  usePermissions: () => ({ isAdmin: true, canMutate: true, role: 'admin' }),
}));

const FEISHU = { id: 7, name: '飞书群', type: 'feishu', enabled: true, created_at: '', updated_at: '' };
const SLACK = { id: 9, name: 'Slack', type: 'slack', enabled: true, created_at: '', updated_at: '' };

function renderTasks() {
  return render(
    <MemoryRouter>
      <ConfirmProvider>
        <TasksPage />
      </ConfirmProvider>
    </MemoryRouter>,
  );
}

function listHandlers(channels: unknown[]) {
  return [
    http.get('/api/v1/tasks', () => HttpResponse.json({ tasks: [] })),
    http.get('/api/v1/notification-channels', () => HttpResponse.json({ items: channels, total: channels.length })),
  ];
}

describe('每日值班简报预设', () => {
  it('预填并提交 daily/09:00/飞书渠道/三节模板', async () => {
    let body: Record<string, unknown> | null = null;
    server.use(
      ...listHandlers([FEISHU, SLACK]),
      http.post('/api/v1/report-schedules', async ({ request }) => {
        body = (await request.json()) as Record<string, unknown>;
        return HttpResponse.json({ id: 1, name: '每日值班简报', kind: 'daily', cron_spec: '0 9 * * *', timezone: 'UTC', scope_json: '{}', channel_ids: [7], in_app_visible: true, agent_persona: 'reporter', enabled: true, created_at: '' });
      }),
    );

    renderTasks();
    await userEvent.click(await screen.findByRole('button', { name: /新建任务|New task/ }));
    await userEvent.click(await screen.findByText(/每日值班简报|Daily on-call brief/));

    // 表单已预填:名称出现。注意:注入的三节模板 textarea 本身也含「每日值班简报」
    // 字样,findByDisplayValue 会多重命中,故锁定「名称」输入框核对预填值。
    const nameField = await screen.findByRole('textbox', { name: /名称|Name/ });
    expect((nameField as HTMLInputElement).value).toMatch(/^(每日值班简报|Daily on-call brief)$/);

    await userEvent.click(screen.getByRole('button', { name: /保存|Save/ }));

    await waitFor(() => expect(body).not.toBeNull());
    expect(body).toMatchObject({ kind: 'daily', cron_spec: '0 9 * * *', channel_ids: [7] });
    expect(body).toHaveProperty('prompt_override', DAILY_BRIEF_PROMPT);
    expect(body).not.toHaveProperty('agent_persona'); // persona 由后端默认 reporter
  });

  it('无飞书渠道 → channel_ids 为空(降级仅平台内)', async () => {
    let body: Record<string, unknown> | null = null;
    server.use(
      ...listHandlers([SLACK]),
      http.post('/api/v1/report-schedules', async ({ request }) => {
        body = (await request.json()) as Record<string, unknown>;
        return HttpResponse.json({ id: 2, name: '每日值班简报', kind: 'daily', cron_spec: '0 9 * * *', timezone: 'UTC', scope_json: '{}', channel_ids: [], in_app_visible: true, agent_persona: 'reporter', enabled: true, created_at: '' });
      }),
    );

    renderTasks();
    await userEvent.click(await screen.findByRole('button', { name: /新建任务|New task/ }));
    await userEvent.click(await screen.findByText(/每日值班简报|Daily on-call brief/));
    await userEvent.click(screen.getByRole('button', { name: /保存|Save/ }));

    await waitFor(() => expect(body).not.toBeNull());
    expect(body).toHaveProperty('channel_ids', []);
  });

  // phase4-ui-deepening:一键预设项与相邻两个普通新建项区分开(一条分隔线),
  // 让它读起来像一个「预设」而非第三个等价动作。文案与点击行为不变。
  it('预设入口与普通新建项视觉可辨识(有分隔线)', async () => {
    server.use(...listHandlers([FEISHU]));
    renderTasks();
    await userEvent.click(await screen.findByRole('button', { name: /新建任务|New task/ }));
    const label = await screen.findByText(/每日值班简报|Daily on-call brief/);
    const btn = label.closest('button');
    expect(btn).not.toBeNull();
    expect(btn?.className).toContain('border-t'); // 与上方普通项分隔
  });
});
