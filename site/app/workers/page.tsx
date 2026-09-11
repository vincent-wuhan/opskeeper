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
  title: 'Worker roles',
  description:
    'The seven operational worker roles in OpsKeeper: alerter, investigator, critic, reviewer, repairer, verifier, reporter — plus specialist skills.',
};

const operational = [
  {
    name: 'alerter',
    role: 'Intake',
    desc: 'Aggregates alerts from Prometheus, Loki, Tempo, webhooks, and on-call channels. Three static rules for PG / Redis / Host plus nine DIAGNOSIS_SKILL_MAP entries and an LLM semantic_dedup stage with circuit breaker.',
    icon: Radio,
    allows: ['read:alerts', 'read:topics', 'dedup:LLM', 'dedup:rules'],
    maxTurns: 12,
  },
  {
    name: 'investigator',
    role: 'Root-cause analysis',
    desc: 'Read-only causal-chain tracer across metrics, logs, traces, git, hosts, and topology. Traces back to patient zero and returns an evidence chain with confidence.',
    icon: Eye,
    allows: ['read:metrics', 'read:logs', 'read:traces', 'read:git', 'read:topology'],
    maxTurns: 40,
  },
  {
    name: 'critic',
    role: 'RCA audit',
    desc: 'Post-RCA auditor on severity ≥ critical. Checks the evidence chain, returns a needs_correction flag. Will not invent issues that are not there.',
    icon: ShieldCheck,
    allows: ['read:rca', 'read:evidence'],
    maxTurns: 8,
  },
  {
    name: 'reviewer',
    role: 'Pre-flight approval',
    desc: 'Second pair of eyes on every mutating or destructive action before HITL. Approves only the minimum-necessary blast radius.',
    icon: CheckCircle2,
    allows: ['read:proposal', 'read:incident'],
    maxTurns: 6,
  },
  {
    name: 'repairer',
    role: 'Narrow mutator',
    desc: 'Executes mutating actions. Each action must match one approved incident, manifest, resource, command, and payload hash. Otherwise the call fails closed.',
    icon: TerminalSquare,
    allows: ['execute:approved-only'],
    maxTurns: 1,
  },
  {
    name: 'verifier',
    role: 'Independent verification',
    desc: 'Calls recovery.verify only. Four-metric allowlist, three warning tiers. Returns a VerifiedDelta for the manager.',
    icon: Gauge,
    allows: ['read:recovery.verify'],
    maxTurns: 4,
  },
  {
    name: 'reporter',
    role: 'Postmortem writer',
    desc: 'Writes structured period reports from pre-computed ReportFacts. Resource trends, monitoring coverage, changes — never fabricated numbers.',
    icon: GitBranch,
    allows: ['read:facts', 'write:report'],
    maxTurns: 10,
  },
];

const specialists = [
  {
    name: 'specialist-sre',
    desc: 'Site reliability patterns: deployment safety, feature flag analysis, dependency blast radius.',
    icon: Server,
  },
  {
    name: 'specialist-network',
    desc: 'Network-layer diagnosis: DNS, LB, BGP, route propagation, packet drops.',
    icon: Network,
  },
  {
    name: 'specialist-compute',
    desc: 'CPU scheduling, container limits, kernel cgroup pressure, throttling detection.',
    icon: Cog,
  },
  {
    name: 'specialist-disk',
    desc: 'Filesystem pressure, IOPS saturation, replica lag, journal replay.',
    icon: HardDrive,
  },
  {
    name: 'specialist-ops',
    desc: 'Operational glue: change windows, on-call rotation, communication templates.',
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
  Multi-source alert aggregation + semantic dedup.
  Circuit breaker on the LLM dedup stage.`;

export default function WorkersPage() {
  return (
    <>
      <Section className="pt-20 pb-12">
        <div className="max-w-3xl">
          <div className="mb-3 inline-flex items-center gap-2 rounded-full border border-white/10 bg-white/5 px-3 py-1 text-xs font-medium text-accent-300">
            Worker roles
          </div>
          <h1 className="text-balance text-4xl font-semibold tracking-tight text-white sm:text-5xl">
            Seven operational roles. Five specialists. One manager.
          </h1>
          <p className="mt-5 text-lg text-ink-300">
            Every worker in OpsKeeper has a narrow contract: a defined role, a defined
            tool allowlist, and a defined maximum turn budget. The manager decides who
            runs when.
          </p>
        </div>
      </Section>

      <Section className="py-10">
        <SectionHeader
          eyebrow="Operational workers"
          title="Phases map to roles."
          description="Each operational worker is bound to one or more phases of the closed loop. The contract is declared in skill_meta.yaml and enforced at runtime."
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
                  ≤ {w.maxTurns} turns
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
          eyebrow="Specialist skills"
          title="Attach domain experts to incidents."
          description="Specialists live under agents/ and join incidents by routing rule, not by the manager's whim. Use them to enrich an RCA or to draft a postmortem section."
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
              eyebrow="Skill metadata"
              title="A worker is a skill_meta.yaml plus a tool allowlist."
              description="Skills live in Nacos Config with a 30s polling hot-reload, plus a local fallback for air-gapped installs. The control plane reads the same schema every time."
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
