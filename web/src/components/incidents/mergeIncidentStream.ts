// mergeIncidentStream — the only logic in the incident group chat that
// isn't rendering. Keeps events and every diagnosis session's messages
// in one timestamp-ascending stream; rendering just maps over it.
//
// Critical-event whitelist source: the literals actually written to the
// `alert_events` table by /v1/alerts/incidents/{id}/events. Producers:
// core/manager/model/alert/model.go:26-43 (production) and
// core/manager/biz/demo/scenario.go:960-967 (demo scenarios, literals from
// core/manager/model/demo/model.go:6-16). A repo-wide grep of EventType
// constants is NOT sufficient — it misses dotted literals and
// variable-written ones and pulls in unrelated tables (loop_event_log,
// incident_timeline).
import type { IncidentEvent } from '@/api/alerts';
import type { ChatMessage, ChatSession } from '@/api/chat';

export type SessionMessages = { session: ChatSession; messages: ChatMessage[] };

export type StreamItem =
  | { ts: string; type: 'event'; collapsed: boolean; payload: IncidentEvent }
  | {
      ts: string;
      type: 'message';
      collapsed: false;
      payload: ChatMessage & { sessionId: string; agentId?: string | null };
    };

// Status changes, HITL approval gates, recovery/close and the AI diagnosis
// announcement stay expanded; process bookkeeping collapses into one chip.
// Finalised against the real backend literals (see task-21-rulings.md):
// `firing` (not `fired`), plus the dotted `alert.received`, plus `reopened`
// and the demo-scenario literals. The `approval_*` / `recovery_observed` /
// `postmortem_generated` names from the plan draft are NOT alert_events
// literals and were removed — no ghost entries.
const CRITICAL_EVENT_TYPES = new Set([
  // Production path — core/manager/model/alert/model.go:26-43
  'firing',
  'acknowledged',
  'silenced',
  'resolved',
  'reopened',
  'alert.received',
  'ai_initial_diagnosis',
  // Demo scenario path — core/manager/biz/demo/scenario.go:960-967,
  // literals from core/manager/model/demo/model.go:6-16 (same table).
  'recovered',
  'closed',
  'awaiting_approval',
  'start_failed',
]);

export function isCriticalEventType(eventType: string): boolean {
  return CRITICAL_EVENT_TYPES.has(eventType);
}

// Timestamps are RFC3339 and may mix precision (Go omits the fractional part
// when nanoseconds are zero: `…00.500Z` vs `…00Z`) and may carry a non-UTC
// offset (`+08:00`). Comparing the strings lexicographically misorders both,
// so compare instants.
function instant(ts: string): number {
  const t = Date.parse(ts);
  return Number.isNaN(t) ? 0 : t;
}

export function mergeIncidentStream(
  events: IncidentEvent[],
  _sessions: ChatSession[],
  messagesBySession: SessionMessages[],
): StreamItem[] {
  const items: StreamItem[] = events.map((e) => ({
    ts: e.occurred_at,
    type: 'event',
    collapsed: !isCriticalEventType(e.event_type),
    payload: e,
  }));

  for (const { session, messages } of messagesBySession) {
    for (const m of messages) {
      if (!m.created_at) continue;
      if (m.role === 'system') continue;
      items.push({
        ts: m.created_at,
        type: 'message',
        collapsed: false,
        payload: { ...m, sessionId: session.id, agentId: session.agent_id },
      });
    }
  }

  return items.sort((a, b) => instant(a.ts) - instant(b.ts));
}
