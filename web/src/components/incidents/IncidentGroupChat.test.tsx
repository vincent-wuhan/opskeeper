import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { server } from '@/test/msw-server';
import type { IncidentEvent } from '@/api/alerts';
import type { ChatMessage, ChatSession } from '@/api/chat';
import { useAgents } from '@/store/agents';
import { IncidentGroupChat } from './IncidentGroupChat';

// ChatInput renders a <Link> in its "no model configured" dropdown branch;
// a <Link> outside a router throws, so every render goes through
// MemoryRouter (same convention as ChatInput.test.tsx).
function renderChat(incidentId = 1) {
  return render(
    <MemoryRouter>
      <IncidentGroupChat incidentId={incidentId} />
    </MemoryRouter>,
  );
}

// stub wires the four GETs the incident group chat issues on mount: the
// incident's alert events, the incident-related chat sessions, each
// session's message tail, and the loop timeline. Handlers are test-local —
// msw-server ships an empty server and each test registers its own.
function stub(init: {
  events?: IncidentEvent[];
  sessions?: ChatSession[];
  messages?: Record<string, ChatMessage[]>;
} = {}) {
  const events = init.events ?? [];
  const sessions = init.sessions ?? [];
  const messages = init.messages ?? {};
  server.use(
    http.get('/api/v1/alerts/incidents/1/events', () =>
      HttpResponse.json({ items: events, total: events.length }),
    ),
    http.get('/api/v1/chat/sessions', () =>
      HttpResponse.json({ items: sessions, total: sessions.length }),
    ),
    http.get('/api/v1/chat/sessions/:id/messages', ({ params }) =>
      HttpResponse.json({ items: messages[String(params.id)] ?? [], total: 0 }),
    ),
    http.get('/api/v1/loops/1/timeline', () => HttpResponse.json({ phases: [] })),
  );
}

describe('IncidentGroupChat', () => {
  // Pin the locale so persona labels are deterministic: getLocale() would
  // otherwise fall back to autoDetectLocale(), which keys off the host
  // timezone (Asia/Shanghai → zh-CN) and then the browser language (jsdom
  // default en-US). A non-CN TZ would render 'Incident investigator' and the
  // zh assertion below would fail — same pin as MessageBubble.test.tsx.
  beforeEach(() => {
    localStorage.setItem('opskeeper-locale', 'zh-CN');
  });

  it('renders system chips for events and message bubbles for chat', async () => {
    stub({
      events: [
        {
          id: 1,
          incident_id: 1,
          event_type: 'firing',
          title: '告警触发',
          actor_type: 'system',
          occurred_at: '2026-10-07T10:00:00Z',
          created_at: '2026-10-07T10:00:00Z',
        },
        {
          id: 2,
          incident_id: 1,
          event_type: 'repeat_suppressed',
          title: '进入调查',
          actor_type: 'system',
          occurred_at: '2026-10-07T10:00:10Z',
          created_at: '2026-10-07T10:00:10Z',
        },
      ],
      sessions: [{ id: 's1', user_id: 1, title: '诊断', agent_id: 'incident-investigator' }],
      messages: {
        s1: [
          {
            id: 'm1',
            role: 'assistant',
            content: '根因是磁盘写满',
            created_at: '2026-10-07T10:01:00Z',
          },
        ],
      },
    });

    renderChat();

    expect(await screen.findByText('告警触发')).toBeTruthy();
    expect(await screen.findByText('根因是磁盘写满')).toBeTruthy();
  });

  it('lists the distinct session personas plus you', async () => {
    stub({ sessions: [{ id: 's1', user_id: 1, title: 'a', agent_id: 'incident-investigator' }] });

    renderChat();

    // Wait for the feed to load before asserting the human is present: with
    // the pre-§4 member logic the empty pre-load feed rendered 「你」 as the
    // `['default']` fallback, so only the post-load frame discriminates.
    // The member row must render the *localized persona name* (personaLabel),
    // not the raw agent_id: '故障诊断' is the zh label for
    // 'incident-investigator', whose raw id leaking here was the defect.
    await screen.findByText('故障诊断');
    expect(screen.queryByText('incident-investigator')).not.toBeInTheDocument();
    // Synchronous on purpose — a findByText here would resolve against the
    // pre-load fallback frame and would not guard the fixed-human behavior.
    expect(screen.getByText('你')).toBeInTheDocument();
  });

  it('sends a follow-up, creating a related session when none exists', async () => {
    // Feed has no session at all → send() must create one before posting.
    stub();
    const sessionPosts: unknown[] = [];
    const messagePosts: { sessionId: string; content: unknown }[] = [];
    server.use(
      http.post('/api/v1/chat/sessions', async ({ request }) => {
        sessionPosts.push(await request.json());
        return HttpResponse.json({ id: 's-new', user_id: 1, title: '事件追问', agent_id: 'default' });
      }),
      http.post('/api/v1/chat/sessions/:id/messages', async ({ params, request }) => {
        const body = (await request.json()) as { content?: unknown };
        messagePosts.push({ sessionId: String(params.id), content: body.content });
        return HttpResponse.json({
          session_id: String(params.id),
          assistant_message: { id: 'a1', content: '收到', created_at: '2026-10-07T10:02:00Z' },
          tool_calls: [],
          usage: { prompt_tokens: 0, completion_tokens: 0, total_tokens: 0 },
          iterations: 1,
        });
      }),
    );

    renderChat();

    const textarea = await screen.findByRole('textbox', { name: /消息输入框|message input/i });
    fireEvent.change(textarea, { target: { value: '磁盘为什么满？' } });
    fireEvent.keyDown(textarea, { key: 'Enter' });

    await waitFor(() => expect(messagePosts).toHaveLength(1));
    expect(sessionPosts).toHaveLength(1);
    expect(messagePosts[0].sessionId).toBe('s-new');
    expect(messagePosts[0].content).toBe('磁盘为什么满？');
    await waitFor(() => expect(textarea).toHaveValue(''));
  });

  it('passes the persona avatar through to the members row', async () => {
    useAgents.setState({
      byName: {
        'incident-investigator': {
          name: 'incident-investigator',
          description: '',
          avatar: '🛰️',
        },
      },
    });
    stub({ sessions: [{ id: 's1', user_id: 1, title: 'a', agent_id: 'incident-investigator' }] });

    renderChat();

    // 成员行在 feed 加载后才渲染 agent 头像；等本地化 persona 名出现再断言，
    // 避免命中加载前的「你」回退帧。此时至少有一个 avatar 应渲染 emoji。
    await screen.findByText('故障诊断');
    const avatars = screen.getAllByTestId('agent-avatar');
    expect(avatars.some((el) => el.textContent?.includes('🛰️'))).toBe(true);
  });

  it('renders the role icon (no broken image / no invented avatar) when the store has no avatar', async () => {
    stub({ sessions: [{ id: 's1', user_id: 1, title: 'a', agent_id: 'incident-investigator' }] });

    renderChat();

    await screen.findByText('故障诊断');
    // 未 seed store → avatarFor 返回 undefined → 渲染图标，无 emoji、无 <img>
    const avatars = screen.getAllByTestId('agent-avatar');
    for (const a of avatars) expect(a.querySelector('img')).toBeNull();
  });

  it('wraps each chat message in a motion-safe entry animation (.anim-rise)', async () => {
    stub({
      sessions: [{ id: 's1', user_id: 1, title: 'a', agent_id: 'incident-investigator' }],
      messages: {
        s1: [
          {
            id: 'm1',
            role: 'assistant',
            content: '根因是磁盘写满',
            created_at: '2026-10-07T10:01:00Z',
          },
        ],
      },
    });
    renderChat();
    const text = await screen.findByText('根因是磁盘写满');
    const wrapper = text.closest('.anim-rise');
    expect(wrapper).not.toBeNull();
  });

  it('shows a typing indicator while the agent reply is in flight, then removes it', async () => {
    stub();
    let release!: () => void;
    const gate = new Promise<void>((r) => (release = r));
    server.use(
      http.post('/api/v1/chat/sessions', () =>
        HttpResponse.json({ id: 's-new', user_id: 1, title: '事件追问', agent_id: 'default' }),
      ),
      http.post('/api/v1/chat/sessions/:id/messages', async () => {
        await gate;
        return HttpResponse.json({
          session_id: 's-new',
          assistant_message: { id: 'a1', content: '收到', created_at: '2026-10-07T10:02:00Z' },
          tool_calls: [],
          usage: { prompt_tokens: 0, completion_tokens: 0, total_tokens: 0 },
          iterations: 1,
        });
      }),
    );

    renderChat();
    const textarea = await screen.findByRole('textbox', { name: /消息输入框|message input/i });
    fireEvent.change(textarea, { target: { value: '磁盘为什么满？' } });
    fireEvent.keyDown(textarea, { key: 'Enter' });

    // typing 指示出现，且呼吸点带 motion-safe 前缀（对齐 Approvals.test 的呼吸点断言）。
    const typing = await screen.findByText(/调查中|Investigating/);
    expect(typing.closest('[data-testid="typing-indicator"]')).not.toBeNull();
    const dot = screen
      .getByTestId('typing-indicator')
      .querySelector('.animate-pulse-dot, .motion-safe\\:animate-pulse-dot');
    expect(dot?.className, '呼吸点未做 motion-safe 门控').toContain('motion-safe:');

    release();
    await waitFor(() => expect(screen.queryByText(/调查中|Investigating/)).not.toBeInTheDocument());
  });

  it('pulses the crystallization milestone (recovered/success) badge, motion-safe', async () => {
    stub();
    server.use(
      http.get('/api/v1/loops/1/timeline', () =>
        HttpResponse.json({ phases: [{ phase: 'recovered', status: 'success' }] }),
      ),
    );
    renderChat();
    const pulse = await screen.findByTestId('crystal-milestone-pulse');
    expect(pulse.className).toContain('motion-safe:');
    expect(pulse.className).toContain('animate-pulse-dot');
  });

  it('does not render the crystallization pulse when no recovered/success phase exists', async () => {
    stub();
    renderChat();
    await screen.findByText('成员');
    expect(screen.queryByTestId('crystal-milestone-pulse')).not.toBeInTheDocument();
  });

  it('keeps message order and content identical regardless of animation classes', async () => {
    stub({
      events: [
        {
          id: 1,
          incident_id: 1,
          event_type: 'firing',
          title: '告警触发',
          actor_type: 'system',
          occurred_at: '2026-10-07T10:00:00Z',
          created_at: '2026-10-07T10:00:00Z',
        },
      ],
      sessions: [{ id: 's1', user_id: 1, title: 'a', agent_id: 'incident-investigator' }],
      messages: {
        s1: [
          { id: 'm1', role: 'assistant', content: '第一条', created_at: '2026-10-07T10:01:00Z' },
          { id: 'm2', role: 'assistant', content: '第二条', created_at: '2026-10-07T10:02:00Z' },
        ],
      },
    });
    renderChat();
    const first = await screen.findByText('第一条');
    const second = await screen.findByText('第二条');
    // DOM 顺序即消息顺序（未因动效改序）。
    expect(first.compareDocumentPosition(second) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it('the entry animation class is reduced-motion gated by index.css, not a bare animation', async () => {
    stub({
      sessions: [{ id: 's1', user_id: 1, title: 'a', agent_id: 'incident-investigator' }],
      messages: {
        s1: [{ id: 'm1', role: 'assistant', content: 'x', created_at: '2026-10-07T10:01:00Z' }],
      },
    });
    renderChat();
    const text = await screen.findByText('x');
    const wrapper = text.closest('.anim-rise');
    // .anim-rise 在 index.css 的 @media (prefers-reduced-motion: no-preference) 内定义：
    // 类名是自研类（非 Tailwind 工具类），不得写成 motion-safe:anim-rise。
    expect(wrapper?.className).not.toContain('motion-safe:anim-rise');
    expect(wrapper?.className).toContain('anim-rise');
  });
});

afterEach(() => {
  useAgents.setState({ byName: {}, loaded: false, loading: null });
});
