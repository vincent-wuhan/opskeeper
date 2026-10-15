// 插件市场 页面测试。
//
// 这个页面只有两件事是可��的，而两件都不是「导入成功」或「查询成功」——
// 是**导入之后那个包还没被人回答过**，以及**控制面答不出来时不许装作答了**。
// 所以断言都压在这两处：
//
//   1. 转换成功后，页面上最显眼的是那份未决清单（字段 / 问题 / 为什么
//      答不出来），不是「导入成功」。存量容器里没有工具清单、没有安全级别、
//      没有 scope、没有爆炸半径，转换器拒绝编造它们——一个把转换结果显示成
//      「完成 ✓」的页面，等于替运维宣称了一个还没人审过的包可以上机器。
//   2. `not-wired` 必须停在错误态。管理面拿不到节点版本快照时，它对
//      「谁能装」根本没有答案；渲染成一张空矩阵，运维读到的是「全网都太旧」，
//      于是去升级整个机队——一个由控制面从未说出口的句子引发的机队级变更。
import { render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { beforeEach, describe, expect, it, vi } from 'vitest';

import PluginMarketplacePage from './PluginMarketplace';
import { server } from '@/test/msw-server';

// The page is admin-only for both actions (an import changes what the
// fleet can run; the release routes are admin for the same reason), so the
// tests exercise it as an admin. The non-admin path only disables controls.
vi.mock('@/store/me', () => ({
  usePermissions: () => ({ isAdmin: true, canMutate: true, role: 'admin' }),
}));

const CONTAINER = new File(['zip-bytes'], 'acme-tools.zip', { type: 'application/zip' });

const EDGE = { id: 7, name: 'edge-prod-1', status: 'online' as const, roles: [], access_key_id: '', last_seen_at: null };

function baseHandlers() {
  return [
    // The node-inventory card lists the fleet on mount, so every test in
    // this file needs this. Handlers are per-test rather than global so a
    // test that forgets one fails loudly (onUnhandledRequest: 'error')
    // instead of quietly reaching the network.
    http.get('/api/v1/edges', () => HttpResponse.json({ items: [EDGE], total: 1 })),
    // The releases link is rendered on every page load but not followed by
    // these tests; the handler is here so an accidental call fails loudly
    // rather than reaching the network.
    http.get('/api/v1/plugins/releases', () => HttpResponse.json({ items: [] })),
  ];
}

const REPORT = {
  dest: '/var/lib/opskeeper/plugins/acme-tools',
  kind: 'claude',
  name: 'acme-tools',
  version: '0.3.0',
  description: 'a legacy container',
  skills: ['skills/triage/SKILL.md'],
  agents: ['agents/sre.md'],
  prompts: 1,
  mcp: 0,
  extensions: 0,
  themes: 0,
  agent_environments: 0,
  source_manifest: { present: false, declares_resources: false },
  decisions: [
    { field: 'spec.tools', question: 'Which tools may this package call?', why: 'the container declares none' },
    { field: 'safety_level', question: 'How dangerous is it?', why: 'not derivable from a file list' },
  ],
  warnings: [{ path: 'hooks/pre.py', reason: 'hook is declared but missing', code: 'missing_file' }],
};

async function convert() {
  const user = userEvent.setup();
  render(
    <MemoryRouter>
      <PluginMarketplacePage />
    </MemoryRouter>
  );
  const input = await screen.findByLabelText(/choose a zip/i, { selector: 'input' });
  await user.upload(input, CONTAINER);
  await user.click(screen.getByRole('button', { name: /convert/i }));
  return user;
}

describe('PluginMarketplacePage', () => {
  beforeEach(() => {
    localStorage.setItem('opskeeper-locale', 'en-US');
  });

  it('leads with the undecided questions rather than a success state', async () => {
    server.use(
      ...baseHandlers(),
      http.post('/api/v1/marketplace/import', () => HttpResponse.json(REPORT))
    );

    await convert();

    // The three parts of a decision are all rendered. `why` is the one
    // that tells a reviewer whether the converter or the vendor has to
    // think, so leaving it behind a tooltip would lose the distinction.
    expect(await screen.findByText('spec.tools')).toBeInTheDocument();
    expect(screen.getByText('Which tools may this package call?')).toBeInTheDocument();
    expect(screen.getByText('the container declares none')).toBeInTheDocument();
    expect(screen.getByText('safety_level')).toBeInTheDocument();

    // Where it landed, described as written — not as installed. The route
    // deliberately stops short of publishing so the review the conversion
    // exists to force cannot be skipped, and the wording is the only place
    // an operator reads that.
    expect(screen.getByText('/var/lib/opskeeper/plugins/acme-tools')).toBeInTheDocument();
    expect(screen.getByText(/An import installs nothing/)).toBeInTheDocument();
  });

  it('treats an empty decision list as something to check, not as success', async () => {
    // Every real legacy container produces decisions, because none of them
    // carry governance. An empty list therefore means the container DID
    // claim to carry it — which is exactly the thing worth a second look,
    // and the opposite of a green tick.
    server.use(
      ...baseHandlers(),
      http.post('/api/v1/marketplace/import', () =>
        HttpResponse.json({ ...REPORT, decisions: [] })
      )
    );

    await convert();

    expect(await screen.findByText(/0 decision\(s\) still to make/)).toBeInTheDocument();
    expect(screen.getByText(/worth confirming that is true/)).toBeInTheDocument();
  });

  it('surfaces loader warnings instead of dropping them', async () => {
    server.use(
      ...baseHandlers(),
      http.post('/api/v1/marketplace/import', () => HttpResponse.json(REPORT))
    );

    await convert();

    expect(await screen.findByText('hooks/pre.py')).toBeInTheDocument();
    expect(screen.getByText(/hook is declared but missing/)).toBeInTheDocument();
  });

  it('says the file is not the problem when the manager has no import root', async () => {
    // 503 is an operator-fixable configuration gap. Flattened into "import
    // failed" it sends them to re-zip an archive that was fine.
    server.use(
      ...baseHandlers(),
      http.post('/api/v1/marketplace/import', () =>
        HttpResponse.json(
          { error: 'plugin import is not configured (set OPSKEEPER_PLUGIN_IMPORT_DIR)', code: 'not-wired' },
          { status: 503 }
        )
      )
    );

    await convert();

    expect(await screen.findByText(/no import directory configured/)).toBeInTheDocument();
    expect(screen.getByText(/The file is not the problem/)).toBeInTheDocument();
  });

  it('does not invite a retry when the package already exists', async () => {
    // 409 exists because a second import would otherwise replace a package
    // an operator had already answered some of those questions on. A
    // console that said "try again" would walk them straight into
    // destroying review work.
    server.use(
      ...baseHandlers(),
      http.post('/api/v1/marketplace/import', () =>
        HttpResponse.json(
          { error: 'acme-tools already exists; an import replaces nothing', code: 'conflict' },
          { status: 409 }
        )
      )
    );

    await convert();

    expect(await screen.findByText(/An import replaces nothing/)).toBeInTheDocument();
    expect(screen.queryByText(/try again/i)).not.toBeInTheDocument();
  });

  it('shows a refusal with the axis that refused and the node’s own sentence', async () => {
    server.use(
      ...baseHandlers(),
      http.get('/api/v1/plugins/acme-tools/compatibility', () =>
        HttpResponse.json({
          plugin: 'acme-tools',
          version: '1.4.0',
          min_edge_version: '0.7.0',
          min_pig_version: '0.3.0',
          hostable: [{ node_id: 2, name: 'edge-b', edge_version: '0.8.1', pig_version: '0.3.0', hostable: true }],
          refused: [
            {
              node_id: 1,
              name: 'edge-a',
              edge_version: '0.6.9',
              pig_version: '0.2.0',
              hostable: false,
              step: 'version',
              reason: 'this node runs edge 0.6.9 and the package needs 0.7.0',
            },
          ],
        })
      )
    );

    const user = userEvent.setup();
    render(
      <MemoryRouter>
        <PluginMarketplacePage />
      </MemoryRouter>
    );
    await user.type(await screen.findByPlaceholderText('opskeeper-sre-repair'), 'acme-tools');
    await user.click(screen.getByRole('button', { name: /check compatibility/i }));

    expect(await screen.findByText('edge-b')).toBeInTheDocument();
    expect(screen.getByText('hostable')).toBeInTheDocument();

    // Both halves of a refusal: WHICH axis (so the upgrade is aimed at the
    // right component) and the sentence the node itself would use, rather
    // than a second wording invented here that would drift from the node's.
    expect(screen.getByText('edge too old')).toBeInTheDocument();
    expect(
      screen.getByText('this node runs edge 0.6.9 and the package needs 0.7.0')
    ).toBeInTheDocument();
  });

  it('renders a missing version snapshot as an error, never as an empty matrix', async () => {
    // The assertion is on the ABSENCE of the matrix, not on the presence of
    // the error. "Hostable (0) / Refused (0)" reads as a uniformly-too-old
    // fleet, and upgrading the fleet is a heavy, expensive answer to a
    // sentence the control plane never said.
    server.use(
      ...baseHandlers(),
      http.get('/api/v1/plugins/*/compatibility', () =>
        HttpResponse.json({ error: 'no version snapshot', code: 'not-wired' }, { status: 503 })
      )
    );

    const user = userEvent.setup();
    render(
      <MemoryRouter>
        <PluginMarketplacePage />
      </MemoryRouter>
    );
    await user.type(await screen.findByPlaceholderText('opskeeper-sre-repair'), 'acme-tools');
    await user.click(screen.getByRole('button', { name: /check compatibility/i }));

    expect(await screen.findByText(/no node version snapshot/)).toBeInTheDocument();
    expect(screen.getByText(/not the same as no node being able to host it/)).toBeInTheDocument();
    await waitFor(() =>
      expect(screen.queryByText(/Hostable \(/)).not.toBeInTheDocument()
    );
  });

  it('says a node that answered with nothing RUNS nothing, which is a fact', async () => {
    // The empty list is the answer, and it has to be phrased as an answer.
    // An empty `<ul>` with no line of text is indistinguishable on screen
    // from a card that has not run yet or a request that failed, and an
    // operator reading "nothing here" against a host they have not
    // successfully asked is being told a clean bill of health they did not
    // earn.
    server.use(
      ...baseHandlers(),
      http.get('/api/v1/plugins/nodes/7/installed', () =>
        HttpResponse.json({ edge_id: 7, packages: [] })
      )
    );

    const user = userEvent.setup();
    render(
      <MemoryRouter>
        <PluginMarketplacePage />
      </MemoryRouter>
    );
    await user.click(screen.getByRole('button', { name: /check node/i }));

    expect(await screen.findByText(/it has no plugin packages installed/)).toBeInTheDocument();
  });

  it('does not let a silent node read as a clean one', async () => {
    // 502 is a fact about ONE host. The failure text has to say so
    // explicitly, because the alternative reading — "no packages" — is
    // the exact sentence that would let an unpatched host pass review.
    server.use(
      ...baseHandlers(),
      http.get('/api/v1/plugins/nodes/7/installed', () =>
        HttpResponse.json(
          { error: { message: 'node 7 did not answer: i/o timeout', code: 'node_unreachable' } },
          { status: 502 }
        )
      )
    );

    const user = userEvent.setup();
    render(
      <MemoryRouter>
        <PluginMarketplacePage />
      </MemoryRouter>
    );
    await user.click(screen.getByRole('button', { name: /check node/i }));

    expect(await screen.findByText(/This node did not answer/)).toBeInTheDocument();
    expect(screen.getByText(/not the same as it having nothing installed/)).toBeInTheDocument();
    expect(screen.queryByText(/has no plugin packages installed/)).not.toBeInTheDocument();
  });

  it('does not let a manager with no tunnel claim the fleet is empty', async () => {
    // 503 says this control plane cannot ask anyone. Rendering that as an
    // empty fleet would be a claim about every host at once, made by a
    // component that reached none of them.
    server.use(
      ...baseHandlers(),
      http.get('/api/v1/plugins/nodes/7/installed', () =>
        HttpResponse.json({ error: { message: 'not wired', code: 'not_wired' } }, { status: 503 })
      )
    );

    const user = userEvent.setup();
    render(
      <MemoryRouter>
        <PluginMarketplacePage />
      </MemoryRouter>
    );
    await user.click(screen.getByRole('button', { name: /check node/i }));

    expect(await screen.findByText(/cannot ask any node/)).toBeInTheDocument();
    expect(screen.getByText(/does not mean every node is empty/)).toBeInTheDocument();
  });

  it('lists what a node reports, with the digest the node computed', async () => {
    server.use(
      ...baseHandlers(),
      http.get('/api/v1/plugins/nodes/7/installed', () =>
        HttpResponse.json({
          edge_id: 7,
          packages: [
            { name: 'opskeeper-sre-readonly', version: '1.2.0', digest: 'sha256:abcdef0123456789' },
            { name: 'opskeeper-sre-repair', version: '0.4.1' },
          ],
        })
      )
    );

    const user = userEvent.setup();
    render(
      <MemoryRouter>
        <PluginMarketplacePage />
      </MemoryRouter>
    );
    await user.click(screen.getByRole('button', { name: /check node/i }));

    expect(await screen.findByText('opskeeper-sre-readonly')).toBeInTheDocument();
    expect(screen.getByText('1.2.0')).toBeInTheDocument();
    // The digest is the node's own tree digest — the value a release
    // compares against what it meant to ship — so it is shown rather than
    // hidden, and a package with no digest is rendered without a gap.
    expect(screen.getByText('sha256:abcdef0123456789')).toBeInTheDocument();
    expect(screen.getByText('0.4.1')).toBeInTheDocument();
  });

  it('offers read-only jumps to the release console and the node install surface', async () => {
    server.use(...baseHandlers());
    render(
      <MemoryRouter>
        <PluginMarketplacePage />
      </MemoryRouter>
    );
    // 发布控制台(已存在)+ 节点安装面(新增)
    expect(
      await screen.findByRole('link', { name: /发布控制台|Release console/ })
    ).toHaveAttribute('href', '/admin/plugins');
    expect(
      screen.getByRole('link', { name: /节点安装面|Node install surface/ })
    ).toHaveAttribute('href', '/devices');
    // 不渲染任何 install/approve 假动作、没有第二套发布模型
    expect(
      screen.queryByRole('button', { name: /^Install|^安装$/ })
    ).not.toBeInTheDocument();
    expect(
      screen.queryByRole('button', { name: /^Approve|^批准/ })
    ).not.toBeInTheDocument();
  });
});

// A container that selected its resources by manifest is the one conversion
// whose result is a different SET of resources rather than a different
// quality of the same set, so the page has to say so above the fold. The
// two counts are here for the narrower reason: an uncounted class is a
// class nobody can tell is missing, which is exactly how `themes` and
// `agent-environments` went missing from a report that read identically.
describe('PluginMarketplacePage resource accounting', () => {
  const withReport = (patch: Record<string, unknown>) => {
    server.use(
      ...baseHandlers(),
      http.post('/api/v1/marketplace/import', () =>
        HttpResponse.json({ ...REPORT, ...patch })
      )
    );
  };

  beforeEach(() => {
    localStorage.setItem('opskeeper-locale', 'en-US');
  });

  it('counts the two classes an earlier converter dropped', async () => {
    withReport({ themes: 2, agent_environments: 1 });
    await convert();
    expect(await screen.findByText(/2 theme\(s\)/i)).toBeInTheDocument();
    expect(screen.getByText(/1 agent environment\(s\)/i)).toBeInTheDocument();
  });

  it('says so when the source declared its resources by manifest', async () => {
    withReport({
      source_manifest: {
        present: true,
        declares_resources: true,
        classes: ['skills', 'themes'],
        entries: { skills: ['skills/triage/SKILL.md'] },
      },
    });
    await convert();
    // The superset is a change to what the node will serve, so it is stated
    // rather than left to the decisions list — a reviewer skimming for the
    // undecided questions is exactly who would otherwise miss it.
    expect(await screen.findByText(/discovers by convention/i)).toBeInTheDocument();
  });

  it('says nothing about a manifest that declared nothing', async () => {
    withReport({
      source_manifest: { present: true, declares_resources: false },
    });
    await convert();
    // The report has rendered once its first decision is on screen, which
    // is the same moment the resource line and the callout would have been.
    await screen.findByText('spec.tools');
    expect(screen.queryByText(/discovers by convention/i)).not.toBeInTheDocument();
  });
});
