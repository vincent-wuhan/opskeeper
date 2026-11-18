import { render, screen } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { server } from '@/test/msw-server';
import RuntimePage from './Runtime';

// The page asks one permission question — "is this an admin" — to decide
// whether the governance column (owner / deployment_location / labels) is
// rendered. Mocking the hook (not the zustand store behind it) keeps the
// test off /v1/me; same shape as PluginReleases.test.tsx.
let mockIsAdmin = true;
vi.mock('@/store/me', () => ({
  usePermissions: () => ({
    isAdmin: mockIsAdmin,
    canMutate: mockIsAdmin,
    role: mockIsAdmin ? 'admin' : 'user',
  }),
}));

const deploymentURL = '/api/v1/version/deployment';
const inventoryURL = '/api/v1/runtimes';

const baseResponse = {
  manager_version: 'v1.2.3',
  go_version: 'go1.24.0',
  os_arch: 'darwin/arm64',
  generated_at: '2026-10-16T00:00:00Z',
};

// Five fixtures, one per closed-set kind — the page must render each of
// the first-slice runtime surfaces without guessing at shapes. Field
// coverage is deliberately uneven across rows: the observers themselves
// are uneven (edge rows have no slots, preview-pg has no capabilities).
const fixtures = [
  {
    runtime_id: 'edge:12',
    kind: 'node_edge',
    owner: 'node-12',
    deployment_location: 'edge-fleet-a',
    labels: { env: 'prod' },
    observed_version: '0.7.43',
    version_source: 'release_metadata',
    capabilities: { last_seen_at: '2026-11-18T07:00:00Z' },
    health: 'online',
    last_heartbeat_at: '2026-11-18T07:00:00Z',
    source: 'projection',
  },
  {
    runtime_id: 'chat-worker:manager',
    kind: 'chat_worker',
    owner: 'manager',
    deployment_location: 'in-process',
    observed_version: 'v1.2.3',
    version_source: 'release_metadata',
    capabilities: { agent_count: 4, tool_count: 32 },
    health: 'online',
    source: 'projection',
  },
  {
    runtime_id: 'agentteams:worker-1',
    kind: 'agentteams_worker',
    owner: 'agentteams',
    capabilities: { ledger_readable: true, installed_plugins: 1 },
    observed_version: '0.9.0',
    version_source: 'manifest',
    health: 'online',
    source: 'projection',
  },
  {
    runtime_id: 'pluginmgr:manager',
    kind: 'plugin_manager',
    owner: 'manager',
    deployment_location: 'in-process',
    observed_version: '0.9.0',
    version_source: 'manifest',
    artifact_digest: 'sha256:0123456789abcdef0123456789abcdef',
    capabilities: { plugins: 3, kind_counts: { tool: 2, datasource: 1 }, entry_count: 3 },
    health: 'online',
    source: 'projection',
  },
  {
    runtime_id: 'previewpg:pool-fixture',
    kind: 'pool_preview',
    owner: 'repair-preview',
    observed_version: 'fp-abc',
    version_source: 'manifest',
    health: 'unknown',
    health_reason: 'pool fixture manager not configured; pool status unavailable',
    total_slots: 4,
    free_slots: 3,
    active_tasks: 1,
    source: 'projection',
  },
];

function stubDeployment(body: Record<string, unknown> = baseResponse, status = 200) {
  server.use(
    http.get(deploymentURL, () => HttpResponse.json(body, status !== 200 ? { status } : undefined)),
  );
}

function stubInventory(body: Record<string, unknown>, status = 200) {
  server.use(
    http.get(inventoryURL, () => HttpResponse.json(body, status !== 200 ? { status } : undefined)),
  );
}

function renderPage() {
  return render(
    <MemoryRouter>
      <RuntimePage />
    </MemoryRouter>,
  );
}

describe('RuntimePage', () => {
  beforeEach(() => {
    mockIsAdmin = true;
    localStorage.setItem('opskeeper-locale', 'zh-CN');
  });

  afterEach(() => {
    vi.clearAllMocks();
  });

  it('渲染全部五类运行时（含健康、能力、容量、版本、digest）', async () => {
    stubDeployment();
    stubInventory({ total: fixtures.length, items: fixtures });

    renderPage();

    // 五个 runtime_id 逐行出现。
    for (const f of fixtures) {
      expect(await screen.findByText(f.runtime_id)).toBeInTheDocument();
    }
    // 五个 kind 徽标（runtime_id 单元格的副行）。
    expect(screen.getByText(/node_edge/)).toBeInTheDocument();
    expect(screen.getByText(/chat_worker/)).toBeInTheDocument();
    expect(screen.getByText(/agentteams_worker/)).toBeInTheDocument();
    expect(screen.getByText(/plugin_manager/)).toBeInTheDocument();
    expect(screen.getByText(/pool_preview/)).toBeInTheDocument();
    // 键值能力逐项渲染。
    expect(screen.getByText('agent_count: 4')).toBeInTheDocument();
    expect(screen.getByText('tool_count: 32')).toBeInTheDocument();
    expect(screen.getByText('ledger_readable: true')).toBeInTheDocument();
    expect(screen.getByText('plugins: 3')).toBeInTheDocument();
    // 容量：preview-pg 是五类里唯一带槽位读数的。
    expect(screen.getByText(/3\/4/)).toBeInTheDocument();
    expect(screen.getByText(/当前任务 1/)).toBeInTheDocument();
    // 版本 + 来源。
    expect(screen.getAllByText(/release_metadata/).length).toBeGreaterThan(0);
    expect(screen.getAllByText(/manifest/).length).toBeGreaterThan(0);
    // 五态健康徽标：在线 + 未知（preview-pg 的 unknown）。
    expect(screen.getAllByText('在线').length).toBeGreaterThan(0);
    expect(screen.getAllByText('未知').length).toBeGreaterThan(0);
    // digest 只在 plugin_manager 一行出现。
    expect(screen.getByTitle("sha256:0123456789abcdef0123456789abcdef")).toBeInTheDocument();
  });

  it('五种健康状态（含 recently_lost / offline / degraded / starting）都有可辨别的徽标', async () => {
    stubDeployment();
    stubInventory({
      total: 5,
      items: [
        { runtime_id: 'rt:starting', kind: 'chat_worker', health: 'starting', source: 'registry' },
        { runtime_id: 'rt:online', kind: 'chat_worker', health: 'online', source: 'registry' },
        { runtime_id: 'rt:degraded', kind: 'chat_worker', health: 'degraded', health_reason: 'observed version drift', source: 'registry' },
        { runtime_id: 'rt:lost', kind: 'chat_worker', health: 'recently_lost', source: 'registry' },
        { runtime_id: 'rt:offline', kind: 'chat_worker', health: 'offline', source: 'registry' },
      ],
    });

    renderPage();

    expect(await screen.findByText('rt:starting')).toBeInTheDocument();
    expect(screen.getByText('启动中')).toBeInTheDocument();
    expect(screen.getByText('在线')).toBeInTheDocument();
    expect(screen.getByText('降级')).toBeInTheDocument();
    expect(screen.getByText('最近失联')).toBeInTheDocument();
    expect(screen.getByText('离线')).toBeInTheDocument();
    // 降级原因作为徽标副行呈现，不吞掉。
    expect(screen.getByText('observed version drift')).toBeInTheDocument();
  });

  it('空清单（total=0 / items=[]）显示空态文案而非空白表格', async () => {
    stubDeployment();
    stubInventory({ total: 0, items: [] });

    renderPage();

    expect(await screen.findByText('暂无已注册或可投影的运行时')).toBeInTheDocument();
  });

  it('清单接口 503 时保留错误面板且部署视图照常渲染', async () => {
    stubDeployment({
      ...baseResponse,
      plugin: { id: 'plugin', name: 'Plugin', version: '1.0.0' },
      skills: [],
      health: { overall: 'ok' },
      dependencies: [],
      recovery_example: {
        incident_id: 'incident',
        title: 'Incident',
        fault_family: 'network',
        phases_observed: 0,
        phases_expected: 0,
        recovery_signal: false,
        closed: true,
        duration_sec: 0,
        audit_kinds: [],
        phases: [],
      },
    });
    stubInventory(
      { error: { message: 'runtime unavailable', code: 'unavailable' } },
      503,
    );

    renderPage();

    // 清单错误面板出现。
    expect(await screen.findByText(/运行时清单加载失败/)).toBeInTheDocument();
    // 部署视图没有被清单故障拖死。
    expect(await screen.findByText('Manager')).toBeInTheDocument();
  });

  it('非 admin 看不到治理列（owner / deployment_location / labels）', async () => {
    mockIsAdmin = false;
    stubDeployment();
    stubInventory({ total: fixtures.length, items: fixtures });

    renderPage();

    expect(await screen.findByText('edge:12')).toBeInTheDocument();
    // 治理字段的值与表头都不出现。
    expect(screen.queryByText('治理')).not.toBeInTheDocument();
    expect(screen.queryByText('node-12')).not.toBeInTheDocument();
    expect(screen.queryByText('edge-fleet-a')).not.toBeInTheDocument();
    expect(screen.queryByText('env=prod')).not.toBeInTheDocument();
    // 非治理列照常。
    expect(screen.getByText('agent_count: 4')).toBeInTheDocument();
  });

  it('普通用户与管理员共用页面时兼容后端空值字段', async () => {
    stubDeployment({
      ...baseResponse,
      skills: null,
      dependencies: null,
      health: null,
      recovery_example: null,
    });
    // 后端 omitempty + #97 形状：items 是数组但字段大量缺失 / null。
    stubInventory({
      total: 2,
      items: [
        { runtime_id: 'edge:1', kind: 'node_edge', capabilities: null, labels: null, last_heartbeat_at: null },
        null,
      ],
    });

    renderPage();

    expect(await screen.findByText('暂无部署依赖数据')).toBeInTheDocument();
    expect(screen.getByText('暂无恢复阶段数据')).toBeInTheDocument();
    expect(screen.getByText('暂无 Worker Skills 数据')).toBeInTheDocument();
    expect(screen.getByText('edge:1')).toBeInTheDocument();
  });

  it('空数组时显示独立空态而不是空白表格', async () => {
    stubDeployment({
      ...baseResponse,
      plugin: { id: 'plugin', name: 'Plugin', version: '1.0.0' },
      skills: [],
      health: { overall: 'ok' },
      dependencies: [],
      recovery_example: {
        incident_id: 'incident',
        title: 'Incident',
        fault_family: 'network',
        phases_observed: 0,
        phases_expected: 0,
        recovery_signal: false,
        closed: true,
        duration_sec: 0,
        audit_kinds: [],
        phases: [],
      },
    });
    stubInventory({ total: 0, items: [] });

    renderPage();

    expect(await screen.findByText('暂无部署依赖数据')).toBeInTheDocument();
    expect(screen.getAllByText('暂无恢复阶段数据')).toHaveLength(1);
    expect(screen.getByText('暂无 Worker Skills 数据')).toBeInTheDocument();
  });

  it('部署接口错误时保留错误面板且页面不崩溃', async () => {
    stubDeployment(
      { error: { message: 'runtime unavailable', code: 'unavailable' } },
      503,
    );
    stubInventory({ total: 0, items: [] });

    renderPage();

    expect(await screen.findByText(/加载失败/)).toBeInTheDocument();
    expect(screen.queryByText('Worker Skills')).not.toBeInTheDocument();
  });
});
