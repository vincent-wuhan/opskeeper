import { Activity, LayoutDashboard, MessagesSquare, ShieldCheck } from 'lucide-react';
import { Button } from '@/components/button';
import { Section, SectionHeader } from '@/components/section';
import { DEMO_URLS } from '@/lib/demos';

export const metadata = {
  title: '在线演示',
  description:
    '体验 OpsKeeper 控制台、AgentTeams Dashboard 和 AgentTeams Element 三个联通环境，还原多智能体运维事件闭环。',
};

const demos = [
  {
    title: 'OpsKeeper 控制台',
    url: DEMO_URLS.opskeeper,
    role: '事件指挥中心',
    description:
      '查看事件、证据、根因分析、处置提案、人工审批、审计记录与恢复验证的完整链路。',
    icon: ShieldCheck,
  },
  {
    title: 'AgentTeams Dashboard',
    url: DEMO_URLS.agentTeamsDashboard,
    role: '团队与插件控制面',
    description:
      '观察智能体团队状态、任务协同、插件安装，以及 OpsKeeper Runtime 观测看板。',
    icon: LayoutDashboard,
  },
  {
    title: 'AgentTeams Element',
    url: DEMO_URLS.agentTeamsElement,
    role: '协同房间',
    description:
      '从房间对话查看 Manager 调度、Worker 响应、Skills/MCP 调用证据、人工审批和异常处理。',
    icon: MessagesSquare,
  },
];

export default function DemoZhPage() {
  return (
    <Section className="py-20 md:py-28">
      <SectionHeader
        eyebrow="在线演示"
        title="一个事件，三个联通视角。"
        description="这组公网环境也是 GOAI Agent Infra 决赛演示环境，演练期间数据可能被重置。"
      />
      <div className="mt-12 grid gap-4 md:grid-cols-3">
        {demos.map((demo) => (
          <article
            key={demo.title}
            className="flex h-full flex-col rounded-2xl border border-white/10 bg-gradient-to-b from-white/[0.04] to-transparent p-6"
          >
            <span className="inline-flex h-10 w-10 items-center justify-center rounded-md bg-accent-500/10 text-accent-300">
              <demo.icon className="h-5 w-5" />
            </span>
            <h2 className="mt-4 text-lg font-semibold text-white">{demo.title}</h2>
            <div className="mt-1 inline-flex items-center gap-1.5 text-xs font-medium text-accent-300">
              <Activity className="h-3.5 w-3.5" />
              {demo.role}
            </div>
            <p className="mt-3 flex-1 text-sm leading-relaxed text-ink-300">
              {demo.description}
            </p>
            <div className="mt-6 flex flex-col gap-3">
              <Button href={demo.url} external>
                进入环境
              </Button>
              <span className="break-all text-xs text-ink-500">{demo.url}</span>
            </div>
          </article>
        ))}
      </div>
      <p className="mt-8 rounded-xl border border-white/10 bg-white/[0.03] p-4 text-sm leading-relaxed text-ink-300">
        推荐路径：先在 AgentTeams Element 发起运维事件，再到 AgentTeams Dashboard
        查看团队与 Runtime 状态，最后进入 OpsKeeper 控制台核对权威时间线、审批、审计链和恢复验证。
      </p>
    </Section>
  );
}
