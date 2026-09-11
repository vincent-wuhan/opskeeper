import Link from 'next/link';
import {
  ArrowRight,
  ShieldCheck,
  Bot,
  GitBranch,
  Radio,
  Database,
  Activity,
  CheckCircle2,
  TerminalSquare,
  GitMerge,
  Gauge,
  Lock,
  Cpu,
  Eye,
} from 'lucide-react';
import { Button } from '@/components/button';
import { Section, SectionHeader } from '@/components/section';
import { CodeBlock } from '@/components/code-block';
import { TechMarquee } from '@/components/tech-marquee';

export const metadata = {
  title: '面向多智能体事件响应的可审计运维平台',
  description:
    'OpsKeeper 是面向多智能体事件响应的可审计运维平台。闭环式 告警 → 证据 → RCA → 审批 → 恢复 → 验证 → 复盘。',
};

const phases = [
  { key: 'detected', label: '检测', desc: '多源告警接入 + 语义去重', icon: Radio },
  { key: 'correlated', label: '关联', desc: '跨源事件分组与去重', icon: GitMerge },
  { key: 'investigated', label: '调查', desc: '只读因果链根因追踪', icon: Eye },
  { key: 'critiqued', label: '评审', desc: '对 RCA 证据链的同行审计', icon: ShieldCheck },
  { key: 'approved', label: '审批', desc: '对挂起提案的人工审批', icon: CheckCircle2 },
  { key: 'recovered', label: '恢复', desc: '窄域授权的可变更动作', icon: TerminalSquare },
  { key: 'verified', label: '验证', desc: '基于 4 项指标白名单的独立验证', icon: Gauge },
  { key: 'postmortem', label: '复盘', desc: '由预计算事实生成的 8 段报告', icon: GitBranch },
];

const workers = [
  {
    name: 'alerter',
    role: '接入',
    desc: '聚合来自 Prometheus / Loki / Tempo 以及外部通道的告警。通过静态规则和 LLM 语义去重，配合熔断器。',
    icon: Radio,
  },
  {
    name: 'investigator',
    role: '根因',
    desc: '跨指标、日志、追踪、代码、主机、拓扑的只读因果链追踪器，附带置信度返回证据。',
    icon: Eye,
  },
  {
    name: 'critic',
    role: '审计',
    desc: '在关键严重度下检查证据链的同行审计员。只发出 needs_correction 标记，不发明问题。',
    icon: ShieldCheck,
  },
  {
    name: 'reviewer',
    role: '预审',
    desc: '每一个可变更动作的第二双眼睛。仅批准最小必要的爆炸半径。',
    icon: CheckCircle2,
  },
  {
    name: 'repairer',
    role: '修复',
    desc: '窄域变更执行器。动作必须精确匹配一个已审批的事件、清单、资源、命令与 payload 哈希。',
    icon: TerminalSquare,
  },
  {
    name: 'verifier',
    role: '验证',
    desc: '只调用 recovery.verify。4 项指标白名单，3 级告警分级，向管理器返回 VerifiedDelta。',
    icon: Gauge,
  },
  {
    name: 'reporter',
    role: '复盘',
    desc: '基于预计算的 ReportFacts 撰写结构化周期报告。资源趋势、监控覆盖、变更 —— 绝不杜撰。',
    icon: GitBranch,
  },
];

const pillars = [
  {
    title: '智能体协作框架',
    desc: 'Manager 风格的派发器，显式安全级别（L0–L3），7 个工作流角色，append-only 账本支撑的 7 阶段闭环。',
    icon: Cpu,
  },
  {
    title: '技能生态',
    desc: '由 Nacos 支撑的技能注册中心，支持热加载，HTTP 2.x Config API，本地降级，并为 OpsKeeper Worker 插件提供 stdio MCP 服务。',
    icon: Database,
  },
  {
    title: '可观测与审计',
    desc: 'OpenTelemetry 追踪上下文、Prometheus 指标、Loki 日志、Tempo 追踪、Grafana 仪表盘 —— 外加 append-only 事件日志与 SHA256 链式提案审计。',
    icon: Activity,
  },
];

const safetyItems = [
  '诊断工具默认只读 —— 可变更动作必须先有挂起的提案。',
  '在派发任何恢复命令前，必须经过显式的人工审批。',
  '对资源、命令、payload 哈希进行精确目标匹配 —— 未知工具和跨资源目标默认拒绝。',
  'loop_event_log 由数据库触发器强制 append-only；每个变更提案的跃迁都做 SHA256 链式记录。',
  '独立验证器把"行动者"和"裁判者"分开，保证每一次恢复都有独立判定。',
];

const codeSnippet = `# 克隆仓库，用根目录 compose 拉起整套本地环境
git clone https://github.com/louloulin/opskeeper.git
cd opskeeper
cp deploy/demo.env.example .env
docker compose up -d --build

# 把 4 个可复现 PostgreSQL 场景写入事件记忆库
go run ./cmd/incident-seed \\
  -dsn "postgres://opskeeper:opskeeper@localhost:5432/opskeeper?sslmode=disable" \\
  -dir deploy/incident-events

# 在 Web 控制台检视 timeline、证据、提案与审计
# → API + Swagger UI 见 http://localhost:8080`;

const installSnippet = `# Worker 插件 —— 从 AgentTeams Dashboard 安装
# （热部署：qwenpaw plugin install <path> --force）

# 或直接为任何 Worker 启动 stdio MCP 代理
cd plugins/opskeeper-teamharness
OPSKEEPER_BACKEND_URL=http://localhost:8080 \\
OPSKEEPER_GATEWAY_KEY="$GATEWAY_KEY" \\
python3 mcp/server.py`;

export default function HomeZhPage() {
  return (
    <>
      {/* Hero */}
      <Section className="relative pt-20 pb-24 md:pt-28 md:pb-32">
        <div
          aria-hidden
          className="pointer-events-none absolute inset-0 -z-10 opacity-[0.035] mix-blend-overlay"
          style={{
            backgroundImage:
              "url(\"data:image/svg+xml;utf8,<svg xmlns='http://www.w3.org/2000/svg' width='240' height='240' viewBox='0 0 240 240'><filter id='n'><feTurbulence type='fractalNoise' baseFrequency='0.9' numOctaves='2' stitchTiles='stitch'/><feColorMatrix type='matrix' values='0 0 0 0 1 0 0 0 0 1 0 0 0 0 1 0 0 0 0.5 0'/></filter><rect width='100%' height='100%' filter='url(%23n)'/></svg>\")",
            backgroundSize: '240px 240px',
          }}
        />
        <div className="grid gap-12 lg:grid-cols-12 lg:gap-16 items-center">
          <div className="lg:col-span-7">
            <div className="mb-5 inline-flex items-center gap-2 rounded-full border border-white/10 bg-white/5 px-3 py-1 text-xs font-medium text-ink-200">
              <span className="inline-block h-1.5 w-1.5 rounded-full bg-accent-400 animate-pulse" />
              v2026.09.03 · Apache-2.0 · 开源
            </div>
            <h1 className="text-balance text-4xl font-semibold tracking-tight text-white sm:text-5xl md:text-6xl">
              让多智能体事件响应{' '}
              <span className="bg-gradient-to-br from-white to-accent-300 bg-clip-text text-transparent">
                全程可审计
              </span>
            </h1>
            <p className="mt-6 max-w-2xl text-pretty text-lg leading-relaxed text-ink-300">
              OpsKeeper 把<strong className="text-white">告警</strong>、
              <strong className="text-white">证据</strong>、
              <strong className="text-white">根因分析</strong>、
              <strong className="text-white">人工审批</strong>、
              <strong className="text-white">窄域授权恢复</strong>、
              <strong className="text-white">独立验证</strong>和
              <strong className="text-white">事后复盘</strong>
              串成同一条闭环。
              没有提案、人工审批、审计记录这三件套，任何变更动作都不会被执行。
            </p>
            <div className="mt-8 flex flex-wrap items-center gap-3">
              <Button href="/zh/docs/getting-started">快速开始</Button>
              <Button href="/zh/platform" variant="secondary">
                查看闭环工作流
              </Button>
              <Button href="https://github.com/louloulin/opskeeper" variant="ghost" external>
                在 GitHub 上加星
              </Button>
            </div>
            <div className="mt-8 flex flex-wrap items-center gap-x-6 gap-y-2 text-xs text-ink-400">
              <span className="inline-flex items-center gap-1.5"><ShieldCheck className="h-3.5 w-3.5 text-accent-400" /> 默认安全</span>
              <span className="inline-flex items-center gap-1.5"><Lock className="h-3.5 w-3.5 text-accent-400" /> 哈希链式审计</span>
              <span className="inline-flex items-center gap-1.5"><Database className="h-3.5 w-3.5 text-accent-400" /> Postgres + Qdrant</span>
              <span className="inline-flex items-center gap-1.5"><Cpu className="h-3.5 w-3.5 text-accent-400" /> 7 个工作流角色</span>
            </div>
          </div>
          <div className="lg:col-span-5">
            <CodeBlock language="bash" title="安装 · 本地环境">
              {installSnippet}
            </CodeBlock>
          </div>
        </div>
      </Section>

      {/* 信任带 */}
      <Section className="py-10">
        <div className="grid grid-cols-2 gap-4 sm:grid-cols-4">
          {[
            { k: '7', l: 'Operational Worker 角色' },
            { k: '8', l: '闭环阶段' },
            { k: '4', l: '可复现事件场景' },
            { k: '100%', l: '审计重放覆盖' },
          ].map((s) => (
            <div
              key={s.l}
              className="rounded-xl border border-white/10 bg-white/[0.03] p-5"
            >
              <div className="text-3xl font-semibold text-white">{s.k}</div>
              <div className="mt-1 text-sm text-ink-300">{s.l}</div>
            </div>
          ))}
        </div>
      </Section>

      <TechMarquee label="基于你已经跑的开源技术栈搭建" />
      <Section id="closed-loop" className="py-20 md:py-28">
        <SectionHeader
          eyebrow="闭环"
          title="八个阶段，每一次跃迁都可追溯。"
          description="OpsKeeper 用显式的状态机驱动事件流转。每个阶段都是 append-only 账本上的一笔事件，每次跃迁都有明确的护栏，整条闭环都能从零重放。"
        />
        <div className="mt-12 grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
          {phases.map((p, i) => (
            <div
              key={p.key}
              className="group relative overflow-hidden rounded-xl border border-white/10 bg-white/[0.02] p-5 transition-colors hover:border-accent-500/40 hover:bg-white/[0.04]"
            >
              <div className="flex items-center justify-between">
                <span className="font-mono text-xs text-ink-400">phase 0{i + 1}</span>
                <p.icon className="h-4 w-4 text-accent-400" />
              </div>
              <div className="mt-3 text-lg font-semibold text-white">{p.label}</div>
              <div className="mt-1 text-sm text-ink-300">{p.desc}</div>
            </div>
          ))}
        </div>
      </Section>

      {/* Worker roles */}
      <Section className="py-20 md:py-28">
        <SectionHeader
          eyebrow="Worker 角色"
          title="专业分工，统一调度。"
          description="七个 Operational 角色由 Manager 风格的派发器统一协调。每个角色都有明确的工具白名单和契约，闭环不依赖任何单一 Agent 耍小聪明。"
        />
        <div className="mt-12 grid gap-4 md:grid-cols-2 lg:grid-cols-3">
          {workers.map((w) => (
            <div
              key={w.name}
              className="rounded-xl border border-white/10 bg-white/[0.02] p-6 transition-colors hover:bg-white/[0.04]"
            >
              <div className="flex items-center gap-3">
                <span className="inline-flex h-9 w-9 items-center justify-center rounded-md border border-white/10 bg-white/5 text-accent-300">
                  <w.icon className="h-4 w-4" />
                </span>
                <div>
                  <div className="font-mono text-xs uppercase tracking-wider text-ink-400">
                    {w.role}
                  </div>
                  <div className="text-lg font-semibold text-white">{w.name}</div>
                </div>
              </div>
              <p className="mt-4 text-sm leading-relaxed text-ink-300">{w.desc}</p>
            </div>
          ))}
          <div className="rounded-xl border border-dashed border-white/10 p-6 flex flex-col justify-center">
            <div className="text-sm font-medium text-white">还有专家技能</div>
            <p className="mt-2 text-sm text-ink-300">
              <code className="font-mono text-xs text-accent-300">specialist-sre</code>、
              <code className="font-mono text-xs text-accent-300">specialist-network</code>、
              <code className="font-mono text-xs text-accent-300">specialist-compute</code>、
              <code className="font-mono text-xs text-accent-300">specialist-disk</code> 和
              <code className="font-mono text-xs text-accent-300">specialist-ops</code> 随仓库
              <code className="font-mono text-xs text-accent-300"> agents/</code> 目录一起发布，
              会按路由规则挂载到对应事件上。
            </p>
            <Link
              href="/zh/workers"
              className="mt-4 inline-flex items-center gap-1.5 text-sm text-accent-300 hover:text-accent-200"
            >
              查看 Worker 契约 <ArrowRight className="h-3.5 w-3.5" />
            </Link>
          </div>
        </div>
      </Section>

      {/* Safety boundary */}
      <Section className="py-20 md:py-28">
        <div className="grid gap-12 lg:grid-cols-12 lg:items-center">
          <div className="lg:col-span-5">
            <SectionHeader
              eyebrow="安全边界"
              title="诊断只读，恢复写入 —— 而且必须有人点头。"
              description="OpsKeeper 在编排层把读和写分开。读工具始终可用，写工具必须先有提案、明确的人工审批人，以及精确的资源 / 命令 / payload 哈希匹配。"
            />
            <div className="mt-8">
              <Button href="/zh/security" variant="secondary">
                阅读安全模型
              </Button>
            </div>
          </div>
          <div className="lg:col-span-7">
            <div className="rounded-2xl border border-white/10 bg-white/[0.02] p-2">
              <div className="rounded-xl bg-ink-900/60 p-6">
                <ul className="space-y-4">
                  {safetyItems.map((s, i) => (
                    <li key={i} className="flex items-start gap-3">
                      <CheckCircle2 className="mt-0.5 h-4 w-4 flex-none text-accent-400" />
                      <span className="text-sm text-ink-100">{s}</span>
                    </li>
                  ))}
                </ul>
              </div>
            </div>
          </div>
        </div>
      </Section>

      {/* Pillars */}
      <Section className="py-20 md:py-28">
        <SectionHeader
          eyebrow="三大支柱"
          title="开箱即用。"
          description="OpsKeeper 是一个平台，三套紧耦合的子系统。每一套单独拿出来都可用、可观测。"
        />
        <div className="mt-12 grid gap-4 md:grid-cols-3">
          {pillars.map((p) => (
            <div
              key={p.title}
              className="rounded-2xl border border-white/10 bg-gradient-to-b from-white/[0.04] to-transparent p-6"
            >
              <span className="inline-flex h-10 w-10 items-center justify-center rounded-md bg-accent-500/10 text-accent-300">
                <p.icon className="h-5 w-5" />
              </span>
              <h3 className="mt-4 text-lg font-semibold text-white">{p.title}</h3>
              <p className="mt-2 text-sm leading-relaxed text-ink-300">{p.desc}</p>
            </div>
          ))}
        </div>
      </Section>

      {/* Code */}
      <Section className="py-20 md:py-28">
        <div className="grid gap-10 lg:grid-cols-12 lg:items-start">
          <div className="lg:col-span-5">
            <SectionHeader
              eyebrow="演示"
              title="把一个真实事件端到端写入闭环。"
              description="deploy/incident-events/ 内置 4 个可复现的 PostgreSQL 场景。选一个写入事件记忆库，然后在 Web Console 里看闭环从检测到复盘跑完全程。"
            />
            <ul className="mt-6 space-y-2 text-sm text-ink-300">
              <li className="flex items-center gap-2"><CheckCircle2 className="h-4 w-4 text-accent-400" /> pg-connection-pool-exhaustion</li>
              <li className="flex items-center gap-2"><CheckCircle2 className="h-4 w-4 text-accent-400" /> pg-disk-io-saturation</li>
              <li className="flex items-center gap-2"><CheckCircle2 className="h-4 w-4 text-accent-400" /> pg-lock-wait-long-transaction</li>
              <li className="flex items-center gap-2"><CheckCircle2 className="h-4 w-4 text-accent-400" /> pg-replica-replay-lag</li>
            </ul>
            <div className="mt-8">
              <Button href="/zh/docs/getting-started" variant="secondary">
                跑一遍演示
              </Button>
            </div>
          </div>
          <div className="lg:col-span-7">
            <CodeBlock language="bash" title="seed · deploy/incident-events">
              {codeSnippet}
            </CodeBlock>
          </div>
        </div>
      </Section>

      {/* Integrations */}
      <Section className="py-20 md:py-28">
        <SectionHeader
          eyebrow="集成"
          title="和你已有的栈天然合得来。"
          description="OpsKeeper 自带 AgentTeams Dashboard 一方插件和标准可观测后端。MCP 服务同时支持 stdio 和 Streamable HTTP，任何 Worker 都能加入。"
        />
        <div className="mt-12 grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
          {[
            { t: 'AgentTeams Dashboard', d: '插件安装器 —— 侧边栏、路由、看板组件、详情面板、工具栏。' },
            { t: 'OpsKeeper TeamHarness', d: 'Worker/Manager 插件 + stdio MCP 代理（17 个工具，Bearer + HMAC + W3C traceparent）。' },
            { t: 'Prometheus + Loki + Tempo', d: '原生抓取配置，按 trace_id 关联日志 / 指标 / 追踪。' },
            { t: 'Grafana 仪表盘', d: '为闭环、审计账本、技能健康度预置仪表盘。' },
            { t: 'PostgreSQL', d: '事件记忆、append-only 账本、MySQL GET_LOCK 风格的咨询锁。' },
            { t: 'Qdrant', d: '向量检索 + 关键词召回 + RRF 融合排序，保留候选决策证据。' },
            { t: 'Nacos Config', d: '技能注册中心，30 秒轮询热加载，本地降级。' },
            { t: 'OpenTelemetry', d: '端到端 W3C traceparent 传递，覆盖 Worker → MCP → 控制平面。' },
          ].map((i) => (
            <div
              key={i.t}
              className="rounded-xl border border-white/10 bg-white/[0.02] p-4"
            >
              <div className="text-sm font-semibold text-white">{i.t}</div>
              <p className="mt-1 text-sm text-ink-300">{i.d}</p>
            </div>
          ))}
        </div>
      </Section>

      {/* CTA */}
      <Section className="py-20 md:py-28">
        <div className="relative overflow-hidden rounded-3xl border border-white/10 bg-gradient-to-br from-accent-500/20 via-ink-900 to-ink-950 p-10 md:p-14">
          <div className="pointer-events-none absolute -right-32 -top-32 h-80 w-80 rounded-full bg-accent-500/30 blur-3xl" />
          <div className="relative">
            <div className="flex flex-wrap items-center gap-2 text-xs uppercase tracking-wider text-accent-200">
              <Bot className="h-4 w-4" />
              开源 · Apache-2.0
            </div>
            <h2 className="mt-3 max-w-2xl text-balance text-3xl font-semibold tracking-tight text-white sm:text-4xl">
              把审计能力带进你的事件响应流程。
            </h2>
            <p className="mt-3 max-w-xl text-pretty text-base text-ink-200">
              五分钟内本地跑起闭环，然后把真实的告警流指过去。
            </p>
            <div className="mt-6 flex flex-wrap gap-3">
              <Button href="/zh/docs/getting-started">快速开始</Button>
              <Button href="/zh/use-cases" variant="secondary">查看应用场景</Button>
              <Button href="https://github.com/louloulin/opskeeper" variant="ghost" external>
                在 GitHub 上查看
              </Button>
            </div>
          </div>
        </div>
      </Section>
    </>
  );
}
