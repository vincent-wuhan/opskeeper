import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { http, HttpResponse } from 'msw';

import { MessageBubble, type ConfigDraftResult } from './MessageBubble';
import type { ChatMessage } from '@/api/chat';
import { getApproval, approveApproval } from '@/api/approvals';
import { useApprovalBadge } from '@/store/approvalBadge';
import { useAgents } from '@/store/agents';
import { server } from '@/test/msw-server';
import { stubIntersectionObserver, triggerVisibleAt, unstubIntersectionObserver } from '@/test/mockIO';

vi.mock('@/api/approvals', () => ({
  getApproval: vi.fn(),
  approveApproval: vi.fn(),
  rejectApproval: vi.fn(),
}));

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
  useAgents.setState({ byName: {}, loaded: false, loading: null });
});

const supportedKinds = [
  'metric_threshold',
  'metric_raw',
  'metric_anomaly',
  'metric_forecast',
  'metric_burn_rate',
  'log_match',
  'log_volume',
  'trace_latency',
  'trace_error_rate',
];

function draftFor(kind: string): ConfigDraftResult {
  return {
    kind: 'config_draft',
    domain: 'alert_rule',
    action: 'create',
    summary: `Create ${kind} rule`,
    payload: {
      action: 'create',
      rule: {
        rule_key: `${kind}_natural_language`,
        kind,
        name: `${kind} from natural language`,
        severity: 'warning',
        spec: specFor(kind),
      },
    },
    preview: {
      fire_count: 2,
      samples: [{ summary: `${kind} sample` }],
    },
    warnings: [`${kind} preview warning`],
    scope: {
      type: 'host',
      label: '主机级',
      reason: '命中后会关联到具体设备。',
      change_hint: '如果要改成全局汇总，可以回复“改成全局”。',
    },
    confirmation_prompt:
      '当前告警范围：主机级。命中后会关联到具体设备。如果要改成全局汇总，可以回复“改成全局”。确认无误后可点击确认应用或回复“ok”。',
    rollback: 'Disable or edit the rule from Alerts.',
    apply_tool: 'apply_config_change',
    draft_hash: `sha256:${kind}`,
  };
}

function specFor(kind: string): Record<string, unknown> {
  switch (kind) {
    case 'metric_raw':
      return {
        expr: '(100 * max(redis_memory_used_bytes) / clamp_min(max(redis_memory_max_bytes), 1)) > 80',
      };
    case 'metric_anomaly':
      return { metric: 'cpu_pct', method: 'zscore', baseline_window: '1h', deviation: 3 };
    case 'metric_forecast':
      return { metric: 'disk_avail_bytes', predict_seconds: 21600, operator: '<=', threshold: 0 };
    case 'metric_burn_rate':
      return {
        sli: 'sum(rate(http_requests_total{code!~"5.."}[$window])) / sum(rate(http_requests_total[$window]))',
        slo: 99.9,
        burns: [{ window: '1h', multiplier: 14.4 }],
      };
    case 'log_match':
      return { stream_selector: '{opskeeper_source=~"journald:.+"}', line_filter: '(?i)error|panic' };
    case 'log_volume':
      return { stream_selector: '{opskeeper_source=~".+"}', ratio_op: '>=', ratio_threshold: 2 };
    case 'trace_latency':
      return { service: 'checkout', quantile: 'p95', threshold_ms: 500 };
    case 'trace_error_rate':
      return { service: 'checkout', operator: '>=', threshold_pct: 1 };
    default:
      return {};
  }
}

function toolCardMessage(draft: ConfigDraftResult): ChatMessage {
  return {
    id: `tool-card-${draft.summary}`,
    role: 'tool',
    kind: 'tool_card',
    tool_call: {
      id: `call-${draft.summary}`,
      name: 'draft_config_change',
      status: 'success',
      result: draft,
    },
  };
}

describe('MessageBubble config draft card', () => {
  beforeEach(() => {
    localStorage.setItem('opskeeper-locale', 'zh-CN');
  });

  it('compacts persisted config confirmation user payloads', () => {
    const longConfirmation = [
      '确认应用这个配置草案。',
      'domain: alert_rule',
      'action: create',
      'draft_hash: sha256:test',
      'apply_tool: apply_config_change',
      '请调用 apply_config_change，传 confirmed=true、domain=alert_rule、action=create、上方 draft_hash 和下方原始 payload，创建这条告警规则；不要改写 payload。',
      'payload:',
      '```json',
      JSON.stringify({
        action: 'create',
        rule: {
          rule_key: 'system_disk_pressure_v2',
          kind: 'metric_raw',
        },
      }, null, 2),
      '```',
    ].join('\n');

    render(<MessageBubble message={{ id: 'user-confirmation', role: 'user', content: longConfirmation }} />);

    expect(screen.getByText('确认创建这条告警规则')).toBeInTheDocument();
    expect(screen.queryByText(/draft_hash/)).not.toBeInTheDocument();
    expect(screen.queryByText(/system_disk_pressure_v2/)).not.toBeInTheDocument();
  });

  it('keeps ordinary user messages unchanged', () => {
    render(<MessageBubble message={{ id: 'user-normal', role: 'user', content: '创建一个 CPU 告警' }} />);

    expect(screen.getByText('创建一个 CPU 告警')).toBeInTheDocument();
  });

  it.each(supportedKinds)('renders and confirms %s drafts', async (kind) => {
    const user = userEvent.setup();
    const onConfirm = vi.fn();
    const draft = draftFor(kind);

    render(<MessageBubble message={toolCardMessage(draft)} onConfirmConfigDraft={onConfirm} />);

    expect(screen.getByText(`Create ${kind} rule`)).toBeInTheDocument();
    expect(screen.getByText('范围：主机级')).toBeInTheDocument();
    expect(screen.getByText(/当前告警范围：主机级/)).toBeInTheDocument();
    expect(screen.getByText(
      `action: create · rule_key: ${kind}_natural_language · kind: ${kind} · name: ${kind} from natural language · severity: warning`,
    )).toBeInTheDocument();
    expect(screen.getByText('Preview fire_count=2')).toBeInTheDocument();
    expect(screen.getByText(`${kind} preview warning`)).toBeInTheDocument();
    expect(screen.getByText('Disable or edit the rule from Alerts.')).toBeInTheDocument();

    await act(async () => {
      await user.click(screen.getByRole('button', { name: /确认应用|Apply/ }));
    });

    expect(onConfirm).toHaveBeenCalledTimes(1);
    expect(onConfirm).toHaveBeenCalledWith(draft);
    await waitFor(() => {
      expect(screen.getByRole('button', { name: /已确认|Confirmed/ })).toBeDisabled();
    });
  });

  it('cancels without calling confirm', async () => {
    const user = userEvent.setup();
    const onConfirm = vi.fn();

    render(<MessageBubble message={toolCardMessage(draftFor('metric_raw'))} onConfirmConfigDraft={onConfirm} />);
    await act(async () => {
      await user.click(screen.getByRole('button', { name: /取消|Cancel/ }));
    });

    expect(onConfirm).not.toHaveBeenCalled();
    await waitFor(() => {
      expect(screen.getByRole('button', { name: /已取消|Cancelled/ })).toBeDisabled();
    });
  });

  it('allows retry when confirm fails', async () => {
    const user = userEvent.setup();
    const onConfirm = vi.fn().mockResolvedValue(false);

    render(<MessageBubble message={toolCardMessage(draftFor('metric_raw'))} onConfirmConfigDraft={onConfirm} />);
    await act(async () => {
      await user.click(screen.getByRole('button', { name: /确认应用|Apply/ }));
    });

    expect(onConfirm).toHaveBeenCalledTimes(1);
    await waitFor(() => {
      expect(screen.getByRole('button', { name: /确认应用|Apply/ })).toBeEnabled();
    });
    expect(screen.queryByRole('button', { name: /已确认|Confirmed/ })).not.toBeInTheDocument();
  });

  it('does not render a config draft card for unsupported config domains', () => {
    const draft = {
      ...draftFor('metric_raw'),
      domain: 'notification_channel',
      summary: 'Create notification channel',
    } as ConfigDraftResult;

    render(<MessageBubble message={toolCardMessage(draft)} onConfirmConfigDraft={vi.fn()} />);

    expect(screen.queryByText('Create notification channel')).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /确认应用|Apply/ })).not.toBeInTheDocument();
  });

  it('renders a persisted tool result string as a draft card', () => {
    const draft = draftFor('metric_raw');
    const message: ChatMessage = {
      id: 'persisted-tool-result',
      role: 'tool',
      tool_name: 'draft_config_change',
      content: JSON.stringify(draft),
    };

    render(<MessageBubble message={message} onConfirmConfigDraft={vi.fn()} />);

    expect(screen.getByText('Create metric_raw rule')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: /确认应用|Apply/ })).toBeInTheDocument();
  });
});

function approvalMessage(approvalID: string): ChatMessage {
  return {
    id: `approval-${approvalID}`,
    role: 'tool',
    kind: 'tool_card',
    tool_call: {
      id: `call-${approvalID}`,
      name: 'cloud_bash',
      status: 'success',
      result: { status: 'pending_approval', approval_id: approvalID, kind: 'cloud_bash' },
      arguments: { command: 'echo opskeeper-dualsign-OK' },
    },
  };
}

describe('MessageBubble agent bubble form', () => {
  beforeEach(() => {
    localStorage.setItem('opskeeper-locale', 'zh-CN');
  });

  const assistantMessage: ChatMessage = {
    id: 'assistant-persona',
    role: 'assistant',
    content: '磁盘使用率已达 92%，建议清理日志。',
    created_at: '2026-10-07T09:41:00Z',
    pending: false,
  };

  it('renders the agent avatar and persona head row when agentId is set', () => {
    render(<MessageBubble message={assistantMessage} agentId="specialist-disk" />);

    expect(screen.getByTestId('agent-avatar')).toBeInTheDocument();
    expect(screen.getByText('磁盘专家')).toBeInTheDocument();
    expect(screen.getByText('09:41')).toBeInTheDocument();
    expect(screen.getByText(/磁盘使用率已达/)).toBeInTheDocument();
  });

  it('passes the persona avatar through from the store to the head-row avatar', () => {
    // 聊天头行（MessageBubble）是 spec「各展示位一致渲染」列出的四处之一：
    // store 中同一 persona 的 avatar 必须原样透传到 AgentAvatar。
    useAgents.setState({
      byName: {
        'specialist-disk': { name: 'specialist-disk', description: '', avatar: '💾' },
      },
    });
    render(<MessageBubble message={assistantMessage} agentId="specialist-disk" />);

    expect(screen.getByTestId('agent-avatar').textContent).toContain('💾');
  });

  it('keeps the bubble-agent semantic class and drops the head row without agentId', () => {
    const { container } = render(<MessageBubble message={assistantMessage} />);

    expect(container.querySelector('.bubble-agent')).not.toBeNull();
    expect(screen.queryByTestId('agent-avatar')).not.toBeInTheDocument();
    expect(screen.queryByText('磁盘专家')).not.toBeInTheDocument();
    expect(screen.getByText(/磁盘使用率已达/)).toBeInTheDocument();
  });

  it('renders the user bubble with the bubble-user semantic class', () => {
    const { container } = render(
      <MessageBubble message={{ id: 'user-bubble-user', role: 'user', content: '看一下磁盘' }} />,
    );

    expect(container.querySelector('.bubble-user')).not.toBeNull();
  });
});

describe('MessageBubble inline approval card', () => {
  beforeEach(() => {
    localStorage.setItem('opskeeper-locale', 'zh-CN');
    vi.mocked(getApproval).mockResolvedValue({
      id: 'ap-dualsign',
      kind: 'cloud_bash',
      title: 'echo opskeeper-dualsign-OK',
      summary: '',
      payload: JSON.stringify({ command: 'echo opskeeper-dualsign-OK' }),
      source: 'chat',
      status: 'pending',
      proposed_by: 1,
      created_at: new Date().toISOString(),
    });
  });

  it('keeps the card honest on a first dual-sign signature: waiting, never 已执行', async () => {
    // Destructive commands need two signatures from two users; the first
    // approve returns HTTP 202 with status still pending and the command
    // NOT run. The card must say so instead of claiming 已执行.
    vi.mocked(approveApproval).mockResolvedValue({
      id: 'ap-dualsign',
      kind: 'cloud_bash',
      title: 'echo opskeeper-dualsign-OK',
      summary: '',
      payload: '{}',
      source: 'chat',
      status: 'pending',
      signers: JSON.stringify([{ user_id: 1, role: 'admin', at: new Date().toISOString() }]),
      proposed_by: 1,
      created_at: new Date().toISOString(),
    });

    render(<MessageBubble message={approvalMessage('ap-dualsign')} />);
    const user = userEvent.setup();
    await screen.findByRole('button', { name: /批准并执行/ });
    await user.click(screen.getByRole('button', { name: /批准并执行/ }));

    await waitFor(() => expect(screen.getByText(/已记录你的签名/)).toBeInTheDocument());
    expect(screen.getByText(/第二位批准人/)).toBeInTheDocument();
    expect(screen.queryByText('已执行')).not.toBeInTheDocument();
  });

  it('shows 已执行 with the result once the row actually executed', async () => {
    vi.mocked(approveApproval).mockResolvedValue({
      id: 'ap-dualsign',
      kind: 'cloud_bash',
      title: 'echo opskeeper-dualsign-OK',
      summary: '',
      payload: '{}',
      source: 'chat',
      status: 'executed',
      result: JSON.stringify({ stdout: 'opskeeper-dualsign-OK\n', exit_code: 0 }),
      proposed_by: 1,
      created_at: new Date().toISOString(),
    });

    render(<MessageBubble message={approvalMessage('ap-dualsign')} />);
    const user = userEvent.setup();
    await screen.findByRole('button', { name: /批准并执行/ });
    await user.click(screen.getByRole('button', { name: /批准并执行/ }));

    await waitFor(() => expect(screen.getByText('已执行')).toBeInTheDocument());
    expect(screen.getAllByText(/opskeeper-dualsign-OK/).length).toBeGreaterThan(0);
  });

  it('refreshes the global pending-approval badge after an approve', async () => {
    // 3.4.3: after an approve the sidebar 审批 red dot must reconcile with the
    // now-decided row. The store's refresh() bails early without a token, so
    // there is nothing observable in the DOM — assert the call with a spy on
    // the very same state object the card reaches through getState().
    vi.mocked(approveApproval).mockResolvedValue({
      id: 'ap-dualsign',
      kind: 'cloud_bash',
      title: 'echo opskeeper-dualsign-OK',
      summary: '',
      payload: '{}',
      source: 'chat',
      status: 'pending',
      signers: JSON.stringify([{ user_id: 1, role: 'admin', at: new Date().toISOString() }]),
      proposed_by: 1,
      created_at: new Date().toISOString(),
    });

    const spy = vi.spyOn(useApprovalBadge.getState(), 'refresh').mockResolvedValue();

    try {
      render(<MessageBubble message={approvalMessage('ap-dualsign')} />);
      const user = userEvent.setup();
      await screen.findByRole('button', { name: /批准并执行/ });
      await user.click(screen.getByRole('button', { name: /批准并执行/ }));

      await waitFor(() => expect(spy).toHaveBeenCalled());
    } finally {
      spy.mockRestore();
    }
  });

  it('surfaces the blast radius / risk class / target on the approval card', async () => {
    // 3.4.3: the operator is authorising a change; the radius, risk class and
    // target are what say how much of the estate it touches. The backend row
    // already carries them (model.go), so the card renders them on mount.
    vi.mocked(getApproval).mockResolvedValue({
      id: 'ap-dualsign',
      kind: 'cloud_bash',
      title: 'echo opskeeper-dualsign-OK',
      summary: '',
      payload: JSON.stringify({ command: 'echo opskeeper-dualsign-OK' }),
      source: 'chat',
      status: 'pending',
      proposed_by: 1,
      created_at: new Date().toISOString(),
      blast_radius: 'node-12 nginx 5s',
      risk_class: 'destructive',
      target: 'node-12',
    });

    render(<MessageBubble message={approvalMessage('ap-dualsign')} />);

    expect(await screen.findByText(/影响面/)).toBeInTheDocument();
    expect(screen.getByText(/风险等级/)).toBeInTheDocument();
    // The blast-radius chip's text is "影响面: node-12 nginx 5s", so a broad
    // /node-12/ match stayed green even with the target span deleted. The
    // exact-match text node "node-12" is produced only by the target span
    // (meta.target), so this genuinely constrains the target rendering.
    expect(screen.getByText('node-12')).toBeInTheDocument();
  });

  it('waiting 态的已签人数取自共享 parseSigners', async () => {
    vi.mocked(approveApproval).mockResolvedValue({
      id: 'ap-dualsign',
      kind: 'cloud_bash',
      title: 'echo opskeeper-dualsign-OK',
      summary: '',
      payload: '{}',
      source: 'chat',
      status: 'pending',
      signers: JSON.stringify([
        { user_id: 1, role: 'admin', at: new Date().toISOString() },
        { user_id: 2, role: 'admin', at: new Date().toISOString() },
      ]),
      proposed_by: 1,
      created_at: new Date().toISOString(),
    });

    render(<MessageBubble message={approvalMessage('ap-dualsign')} />);
    const user = userEvent.setup();
    await screen.findByRole('button', { name: /批准并执行/ });
    await user.click(screen.getByRole('button', { name: /批准并执行/ }));

    await waitFor(() => expect(screen.getByText(/2 人已签/)).toBeInTheDocument());
    expect(screen.getByText(/第二位批准人/)).toBeInTheDocument();
  });
});

describe('MessageBubble deliverable link rendering', () => {
  beforeEach(() => {
    localStorage.setItem('opskeeper-locale', 'zh-CN');
  });

  // 报表那条用例 stub 了 IO 与 msw handler,两者都是全局的,不能漏给下一个 describe。
  afterEach(() => {
    unstubIntersectionObserver();
    server.resetHandlers();
  });

  it('renders a hosted-page markdown link as a DeliverableCard, not an anchor', () => {
    const { container } = render(
      <MessageBubble
        message={{
          id: 'assistant-deliverable',
          role: 'assistant',
          content: '报告已生成：[查看托管页](/pages/a3f9c2d81b7e4056c9d0e1f2)',
          pending: false,
        }}
      />,
    );

    // 类型 chip + 动作在,缩略未进视口不 fetch(jsdom 无 IO → 恒不可见)
    expect(screen.getByText('托管页')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: '新窗口打开' })).toBeInTheDocument();
    expect(container.querySelector('a[href="/pages/a3f9c2d81b7e4056c9d0e1f2"]')).toBeNull();
  });

  it('keeps a non-deliverable markdown link as a plain anchor with no target attr', () => {
    const { container } = render(
      <MessageBubble
        message={{
          id: 'assistant-plain-link',
          role: 'assistant',
          content: '请先到[设置页](/settings)调整阈值。',
          pending: false,
        }}
      />,
    );

    const link = container.querySelector('a[href="/settings"]');
    expect(link).not.toBeNull();
    // Pre-refactor fallback is verbatim: same markup, children preserved,
    // and no target attribute was ever added.
    expect(link?.textContent).toBe('设置页');
    expect(link?.hasAttribute('target')).toBe(false);
    // No deliverable card is produced for a plain link. 对齐当前按钮名
    // 「新窗口打开」:沿用旧名「打开」会让这条断言无条件通过(按钮已改名)。
    expect(screen.queryByRole('button', { name: '新窗口打开' })).not.toBeInTheDocument();
    // 反向锁定:带交付物卡的段落被换成 <div> 之后,没有卡的段落必须仍是真正的 <p>,
    // 否则 markdown 排版基础(以及 .md-body p 的间距规则)对普通文本就失效了。
    expect(container.querySelector('p')).not.toBeNull();
  });

  it('keeps a third-party absolute link as a plain anchor and renders no iframe', () => {
    // 上面那条锁的是「站内非交付物路径」。这条锁的是形状相同但指向外部的链接:
    // `https://evil.example.com/pages/<hex24>` 的 path 与托管页一模一样,若换卡判定
    // 只看路径不看 origin(或正则丢掉 `^`),外部页面就会被当成交付物 —— 消息里出现
    // 一张指向 evil.example.com 的卡片,进而用 sandbox iframe 去取第三方 HTML。
    // 第三方内容绝不进入本应用的 iframe/卡片管线,只留普通外链。
    const HEX24 = 'a3f9c2d81b7e4056c9d0e1f2';
    const href = `https://evil.example.com/pages/${HEX24}`;
    const { container } = render(
      <MessageBubble
        message={{
          id: 'assistant-third-party-deliverable',
          role: 'assistant',
          content: `参考这份材料：[外部页面](${href})`,
          pending: false,
        }}
      />,
    );

    const link = container.querySelector(`a[href="${href}"]`);
    expect(link).not.toBeNull();
    expect(link?.textContent).toBe('外部页面');
    expect(link?.hasAttribute('target')).toBe(false); // 沿用普通外链,不加 target
    // 无卡片、无 iframe:这是「第三方链接不渲染 iframe」这条 spec 的 DOM 级断言。
    expect(container.querySelector('[data-testid="deliverable-card"]')).toBeNull();
    expect(container.querySelector('iframe')).toBeNull();
  });

  it('renders a report card preview without nesting block content inside a <p>', async () => {
    // 报表预览走 ReportHostedView/ReportContentView,内部是成片的 <div>。卡片整体
    // 被 ReactMarkdown 注入 <p>,若段落还是 <p>,真实 DOM 就是 <p><span><div> —— 非法
    // 嵌套,dev 每展开一次报表卡就报一次 validateDOMNesting。MessageBubble 的 p 覆盖
    // 把带卡的段落换成 <div class="md-p-card">,这里断言换掉之后 <p> 下面没有块级内容。
    const UUID = '7b2c1a9e-3f4d-4c5b-8e9f-0a1b2c3d4e5f';
    server.use(http.get('/api/v1/reports/:id', () => HttpResponse.json({
      id: UUID, title: '10月8日日报', kind: 'daily', status: 'ready', summary: '',
      period_start: '', period_end: '', generated_at: '2026-10-08T09:00:00Z',
      created_at: '2026-10-08T09:00:00Z', content_md: '', timezone: 'Asia/Shanghai',
      content: {
        version: '1', hero: [], narrative: { headline: '集群平稳' },
        resource: { available: false, cpu_avg: 0, cpu_peak: 0, mem_avg: 0, mem_peak: 0, disk_avg: 0, disk_peak: 0 },
        fleet: { total: 3, online: 2 },
        actions_summary: { mutating_total: 0, mutating_approved: 0, safe_total: 1 },
        assets: { new_agents: 0, new_skills: 0, new_repos: 0 },
        usage: { sessions: 1, prompt_tokens: 10, completion_tokens: 5 },
      },
    })));
    stubIntersectionObserver();
    const { container } = render(
      <MessageBubble
        message={{
          id: 'assistant-report-preview',
          role: 'assistant',
          content: `报告已生成：[查看报表](/reports/${UUID})`,
          pending: false,
        }}
      />,
    );
    triggerVisibleAt(0);
    await waitFor(() => expect(screen.getByText('日报')).toBeInTheDocument());
    fireEvent.click(screen.getByTestId('deliverable-card-header'));
    await waitFor(() => expect(screen.getByTestId('deliverable-preview')).toBeInTheDocument());
    expect(screen.getByText('集群平稳')).toBeInTheDocument();
    expect(container.querySelector('p div')).toBeNull(); // <p> 之下不得再有块级元素
    expect(container.querySelector('.md-p-card')).not.toBeNull();
  });

  it('renders a bold-wrapped report card without nesting block content inside a <p>', async () => {
    // 回归锁定:卡链接被行内包裹元素(这里是 **[…]** → <strong>)套住时,p 覆盖
    // 的判定必须递归到后代才找得到那个 <a>。只看直接子节点会漏,产出
    // <p><strong><span card><span preview><div>,正是要禁掉的那棵树。
    // `**[链接](…)**` 是模型输出的常规形态,不是构造出来的边角形状。
    const UUID = '9c4d2b0f-5e6a-4c7d-9f10-2b3c4d5e6f70';
    server.use(http.get('/api/v1/reports/:id', () => HttpResponse.json({
      id: UUID, title: '10月8日日报', kind: 'daily', status: 'ready', summary: '',
      period_start: '', period_end: '', generated_at: '2026-10-08T09:00:00Z',
      created_at: '2026-10-08T09:00:00Z', content_md: '', timezone: 'Asia/Shanghai',
      content: {
        version: '1', hero: [], narrative: { headline: '集群平稳' },
        resource: { available: false, cpu_avg: 0, cpu_peak: 0, mem_avg: 0, mem_peak: 0, disk_avg: 0, disk_peak: 0 },
        fleet: { total: 3, online: 2 },
        actions_summary: { mutating_total: 0, mutating_approved: 0, safe_total: 1 },
        assets: { new_agents: 0, new_skills: 0, new_repos: 0 },
        usage: { sessions: 1, prompt_tokens: 10, completion_tokens: 5 },
      },
    })));
    stubIntersectionObserver();
    const { container } = render(
      <MessageBubble
        message={{
          id: 'assistant-bold-report-preview',
          role: 'assistant',
          content: `报告：**[查看报表](/reports/${UUID})**`,
          pending: false,
        }}
      />,
    );
    triggerVisibleAt(0);
    await waitFor(() => expect(screen.getByText('日报')).toBeInTheDocument());
    fireEvent.click(screen.getByTestId('deliverable-card-header'));
    await waitFor(() => expect(screen.getByTestId('deliverable-preview')).toBeInTheDocument());
    expect(screen.getByText('集群平稳')).toBeInTheDocument();
    expect(container.querySelector('.md-p-card')).not.toBeNull();
    expect(container.querySelector('p div')).toBeNull(); // <p> 之下不得再有块级元素
  });
});

describe('MessageBubble deliverable in-place preview', () => {
  const HEX = 'a3f9c2d81b7e4056c9d0e1f2';

  beforeEach(() => {
    localStorage.setItem('opskeeper-locale', 'zh-CN');
    stubIntersectionObserver();
    server.use(http.get('/api/pages/:id', () =>
      HttpResponse.text('<!doctype html><html><head><title>预览页</title></head><body><h1>ok</h1></body></html>')));
  });
  afterEach(() => {
    unstubIntersectionObserver();
    server.resetHandlers();
  });

  it('expands the preview inside the bubble without navigating away', async () => {
    const open = vi.spyOn(window, 'open').mockImplementation(() => null);
    const { container } = render(
      <MessageBubble
        message={{
          id: 'assistant-preview',
          role: 'assistant',
          content: `报告已生成：[查看托管页](/pages/${HEX})`,
          pending: false,
        }}
      />,
    );
    triggerVisibleAt(0);
    await waitFor(() => expect(document.querySelector('iframe')).not.toBeNull());
    fireEvent.click(screen.getByTestId('deliverable-card-header'));
    expect(screen.getByTestId('deliverable-preview')).toBeInTheDocument();
    expect(container.querySelector('.bubble-agent')).not.toBeNull(); // 消息流仍在原地
    expect(open).not.toHaveBeenCalled();
    open.mockRestore();
  });
});

describe('MessageBubble deliverable thumbnails: lazy mount + cap', () => {
  // 5 个互不相同的 hex24 产物 id。
  const HEX_IDS = [
    'a3f9c2d81b7e4056c9d0e1f2',
    'b3f9c2d81b7e4056c9d0e1f2',
    'c3f9c2d81b7e4056c9d0e1f2',
    'd3f9c2d81b7e4056c9d0e1f2',
    'e3f9c2d81b7e4056c9d0e1f2',
  ];
  const deliverableMessage = {
    id: 'assistant-many-deliverables',
    role: 'assistant' as const,
    content: HEX_IDS.map((id, i) => `[页面${i}](/pages/${id})`).join('\n'),
    pending: false,
  };

  beforeEach(() => {
    localStorage.setItem('opskeeper-locale', 'zh-CN');
    stubIntersectionObserver();
    server.use(http.get('/api/pages/:id', ({ params }) =>
      HttpResponse.text(`<!doctype html><html><head><title>页面 ${String(params.id)}</title></head><body></body></html>`)));
  });
  afterEach(unstubIntersectionObserver);

  it('renders thumb panes for the first 3 cards and compact cards beyond', () => {
    const { container } = render(<MessageBubble message={deliverableMessage} />);
    expect(container.querySelectorAll('[data-testid="deliverable-card"]')).toHaveLength(5);
    expect(screen.getAllByTestId('deliverable-thumb')).toHaveLength(3); // 前 3 张(含 idle 占位)
    expect(screen.getAllByRole('button', { name: '新窗口打开' })).toHaveLength(5); // 动作全部可用
  });

  it('loads thumbnail content only when a card enters the viewport', async () => {
    render(<MessageBubble message={deliverableMessage} />);
    triggerVisibleAt(0); // 仅第一张卡可见
    await waitFor(() => expect(document.querySelectorAll('iframe')).toHaveLength(1));
    expect(document.querySelectorAll('iframe')).toHaveLength(1); // 其余卡零 fetch 零 iframe
  });

  it('compact cards beyond the cap expand their preview on demand', async () => {
    render(<MessageBubble message={deliverableMessage} />);
    triggerVisibleAt(3); // 第 4 张(超限紧凑卡)进入视口
    fireEvent.click(screen.getAllByTestId('deliverable-card-header')[3]);
    await waitFor(() =>
      expect(screen.getAllByTestId('deliverable-preview')[0].querySelector('iframe')).not.toBeNull());
  });
});
