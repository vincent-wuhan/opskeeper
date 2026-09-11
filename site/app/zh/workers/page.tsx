import { Section, SectionHeader } from '@/components/section';
import { CodeBlock } from '@/components/code-block';
import {
  Radio,
  Eye,
  ShieldCheck,
  CheckCircle2,
  TerminalSquare,
  Gauge,
  GitBranch,
  Network,
  Server,
  HardDrive,
  Cog,
} from 'lucide-react';

export const metadata = {
  title: 'Worker 角色',
  description:
    'OpsKeeper 的七个 Operational Worker 角色：alerter / investigator / critic / reviewer / repairer / verifier / reporter，外加专家技能。',
};

const operational = [
  {
    name: 'alerter',
    role: '告警接入',
    desc: '聚合来自 Prometheus / Loki / Tempo / Webhook / On-call 的告警。三条 PG / Redis / Host 静态规则，九条 DIAGNOSIS_SKILL_MAP 条目，LLM 语义去重阶段配熔断器。',
    icon: Radio,
    allows: ['read:alerts', 'read:topics', 'dedup:LLM', 'dedup:rules'],
    maxTurns: 12,
  },
  {
    name: 'investigator',
    role: '根因分析',
    desc: '跨指标、日志、追踪、代码、主机、拓扑的只读因果链追踪器。回溯到 patient zero 并返回带置信度的证据链。',
    icon: Eye,
    allows: ['read:metrics', 'read:logs', 'read:traces', 'read:git', 'read:topology'],
    maxTurns: 40,
  },
  {
    name: 'critic',
    role: 'RCA 审计',
    desc: '在严重度 ≥ critical 时对 RCA 做同行审计。检查证据链，返回 needs_correction 标记。不会无中生有。',
    icon: ShieldCheck,
    allows: ['read:rca', 'read:evidence'],
    maxTurns: 8,
  },
  {
    name: 'reviewer',
    role: '预审',
    desc: '在 HITL 之前对每个可变更或破坏性动作做第二双眼睛。仅批准最小必要的爆炸半径。',
    icon: CheckCircle2,
    allows: ['read:proposal', 'read:incident'],
    maxTurns: 6,
  },
  {
    name: 'repairer',
    role: '窄域变更',
    desc: '执行可变更动作。每个动作必须精确匹配一个已审批的事件、清单、资源、命令和 payload 哈希，否则默认拒绝。',
    icon: TerminalSquare,
    allows: ['execute:approved-only'],
    maxTurns: 1,
  },
  {
    name: 'verifier',
    role: '独立验证',
    desc: '只调用 recovery.verify。四项指标白名单，三级告警分级，向 Manager 返回 VerifiedDelta。',
    icon: Gauge,
    allows: ['read:recovery.verify'],
    maxTurns: 4,
  },
  {
    name: 'reporter',
    role: '复盘撰写',
    desc: '由预计算的 ReportFacts 撰写结构化周期报告。资源趋势、监控覆盖、变更 —— 数字绝不杜撰。',
    icon: GitBranch,
    allows: ['read:facts', 'write:report'],
    maxTurns: 10,
  },
];

const specialists = [
  {
    name: 'specialist-sre',
    desc: '站点可靠性模式：发布安全、特性开关分析、依赖爆炸半径。',
    icon: Server,
  },
  {
    name: 'specialist-network',
    desc: '网络层诊断：DNS / LB / BGP / 路由传播 / 丢包。',
    icon: Network,
  },
  {
    name: 'specialist-compute',
    desc: 'CPU 调度、容器 limits、kernel cgroup 压力、throttling 检测。',
    icon: Cog,
  },
  {
    name: 'specialist-disk',
    desc: '文件系统压力、IOPS 饱和、副本延迟、journal replay。',
    icon: HardDrive,
  },
  {
    name: 'specialist-ops',
    desc: '运维胶水：变更窗口、On-call 轮值、沟通模板。',
    icon: Cog,
  },
];

const skillYaml = `# skills/alerter/skill_meta.yaml
name: alerter
role: intake
phase: detected,correlated
safety_level: L0
max_turns: 12
tool_allowlist:
  - read:alerts
  - read:topics
  - dedup:rules
  - dedup:LLM
inputs:
  - AlertSet
outputs:
  - Incident
  - CorrelationID
description: |
  多源告警聚合 + 语义去重。
  LLM 去重阶段带熔断器。`;

export default function WorkersZhPage() {
  return (
    <>
      <Section className="pt-20 pb-12">
        <div className="max-w-3xl">
          <div className="mb-3 inline-flex items-center gap-2 rounded-full border border-white/10 bg-white/5 px-3 py-1 text-xs font-medium text-accent-300">
            Worker 角色
          </div>
          <h1 className="text-balance text-4xl font-semibold tracking-tight text-white sm:text-5xl">
            七个 Operational 角色，五个专家技能，一个 Manager。
          </h1>
          <p className="mt-5 text-lg text-ink-300">
            OpsKeeper 里的每个 Worker 都有明确的契约：明确的角色、明确的工具白名单、明确的最大轮次预算。由 Manager 决定谁什么时候跑。
          </p>
        </div>
      </Section>

      <Section className="py-10">
        <SectionHeader
          eyebrow="Operational Worker"
          title="阶段和角色一一对应。"
          description="每个 Operational Worker 都绑定闭环中的一个或多个阶段。契约写在 skill_meta.yaml 里，运行期强制执行。"
        />
        <div className="mt-12 grid gap-4 md:grid-cols-2">
          {operational.map((w) => (
            <div
              key={w.name}
              className="rounded-2xl border border-white/10 bg-white/[0.02] p-6"
            >
              <div className="flex items-center gap-3">
                <span className="inline-flex h-10 w-10 items-center justify-center rounded-md border border-white/10 bg-white/5 text-accent-300">
                  <w.icon className="h-5 w-5" />
                </span>
                <div>
                  <div className="font-mono text-xs uppercase tracking-wider text-ink-400">
                    {w.role}
                  </div>
                  <div className="text-lg font-semibold text-white">{w.name}</div>
                </div>
                <span className="ml-auto rounded-md border border-white/10 bg-white/5 px-2 py-0.5 font-mono text-[11px] text-ink-200">
                  ≤ {w.maxTurns} 轮
                </span>
              </div>
              <p className="mt-4 text-sm text-ink-300">{w.desc}</p>
              <div className="mt-4 flex flex-wrap gap-1.5">
                {w.allows.map((a) => (
                  <span
                    key={a}
                    className="rounded-md border border-accent-500/20 bg-accent-500/10 px-2 py-0.5 font-mono text-[11px] text-accent-300"
                  >
                    {a}
                  </span>
                ))}
              </div>
            </div>
          ))}
        </div>
      </Section>

      <Section className="py-10 md:py-16">
        <SectionHeader
          eyebrow="专家技能"
          title="把领域专家挂到事件上。"
          description="专家技能放在 agents/ 下，按路由规则（而不是 Manager 心情）挂到事件上。用来丰富 RCA 或撰写复盘章节。"
        />
        <div className="mt-10 grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
          {specialists.map((s) => (
            <div
              key={s.name}
              className="rounded-xl border border-white/10 bg-white/[0.02] p-5"
            >
              <div className="flex items-center gap-2">
                <s.icon className="h-4 w-4 text-accent-400" />
                <span className="font-mono text-sm text-white">{s.name}</span>
              </div>
              <p className="mt-2 text-sm text-ink-300">{s.desc}</p>
            </div>
          ))}
        </div>
      </Section>

      <Section className="py-10 md:py-16">
        <div className="grid gap-10 lg:grid-cols-12 lg:items-start">
          <div className="lg:col-span-5">
            <SectionHeader
              eyebrow="技能元数据"
              title="Worker = skill_meta.yaml + 工具白名单。"
              description="技能放在 Nacos Config 里，30 秒轮询热加载；本地降级支持离线安装。控制平面读的是同一份 schema。"
            />
          </div>
          <div className="lg:col-span-7">
            <CodeBlock language="yaml" title="skills/alerter/skill_meta.yaml">
              {skillYaml}
            </CodeBlock>
          </div>
        </div>
      </Section>
    </>
  );
}
