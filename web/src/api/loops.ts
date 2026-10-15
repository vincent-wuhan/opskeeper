// Shared closed-loop timeline read. ClosedLoopTimeline keeps its own
// richer rendering; the incident group chat only needs the phase list
// for its progress strip, so both read the same endpoint shape.
import { request } from './client';
import type { TimelinePhase } from '@/components/dpo';

export type LoopTimeline = { phases: TimelinePhase[] };

export function fetchLoopTimeline(incidentId: number) {
  return request<LoopTimeline>('GET', `/loops/${incidentId}/timeline`);
}
