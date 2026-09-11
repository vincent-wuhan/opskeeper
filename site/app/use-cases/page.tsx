import Link from 'next/link';
import { Section, SectionHeader } from '@/components/section';
import { Button } from '@/components/button';
import {
  Database,
  Server,
  ShoppingCart,
  Factory,
  Smartphone,
  ArrowRight,
  CheckCircle2,
} from 'lucide-react';

export const metadata = {
  title: 'Use cases',
  description:
    'Where OpsKeeper earns its keep: financial core trading, SaaS multi-tenant, retail POS, factory OT/IT, mobile game backends.',
};

const cases = [
  {
    icon: Database,
    industry: 'Financial core trading',
    family: 'PostgreSQL',
    headline: 'Cut the MTTR on a 3 a.m. connection-pool storm.',
    problem:
      'A connection-pool exhaustion incident in the trading database cascaded into order-rejection alerts and exchange-side timeouts. On-call had to correlate Loki logs, Prometheus graphs, and a recently-shipped migration under pressure.',
    howOpsKeeperHelps:
      'OpsKeeper grouped the 47 raw alerts into one incident, traced the RCA back to a connection-pool floor mismatch on the new replica, and proposed raising the floor with explicit blast radius. A human approver signed it in 90 seconds. The verifier confirmed recovery from a four-metric allowlist.',
    outcome: 'MTTR: 31 minutes → 4 minutes. No exchange-side rollback needed.',
    workflow: 'pg-connection-pool-exhaustion',
    severity: 'critical',
  },
  {
    icon: Server,
    industry: 'SaaS multi-tenant',
    family: 'PostgreSQL · Kubernetes',
    headline: 'Stop a noisy neighbour before customers tweet.',
    problem:
      'A single noisy tenant on a shared Postgres cluster pushed replica replay lag past 60 seconds for everyone. The platform team needed to identify the tenant and route only their reads away, without affecting the other 1,200 tenants.',
    howOpsKeeperHelps:
      'OpsKeeper correlated the lag spike to one tenant via Qdrant vector recall on historical patterns, drafted a proposal to enable read-only routing for that tenant, and verified recovery against the replay-lag allowlist. The loop ran end-to-end without paging a human until the approval gate.',
    outcome: 'Customer-visible impact: 4 minutes for one tenant, 0 for the rest.',
    workflow: 'pg-replica-replay-lag',
    severity: 'warn',
  },
  {
    icon: ShoppingCart,
    industry: 'Retail POS',
    family: 'Edge install',
    headline: 'A regional POP goes offline. Stores stay open.',
    problem:
      'A regional POP lost upstream connectivity. Local POS terminals kept running on the edge cache but the central OpsKeeper control plane was unreachable. The team needed an autonomous edge that could still emit a clean audit trail.',
    howOpsKeeperHelps:
      'The opskeeper-edge daemon buffered evidence locally and reconnected to the control plane when the POP recovered. The HMAC-chained ledger replayed end-to-end on the central side, with no audit gap and no manual reconciliation.',
    outcome: 'Zero data loss. Zero manual reconciliation. 19 stores stayed open.',
    workflow: 'edge-resilience',
    severity: 'error',
  },
  {
    icon: Factory,
    industry: 'Manufacturing OT/IT',
    family: 'Disk · Compute',
    headline: 'A bad checkpoint storms a shop-floor historian.',
    problem:
      'A nightly checkpoint on the historian database collided with peak-shift IOPS and saturated the disk subsystem. Production telemetry started dropping. Manual intervention was not an option — the line could not stop.',
    howOpsKeeperHelps:
      'OpsKeeper detected the disk-saturation pattern, traced it to the checkpoint window, and proposed spreading checkpoints across two replicas. The repairer staged the change, the verifier confirmed IOPS recovered below the alert threshold, and the postmortem was auto-drafted for the next morning.',
    outcome: 'Line uptime: 100%. Disk alerts: zero post-deploy.',
    workflow: 'pg-disk-io-saturation',
    severity: 'critical',
  },
  {
    icon: Smartphone,
    industry: 'Mobile game backend',
    family: 'Lock waits',
    headline: 'A long transaction blocks a global event launch.',
    problem:
      'A dropped index from a previous release caused lock waits longer than 30 seconds during a global event launch. Player-facing queries stalled and the on-call was staring at a chain of blocked transactions.',
    howOpsKeeperHelps:
      'The investigator traced the blocker back to a single batch job, the reviewer signed off on cancelling it with explicit blast radius, and the verifier confirmed lock waits returned to baseline. The postmortem landed in the team Slack within 60 seconds of recovery.',
    outcome: 'Launch saved. Postmortem in Slack within 60 seconds of recovery.',
    workflow: 'pg-lock-wait-long-transaction',
    severity: 'error',
  },
];

export default function UseCasesPage() {
  return (
    <>
      <Section className="pt-20 pb-12">
        <div className="max-w-3xl">
          <div className="mb-3 inline-flex items-center gap-2 rounded-full border border-white/10 bg-white/5 px-3 py-1 text-xs font-medium text-accent-300">
            Use cases
          </div>
          <h1 className="text-balance text-4xl font-semibold tracking-tight text-white sm:text-5xl">
            Where the closed loop earns its keep.
          </h1>
          <p className="mt-5 text-lg text-ink-300">
            Five real patterns from the OpsKeeper workflow catalog. Each one shipped as a
            reproducible scenario in the repo so you can replay it end-to-end.
          </p>
          <div className="mt-6 flex flex-wrap gap-3">
            <Button href="/docs/getting-started">Try the demos</Button>
            <Button href="/docs/workflow-catalog" variant="secondary">
              Full catalog
            </Button>
          </div>
        </div>
      </Section>

      <Section className="py-10">
        <div className="grid gap-6">
          {cases.map((c, i) => (
            <article
              key={c.headline}
              className="rounded-2xl border border-white/10 bg-white/[0.02] p-6 md:p-8"
            >
              <div className="grid gap-8 md:grid-cols-12">
                <div className="md:col-span-1">
                  <span className="inline-flex h-10 w-10 items-center justify-center rounded-md border border-white/10 bg-white/5 text-accent-300">
                    <c.icon className="h-5 w-5" />
                  </span>
                </div>
                <div className="md:col-span-8">
                  <div className="flex flex-wrap items-center gap-2 text-xs">
                    <span className="rounded-md border border-white/10 bg-white/5 px-2 py-0.5 font-mono uppercase tracking-wider text-ink-200">
                      {c.industry}
                    </span>
                    <span className="text-ink-400">·</span>
                    <span className="font-mono text-ink-300">{c.family}</span>
                  </div>
                  <h2 className="mt-3 text-2xl font-semibold tracking-tight text-white">
                    {c.headline}
                  </h2>
                  <div className="mt-4 grid gap-4 md:grid-cols-2">
                    <div>
                      <div className="text-xs font-medium uppercase tracking-wider text-ink-400">
                        Problem
                      </div>
                      <p className="mt-2 text-sm text-ink-300">{c.problem}</p>
                    </div>
                    <div>
                      <div className="text-xs font-medium uppercase tracking-wider text-ink-400">
                        How OpsKeeper helped
                      </div>
                      <p className="mt-2 text-sm text-ink-300">{c.howOpsKeeperHelps}</p>
                    </div>
                  </div>
                  <div className="mt-5 flex items-center gap-2 rounded-lg border border-accent-500/20 bg-accent-500/5 p-3 text-sm">
                    <CheckCircle2 className="h-4 w-4 flex-none text-accent-400" />
                    <span className="text-ink-100">{c.outcome}</span>
                  </div>
                </div>
                <div className="md:col-span-3">
                  <div className="rounded-xl border border-white/10 bg-white/[0.02] p-4">
                    <div className="text-xs font-medium uppercase tracking-wider text-ink-400">
                      Workflow
                    </div>
                    <div className="mt-1 font-mono text-sm text-white">{c.workflow}</div>
                    <div className="mt-3 text-xs font-medium uppercase tracking-wider text-ink-400">
                      Severity
                    </div>
                    <div
                      className={
                        c.severity === 'critical'
                          ? 'mt-1 text-sm text-rose-500'
                          : c.severity === 'error'
                          ? 'mt-1 text-sm text-amber-400'
                          : 'mt-1 text-sm text-ink-200'
                      }
                    >
                      {c.severity}
                    </div>
                  </div>
                </div>
              </div>
            </article>
          ))}
        </div>
      </Section>

      <Section className="py-10 md:py-16">
        <div className="rounded-3xl border border-white/10 bg-gradient-to-br from-accent-500/15 via-ink-900 to-ink-950 p-10 md:p-14">
          <h2 className="max-w-2xl text-balance text-3xl font-semibold tracking-tight text-white sm:text-4xl">
            Have a scenario that is not here?
          </h2>
          <p className="mt-3 max-w-xl text-ink-200">
            Open a PR with a new workflow under <code className="text-accent-300">workflows/</code>{' '}
            and a matching scenario under <code className="text-accent-300">deploy/incident-events/</code>.
            The four PG scenarios in the catalog were all contributed this way.
          </p>
          <div className="mt-6 flex flex-wrap gap-3">
            <Button href="https://github.com/louloulin/opskeeper/blob/main/CONTRIBUTING.md" external>
              Contributing guide
            </Button>
            <Link
              href="/docs"
              className="inline-flex items-center gap-2 rounded-md border border-white/10 bg-white/5 px-4 py-2 text-sm font-medium text-ink-100 hover:bg-white/10"
            >
              Read the docs <ArrowRight className="h-4 w-4" />
            </Link>
          </div>
        </div>
      </Section>
    </>
  );
}
