import { Activity, LayoutDashboard, MessagesSquare, ShieldCheck } from 'lucide-react';
import { Button } from '@/components/button';
import { Section, SectionHeader } from '@/components/section';
import { DEMO_URLS } from '@/lib/demos';

export const metadata = {
  title: 'Live demos',
  description:
    'Explore the OpsKeeper console, AgentTeams Dashboard, and AgentTeams Element environments used for the end-to-end competition demonstration.',
};

const demos = [
  {
    title: 'OpsKeeper Console',
    url: DEMO_URLS.opskeeper,
    role: 'Incident command center',
    description:
      'Inspect incidents, evidence, root-cause analysis, proposals, approvals, audit records, and recovery verification.',
    icon: ShieldCheck,
  },
  {
    title: 'AgentTeams Dashboard',
    url: DEMO_URLS.agentTeamsDashboard,
    role: 'Team and plugin control plane',
    description:
      'Follow agent-team state, task coordination, plugin installation, and the OpsKeeper runtime observation widget.',
    icon: LayoutDashboard,
  },
  {
    title: 'AgentTeams Element',
    url: DEMO_URLS.agentTeamsElement,
    role: 'Collaboration room',
    description:
      'See manager dispatch, worker replies, skills/MCP evidence, human approval, and exception handling in the team conversation.',
    icon: MessagesSquare,
  },
];

export default function DemoPage() {
  return (
    <Section className="py-20 md:py-28">
      <SectionHeader
        eyebrow="Live demo"
        title="One incident, three connected views."
        description="These hosted environments are also used for the GOAI Agent Infra final demonstration. Data may be reset between rehearsal runs."
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
                Open environment
              </Button>
              <span className="break-all text-xs text-ink-500">{demo.url}</span>
            </div>
          </article>
        ))}
      </div>
      <p className="mt-8 rounded-xl border border-white/10 bg-white/[0.03] p-4 text-sm leading-relaxed text-ink-300">
        Recommended path: start in AgentTeams Element to trigger the operational incident,
        use AgentTeams Dashboard to inspect team and runtime state, then open OpsKeeper Console
        for the authoritative incident timeline, approvals, audit chain, and recovery verification.
      </p>
    </Section>
  );
}
