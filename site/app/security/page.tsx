import { Section, SectionHeader } from '@/components/section';
import { CheckCircle2, Lock, ShieldCheck, FileLock, KeyRound, Eye, GitBranch } from 'lucide-react';

export const metadata = {
  title: 'Security',
  description:
    'OpsKeeper security model: read-by-default, write-by-proposal, HMAC-chained audit ledger, and an independent verifier on every recovery.',
};

const principles = [
  {
    title: 'Read by default',
    desc: 'Diagnosis tools — metrics, logs, traces, git, topology, RCA reports — are always available to every worker. There is no implicit gate.',
    icon: Eye,
  },
  {
    title: 'Write by proposal',
    desc: 'Mutating tools require a pending proposal with an explicit blast radius. The control plane refuses to dispatch a recovery without one.',
    icon: FileLock,
  },
  {
    title: 'Approve by a human',
    desc: 'A human approver is required before any recovery command runs. The approval is bound to a specific proposal and resource.',
    icon: KeyRound,
  },
  {
    title: 'Exact-match guards',
    desc: 'Resource, command, and payload hash must match the approved proposal exactly. Unknown tools and cross-resource targets fail closed.',
    icon: Lock,
  },
  {
    title: 'Independent verifier',
    desc: 'The actor and the judge are different workers. recovery.verify uses a four-metric allowlist and three warning tiers — never the same call path as recovery.dispatch.',
    icon: ShieldCheck,
  },
  {
    title: 'HMAC-chained audit',
    desc: 'Every dispatched action appends to the ledger; on completion the event is sealed. The chain is daily-exported as ndjson and is replayable.',
    icon: GitBranch,
  },
];

const threatModel = [
  {
    label: 'Tool injection',
    desc: 'A worker is tricked into requesting a tool outside its allowlist. Mitigated by the explicit tool_allowlist per skill_meta.yaml.',
  },
  {
    label: 'Role escape',
    desc: 'A worker tries to take on a phase it is not contracted for. Mitigated by phase → role binding in the manager.',
  },
  {
    label: 'Blast-radius bypass',
    desc: 'A proposal is approved for one resource and executed against another. Mitigated by exact match on resource, command, and payload hash.',
  },
  {
    label: 'Replan loop',
    desc: 'An agent loops on planning without ever executing. Mitigated by max_turns budgets and the verified-only rule on recovery.verify.',
  },
];

export default function SecurityPage() {
  return (
    <>
      <Section className="pt-20 pb-12">
        <div className="max-w-3xl">
          <div className="mb-3 inline-flex items-center gap-2 rounded-full border border-white/10 bg-white/5 px-3 py-1 text-xs font-medium text-accent-300">
            Security
          </div>
          <h1 className="text-balance text-4xl font-semibold tracking-tight text-white sm:text-5xl">
            Safety is a property of the loop, not of the LLM.
          </h1>
          <p className="mt-5 text-lg text-ink-300">
            OpsKeeper does not trust any single agent. The closed loop enforces read-by-default,
            write-by-proposal, approve-by-a-human, and verify-by-an-independent-worker — at the
            orchestrator, not at the prompt.
          </p>
        </div>
      </Section>

      <Section className="py-10">
        <SectionHeader
          eyebrow="Six principles"
          title="The safety model in six lines."
          description="The principles below are enforced at the manager level. Every skill and worker in OpsKeeper is bound by them."
        />
        <div className="mt-12 grid gap-4 md:grid-cols-2 lg:grid-cols-3">
          {principles.map((p) => (
            <div
              key={p.title}
              className="rounded-2xl border border-white/10 bg-white/[0.02] p-6"
            >
              <span className="inline-flex h-10 w-10 items-center justify-center rounded-md border border-white/10 bg-white/5 text-accent-300">
                <p.icon className="h-5 w-5" />
              </span>
              <h3 className="mt-4 text-lg font-semibold text-white">{p.title}</h3>
              <p className="mt-2 text-sm text-ink-300">{p.desc}</p>
            </div>
          ))}
        </div>
      </Section>

      <Section className="py-10 md:py-16">
        <SectionHeader
          eyebrow="Threat model"
          title="What we defend against."
          description="These four classes of attack are explicitly covered. Red-team playbooks extend the set; the mitigations live in the manager, not in the prompt."
        />
        <div className="mt-10 overflow-hidden rounded-2xl border border-white/10">
          <table className="w-full text-left text-sm">
            <thead className="bg-white/[0.03] text-ink-200">
              <tr>
                <th className="px-4 py-3 font-medium">Threat</th>
                <th className="px-4 py-3 font-medium">Mitigation</th>
              </tr>
            </thead>
            <tbody className="divide-y divide-white/5">
              {threatModel.map((t) => (
                <tr key={t.label} className="bg-white/[0.01]">
                  <td className="px-4 py-3 font-mono text-xs text-accent-300">{t.label}</td>
                  <td className="px-4 py-3 text-ink-200">{t.desc}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </Section>

      <Section className="py-10 md:py-16">
        <div className="grid gap-6 md:grid-cols-3">
          <div className="rounded-2xl border border-white/10 bg-white/[0.02] p-6">
            <div className="flex items-center gap-2 text-sm font-medium text-white">
              <CheckCircle2 className="h-4 w-4 text-accent-400" /> Reporting a vulnerability
            </div>
            <p className="mt-3 text-sm text-ink-300">
              Please email <span className="text-white">security@opskeeper.dev</span> or open a
              private security advisory on GitHub. See <code className="text-accent-300">SECURITY.md</code> for
              disclosure timelines.
            </p>
          </div>
          <div className="rounded-2xl border border-white/10 bg-white/[0.02] p-6">
            <div className="flex items-center gap-2 text-sm font-medium text-white">
              <CheckCircle2 className="h-4 w-4 text-accent-400" /> Supported versions
            </div>
            <p className="mt-3 text-sm text-ink-300">
              OpsKeeper supports the latest release and the previous minor. Older releases
              receive critical fixes only.
            </p>
          </div>
          <div className="rounded-2xl border border-white/10 bg-white/[0.02] p-6">
            <div className="flex items-center gap-2 text-sm font-medium text-white">
              <CheckCircle2 className="h-4 w-4 text-accent-400" /> Audit replay
            </div>
            <p className="mt-3 text-sm text-ink-300">
              Every dispatch and completion is replayable from the HMAC-chained ledger.
              See the <code className="text-accent-300">opskeeper audit replay</code> command.
            </p>
          </div>
        </div>
      </Section>
    </>
  );
}
