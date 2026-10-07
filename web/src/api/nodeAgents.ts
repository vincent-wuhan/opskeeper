// Node agent API client — talks to /v1/node-agents/*.
//
// A node agent is a `pig --mode rpc` process running on an edge, reached
// through the control plane rather than beside it. That changes three
// things about how the console drives it, and all three are visible in the
// signatures below.
//
// The first is that opening a conversation is a round trip that can be
// refused, and the reason it can be refused is worth a screen of its own.
// Every edge holds a bounded number of conversations (the fleet's cap), so
// `openSession` can answer 429 with code `conversation_limit`. A console
// that renders that as "try again later" is lying: the conversations
// already open are not going to close themselves, and the operator's next
// action is to close one. ApiError carries the `code` through so the page
// can say which of those it is.
//
// The second is that sending a message is accepted, not answered. The
// reply is a stream on a *different* endpoint, and that endpoint is a GET
// that must be attached BEFORE the message goes out — the manager refuses
// a send on a conversation nobody is listening to (409 `not_streaming`).
// So the page's order is attach-then-send, and `attachStream` exists as a
// separate call rather than a convenience wrapper around the two because
// the order is the contract, not an implementation detail of this client.
//
// The third is that the frames arriving on that stream are the console's
// existing stream vocabulary — assistant_start, assistant_delta, tool_start,
// tool_end, approval_pending, done, error — because the translation to it
// happens on the node. A node agent therefore renders with the same bubble
// and the same tool tiles as a control-plane conversation. The types below
// are duplicated from @/api/chat on purpose rather than imported: that
// module's events describe a POST-then-stream round trip with its own
// envelope, and sharing the type would hide the one field that differs.
import { ApiError, request } from '@/api/client';
import { getToken } from '@/store/auth';

// ---------------------------------------------------------------------------
// State and health
// ---------------------------------------------------------------------------

/** NodeState is ports.ProcessState: what the node's agent is doing right
 *  now. `running` is true while a turn is in flight; `model`/`provider`
 *  are the ones the answer actually came from rather than the defaults, so
 *  a console can show what answered. */
export interface NodeState {
  session_id?: string;
  running: boolean;
  model?: string;
  provider?: string;
  version?: string;
  pending_tool_calls?: number;
}

/** NodeHealth is tunnel.AgentHealthResponse: what the node's supervisor
 *  sees, which is not the same question as what the agent is doing. A
 *  process can be running and still crash-looping, and `degraded` with a
 *  non-zero `restarts` is how that looks before it becomes an outage.
 *  `last_error` is carried verbatim from the node because "plugin manifest
 *  not found" tells an operator what to fix and "agent unavailable" does
 *  not. */
export interface NodeHealth {
  running: boolean;
  degraded: boolean;
  restarts: number;
  version?: string;
  last_error?: string;
  last_start_at?: string;
}

// ---------------------------------------------------------------------------
// Conversations
// ---------------------------------------------------------------------------

export interface OpenSessionRequest {
  edge_id: number;
  session_id?: string;
  role?: string;
  locale?: string;
  provider?: string;
  model?: string;
}

export interface OpenSessionResponse {
  session_id: string;
  edge_id: number;
}

/** SessionStat is one open conversation.
 *
 *  `dropped` is the reason this endpoint exists at all. The stream applies
 *  backpressure by discarding frames when a consumer falls behind, which
 *  is the right thing to do to a conversation nobody is reading and the
 *  wrong thing to do silently. An operator looking at a transcript with
 *  holes in it needs to be able to tell "the transport dropped these" from
 *  "the agent stopped speaking", and this counter is the only place that
 *  answer exists. A console that renders the transcript without it is
 *  showing a story it cannot vouch for. */
export interface SessionStat {
  session_id: string;
  edge_id: number;
  frames: number;
  dropped: number;
  attached: boolean;
  terminal: boolean;
}

export async function listSessions(): Promise<SessionStat[]> {
  const r = await request<{ sessions?: SessionStat[] }>('GET', '/node-agents/sessions');
  return r.sessions ?? [];
}

export function openSession(body: OpenSessionRequest): Promise<OpenSessionResponse> {
  return request<OpenSessionResponse>('POST', '/node-agents/sessions', body);
}

/** sendMessage starts a turn, or steers the one already running.
 *
 *  `steer` is a separate boolean rather than a second endpoint because it
 *  is the same action with a different intent, and the backend keeps them
 *  apart for a reason worth repeating here: steering injects into the
 *  investigation in flight, which is an operator correcting the agent
 *  mid-thought. Expressing that as an accidental second prompt would make
 *  the most dangerous form of it — two prompts racing on one turn — the
 *  easiest to perform by accident. */
export function sendMessage(
  sessionId: string,
  content: string,
  steer = false
): Promise<{ status: string }> {
  return request<{ status: string }>(
    'POST',
    `/node-agents/sessions/${encodeURIComponent(sessionId)}/messages`,
    { content, steer }
  );
}

export function stopSession(sessionId: string): Promise<{ status: string }> {
  return request<{ status: string }>(
    'POST',
    `/node-agents/sessions/${encodeURIComponent(sessionId)}/stop`
  );
}

export function closeSession(sessionId: string): Promise<{ status: string }> {
  return request<{ status: string }>(
    'DELETE',
    `/node-agents/sessions/${encodeURIComponent(sessionId)}`
  );
}

export interface ApprovalDecision {
  request_id: string;
  /** digest is echoed from the approval_pending frame. The node recomputes
   *  it and refuses a decision that does not match the request it names, so
   *  a console cannot authorise a different call by answering a stale
   *  prompt. Omitting it is not a weaker decision; it is a rejected one. */
  digest: string;
  grant: boolean;
  note?: string;
}

export function decideApproval(
  sessionId: string,
  requestId: string,
  decision: Omit<ApprovalDecision, 'request_id'>
): Promise<{ status: string }> {
  return request<{ status: string }>(
    'POST',
    `/node-agents/sessions/${encodeURIComponent(sessionId)}/approvals/${encodeURIComponent(
      requestId
    )}/decide`,
    { ...decision, request_id: requestId }
  );
}

export function nodeState(edgeId: number): Promise<NodeState> {
  return request<NodeState>('GET', `/node-agents/${edgeId}/state`);
}

export function nodeHealth(edgeId: number): Promise<NodeHealth> {
  return request<NodeHealth>('GET', `/node-agents/${edgeId}/health`);
}

// ---------------------------------------------------------------------------
// Stream
// ---------------------------------------------------------------------------

/** ToolFrame mirrors wire.ToolFrame. `status` distinguishes `blocked` from
 *  `error` because the host policy gate refusing a call is not a failure
 *  of the call — the console shows the two differently, and collapsing
 *  them would make a working safety feature look like a bug. */
export interface ToolFrame {
  tool_call_id: string;
  name: string;
  status?: 'pending' | 'success' | 'error' | 'timeout' | 'blocked';
  class?: string;
  device_id?: number;
  args_json?: string;
  result_json?: string;
  error?: string;
  started_at?: string;
  ended_at?: string;
  duration_ms?: number;
}

export interface AssistantFrame {
  content: string;
  message_id?: string;
  pending_tool_calls?: number;
  created_at?: string;
}

export interface ApprovalFrame {
  request_id: string;
  digest: string;
  tool: string;
  class?: string;
  summary?: string;
  blast_radius?: string;
  target?: string;
}

export interface DoneFrame {
  iterations: number;
  tool_calls: number;
  usage?: {
    input_tokens: number;
    output_tokens: number;
    cache_read_tokens?: number;
    cost_usd?: number;
    model?: string;
  };
}

export interface ErrorFrame {
  code: string;
  message: string;
  retryable?: boolean;
}

/** StreamFrame is the envelope. The console keys on `type` and reads one
 *  body field, so an unknown type renders as nothing rather than as a
 *  crash — the agent's vocabulary is PiG's and it moves. */
export interface StreamFrame {
  type: string;
  session_id?: string;
  iteration?: number;
  seq?: number;
  assistant?: AssistantFrame;
  tool?: ToolFrame;
  done?: DoneFrame;
  error?: ErrorFrame;
  approval?: ApprovalFrame;
}

export interface StreamHandlers {
  onFrame?: (frame: StreamFrame) => void;
  onOpen?: () => void;
  onError?: (err: Error) => void;
  onClose?: () => void;
}

/** attachStream opens the conversation's SSE stream.
 *
 *  It is a GET carrying a bearer token rather than an EventSource, because
 *  EventSource cannot set an Authorization header. The cost of that is
 *  that reconnection is manual: a console that wants to survive a proxy
 *  dropping an idle connection has to call this again, and it should —
 *  the manager's stream is a live subscription, not a replay, so
 *  reconnecting does not duplicate frames the way a resumed download
 *  would. It reattaches to whatever is happening now.
 *
 *  The promise resolves when the stream closes, which for a long-lived
 *  conversation is when the operator navigates away or the node's turn
 *  goes terminal. It does not reject on a normal close. */
export async function attachStream(
  sessionId: string,
  handlers: StreamHandlers,
  signal?: AbortSignal
): Promise<void> {
  const url = `/api/v1/node-agents/sessions/${encodeURIComponent(sessionId)}/stream`;
  const headers: Record<string, string> = { Accept: 'text/event-stream' };
  const token = getToken();
  if (token) headers['Authorization'] = `Bearer ${token}`;

  // An already-aborted signal means the console navigated away between
  // asking to attach and the request going out. Opening a stream nobody
  // will read is the one outcome worth avoiding here: the manager counts
  // an attached console, and a phantom attachment makes a later send look
  // answered when it is being dropped on the floor.
  if (signal?.aborted) return;

  let res: Response;
  try {
    res = await fetch(url, { headers });
  } catch (err) {
    if ((err as Error).name === 'AbortError') return;
    const wrapped = new ApiError((err as Error).message || 'Network error', 0);
    handlers.onError?.(wrapped);
    return;
  }

  if (!res.ok || !res.body) {
    let parsed: unknown = null;
    try {
      parsed = await res.json();
    } catch {
      parsed = null;
    }
    let msg = `HTTP ${res.status}`;
    let code: string | undefined;
    if (parsed && typeof parsed === 'object') {
      const obj = parsed as Record<string, unknown>;
      const errObj = obj.error;
      if (errObj && typeof errObj === 'object') {
        const e = errObj as Record<string, unknown>;
        if (typeof e.message === 'string') msg = e.message;
        if (typeof e.code === 'string') code = e.code;
      }
    }
    handlers.onError?.(new ApiError(msg, res.status, code, parsed));
    return;
  }

  handlers.onOpen?.();

  const reader = res.body.getReader();
  const decoder = new TextDecoder();
  let buf = '';

  // The abort is wired to the READER, not to the fetch.
  //
  // Passing the signal to fetch() is the usual idiom and it is the wrong
  // one here. By the time this runs the response has arrived and the only
  // thing left to stop is the body stream, so cancelling the reader is the
  // precise instrument: it resolves the pending read immediately and tears
  // the connection down, where an abort on the Request is a heavier hammer
  // aimed at a phase the request has already left. It also keeps the
  // signal from crossing into fetch, which matters under test — jsdom's
  // AbortSignal and the fetch implementation's are different objects and
  // the latter refuses the former outright.
  const onAbort = () => {
    void reader.cancel().catch(() => undefined);
  };
  if (signal) {
    if (signal.aborted) onAbort();
    else signal.addEventListener('abort', onAbort, { once: true });
  }

  try {
    for (;;) {
      const { value, done } = await reader.read();
      if (done) break;
      buf += decoder.decode(value, { stream: true });

      // Frames are separated by a blank line. The server's first write is
      // a bare ": ok" comment, which carries no data line and is dropped
      // here — that comment exists to prove the connection is alive, and
      // treating it as a frame would render an empty bubble.
      let sep: number;
      while ((sep = buf.indexOf('\n\n')) >= 0) {
        const raw = buf.slice(0, sep);
        buf = buf.slice(sep + 2);
        const frame = parseFrame(raw);
        if (frame) handlers.onFrame?.(frame);
      }
    }
    if (buf.trim()) {
      const frame = parseFrame(buf);
      if (frame) handlers.onFrame?.(frame);
    }
  } catch (err) {
    if ((err as Error).name !== 'AbortError') {
      handlers.onError?.(new ApiError((err as Error).message || 'Stream failed', 0));
    }
  } finally {
    signal?.removeEventListener('abort', onAbort);
    handlers.onClose?.();
  }
}

function parseFrame(raw: string): StreamFrame | null {
  const dataLines: string[] = [];
  for (const line of raw.split('\n')) {
    if (!line || line.startsWith(':')) continue;
    if (line.startsWith('data:')) dataLines.push(line.slice(5).trim());
  }
  if (dataLines.length === 0) return null;
  try {
    return JSON.parse(dataLines.join('\n')) as StreamFrame;
  } catch {
    // A frame the server could not encode arrives as a well-formed error
    // body rather than as a parse failure here. Losing it would show the
    // operator a gap they cannot explain, and the gap is the symptom.
    return {
      type: 'error',
      error: { code: 'bad_frame', message: 'a frame from the node could not be read' },
    };
  }
}
