// 白名单来源:后端 EventType 字面量,定稿依据见 task-21-rulings.md §0/§1
// (逐跳核实端点 → `alert_events` 表的写入方)。不要按仓库 grep 结论改写白名单。
import { describe, expect, it } from 'vitest';
import { isCriticalEventType, mergeIncidentStream } from './mergeIncidentStream';
import type { IncidentEvent } from '@/api/alerts';
import type { ChatMessage } from '@/api/chat';

const ev = (event_type: string, occurred_at: string): IncidentEvent => ({
  id: 1, incident_id: 7, event_type, actor_type: 'system', occurred_at, created_at: occurred_at,
});

const msg = (id: string, created_at: string): ChatMessage => ({ id, role: 'assistant', content: 'hi', created_at });

describe('mergeIncidentStream', () => {
  it('sorts events and messages from different sources by timestamp', () => {
    const items = mergeIncidentStream(
      [ev('firing', '2026-10-07T10:00:00Z'), ev('silenced', '2026-10-07T10:02:00Z')],
      [],
      [{ session: { id: 's1', user_id: 1, title: 't' }, messages: [msg('m1', '2026-10-07T10:01:00Z')] }],
    );
    expect(items.map((i) => i.ts)).toEqual([
      '2026-10-07T10:00:00Z',
      '2026-10-07T10:01:00Z',
      '2026-10-07T10:02:00Z',
    ]);
    expect(items[1].type).toBe('message');
  });

  it('marks non-critical events collapsed and status changes expanded', () => {
    const items = mergeIncidentStream(
      [ev('firing', '2026-10-07T10:00:00Z'), ev('repeat_suppressed', '2026-10-07T10:00:30Z')],
      [],
      [],
    );
    expect(items.map((i) => i.collapsed)).toEqual([false, true]);
  });

  it('tags each message with its session and agent', () => {
    const items = mergeIncidentStream([], [{ id: 's9', user_id: 1, title: 't', agent_id: 'critic' }],
      [{ session: { id: 's9', user_id: 1, title: 't', agent_id: 'critic' }, messages: [msg('m1', '2026-10-07T10:00:00Z')] }]);
    const payload = items[0].payload as { sessionId: string; agentId?: string | null };
    expect(payload.sessionId).toBe('s9');
    expect(payload.agentId).toBe('critic');
  });

  it('returns an empty stream when the incident has no events and no sessions', () => {
    expect(mergeIncidentStream([], [], [])).toEqual([]);
  });

  it('treats exactly the alert_events status literals as critical', () => {
    // 白名单来源:alert_events 表写入方的 EventType 字面量。
    // 定稿依据见 task-21-rulings.md §0/§1(逐跳核实端点→表)。
    expect(isCriticalEventType('firing')).toBe(true);
    expect(isCriticalEventType('alert.received')).toBe(true);
    expect(isCriticalEventType('reopened')).toBe(true);
    // 计划初稿里的幽灵名字,必须为 false —— 它们在 alert_events 中不存在
    expect(isCriticalEventType('fired')).toBe(false);
    expect(isCriticalEventType('approval_requested')).toBe(false);
    // 过程性噪声,折叠
    expect(isCriticalEventType('repeat_suppressed')).toBe(false);
  });

  it('orders mixed-precision timestamps by instant, not lexicographically', () => {
    // Go omits the fractional part when nanoseconds are zero, so the same feed
    // can carry both `…00.500Z` and `…00Z`. Lexicographically `.` sorts before
    // `Z`, which would put the later instant first.
    const items = mergeIncidentStream(
      [ev('firing', '2026-10-07T10:00:00.500Z')],
      [],
      [{ session: { id: 's1', user_id: 1, title: 't' }, messages: [msg('m1', '2026-10-07T10:00:00Z')] }],
    );
    expect(items.map((i) => i.type)).toEqual(['message', 'event']);
  });

  it('orders timestamps carrying a non-UTC offset by instant', () => {
    // 10:00+08:00 is 02:00Z — earlier than the 05:00Z message below, even
    // though its hour digits are larger.
    const items = mergeIncidentStream(
      [ev('firing', '2026-10-07T10:00:00+08:00')],
      [],
      [{ session: { id: 's1', user_id: 1, title: 't' }, messages: [msg('m1', '2026-10-07T05:00:00Z')] }],
    );
    expect(items.map((i) => i.type)).toEqual(['event', 'message']);
  });
});
