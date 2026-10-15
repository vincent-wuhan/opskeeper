import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { MemoryRouter } from 'react-router-dom';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import SettingsMarketplace from './Marketplace';
import { SignatureBadge } from '@/components/marketplace/SignatureBadge';
import { setLocale } from '@/i18n/locale';
import { server } from '@/test/msw-server';
import {
  catalogFixture,
  etcdCapabilities,
  etcdPack,
  registries,
  registryOfficial,
} from '@/test/fixtures/marketplace';

// useAuth is mocked module-wide so individual tests can flip the role
// (admin vs user) without touching zustand internals. The selector form
// `useAuth((s) => s.role)` means the mock just needs to honour the
// callback contract.
let mockRole: string = 'admin';
vi.mock('@/store/auth', () => ({
  useAuth: <T,>(selector: (s: { role: string }) => T): T =>
    selector({ role: mockRole }),
  getToken: () => null,
  getRefreshToken: () => null,
}));

const installedURL = '/api/v1/marketplace/installed';
const installURL = '/api/v1/marketplace/install';
const registriesURL = '/api/v1/marketplace/registries';
const catalogURL = '/api/v1/marketplace/catalog';
const secretsURL = '/api/v1/secrets';
const installedItemURL = (id: string) =>
  `/api/v1/marketplace/installed/${encodeURIComponent(id)}`;

beforeEach(() => {
  mockRole = 'admin';
  setLocale('zh-CN');
  server.use(
    http.get(secretsURL, () => HttpResponse.json({ items: [] })),
    // The catalog section fetches on mount in every case; give it a default
    // empty catalog so cases that don't register a handler don't trip
    // onUnhandledRequest:'error'.
    http.get(catalogURL, () => HttpResponse.json({ items: [], total: 0 })),
  );
});

afterEach(() => {
  vi.clearAllMocks();
});

async function switchToLocalInstallTab() {
  await userEvent.click(
    screen.getByRole('button', { name: /本地路径|local path/i }),
  );
}

describe('SettingsMarketplace', () => {
  it('renders empty state when no packs installed', async () => {
    server.use(
      http.get(installedURL, () => HttpResponse.json({ items: [] })),
      http.get(registriesURL, () => HttpResponse.json({ items: registries })),
    );

    render(
      <MemoryRouter>
        <SettingsMarketplace />
      </MemoryRouter>,
    );

    expect(
      await screen.findByText(/还没有安装任何包/),
    ).toBeInTheDocument();
    expect(screen.getByText('已安装 (0)')).toBeInTheDocument();
  });

  it('renders installed pack list with capabilities expander', async () => {
    server.use(
      http.get(installedURL, () => HttpResponse.json({ items: [etcdPack] })),
      http.get(registriesURL, () => HttpResponse.json({ items: registries })),
    );

    render(
      <MemoryRouter>
        <SettingsMarketplace />
      </MemoryRouter>,
    );

    expect(await screen.findByText('已安装 (1)')).toBeInTheDocument();
    expect(screen.getByText('etcd-troubleshoot')).toBeInTheDocument();

    // Click the [详情] button to expand and verify bins / config_keys appear.
    const detailsBtn = screen.getByRole('button', { name: '详情' });
    await userEvent.click(detailsBtn);

    expect(await screen.findByText(/binaries/i)).toBeInTheDocument();
    expect(screen.getByText(/etcdctl/)).toBeInTheDocument();
    expect(screen.getByText(/config keys/i)).toBeInTheDocument();
    expect(screen.getByText(/ETCD_ENDPOINTS/)).toBeInTheDocument();
  });

  it('local install happy path', async () => {
    let installedItems: typeof etcdPack[] = [];
    server.use(
      http.get(installedURL, () =>
        HttpResponse.json({ items: installedItems }),
      ),
      http.get(registriesURL, () => HttpResponse.json({ items: registries })),
      http.post(installURL, async () => {
        installedItems = [etcdPack];
        return HttpResponse.json({
          pack: etcdPack,
          capabilities: etcdCapabilities,
          warnings: [],
        });
      }),
    );

    render(
      <MemoryRouter>
        <SettingsMarketplace />
      </MemoryRouter>,
    );

    // Wait for the empty state to settle, then drive the install form.
    await screen.findByText(/还没有安装任何包/);
    await switchToLocalInstallTab();

    const pathInput = screen.getByPlaceholderText(/var\/lib\/opskeeper\/uploads/);
    await userEvent.type(pathInput, '/var/lib/opskeeper/uploads/etcd-troubleshoot');

    const installBtn = screen.getByRole('button', { name: /^安装$/ });
    await userEvent.click(installBtn);

    // Confirm modal lands with capabilities snapshot.
    expect(
      await screen.findByText(/已安装: etcd-troubleshoot v0\.1\.0/),
    ).toBeInTheDocument();
    const dialog = screen.getByRole('dialog');
    expect(within(dialog).getByText('能力声明')).toBeInTheDocument();

    // Click [完成] to keep the install. List should now show the pack.
    await userEvent.click(within(dialog).getByRole('button', { name: '完成' }));

    expect(await screen.findByText('已安装 (1)')).toBeInTheDocument();
    expect(screen.getByText('etcd-troubleshoot')).toBeInTheDocument();
  });

  it('local install rollback button', async () => {
    let installedItems: typeof etcdPack[] = [];
    let deleteCalled = false;
    server.use(
      http.get(installedURL, () =>
        HttpResponse.json({ items: installedItems }),
      ),
      http.get(registriesURL, () => HttpResponse.json({ items: registries })),
      http.post(installURL, async () => {
        installedItems = [etcdPack];
        return HttpResponse.json({
          pack: etcdPack,
          capabilities: etcdCapabilities,
          warnings: [],
        });
      }),
      http.delete(installedItemURL(etcdPack.pack_id), () => {
        deleteCalled = true;
        installedItems = [];
        return new HttpResponse(null, { status: 204 });
      }),
    );

    render(
      <MemoryRouter>
        <SettingsMarketplace />
      </MemoryRouter>,
    );

    await screen.findByText(/还没有安装任何包/);
    await switchToLocalInstallTab();

    await userEvent.type(
      screen.getByPlaceholderText(/var\/lib\/opskeeper\/uploads/),
      '/var/lib/opskeeper/uploads/etcd-troubleshoot',
    );
    await userEvent.click(screen.getByRole('button', { name: /^安装$/ }));

    const dialog = await screen.findByRole('dialog');
    await userEvent.click(
      within(dialog).getByRole('button', { name: /回滚卸载/ }),
    );

    await waitFor(() => expect(deleteCalled).toBe(true));

    // Modal closed and installed list back to empty state.
    await waitFor(() =>
      expect(screen.queryByRole('dialog')).not.toBeInTheDocument(),
    );
    expect(await screen.findByText(/还没有安装任何包/)).toBeInTheDocument();
    expect(screen.queryByText('etcd-troubleshoot')).not.toBeInTheDocument();
  });

  it('install rejects duplicate sha (409)', async () => {
    server.use(
      http.get(installedURL, () => HttpResponse.json({ items: [] })),
      http.get(registriesURL, () => HttpResponse.json({ items: registries })),
      http.post(installURL, () =>
        HttpResponse.json(
          { error: 'manifest sha already installed' },
          { status: 409 },
        ),
      ),
    );

    render(
      <MemoryRouter>
        <SettingsMarketplace />
      </MemoryRouter>,
    );

    await screen.findByText(/还没有安装任何包/);
    await switchToLocalInstallTab();
    await userEvent.type(
      screen.getByPlaceholderText(/var\/lib\/opskeeper\/uploads/),
      '/var/lib/opskeeper/uploads/etcd-troubleshoot',
    );
    await userEvent.click(screen.getByRole('button', { name: /^安装$/ }));

    const toast = await screen.findByRole('status');
    expect(toast).toHaveTextContent(/已经安装过/);
  });

  it('install rejects non-allowed source (400)', async () => {
    server.use(
      http.get(installedURL, () => HttpResponse.json({ items: [] })),
      http.get(registriesURL, () => HttpResponse.json({ items: registries })),
      http.post(installURL, () =>
        HttpResponse.json(
          { error: 'source not in allow list' },
          { status: 400 },
        ),
      ),
    );

    render(
      <MemoryRouter>
        <SettingsMarketplace />
      </MemoryRouter>,
    );

    await screen.findByText(/还没有安装任何包/);
    await switchToLocalInstallTab();
    await userEvent.type(
      screen.getByPlaceholderText(/var\/lib\/opskeeper\/uploads/),
      '/etc/passwd',
    );
    await userEvent.click(screen.getByRole('button', { name: /^安装$/ }));

    const toast = await screen.findByRole('status');
    expect(toast).toHaveTextContent(/未在允许列表/);
  });

  it('non-admin sees install button disabled', async () => {
    mockRole = 'user';
    server.use(
      http.get(installedURL, () => HttpResponse.json({ items: [] })),
      http.get(registriesURL, () => HttpResponse.json({ items: registries })),
    );

    render(
      <MemoryRouter>
        <SettingsMarketplace />
      </MemoryRouter>,
    );

    await screen.findByText(/还没有安装任何包/);
    await switchToLocalInstallTab();

    // Even if the user types something, the submit button stays disabled
    // because !isAdmin gates submit independently of canSubmit.
    const installBtn = screen.getByRole('button', { name: /^安装$/ });
    expect(installBtn).toBeDisabled();
    expect(installBtn).toHaveAttribute('title', '需要 admin 权限');

    // The local-path input is also disabled for non-admin viewers.
    const pathInput = screen.getByPlaceholderText(/var\/lib\/opskeeper\/uploads/);
    expect(pathInput).toBeDisabled();

    // And the helper line nudges them toward admin login.
    expect(screen.getByText(/仅 admin 可执行安装/)).toBeInTheDocument();
  });

  it('renders the installable catalog and marks installed rows', async () => {
    server.use(
      http.get(installedURL, () => HttpResponse.json({ items: [etcdPack] })),
      http.get(registriesURL, () => HttpResponse.json({ items: registryOfficial })),
      http.get(catalogURL, () =>
        HttpResponse.json({
          items: [
            catalogFixture({ name: 'etcd-troubleshoot', version: '0.1.0', origin: 'registry' }),
            catalogFixture({ name: 'opskeeper-sre-readonly', version: '0.2.0', origin: 'registry' }),
          ],
          total: 2,
        }),
      ),
    );
    render(
      <MemoryRouter>
        <SettingsMarketplace />
      </MemoryRouter>,
    );
    // Scope to the catalog card: the InstallCard also renders a global「安装」
    // button, so a page-wide count would double-match.
    const catalog = (await screen.findByText(/可安装目录/)).closest('section')!;
    expect(within(catalog).getByText('etcd-troubleshoot')).toBeInTheDocument();
    expect(within(catalog).getByText('opskeeper-sre-readonly')).toBeInTheDocument();
    // etcd-troubleshoot is already installed → 已安装 chip, no install action.
    expect(within(catalog).getByText('已安装')).toBeInTheDocument();
    // Only the not-yet-installed row offers an install button.
    expect(within(catalog).getAllByRole('button', { name: /^安装$/ }).length).toBe(1);
  });

  it('renders local-origin rows read-only (no install action)', async () => {
    server.use(
      http.get(installedURL, () => HttpResponse.json({ items: [] })),
      http.get(registriesURL, () => HttpResponse.json({ items: registryOfficial })),
      http.get(catalogURL, () =>
        HttpResponse.json({
          items: [catalogFixture({ name: 'builtin-pack', origin: 'builtin' })],
          total: 1,
        }),
      ),
    );
    render(
      <MemoryRouter>
        <SettingsMarketplace />
      </MemoryRouter>,
    );
    const catalog = (await screen.findByText(/可安装目录/)).closest('section')!;
    expect(within(catalog).getByText('builtin-pack')).toBeInTheDocument();
    expect(
      within(catalog).queryByRole('button', { name: /^安装$/ }),
    ).not.toBeInTheDocument();
    expect(within(catalog).getByText(/本地已有|Local — already on disk/)).toBeInTheDocument();
  });

  it('hides install actions for non-admins and says why', async () => {
    mockRole = 'user';
    server.use(
      http.get(installedURL, () => HttpResponse.json({ items: [] })),
      http.get(registriesURL, () => HttpResponse.json({ items: registryOfficial })),
      http.get(catalogURL, () =>
        HttpResponse.json({
          items: [catalogFixture({ name: 'opskeeper-sre-readonly', origin: 'registry' })],
          total: 1,
        }),
      ),
    );
    render(
      <MemoryRouter>
        <SettingsMarketplace />
      </MemoryRouter>,
    );
    const catalog = (await screen.findByText(/可安装目录/)).closest('section')!;
    expect(within(catalog).getByText('opskeeper-sre-readonly')).toBeInTheDocument();
    expect(
      within(catalog).queryByRole('button', { name: /^安装$/ }),
    ).not.toBeInTheDocument();
    expect(
      within(catalog).getByText(/仅 admin 可执行安装/),
    ).toBeInTheDocument();
  });

  it('isolates a catalog failure: section shows retry, install form still renders', async () => {
    server.use(
      http.get(installedURL, () => HttpResponse.json({ items: [] })),
      http.get(registriesURL, () => HttpResponse.json({ items: registryOfficial })),
      http.get(catalogURL, () => HttpResponse.json({ error: 'boom' }, { status: 500 })),
    );
    render(
      <MemoryRouter>
        <SettingsMarketplace />
      </MemoryRouter>,
    );
    // Existing install form is unaffected by the catalog fetch failing.
    expect(await screen.findByText('安装新包')).toBeInTheDocument();
    // The catalog section lands in a failed state with a retry action.
    expect(await screen.findByText(/目录加载失败/)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: /^重试$/ })).toBeInTheDocument();
  });

  it('install from the catalog goes through an in-app Modal (no window.confirm) and sends the registry payload', async () => {
    const confirmSpy = vi.spyOn(window, 'confirm').mockReturnValue(true); // 若被调用即失败
    let installBody: unknown = null;
    let installed: typeof etcdPack[] = [];
    server.use(
      http.get(installedURL, () => HttpResponse.json({ items: installed })),
      http.get(registriesURL, () => HttpResponse.json({ items: registryOfficial })),
      http.get(catalogURL, () =>
        HttpResponse.json({ items: [catalogFixture({ name: 'opskeeper-sre-readonly', origin: 'registry' })], total: 1 }),
      ),
      http.post(installURL, async ({ request }) => {
        installBody = await request.json();
        installed = [etcdPack]; // 对账后转已安装
        return HttpResponse.json({ pack: etcdPack, capabilities: etcdCapabilities, warnings: [] });
      }),
    );
    render(<MemoryRouter><SettingsMarketplace /></MemoryRouter>);
    await screen.findByText('opskeeper-sre-readonly');

    // Scope to the catalog card: the InstallCard also renders a global「安装」
    // button, so a page-wide query would double-match.
    const catalog = screen.getByText(/可安装目录/).closest('section')!;
    await userEvent.click(within(catalog).getByRole('button', { name: /^安装$/ }));

    // 应用内 Modal,而非 window.confirm
    const dialog = await screen.findByRole('dialog');
    expect(confirmSpy).not.toHaveBeenCalled();
    // 标题与正文都含包名,用 getAllByText;来源行是正文独有,可唯一锁定。
    expect(within(dialog).getAllByText(/opskeeper-sre-readonly/).length).toBeGreaterThan(0);
    expect(within(dialog).getByText(/来源 registry/)).toBeInTheDocument();

    await userEvent.click(within(dialog).getByRole('button', { name: /确认安装|Install/ }));

    await waitFor(() =>
      expect(installBody).toEqual({
        type: 'registry',
        registry: 'opskeeper-official',
        pack_id: 'opskeeper-sre-readonly',
        version: '0.2.0',
      }),
    );
    confirmSpy.mockRestore();
  });

  it('shows a failure state with retry when the catalog install fails, and stays on the page', async () => {
    let calls = 0;
    server.use(
      http.get(installedURL, () => HttpResponse.json({ items: [] })),
      http.get(registriesURL, () => HttpResponse.json({ items: registryOfficial })),
      http.get(catalogURL, () =>
        HttpResponse.json({ items: [catalogFixture({ name: 'opskeeper-sre-readonly', origin: 'registry' })], total: 1 }),
      ),
      http.post(installURL, () => {
        calls += 1;
        return calls === 1
          ? HttpResponse.json({ error: 'conflict' }, { status: 409 })
          : HttpResponse.json({ pack: etcdPack, capabilities: etcdCapabilities, warnings: [] });
      }),
    );
    render(<MemoryRouter><SettingsMarketplace /></MemoryRouter>);
    await screen.findByText('opskeeper-sre-readonly');
    const catalog = screen.getByText(/可安装目录/).closest('section')!;
    await userEvent.click(within(catalog).getByRole('button', { name: /^安装$/ }));
    await userEvent.click(within(await screen.findByRole('dialog')).getByRole('button', { name: /确认安装|Install/ }));

    // 失败态 + 重试入口,不静默吞掉
    const retry = await screen.findByRole('button', { name: /重试|Retry/ });
    // 重试重新触发一次安装(再次经应用内确认)
    await userEvent.click(retry);
    await userEvent.click(within(await screen.findByRole('dialog')).getByRole('button', { name: /确认安装|Install/ }));
    await waitFor(() => expect(calls).toBe(2));
  });

  it('signature_state badge variants', () => {
    const { rerender, container } = render(<SignatureBadge state="verified" />);
    expect(container).toHaveTextContent('verified');

    rerender(<SignatureBadge state="unsigned" />);
    expect(container).toHaveTextContent('unsigned');

    rerender(<SignatureBadge state="failed" />);
    expect(container).toHaveTextContent('signature failed');
  });
});
