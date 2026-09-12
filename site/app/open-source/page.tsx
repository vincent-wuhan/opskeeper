import Link from 'next/link';
import { Section, SectionHeader } from '@/components/section';
import { Button } from '@/components/button';
import {
  Github,
  GitFork,
  GitPullRequest,
  Star,
  Users,
  Scale,
  CheckCircle2,
  Heart,
  Code2,
  Sparkles,
} from 'lucide-react';

export const metadata = {
  title: 'Open source',
  description:
    'OpsKeeper is open source under Apache-2.0. Source, plugins, and reproducible incident fixtures all live in the public repo.',
};

const stats = [
  { icon: Github, k: 'Apache-2.0', l: 'License' },
  { icon: Code2, k: '7 + 5', l: 'Worker roles & specialist skills' },
  { icon: GitPullRequest, k: 'PRs welcome', l: 'Bug fixes · skills · workflows · docs' },
  { icon: Scale, k: 'Brand-governed', l: 'Three-layer naming policy' },
];

const principles = [
  'The product layer (OpsKeeper) is what users see. The compliance layer (AgentTeams, OnGrid attribution) stays in NOTICE.md / TRADEMARK.md. The evolution layer (compatibility identifiers) is documented without becoming a brand claim.',
  'Reproducible incident fixtures ship in the repo — no customer data, no production telemetry, no credentials.',
  'The audit ledger, the safety boundary, and the closed-loop orchestrator are all in this repo and tested in CI.',
  'Plugins (agentteams-plugin-installer, opskeeper-teamharness) are first-party open source, not vendored binaries.',
];

const contributingTracks = [
  {
    icon: Code2,
    title: 'Code',
    desc: 'Bug fixes, new skills, new worker roles, new MCP tools. The harness verifies your change did not regress the closed loop.',
    cta: { label: 'CONTRIBUTING.md', href: 'https://github.com/vincent-wuhan/opskeeper/blob/main/CONTRIBUTING.md' },
  },
  {
    icon: Sparkles,
    title: 'Workflows',
    desc: 'Add a new reproducible scenario under deploy/incident-events/ and a matching workflow under workflows/.',
    cta: { label: 'Workflow catalog', href: '/docs/workflow-catalog' },
  },
  {
    icon: Heart,
    title: 'Docs & site',
    desc: 'Improve site/app/docs/**, fix typos, add examples. Docs are React components in the marketing site repo.',
    cta: { label: 'Browse docs', href: '/docs' },
  },
];

export default function OpenSourcePage() {
  return (
    <>
      <Section className="pt-20 pb-12">
        <div className="max-w-3xl">
          <div className="mb-3 inline-flex items-center gap-2 rounded-full border border-white/10 bg-white/5 px-3 py-1 text-xs font-medium text-accent-300">
            Open source
          </div>
          <h1 className="text-balance text-4xl font-semibold tracking-tight text-white sm:text-5xl">
            Apache-2.0, brand-governed, contribution-friendly.
          </h1>
          <p className="mt-5 text-lg text-ink-300">
            OpsKeeper is open source because auditable incident response should not be a
            proprietary lock-in. The code, the plugins, the docs, and the reproducible
            incident fixtures all live in the public repo.
          </p>
          <div className="mt-6 flex flex-wrap gap-3">
            <Button href="https://github.com/vincent-wuhan/opskeeper" external>
              View on GitHub
            </Button>
            <Button href="/brand" variant="secondary">
              Brand guidelines
            </Button>
          </div>
        </div>
      </Section>

      <Section className="py-10">
        <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
          {stats.map((s) => (
            <div
              key={s.l}
              className="rounded-xl border border-white/10 bg-white/[0.02] p-5"
            >
              <s.icon className="h-4 w-4 text-accent-400" />
              <div className="mt-3 text-xl font-semibold text-white">{s.k}</div>
              <div className="mt-1 text-sm text-ink-300">{s.l}</div>
            </div>
          ))}
        </div>
      </Section>

      <Section className="py-10 md:py-16">
        <SectionHeader
          eyebrow="Principles"
          title="What the open-source project commits to."
          description="These are the things OpsKeeper will keep doing, and the things it will not do. PRs that violate them will be declined politely."
        />
        <div className="mt-10 grid gap-3 md:grid-cols-2">
          {principles.map((p, i) => (
            <div
              key={i}
              className="flex items-start gap-3 rounded-xl border border-white/10 bg-white/[0.02] p-5"
            >
              <CheckCircle2 className="mt-0.5 h-4 w-4 flex-none text-accent-400" />
              <p className="text-sm text-ink-200">{p}</p>
            </div>
          ))}
        </div>
      </Section>

      <Section className="py-10 md:py-16">
        <SectionHeader
          eyebrow="Contributing"
          title="Three tracks."
          description="Pick the one that matches the change you want to make. The harness runs in CI on every PR."
        />
        <div className="mt-12 grid gap-4 md:grid-cols-3">
          {contributingTracks.map((t) => (
            <div
              key={t.title}
              className="rounded-2xl border border-white/10 bg-white/[0.02] p-6"
            >
              <span className="inline-flex h-10 w-10 items-center justify-center rounded-md border border-white/10 bg-white/5 text-accent-300">
                <t.icon className="h-5 w-5" />
              </span>
              <h3 className="mt-4 text-lg font-semibold text-white">{t.title}</h3>
              <p className="mt-2 text-sm text-ink-300">{t.desc}</p>
              <Link
                href={t.cta.href}
                className="mt-4 inline-flex items-center gap-1.5 text-sm text-accent-300 hover:text-accent-200"
              >
                {t.cta.label} →
              </Link>
            </div>
          ))}
        </div>
      </Section>

      <Section className="py-10 md:py-16">
        <div className="rounded-3xl border border-white/10 bg-gradient-to-br from-accent-500/15 via-ink-900 to-ink-950 p-10 md:p-14">
          <div className="grid gap-6 md:grid-cols-12 md:items-center">
            <div className="md:col-span-8">
              <div className="flex items-center gap-2 text-xs uppercase tracking-wider text-accent-200">
                <Users className="h-4 w-4" />
                Maintainers & contributors
              </div>
              <h2 className="mt-3 text-balance text-3xl font-semibold tracking-tight text-white sm:text-4xl">
                A small core team, a long tail of contributors.
              </h2>
              <p className="mt-3 max-w-xl text-ink-200">
                OpsKeeper is maintained by a core team and a long tail of contributors from
                SRE / DevOps backgrounds. CODEOWNERS, a public roadmap, and a quarterly
                release cadence keep the project honest.
              </p>
            </div>
            <div className="md:col-span-4">
              <div className="grid gap-3">
                <div className="rounded-xl border border-white/10 bg-white/[0.04] p-4">
                  <div className="flex items-center gap-2 text-sm font-medium text-white">
                    <Star className="h-4 w-4 text-accent-300" /> Roadmap
                  </div>
                  <p className="mt-1 text-sm text-ink-300">
                    Public, dated, and honest about slippage.
                  </p>
                  <Link
                    href="/roadmap"
                    className="mt-2 inline-flex text-sm text-accent-300 hover:text-accent-200"
                  >
                    See the roadmap →
                  </Link>
                </div>
                <div className="rounded-xl border border-white/10 bg-white/[0.04] p-4">
                  <div className="flex items-center gap-2 text-sm font-medium text-white">
                    <GitFork className="h-4 w-4 text-accent-300" /> Governance
                  </div>
                  <p className="mt-1 text-sm text-ink-300">
                    Brand, provenance, and release-gate docs.
                  </p>
                  <Link
                    href="/brand"
                    className="mt-2 inline-flex text-sm text-accent-300 hover:text-accent-200"
                  >
                    See the brand docs →
                  </Link>
                </div>
              </div>
            </div>
          </div>
        </div>
      </Section>
    </>
  );
}
