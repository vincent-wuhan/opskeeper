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

export const metadata = {
  title: 'Auditable operations for multi-agent incident response',
  description:
    'OpsKeeper is the auditable operations platform for multi-agent incident response. Closed-loop alert → evidence → RCA → approval → recovery → verification → learning.',
};

const phases = [
  { key: 'detected', label: 'Detected', desc: 'Multi-source alert intake + semantic dedup', icon: Radio },
  { key: 'correlated', label: 'Correlated', desc: 'Cross-source grouping and de-duplication', icon: GitMerge },
  { key: 'investigated', label: 'Investigated', desc: 'Read-only causal-chain root-cause tracing', icon: Eye },
  { key: 'critiqued', label: 'Critiqued', desc: 'Peer audit of the RCA evidence chain', icon: ShieldCheck },
  { key: 'approved', label: 'Approved', desc: 'Human-in-the-loop on a pending proposal', icon: CheckCircle2 },
  { key: 'recovered', label: 'Recovered', desc: 'Narrowly authorized mutating action', icon: TerminalSquare },
  { key: 'verified', label: 'Verified', desc: 'Independent verification via 4-metric allowlist', icon: Gauge },
  { key: 'postmortem', label: 'Postmortem', desc: '8-section report from pre-computed facts', icon: GitBranch },
];

const workers = [
  {
    name: 'alerter',
    role: 'Intake',
    desc: 'Aggregates alerts from Prometheus, Loki, Tempo, and external channels. Deduplicates via static rules and LLM semantic dedup with circuit breaker.',
    icon: Radio,
  },
  {
    name: 'investigator',
    role: 'RCA',
    desc: 'Read-only causal-chain tracer across metrics, logs, traces, git, hosts, and topology. Returns evidence with confidence.',
    icon: Eye,
  },
  {
    name: 'critic',
    role: 'Audit',
    desc: 'Peer auditor that checks the evidence chain on critical severities. Emits a needs_correction flag without inventing issues.',
    icon: ShieldCheck,
  },
  {
    name: 'reviewer',
    role: 'Pre-flight',
    desc: 'Second pair of eyes on every mutating action. Approves only the minimum-necessary blast radius.',
    icon: CheckCircle2,
  },
  {
    name: 'repairer',
    role: 'Repair',
    desc: 'Narrow-scope mutator. Actions must match one approved incident, manifest, resource, command, and payload hash.',
    icon: TerminalSquare,
  },
  {
    name: 'verifier',
    role: 'Verify',
    desc: 'Calls recovery.verify only. 4-metric allowlist, 3 warning tiers, returns a VerifiedDelta for the manager.',
    icon: Gauge,
  },
  {
    name: 'reporter',
    role: 'Postmortem',
    desc: 'Writes structured period reports from pre-computed ReportFacts. Resource trends, monitoring coverage, changes — never fabricated.',
    icon: GitBranch,
  },
];

const pillars = [
  {
    title: 'Agent collaboration framework',
    desc: 'Manager-style dispatch with explicit safety levels (L0–L3), 7 worker roles, and a 7-phase closed loop backed by append-only ledger tables.',
    icon: Cpu,
  },
  {
    title: 'Skill ecosystem',
    desc: 'Nacos-backed Skill Registry with hot reload, HTTP 2.x Config API, local fallback, and a stdio MCP server for the OpsKeeper Worker plugin.',
    icon: Database,
  },
  {
    title: 'Observability & audit',
    desc: 'OpenTelemetry trace context, Prometheus metrics, Loki logs, Tempo traces, Grafana dashboards — plus an HMAC-chained audit ledger.',
    icon: Activity,
  },
];

const safetyItems = [
  'Diagnosis tools are read-only by default — mutating actions require a pending proposal.',
  'Explicit human-in-the-loop approval before any recovery command is dispatched.',
  'Exact target matching on resource, command, and payload hash — unknown tools and cross-resource targets fail closed.',
  'Append-only HMAC-chained audit ledger; every action is replayable.',
  'Independent verifier separates the actor from the judge on every recovery.',
];

const codeSnippet = `# Install the OpsKeeper CLI and start the local stack
curl -fsSL https://opskeeper.dev/install.sh | bash

# Bring up Postgres + Qdrant + the OpsKeeper control plane
docker compose up -d opskeeper postgres qdrant

# Open the closed-loop demo (4 reproducible scenarios included)
opskeeper demo replay pg-connection-pool-exhaustion

# Inspect any incident end-to-end
opskeeper incident show INC-PG-POOL-001 \\
  --include timeline,evidence,proposals,audit`;

const installSnippet = `# Worker plugin — drop into AgentTeams Dashboard
opskeeper plugin install agentteams-plugin-installer

# Or run the MCP proxy directly for any worker
opskeeper-teamharness serve \\
  --mcp-transport stdio \\
  --opskeeper-endpoint http://localhost:8090`;

export default function HomePage() {
  return (
    <>
      {/* Hero */}
      <Section className="pt-20 pb-24 md:pt-28 md:pb-32">
        <div className="grid gap-12 lg:grid-cols-12 lg:gap-16 items-center">
          <div className="lg:col-span-7">
            <div className="mb-5 inline-flex items-center gap-2 rounded-full border border-white/10 bg-white/5 px-3 py-1 text-xs font-medium text-ink-200">
              <span className="inline-block h-1.5 w-1.5 rounded-full bg-accent-400 animate-pulse" />
              v1.0.0 · Apache-2.0 · open source
            </div>
            <h1 className="text-balance text-4xl font-semibold tracking-tight text-white sm:text-5xl md:text-6xl">
              Auditable operations for{' '}
              <span className="bg-gradient-to-br from-white to-accent-300 bg-clip-text text-transparent">
                multi-agent incident response
              </span>
            </h1>
            <p className="mt-6 max-w-2xl text-pretty text-lg leading-relaxed text-ink-300">
              OpsKeeper closes the loop between <strong className="text-white">alert</strong>,{' '}
              <strong className="text-white">evidence</strong>,{' '}
              <strong className="text-white">root-cause analysis</strong>,{' '}
              <strong className="text-white">human approval</strong>,{' '}
              <strong className="text-white">narrowly authorized recovery</strong>,{' '}
              <strong className="text-white">independent verification</strong>, and{' '}
              <strong className="text-white">post-incident learning</strong>.
              Mutating actions never run without a proposal, a human approver, and an
              audit record.
            </p>
            <div className="mt-8 flex flex-wrap items-center gap-3">
              <Button href="/docs/getting-started">Get started</Button>
              <Button href="/platform" variant="secondary">
                How the closed loop works
              </Button>
              <Button href="https://github.com/louloulin/opskeeper" variant="ghost" external>
                Star on GitHub
              </Button>
            </div>
            <div className="mt-8 flex flex-wrap items-center gap-x-6 gap-y-2 text-xs text-ink-400">
              <span className="inline-flex items-center gap-1.5"><ShieldCheck className="h-3.5 w-3.5 text-accent-400" /> Safety by default</span>
              <span className="inline-flex items-center gap-1.5"><Lock className="h-3.5 w-3.5 text-accent-400" /> HMAC-chained audit</span>
              <span className="inline-flex items-center gap-1.5"><Database className="h-3.5 w-3.5 text-accent-400" /> Postgres + Qdrant</span>
              <span className="inline-flex items-center gap-1.5"><Cpu className="h-3.5 w-3.5 text-accent-400" /> 7 worker roles</span>
            </div>
          </div>
          <div className="lg:col-span-5">
            <CodeBlock language="bash" title="install · 30s">
              {installSnippet}
            </CodeBlock>
          </div>
        </div>
      </Section>

      {/* Trust strip */}
      <Section className="py-10">
        <div className="grid grid-cols-2 gap-4 sm:grid-cols-4">
          {[
            { k: '7', l: 'Operational worker roles' },
            { k: '8', l: 'Closed-loop phases' },
            { k: '4', l: 'Reproducible incident scenarios' },
            { k: '100%', l: 'Audit replay coverage' },
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

      {/* The closed loop */}
      <Section id="closed-loop" className="py-20 md:py-28">
        <SectionHeader
          eyebrow="The closed loop"
          title="Eight phases. Every transition is durable."
          description="OpsKeeper runs incidents through an explicit state machine. Each phase is an append-only ledger event, each transition has a defined guard, and the loop is replayable from scratch."
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

      {/* Workers */}
      <Section className="py-20 md:py-28">
        <SectionHeader
          eyebrow="Worker roles"
          title="Specialized agents. One manager."
          description="Seven operational roles coordinate through a manager-style dispatcher. Each role has a narrow tool allowlist and a clear contract — the loop never depends on a single agent being clever."
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
            <div className="text-sm font-medium text-white">Plus specialist skills</div>
            <p className="mt-2 text-sm text-ink-300">
              <code className="font-mono text-xs text-accent-300">specialist-sre</code>,{' '}
              <code className="font-mono text-xs text-accent-300">specialist-network</code>,{' '}
              <code className="font-mono text-xs text-accent-300">specialist-compute</code>,{' '}
              <code className="font-mono text-xs text-accent-300">specialist-disk</code>, and{' '}
              <code className="font-mono text-xs text-accent-300">specialist-ops</code> ship in the
              <code className="font-mono text-xs text-accent-300"> agents/</code> directory and
              attach to incidents based on routing rules.
            </p>
            <Link
              href="/workers"
              className="mt-4 inline-flex items-center gap-1.5 text-sm text-accent-300 hover:text-accent-200"
            >
              Read the worker contracts <ArrowRight className="h-3.5 w-3.5" />
            </Link>
          </div>
        </div>
      </Section>

      {/* Safety boundary */}
      <Section className="py-20 md:py-28">
        <div className="grid gap-12 lg:grid-cols-12 lg:items-center">
          <div className="lg:col-span-5">
            <SectionHeader
              eyebrow="Safety boundary"
              title="Diagnosis reads. Recovery writes. Only with a human."
              description="OpsKeeper separates read from write at the orchestrator. Read-only tools are always available; mutating tools require a proposal, an explicit human approver, and exact resource / command / payload hash match."
            />
            <div className="mt-8">
              <Button href="/security" variant="secondary">
                Read the security model
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
          eyebrow="Three pillars"
          title="What ships in the box."
          description="OpsKeeper is one platform, three tightly integrated subsystems. Each one is independently useful and observable."
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
              eyebrow="Demo in 30 seconds"
              title="Replay a real incident end-to-end."
              description="Four reproducible PostgreSQL scenarios ship with the repo. Pick one, replay it, and watch the closed loop run from detection to postmortem in the web console."
            />
            <ul className="mt-6 space-y-2 text-sm text-ink-300">
              <li className="flex items-center gap-2"><CheckCircle2 className="h-4 w-4 text-accent-400" /> pg-connection-pool-exhaustion</li>
              <li className="flex items-center gap-2"><CheckCircle2 className="h-4 w-4 text-accent-400" /> pg-disk-io-saturation</li>
              <li className="flex items-center gap-2"><CheckCircle2 className="h-4 w-4 text-accent-400" /> pg-lock-wait-long-transaction</li>
              <li className="flex items-center gap-2"><CheckCircle2 className="h-4 w-4 text-accent-400" /> pg-replica-replay-lag</li>
            </ul>
            <div className="mt-8">
              <Button href="/docs/getting-started" variant="secondary">
                Run the demos
              </Button>
            </div>
          </div>
          <div className="lg:col-span-7">
            <CodeBlock language="bash" title="demo · pg-connection-pool-exhaustion">
              {codeSnippet}
            </CodeBlock>
          </div>
        </div>
      </Section>

      {/* Integrations */}
      <Section className="py-20 md:py-28">
        <SectionHeader
          eyebrow="Integrations"
          title="Plays well with the rest of your stack."
          description="OpsKeeper ships first-party plugins for the AgentTeams Dashboard and the standard observability backend. The MCP server is stdio and Streamable HTTP, so any worker can join."
        />
        <div className="mt-12 grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
          {[
            { t: 'AgentTeams Dashboard', d: 'Plugin installer — sidebar, route, dashboard widget, detail panel, toolbar.' },
            { t: 'OpsKeeper TeamHarness', d: 'Worker/Manager plugin + stdio MCP proxy (14 tools, Bearer + HMAC + W3C traceparent).' },
            { t: 'Prometheus + Loki + Tempo', d: 'Native scrape config, log/metric/trace correlation by trace_id.' },
            { t: 'Grafana dashboards', d: 'Provisioned dashboards for the closed loop, audit ledger, and skill health.' },
            { t: 'PostgreSQL', d: 'Incident memory, append-only ledger, MySQL GET_LOCK advisory locks.' },
            { t: 'Qdrant', d: 'Vector retrieval, keyword recall, RRF ranking, retained candidate-decision evidence.' },
            { t: 'Nacos Config', d: 'Skill registry with 30s polling hot-reload and local fallback.' },
            { t: 'OpenTelemetry', d: 'W3C traceparent propagation end-to-end across worker → MCP → control plane.' },
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
              Open source · Apache-2.0
            </div>
            <h2 className="mt-3 max-w-2xl text-balance text-3xl font-semibold tracking-tight text-white sm:text-4xl">
              Bring the audit trail to your incident response.
            </h2>
            <p className="mt-3 max-w-xl text-pretty text-base text-ink-200">
              Run the closed loop locally in under five minutes. Then point it at your real
              incident stream.
            </p>
            <div className="mt-6 flex flex-wrap gap-3">
              <Button href="/docs/getting-started">Get started</Button>
              <Button href="https://github.com/louloulin/opskeeper" variant="secondary" external>
                View on GitHub
              </Button>
            </div>
          </div>
        </div>
      </Section>
    </>
  );
}
