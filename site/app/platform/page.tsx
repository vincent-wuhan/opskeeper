import {
  Radio,
  GitMerge,
  Eye,
  ShieldCheck,
  CheckCircle2,
  TerminalSquare,
  Gauge,
  GitBranch,
  ArrowRight,
  Database,
  Activity,
  Bot,
  Cpu,
} from 'lucide-react';
import Link from 'next/link';
import { Section, SectionHeader } from '@/components/section';
import { Button } from '@/components/button';
import { CodeBlock } from '@/components/code-block';

export const metadata = {
  title: 'Platform',
  description:
    'The OpsKeeper platform: closed-loop orchestrator, append-only ledger, manager-style worker dispatch, and the safety boundary that keeps mutating actions under human control.',
};

const phases = [
  {
    key: 'detected',
    title: 'Detected',
    desc: 'Multi-source alert intake from Prometheus, Loki, Tempo, webhooks, and on-call channels. Static rules for PG/Redis/Host pre-group incoming alerts.',
    icon: Radio,
    outputs: ['AlertSet', 'SourceMap'],
  },
  {
    key: 'correlated',
    title: 'Correlated',
    desc: 'Semantic de-duplication across sources using the DIAGNOSIS_SKILL_MAP and an LLM semantic_dedup stage with a circuit breaker.',
    icon: GitMerge,
    outputs: ['Incident', 'CorrelationID'],
  },
  {
    key: 'investigated',
    title: 'Investigated',
    desc: 'Read-only root-cause analysis across metrics, logs, traces, git, hosts, and topology. Returns an evidence chain with confidence.',
    icon: Eye,
    outputs: ['RCAReport', 'Evidence[]'],
  },
  {
    key: 'critiqued',
    title: 'Critiqued',
    desc: 'On severity ≥ critical, a peer critic audits the RCA. Emits needs_correction without inventing issues — a fast no-op most of the time.',
    icon: ShieldCheck,
    outputs: ['CritiqueFlag'],
  },
  {
    key: 'approved',
    title: 'Approved',
    desc: 'Human-in-the-loop on a pending proposal. Exact target match on resource, command, and payload hash. No approval → no execution.',
    icon: CheckCircle2,
    outputs: ['Proposal', 'Approval'],
  },
  {
    key: 'recovered',
    title: 'Recovered',
    desc: 'Narrowly authorized repair execution. Unknown tools and cross-resource targets fail closed. Every action lands in the audit ledger.',
    icon: TerminalSquare,
    outputs: ['ActionReceipt', 'AuditEvent'],
  },
  {
    key: 'verified',
    title: 'Verified',
    desc: 'Independent verifier calls recovery.verify only. Four-metric allowlist, three warning tiers, returns a VerifiedDelta for the manager.',
    icon: Gauge,
    outputs: ['VerifiedDelta'],
  },
  {
    key: 'postmortem',
    title: 'Postmortem',
    desc: 'Reporter writes an eight-section postmortem from pre-computed ReportFacts. Resource trends, monitoring coverage, changes — never fabricated.',
    icon: GitBranch,
    outputs: ['Postmortem', 'GitArtifact'],
  },
];

const dataPlane = [
  {
    t: 'PostgreSQL',
    d: 'Incident memory, append-only ledger (loop_event_log / loop_state / loop_contract), and MySQL GET_LOCK advisory locks for orchestrator serialization.',
  },
  {
    t: 'Qdrant',
    d: 'Vector retrieval over historical incidents. Keyword recall + RRF ranking. Retained candidate-decision evidence per query.',
  },
  {
    t: 'OpenTelemetry',
    d: 'W3C traceparent propagation end-to-end across worker → MCP proxy → control plane → web console.',
  },
  {
    t: 'Nacos Config',
    d: 'Skill registry with HTTP 2.x Config API, local fallback, and 30s polling hot-reload. skill_meta.yaml schema.',
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

export default function PlatformPage() {
  return (
    <>
      <Section className="pt-20 pb-12">
        <div className="max-w-3xl">
          <div className="mb-3 inline-flex items-center gap-2 rounded-full border border-white/10 bg-white/5 px-3 py-1 text-xs font-medium text-accent-300">
            <Cpu className="h-3.5 w-3.5" />
            Platform
          </div>
          <h1 className="text-balance text-4xl font-semibold tracking-tight text-white sm:text-5xl">
            One platform. Eight phases. Zero silent recovery.
          </h1>
          <p className="mt-5 text-lg text-ink-300">
            The OpsKeeper control plane runs incidents through an explicit state machine. Each
            transition is an append-only ledger event with a defined guard. Every mutating
            action is observable, replayable, and provably authorized.
          </p>
          <div className="mt-6 flex flex-wrap gap-3">
            <Button href="/docs/architecture">Architecture reference</Button>
            <Button href="/security" variant="secondary">Safety model</Button>
          </div>
        </div>
      </Section>

      <Section id="closed-loop" className="py-12 md:py-16">
        <SectionHeader
          eyebrow="Closed loop"
          title="Phases, guards, and ledger events"
          description="Every incident runs through the same eight phases. Each phase has explicit inputs, outputs, and a guard that decides whether the loop can advance."
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
                  Outputs
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
              eyebrow="Contracts"
              title="A proposal is a contract, not a wish."
              description="OpsKeeper's approved phase requires a proposal with explicit blast radius, exact-match guards, and audit hooks. The control plane will refuse to dispatch a recovery without all of them."
            />
            <ul className="mt-6 space-y-2 text-sm text-ink-300">
              <li className="flex items-start gap-2"><CheckCircle2 className="mt-0.5 h-4 w-4 text-accent-400" /> resource, command, and payload hash must match the approved incident exactly</li>
              <li className="flex items-start gap-2"><CheckCircle2 className="mt-0.5 h-4 w-4 text-accent-400" /> unknown tools and cross-resource targets fail closed</li>
              <li className="flex items-start gap-2"><CheckCircle2 className="mt-0.5 h-4 w-4 text-accent-400" /> audit events append on dispatch and seal on completion</li>
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
          eyebrow="Data plane"
          title="What the closed loop is built on."
          description="The data plane is intentionally boring: a relational store, a vector store, a config registry, and a tracing standard. Replace any of them without rewriting the loop."
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
            Want to extend it?
          </div>
          <h2 className="mt-3 max-w-2xl text-balance text-3xl font-semibold tracking-tight text-white sm:text-4xl">
            Drop a new worker, register a skill, watch the loop route to it.
          </h2>
          <p className="mt-3 max-w-xl text-ink-200">
            OpsKeeper ships a manager-style dispatcher with safety levels L0–L3. New workers
            join by registering a Skill and declaring their tool allowlist.
          </p>
          <div className="mt-6 flex flex-wrap gap-3">
            <Button href="/docs/integrations">Worker plugin guide</Button>
            <Button href="/workers" variant="secondary">Browse worker roles</Button>
          </div>
        </div>
      </Section>
    </>
  );
}
