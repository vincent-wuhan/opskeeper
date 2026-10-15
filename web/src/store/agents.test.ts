import { beforeEach, describe, expect, it, vi } from 'vitest';

const listAgents = vi.fn();
vi.mock('@/api/agents', () => ({
  listAgents: () => listAgents(),
}));

import { useAgents, avatarFor, ensureAgentsOnBoot } from './agents';

describe('persona store (agents)', () => {
  beforeEach(() => {
    listAgents.mockReset();
    useAgents.setState({ byName: {}, loading: null, loaded: false });
  });

  it('缓存命中时不发第二次请求（并发共享同一 promise，成功后再调不重拉）', async () => {
    listAgents.mockResolvedValue({
      items: [{ name: 'specialist-sre', description: 'x', avatar: '📈' }],
      total: 1,
    });
    const a = useAgents.getState().ensureAgents();
    const b = useAgents.getState().ensureAgents();
    await Promise.all([a, b]);
    await useAgents.getState().ensureAgents();
    expect(listAgents).toHaveBeenCalledTimes(1);
    expect(useAgents.getState().byName['specialist-sre']?.avatar).toBe('📈');
  });

  it('拉取失败时静默：不抛出，avatarFor 回退 undefined', async () => {
    listAgents.mockRejectedValue(new Error('boom'));
    await expect(useAgents.getState().ensureAgents()).resolves.toBeUndefined();
    expect(avatarFor(useAgents.getState().byName, 'specialist-sre')).toBeUndefined();
  });

  it('avatarFor 对未知 agentId 返回 undefined，别名先归一', () => {
    const byName = {
      'specialist-sre': { name: 'specialist-sre', description: '', avatar: '📈' },
    };
    expect(avatarFor(byName, 'sre-agent')).toBe('📈'); // 别名归一
    expect(avatarFor(byName, 'mystery-agent')).toBeUndefined(); // 不编造
    expect(avatarFor(byName, undefined)).toBeUndefined();
  });

  it('avatarFor 把空白 avatar 视作缺失', () => {
    expect(avatarFor({ x: { name: 'x', description: '', avatar: '   ' } }, 'x')).toBeUndefined();
  });

  it('ensureAgentsOnBoot 触发一次预取', () => {
    listAgents.mockResolvedValue({ items: [], total: 0 });
    ensureAgentsOnBoot();
    expect(listAgents).toHaveBeenCalledTimes(1);
  });
});
