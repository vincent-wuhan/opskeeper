import { cleanup, fireEvent, render, screen, within } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { ContextPanel } from './ContextPanel';
import { useContextPanel } from '@/store/contextPanel';
import type { SessionContext } from '@/lib/sessionContext';

function renderPanel(context: SessionContext) {
  return render(
    <MemoryRouter>
      <ContextPanel context={context} />
    </MemoryRouter>,
  );
}

const sample: SessionContext = {
  mentions: [{ type: 'device', id: '7', label: '核心网关', sourceIndex: 0, messageId: 'm1' }],
  knowledgeRefs: [{ name: 'query_knowledge', status: 'success', durationMs: 120, sourceIndex: 1, messageId: 'a1' }],
};

beforeEach(() => {
  useContextPanel.setState({ expanded: true });
});
afterEach(() => {
  cleanup();
  useContextPanel.setState({ expanded: false });
  localStorage.clear();
});

describe('ContextPanel', () => {
  it('默认折叠:展开记忆为 false 时不渲染分节,仅留展开开关', () => {
    useContextPanel.setState({ expanded: false });
    renderPanel(sample);
    expect(screen.getByRole('button', { name: /展开上下文面板|expand context panel/i })).toBeInTheDocument();
    expect(screen.queryByTestId('context-mentions')).toBeNull();
  });

  it('展开后渲染 @提及对象分节,条目为跳转链接并携带来源', () => {
    renderPanel(sample);
    const section = screen.getByTestId('context-mentions');
    const link = within(section).getByRole('link');
    expect(link).toHaveTextContent('核心网关');
    expect(link).toHaveAttribute('href', '/devices/7');
    expect(link).toHaveAttribute('data-message-id', 'm1');
  });

  it('渲染知识引用分节,列出工具名与状态', () => {
    renderPanel(sample);
    const section = screen.getByTestId('context-knowledge');
    expect(within(section).getByText('query_knowledge')).toBeInTheDocument();
    expect(within(section).getByText('success')).toBeInTheDocument();
  });

  it('只读:分节内不存在任何编辑/删除按钮', () => {
    renderPanel(sample);
    expect(within(screen.getByTestId('context-mentions')).queryAllByRole('button')).toHaveLength(0);
    expect(within(screen.getByTestId('context-knowledge')).queryAllByRole('button')).toHaveLength(0);
  });

  it('无任何结构化上下文时展示引导空态', () => {
    renderPanel({ mentions: [], knowledgeRefs: [] });
    expect(screen.getByText(/本会话还没有可反推的上下文|no derivable context yet/i)).toBeInTheDocument();
  });

  it('切换折叠态写入持久化记忆', () => {
    useContextPanel.setState({ expanded: false });
    renderPanel(sample);
    fireEvent.click(screen.getByRole('button', { name: /展开上下文面板|expand context panel/i }));
    expect(useContextPanel.getState().expanded).toBe(true);
    expect(localStorage.getItem('opskeeper.context-panel') ?? '').toContain('"expanded":true');
  });

  // phase4-ui-deepening:圆角收敛到 rk 阶梯 —— 折叠按钮与行项不得再用裸 rounded/rounded-md。
  it('行项与折叠按钮使用 rk 圆角 token,无裸 rounded 残留', () => {
    renderPanel(sample);
    const link = within(screen.getByTestId('context-mentions')).getByRole('link');
    expect(link.className).toContain('rounded-rk-sm');
    expect(link.className).not.toContain('rounded-md');

    const row = screen.getByTestId('context-knowledge').querySelector('div');
    expect(row?.className).toContain('rounded-rk-sm');
    expect(row?.className).not.toContain('rounded-md');

    const toggle = screen.getByRole('button', { name: /收起上下文面板|collapse context panel/i });
    expect(toggle.className).toContain('rounded-rk-sm');
    expect(toggle.className).not.toMatch(/(^|\s)rounded(\s|$)/); // 无裸 rounded
  });
});
