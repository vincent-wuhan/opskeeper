import { Section } from '@/components/section';

export const metadata = {
  title: 'FAQ',
  description:
    'Frequently asked questions about OpsKeeper: closed loop, safety boundary, license, deployment, integrations.',
};

const groups: { title: string; items: { q: string; a: string }[] }[] = [
  {
    title: 'Closed loop & agents',
    items: [
      {
        q: 'What is the closed loop?',
        a: 'Eight explicit phases — detected → correlated → investigated → critiqued → approved → recovered → verified → postmortem. Each transition is a guarded event in an append-only ledger. The loop refuses to advance without a satisfied guard.',
      },
      {
        q: 'Do the agents run mutating actions on their own?',
        a: 'No. Diagnosis is always read-only. Mutating actions require a pending proposal with explicit blast radius, a human approver signature, and exact-match guards on resource, command, and payload hash. Unknown tools and cross-resource targets fail closed.',
      },
      {
        q: 'How many worker roles are there?',
        a: 'Seven operational roles — alerter, investigator, critic, reviewer, repairer, verifier, reporter — plus five specialist skills (specialist-sre, specialist-network, specialist-compute, specialist-disk, specialist-ops).',
      },
    ],
  },
  {
    title: 'Safety & security',
    items: [
      {
        q: 'What happens if a worker is compromised?',
        a: 'The safety boundary is enforced at the manager, not the prompt. A compromised worker still cannot bypass tool allowlists, blast-radius guards, or human approval. The HMAC-chained audit ledger preserves a verifiable record of every dispatch.',
      },
      {
        q: 'What is the audit ledger and how is it preserved?',
        a: 'Every dispatch and completion appends to loop_event_log. Each event is HMAC-chained (hash_n = HMAC(hash_prev, event_n)). The chain is daily-exported to ndjson and can be replayed end-to-end with `opskeeper audit replay`.',
      },
      {
        q: 'Where does the LLM run? Does OpsKeeper send my data to OpenAI?',
        a: 'LLM calls happen in the worker that needs them. OpsKeeper does not proxy or log LLM traffic. You choose your LLM backend (vLLM, OpenAI, Aliyun DashScope, Anthropic) — multi-LLM backend ships in Q1 2027.',
      },
    ],
  },
  {
    title: 'Deployment & operations',
    items: [
      {
        q: 'What does it take to run OpsKeeper locally?',
        a: 'Docker 24+, Go 1.25+, Node 20+, pnpm 9+, Python 3.11+. Run `docker compose up -d opskeeper postgres qdrant` and you have the full stack. The first replay takes under five minutes.',
      },
      {
        q: 'Can it run on Kubernetes?',
        a: 'Yes — a Helm chart ships under deploy/k8s/charts/opskeeper today. A Kubernetes Operator (CRD for OpsKeeper + plugins) is on the Q1 2027 roadmap.',
      },
      {
        q: 'How do I upgrade?',
        a: 'Drain active incidents to verified or postmortem, apply the new image, roll control plane instances one at a time, then replay the most recent ledger events to confirm no event was lost. The full runbook lives at docs/deployment/upgrade.md.',
      },
    ],
  },
  {
    title: 'Open source',
    items: [
      {
        q: 'Is OpsKeeper open source?',
        a: 'Yes — Apache-2.0. Source, plugin code, and reproducible incident fixtures all live in the public repo at github.com/louloulin/opskeeper.',
      },
      {
        q: 'Can I run a forked version under my own brand?',
        a: 'You can fork the source code under Apache-2.0. The OpsKeeper name and wordmark are not part of that license — see TRADEMARK.md. For commercial co-branding, open a trademark-tagged issue.',
      },
      {
        q: 'How can I contribute?',
        a: 'PRs for bug fixes, new skills, new workflow scenarios, and docs are welcome. See CONTRIBUTING.md for the workflow. The site itself is a Next.js app under site/ — docs contributions can be PRs against site/app/docs/.',
      },
    ],
  },
];

export default function FaqPage() {
  return (
    <Section className="py-20">
      <div className="mx-auto max-w-3xl">
        <div className="mb-3 inline-flex items-center gap-2 rounded-full border border-white/10 bg-white/5 px-3 py-1 text-xs font-medium text-accent-300">
          FAQ
        </div>
        <h1 className="text-balance text-4xl font-semibold tracking-tight text-white sm:text-5xl">
          Frequently asked questions.
        </h1>
        <p className="mt-5 text-lg text-ink-300">
          The short answers to the questions we hear most often. For deeper detail, the docs and
          the security model page cover everything.
        </p>

        <div className="mt-12 space-y-10">
          {groups.map((g) => (
            <div key={g.title}>
              <h2 className="text-xl font-semibold text-white">{g.title}</h2>
              <div className="mt-4 divide-y divide-white/10 rounded-2xl border border-white/10">
                {g.items.map((it) => (
                  <details key={it.q} className="group bg-white/[0.01] p-5 open:bg-white/[0.03]">
                    <summary className="flex cursor-pointer list-none items-center justify-between gap-4 text-base font-medium text-white">
                      <span>{it.q}</span>
                      <span className="text-ink-400 transition-transform group-open:rotate-45">
                        +
                      </span>
                    </summary>
                    <p className="mt-3 text-sm leading-relaxed text-ink-300">{it.a}</p>
                  </details>
                ))}
              </div>
            </div>
          ))}
        </div>
      </div>
    </Section>
  );
}
