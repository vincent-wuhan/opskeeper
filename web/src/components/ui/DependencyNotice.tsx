import type { ReactNode } from 'react';
import { AlertTriangle, PlugZap, TimerOff, WifiOff } from 'lucide-react';
import { cn } from '@/lib/cn';
import { useI18n } from '@/i18n/locale';

// DependencyNotice — the single way the console reports "a backing
// dependency isn't available right now" (Loki / Tempo / Prometheus not
// wired, DNS not resolving, query timed out).
//
// Before this existed every page printed the raw transport string the
// backend returned (`logquery: /loki/api/v1/query_range: Get "http://
// loki:3100/…": dial tcp: lookup loki: no such host`) straight into a red
// band. That reads as "the product is broken" when the truth is usually
// "this integration isn't configured in your deployment". We classify the
// message, lead with a human sentence + the one thing the operator can do
// about it, and tuck the raw text behind a disclosure for support tickets.
//
// Copy is deliberately in the same register as the rest of the console:
// a short title, a hint that names the concrete next step, and the
// technical detail only on demand.

type Classification = {
  kind: 'unconfigured' | 'unreachable' | 'timeout' | 'unknown';
  /** Host or dependency name pulled out of the raw message, if any. */
  target?: string;
};

const HOST_RE = /(?:lookup|dial tcp|dial udp|connect to|resolve)\s+([a-z0-9._-]+(?::\d+)?)/i;
const UNCONFIGURED_RE = /(not wired|not configured|disabled|no .*configured|未接入|未配置)/i;
const TIMEOUT_RE = /(timeout|timed out|deadline exceeded|context deadline)/i;
const UNREACHABLE_RE = /(no such host|connection refused|connect:|network is unreachable|i\/o timeout|eof)/i;

export function classifyDependencyError(raw: string): Classification {
  const msg = raw || '';
  if (UNCONFIGURED_RE.test(msg)) return { kind: 'unconfigured', target: depName(msg) };
  if (TIMEOUT_RE.test(msg)) return { kind: 'timeout', target: depName(msg) };
  const host = HOST_RE.exec(msg)?.[1];
  if (UNREACHABLE_RE.test(msg) || host) return { kind: 'unreachable', target: host ?? depName(msg) };
  return { kind: 'unknown' };
}

// depName pulls a dependency label out of common backend phrasings like
// "not wired yet: prometheus disabled" or "loki is not configured".
function depName(msg: string): string | undefined {
  const m = /([a-z][a-z0-9_-]{2,})\s+(?:is\s+)?(?:not wired|not configured|disabled)/i.exec(msg);
  if (m) return m[1];
  const wired = /not wired[^:]*:\s*([a-z][a-z0-9_-]{2,})/i.exec(msg);
  return wired?.[1];
}

const DEP_LABEL: Record<string, string> = {
  prometheus: 'Prometheus',
  loki: 'Loki',
  tempo: 'Tempo',
  grafana: 'Grafana',
  qdrant: 'Qdrant',
  searxng: 'SearXNG',
};

function prettyDep(name?: string): string | undefined {
  if (!name) return undefined;
  const bare = name.split(':')[0];
  return DEP_LABEL[bare] ?? bare.charAt(0).toUpperCase() + bare.slice(1);
}

export type DependencyNoticeProps = {
  /** Raw backend message. Kept verbatim in the disclosure. */
  message: string;
  /** Overrides the auto-classified title. */
  title?: string;
  /** Overrides the auto-classified hint (the "what to do" line). */
  hint?: string;
  /** Optional CTA — e.g. a link to Settings → Integrations. */
  action?: ReactNode;
  /** `warning` for "not configured yet" (amber), `danger` for a real failure. */
  tone?: 'warning' | 'danger';
  className?: string;
};

export function DependencyNotice({
  message,
  title,
  hint,
  action,
  tone,
  className,
}: DependencyNoticeProps) {
  const { tr } = useI18n();
  const cls = classifyDependencyError(message);
  const dep = prettyDep(cls.target);

  const { icon: Icon, titleText, hintText } = describe(cls, dep, tr);
  const resolvedTone = tone ?? (cls.kind === 'unconfigured' ? 'warning' : 'danger');

  const toneClass =
    resolvedTone === 'warning'
      ? 'border-amber-500/30 bg-amber-500/10 text-amber-200'
      : 'border-red-500/30 bg-red-500/10 text-red-200';

  return (
    <div
      role="status"
      className={cn(
        'flex items-start gap-2.5 rounded-lg border px-3 py-2.5 text-xs',
        toneClass,
        className,
      )}
    >
      <Icon size={14} className="mt-0.5 shrink-0" />
      <div className="min-w-0 flex-1">
        <div className="font-medium">{title ?? titleText}</div>
        {(hint ?? hintText) && (
          <div className="mt-0.5 text-[11px] text-zinc-400">{hint ?? hintText}</div>
        )}
        {action && <div className="mt-2 flex flex-wrap items-center gap-2">{action}</div>}
        <details className="group mt-1.5">
          <summary className="cursor-pointer list-none text-[11px] text-zinc-500 transition-colors hover:text-zinc-300">
            <span className="group-open:hidden">{tr('技术细节', 'Technical details')}</span>
            <span className="hidden group-open:inline">{tr('收起', 'Hide')}</span>
          </summary>
          <pre className="mt-1 max-h-40 overflow-auto whitespace-pre-wrap break-all rounded-md bg-zinc-950/60 px-2 py-1.5 font-mono text-[10px] leading-relaxed text-zinc-400">
            {message}
          </pre>
        </details>
      </div>
    </div>
  );
}

function describe(
  cls: Classification,
  dep: string | undefined,
  tr: (zh: string, en: string) => string,
): { icon: typeof AlertTriangle; titleText: string; hintText?: string } {
  const name = dep ?? tr('依赖服务', 'the dependency');
  switch (cls.kind) {
    case 'unconfigured':
      return {
        icon: PlugZap,
        titleText: tr(`${name} 未接入`, `${name} isn't configured`),
        hintText: tr(
          '该数据源还没在本部署里启用，所以这一屏暂时没有数据。配置后即可正常出图 / 查询。',
          'This data source isn’t enabled in this deployment, so this screen has nothing to show yet. Configure it and it will populate.',
        ),
      };
    case 'unreachable':
      return {
        icon: WifiOff,
        titleText: tr(`连不上 ${name}`, `Can’t reach ${name}`),
        hintText: tr(
          '服务地址可能填错、容器没起来，或网络不通。检查一下地址与容器状态后重试。',
          'The address may be wrong, the container may be down, or the network is blocked. Check the endpoint and retry.',
        ),
      };
    case 'timeout':
      return {
        icon: TimerOff,
        titleText: tr('查询超时', 'Query timed out'),
        hintText: tr(
          '数据量太大或后端较慢。缩小时间范围、加过滤条件后再试。',
          'Too much data or a slow backend. Narrow the time range or add filters and retry.',
        ),
      };
    default:
      return {
        icon: AlertTriangle,
        titleText: tr('查询失败', 'Query failed'),
        hintText: tr(
          '展开下方技术细节可看到后端返回的原始错误。',
          'Expand the technical details below for the raw backend error.',
        ),
      };
  }
}
