import { Section, SectionHeader } from '@/components/section';
import { CheckCircle2, CircleDashed, Sparkles } from 'lucide-react';

export const metadata = {
  title: 'Roadmap',
  description:
    'The OpsKeeper public roadmap: shipping now, next in Q3 2026, Q4 2026, and the Q1 2027 vision.',
};

const shipping = [
  '7 worker skills + manager decision table with L0–L3 safety levels',
  'Append-only loop_event_log + SHA256-chained proposal audit',
  'Real-environment verification harness (alert_storm, rca_loop, recovery_verify)',
  'Plugin stdio MCP server with W3C traceparent passthrough',
  'Four reproducible PostgreSQL incident scenarios',
];

const next = [
  'AgentLoop / LoongSuite OTLP adapter (env-var switch ingest endpoint)',
  'Red-team security playbooks: tool injection, role escape, blast-radius bypass, replan loop',
  'External anchoring of the audit chain root (transparency log) for regulatory evidence',
  'Product demo video (≤ 3 min, end-to-end playbook)',
];

const q4 = [
  'Aliyun Skills integration (sre-aliyun-mcp / ack-mcp / arms-mcp)',
  'Cross-cloud migration templates v1 (financial core trading + SaaS multi-tenant)',
  'LLM-as-evaluator Skill assessment (8-case gold set)',
  'Manager dispatch dual-track: ruleset + LLM arbitration',
];

const q1 = [
  'Multi-LLM backend (vLLM / OpenAI / Aliyun DashScope / Anthropic)',
  'Kubernetes Operator (CRD for OpsKeeper + plugins)',
  'Skill marketplace (Nacos Config + Web UI)',
];

export default function RoadmapPage() {
  return (
    <>
      <Section className="pt-20 pb-12">
        <div className="max-w-3xl">
          <div className="mb-3 inline-flex items-center gap-2 rounded-full border border-white/10 bg-white/5 px-3 py-1 text-xs font-medium text-accent-300">
            Roadmap
          </div>
          <h1 className="text-balance text-4xl font-semibold tracking-tight text-white sm:text-5xl">
            What we&apos;re building next.
          </h1>
          <p className="mt-5 text-lg text-ink-300">
            A living list of what has shipped, what is in flight, and where OpsKeeper is going
            over the next two quarters. Items are pulled from the public roadmap in the repo.
          </p>
        </div>
      </Section>

      <Section className="py-10">
        <div className="grid gap-6 md:grid-cols-2">
          <Column title="Shipping now" tone="shipped" items={shipping} icon={CheckCircle2} />
          <Column title="Next · Q3 2026" tone="next" items={next} icon={Sparkles} />
          <Column title="Q4 2026" tone="later" items={q4} icon={CircleDashed} />
          <Column title="Vision · Q1 2027" tone="later" items={q1} icon={CircleDashed} />
        </div>
      </Section>

      <Section className="py-10 md:py-16">
        <SectionHeader
          eyebrow="How we plan"
          title="Public, dated, and honest."
          description="The roadmap in the repo is the source of truth. Items move left to right; if a quarter slips, we say so in the changelog."
        />
        <div className="mt-8 text-sm text-ink-300">
          See <code className="text-accent-300">ROADMAP_PUBLIC.md</code> in the repo, or the full{' '}
          <code className="text-accent-300">ROADMAP.md</code> for the long view including
          research bets.
        </div>
      </Section>
    </>
  );
}

function Column({
  title,
  items,
  tone,
  icon: Icon,
}: {
  title: string;
  items: string[];
  tone: 'shipped' | 'next' | 'later';
  icon: React.ComponentType<{ className?: string }>;
}) {
  const ring =
    tone === 'shipped'
      ? 'border-accent-500/30 bg-accent-500/5'
      : tone === 'next'
      ? 'border-white/15 bg-white/[0.04]'
      : 'border-white/10 bg-white/[0.02]';
  const tag =
    tone === 'shipped'
      ? 'text-accent-300'
      : tone === 'next'
      ? 'text-white'
      : 'text-ink-300';
  return (
    <div className={`rounded-2xl border p-6 ${ring}`}>
      <div className={`flex items-center gap-2 text-sm font-medium ${tag}`}>
        <Icon className="h-4 w-4" />
        {title}
      </div>
      <ul className="mt-4 space-y-3 text-sm text-ink-200">
        {items.map((it) => (
          <li key={it} className="flex items-start gap-2">
            <CheckCircle2
              className={`mt-0.5 h-4 w-4 flex-none ${tone === 'shipped' ? 'text-accent-400' : 'text-ink-400'}`}
            />
            <span>{it}</span>
          </li>
        ))}
      </ul>
    </div>
  );
}
