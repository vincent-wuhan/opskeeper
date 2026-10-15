import { render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';
import { MemoryRouter } from 'react-router-dom';
import { SessionList } from './SessionList';
import { useAgents } from '@/store/agents';
import type { ChatSession } from '@/api/chat';

// getLocale() falls back to autoDetectLocale() when localStorage is empty,
// which in jsdom resolves to en-US (node ICU reports a non-CN timezone).
// The persona assertions below are on the Chinese labels, so pin the locale
// explicitly instead of relying on detection — same precedent as
// MessageBubble.test.tsx / Agents.test.tsx.
localStorage.setItem('opskeeper-locale', 'zh-CN');

const sessions: ChatSession[] = [
  {
    id: 's1',
    user_id: 1,
    title: '磁盘打满的排查过程',
    agent_id: 'specialist-sre',
    updated_at: '2026-10-07T10:00:00Z',
  },
  { id: 's2', user_id: 1, title: '网络抖动', agent_id: 'default', updated_at: '2026-10-07T09:00:00Z' },
];

// NavLink needs router context; the active row is derived from the current
// location rather than an activeId prop, so the route is set here.
function renderList(initialEntry = '/dashboard') {
  return render(
    <MemoryRouter initialEntries={[initialEntry]}>
      <SessionList sessions={sessions} onDelete={() => {}} />
    </MemoryRouter>,
  );
}

describe('SessionList', () => {
  it('renders one avatar, persona name and title summary per session', () => {
    renderList();
    expect(screen.getAllByTestId('agent-avatar')).toHaveLength(2);
    // specialist-sre 本地化名
    expect(screen.getByText('SRE 专家')).toBeTruthy();
    expect(screen.getByText('磁盘打满的排查过程')).toBeTruthy();
  });

  it('exposes the full title for hover', () => {
    renderList();
    expect(screen.getByTitle('磁盘打满的排查过程')).toBeTruthy();
  });

  it('marks the active session', () => {
    renderList('/chat/s2');
    const active = screen.getByText('网络抖动').closest('[data-session-id]');
    expect(active?.getAttribute('data-active')).toBe('true');
    const inactive = screen.getByText('磁盘打满的排查过程').closest('[data-session-id]');
    expect(inactive?.getAttribute('data-active')).toBe('false');
  });

  it('passes the persona avatar through to AgentAvatar for a known agent_id', () => {
    useAgents.setState({
      byName: { 'specialist-sre': { name: 'specialist-sre', description: '', avatar: '📈' } },
    });
    renderList();
    // sessions[0].agent_id 为 'specialist-sre'，应渲染 emoji 头像而非角色图标。
    expect(screen.getAllByTestId('agent-avatar')[0].textContent).toContain('📈');
  });
});

afterEach(() => {
  useAgents.setState({ byName: {}, loaded: false, loading: null });
});
