import { render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { ReportHostedView } from './ReportHostedView';
import type { ReportContent } from '@/api/reports';

// 最小合法 ContentJSON(镜像 biz/report/content.go 字段)。
const content = {
  version: '1',
  hero: [],
  narrative: { headline: '集群平稳' },
  resource: { available: false, cpu_avg: 0, cpu_peak: 0, mem_avg: 0, mem_peak: 0, disk_avg: 0, disk_peak: 0 },
  fleet: { total: 3, online: 2 },
  actions_summary: { mutating_total: 0, mutating_approved: 0, safe_total: 1 },
  assets: { new_agents: 0, new_skills: 0, new_repos: 0 },
  usage: { sessions: 1, prompt_tokens: 10, completion_tokens: 5 },
} satisfies ReportContent;

describe('ReportHostedView', () => {
  it('wraps ReportContentView in a 480px scroll bound by default', () => {
    const { container } = render(<ReportHostedView content={content} />);
    const scroller = container.firstElementChild as HTMLElement;
    expect(scroller.style.maxHeight).toBe('480px');
    expect(scroller.className).toContain('overflow-y-auto');
    expect(screen.getByText('集群平稳')).toBeInTheDocument();
  });
  // 回归防护:maxHeight="none" 必须**不套任何壳**。只断言 style.maxHeight
  // 是弱断言——若有人把该分支"简化"成始终渲染包裹 div,包裹层因为没有
  // maxHeight 同样返回 '',三条断言照样全绿,而独立报表页会凭空多出一层
  // DOM,改动 ReportDetail.tsx:115 外层 flex-1 overflow-y-auto 的滚动路径。
  // 断言壳自身的 className 不存在,与 ReportContentView 内部结构解耦。
  it('renders unbounded for the standalone report page', () => {
    const { container } = render(<ReportHostedView content={content} maxHeight="none" />);
    const root = container.firstElementChild as HTMLElement;
    expect(root.style.maxHeight).toBe('');
    expect(root.className).not.toContain('overflow-y-auto');
    expect(root.className).not.toContain('pr-1');
    expect(screen.getByText('集群平稳')).toBeInTheDocument();
  });
  it('honours an explicit maxHeight override', () => {
    const { container } = render(<ReportHostedView content={content} maxHeight={240} />);
    expect((container.firstElementChild as HTMLElement).style.maxHeight).toBe('240px');
  });
});
