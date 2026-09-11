import {
  Radio,
  GitMerge,
  Eye,
  ShieldCheck,
  CheckCircle2,
  TerminalSquare,
  Gauge,
  GitBranch,
  Database,
  Bot,
  Cpu,
} from 'lucide-react';
import { Section, SectionHeader } from '@/components/section';
import { Button } from '@/components/button';
import { CodeBlock } from '@/components/code-block';

export const metadata = {
  title: '平台',
  description:
    'OpsKeeper 平台：闭环式编排器、append-only 账本、Manager 风格的 Worker 派发，以及把可变更动作关在人工控制范围内的安全边界。',
};

const phases = [
  {
    key: 'detected',
    title: '检测',
    desc: '从 Prometheus / Loki / Tempo / Webhook / On-call 多源接入告警。静态规则负责 PG / Redis / Host 的预分组。',
    icon: Radio,
    outputs: ['AlertSet', 'SourceMap'],
  },
  {
    key: 'correlated',
    title: '关联',
    desc: '使用 DIAGNOSIS_SKILL_MAP + LLM 语义去重（带熔断器）做跨源去重。',
    icon: GitMerge,
    outputs: ['Incident', 'CorrelationID'],
  },
  {
    key: 'investigated',
    title: '调查',
    desc: '跨指标、日志、追踪、代码、主机、拓扑的只读根因分析，附带置信度的证据链。',
    icon: Eye,
    outputs: ['RCAReport', 'Evidence[]'],
  },
  {
    key: 'critiqued',
    title: '评审',
    desc: '在严重度 ≥ critical 时，由同行 critic 审计 RCA。只发 needs_correction，不发明问题 —— 多数时候是快速 no-op。',
    icon: ShieldCheck,
    outputs: ['CritiqueFlag'],
  },
  {
    key: 'approved',
    title: '审批',
    desc: '对挂起提案的人工审批。对资源、命令、payload 哈希做精确匹配。没有审批 → 不执行。',
    icon: CheckCircle2,
    outputs: ['Proposal', 'Approval'],
  },
  {
    key: 'recovered',
    title: '恢复',
    desc: '窄域授权的修复执行。未知工具和跨资源目标默认拒绝。所有动作落入审计账本。',
    icon: TerminalSquare,
    outputs: ['ActionReceipt', 'AuditEvent'],
  },
  {
    key: 'verified',
    title: '验证',
    desc: '独立验证器只调用 recovery.verify。四项指标白名单，三级告警分级，向 Manager 返回 VerifiedDelta。',
    icon: Gauge,
    outputs: ['VerifiedDelta'],
  },
  {
    key: 'postmortem',
    title: '复盘',
    desc: 'Reporter 由预计算的 ReportFacts 撰写八段式报告。资源趋势、监控覆盖、变更 —— 绝不杜撰。',
    icon: GitBranch,
    outputs: ['Postmortem', 'GitArtifact'],
  },
];

const dataPlane = [
  {
    t: 'PostgreSQL',
    d: '事件记忆、append-only 账本（loop_event_log / loop_state / loop_contract），以及 MySQL GET_LOCK 风格的咨询锁用于编排串行化。',
  },
  {
    t: 'Qdrant',
    d: '对历史事件做向量检索。关键字召回 + RRF 融合排序，每个查询保留候选决策证据。',
  },
  {
    t: 'OpenTelemetry',
    d: '端到端 W3C traceparent 传递，覆盖 Worker → MCP 代理 → 控制平面 → Web Console。',
  },
  {
    t: 'Nacos Config',
    d: '技能注册中心，HTTP 2.x Config API，本地降级，30 秒轮询热加载。skill_meta.yaml 是事实标准。',
  },
];

const contractExample = `phase: approved
proposal:
  incident_id: INC-PG-POOL-001
  worker: repairer
  blast_radius: pg.connection_pool / one-db
guard:
  required_approval: human
  exact_match:
    - resource
    - command
    - payload_hash
audit:
  on_dispatch: ledger.append
  on_complete: ledger.seal`;

export default function PlatformZhPage() {
  return (
    <>
      <Section className="pt-20 pb-12">
        <div className="max-w-3xl">
          <div className="mb-3 inline-flex items-center gap-2 rounded-full border border-white/10 bg-white/5 px-3 py-1 text-xs font-medium text-accent-300">
            <Cpu className="h-3.5 w-3.5" />
            平台
          </div>
          <h1 className="text-balance text-4xl font-semibold tracking-tight text-white sm:text-5xl">
            一个平台，八个阶段，零静默恢复。
          </h1>
          <p className="mt-5 text-lg text-ink-300">
            OpsKeeper 的控制平面用显式状态机驱动事件流转。每次跃迁都是一条带护栏的 append-only 账本事件。每一次变更都&ldquo;看得见、可重放、有据可查&rdquo;。
          </p>
          <div className="mt-6 flex flex-wrap gap-3">
            <Button href="/zh/docs/architecture">架构参考</Button>
            <Button href="/zh/security" variant="secondary">安全模型</Button>
          </div>
        </div>
      </Section>

      <Section id="closed-loop" className="py-12 md:py-16">
        <SectionHeader
          eyebrow="闭环"
          title="阶段、护栏与账本事件"
          description="每一个事件都流过同样的八个阶段，每个阶段都有明确的输入、输出和护栏，决定闭环能不能往下走。"
        />
        <div className="mt-12 space-y-3">
          {phases.map((p, i) => (
            <div
              key={p.key}
              className="grid gap-6 rounded-2xl border border-white/10 bg-white/[0.02] p-6 md:grid-cols-12 md:items-start"
            >
              <div className="md:col-span-1 font-mono text-xs text-ink-400">0{i + 1}</div>
              <div className="md:col-span-7">
                <div className="flex items-center gap-3">
                  <span className="inline-flex h-9 w-9 items-center justify-center rounded-md border border-white/10 bg-white/5 text-accent-300">
                    <p.icon className="h-4 w-4" />
                  </span>
                  <h3 className="text-lg font-semibold text-white">{p.title}</h3>
                </div>
                <p className="mt-3 text-sm text-ink-300">{p.desc}</p>
              </div>
              <div className="md:col-span-4">
                <div className="text-xs font-medium uppercase tracking-wider text-ink-400">
                  输出
                </div>
                <div className="mt-2 flex flex-wrap gap-1.5">
                  {p.outputs.map((o) => (
                    <span
                      key={o}
                      className="rounded-md border border-white/10 bg-white/5 px-2 py-0.5 font-mono text-[11px] text-ink-200"
                    >
                      {o}
                    </span>
                  ))}
                </div>
              </div>
            </div>
          ))}
        </div>
      </Section>

      <Section className="py-12 md:py-20">
        <div className="grid gap-10 lg:grid-cols-12 lg:items-start">
          <div className="lg:col-span-5">
            <SectionHeader
              eyebrow="契约"
              title="提案是契约，不是愿望清单。"
              description="OpsKeeper 的审批阶段要求一份写明爆炸半径、精确匹配护栏和审计钩子的提案。控制平面会在缺失任何一项时拒绝派发恢复动作。"
            />
            <ul className="mt-6 space-y-2 text-sm text-ink-300">
              <li className="flex items-start gap-2"><CheckCircle2 className="mt-0.5 h-4 w-4 text-accent-400" /> 资源、命令、payload 哈希必须与已审批事件精确一致</li>
              <li className="flex items-start gap-2"><CheckCircle2 className="mt-0.5 h-4 w-4 text-accent-400" /> 未知工具和跨资源目标默认拒绝</li>
              <li className="flex items-start gap-2"><CheckCircle2 className="mt-0.5 h-4 w-4 text-accent-400" /> 审计事件在派发时 append，完成时 seal</li>
            </ul>
          </div>
          <div className="lg:col-span-7">
            <CodeBlock language="yaml" title="loop_contract.yaml">
              {contractExample}
            </CodeBlock>
          </div>
        </div>
      </Section>

      <Section className="py-12 md:py-20">
        <SectionHeader
          eyebrow="数据平面"
          title="闭环的底座。"
          description="数据平面是故意做得朴素：一个关系库、一个向量库、一个配置中心、一套追踪标准。任何一个都能替换，不必重写闭环。"
        />
        <div className="mt-12 grid gap-4 md:grid-cols-2">
          {dataPlane.map((d) => (
            <div
              key={d.t}
              className="rounded-2xl border border-white/10 bg-white/[0.02] p-6"
            >
              <div className="flex items-center gap-3">
                <span className="inline-flex h-9 w-9 items-center justify-center rounded-md border border-white/10 bg-white/5 text-accent-300">
                  <Database className="h-4 w-4" />
                </span>
                <h3 className="text-lg font-semibold text-white">{d.t}</h3>
              </div>
              <p className="mt-3 text-sm text-ink-300">{d.d}</p>
            </div>
          ))}
        </div>
      </Section>

      <Section className="py-12 md:py-20">
        <div className="rounded-3xl border border-white/10 bg-gradient-to-br from-accent-500/15 via-ink-900 to-ink-950 p-10 md:p-14">
          <div className="flex flex-wrap items-center gap-2 text-xs uppercase tracking-wider text-accent-200">
            <Bot className="h-4 w-4" />
            想扩展它？
          </div>
          <h2 className="mt-3 max-w-2xl text-balance text-3xl font-semibold tracking-tight text-white sm:text-4xl">
            加一个新 Worker，注册一个技能，看闭环把它路由进来。
          </h2>
          <p className="mt-3 max-w-xl text-ink-200">
            OpsKeeper 自带 Manager 风格的派发器和 L0–L3 安全级别。新 Worker 只需要注册一个 Skill 并声明工具白名单即可加入。
          </p>
          <div className="mt-6 flex flex-wrap gap-3">
            <Button href="/zh/docs/integrations">Worker 插件指南</Button>
            <Button href="/zh/workers" variant="secondary">浏览 Worker 角色</Button>
          </div>
        </div>
      </Section>
    </>
  );
}
