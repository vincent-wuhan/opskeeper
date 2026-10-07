// 节点 Agent 页面测试。
//
// 选这几条断言的理由是它们各自对应一个「页面如果写错了、运维会误判」
// 的具体后果，而不是「覆盖了多少行」：
//
//   1. 流上的帧真的渲染出来了 —— 挂流在前、发消息在后是契约，页面写反了
//      的话每条消息都会被管理面拒掉，而症状是"什么都没发生"。
//   2. assistant_end 覆盖累积的 delta —— 两个字段都发是上游的设计，用
//      delta 拼的结果一旦丢帧就是截断的，assistant_end 是权威值。
//   3. 丢帧计数出现在被它限定的那段记录旁边 —— 背压丢帧和"agent 不说话了"
//      在界面上长得一模一样，没有这个数字运维只能猜。
//   4. 429 的文案是"去关一个会话"而不是"稍后重试" —— 这是本页唯一一个
//      重试无意义的状态，写错了会把运维引到唯一无效的那个动作上。
//   5. 审批卡片回传 digest —— 节点会重算并拒收不匹配的答复，页面若不带
//      digest，批准会被静默丢弃，界面上表现为"点了没反应"。
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { beforeEach, describe, expect, it } from 'vitest';

import NodeAgentsPage from './NodeAgents';
import { server } from '@/test/msw-server';

const EDGE = { id: 7, name: 'edge-prod-1', status: 'online' as const, roles: [], access_key_id: '', last_seen_at: null };

/** sse builds an SSE response that stays open.
 *
 *  It deliberately does not close. The page's send button is enabled by
 *  the stream being *attached*, not by it having ended, so a closed stream
 *  would leave the composer permanently disabled and every send-path
 *  assertion would be untestable for a reason that has nothing to do with
 *  the page. The pending read is released by the page's AbortController on
 *  unmount. */
function sse(frames: string[]): Response {
  const enc = new TextEncoder();
  const body = new ReadableStream<Uint8Array>({
    start(controller) {
      // The manager's liveness comment. It carries no data line, so the
      // client must drop it rather than render an empty bubble.
      controller.enqueue(enc.encode(': ok\n\n'));
      for (const f of frames) controller.enqueue(enc.encode(f));
    },
  });
  return new HttpResponse(body, { headers: { 'Content-Type': 'text/event-stream' } });
}

function frame(type: string, payload: Record<string, unknown>): string {
  return `event: ${type}\ndata: ${JSON.stringify(payload)}\n\n`;
}

function baseHandlers(sessions: unknown[] = []) {
  return [
    http.get('/api/v1/edges', () => HttpResponse.json({ items: [EDGE], total: 1 })),
    http.get('/api/v1/node-agents/sessions', () => HttpResponse.json({ sessions })),
    http.get('/api/v1/node-agents/7/state', () =>
      HttpResponse.json({ running: false, model: 'qwen3-max', provider: 'dashscope' })
    ),
    http.get('/api/v1/node-agents/7/health', () =>
      HttpResponse.json({ running: true, degraded: false, restarts: 0, version: '0.3.0' })
    ),
  ];
}

/** The successful open. Kept out of baseHandlers so a test that wants the
 *  open to FAIL registers its own answer without two handlers competing for
 *  one path — which handler wins there is a property of registration
 *  order, not of what the test is trying to prove. */
const openSucceeds = () =>
  http.post('/api/v1/node-agents/sessions', () =>
    HttpResponse.json({ session_id: 'sess-1', edge_id: 7 })
  );

async function openConversation() {
  server.use(openSucceeds());
  const user = userEvent.setup();
  render(<NodeAgentsPage />);
  await user.click(await screen.findByRole('button', { name: /new session/i }));
  return user;
}

describe('NodeAgentsPage', () => {
  beforeEach(() => {
    // en-US so the assertions below can quote the page's own English copy.
    // The page is bilingual; testing one locale is enough to pin the
    // behaviour, and the strings that matter here are the ones an operator
    // reads while something is refusing.
    localStorage.setItem('opskeeper-locale', 'en-US');
  });

  it('renders the assistant text the node streams back', async () => {
    server.use(
      ...baseHandlers(),
      http.get('/api/v1/node-agents/sessions/sess-1/stream', () =>
        sse([
          frame('assistant_start', { type: 'assistant_start', session_id: 'sess-1', iteration: 1 }),
          frame('assistant_delta', {
            type: 'assistant_delta',
            session_id: 'sess-1',
            iteration: 1,
            assistant: { content: 'the pod is in CrashLoopBackOff' },
          }),
          frame('done', {
            type: 'done',
            session_id: 'sess-1',
            iteration: 1,
            done: { iterations: 1, tool_calls: 2 },
          }),
        ])
      )
    );

    await openConversation();

    expect(await screen.findByText('the pod is in CrashLoopBackOff')).toBeInTheDocument();
    expect(await screen.findByText(/1 iteration\(s\) · 2 tool call\(s\)/)).toBeInTheDocument();
  });

  it('accumulates deltas when no assistant_end ever arrives', async () => {
    // The other direction, and the one a streaming turn actually takes.
    // assistant_end is allowed to be omitted by a non-streaming producer,
    // so a console that only ever reads the end frame shows an empty
    // bubble for every turn that ends without one. Concatenation is what
    // makes the deltas worth sending at all — without it the token-level
    // frame is decoration.
    server.use(
      ...baseHandlers(),
      http.get('/api/v1/node-agents/sessions/sess-1/stream', () =>
        sse([
          frame('assistant_delta', {
            type: 'assistant_delta',
            iteration: 1,
            assistant: { content: 'the pod has been ' },
          }),
          frame('assistant_delta', {
            type: 'assistant_delta',
            iteration: 1,
            assistant: { content: 'in CrashLoopBackOff for 6 minutes' },
          }),
        ])
      )
    );

    await openConversation();

    // One element holding both halves. Asserting each half separately would
    // pass even if the second delta replaced the first, which is precisely
    // the bug: the operator would read a sentence that was never said.
    expect(
      await screen.findByText('the pod has been in CrashLoopBackOff for 6 minutes')
    ).toBeInTheDocument();
  });

  it('prefers the full text on assistant_end over the deltas it accumulated', async () => {
    // The two deltas spell "wor" and then "ld". A console that concatenated
    // them would render "world" too — so the assertion below is paired
    // with a THIRD delta that assistant_end contradicts. The full text
    // wins because a dropped delta then costs staleness rather than
    // truncation, and truncation is not recoverable after the fact.
    server.use(
      ...baseHandlers(),
      http.get('/api/v1/node-agents/sessions/sess-1/stream', () =>
        sse([
          frame('assistant_delta', {
            type: 'assistant_delta',
            iteration: 1,
            assistant: { content: 'wor' },
          }),
          frame('assistant_delta', {
            type: 'assistant_delta',
            iteration: 1,
            assistant: { content: 'ld, but also trailing junk' },
          }),
          frame('assistant_end', {
            type: 'assistant_end',
            iteration: 1,
            assistant: { content: 'world' },
          }),
        ])
      )
    );

    await openConversation();

    expect(await screen.findByText('world')).toBeInTheDocument();
    expect(screen.queryByText('world, but also trailing junk')).not.toBeInTheDocument();
  });

  it('shows a tool call that the policy gate blocked as blocked, not as an error', async () => {
    server.use(
      ...baseHandlers(),
      http.get('/api/v1/node-agents/sessions/sess-1/stream', () =>
        sse([
          frame('tool_end', {
            type: 'tool_end',
            iteration: 1,
            tool: {
              tool_call_id: 'tc-1',
              name: 'restart_service',
              status: 'blocked',
              class: 'L2',
              error: 'the host policy gate refused this call',
            },
          }),
        ])
      )
    );

    await openConversation();

    // A blocked call is the safety feature working. Rendering it in the
    // same red as a failure teaches operators to ignore the one signal
    // that says the gate held.
    expect(await screen.findByText('blocked')).toBeInTheDocument();
    expect(screen.queryByText('error')).not.toBeInTheDocument();
    expect(await screen.findByText('the host policy gate refused this call')).toBeInTheDocument();
  });

  it('renders the dropped-frame count next to the conversation it qualifies', async () => {
    server.use(
      ...baseHandlers([
        { session_id: 'sess-1', edge_id: 7, frames: 120, dropped: 4, attached: true, terminal: false },
      ]),
      http.get('/api/v1/node-agents/sessions/sess-1/stream', () => sse([]))
    );

    await openConversation();

    // Both places: the session row (so a conversation is self-describing
    // before you open it) and the transcript footer (so the gaps on screen
    // are explained while you are looking at them).
    expect(await screen.findByText('4 dropped')).toBeInTheDocument();
    expect(
      await screen.findByText(/4 frames were dropped by backpressure; the transcript above has gaps/)
    ).toBeInTheDocument();
  });

  it('tells the operator to end a conversation when the node is at its limit', async () => {
    server.use(
      ...baseHandlers(),
      http.post('/api/v1/node-agents/sessions', () =>
        HttpResponse.json(
          { error: { message: 'fleet full', code: 'conversation_limit' } },
          { status: 429 }
        )
      )
    );


    const user = userEvent.setup();
    render(<NodeAgentsPage />);
    await user.click(await screen.findByRole('button', { name: /new session/i }));

    // The assertion is on the *action*, not just on an error appearing.
    // "Try again later" would also render an error and would send the
    // operator to do the one thing that cannot help here.
    expect(
      await screen.findByText(/End one and try again — waiting will not help/)
    ).toBeInTheDocument();
    expect(screen.queryByText(/try again later/i)).not.toBeInTheDocument();
  });

  it('sends the digest back with an approval decision', async () => {
    let decided: Record<string, unknown> | null = null;
    server.use(
      ...baseHandlers(),
      http.get('/api/v1/node-agents/sessions/sess-1/stream', () =>
        sse([
          frame('approval_pending', {
            type: 'approval_pending',
            iteration: 1,
            approval: {
              request_id: 'req-9',
              digest: 'sha256:abc',
              tool: 'restart_service',
              class: 'L2',
              summary: 'restart api-gateway on edge-prod-1',
              blast_radius: 'pod',
            },
          }),
        ])
      ),
      http.post(
        '/api/v1/node-agents/sessions/sess-1/approvals/:rid/decide',
        ({ params, request }) => {
          decided = { ...(params as Record<string, string>), body: null };
          return request.json().then((body) => {
            decided = { rid: (params as Record<string, string>).rid, body };
            return HttpResponse.json({ status: 'applied' });
          });
        }
      )
    );

    const user = await openConversation();

    // blast_radius is on the card because the operator is being asked to
    // authorise a change and the radius is the only thing on screen that
    // says how much of the estate is about to move.
    expect(await screen.findByText(/blast radius: pod/i)).toBeInTheDocument();

    await user.click(screen.getByRole('button', { name: /^approve$/i }));

    await waitFor(() => expect(decided).not.toBeNull());
    expect(decided).toMatchObject({
      rid: 'req-9',
      body: { request_id: 'req-9', digest: 'sha256:abc', grant: true },
    });
  });

  it('disables sending when the stream drops with text already typed', async () => {
    // The other half of "you cannot send into the void", and the half that
    // is actually reachable: the operator is mid-sentence when the node's
    // stream breaks. The composer is enabled by the stream being attached,
    // so a drop has to disable it again — otherwise the button stays live
    // on a conversation the manager will answer with 409, and the operator
    // watches a message vanish with no error.
    //
    // This case is also the only one that can catch a regression in the
    // send button's own guard. Asserting it before attach cannot: the
    // textarea is disabled then too, so the input is always empty and
    // `!input.trim()` disables the button by itself. Two guards both
    // holding is not coverage of either.
    // The stream's controller is held so the test decides when the node
    // goes away. Letting a timer do it would race the typing below, and a
    // test whose subject is "what is enabled at this instant" cannot have
    // its subject decided by scheduling.
    // The holder is an object rather than a bare `let` because the
    // assignment happens inside the ReadableStream's start callback, and
    // TypeScript's control-flow analysis narrows a bare `let` to `null` at
    // every use site outside that callback — it cannot see the assignment
    // and concludes the call is on `never`. A property is not narrowed
    // across the same boundary.
    const drop: { now: (() => void) | null } = { now: null };
    server.use(
      ...baseHandlers(),
      http.get('/api/v1/node-agents/sessions/sess-1/stream', () => {
        const enc = new TextEncoder();
        const body = new ReadableStream<Uint8Array>({
          start(controller) {
            controller.enqueue(enc.encode(': ok\n\n'));
            controller.enqueue(
              enc.encode(
                frame('assistant_delta', {
                  type: 'assistant_delta',
                  iteration: 1,
                  assistant: { content: 'checking the pod now' },
                })
              )
            );
            drop.now = () => controller.error(new Error('node agent went away'));
          },
        });
        return new HttpResponse(body, { headers: { 'Content-Type': 'text/event-stream' } });
      })
    );

    const user = await openConversation();

    expect(await screen.findByText('checking the pod now')).toBeInTheDocument();
    const box = await screen.findByRole('textbox');
    const send = screen.getByRole('button', { name: /^send$/i });

    await user.type(box, 'and now restart it');
    expect(send).toBeEnabled();

    // The node's agent process dies with a half-typed message in the box.
    // That is the moment the send button has to go: the manager is no
    // longer listening, and a live button would swallow the message.
    drop.now?.();
    await waitFor(() => expect(send).toBeDisabled());
    expect(box).toBeDisabled();
    expect(box).toHaveValue('and now restart it');
  });

  it('will not send before the stream is attached', async () => {
    let sent = 0;
    server.use(
      ...baseHandlers(),
      // A stream that fails to attach. The composer must stay disabled:
      // the manager answers 409 to a send nobody is listening for, and a
      // console that lets the operator try anyway has taught them that
      // this button is unreliable.
      http.get('/api/v1/node-agents/sessions/sess-1/stream', () =>
        HttpResponse.json({ error: { message: 'no such session', code: 'no_session' } }, { status: 404 })
      ),
      http.post('/api/v1/node-agents/sessions/:sid/messages', () => {
        sent += 1;
        return HttpResponse.json({ status: 'accepted' }, { status: 202 });
      })
    );

    await openConversation();

    expect(await screen.findByText(/no such session/)).toBeInTheDocument();
    const box = await screen.findByPlaceholderText(/Attaching to the stream/i);
    expect(box).toBeDisabled();
    expect(sent).toBe(0);
  });
});
