import { useCallback, useEffect, useMemo, useState } from 'react';
import { listIncidentEvents, type IncidentEvent } from '@/api/alerts';
import {
  createSession,
  getMessages,
  listSessions,
  postMessage,
  type ChatSession,
} from '@/api/chat';
import { fetchLoopTimeline } from '@/api/loops';
import { usePoll } from '@/lib/usePoll';
import { cn } from '@/lib/cn';
import { personaLabel } from '@/components/AgentBadge';
import { AgentAvatar } from '@/components/AgentAvatar';
import { useAgents, avatarFor } from '@/store/agents';
import { ChatInput } from '@/components/ChatInput';
import { MessageBubble } from '@/components/MessageBubble';
import { PhaseIndicator } from '@/components/dpo';
import { getLocale, tr } from '@/i18n/locale';
import { stampTime } from '@/lib/format';
import { mergeIncidentStream, type SessionMessages } from './mergeIncidentStream';

// Event rows used to print `title` — the rule key — so a firing, its
// webhook receipt and the human ack all rendered as three identical
// "PGLockWaitLongTransaction" pills. Read the event_type instead: it is
// what actually happened, and the rule name is already in the page header.
const EVENT_LABEL: Record<string, { zh: string; en: string }> = {
  firing: { zh: '告警触发', en: 'Alert fired' },
  'alert.received': { zh: '收到告警', en: 'Alert received' },
  acknowledged: { zh: '已确认', en: 'Acknowledged' },
  resolved: { zh: '已解决', en: 'Resolved' },
  silenced: { zh: '已静默', en: 'Silenced' },
  escalated: { zh: '已升级', en: 'Escalated' },
  reopened: { zh: '重新触发', en: 'Reopened' },
  created: { zh: '事件创建', en: 'Incident created' },
  closed: { zh: '事件关闭', en: 'Incident closed' },
};

function eventLabel(
  e: { event_type: string; actor_type: string; title?: string; message?: string; reason?: string },
  trFn: (zh: string, en: string) => string,
): string {
  const known = EVENT_LABEL[e.event_type];
  const base = known ? trFn(known.zh, known.en) : (e.message ?? e.title ?? e.event_type);
  // A user-driven event is worth attributing — "已确认" alone hides who did it.
  const prefix = e.actor_type === 'user' ? trFn('操作人 · ', 'Operator · ') : '';
  return e.reason ? `${prefix}${base} · ${e.reason}` : `${prefix}${base}`;
}

const EVENT_LIMIT = 200;
const MESSAGE_TAIL = 100;

// The incident group chat's raw feed: the alert event timeline, the
// incident's diagnosis sessions, and each session's message tail. Held as
// one object so a single load updates them together. The progress strip and
// members row render from it, and the message stream is
// `mergeIncidentStream(feed.events, feed.sessions, feed.messagesBySession)`.
type ChatFeed = {
  events: IncidentEvent[];
  sessions: ChatSession[];
  messagesBySession: SessionMessages[];
};

const EMPTY_FEED: ChatFeed = { events: [], sessions: [], messagesBySession: [] };

// updated_at is RFC3339 and may mix precision (Go omits the fractional part
// when nanoseconds are zero: `…00.500Z` vs `…00Z`) and may carry a non-UTC
// offset (`+08:00`). Comparing the strings lexicographically misorders both,
// so compare instants — the same trap mergeIncidentStream already fixed.
function instant(s?: string): number {
  const t = Date.parse(s ?? '');
  return Number.isNaN(t) ? 0 : t;
}

export function IncidentGroupChat({ incidentId }: { incidentId: number }) {
  const byName = useAgents((s) => s.byName);
  const [feed, setFeed] = useState<ChatFeed>(EMPTY_FEED);
  const [phases, setPhases] = useState<Awaited<ReturnType<typeof fetchLoopTimeline>>['phases']>([]);
  const [draft, setDraft] = useState('');
  const [sending, setSending] = useState(false);
  const [sendError, setSendError] = useState<string | null>(null);

  const load = useCallback(async () => {
    try {
      const [ev, ss] = await Promise.all([
        listIncidentEvents(incidentId, EVENT_LIMIT),
        listSessions({ related_incident_id: incidentId }),
      ]);
      const messagesBySession = await Promise.all(
        ss.items.map(async (s) => ({
          session: s,
          messages: (await getMessages(s.id)).items.slice(-MESSAGE_TAIL),
        })),
      );
      setFeed({ events: ev.items, sessions: ss.items, messagesBySession });
    } catch {
      // A transient poll failure must not blank the loaded feed, and
      // usePoll calls this with `void` — an uncaught rejection here would
      // surface as an unhandled promise rejection. The page's own error
      // banner covers load failures for the incident itself.
    }
  }, [incidentId]);

  useEffect(() => {
    void load();
  }, [load]);
  usePoll(load, 5_000);

  useEffect(() => {
    void fetchLoopTimeline(incidentId)
      .then((r) => setPhases(r.phases))
      .catch(() => {});
  }, [incidentId]);

  // The agent members of the incident's diagnosis sessions. The human ("你")
  // is a fixed first entry at render time, not a fallback — an incident with
  // one session must still show the human alongside that session's agent.
  const members = useMemo(
    () => Array.from(new Set(feed.sessions.map((s) => s.agent_id).filter(Boolean))) as string[],
    [feed.sessions],
  );

  const stream = useMemo(
    () => mergeIncidentStream(feed.events, feed.sessions, feed.messagesBySession),
    [feed],
  );

  // send posts a follow-up to the incident's chat. When no active session
  // exists it first creates one related to this incident. A failed send is a
  // user-visible action, so surface it in `sendError` instead of swallowing
  // it; only clear the draft once the message actually posted.
  async function send(text: string) {
    const trimmed = text.trim();
    if (!trimmed || sending) return;
    setSending(true);
    setSendError(null);
    try {
      // Active session = most recently updated, non-closed one.
      const active = feed.sessions
        .filter((s) => !s.closed_at)
        .sort((a, b) => instant(b.updated_at) - instant(a.updated_at))[0];
      const target =
        active ??
        (await createSession({
          title: tr('事件追问', 'Incident follow-up'),
          related_incident_id: incidentId,
        }));
      await postMessage(target.id, trimmed, { locale: getLocale() });
      setDraft('');
      await load();
    } catch (e) {
      setSendError(e instanceof Error ? e.message : String(e));
    } finally {
      setSending(false);
    }
  }

  return (
    <div className="flex flex-col gap-3">
      <div className="surface-card flex items-center gap-1 overflow-x-auto rounded-2xl px-3 py-2">
        {phases.map((p) => (
          <span key={p.phase} className="inline-flex shrink-0 items-center gap-1.5">
            <PhaseIndicator phase={p.phase} status={p.status} className="shrink-0" />
            {p.phase === 'recovered' && p.status === 'success' && (
              <span
                data-testid="crystal-milestone-pulse"
                className="inline-block h-1.5 w-1.5 rounded-full bg-emerald-400 motion-safe:animate-pulse-dot"
                aria-hidden="true"
              />
            )}
          </span>
        ))}
      </div>

      <div className="flex items-center gap-2 px-1">
        <span className="text-xs text-zinc-500">{tr('成员', 'Members')}</span>
        <span className="flex items-center gap-1.5 text-xs text-zinc-400">
          <AgentAvatar agentId="default" size={32} avatar={avatarFor(byName, 'default')} />
          {tr('你', 'You')}
        </span>
        {members.map((id) => (
          <span key={id} className="flex items-center gap-1.5 text-xs text-zinc-400">
            <AgentAvatar agentId={id} size={32} avatar={avatarFor(byName, id)} />
            {personaLabel(id, tr)}
          </span>
        ))}
      </div>

      <div className="space-y-2">
        {stream.map((item, i) =>
          item.type === 'event' ? (
            <div
              key={`e-${item.payload.id}-${i}`}
              className="flex items-center justify-center gap-2 py-1 text-[11px] text-zinc-500"
            >
              <span
                className={cn(
                  'rounded-full bg-zinc-900/60 px-2 py-0.5',
                  item.collapsed && 'opacity-70',
                )}
                title={item.payload.reason || item.payload.event_type}
              >
                {eventLabel(item.payload, tr)}
              </span>
              <span className="tabular-nums">{stampTime(item.payload.occurred_at)}</span>
            </div>
          ) : (
            <div key={`m-${item.payload.sessionId}-${item.payload.id}`} className="anim-rise">
              <MessageBubble message={item.payload} agentId={item.payload.agentId} />
            </div>
          ),
        )}
        {sending && (
          <div
            data-testid="typing-indicator"
            className="anim-fade flex items-center gap-2 px-1 py-1 text-[11px] text-zinc-500"
          >
            <span
              className="inline-block h-1.5 w-1.5 rounded-full bg-zinc-400 motion-safe:animate-pulse-dot"
              aria-hidden="true"
            />
            <span>{tr('调查中…', 'Investigating…')}</span>
          </div>
        )}
      </div>

      {sendError && (
        <div
          role="alert"
          className="rounded-lg border border-red-500/20 bg-red-500/10 px-3 py-2 text-xs text-red-300"
        >
          {sendError}
        </div>
      )}

      <div className="rounded-2xl transition-shadow focus-within:shadow-pop">
        <ChatInput
          value={draft}
          onChange={setDraft}
          onSubmit={(p) => void send(p.text)}
          disabled={sending}
          placeholder={tr('在事件群里追问…', 'Ask a follow-up in the incident group…')}
        />
      </div>
    </div>
  );
}
