export const metadata = { title: 'Workflow catalog' };

const workflows = [
  {
    name: 'pg-connection-pool-exhaustion',
    family: 'capacity',
    severity: 'critical',
    summary:
      'Detect connection_pool_used_ratio > 0.9 across replicas, correlate to recent deploys, propose narrowing the pool floor.',
    phases: ['detected', 'correlated', 'investigated', 'critiqued', 'approved', 'recovered', 'verified', 'postmortem'],
  },
  {
    name: 'pg-disk-io-saturation',
    family: 'disk',
    severity: 'critical',
    summary:
      'Detect sustained disk_io_utilization > 95%, correlate to checkpoint storms, propose IOPS throttling or WAL tuning.',
    phases: ['detected', 'correlated', 'investigated', 'approved', 'recovered', 'verified'],
  },
  {
    name: 'pg-lock-wait-long-transaction',
    family: 'lock',
    severity: 'error',
    summary:
      'Detect lock waits > 30s, identify the head blocker, propose cancellation with explicit blast radius.',
    phases: ['detected', 'correlated', 'investigated', 'critiqued', 'approved', 'recovered', 'verified', 'postmortem'],
  },
  {
    name: 'pg-replica-replay-lag',
    family: 'replication',
    severity: 'warn',
    summary:
      'Detect replica replay lag > 60s, propose failover or read-only routing depending on the recovery budget.',
    phases: ['detected', 'correlated', 'investigated', 'approved', 'recovered', 'verified'],
  },
  {
    name: 'redis-eviction-storm',
    family: 'cache',
    severity: 'warn',
    summary:
      'Detect eviction rate spike, correlate to recent key TTL changes, propose policy tuning.',
    phases: ['detected', 'correlated', 'investigated', 'approved', 'verified'],
  },
];

export default function WorkflowCatalogPage() {
  return (
    <>
      <header>
        <div className="text-xs font-medium uppercase tracking-wider text-accent-300">
          Run as a team
        </div>
        <h1>Workflow catalog</h1>
        <p>
          Reference playbooks for the closed loop. Each entry is a real, reproducible incident
          pattern that ships in the repo or is described in <code>docs/workflow-catalog.md</code>.
        </p>
      </header>

      <h2 id="how-to-read">How to read a workflow</h2>
      <p>
        Each workflow lists the phases the loop will run through. A workflow that omits{' '}
        <code>critiqued</code> means severity is below critical and the critic is skipped. A
        workflow that omits <code>postmortem</code> means the change was below the postmortem
        threshold.
      </p>

      <h2 id="catalog">Catalog</h2>
      <div className="not-prose overflow-hidden rounded-2xl border border-white/10">
        <table className="w-full text-left text-sm">
          <thead className="bg-white/[0.03] text-ink-200">
            <tr>
              <th className="px-4 py-3 font-medium">Workflow</th>
              <th className="px-4 py-3 font-medium">Family</th>
              <th className="px-4 py-3 font-medium">Severity</th>
              <th className="px-4 py-3 font-medium">Summary</th>
            </tr>
          </thead>
          <tbody className="divide-y divide-white/5">
            {workflows.map((w) => (
              <tr key={w.name} className="align-top bg-white/[0.01]">
                <td className="px-4 py-3">
                  <div className="font-mono text-xs text-accent-300">{w.name}</div>
                  <div className="mt-1 flex flex-wrap gap-1">
                    {w.phases.map((p) => (
                      <span
                        key={p}
                        className="rounded border border-white/10 bg-white/5 px-1.5 py-0.5 font-mono text-[10px] text-ink-200"
                      >
                        {p}
                      </span>
                    ))}
                  </div>
                </td>
                <td className="px-4 py-3 text-ink-200">{w.family}</td>
                <td className="px-4 py-3">
                  <span
                    className={
                      w.severity === 'critical'
                        ? 'text-rose-500'
                        : w.severity === 'error'
                        ? 'text-amber-400'
                        : 'text-ink-200'
                    }
                  >
                    {w.severity}
                  </span>
                </td>
                <td className="px-4 py-3 text-ink-200">{w.summary}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>

      <h2 id="extending">Extending the catalog</h2>
      <p>
        Add a new workflow by contributing a YAML file under <code>workflows/</code> and a
        matching scenario under <code>deploy/incident-events/</code>. PRs are welcome — see{' '}
        <code>CONTRIBUTING.md</code>.
      </p>
    </>
  );
}
