// 节点 Agent 页面 — drive a `pig --mode rpc` process living on an edge.
//
// The page is a conversation surface, not a dashboard, and that shapes
// almost every decision in it. A node agent is reached through the control
// plane, so the console cannot assume a live socket: it opens a
// conversation, attaches to that conversation's stream, and only then may
// send. That ordering is the contract (the manager answers 409
// `not_streaming` to a send nobody is listening for), which is why `send`
// is wired to the stream being attached rather than to the button being
// enabled.
//
// Two things on this screen exist purely to keep the page honest, and both
// would be the first things to cut if this were only a chat UI:
//
//   - The dropped-frame counter. The stream discards frames when a
//     consumer falls behind, which is correct for a conversation nobody is
//     reading and otherwise indistinguishable from the agent going quiet.
//     An operator reading a transcript with holes in it has to be able to
//     see that the holes are ours. `listSessions` is the only place that
//     number exists, so the page polls it and shows it next to the
//     transcript it qualifies.
//   - The node's own health, separately from what the agent is doing. A
//     process can be mid-turn and still crash-looping; `degraded` plus a
//     non-zero restart count is what that looks like before it becomes an
//     outage, and a page that only showed "running" would report healthy
//     for the whole window in which it was not.
import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { AlertTriangle, Bot, CircleSlash, Loader2, RefreshCw, Square, X } from 'lucide-react';

import { listEdges, type Edge } from '@/api/edges';
import {
  attachStream,
  closeSession,
  decideApproval,
  listSessions,
  nodeHealth,
  nodeState,
  openSession,
  sendMessage,
  stopSession,
  type ApprovalFrame,
  type NodeHealth,
  type NodeState,
  type SessionStat,
  type StreamFrame,
  type ToolFrame,
} from '@/api/nodeAgents';
import { ApiError } from '@/api/client';
import { Button, Card, Chip, EmptyState, PageHeader } from '@/components/ui';
import { cn } from '@/lib/cn';
import { useI18n } from '@/i18n/locale';

/** Translator is useI18n()'s `tr`. It is named structurally rather than
 *  imported from the hook because the failure copy below is pure functions
 *  that run outside render; typing them this way keeps them testable with
 *  no provider while the hook stays the single source of the real one. */
type Translator = (zh: string, en: string) => string;

// ---------------------------------------------------------------------------
// Transcript model
// ---------------------------------------------------------------------------

/** A tool call as the transcript shows it. One entry per tool_call_id,
 *  created on tool_start and settled by tool_end — the two frames carry
 *  the same id precisely so a start that never settles stays visible as
 *  pending rather than vanishing. */
type ToolEntry = ToolFrame & { settled: boolean };

type Bubble = {
  key: string;
  /** deltas accumulate into this until assistant_end replaces it with
   *  the authoritative full text. The node sends both, and preferring the
   *  end frame means a dropped delta degrades to a slightly stale bubble
   *  instead of a truncated one. */
  content: string;
  iteration: number;
  streaming: boolean;
  tools: ToolEntry[];
  approvals: ApprovalFrame[];
  error?: string;
  done?: { iterations: number; tool_calls: number; cost_usd?: number };
};

/** applyFrame folds one stream frame into the transcript.
 *
 *  It is a pure reducer rather than a set of effects because frame order
 *  is the only thing that makes this renderable: assistant_delta frames
 *  arrive interleaved with tool frames for the same iteration, and a tool
 *  that starts before its assistant bubble closes has to land in the
 *  bubble that is still open. Anything that dispatched per-frame would
 *  get that wrong under exactly the conditions that matter — a turn doing
 *  real work. */
function applyFrame(bubbles: Bubble[], frame: StreamFrame): Bubble[] {
  const iteration = frame.iteration ?? 1;

  // A frame for a turn that has no bubble yet opens one. This happens on
  // reattach, where the console joins a conversation mid-turn and the
  // assistant_start that would have opened the bubble was already sent.
  const ensure = (list: Bubble[]): Bubble[] => {
    if (list.some((b) => b.iteration === iteration && b.streaming)) return list;
    return [
      ...list,
      { key: `it-${iteration}`, content: '', iteration, streaming: true, tools: [], approvals: [] },
    ];
  };

  switch (frame.type) {
    case 'assistant_start':
      return ensure(bubbles).map((b) =>
        b.iteration === iteration ? { ...b, streaming: true, error: undefined } : b
      );

    case 'assistant_delta': {
      const chunk = frame.assistant?.content ?? '';
      return ensure(bubbles).map((b) =>
        b.iteration === iteration ? { ...b, content: b.content + chunk } : b
      );
    }

    case 'assistant_end':
      return ensure(bubbles).map((b) =>
        b.iteration === iteration
          ? {
              ...b,
              // Full text wins over what was accumulated. A dropped delta
              // shows up as a stale bubble, which is recoverable; a
              // truncated one is not.
              content: frame.assistant?.content ?? b.content,
              streaming: false,
            }
          : b
      );

    case 'tool_start':
    case 'tool_update':
    case 'tool_end': {
      const tool = frame.tool;
      if (!tool?.tool_call_id) return bubbles;
      const settled = frame.type === 'tool_end';
      return ensure(bubbles).map((b) => {
        if (b.iteration !== iteration) return b;
        const existing = b.tools.find((t) => t.tool_call_id === tool.tool_call_id);
        if (existing) {
          return {
            ...b,
            tools: b.tools.map((t) =>
              t.tool_call_id === tool.tool_call_id ? { ...t, ...tool, settled } : t
            ),
          };
        }
        return { ...b, tools: [...b.tools, { ...tool, settled }] };
      });
    }

    case 'approval_pending': {
      const approval = frame.approval;
      if (!approval?.request_id) return bubbles;
      return ensure(bubbles).map((b) => {
        if (b.iteration !== iteration) return b;
        if (b.approvals.some((a) => a.request_id === approval.request_id)) return b;
        return { ...b, approvals: [...b.approvals, approval] };
      });
    }

    case 'approval_resolved':
      // The decision has landed. The card stays — an operator reading back
      // wants to see what was asked and what was answered — but it stops
      // offering buttons, which is what removing it would also achieve
      // and what re-rendering it as unanswered would not.
      return bubbles;

    case 'error':
      return ensure(bubbles).map((b) =>
        b.iteration === iteration
          ? {
              ...b,
              streaming: false,
              error: frame.error?.message || frame.error?.code || 'unknown error',
            }
          : b
      );

    case 'done':
      return ensure(bubbles).map((b) =>
        b.iteration === iteration
          ? {
              ...b,
              streaming: false,
              done: {
                iterations: frame.done?.iterations ?? iteration,
                tool_calls: frame.done?.tool_calls ?? b.tools.length,
                cost_usd: frame.done?.usage?.cost_usd,
              },
            }
          : b
      );

    default:
      // The agent's vocabulary is PiG's and it moves. An unknown frame is
      // relayed rather than dropped upstream specifically so this can
      // ignore it — rendering nothing is correct, throwing is not.
      return bubbles;
  }
}

// ---------------------------------------------------------------------------
// Page
// ---------------------------------------------------------------------------

export default function NodeAgentsPage() {
  const { tr } = useI18n();

  const [edges, setEdges] = useState<Edge[]>([]);
  const [edgeId, setEdgeId] = useState<number | null>(null);
  const [state, setState] = useState<NodeState | null>(null);
  const [health, setHealth] = useState<NodeHealth | null>(null);
  const [sessions, setSessions] = useState<SessionStat[]>([]);

  const [sessionId, setSessionId] = useState<string | null>(null);
  const [bubbles, setBubbles] = useState<Bubble[]>([]);
  const [attached, setAttached] = useState(false);
  const [input, setInput] = useState('');
  const [busy, setBusy] = useState(false);
  const [notice, setNotice] = useState<{ tone: 'error' | 'info'; text: string } | null>(null);

  const abortRef = useRef<AbortController | null>(null);
  const bottomRef = useRef<HTMLDivElement | null>(null);

  // Node list. The page is useless without a node to talk to, so an empty
  // list is an empty state rather than a silent blank page.
  useEffect(() => {
    let alive = true;
    listEdges()
      .then((r) => {
        if (!alive) return;
        setEdges(r.items ?? []);
        setEdgeId((cur) => cur ?? r.items?.[0]?.id ?? null);
      })
      .catch(() => {
        if (alive) setNotice({ tone: 'error', text: tr('节点列表加载失败', 'Could not load nodes') });
      });
    return () => {
      alive = false;
    };
  }, [tr]);

  // Per-node state and health. Polled rather than pushed: both answers
  // change on the node's schedule (a turn starting, a supervisor deciding
  // to restart), not on the console's, and the poll interval is short
  // enough that an operator watching a node come back up sees it.
  useEffect(() => {
    if (edgeId == null) return;
    let alive = true;
    const load = () => {
      nodeState(edgeId).then((s) => alive && setState(s)).catch(() => undefined);
      nodeHealth(edgeId).then((h) => alive && setHealth(h)).catch(() => undefined);
    };
    load();
    const t = window.setInterval(load, 5000);
    return () => {
      alive = false;
      window.clearInterval(t);
    };
  }, [edgeId]);

  // Session list, for the drop counter and for letting an operator rejoin
  // a conversation another console left open.
  const refreshSessions = useCallback(() => {
    listSessions()
      .then(setSessions)
      .catch(() => undefined);
  }, []);

  useEffect(() => {
    refreshSessions();
    const t = window.setInterval(refreshSessions, 5000);
    return () => window.clearInterval(t);
  }, [refreshSessions]);

  // The transcript follows its tail. Pinned to the bottom rather than to
  // the last bubble so an expanding tool card does not scroll past it.
  useEffect(() => {
    // The optional call is not defensive padding. scrollIntoView is absent
    // in jsdom, so an unguarded call takes the whole page down under test,
    // and a transcript that throws while it renders is a transcript nobody
    // can read. Following the tail is a nicety; rendering is not.
    bottomRef.current?.scrollIntoView?.({ block: 'end' });
  }, [bubbles]);

  // Detach on unmount and on session switch. The manager tracks whether a
  // console is attached and uses it to decide whether a send has a
  // listener, so leaking an attachment would make a send look answered
  // when it is being dropped.
  useEffect(() => {
    return () => {
      abortRef.current?.abort();
      abortRef.current = null;
    };
  }, []);

  const attach = useCallback(
    (sid: string) => {
      abortRef.current?.abort();
      const ac = new AbortController();
      abortRef.current = ac;
      setAttached(false);
      void attachStream(
        sid,
        {
          onOpen: () => setAttached(true),
          onFrame: (f) => setBubbles((cur) => applyFrame(cur, f)),
          onError: (err) => {
            setAttached(false);
            setNotice({ tone: 'error', text: err.message });
          },
          onClose: () => setAttached(false),
        },
        ac.signal
      );
    },
    []
  );

  const handleOpen = useCallback(async () => {
    if (edgeId == null) return;
    setBusy(true);
    setNotice(null);
    try {
      const r = await openSession({ edge_id: edgeId, locale: undefined });
      setSessionId(r.session_id);
      setBubbles([]);
      attach(r.session_id);
      refreshSessions();
    } catch (err) {
      setNotice({ tone: 'error', text: describeOpenFailure(err, tr) });
    } finally {
      setBusy(false);
    }
  }, [attach, edgeId, refreshSessions, tr]);

  const handleJoin = useCallback(
    (sid: string) => {
      setSessionId(sid);
      setBubbles([]);
      attach(sid);
    },
    [attach]
  );

  const handleSend = useCallback(
    async (steer: boolean) => {
      const text = input.trim();
      if (!sessionId || !text) return;
      setBusy(true);
      try {
        await sendMessage(sessionId, text, steer);
        setInput('');
      } catch (err) {
        setNotice({ tone: 'error', text: describeSendFailure(err, tr) });
      } finally {
        setBusy(false);
      }
    },
    [input, sessionId, tr]
  );

  const handleStop = useCallback(async () => {
    if (!sessionId) return;
    try {
      await stopSession(sessionId);
    } catch (err) {
      setNotice({ tone: 'error', text: (err as Error).message });
    }
  }, [sessionId]);

  const handleClose = useCallback(async () => {
    if (!sessionId) return;
    abortRef.current?.abort();
    abortRef.current = null;
    try {
      await closeSession(sessionId);
    } catch {
      // Closing is best-effort from the console's side: the manager drops
      // the conversation either way when the last console detaches, and a
      // failed call here must not strand the operator on a dead session.
    }
    setSessionId(null);
    setBubbles([]);
    setAttached(false);
    refreshSessions();
  }, [refreshSessions, sessionId]);

  const handleDecide = useCallback(
    async (approval: ApprovalFrame, grant: boolean) => {
      if (!sessionId) return;
      try {
        await decideApproval(sessionId, approval.request_id, {
          digest: approval.digest,
          grant,
        });
      } catch (err) {
        setNotice({ tone: 'error', text: (err as Error).message });
      }
    },
    [sessionId]
  );

  const current = useMemo(
    () => sessions.find((s) => s.session_id === sessionId) ?? null,
    [sessionId, sessions]
  );
  const streaming = bubbles.some((b) => b.streaming);

  return (
    <main className="flex h-full flex-col">
      <PageHeader
        title={tr('节点 Agent', 'Node Agents')}
        subtitle={tr(
          '在边缘节点上运行 pig 的运维 Agent，全部工具调用经宿主策略闸门',
          'Operational agents running PiG on the edge; every tool call passes the host policy gate'
        )}
        actions={
          <>
            <select
              className="rounded-md border border-zinc-700 bg-zinc-900 px-2 py-1.5 text-xs text-zinc-300"
              value={edgeId ?? ''}
              onChange={(e) => {
                const v = e.target.value ? Number(e.target.value) : null;
                setEdgeId(v);
                setState(null);
                setHealth(null);
              }}
            >
              <option value="">{tr('选择节点…', 'Select a node…')}</option>
              {edges.map((e) => (
                <option key={e.id} value={e.id}>
                  {e.name}
                </option>
              ))}
            </select>
            <Button variant="ghost" onClick={refreshSessions} title={tr('刷新', 'Refresh')}>
              <RefreshCw className="h-3.5 w-3.5" />
            </Button>
            {sessionId ? (
              <Button variant="ghost" onClick={handleStop} disabled={!streaming}>
                <Square className="h-3.5 w-3.5" />
                {tr('停止', 'Stop')}
              </Button>
            ) : null}
            {sessionId ? (
              <Button variant="danger" onClick={handleClose}>
                <X className="h-3.5 w-3.5" />
                {tr('结束会话', 'End session')}
              </Button>
            ) : (
              <Button variant="primary" onClick={handleOpen} disabled={busy || edgeId == null}>
                {busy ? <Loader2 className="h-3.5 w-3.5 animate-spin" /> : <Bot className="h-3.5 w-3.5" />}
                {tr('新建会话', 'New session')}
              </Button>
            )}
          </>
        }
      />

      <div className="grid flex-1 grid-cols-1 gap-4 overflow-auto p-6 lg:grid-cols-[280px_1fr]">
        <div className="space-y-4">
          <NodeStatusCard state={state} health={health} />

          <Card className="space-y-2">
            <div className="flex items-center justify-between">
              <h2 className="text-xs font-semibold text-zinc-300">
                {tr('进行中的会话', 'Open conversations')}
              </h2>
              <Chip dense>{sessions.length}</Chip>
            </div>
            {sessions.length === 0 ? (
              <p className="text-xs text-zinc-500">{tr('暂无会话', 'No conversations')}</p>
            ) : (
              <ul className="space-y-1.5">
                {sessions.map((s) => (
                  <li key={s.session_id}>
                    <button
                      type="button"
                      onClick={() => handleJoin(s.session_id)}
                      className={cn(
                        'w-full rounded-md border border-zinc-800/60 px-2 py-1.5 text-left text-xs transition-colors hover:border-zinc-700',
                        s.session_id === sessionId && 'border-accent/60 bg-accent/10'
                      )}
                    >
                      <div className="flex items-center justify-between gap-2">
                        <span className="truncate font-mono text-zinc-300">{s.session_id}</span>
                        <span className="shrink-0 text-[10px] text-zinc-500">#{s.edge_id}</span>
                      </div>
                      <div className="mt-1 flex flex-wrap items-center gap-1">
                        {s.attached ? (
                          <Chip tone="info" dense>
                            {tr('已连接', 'attached')}
                          </Chip>
                        ) : (
                          <Chip dense>{tr('未连接', 'detached')}</Chip>
                        )}
                        {s.terminal ? (
                          <Chip dense>{tr('已结束', 'terminal')}</Chip>
                        ) : null}
                        {/* The drop counter is rendered here, beside the
                            conversation it qualifies, rather than in a
                            banner. It answers a question about this
                            transcript and belongs next to it. */}
                        {s.dropped > 0 ? (
                          <Chip tone="warning" dense>
                            {tr(`丢帧 ${s.dropped}`, `${s.dropped} dropped`)}
                          </Chip>
                        ) : null}
                      </div>
                    </button>
                  </li>
                ))}
              </ul>
            )}
          </Card>
        </div>

        <div className="flex min-h-[480px] flex-col">
          {notice ? (
            <div
              className={cn(
                'mb-3 flex items-start gap-2 rounded-md border px-3 py-2 text-xs',
                notice.tone === 'error'
                  ? 'border-red-500/40 bg-red-500/10 text-red-200'
                  : 'border-sky-500/40 bg-sky-500/10 text-sky-200'
              )}
            >
              <AlertTriangle className="mt-0.5 h-3.5 w-3.5 shrink-0" />
              <span>{notice.text}</span>
            </div>
          ) : null}

          {!sessionId ? (
            <EmptyState
              icon={Bot}
              title={tr('未打开会话', 'No conversation open')}
              hint={tr(
                '选一个节点然后新建会话；会话建立后先挂上流再发消息，否则请求会被拒',
                'Pick a node and open a conversation; the stream must be attached before the first message'
              )}
            />
          ) : (
            <>
              <div className="flex-1 space-y-3 overflow-auto rounded-xl border border-zinc-800/60 bg-zinc-900/40 p-4">
                {bubbles.length === 0 ? (
                  <p className="text-xs text-zinc-500">
                    {attached
                      ? tr('已连接，等待节点回答…', 'Attached, waiting for the node…')
                      : tr('正在连接…', 'Connecting…')}
                  </p>
                ) : (
                  bubbles.map((b) => (
                    <BubbleView
                      key={b.key}
                      bubble={b}
                      onDecide={handleDecide}
                      decidingDisabled={!sessionId}
                    />
                  ))
                )}
                <div ref={bottomRef} />
              </div>

              {current && current.dropped > 0 ? (
                <p className="mt-2 text-[11px] text-amber-300">
                  {tr(
                    `此会话已丢弃 ${current.dropped} 帧（消费端跟不上时的背压保护）——上面的记录有缺口。`,
                    `${current.dropped} frames were dropped by backpressure; the transcript above has gaps.`
                  )}
                </p>
              ) : null}

              <div className="mt-3 flex items-end gap-2">
                <textarea
                  className="min-h-[64px] flex-1 resize-y rounded-md border border-zinc-700 bg-zinc-900 px-3 py-2 text-sm text-zinc-100 placeholder:text-zinc-500"
                  placeholder={
                    attached
                      ? tr('对节点上的 Agent 说话…', 'Talk to the agent on the node…')
                      : tr('正在连接流…', 'Attaching to the stream…')
                  }
                  value={input}
                  disabled={!attached}
                  onChange={(e) => setInput(e.target.value)}
                  onKeyDown={(e) => {
                    // Enter sends, Shift+Enter newlines. A node agent turn
                    // can run for minutes, so a stray newline should not
                    // be the thing that sends it.
                    if (e.key === 'Enter' && !e.shiftKey) {
                      e.preventDefault();
                      void handleSend(false);
                    }
                  }}
                />
                <div className="flex flex-col gap-1.5">
                  <Button variant="primary" disabled={!attached || busy || !input.trim()} onClick={() => void handleSend(false)}>
                    {tr('发送', 'Send')}
                  </Button>
                  {/* Steer is a separate button, not a modifier, because
                      it is a different action: it injects into the turn
                      already running. Hiding it behind a modifier is how
                      it becomes an accident. */}
                  <Button variant="ghost" disabled={!attached || busy || !streaming || !input.trim()} onClick={() => void handleSend(true)}>
                    {tr('插话', 'Steer')}
                  </Button>
                </div>
              </div>
            </>
          )}
        </div>
      </div>
    </main>
  );
}

// ---------------------------------------------------------------------------
// Pieces
// ---------------------------------------------------------------------------

function NodeStatusCard({ state, health }: { state: NodeState | null; health: NodeHealth | null }) {
  const { tr } = useI18n();
  return (
    <Card className="space-y-2">
      <h2 className="text-xs font-semibold text-zinc-300">{tr('Agent 状态', 'Agent state')}</h2>
      {!state && !health ? (
        <p className="text-xs text-zinc-500">{tr('未选择节点', 'No node selected')}</p>
      ) : (
        <div className="space-y-1.5 text-xs text-zinc-400">
          <div className="flex flex-wrap items-center gap-1.5">
            {health?.running ? (
              <Chip tone="success" dense>
                {tr('运行中', 'running')}
              </Chip>
            ) : (
              <Chip tone="danger" dense>
                {tr('未运行', 'not running')}
              </Chip>
            )}
            {state?.running ? (
              <Chip tone="info" dense>
                {tr('执行中', 'turn in flight')}
              </Chip>
            ) : null}
            {/* Degraded is rendered as its own chip rather than folded
                into the running state. A supervisor that has given up
                restarting is running a process it does not trust, and
                that is a different thing from running. */}
            {health?.degraded ? (
              <Chip tone="warning" dense>
                {tr('降级', 'degraded')}
              </Chip>
            ) : null}
            {health && health.restarts > 0 ? (
              <Chip dense>{tr(`重启 ${health.restarts} 次`, `${health.restarts} restarts`)}</Chip>
            ) : null}
          </div>
          {state?.model ? (
            <div>
              {tr('模型', 'Model')}: {state.provider ? `${state.provider}/` : ''}
              {state.model}
            </div>
          ) : null}
          {state?.pending_tool_calls ? (
            <div>
              {tr('待定工具调用', 'Pending tool calls')}: {state.pending_tool_calls}
            </div>
          ) : null}
          {health?.version ? (
            <div>
              {tr('版本', 'Version')}: {health.version}
            </div>
          ) : null}
          {health?.last_start_at ? (
            <div>
              {tr('启动于', 'Started')}: {new Date(health.last_start_at).toLocaleString()}
            </div>
          ) : null}
          {health?.last_error ? (
            <div className="rounded-md border border-red-500/30 bg-red-500/10 px-2 py-1 text-red-200">
              {health.last_error}
            </div>
          ) : null}
        </div>
      )}
    </Card>
  );
}

function BubbleView({
  bubble,
  onDecide,
}: {
  bubble: Bubble;
  onDecide: (approval: ApprovalFrame, grant: boolean) => void;
  decidingDisabled?: boolean;
}) {
  const { tr } = useI18n();
  return (
    <div className="space-y-2 rounded-lg border border-zinc-800/60 bg-zinc-900/60 p-3">
      {bubble.content ? (
        <div className="whitespace-pre-wrap text-sm text-zinc-100">{bubble.content}</div>
      ) : bubble.streaming ? (
        <div className="text-xs text-zinc-500">{tr('思考中…', 'Thinking…')}</div>
      ) : null}

      {bubble.tools.map((t) => (
        <ToolRow key={t.tool_call_id} tool={t} />
      ))}

      {bubble.approvals.map((a) => (
        <ApprovalCard key={a.request_id} approval={a} onDecide={onDecide} />
      ))}

      {bubble.error ? (
        <div className="flex items-start gap-1.5 text-xs text-red-300">
          <CircleSlash className="mt-0.5 h-3.5 w-3.5 shrink-0" />
          <span>{bubble.error}</span>
        </div>
      ) : null}

      {bubble.done ? (
        <div className="text-[11px] text-zinc-500">
          {tr(
            `第 ${bubble.done.iterations} 轮 · ${bubble.done.tool_calls} 次工具调用` +
              (bubble.done.cost_usd ? ` · $${bubble.done.cost_usd.toFixed(4)}` : ''),
            `${bubble.done.iterations} iteration(s) · ${bubble.done.tool_calls} tool call(s)` +
              (bubble.done.cost_usd ? ` · $${bubble.done.cost_usd.toFixed(4)}` : '')
          )}
        </div>
      ) : null}
    </div>
  );
}

function ToolRow({ tool }: { tool: ToolEntry }) {
  const { tr } = useI18n();
  const tone = !tool.settled
    ? 'info'
    : tool.status === 'success'
      ? 'success'
      : tool.status === 'blocked'
        ? 'warning'
        : 'danger';
  return (
    <div className="rounded-md border border-zinc-800/60 bg-zinc-950/40 px-2.5 py-2">
      <div className="flex items-center justify-between gap-2">
        <span className="truncate font-mono text-xs text-zinc-300">{tool.name}</span>
        <Chip tone={tone} dense>
          {!tool.settled
            ? tr('执行中', 'running')
            : tool.status === 'blocked'
              ? tr('被策略拦截', 'blocked')
              : (tool.status ?? tr('完成', 'done'))}
        </Chip>
      </div>
      {tool.class ? <div className="mt-1 text-[10px] text-zinc-500">{tool.class}</div> : null}
      {tool.args_json ? (
        <pre className="mt-1 overflow-x-auto text-[11px] text-zinc-500">{tool.args_json}</pre>
      ) : null}
      {tool.error ? <div className="mt-1 text-[11px] text-red-300">{tool.error}</div> : null}
    </div>
  );
}

function ApprovalCard({
  approval,
  onDecide,
}: {
  approval: ApprovalFrame;
  onDecide: (approval: ApprovalFrame, grant: boolean) => void;
}) {
  const { tr } = useI18n();
  return (
    <div className="rounded-md border border-amber-500/40 bg-amber-500/10 p-2.5">
      <div className="text-xs font-medium text-amber-200">
        {tr('需要人工审批', 'Approval required')}
      </div>
      <div className="mt-1 text-xs text-zinc-200">{approval.summary || approval.tool}</div>
      <div className="mt-1 flex flex-wrap items-center gap-1.5 text-[11px] text-zinc-400">
        {approval.class ? <Chip dense>{approval.class}</Chip> : null}
        {/* blast_radius is shown because the operator is being asked to
            authorise a change and the radius is the only thing that tells
            them how much of the estate is about to change. */}
        {approval.blast_radius ? (
          <Chip tone="warning" dense>
            {tr('影响半径', 'blast radius')}: {approval.blast_radius}
          </Chip>
        ) : null}
        {approval.target ? <span className="font-mono">{approval.target}</span> : null}
      </div>
      <div className="mt-2 flex gap-2">
        <Button variant="primary" onClick={() => onDecide(approval, true)}>
          {tr('批准', 'Approve')}
        </Button>
        <Button variant="ghost" onClick={() => onDecide(approval, false)}>
          {tr('拒绝', 'Deny')}
        </Button>
      </div>
    </div>
  );
}

// ---------------------------------------------------------------------------
// Failure copy
// ---------------------------------------------------------------------------

/** describeOpenFailure turns a refusal into the sentence an operator can
 *  act on.
 *
 *  The 429 is the one that matters. It arrives as `conversation_limit`, and
 *  it does not mean "wait and retry" — the conversations already open on
 *  that node are not going to close themselves, so the next action is to
 *  end one. Rendering it as a generic failure would send the operator to
 *  do the one thing that cannot help. */
function describeOpenFailure(err: unknown, tr: Translator): string {
  if (err instanceof ApiError && err.code === 'conversation_limit') {
    return tr(
      '该节点的会话数已达上限。请先结束一个会话再试——等待不会解决问题。',
      'This node already holds as many conversations as it allows. End one and try again — waiting will not help.'
    );
  }
  if (err instanceof ApiError && err.status === 0) {
    return tr('控制面不可达。', 'The control plane is unreachable.');
  }
  return (err as Error).message || tr('无法打开会话', 'could not open a conversation');
}

function describeSendFailure(err: unknown, tr: Translator): string {
  if (err instanceof ApiError && err.code === 'not_streaming') {
    return tr(
      '控制台没有挂上该会话的流，消息发出去也没有人回答。',
      'The console is not attached to this conversation\'s stream, so the message would have had nobody to answer it.'
    );
  }
  return (err as Error).message || tr('发送失败', 'could not send');
}
