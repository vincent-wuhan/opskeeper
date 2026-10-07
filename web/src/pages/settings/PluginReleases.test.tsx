import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { MemoryRouter } from 'react-router-dom';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import PluginReleases from './PluginReleases';
import { setLocale } from '@/i18n/locale';
import { server } from '@/test/msw-server';

// usePermissions is mocked rather than the zustand stores behind it: the
// page's only permission question is "is this an admin", and mocking one
// hook keeps the test from depending on how /v1/me resolves.
let mockIsAdmin = true;
vi.mock('@/store/me', () => ({
  usePermissions: () => ({ isAdmin: mockIsAdmin, role: mockIsAdmin ? 'admin' : 'viewer' }),
}));

const releasesURL = '/api/v1/plugins/releases';
const releaseURL = (name: string) => `${releasesURL}/${encodeURIComponent(name)}`;

// A release mid-canary: wave 1 of 3, one node that has not answered.
// `pending` is the field this page exists to render correctly — it is not
// a failure, and it is what blocks `advance`.
const midCanary = {
  plugin: 'opskeeper-sre-repair',
  version: '1.0.0',
  wave: 1,
  waves: 3,
  pending: [7],
  failed: null,
  summary: 'wave 1 of 3: 1 installed, 1 pending',
};

beforeEach(() => {
  mockIsAdmin = true;
  setLocale('zh-CN');
  server.use(http.get(releasesURL, () => HttpResponse.json({ items: [], total: 0 })));
});

afterEach(() => {
  vi.clearAllMocks();
});

function renderPage() {
  return render(
    <MemoryRouter>
      <PluginReleases />
    </MemoryRouter>,
  );
}

describe('PluginReleases', () => {
  it('renders the empty state when nothing is in flight', async () => {
    renderPage();
    expect(await screen.findByText(/没有进行中的发布|No releases in flight/)).toBeInTheDocument();
  });

  it('shows a node that has not answered as pending, not as a failure', async () => {
    // The distinction is the page's whole reason for existing. A pending
    // node is "we do not know yet" and blocks the wave; a failed node is
    // an answer and does not. Rendering them the same is how an operator
    // restarts a release that is simply waiting.
    server.use(http.get(releasesURL, () => HttpResponse.json({ items: [midCanary], total: 1 })));
    renderPage();

    expect(await screen.findByText('opskeeper-sre-repair')).toBeInTheDocument();
    expect(screen.getByText(/未回音/)).toBeInTheDocument();
    expect(screen.queryByText(/个节点失败/)).not.toBeInTheDocument();
  });

  it('disables advance while a node in the wave is unaccounted for', async () => {
    // The backend refuses this too; the console must not offer a button
    // whose only outcome is an error. A pending node blocks the wave by
    // design — one machine that is down for unrelated reasons must not
    // stall the fleet silently, so it is surfaced instead.
    server.use(http.get(releasesURL, () => HttpResponse.json({ items: [midCanary], total: 1 })));
    renderPage();

    const advance = await screen.findByRole('button', { name: '推进' });
    expect(advance).toBeDisabled();
  });

  it('enables advance once the wave is accounted for', async () => {
    server.use(
      http.get(releasesURL, () =>
        HttpResponse.json({ items: [{ ...midCanary, pending: [] }], total: 1 }),
      ),
    );
    renderPage();

    const advance = await screen.findByRole('button', { name: '推进' });
    expect(advance).toBeEnabled();
  });

  it('surfaces a wave that did not move rather than reporting success', async () => {
    // `moved: false` is the backend saying "nothing advanced". The page
    // must not silently refresh and look like it worked.
    server.use(
      http.get(releasesURL, () =>
        HttpResponse.json({ items: [{ ...midCanary, pending: [] }], total: 1 }),
      ),
      http.post(`${releaseURL('opskeeper-sre-repair')}/advance`, () =>
        HttpResponse.json({ moved: false, ...midCanary }),
      ),
    );
    renderPage();

    await userEvent.click(await screen.findByRole('button', { name: '推进' }));
    expect(await screen.findByText(/没有前进/)).toBeInTheDocument();
  });

  it('tells the operator that halt leaves the installed package alone', async () => {
    // halt and rollback are different calls, and the difference is the
    // one thing an operator under pressure gets wrong.
    server.use(http.get(releasesURL, () => HttpResponse.json({ items: [midCanary], total: 1 })));
    renderPage();

    await userEvent.click(await screen.findByRole('button', { name: '停止' }));
    expect(await screen.findByText(/不会/)).toBeInTheDocument();
  });

  it('sends the operator sentence with the halt', async () => {
    // The reason lands in the audit payload. It is the only field in the
    // row that explains why, and the reader is usually not the writer.
    let body: unknown = null;
    server.use(
      http.get(releasesURL, () => HttpResponse.json({ items: [midCanary], total: 1 })),
      http.post(`${releaseURL('opskeeper-sre-repair')}/halt`, async ({ request }) => {
        body = await request.json();
        return HttpResponse.json({ ...midCanary, halted: true });
      }),
    );
    renderPage();

    await userEvent.click(await screen.findByRole('button', { name: '停止' }));
    const input = await screen.findByPlaceholderText(/原因/);
    await userEvent.type(input, 'golden signals 恶化');
    // The dialog's confirm button, not the row's — both say 停止.
    const buttons = screen.getAllByRole('button', { name: '停止' });
    await userEvent.click(buttons[buttons.length - 1]);

    await waitFor(() => expect(body).toEqual({ reason: 'golden signals 恶化' }));
  });

  it('does not offer start to a non-admin', async () => {
    mockIsAdmin = false;
    renderPage();
    await screen.findByText(/没有进行中的发布|No releases in flight/);
    expect(screen.queryByRole('button', { name: /新建发布/ })).not.toBeInTheDocument();
  });

  it('explains that an unwired transport is a configuration problem', async () => {
    // 503 means the tunnel is not mounted, not that a release failed. An
    // operator sent to the logs for a deployment gap loses an hour.
    server.use(
      http.get(releasesURL, () =>
        HttpResponse.json({ error: { message: 'not wired', code: 'not_wired' } }, { status: 503 }),
      ),
    );
    renderPage();
    expect(await screen.findByText(/未接线/)).toBeInTheDocument();
  });
});
