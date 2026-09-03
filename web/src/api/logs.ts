import { request } from './client';

// Loki streams response: each stream has `stream` (label key/value map)
// and `values` ([[<unix_ns_string>, <line_string>], ...]).
export type LokiStream = {
  stream: Record<string, string>;
  values: [string, string][];
};

export type LokiQueryRangeResponse = {
  resultType: 'streams' | 'matrix';
  // For streams: LokiStream[]. For matrix: same shape as Prom matrix
  // (used by count_over_time / rate). The page renders streams today;
  // matrix support lands when the Logs page grows a "metric mode" tab.
  result: unknown;
  from: string;
  to: string;
};

export function queryLogsRange(params: {
  query: string;
  start: string; // RFC3339 or unix-seconds string
  end: string;
  limit?: number;
  step?: string; // duration string, only meaningful for metric queries
  direction?: 'forward' | 'backward';
}) {
  const qs = new URLSearchParams();
  qs.set('query', params.query);
  qs.set('start', params.start);
  qs.set('end', params.end);
  if (params.limit) qs.set('limit', String(params.limit));
  if (params.step) qs.set('step', params.step);
  if (params.direction) qs.set('direction', params.direction);
  return request<LokiQueryRangeResponse>('GET', `/logs/query_range?${qs.toString()}`);
}

export function listLogLabels(params?: { start?: string; end?: string }) {
  const qs = new URLSearchParams();
  if (params?.start) qs.set('start', params.start);
  if (params?.end) qs.set('end', params.end);
  const suffix = qs.toString() ? `?${qs.toString()}` : '';
  return request<{ labels: string[] }>('GET', `/logs/labels${suffix}`);
}

export function listLogLabelValues(name: string, params?: { start?: string; end?: string }) {
  const qs = new URLSearchParams();
  if (params?.start) qs.set('start', params.start);
  if (params?.end) qs.set('end', params.end);
  const suffix = qs.toString() ? `?${qs.toString()}` : '';
  return request<{ values: string[] }>('GET', `/logs/labels/${encodeURIComponent(name)}/values${suffix}`);
}
