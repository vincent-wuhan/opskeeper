import { request } from './client';

// NL→Query translation. Backend route POST /aiops/query-translate.
// Returns the rendered query (LogQL / TraceQL / PromQL) plus a short
// (≤30 字) Chinese explanation. The endpoint:
//   - 503 when no LLM is configured → callers should hide the entry
//     point rather than showing a disabled button.
//   - 502 on LLM error / translation failure → callers should surface
//     the failure inside the helper popover only (NOT a global toast).
//   - 6s server-side timeout.

export type QueryDialect = 'logql' | 'traceql' | 'promql';

export type TranslateQueryResp = {
  query: string;
  explanation?: string;
  dialect: QueryDialect;
};

export function translateQuery(
  dialect: QueryDialect,
  prompt: string,
  context?: Record<string, unknown>,
) {
  const body: Record<string, unknown> = { dialect, prompt };
  if (context && Object.keys(context).length > 0) body.context = context;
  return request<TranslateQueryResp>('POST', '/aiops/query-translate', body);
}
