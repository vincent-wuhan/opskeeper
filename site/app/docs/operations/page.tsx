import { CodeBlock } from '@/components/code-block';

export const metadata = { title: 'Operations manual' };

export default function OperationsPage() {
  return (
    <>
      <header>
        <div className="text-xs font-medium uppercase tracking-wider text-accent-300">
          Operate
        </div>
        <h1>Operations manual</h1>
        <p>
          Day-2 operations for OpsKeeper. The full document lives at{' '}
          <code>docs/operations-manual.md</code> in the repo; this page is the executive summary.
        </p>
      </header>

      <h2 id="backups">Backups</h2>
      <p>
        Back up three things: PostgreSQL (the ledger is authoritative), Qdrant snapshots
        (vector memory), and the daily ndjson exports (audit chain).
      </p>
      <CodeBlock language="bash" title="backup">
        {`# 1. PostgreSQL logical backup
pg_dump --schema=public --file=opskeeper-$(date +%F).sql "$POSTGRES_DSN"

# 2. Qdrant snapshot
curl -X POST "$QDRANT_URL/snapshots" -H 'content-type: application/json' \\
  -d '{"collection_name":"opskeeper_incidents"}'

# 3. Daily ndjson (HMAC-chained) is already on disk under /var/lib/opskeeper/ledger/
#    Sync to object storage with your existing pipeline.`}
      </CodeBlock>

      <h2 id="key-rotation">Key and HMAC rotation</h2>
      <ul>
        <li><strong>Plugin HMAC</strong>: rotate via <code>opskeeper plugin rotate-secret</code>. The new key is staged; the old key is honored for 24 hours.</li>
        <li><strong>JWT signing key</strong>: rotate via the API. Old tokens expire on their next refresh.</li>
        <li><strong>Ledger HMAC root</strong>: re-keying requires a fresh append-only chain; the old chain is sealed and archived.</li>
      </ul>

      <h2 id="scaling">Scaling</h2>
      <p>
        The control plane is stateless; scale horizontally behind a TCP load balancer. The
        orchestrator serializes per-incident via MySQL <code>GET_LOCK</code>, so contention is
        bounded by the active incident count, not the control plane size.
      </p>

      <h2 id="monitoring">Monitoring</h2>
      <p>
        Grafana dashboards are provisioned automatically. Key signals:
      </p>
      <ul>
        <li><code>opskeeper_loop_phase_duration_seconds</code> — time spent in each phase (p50 / p95 / p99).</li>
        <li><code>opskeeper_proposals_pending</code> — count of pending proposals awaiting human approval.</li>
        <li><code>opskeeper_audit_chain_valid</code> — boolean: is the HMAC chain still verifiable end-to-end?</li>
        <li><code>opskeeper_skill_health</code> — per-skill success/failure over the last 5 minutes.</li>
      </ul>

      <h2 id="incident-drill">Incident drill</h2>
      <p>
        Run a drill at least once per quarter. The repo ships four reproducible PostgreSQL
        scenarios; rotate through them and confirm:
      </p>
      <ol>
        <li>The loop reaches <code>postmortem</code> for each scenario.</li>
        <li>The audit chain is verifiable after the drill.</li>
        <li>The verifier returns a <code>VerifiedDelta</code> matching the expected metric allowlist.</li>
        <li>The reporter writes a postmortem in &lt; 60 seconds.</li>
      </ol>

      <h2 id="data-retention">Data retention</h2>
      <p>
        Ledger events are retained for 365 days by default. Vector memory is retained
        indefinitely unless your retention policy deletes it. Daily ndjson exports are the
        long-term authoritative record.
      </p>
    </>
  );
}
