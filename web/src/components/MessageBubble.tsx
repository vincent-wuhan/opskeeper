import { useState, useEffect } from 'react';
import type { Element } from 'hast';
import ReactMarkdown from 'react-markdown';
import remarkGfm from 'remark-gfm';
import { Wrench, ChevronDown, ChevronRight, Loader2, AlertCircle, CheckCircle2, ShieldAlert, Check, X, XCircle, Hourglass } from 'lucide-react';
import type { ChatMessage, ToolCallSummary } from '@/api/chat';
import { approveApproval, rejectApproval, getApproval } from '@/api/approvals';
import { cn } from '@/lib/cn';
import { isConfigDraftConfirmationMessage } from '@/lib/configDraftConfirmation';
import { parseSigners } from '@/lib/approvalSigners';
import { useI18n } from '@/i18n/locale';
import { useApprovalBadge } from '@/store/approvalBadge';
import { personaLabel } from '@/components/AgentBadge';
import { useAgents, avatarFor } from '@/store/agents';
import { AgentAvatar } from './AgentAvatar';
import { DeliverableCard, DeliverableSequence, matchDeliverable } from './DeliverableCard';
import { Button, Chip } from '@/components/ui';
import { blastRadiusLabel, riskClassLabel, riskClassTone } from '@/lib/riskClass';

export type ConfigDraftResult = {
  kind: 'config_draft';
  domain?: string;
  action?: string;
  summary?: string;
  target?: { id?: number; name?: string; type?: string; existing?: boolean };
  payload?: unknown;
  preview?: unknown;
  diff?: unknown;
  warnings?: string[];
  scope?: { type?: string; label?: string; reason?: string; change_hint?: string };
  confirmation_prompt?: string;
  rollback?: string;
  apply_tool?: string;
  draft_hash?: string;
};

type ConfirmConfigDraft = (draft: ConfigDraftResult) => boolean | void | Promise<boolean | void>;

type Props = {
  message: ChatMessage;
  onConfirmConfigDraft?: ConfirmConfigDraft;
};

// MessageBubble additionally takes the session's pinned persona so the
// assistant side can render the messenger-style avatar + name head row.
// Only AssistantBubble consumes it; user/tool rows keep their signatures.
type MessageBubbleProps = Props & { agentId?: string | null };

export function MessageBubble({ message, agentId, onConfirmConfigDraft }: MessageBubbleProps) {
  if (message.kind === 'tool_card' && message.tool_call) {
    return <ToolCallSummaryBlock call={fromSummary(message.tool_call)} onConfirmConfigDraft={onConfirmConfigDraft} />;
  }
  if (message.role === 'tool') return <ToolBubble message={message} onConfirmConfigDraft={onConfirmConfigDraft} />;
  if (message.role === 'user') return <UserBubble message={message} />;
  // Tool-only assistant rows (empty content + has tool_calls) shouldn't
  // appear during streaming; on history reload they would, so suppress.
  if (
    message.role === 'assistant' &&
    (!message.content || message.content.length === 0) &&
    !message.pending
  ) {
    return null;
  }
  return <AssistantBubble message={message} agentId={agentId} onConfirmConfigDraft={onConfirmConfigDraft} />;
}

// fromSummary maps the wire-level ToolCallSummary (server SSE shape) to
// the {arguments,result,...} shape the rich card already understands.
function fromSummary(tc: ToolCallSummary) {
  const args = tc.arguments ?? (tc.arguments_raw ? safeParse(tc.arguments_raw) : undefined);
  const result = tc.result ?? (tc.result_raw ? safeParse(tc.result_raw) : undefined);
  return {
    name: tc.name,
    device_id: tc.device_id,
    status: tc.status,
    duration_ms: tc.duration_ms,
    error: tc.error,
    arguments: args as Record<string, unknown> | undefined,
    result,
  };
}

function safeParse(s: string): unknown {
  try {
    return JSON.parse(s);
  } catch {
    return s;
  }
}

function UserBubble({ message }: Props) {
  const { tr } = useI18n();
  const content = compactUserContent(message.content ?? '', tr);

  // Codex-style: small, compact chip pinned right. `.bubble-user` (Task 8)
  // carries the accent background/color/border — zinc bg/text/ring must
  // NOT come back here or utilities would repaint the accent bubble.
  return (
    <div className="flex justify-end">
      <div className="bubble-user max-w-[78%] rounded-2xl rounded-br-md px-4 py-2.5 text-[14px] leading-relaxed">
        {content}
      </div>
    </div>
  );
}

function compactUserContent(
  content: string,
  tr: (zh: string, en: string) => string,
): string {
  if (!isConfigDraftConfirmationMessage(content)) return content;
  return tr('确认创建这条告警规则', 'Confirm creating this alert rule');
}

// react-markdown 把 `components` 里的内联覆盖当作普通组件塞进树里,真正的
// `<DeliverableCard/>` 要等渲染期才展开 —— 所以 p 覆盖拿到的 children 里是 `a`
// 覆盖函数本身,按组件类型认卡是认不出来的。改读 markdown 节点:在子树里找 href
// 能匹配的 <a>,用同一个 matchDeliverable 判定。这样「这个段落会产出交付物卡」与
// a 覆盖的判定条件是同一段代码,两者永远不会漂移。
// 必须递归:卡链接常被行内元素包着(`**[查看报表](…)**` → <strong>、
// `*看[这里](…)*` → <em>),只看直接子节点会漏,漏判就会渲染出非法的
// <p><strong><span card><span preview><div>。
// react-markdown 传了 passNode,所以 p 覆盖拿得到原始 hast 节点;用 hast 自己的
// Element 类型,不做手写窄类型 + 断言 —— 后者在 hast 形状变化时会静默判 false,
// 正好退化成要避免的那棵树。
function hasDeliverableLink(node: Element | undefined): boolean {
  if (!node) return false;
  // 深度优先遍历整棵子树。普通 markdown 嵌套很浅,不做深度上限。
  for (const child of node.children) {
    if (child.type !== 'element') continue; // 文本节点没有 children
    const href = child.properties?.href;
    if (child.tagName === 'a' && matchDeliverable(typeof href === 'string' ? href : '') !== null) return true;
    if (hasDeliverableLink(child)) return true;
  }
  return false;
}

function AssistantBubble({ message, agentId, onConfirmConfigDraft }: Props & { agentId?: string | null }) {
  const { tr } = useI18n();
  const byName = useAgents((s) => s.byName);
  // Messenger-style: persona avatar + name/time head row on the left, prose
  // in a rounded `.bubble-agent` bubble. When the session has no pinned
  // persona (agentId falsy) the avatar row is dropped and the bubble still
  // renders — a default conversation reads as plain content, not a
  // "默认助理" row on every turn. Preserved from the doc-style form: the
  // pending branch (loading dots instead of empty markdown), the `md-body`
  // wrapper (markdown typography), and the tool_calls map below the bubble.
  return (
    <div className="flex w-full items-start gap-2.5">
      {agentId ? (
        <AgentAvatar agentId={agentId} size={32} className="mt-0.5" avatar={avatarFor(byName, agentId)} />
      ) : null}
      <div className="min-w-0 max-w-[78%] space-y-1">
        {agentId ? (
          <div className="flex items-center gap-2 text-[11px] text-zinc-500">
            <span className="text-zinc-400">{personaLabel(agentId, tr)}</span>
            {message.created_at ? <span>{message.created_at.slice(11, 16)}</span> : null}
          </div>
        ) : null}
        <div className="bubble-agent rounded-2xl rounded-bl-md px-4 py-2.5">
          {message.pending ? (
            <span className="text-zinc-500">
              <PendingDots />
            </span>
          ) : (
            <div className="md-body text-zinc-100">
              <DeliverableSequence>
                <ReactMarkdown
                  remarkPlugins={[remarkGfm]}
                  components={{
                    a: ({ href, children }) => {
                      const info = href ? matchDeliverable(href) : null;
                      if (info) return <DeliverableCard info={info} />;
                      // Non-deliverable links keep their pre-refactor behavior verbatim —
                      // same markup, no target attr.
                      return <a href={href}>{children}</a>;
                    },
                    // 交付物卡卡内报表预览是成片的块级内容(<div>),而卡整体被上面的 a
                    // 覆盖塞进 ReactMarkdown 的 <p> 里。不换掉这个 <p>,真实 DOM 就是
                    // <p><span><div>,非法嵌套。判定见 hasDeliverableLink。
                    // 不带卡的段落仍是真正的 <p>,markdown 排版不受影响。
                    p: ({ node, children }) =>
                      hasDeliverableLink(node) ? (
                        <div className="md-p-card">{children}</div>
                      ) : (
                        <p>{children}</p>
                      ),
                  }}
                >
                  {message.content}
                </ReactMarkdown>
              </DeliverableSequence>
            </div>
          )}
        </div>
        {message.tool_calls?.map((tc, i) => (
          <ToolCallSummaryBlock key={`${tc.name}-${i}`} call={tc} onConfirmConfigDraft={onConfirmConfigDraft} />
        ))}
      </div>
    </div>
  );
}

function ToolBubble({ message, onConfirmConfigDraft }: Props) {
  // History-reload path: the message persisted by the agent loop only
  // carries the tool name + JSON result string. We don't have args for
  // these (would need to join chat_tool_calls); show what we have.
  const result = message.content ? safeParse(message.content) : undefined;
  return (
    <ToolCallSummaryBlock
      onConfirmConfigDraft={onConfirmConfigDraft}
      call={{
        name: message.tool_name ?? 'tool',
        status: 'success',
        result,
      }}
    />
  );
}

function ToolCallSummaryBlock({
  call,
  onConfirmConfigDraft,
}: {
  call: {
    name: string;
    device_id?: number;
    status?: string;
    arguments?: Record<string, unknown> | unknown;
    result?: unknown;
    duration_ms?: number;
    error?: string;
  };
  onConfirmConfigDraft?: ConfirmConfigDraft;
}) {
  const { tr } = useI18n();
  const [open, setOpen] = useState(false);
  const status = call.status ?? (call.error ? 'error' : 'success');
  const isPending = status === 'pending';
  const isError = status === 'error' || status === 'timeout' || !!call.error;
  const hint = argSummary(call.arguments);
  // Inline approval (HLD-017): a cloud_bash / MCP tool result that returned
  // pending_approval renders an in-conversation 批准/拒绝 card instead of a
  // plain result blob — the human confirms right here, no inbox detour.
  const approval = pendingApproval(call.result);
  if (approval) {
    return <PendingApprovalCard approvalID={approval.id} kind={approval.kind} command={argCommandText(call.arguments)} />;
  }
  const configDraft = !isError ? asConfigDraft(call.result) : null;
  return (
    <div className="flex w-full flex-col gap-1">
      <button
        type="button"
        onClick={() => setOpen((v) => !v)}
        aria-label={tr(`工具调用 ${call.name}`, `Tool call ${call.name}`)}
        className={cn(
          'flex w-full items-center gap-2 rounded-rk-md border border-zinc-800 bg-zinc-900/60 px-3 py-2 text-left text-xs text-zinc-300 hover:bg-zinc-800/40',
          isError && 'ring-1 ring-red-500/30',
        )}
      >
        <StatusIcon status={status} />
        <Wrench size={12} className="text-zinc-500" />
        <span className="font-medium text-zinc-200">{call.name}</span>
        {hint && (
          <span className="truncate text-[11px] text-zinc-500" title={hint}>
            {hint}
          </span>
        )}
        {typeof call.device_id === 'number' && (
          <span className="rounded bg-zinc-800 px-1.5 py-0.5 text-[10px] text-zinc-400">
            edge#{call.device_id}
          </span>
        )}
        <span className="ml-auto flex items-center gap-2 text-[11px] text-zinc-500">
          {typeof call.duration_ms === 'number' && call.duration_ms > 0 && (
            <span>{formatDuration(call.duration_ms)}</span>
          )}
          {isPending && <span className="text-blue-400">{tr('运行中', 'Running')}</span>}
          {isError && <span className="text-red-400">{tr('失败', 'Failed')}</span>}
          {open ? (
            <ChevronDown size={13} className="text-zinc-500" />
          ) : (
            <ChevronRight size={13} className="text-zinc-500" />
          )}
        </span>
      </button>
      {configDraft && (
        <ConfigDraftCard draft={configDraft} onConfirm={onConfirmConfigDraft} />
      )}
      {open && (
        <div className="rounded-rk-sm bg-zinc-950/60 p-3 text-xs">
          {call.arguments !== undefined && (
            <div className="mb-2">
              <div className="mb-1 text-[10px] uppercase tracking-wide text-zinc-500">{tr('参数', 'Arguments')}</div>
              <pre className="max-h-48 overflow-auto text-[11px] leading-5 text-zinc-300">
                {typeof call.arguments === 'string'
                  ? call.arguments
                  : JSON.stringify(call.arguments, null, 2)}
              </pre>
            </div>
          )}
          {call.result !== undefined && (
            <div>
              <div className="mb-1 text-[10px] uppercase tracking-wide text-zinc-500">{tr('结果', 'Result')}</div>
              <pre className="max-h-72 overflow-auto text-[11px] leading-5 text-zinc-300">
                {typeof call.result === 'string'
                  ? call.result
                  : JSON.stringify(call.result, null, 2)}
              </pre>
            </div>
          )}
          {call.error && (
            <div className="mt-1 text-[11px] text-red-400">{call.error}</div>
          )}
          {!call.error && call.result === undefined && isPending && (
            <div className="text-[11px] text-zinc-500">{tr('等待结果…', 'Waiting for result…')}</div>
          )}
        </div>
      )}
    </div>
  );
}

function ConfigDraftCard({
  draft,
  onConfirm,
}: {
  draft: ConfigDraftResult;
  onConfirm?: ConfirmConfigDraft;
}) {
  const { tr } = useI18n();
  const [state, setState] = useState<'idle' | 'submitting' | 'confirmed' | 'cancelled'>('idle');
  const preview = previewSummary(draft.preview);
  const warnings = Array.isArray(draft.warnings) ? draft.warnings.filter(Boolean) : [];
  const payload = payloadSummary(draft.payload);
  const scope = scopeSummary(draft.scope, tr, !draft.confirmation_prompt);
  const disabled = state !== 'idle' || !onConfirm;

  return (
    <div className="border-t border-zinc-800/80 bg-zinc-950/30 px-3 py-3">
      <div className="flex flex-col gap-3 sm:flex-row sm:items-start sm:justify-between">
        <div className="min-w-0 space-y-2">
          <div className="flex flex-wrap items-center gap-2">
            <span className="rounded bg-zinc-800 px-1.5 py-0.5 text-[10px] font-medium text-zinc-300">
              {domainLabel(draft.domain, tr)}
            </span>
            {draft.action && (
              <span className="text-[11px] uppercase text-zinc-500">{draft.action}</span>
            )}
            {draft.target?.name && (
              <span className="truncate text-[11px] text-zinc-500">{draft.target.name}</span>
            )}
          </div>
          <div className="text-sm font-medium text-zinc-100">
            {draft.summary || tr('配置草案', 'Configuration draft')}
          </div>
          {scope && <div className="text-[11px] leading-5 text-zinc-300">{scope}</div>}
          {draft.confirmation_prompt && (
            <div className="text-[11px] leading-5 text-zinc-400">{draft.confirmation_prompt}</div>
          )}
          {payload && <div className="text-[11px] leading-5 text-zinc-400">{payload}</div>}
          {preview && <div className="text-[11px] leading-5 text-zinc-400">{preview}</div>}
          {warnings.length > 0 && (
            <div className="space-y-1 text-[11px] leading-5 text-amber-300">
              {warnings.slice(0, 3).map((w, i) => (
                <div key={`${w}-${i}`}>{w}</div>
              ))}
            </div>
          )}
          {draft.rollback && (
            <div className="text-[11px] leading-5 text-zinc-500">{draft.rollback}</div>
          )}
        </div>
        <div className="flex shrink-0 items-center gap-2">
          <Button
            variant="primary"
            disabled={disabled}
            onClick={async () => {
              if (!onConfirm) return;
              setState('submitting');
              try {
                const ok = await onConfirm(draft);
                setState(ok === false ? 'idle' : 'confirmed');
              } catch {
                setState('idle');
              }
            }}
          >
            <CheckCircle2 size={13} />
            {state === 'confirmed'
              ? tr('已确认', 'Confirmed')
              : state === 'submitting'
                ? tr('应用中', 'Applying')
                : tr('确认应用', 'Apply')}
          </Button>
          <Button
            variant="ghost"
            disabled={state !== 'idle'}
            onClick={() => setState('cancelled')}
          >
            <XCircle size={13} />
            {state === 'cancelled' ? tr('已取消', 'Cancelled') : tr('取消', 'Cancel')}
          </Button>
        </div>
      </div>
    </div>
  );
}

function asConfigDraft(result: unknown): ConfigDraftResult | null {
  const value = typeof result === 'string' ? safeParse(result) : result;
  if (!value || typeof value !== 'object') return null;
  const obj = value as Record<string, unknown>;
  if (obj.kind !== 'config_draft') return null;
  if (obj.domain !== 'alert_rule') return null;
  return obj as ConfigDraftResult;
}

function domainLabel(domain: string | undefined, tr: (zh: string, en: string) => string): string {
  switch (domain) {
    case 'alert_rule':
      return tr('告警规则', 'Alert rule');
    default:
      return tr('配置', 'Config');
  }
}

function previewSummary(preview: unknown): string {
  if (!preview || typeof preview !== 'object') return '';
  const p = preview as Record<string, unknown>;
  if (typeof p.skipped_reason === 'string' && p.skipped_reason) return p.skipped_reason;
  if (typeof p.fire_count === 'number') {
    const unit = typeof p.unit === 'string' && p.unit ? ` ${p.unit}` : '';
    return `Preview fire_count=${p.fire_count}${unit}`;
  }
  return '';
}

function scopeSummary(
  scope: ConfigDraftResult['scope'] | undefined,
  tr: (zh: string, en: string) => string,
  includeHint: boolean,
): string {
  if (!scope) return '';
  const label = typeof scope.label === 'string' ? scope.label.trim() : '';
  const type = typeof scope.type === 'string' ? scope.type.trim() : '';
  const scopeText = label || type;
  if (!scopeText) return '';
  const hint = typeof scope.change_hint === 'string' ? scope.change_hint.trim() : '';
  return includeHint && hint
    ? `${tr('范围：', 'Scope: ')}${scopeText} · ${hint}`
    : `${tr('范围：', 'Scope: ')}${scopeText}`;
}

function payloadSummary(payload: unknown): string {
  if (!payload || typeof payload !== 'object') return '';
  const obj = payload as Record<string, unknown>;
  const rows: string[] = [];
  for (const [key, value] of Object.entries(obj)) {
    if (rows.length >= 4) break;
    if (value === null || value === undefined || value === '') continue;
    if (typeof value === 'string' || typeof value === 'number' || typeof value === 'boolean') {
      rows.push(`${key}: ${String(value)}`);
      continue;
    }
    if (typeof value === 'object') {
      const nested = Object.entries(value as Record<string, unknown>)
        .filter(([, v]) => v !== null && v !== undefined && v !== '')
        .slice(0, 4)
        .map(([k, v]) => `${k}: ${String(v)}`);
      if (nested.length > 0) rows.push(nested.join(' · '));
    }
  }
  return rows.join(' · ');
}

function StatusIcon({ status }: { status?: string }) {
  if (status === 'pending') {
    return <Loader2 size={13} className="animate-spin text-blue-400" />;
  }
  if (status === 'error' || status === 'timeout') {
    return <AlertCircle size={13} className="text-red-400" />;
  }
  return <CheckCircle2 size={13} className="text-emerald-400" />;
}

// argSummary picks a compact one-line preview from the arguments object.
// Most builtin skills have a single load-bearing field (query, host,
// path, ...) — show that. Falls back to the first scalar value.
function argSummary(args: unknown): string {
  if (!args || typeof args !== 'object') return '';
  const obj = args as Record<string, unknown>;
  const preferred = ['query', 'host', 'url', 'path', 'unit', 'expr', 'instance', 'device_id'];
  for (const k of preferred) {
    const v = obj[k];
    if (typeof v === 'string' && v) return truncate(v, 80);
    if (typeof v === 'number') return String(v);
  }
  for (const [, v] of Object.entries(obj)) {
    if (typeof v === 'string' && v) return truncate(v, 80);
    if (typeof v === 'number') return String(v);
  }
  return '';
}

function truncate(s: string, n: number): string {
  return s.length > n ? s.slice(0, n - 1) + '…' : s;
}

function formatDuration(ms: number): string {
  if (ms < 1000) return `${ms}ms`;
  return `${(ms / 1000).toFixed(1)}s`;
}

function PendingDots() {
  const { tr } = useI18n();
  return (
    <span className="inline-flex items-center gap-1.5 text-sm">
      <span>{tr('思考中', 'Thinking')}</span>
      <span className="inline-flex gap-0.5">
        <Dot delay={0} />
        <Dot delay={0.2} />
        <Dot delay={0.4} />
      </span>
    </span>
  );
}

function Dot({ delay }: { delay: number }) {
  return (
    <span
      className="inline-block h-1 w-1 animate-pulse-dot rounded-full bg-zinc-400"
      style={{ animationDelay: `${delay}s` }}
    />
  );
}

// --- HLD-017 inline approval -------------------------------------------

// pendingApproval returns the approval metadata when a tool result is the
// approval "pending_approval" envelope, else null.
function pendingApproval(result: unknown): { id: string; kind: string } | null {
  if (result && typeof result === 'object') {
    const r = result as Record<string, unknown>;
    if (r.status === 'pending_approval' && typeof r.approval_id === 'string') {
      return { id: r.approval_id, kind: typeof r.kind === 'string' ? r.kind : 'cloud_bash' };
    }
  }
  return null;
}

function argCommandText(args: unknown): string {
  if (args && typeof args === 'object') {
    const c = (args as Record<string, unknown>).command;
    if (typeof c === 'string') return c;
  }
  return '';
}

// PendingApprovalCard renders an in-conversation approve/reject prompt for a
// proposed cloud_bash command. Approve runs the command (the backend executor
// runs synchronously) and shows the result inline; reject discards it.
function PendingApprovalCard({ approvalID, kind, command }: { approvalID: string; kind: string; command: string }) {
  const { tr } = useI18n();
  const [state, setState] = useState<'loading' | 'idle' | 'busy' | 'done' | 'waiting' | 'rejected' | 'error' | 'stale'>('loading');
  const [resultText, setResultText] = useState('');
  const [errText, setErrText] = useState('');
  const [signedCount, setSignedCount] = useState(0);
  const [cmd, setCmd] = useState(command);
  const [approvalKind, setApprovalKind] = useState(kind);
  const [creds, setCreds] = useState<string[]>([]);
  // Risk metadata the backend already returns on the row (approval
  // model.go:59-79). Absent on rows predating 2d58a31, hence all optional.
  const [meta, setMeta] = useState<{ blast_radius?: string; risk_class?: string; target?: string } | null>(null);
  const [payload, setPayload] = useState('');
  const [showPayload, setShowPayload] = useState(false);
  const isHostBash = approvalKind === 'host_bash';

  // Reconcile with the authoritative server status on mount. When chat
  // history is reloaded, the persisted tool message carries only the result
  // blob (no arguments, no live status), so a long-decided proposal would
  // otherwise replay with dead 批准/拒绝 buttons that 404 on click ("not
  // found"). Mirrors ztna-agent's rule: a proposal's status is read from the
  // store on replay, never trusted from the message. The approval record
  // also carries the payload, so we recover the command text here too (fixes
  // the "(命令)" placeholder on the reload path).
  useEffect(() => {
    let alive = true;
    getApproval(approvalID)
      .then((a) => {
        if (!alive) return;
        setApprovalKind(a.kind || approvalKind);
        try {
          const p = JSON.parse(a.payload) as { command?: string; credentials?: string[] };
          if (!cmd && p.command) setCmd(p.command);
          if (Array.isArray(p.credentials)) setCreds(p.credentials.filter(Boolean));
        } catch {
          /* payload not JSON — leave placeholder */
        }
        // The row is already in hand — no second request. Risk metadata and
        // the raw payload feed the impact chips and the 详情 expander below.
        setMeta({ blast_radius: a.blast_radius, risk_class: a.risk_class, target: a.target });
        setPayload(a.payload ?? '');
        if (a.status === 'executed') {
          setState('done');
          setResultText(a.result ?? '');
        } else if (a.status === 'rejected') {
          setState('rejected');
        } else if (a.status === 'failed') {
          setState('error');
          setErrText(a.result ?? 'failed');
        } else {
          setState('idle'); // pending — offer the buttons
        }
      })
      .catch(() => {
        // Genuinely gone (404) or unreachable: never show dead buttons —
        // point the user at the inbox instead of letting a click 404.
        if (alive) {
          setState('stale');
          // Decided elsewhere — the sidebar count is certain to be stale.
          useApprovalBadge.getState().refresh();
        }
      });
    return () => {
      alive = false;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [approvalID]);

  const approve = async () => {
    setState('busy');
    try {
      const a = await approveApproval(approvalID);
      // Only an executed row may claim 已执行. A destructive command is
      // dual-sign: the first signature returns HTTP 202 with the row still
      // pending, and the command has NOT run. Rendering that as 已执行 told
      // the operator the work was done when it was still queued for a second
      // approver — so pending gets its own honest state instead.
      if (a.status === 'executed') {
        setState('done');
        setResultText(a.result ?? '');
      } else if (a.status === 'failed') {
        setState('error');
        setErrText(a.result ?? 'failed');
      } else {
        setState('waiting');
        setSignedCount(parseSigners(a.signers).signers.length);
      }
      // Any verdict moves the global pending count (executed / failed / just
      // signed). Fire-and-forget: refresh() never rejects and never blocks the
      // render; a miss here self-heals on the 30 s poll.
      useApprovalBadge.getState().refresh();
    } catch (e) {
      setState('error');
      setErrText((e as Error).message);
    }
  };
  const reject = async () => {
    setState('busy');
    try {
      await rejectApproval(approvalID, '');
      setState('rejected');
      useApprovalBadge.getState().refresh();
    } catch (e) {
      setState('error');
      setErrText((e as Error).message);
    }
  };

  return (
    <div className="w-full overflow-hidden rounded-lg bg-zinc-900/30 text-xs ring-1 ring-zinc-800/80">
      <div className="flex items-center gap-2 border-b border-zinc-800/60 px-3 py-2">
        <ShieldAlert size={13} className="text-amber-500/90" />
        <span className="font-medium text-zinc-200">
          {isHostBash
            ? tr('需要你确认才能在边端主机执行', 'Needs your approval to run on the edge host')
            : tr('需要你确认才能在云端执行', 'Needs your approval to run in the cloud')}
        </span>
      </div>
      <div className="px-3 pb-2.5">
        <pre className="mb-2 max-h-40 overflow-auto whitespace-pre-wrap break-all rounded bg-zinc-950 p-2 text-[11px] text-zinc-300">
          {cmd || tr('(命令)', '(command)')}
        </pre>
        {creds.length > 0 && (
          <div className="mb-2 flex flex-wrap items-center gap-1 text-[11px] text-zinc-400">
            <span>{tr('将注入凭证：', 'Injects credentials: ')}</span>
            {creds.map((c) => (
              <span key={c} className="rounded bg-zinc-800/60 px-1.5 py-0.5 font-mono text-zinc-300 ring-1 ring-zinc-700/50">
                {c}
              </span>
            ))}
          </div>
        )}
        {(meta?.blast_radius || meta?.risk_class || meta?.target) && (
          <div className="mb-2 flex flex-wrap items-center gap-1 text-[11px] text-zinc-400">
            {/* The radius and risk class are the point: the operator is being
                asked to authorise a change, and this is the only thing that
                says how much of the estate it touches. Mirrors NodeAgents'
                approval card. */}
            {meta.blast_radius && (
              <Chip tone="warning" dense>
                {tr('影响面', 'Blast radius')}: {blastRadiusLabel(meta.blast_radius, tr)}
              </Chip>
            )}
            {meta.risk_class && (
              <Chip tone={riskClassTone(meta.risk_class)} dense>
                {tr('风险等级', 'Risk')}: {riskClassLabel(meta.risk_class, tr)}
              </Chip>
            )}
            {meta.target && (
              <span className="rounded bg-zinc-800/60 px-1.5 py-0.5 font-mono text-zinc-300 ring-1 ring-zinc-700/50">
                {meta.target}
              </span>
            )}
          </div>
        )}
        {payload && (
          <>
            <button
              type="button"
              onClick={() => setShowPayload((v) => !v)}
              className="mb-1 inline-flex items-center gap-1 text-[11px] text-zinc-500 hover:text-zinc-300"
            >
              {showPayload ? <ChevronDown size={12} /> : <ChevronRight size={12} />}
              {tr('详情', 'Details')}
            </button>
            {showPayload && (
              <pre className="mb-2 max-h-40 overflow-auto whitespace-pre-wrap break-all rounded bg-zinc-950 p-2 text-[11px] text-zinc-400">
                {prettyResult(payload)}
              </pre>
            )}
          </>
        )}
        {state === 'loading' && (
          <div className="flex items-center gap-1.5 text-zinc-500">
            <Loader2 size={12} className="animate-spin" />
            {tr('加载审批状态…', 'Loading approval status…')}
          </div>
        )}
        {state === 'stale' && (
          <div className="text-zinc-500">
            {tr('该审批已失效或已处理，请前往「待确认」页查看。', 'This approval is gone or already handled — see the Approvals page.')}
          </div>
        )}
        {state === 'idle' && (
          <div className="flex gap-2">
            <button
              type="button"
              onClick={() => void approve()}
              className="inline-flex items-center gap-1 rounded-md border border-emerald-700 bg-emerald-950/40 px-2.5 py-1 text-emerald-300 hover:bg-emerald-900/40"
            >
              <Check size={12} />
              {tr('批准并执行', 'Approve & run')}
            </button>
            <button
              type="button"
              onClick={() => void reject()}
              className="inline-flex items-center gap-1 rounded-md border border-zinc-700 px-2.5 py-1 text-zinc-400 hover:border-red-800 hover:text-red-400"
            >
              <X size={12} />
              {tr('拒绝', 'Reject')}
            </button>
          </div>
        )}
        {state === 'busy' && <div className="flex items-center gap-1.5 text-zinc-400"><Loader2 size={12} className="animate-spin" />{tr('执行中…', 'Running…')}</div>}
        {state === 'waiting' && (
          <div className="flex items-start gap-1.5 text-amber-400">
            <Hourglass size={12} className="mt-0.5 shrink-0" />
            <span>
              {tr(
                `已记录你的签名（${signedCount} 人已签）。危险命令需第二位批准人确认后才会执行。`,
                `Your signature is recorded (${signedCount} so far). A second approver must confirm before the command runs.`,
              )}
            </span>
          </div>
        )}
        {state === 'rejected' && <div className="text-zinc-500">{tr('已拒绝，未执行', 'Rejected — not run')}</div>}
        {state === 'error' && <div className="break-all text-red-400">{errText}</div>}
        {state === 'done' && (
          <div>
            <div className="mb-1 text-emerald-400">{tr('已执行', 'Executed')}</div>
            <pre className="max-h-56 overflow-auto whitespace-pre-wrap break-all rounded bg-zinc-950 p-2 text-[11px] text-zinc-300">{prettyResult(resultText)}</pre>
          </div>
        )}
      </div>
    </div>
  );
}

function prettyResult(s: string): string {
  if (!s) return '';
  try {
    const o = JSON.parse(s) as Record<string, unknown>;
    const parts: string[] = [];
    if (o.stdout) parts.push(String(o.stdout));
    if (o.stderr) parts.push(`[stderr] ${String(o.stderr)}`);
    if (typeof o.exit_code === 'number') parts.push(`[exit ${o.exit_code}]`);
    return parts.join('\n') || s;
  } catch {
    return s;
  }
}
