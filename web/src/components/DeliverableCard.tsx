// DeliverableCard — the only place a chat link turns into a card.
// Deliverable hrefs exist purely as markdown text inside message.content, so
// ReactMarkdown's `components.a` in AssistantBubble is the single interception
// point (D4). 换卡点(MessageBubble.tsx:142-143)不改。
import { createContext, useCallback, useContext, useEffect, useRef, useState, type ReactNode } from 'react';
import { BarChart3, FileText, Loader2 } from 'lucide-react';
import { tr } from '@/i18n/locale';
import { fetchPageHTML } from '@/api/pages';
import { getReport, type ReportDetail, type ReportKind } from '@/api/reports';
import { shouldRenderThumb, useInViewOnce } from './deliverableLazy';
import { HostedPageView } from './HostedPageView';
import { ReportHostedView } from './ReportHostedView';
import { Button } from './ui/Button';
import { Chip } from './ui/Chip';

const PAGE_RE = /^\/pages\/([0-9a-f]{16,64})$/;
// FIX 2:报表 id 是小写 UUID(8-4-4-4-12),对齐后端 uuid.NewString。收紧前 `[0-9a-f-]{16,64}`
// 会把全连字符(如 16 个 '-')这类绝不可能是真实 id 的串也成卡。
const REPORT_RE = /^\/reports\/([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})$/;

export type DeliverableInfo = { type: 'page' | 'report'; id: string; href: string };

export function matchDeliverable(href: string): DeliverableInfo | null {
  const page = PAGE_RE.exec(href);
  if (page) return { type: 'page', id: page[1], href };
  const report = REPORT_RE.exec(href);
  if (report) return { type: 'report', id: report[1], href };
  return null;
}

// extractHtmlTitle — 托管页标题的唯一零后端改动来源(design §3)。
// 解析不到 → 空串(留空,不编造)。
export function extractHtmlTitle(html: string): string {
  const m = /<title[^>]*>([\s\S]*?)<\/title>/i.exec(html);
  return m ? m[1].trim() : '';
}

type LoadState = 'idle' | 'loading' | 'ready' | 'failed';

// --- 单条消息交付物序号(缩略上限用,design §5;Task 7 由 MessageBubble 接线)---
// 卡片按 href 向 Provider 领取序号:同一 href 在同一 Provider 内永远拿到同一个序号。
// 键必须是 href 而不是「首次渲染自增计数」——StrictMode(dev,web/src/main.tsx)会把
// 组件渲染两次,第二次渲染时卡片的 useRef 被重建回初值,自增式 claim 会让每张卡
// 烧掉两个序号,实测 3 张卡的序号变成 [1,3,5],THUMB_CAP=3 最终只渲染 1 个窗格。
// 幂等 claim 让双重渲染拿回同一个序号;即使 Provider 自己的 ref 也在这轮被重建,
// Map 从空表重来也只是重新分配 0,1,2… 顺序,两种情况都拿到契约要求的上限行为。
const SequenceCtx = createContext<((key: string) => number) | null>(null);

export function DeliverableSequence({ children }: { children: ReactNode }) {
  const allocated = useRef(new Map<string, number>());
  const claim = useCallback((key: string) => {
    const existing = allocated.current.get(key);
    if (existing !== undefined) return existing;
    // 序号只在首次领取时分配,之后该 href 永远复用同一格(重复链接同一交付物只占一格)。
    const next = allocated.current.size;
    allocated.current.set(key, next);
    return next;
  }, []);
  return <SequenceCtx.Provider value={claim}>{children}</SequenceCtx.Provider>;
}

export function DeliverableCard({ info }: { info: DeliverableInfo }) {
  const claim = useContext(SequenceCtx);
  // 无 Provider(单测直渲染、未接线的旧调用点)时序号恒为 0,缩略照常允许。
  const index = claim ? claim(info.href) : 0;
  const thumbAllowed = shouldRenderThumb(index);

  const cardRef = useRef<HTMLSpanElement>(null);
  const visible = useInViewOnce(cardRef);
  const [state, setState] = useState<LoadState>('idle');
  const [pageTitle, setPageTitle] = useState('');
  const [html, setHtml] = useState<string | null>(null);
  const [report, setReport] = useState<ReportDetail | null>(null);
  // 展开态:点击卡头就地开预览,复用独立页的同一渲染器,不做导航。
  const [expanded, setExpanded] = useState(false);

  // 卸载后不再 setState。用 ref 而不是 effect 局部 `let alive`:下面取数 effect 的
  // 依赖含 state,setState('loading') 会让它重跑并 cleanup 掉局部 alive,响应回来后
  // 成功与失败两条分支都会被静默丢弃(卡片永远停在 loading)。
  // 挂载 effect 开头重置为 true,StrictMode 双调用下也不会残留 false。
  const aliveRef = useRef(true);
  useEffect(() => {
    aliveRef.current = true;
    return () => {
      aliveRef.current = false;
    };
  }, []);

  // 三态状态机:visible 之前不发请求(idle);失败不重试,降级占位 + 保留出口。
  // FIX 1:显式展开(expanded)必须无条件取数——卡片只露出一角时 IO 因交集比 < 0.1 报
  // isIntersecting=false,visible 恒 false(且展开后卡变高,交集更低,无法自愈);修前
  // `!visible` 短路会让 state 停在 idle,预览永久 spinner。缩略本身仍按 visible 懒挂载。
  useEffect(() => {
    if (state !== 'idle') return;
    if (!(expanded || (visible && thumbAllowed))) return;
    setState('loading');
    if (info.type === 'page') {
      fetchPageHTML(info.id)
        .then((h) => {
          if (!aliveRef.current) return;
          setHtml(h);
          setPageTitle(extractHtmlTitle(h));
          setState('ready');
        })
        .catch(() => aliveRef.current && setState('failed'));
    } else {
      getReport(info.id)
        .then((r) => {
          if (!aliveRef.current) return;
          setReport(r);
          setState('ready');
        })
        .catch(() => aliveRef.current && setState('failed'));
    }
  }, [visible, thumbAllowed, state, info.type, info.id, expanded]);

  const Icon = info.type === 'page' ? FileText : BarChart3;
  const typeLabel = info.type === 'page' ? tr('托管页', 'Hosted page') : tr('报表', 'Report');
  // 标题诚实来源:托管页 = HTML <title>;报表 = fetchReportDetail title;缺失留空。
  const title = info.type === 'page' ? pageTitle : report?.title ?? '';

  return (
    <span ref={cardRef} data-testid="deliverable-card" className="surface-card my-1.5 block rounded-rk-md">
      <span
        data-testid="deliverable-card-header"
        role="button"
        tabIndex={0}
        aria-expanded={expanded}
        onClick={() => setExpanded((v) => !v)}
        onKeyDown={(e) => {
          // 只响应打在卡头自身的按键。出口是卡头里一个真实的 <button>,它的 keydown
          // 会冒泡上来:Enter/Space 在按钮上的默认行为就是激活,若卡头继续
          // preventDefault + 切换,键盘用户唯一的出口会被静默吞掉(鼠标点击不受
          // 影响,click 那侧有 stopPropagation)。
          if (e.target !== e.currentTarget) return;
          if (e.key === 'Enter' || e.key === ' ') {
            e.preventDefault();
            setExpanded((v) => !v);
          }
        }}
        className="flex cursor-pointer items-center gap-3 px-3 py-2.5 hover:bg-zinc-800/30"
      >
        <span className="flex h-8 w-8 shrink-0 items-center justify-center rounded-rk-sm bg-accent-50 text-accent-700">
          <Icon className="h-4 w-4" aria-hidden="true" />
        </span>
        <span className="min-w-0 flex-1">
          <span className="flex items-center gap-1.5">
            <Chip>{typeLabel}</Chip>
            {title ? <span className="block truncate text-xs font-medium text-zinc-200">{title}</span> : null}
          </span>
          <span className="block truncate text-[11px] text-zinc-500">{info.href}</span>
        </span>
        <Button
          variant="ghost"
          onClick={(e) => {
            e.stopPropagation();
            window.open(info.href, '_blank');
          }}
        >
          {tr('新窗口打开', 'Open in new tab')}
        </Button>
      </span>
      {thumbAllowed && (
        <span data-testid="deliverable-thumb" className="mx-3 mb-2.5 block h-40 overflow-hidden rounded-rk-sm border border-border">
          {state === 'idle' && <span className="block h-full w-full bg-zinc-900/40" />}
          {state === 'loading' && (
            <span className="flex h-full w-full items-center justify-center bg-zinc-900/40 text-zinc-500">
              <Loader2 className="h-4 w-4 motion-safe:animate-spin" />
            </span>
          )}
          {state === 'failed' && <TypePlaceholder type={info.type} failed />}
          {state === 'ready' && info.type === 'page' && html != null && <HostedPageView html={html} />}
          {state === 'ready' && info.type === 'report' && report != null && <ReportThumb report={report} />}
        </span>
      )}
      {expanded && (
        <span data-testid="deliverable-preview" className="mx-3 mb-2.5 block rounded-rk-sm border border-border bg-zinc-950/40 p-2">
          {state === 'idle' || state === 'loading' ? (
            <span className="flex h-24 items-center justify-center text-zinc-500">
              <Loader2 className="h-4 w-4 motion-safe:animate-spin" />
            </span>
          ) : state === 'failed' ? (
            <TypePlaceholder type={info.type} failed />
          ) : info.type === 'page' && html != null ? (
            <HostedPageView html={html} height="60vh" />
          ) : info.type === 'report' && report != null ? (
            report.status === 'pending' || report.status === 'generating' ? (
              <TypePlaceholder type="report" line1={tr('报告生成中…', 'Report is generating…')} />
            ) : report.content ? (
              <ReportHostedView content={report.content} />
            ) : report.status === 'failed' ? (
              <TypePlaceholder type="report" failed />
            ) : (
              // FIX 3:ready 但 content 为空 =「取到但无内容」,与取数失败不同,不能谎报「加载失败」。
              <TypePlaceholder type="report" line1={tr('报告暂无内容', 'No content in this report')} />
            )
          ) : (
            <TypePlaceholder type={info.type} failed />
          )}
        </span>
      )}
    </span>
  );
}

// ReportThumb — 报表类型化占位缩略:类型 + 生成时间,无 img/无 iframe(spec:报表缩略为类型占位)。
// pending/generating → 生成中占位,不当作 failed(design §7);后端 failed → 降级占位。
function ReportThumb({ report }: { report: ReportDetail }) {
  if (report.status === 'pending' || report.status === 'generating') {
    return <TypePlaceholder type="report" line1={tr('报告生成中…', 'Report is generating…')} line2="" />;
  }
  if (report.status === 'failed') return <TypePlaceholder type="report" failed />;
  return <TypePlaceholder type="report" line1={reportKindLabel(report.kind)} line2={report.generated_at ?? ''} />;
}

function reportKindLabel(kind: ReportKind): string {
  switch (kind) {
    case 'daily': return tr('日报', 'Daily report');
    case 'weekly': return tr('周报', 'Weekly report');
    case 'monthly': return tr('月报', 'Monthly report');
    default: return tr('自定义报告', 'Custom report');
  }
}

// TypePlaceholder — 类型化占位(缩略失败降级 / 报表占位共用,design §1.2)。
function TypePlaceholder({ type, failed, line1, line2 }: { type: 'page' | 'report'; failed?: boolean; line1?: string; line2?: string }) {
  const Icon = type === 'page' ? FileText : BarChart3;
  return (
    <span className="flex h-full w-full flex-col items-center justify-center gap-1 bg-zinc-900/60 text-zinc-500">
      <Icon className="h-6 w-6" aria-hidden="true" />
      <span className="text-[11px]">{line1 ?? (failed ? tr('加载失败', 'Failed to load') : '')}</span>
      {line2 ? <span className="text-[10px] text-zinc-600">{line2}</span> : null}
    </span>
  );
}
