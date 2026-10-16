import { render, screen } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { http, HttpResponse } from 'msw';
import { beforeEach, describe, expect, it } from 'vitest';

import { server } from '@/test/msw-server';
import RuntimePage from './Runtime';

const deploymentURL = '/api/v1/version/deployment';

const baseResponse = {
  manager_version: 'v1.2.3',
  go_version: 'go1.24.0',
  os_arch: 'darwin/arm64',
  generated_at: '2026-10-16T00:00:00Z',
};

describe('RuntimePage', () => {
  beforeEach(() => {
    localStorage.setItem('opskeeper-locale', 'zh-CN');
  });

  it('普通用户与管理员共用页面时兼容后端空值字段', async () => {
    server.use(
      http.get(deploymentURL, () =>
        HttpResponse.json({
          ...baseResponse,
          skills: null,
          dependencies: null,
          health: null,
          recovery_example: null,
        }),
      ),
    );

    render(
      <MemoryRouter>
        <RuntimePage />
      </MemoryRouter>,
    );

    expect(await screen.findByText('暂无部署依赖数据')).toBeInTheDocument();
    expect(screen.getByText('暂无恢复阶段数据')).toBeInTheDocument();
    expect(screen.getByText('暂无 Worker Skills 数据')).toBeInTheDocument();
    expect(screen.getAllByText('未知').length).toBeGreaterThan(0);
  });

  it('空数组时显示独立空态而不是空白表格', async () => {
    server.use(
      http.get(deploymentURL, () =>
        HttpResponse.json({
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
        }),
      ),
    );

    render(
      <MemoryRouter>
        <RuntimePage />
      </MemoryRouter>,
    );

    expect(await screen.findByText('暂无部署依赖数据')).toBeInTheDocument();
    expect(screen.getAllByText('暂无恢复阶段数据')).toHaveLength(1);
    expect(screen.getByText('暂无 Worker Skills 数据')).toBeInTheDocument();
  });

  it('接口错误时保留错误面板且页面不崩溃', async () => {
    server.use(
      http.get(deploymentURL, () =>
        HttpResponse.json(
          { error: { message: 'runtime unavailable', code: 'unavailable' } },
          { status: 503 },
        ),
      ),
    );

    render(
      <MemoryRouter>
        <RuntimePage />
      </MemoryRouter>,
    );

    expect(await screen.findByText(/加载失败/)).toBeInTheDocument();
    expect(screen.queryByText('Worker Skills')).not.toBeInTheDocument();
  });
});
