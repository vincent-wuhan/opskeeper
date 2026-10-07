// 自愈规则 页面测试。
//
// 这个页面的两件事值得钉住，都不是「列表渲染成功」：
//
//   1. `not-wired` 必须停在明确的状态，而不是渲染成空列表。管理面从没
//      接入账本时，它对「平台准备自己跑哪些修复」根本没有答案；渲染成一页
//      「还没有模式被晋升」，运维读到的是「自愈规则一个都没有」，于是不会
//      去查为什么结晶没接上。
//   2. 详情抽屉展示的是**那份送审的 pig-ops.yaml 原文**，逐词 argv 与来源
//      注释都在。审批人据此决定，而不是只看一个总结。
//
// 页面**没有** approve/install 按钮——写草稿是它走得最远的一步，安装仍然
// 在发布控制台。把这条钉进测试，是防止后来者「顺手」加一个安装按钮。
import { render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { beforeEach, describe, expect, it, vi } from 'vitest';

import CrystallizedPage from './Crystallized';
import { server } from '@/test/msw-server';

vi.mock('@/store/me', () => ({
  usePermissions: () => ({ isAdmin: true, canMutate: true, role: 'admin' }),
}));

const PATTERN = {
  name: 'opskeeper-crystallized-host-disk-full-abc123',
  action: 'crystallized-host-disk-full-abc123',
  fault_kind: 'host.disk_full',
  family: 'host',
  tool: 'host.restart_service',
  class: 'write',
  argv: ['systemctl', 'restart', 'orders-api'],
  target: 'host:i-0abc123',
  trigger: { kind: 'metric_above', metric: 'node_disk_used_ratio', threshold: 0.92 },
  blast_radius: 'pod',
  ttl_seconds: 900,
  safety_level: 'L2',
  streak: 3,
  verified: 3,
  attempts: 3,
  rejections: 0,
  first_seen: '2026-09-01T12:00:00Z',
  last_seen: '2026-09-01T12:02:00Z',
  promoted_at: '2026-09-01T12:02:00Z',
  evidence: ['incident-a', 'incident-b', 'incident-c'],
};

const POLICY = {
  min_clean_streak: 3,
  max_ttl_seconds: 1800,
  package_prefix: 'opskeeper-crystallized',
  version: '0.1.0-draft',
  vendor: 'opskeeper',
};

function renderPage() {
  return render(
    <MemoryRouter>
      <CrystallizedPage />
    </MemoryRouter>
  );
}

describe('CrystallizedPage', () => {
  beforeEach(() => {
    localStorage.setItem('opskeeper-locale', 'en-US');
  });

  it('shows the exact command and the grant for a promoted pattern', async () => {
    server.use(
      http.get('/api/v1/loops/crystallized', () =>
        HttpResponse.json({ items: [PATTERN], total: 1, policy: POLICY })
      )
    );
    renderPage();
    // The literal argv is the point of the page: an operator approves a
    // program, not a description of one.
    expect(await screen.findByText('systemctl restart orders-api')).toBeInTheDocument();
    expect(screen.getByText('host:i-0abc123')).toBeInTheDocument();
    expect(screen.getByText('pod')).toBeInTheDocument();
    // The promotion policy is stated so a reviewer knows what "3 clean"
    // meant without hardcoding it.
    expect(screen.getByText(/3 consecutive first-try verifications/)).toBeInTheDocument();
  });

  it('does not offer an install button', async () => {
    server.use(
      http.get('/api/v1/loops/crystallized', () =>
        HttpResponse.json({ items: [PATTERN], total: 1, policy: POLICY })
      )
    );
    renderPage();
    await screen.findByText('systemctl restart orders-api');
    // 落盘 is the furthest the page goes. There is no install/approve.
    expect(screen.getByRole('button', { name: /Write for review/ })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /Install/i })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /Approve/i })).not.toBeInTheDocument();
  });

  it('treats not-wired as a state, not as an empty list', async () => {
    server.use(
      http.get('/api/v1/loops/crystallized', () =>
        HttpResponse.json(
          { error: 'cost crystallisation is not wired on this manager', code: 'not-wired' },
          { status: 503 }
        )
      )
    );
    renderPage();
    // The loading state resolves into an explicit "not wired" banner; an
    // empty list would read as "nothing has been promoted anywhere".
    expect(await screen.findByText(/not wired on this manager/)).toBeInTheDocument();
    // And the empty-state copy that means "none promoted" is NOT shown.
    expect(screen.queryByText('No pattern has earned a runbook yet')).not.toBeInTheDocument();
  });

  it('scopes an empty list to the window it counted, not to all history', async () => {
    // The ledger is in-memory by design, so a manager that restarted serves
    // a valid, empty list meaning "nothing since boot". Saying "no pattern
    // has earned a runbook yet" there is a claim about all of history that
    // this page cannot support, and the two readings call for opposite
    // responses from the operator.
    server.use(
      http.get('/api/v1/loops/crystallized', () =>
        HttpResponse.json({
          items: [],
          total: 0,
          policy: POLICY,
          observing_since: '2026-10-05T11:58:00Z',
        })
      )
    );
    renderPage();
    expect(await screen.findByText(/No pattern has been promoted since 2026-10-05T11:58:00Z/))
      .toBeInTheDocument();
    // The unbounded claim is exactly what must not be shown.
    expect(screen.queryByText('No pattern has earned a runbook yet')).not.toBeInTheDocument();
    // And the reason has to be stated, or the operator cannot tell a fleet
    // that has promoted nothing from evidence a restart threw away.
    expect(screen.getByText(/in-memory/)).toBeInTheDocument();
  });

  it('falls back to the plain copy when the server sends no window', async () => {
    // An older manager does not send observing_since. Guessing a window here
    // would be inventing one; the copy has to fall back rather than claim
    // an observation start nobody reported.
    server.use(
      http.get('/api/v1/loops/crystallized', () =>
        HttpResponse.json({ items: [], total: 0, policy: POLICY })
      )
    );
    renderPage();
    expect(await screen.findByText('No pattern has earned a runbook yet')).toBeInTheDocument();
  });

  it('renders the exact pig-ops.yaml in the detail drawer', async () => {
    const yaml = [
      '# Draft, not a release: emitted by opskeeper crystallize and awaiting review.',
      'apiVersion: opskeeper.io/v1',
      'kind: Plugin',
      'spec:',
      '  autonomy:',
      '    actions:',
      '      - argv: [systemctl, restart, orders-api]',
    ].join('\n');
    server.use(
      http.get('/api/v1/loops/crystallized', () =>
        HttpResponse.json({ items: [PATTERN], total: 1, policy: POLICY })
      ),
      http.get(`/api/v1/loops/crystallized/${PATTERN.name}`, () =>
        HttpResponse.json({ pattern: PATTERN, yaml })
      )
    );
    renderPage();
    await screen.findByText('systemctl restart orders-api');
    await userEvent.click(screen.getByRole('button', { name: /View declaration/ }));
    await waitFor(() => {
      expect(screen.getByText(/Draft, not a release/)).toBeInTheDocument();
    });
    expect(screen.getByText(/approval happens in the release console/i)).toBeInTheDocument();
  });
});
