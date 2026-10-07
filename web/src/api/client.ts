import { getRefreshToken, getToken, useAuth } from '@/store/auth';
import { getLocale } from '@/i18n/locale';

export class ApiError extends Error {
  status: number;
  code?: string;
  payload?: unknown;
  constructor(message: string, status: number, code?: string, payload?: unknown) {
    super(message);
    this.name = 'ApiError';
    this.status = status;
    this.code = code;
    this.payload = payload;
  }
}

type RequestOpts = {
  signal?: AbortSignal;
  headers?: Record<string, string>;
  noAuth?: boolean;
  _retryingAfterRefresh?: boolean;
};

const BASE = '/api/v1';
let refreshInFlight: Promise<string | null> | null = null;

export async function request<T = unknown>(
  method: 'GET' | 'POST' | 'PUT' | 'DELETE' | 'PATCH',
  path: string,
  body?: unknown,
  opts: RequestOpts = {}
): Promise<T> {
  const headers: Record<string, string> = {
    Accept: 'application/json',
    // Tell the backend which language the operator's UI is in. Used by
    // any LLM-driven endpoint (RCA worker, summarizers, future chat
    // helpers) so the generated text matches the SPA. Convention:
    // feedback_ai_output_locale — AI output language follows UI locale.
    'Accept-Language': getLocale(),
    ...(opts.headers ?? {}),
  };

  if (!opts.noAuth) {
    const token = getToken();
    if (token) headers['Authorization'] = `Bearer ${token}`;
  }

  let payload: BodyInit | undefined;
  if (body !== undefined && body !== null) {
    if (body instanceof FormData) {
      // Let the browser set multipart/form-data + boundary; don't JSON it.
      payload = body;
    } else {
      headers['Content-Type'] = 'application/json';
      payload = JSON.stringify(body);
    }
  }

  const url = path.startsWith('http') ? path : `${BASE}${path.startsWith('/') ? path : `/${path}`}`;

  let res: Response;
  try {
    res = await fetch(url, { method, headers, body: payload, signal: opts.signal });
  } catch (err) {
    if ((err as Error).name === 'AbortError') throw err;
    throw new ApiError((err as Error).message || 'Network error', 0);
  }

  const ct = res.headers.get('content-type') ?? '';
  let parsed: unknown = null;
  if (ct.includes('application/json')) {
    try {
      parsed = await res.json();
    } catch {
      parsed = null;
    }
  } else {
    const text = await res.text();
    parsed = text ? text : null;
  }

  if (!res.ok) {
    let msg = `HTTP ${res.status}`;
    let code: string | undefined;
    if (parsed && typeof parsed === 'object') {
      const { message, code: errCode } = readErrorEnvelope(parsed);
      if (message) msg = message;
      code = errCode;
    } else if (typeof parsed === 'string' && parsed.trim()) {
      // Plain-text body (e.g. handlers calling http.Error). Surface a
      // trimmed slice so the UI can show actionable detail like
      // "llm: context deadline exceeded" instead of a bare "HTTP 502".
      const text = parsed.trim();
      msg = `HTTP ${res.status}: ${text.length > 200 ? text.slice(0, 200) + '…' : text}`;
    }
    if (res.status === 401 && !opts.noAuth) {
      const nextToken = await refreshAccessToken();
      if (nextToken && !opts._retryingAfterRefresh) {
        return request<T>(method, path, body, { ...opts, _retryingAfterRefresh: true });
      }
      // Only force-logout when refresh itself failed — that's the
      // authoritative "session expired" signal. A retry that still
      // 401s (with a fresh token) is some other server-side issue
      // (one buggy route, casbin policy mismatch, missing membership)
      // and should surface the error rather than booting the user.
      if (!nextToken) {
        useAuth.getState().logout();
      }
    }
    throw new ApiError(msg, res.status, code, parsed);
  }

  return parsed as T;
}

/** readErrorEnvelope pulls the message and code out of an error body.
 *
 *  The control plane writes failures in two shapes and both are live on the
 *  wire. Most handlers emit a flat `{"error": "...", "code": "..."}`
 *  (marketplace, skill, monitor, alert), while the two written for 2.0 —
 *  nodeagent's writeErr and plugin's — emit a nested
 *  `{"error": {"message": "...", "code": "..."}}`.
 *
 *  Reading only the flat shape is not a cosmetic gap. The code is what tells
 *  a page which refusal it is looking at, and pages branch on it:
 *  Tasks matches `not-wired-yet`, Marketplace matches `invalid-argument`,
 *  and the node-agent page matches `conversation_limit` and `not_streaming`
 *  — the last of which is the difference between "close a conversation" and
 *  "try again", advice that sends an operator to do the one thing that
 *  cannot help. With the nested body unread those branches all fall
 *  through to a bare `HTTP 429`, and every one of them is wrong.
 *
 *  Flat is checked first because it is the older and more common shape, and
 *  a body carrying both would be a handler bug rather than a shape to
 *  guess about. */
function readErrorEnvelope(parsed: unknown): { message?: string; code?: string } {
  const obj = parsed as Record<string, unknown>;
  if (typeof obj.error === 'string') {
    return {
      message: obj.error,
      code: typeof obj.code === 'string' ? obj.code : undefined,
    };
  }
  if (obj.error && typeof obj.error === 'object') {
    const inner = obj.error as Record<string, unknown>;
    return {
      message: typeof inner.message === 'string' ? inner.message : undefined,
      code: typeof inner.code === 'string' ? inner.code : undefined,
    };
  }
  return {
    message: typeof obj.message === 'string' ? obj.message : undefined,
    code: typeof obj.code === 'string' ? obj.code : undefined,
  };
}

async function refreshAccessToken(): Promise<string | null> {
  if (refreshInFlight) return refreshInFlight;

  refreshInFlight = (async () => {
    const refreshToken = getRefreshToken();
    if (!refreshToken) return null;

    const res = await fetch(`${BASE}/auth/refresh`, {
      method: 'POST',
      headers: {
        Accept: 'application/json',
        'Content-Type': 'application/json',
      },
      body: JSON.stringify({ refresh_token: refreshToken }),
    });

    if (!res.ok) {
      return null;
    }

    const parsed = (await res.json()) as {
      access_token?: string;
      refresh_token?: string;
      role?: string;
    } | null;

    if (!parsed?.access_token) {
      return null;
    }

    const current = useAuth.getState();
    current.setSession({
      access_token: parsed.access_token,
      refresh_token: parsed.refresh_token ?? refreshToken,
      role: parsed.role ?? current.role ?? 'user',
      email: current.email ?? '',
    });
    return parsed.access_token;
  })()
    .catch(() => null)
    .finally(() => {
      refreshInFlight = null;
    });

  return refreshInFlight;
}
