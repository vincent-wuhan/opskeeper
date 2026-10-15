import { useEffect, useMemo, useState } from 'react';
import { useNavigate } from 'react-router-dom';
import {
  Activity,
  AlertTriangle,
  Bell,
  Database,
  FileSearch,
  Flame,
  Gauge,
  HardDrive,
  History,
  ListTree,
  Network,
  Search,
  ShieldAlert,
  TrendingUp,
} from 'lucide-react';
import { ChatInput, type ModelSelection } from '@/components/ChatInput';
import { useModelSelection } from '@/store/modelSelection';
import { PromptCard } from '@/components/PromptCard';
import { StatusRow } from '@/components/StatusRow';
import { AgentAvatar } from '@/components/AgentAvatar';
import { useAgents, avatarFor } from '@/store/agents';
import { Chip } from '@/components/ui/Chip';
// personaLabel resolves an agent_id to its localized display name — same
// tables AgentBadge renders from. Don't re-declare a mapping here.
import { personaLabel } from '@/components/AgentBadge';
import { listAgents, type AgentSummary } from '@/api/agents';
import { createSession, listModels, type LLMProvider } from '@/api/chat';
import { setSetting, invalidateLLMRouter } from '@/api/settings';
import { listEdges } from '@/api/edges';
import { listIncidents, type Incident } from '@/api/alerts';
import { useApprovalBadge } from '@/store/approvalBadge';
import { usePermissions } from '@/store/me';
import { useI18n } from '@/i18n/locale';

// Hero 标语 —— 全部走"助理向用户报到"语气：听候差遣 / 今天能做些什么 /
// 让我们 xxx。useMemo 在 mount 时抽一条，re-render 不重洗。早期那些
// 偏哲学的（"Production is calm. So are you" / "Reset 之前先 Read"）
// 撤掉了，节奏不像助理在跟你打招呼。
type Greeting = { zh: string; en: string };

// NOT_CHATTABLE = 后端 persona 里不该在首页做成快捷卡的（点了都是死路）。
//   'default'  —— 虚拟 persona。上面那个大输入框 startSession 绑的就是它，
//     点它的卡 ≡ 在输入框少写一句 prompt，零信息量。
//   'reporter' —— agents/reporter.md 的 frontmatter 写明「由 report 调度器
//     / 手动"立即生成"触发（非用户 chat spawn）」且 tools: []。它等的输入是
//     一份 ReportFacts JSON，由后端调度器喂，用户点开只会得到一个没有工具、
//     在等不存在输入的死会话。
//
// 判据只能是人名 —— critic / reviewer 同样是 tools: [] + read-only，但它们
// 是完全正常的对话 persona（Manager spawn 后质疑诊断结论 / 二审高危操作），
// 用 tools.length 或 permission_mode 当判据会误杀。
//
// 这是一份前端硬编码的重复真相（仓库既有同类：AgentAvatar.PERSONA_VISUALS
// 11 条、AGENT_LABELS_ZH/EN、PERSONA_ALIASES），将来若新增非对话 worker
// persona 需同步此表；根治要后端给 AgentSummary 加 chat_capable 字段，
// 超出本 change 的零后端约束，已记 deferred。
const NOT_CHATTABLE = new Set(['default', 'reporter']);

const GREETINGS: Greeting[] = [
  { zh: '听候差遣', en: 'At your service.' },
  { zh: '随时待命', en: 'Ready when you are.' },
  { zh: '今天能为你做些什么？', en: 'What can I help with today?' },
  { zh: '说出你的目标，剩下交给我', en: "Tell me the goal — I'll handle the rest." },
  { zh: '想从哪儿开始？', en: 'Where do you want to start?' },
  { zh: '需要诊断、对比，还是只想瞅瞅？', en: 'Diagnose, compare, or just browse?' },
  { zh: '让我们看看现在最值得关心的事', en: "Let's tackle what matters most." },
  { zh: '让我们从最严重的那条开始', en: 'Start with the most critical one.' },
  { zh: '让我们一起把这班守好', en: "Let's work the shift together." },
  { zh: '让告警先过我这一关', en: 'I can triage the alerts first.' },
  { zh: '让我先帮你看一眼集群', en: 'Let me check the cluster.' },
  { zh: '让我帮你把头绪理一理', en: 'Let me help you untangle this.' },
  { zh: '让我们一起把它解决掉', en: "Let's solve this." },
  { zh: '想看看哪条线索？', en: 'Which lead first?' },
  { zh: '需要我先做些什么？', en: 'Where should I start?' },
  { zh: '随便问，我会给你一个起点', en: "Ask anything — I'll find a starting point." },
];

// PROMPT_POOL is the full set of starter prompts; sample 4 each render
// so users see variety.
type PromptPreset = {
  titleZh: string; titleEn: string;
  descZh: string; descEn: string;
  promptZh: string; promptEn: string;
  icon: typeof Flame;
};
const PROMPT_POOL: PromptPreset[] = [
  { titleZh: '找出资源最紧张的 3 台设备', titleEn: 'Top 3 busiest devices',
    descZh: 'CPU / 内存 / 负载综合排序', descEn: 'Ranked by CPU, memory, and load',
    promptZh: '找出当前 CPU、内存或负载最紧张的 3 台设备，给出关键指标和判断依据。',
    promptEn: 'Find the 3 busiest devices by CPU, memory, or load. Show key metrics and why.', icon: Flame },
  { titleZh: '看一眼整个集群健康状况', titleEn: 'Cluster health at a glance',
    descZh: '一句话掌握全局', descEn: 'One-line fleet summary',
    promptZh: '帮我总览所有设备的健康状况：在线 / 离线分布、平均 CPU / 内存 / 磁盘占用，以及任何明显异常。',
    promptEn: 'Summarise all devices: online vs offline, average CPU, memory, disk, and any obvious anomalies.', icon: Activity },
  { titleZh: '列出离线超过 24 小时的设备', titleEn: 'List devices offline > 24h',
    descZh: '找掉线节点', descEn: 'Find disconnected nodes',
    promptZh: '列出离线超过 24 小时的设备，按最后在线时间倒序展示。',
    promptEn: 'List devices offline for over 24h, sorted by last-seen descending.', icon: AlertTriangle },
  { titleZh: '对比本周和上周的整体负载', titleEn: 'Compare load: this week vs. last',
    descZh: '看趋势变化', descEn: 'See the trend',
    promptZh: '对比本周和上周所有设备的整体 CPU、内存、负载趋势，指出变化最大的节点。',
    promptEn: "Compare this week's vs. last week's CPU / memory / load trend across all devices; highlight the nodes that changed most.", icon: TrendingUp },
  { titleZh: '当前最严重的活跃告警', titleEn: 'Top active alerts',
    descZh: '哪些事在烧', descEn: "What's on fire",
    promptZh: '列出当前所有未解决的告警，按 severity 排序，给我每条告警的目标设备 + 触发原因摘要。',
    promptEn: 'List unresolved alerts by severity; for each, the target device and a one-line reason.', icon: ShieldAlert },
  { titleZh: '过去 24 小时新增告警有哪些', titleEn: 'New alerts in the last 24h',
    descZh: '看夜班发生了什么', descEn: 'What the night shift saw',
    promptZh: '过去 24 小时内首次触发的告警有哪些？按时间倒序列，每条给规则名 / 设备 / severity。',
    promptEn: 'Which alerts fired for the first time in the last 24h? Newest first, with rule, device, and severity.', icon: Bell },
  { titleZh: '诊断最近一条 critical 告警', titleEn: 'Diagnose the latest critical alert',
    descZh: '关联 metric / log / trace', descEn: 'Correlate metrics, logs, traces',
    promptZh: '查最近一条 critical 级别的告警，做完整的根因关联分析（metric + 日志 + trace），给出 3 段输出：现象 / 关联信号 / 假设。',
    promptEn: 'Take the latest critical alert and run a full root-cause correlation across metrics, logs, and traces. Output three sections: symptoms, correlated signals, hypotheses.', icon: AlertTriangle },
  { titleZh: '哪些设备磁盘快满了', titleEn: 'Which devices are running out of disk',
    descZh: '提前发现容量问题', descEn: 'Catch capacity issues early',
    promptZh: '列出磁盘使用率超过 80% 的设备，按使用率降序排，对前 3 台给出 top 占用目录。',
    promptEn: 'Devices over 80% disk usage, sorted descending. For the top 3, also list the largest directories.', icon: HardDrive },
  { titleZh: '某台设备的 top 大文件', titleEn: 'Top large files on a device',
    descZh: '帮我找空间被谁吃掉了', descEn: 'Find what is eating the space',
    promptZh: '在使用率最高的那台设备上，列出 / 目录下 top 20 大文件，按 size 降序。',
    promptEn: 'On the device with the highest disk usage, list the 20 largest files under /, biggest first.', icon: FileSearch },
  { titleZh: '哪台设备的某个进程占资源最多', titleEn: 'Find the heaviest processes',
    descZh: '定位异常进程', descEn: 'Pinpoint runaway processes',
    promptZh: '看一下当前所有设备里 top CPU + top 内存 进程，列出前 5 个，标出对应设备。',
    promptEn: 'Across all devices, the top 5 processes by CPU and the top 5 by memory, tagged with the device.', icon: ListTree },
  { titleZh: '检测异常波动的设备', titleEn: 'Spot anomalous devices',
    descZh: 'z-score 离群点', descEn: 'z-score outliers',
    promptZh: '在所有设备里，找最近 2 小时 CPU 或内存 z-score 偏离基线最远的设备（异常 outlier）。',
    promptEn: 'Find the devices whose CPU or memory z-score has drifted farthest from baseline in the last 2 hours.', icon: Gauge },
  { titleZh: '搜索最近的错误日志', titleEn: 'Search recent error logs',
    descZh: '快速定位 error / panic', descEn: 'Find error, panic, OOM fast',
    promptZh: '查所有设备最近 1 小时内的 error / panic / OOM 日志，按设备和频次聚合。',
    promptEn: 'Search error / panic / OOM logs in the last hour across all devices, grouped by device and frequency.', icon: Search },
  { titleZh: '看一台设备的实时负载', titleEn: 'Live view: one device',
    descZh: '即时 cpu/mem/load', descEn: 'Real-time CPU, memory, load',
    promptZh: '随便挑一台在线设备，给我看它现在的 CPU / 内存 / 负载（实时）+ 最近 24h 趋势对比。',
    promptEn: 'Pick an online device and show its current CPU / memory / load alongside the 24h trend.', icon: Activity },
  { titleZh: '对比设备之间的网络流量', titleEn: 'Compare network traffic',
    descZh: 'rx/tx 排序', descEn: 'Rank by rx/tx',
    promptZh: '比较所有设备最近 1 小时入站 / 出站流量，找最高的 3 台并解读对比。',
    promptEn: 'Compare rx/tx traffic over the last hour; surface the top 3 devices and explain the gap.', icon: Network },
  { titleZh: '看这条规则最近的触发频次', titleEn: 'Rule firing frequency',
    descZh: '规则疲劳分析', descEn: 'Alert-fatigue analysis',
    promptZh: '列出过去 7 天触发频次最高的 5 条告警规则，对每条给触发分布 + 代表设备。',
    promptEn: 'List the top 5 noisiest alert rules in the past week, with firing distribution and a sample device.', icon: History },
  { titleZh: '集群整体存储增长', titleEn: 'Cluster-wide storage growth',
    descZh: '容量规划视角', descEn: 'Capacity-planning view',
    promptZh: '所有设备过去 7 天磁盘使用增长趋势（百分点 / 天），找出增长最快的 3 台并预估到 90% 的剩余天数。',
    promptEn: 'Disk usage growth (pp/day) over the past week. Surface the 3 fastest-growing and estimate days until 90% full.', icon: Database },
  { titleZh: '分析数据库状态', titleEn: 'Analyze database status',
    descZh: 'MySQL / PostgreSQL / Redis / MongoDB', descEn: 'MySQL / PostgreSQL / Redis / MongoDB',
    promptZh: '分析当前数据库状态，覆盖 MySQL、PostgreSQL、Redis、MongoDB；按异常优先输出总体结论、每个数据库的关键指标、风险和证据。',
    promptEn: 'Analyze current database status across MySQL, PostgreSQL, Redis, and MongoDB. Prioritize anomalies and include the overall conclusion, key metrics, risks, and evidence for each database.', icon: Database },
  { titleZh: '配置一条 CPU 告警', titleEn: 'Configure a CPU alert',
    descZh: '先草案试算，确认后应用', descEn: 'Draft and preview before applying',
    promptZh: '配置一条 CPU 超过 90% 且持续 5 分钟的 warning 告警，先生成草案并试算，不要直接应用。',
    promptEn: 'Configure a warning alert for CPU over 90% for 5 minutes. Generate a draft with preview first; do not apply it yet.', icon: Bell },
];

function samplePrompts(n: number): typeof PROMPT_POOL {
  const pool = [...PROMPT_POOL];
  for (let i = pool.length - 1; i > 0; i--) {
    const j = Math.floor(Math.random() * (i + 1));
    [pool[i], pool[j]] = [pool[j], pool[i]];
  }
  return pool.slice(0, n);
}

// 时段问候（眉标）。tr 作为参数传入 —— locale.ts 的 tr 只在调用时读取
// 当前语言，模块作用域调用会被求值一次并冻在首次加载的语言上。
function greetingFor(hour: number, tr: (zh: string, en: string) => string): string {
  if (hour < 5) return tr('凌晨好', 'Still up');
  if (hour < 11) return tr('早上好', 'Good morning');
  if (hour < 13) return tr('中午好', 'Good noon');
  if (hour < 18) return tr('下午好', 'Good afternoon');
  return tr('晚上好', 'Good evening');
}

export default function HomePage() {
  const { tr } = useI18n();
  const navigate = useNavigate();
  const byName = useAgents((s) => s.byName);
  const [draft, setDraft] = useState('');
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [edgeTotal, setEdgeTotal] = useState<number | null>(null);
  // 首页摘要用的未关闭事件总数 + 「进行中」卡要渲染的那几条 items。
  // 同一份响应一次 setState：openTotal 是摘要行数字，openIncidents 是卡片列表。
  // 摘要行**必须**读 total 而不是 openIncidents.length —— 后端
  // core/manager/server/alert/http.go:171 明确注释过 len(items) 恒 <= page_size，
  // 拿 items 长度当总数会静默少报。
  const [openTotal, setOpenTotal] = useState(0);
  const [openIncidents, setOpenIncidents] = useState<Incident[]>([]);
  // 待审批数字复用侧栏的审批 badge store（自带 admin 门禁 + 30s 轮询），
  // 首页不再单独发 /v1/approvals 请求 —— 那条路由每个 handler 都在
  // requireAdmin 之后，非 admin 打过去是必然 403。
  const pendingApprovals = useApprovalBadge((s) => s.pending);
  const { isAdmin } = usePermissions();
  const [providers, setProviders] = useState<LLMProvider[]>([]);
  // Model selection lives in a persisted store (shared with ChatThread), so a
  // pick survives navigation + reload and the launched session inherits it.
  // selected==null → fall back to the live catalog default.
  const storeModel = useModelSelection((s) => s.selected);
  const setStoreModel = useModelSelection((s) => s.setSelected);
  const [catalogDefault, setCatalogDefault] = useState<ModelSelection | null>(null);
  const selectedModel = storeModel ?? catalogDefault;
  // SearXNG ships zero-key zero-quota in our compose stack — leave on by default.
  const [webSearchEnabled, setWebSearchEnabled] = useState(true);
  // 「你的 Agent」快捷卡。persona id 就是 AgentSummary.name（没有单独的
  // id 字段）—— 与 Agents.tsx 的 createSession({ agent_id: agent.name }) 一致。
  const [agents, setAgents] = useState<AgentSummary[]>([]);
  // 防连点：正在建会话的 persona id。刻意不复用 submitting —— 那是主
  // 输入框的语义，两者不该互相锁死。
  const [startingAgent, setStartingAgent] = useState<string | null>(null);

  // 进首页时随机一条问候 + 4 张 prompt 卡；mount 期间不变。
  const greetingPair = useMemo(() => GREETINGS[Math.floor(Math.random() * GREETINGS.length)], []);
  const greeting = tr(greetingPair.zh, greetingPair.en);
  // 时段问候在渲染期求值（而非模块作用域），语言切换后才会跟着重绘。
  const hourGreeting = greetingFor(new Date().getHours(), tr);
  // Pin the webpage-generator card first, then 3 random suggestions.
  const prompts = useMemo(() => samplePrompts(4), []);

  useEffect(() => {
    let cancelled = false;
    listEdges()
      .then((r) => {
        if (!cancelled) setEdgeTotal(r.total ?? r.items?.length ?? 0);
      })
      .catch(() => {
        // On failure, assume servers exist so we don't show the empty-state CTA on a transient error.
        if (!cancelled) setEdgeTotal(null);
      });
    // pageSize 5 —— 「进行中」卡要渲染前几条 items。仍留在 Task 12 留下的这个
    // effect 里（它有 cancelled 守卫）；另起一个 effect 会重复发请求且缺守卫。
    listIncidents({ status: 'open', pageSize: 5 })
      .then((r) => {
        if (cancelled) return;
        setOpenTotal(r.total ?? 0);
        setOpenIncidents(r.items ?? []);
      })
      .catch(() => {
        // Best-effort chrome — the header summary keeps its previous number
        // rather than blanking. /alerts surfaces the real error if clicked.
        if (cancelled) return;
        setOpenTotal(0);
        setOpenIncidents([]);
      });
    listModels()
      .then((cat) => {
        if (cancelled) return;
        setProviders(cat.providers ?? []);
        if (cat.default && cat.default.provider) {
          const d: ModelSelection = { provider: cat.default.provider, model: cat.default.model || '' };
          setCatalogDefault(d);
          // The server default (default_provider + <provider>_default_model) is
          // authoritative. A persisted pick can go stale — the default was
          // changed in Settings, or the settings were reseeded — and would
          // otherwise pin an outdated model both here and in the launched
          // session (which inherits the store). Reconcile a stale/absent store
          // to the server default; an in-session pick still wins (it updates
          // the store after this mount-time effect has run).
          const cur = useModelSelection.getState().selected;
          if (!cur || cur.provider !== d.provider || cur.model !== d.model) {
            setStoreModel(d);
          }
        }
      })
      .catch(() => {
        if (!cancelled) setProviders([]);
      });
    return () => {
      cancelled = true;
    };
  }, []);

  // persona 快捷卡。best-effort: 拉不到就整段不渲染，绝不把首页拖进错误态。
  // 取后端返回的前 6 个 —— Agents.tsx 的 builtinRank/BUILTIN_ORDER 排序是
  // 该文件的模块私有，本任务不动它（顺序可能与档案墙不一致，已记 deferred）。
  useEffect(() => {
    let cancelled = false;
    listAgents()
      .then((r) => {
        // 先 filter 再 slice，排掉的空位才由真正的 persona 补位；反过来
        // slice 再 filter 会永远少一格。
        if (!cancelled) setAgents(r.items.filter((a) => !NOT_CHATTABLE.has(a.name)).slice(0, 6));
      })
      .catch(() => {
        if (!cancelled) setAgents([]);
      });
    return () => {
      cancelled = true;
    };
  }, []);

  // Persist a home-page pick as the GLOBAL default (default_provider +
  // <provider>_default_model) so every server-side LLM consumer that doesn't
  // pin a model — the RCA investigator worker, query_translate — and the chat
  // default all use the same model shown here. The pick also rides the shared
  // store so the launched chat session inherits it. Per-message overrides
  // inside a chat thread stay transient (ChatThread never writes the default).
  async function handleModelChange(sel: ModelSelection | null) {
    setStoreModel(sel);
    if (!sel?.provider) return;
    try {
      await setSetting('llm', 'default_provider', sel.provider, false);
      if (sel.model) {
        await setSetting('llm', `${sel.provider}_default_model`, sel.model, false);
      }
      await invalidateLLMRouter();
    } catch {
      /* non-fatal — the pick still rides the per-session store */
    }
  }

  async function startSession(content: string) {
    if (!content.trim() || submitting) return;
    setError(null);
    setSubmitting(true);
    try {
      const title = content.trim().slice(0, 30);
      // Bind home-launched sessions to the virtual "default" persona —
      // shows the 默认 badge in sidebar/agents and uses the unrestricted
      // coordinator-equivalent toolBag on the backend.
      const session = await createSession({ title, agent_id: 'default' });
      // Don't post here — ChatThread takes the initialPrompt and runs it
      // through the SSE streamMessage path so the user sees tool cards and
      // the assistant reply incrementally. The picked model rides the shared
      // store (useModelSelection), so the launched session inherits it.
      navigate(`/chat/${session.id}`, { state: { initialPrompt: content } });
    } catch (err) {
      setError((err as Error).message || tr('创建会话失败', 'Failed to create session'));
      setSubmitting(false);
    }
  }

  // 从 persona 快捷卡直达一个新会话。失败复用首页既有的 error 展示块
  // （上面那个 role="alert"），不再造第二套错误 UI。成功后直接导航 —— 没有
  // initialPrompt 可传，所以不需要 state 参数。
  async function startWith(agentName: string) {
    if (startingAgent) return;
    setError(null);
    setStartingAgent(agentName);
    try {
      const label = personaLabel(agentName, tr);
      const session = await createSession({ title: label.slice(0, 30), agent_id: agentName });
      navigate(`/chat/${session.id}`);
    } catch (err) {
      setError((err as Error).message || tr('创建会话失败', 'Failed to create session'));
      setStartingAgent(null);
    }
  }

  const showEmptyState = edgeTotal === 0;
  return (
    <main className="flex flex-1 flex-col overflow-hidden">
      <div className="flex-1 overflow-y-auto">
        <div className="mx-auto flex w-full max-w-3xl flex-col items-stretch px-6 pb-16 pt-16 sm:pt-20">
          <StatusRow />

          <p className="mt-8 text-center text-sm text-zinc-500">
            {hourGreeting}
          </p>

          <h1 className="mb-2 mt-1 text-center text-3xl font-semibold tracking-tight text-zinc-100">
            {greeting}
          </h1>

          <p className="mb-8 text-center text-sm text-zinc-400">
            {tr('未关闭事件', 'Open incidents')}:{' '}
            <span className="text-zinc-200">{openTotal}</span>
            {isAdmin && (
              <>
                {' · '}
                {tr('待审批', 'Pending approvals')}:{' '}
                <span className="text-zinc-200">{pendingApprovals}</span>
              </>
            )}
          </p>

          {/* 只做抬升、不做表面 —— ChatInput 根自带 rounded-2xl +
              border + bg(ChatInput.tsx:356)，再套一层有底色的卡片就是
              双框。圆角必须留：阴影画在 wrapper 的 border-box 上，
              不圆的话阴影是直角矩形，跟里面的圆角输入框对不上。 */}
          <div className="rounded-2xl transition-shadow focus-within:shadow-pop">
            <ChatInput
              value={draft}
              onChange={setDraft}
              onSubmit={(p) => {
                setDraft('');
                void startSession(p.text);
              }}
              disabled={submitting}
              autoFocus
              providers={providers}
              selectedModel={selectedModel}
              onModelChange={handleModelChange}
              webSearchEnabled={webSearchEnabled}
              onWebSearchToggle={setWebSearchEnabled}
            />
          </div>

          {error && (
            <div
              role="alert"
              className="mt-3 rounded-lg border border-red-500/20 bg-red-500/10 px-3 py-2 text-xs text-red-300"
            >
              {error}
            </div>
          )}

          {/* 「你的 Agent」快捷卡：点一下直接开一个绑定该 persona 的新会话。
              位置在提示词卡之上 —— 这一节最该被先看到的入口就是 persona，
              提示词卡是退而求其次的备选。
              用原生 <button> 而不是 <Card>：Card 的 as prop 只允许
              div/section/article，渲染不出 button，而这个卡必须键盘可达。
              .surface-card 提供卡面 + 弱边框，hover 用与 Card.tsx
              interactive 分支一致的语义 token。 */}
          {agents.length > 0 && (
            <section className="mt-10">
              <h2 className="mb-2 text-xs font-medium uppercase tracking-wide text-zinc-500">
                {tr('你的 Agent', 'Your agents')}
              </h2>
              <div className="grid grid-cols-2 gap-3 sm:grid-cols-3 lg:grid-cols-6">
                {agents.map((a) => {
                  const busy = startingAgent === a.name;
                  return (
                    <button
                      key={a.name}
                      type="button"
                      disabled={busy || startingAgent !== null}
                      onClick={() => void startWith(a.name)}
                      className="surface-card flex flex-col items-center gap-2 rounded-2xl px-3 py-4 transition-colors hover:border-border hover:bg-card disabled:cursor-not-allowed disabled:opacity-50"
                    >
                      <AgentAvatar agentId={a.name} size={40} avatar={avatarFor(byName, a.name)} />
                      <span className="truncate text-xs text-zinc-300">
                        {personaLabel(a.name, tr)}
                      </span>
                    </button>
                  );
                })}
              </div>
            </section>
          )}

          {/* 「进行中」：未关闭事件 + 待审批入口。mt-8 是**分层间距**（32px），
              不是把两节撕开 —— 上面 persona section 有 mt-10 顶部但没有底部间距，
              两节又都带 <h2>，0 间距读起来像渲染坏了。persona 降级不渲染时
              （listAgents 失败 → setAgents([])）上方是摘要行的 mb-8，同为 32px，
              两种路径间距一致，不需要额外的条件类名。
              审批只渲染**一张**泛化卡，不拉列表：/v1/approvals 每个 handler
              都在 requireAdmin 之后，首页再发一次请求非 admin 必然 403
              （Task 12 刚把这条请求整个删掉）。数字复用侧栏 badge store。 */}
          {(openIncidents.length > 0 || (isAdmin && pendingApprovals > 0)) && (
            <section className="mt-8">
              <h2 className="mb-2 text-xs font-medium uppercase tracking-wide text-zinc-500">
                {tr('进行中', 'In progress')}
              </h2>
              <div className="grid gap-2 sm:grid-cols-2">
                {openIncidents.map((inc) => (
                  <button
                    key={inc.id}
                    type="button"
                    onClick={() => navigate(`/incidents/${inc.id}`)}
                    className="surface-card flex items-center gap-3 rounded-2xl px-4 py-3 text-left transition-colors hover:border-border hover:bg-card"
                  >
                    <span className="flex-1 truncate text-sm text-zinc-200">{inc.summary}</span>
                    {/* IncidentSeverity 带 `| string` 兜底，不是穷尽联合类型 ——
                        所以只判 critical，其余一律 warning，不写 switch。 */}
                    <Chip tone={inc.severity === 'critical' ? 'danger' : 'warning'}>
                      {inc.severity}
                    </Chip>
                  </button>
                ))}
                {isAdmin && pendingApprovals > 0 && (
                  <button
                    type="button"
                    onClick={() => navigate('/approvals')}
                    className="surface-card flex items-center gap-3 rounded-2xl px-4 py-3 text-left transition-colors hover:border-border hover:bg-card"
                  >
                    <span className="flex-1 text-sm text-zinc-200">
                      ⏸ {tr('等你审批', 'Waiting for your approval')} · {pendingApprovals}{' '}
                      {tr('项', 'items')}
                    </span>
                    <span className="text-xs text-zinc-400">
                      {tr('去审批', 'Review')} →
                    </span>
                  </button>
                )}
              </div>
            </section>
          )}

          <div className="mt-10">
            {showEmptyState ? (
              <button
                type="button"
                onClick={() => navigate('/edges')}
                aria-label={tr('还没有设备接入', 'No devices onboarded')}
                className="group flex w-full flex-col items-start gap-2 rounded-xl border border-dashed border-zinc-700 bg-zinc-900/30 p-5 text-left transition-colors hover:border-zinc-600 hover:bg-zinc-900/50"
              >
                <div className="flex w-full items-center gap-2">
                  <HardDrive size={16} className="text-zinc-300 group-hover:text-zinc-100" />
                  <span className="text-sm font-semibold text-zinc-100">
                    {tr('还没有设备接入', 'No devices onboarded')}
                  </span>
                </div>
                <span className="text-xs leading-relaxed text-zinc-400">
                  {tr(
                    '点这里去新建第一台设备，在“设备”页面右上角拿一次性 access/secret，然后用弹窗里的 curl 一键命令在目标主机上跑。',
                    'Click to onboard your first device — grab a one-shot access/secret from the top right of the Devices page, then run the curl command from the dialog on the target host.',
                  )}
                </span>
              </button>
            ) : (
              <>
                {/* 「试试这些」= 下面这组既有 PromptCard（samplePrompts(4)）。
                    tasks.md 2.6 说的「建议提示词卡」由它满足，本任务不新增第二组。
                    标题必须留在**非空分支**里：外层那个 div-10 里是三元，零设备时
                    渲染的是 onboarding CTA，挂着「试试这些」标题会与内容对不上。 */}
                <h2 className="mb-2 text-xs font-medium uppercase tracking-wide text-zinc-500">
                  {tr('试试这些', 'Try these')}
                </h2>
                <div className="grid grid-cols-1 gap-2.5 sm:grid-cols-2">
                  {prompts.map((p) => (
                    <PromptCard
                      key={p.titleEn}
                      title={tr(p.titleZh, p.titleEn)}
                      description={tr(p.descZh, p.descEn)}
                      icon={p.icon}
                      onClick={() => void startSession(tr(p.promptZh, p.promptEn))}
                    />
                  ))}
                </div>
              </>
            )}
          </div>
        </div>
      </div>
    </main>
  );
}
