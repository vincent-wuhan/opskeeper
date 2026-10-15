// ChatThread 布局接线测试 —— 验证会话上下文面板已挂进聊天页右列,
// 且其内容确实由本会话消息反推(deriveSessionContext 接线成立)。
//
// 消息渲染路径与输入区被 stub 掉,焦点只在「面板是否挂载 + 是否拿到
// 反推结果」;真实气泡/输入由各自测试覆盖。api 与 me store 全部 mock,
// 避免 onUnhandledRequest:'error' 下挂载即发请求。
import { cleanup, render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter, Route, Routes } from 'react-router-dom';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import ChatThreadPage from './ChatThread';
import { useContextPanel } from '@/store/contextPanel';

vi.mock('@/components/MessageBubble', () => ({
  MessageBubble: () => <div data-testid="bubble" />,
}));
vi.mock('@/components/ChatInput', () => ({
  ChatInput: () => <div data-testid="chat-input-stub" />,
}));
vi.mock('@/store/me', () => ({
  usePermissions: () => ({ isAdmin: false, canMutate: true, isViewer: false, role: 'user' }),
}));
vi.mock('@/api/chat', () => ({
  getMessages: vi.fn(async () => ({ items: [], total: 0 })),
  listModels: vi.fn(async () => ({ providers: [], default: null })),
  stopSession: vi.fn(async () => undefined),
  streamMessage: vi.fn(async () => undefined),
}));
vi.mock('@/api/approvals', () => ({
  listApprovals: vi.fn(async () => ({ items: [] })),
}));

import { getMessages } from '@/api/chat';

function renderAt(entry: string) {
  return render(
    <MemoryRouter initialEntries={[entry]}>
      <Routes>
        <Route path="/chat/:sessionId" element={<ChatThreadPage />} />
      </Routes>
    </MemoryRouter>,
  );
}

beforeEach(() => {
  useContextPanel.setState({ expanded: false });
  localStorage.clear();
  vi.mocked(getMessages).mockResolvedValue({ items: [], total: 0 });
});
afterEach(() => {
  cleanup();
  useContextPanel.setState({ expanded: false });
  localStorage.clear();
});

describe('ChatThreadPage 上下文面板接线', () => {
  it('挂载会话上下文面板,默认折叠时不渲染分节', async () => {
    renderAt('/chat/s1');
    expect(await screen.findByTestId('context-panel')).toBeInTheDocument();
    expect(screen.queryByTestId('context-mentions')).toBeNull();
    expect(screen.queryByTestId('context-knowledge')).toBeNull();
  });

  it('展开后展示由本会话消息反推的 @提及对象', async () => {
    vi.mocked(getMessages).mockResolvedValue({
      items: [
        { id: 'm1', role: 'user', content: '@device:7(核心网关) 怎么了' },
      ],
      total: 1,
    });
    useContextPanel.setState({ expanded: true });
    renderAt('/chat/s1');
    const panel = await screen.findByTestId('context-panel');
    await waitFor(() => {
      const link = panel.querySelector('a[href="/devices/7"]');
      expect(link).not.toBeNull();
      expect(link).toHaveTextContent('核心网关');
    });
  });
});
