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
        Back up two things: PostgreSQL (the ledger and incident memory are authoritative) and
        Qdrant snapshots (vector memory). There is no separate export pipeline — the database
        is the source of truth.
      </p>
      <CodeBlock language="bash" title="backup">
        {`# 1. PostgreSQL logical backup
pg_dump --schema=public --file=opskeeper-$(date +%F).sql "$POSTGRES_DSN"

# 2. Qdrant snapshot
curl -X POST "$QDRANT_URL/snapshots" -H 'content-type: application/json' \\
  -d '{"collection_name":"opskeeper_incidents"}'`}
      </CodeBlock>

      <h2 id="key-rotation">Key rotation</h2>
      <ul>
        <li><strong>Plugin HMAC secrets</strong>: stage a new secret in your secret manager, then roll workers. The MCP proxy reads <code>OPSKEEPER_GATEWAY_KEY</code> from the environment at startup.</li>
        <li><strong>JWT signing key</strong>: rotate via the API. Old tokens expire on their next refresh.</li>
        <li><strong>Edge secret keys</strong>: <code>RotateSecret</code> regenerates an edge&apos;s key and replaces the stored hash.</li>
      </ul>

      <h2 id="scaling">Scaling</h2>
      <p>
        The control plane is stateless; scale horizontally behind a TCP load balancer. The
        orchestrator serializes per-incident via MySQL <code>GET_LOCK</code>, so contention is
        bounded by the active incident count, not the control plane size.
      </p>

      <h2 id="monitoring">Monitoring</h2>
      <p>
        Grafana dashboards are provisioned automatically. Key signals emitted by the control
        plane:
      </p>
      <ul>
        <li><code>loop_phase_total</code> / <code>loop_phase_duration_seconds</code> — closed-loop throughput and latency per phase.</li>
        <li><code>opskeeper_tool_invocations_total</code> / <code>opskeeper_tool_duration_seconds</code> — per-tool call counts and latency.</li>
        <li><code>opskeeper_llm_requests_total</code> / <code>opskeeper_llm_tokens_total</code> — LLM usage per worker.</li>
        <li><code>opskeeper_http_requests_total</code> / <code>opskeeper_http_request_duration_seconds</code> — API health.</li>
      </ul>

      <h2 id="incident-drill">Incident drill</h2>
      <p>
        Run a drill at least once per quarter. The repo ships four reproducible PostgreSQL
        scenarios; seed them with <code>cmd/incident-seed</code> and confirm:
      </p>
      <ol>
        <li>The loop reaches <code>postmortem</code> for each scenario.</li>
        <li>The proposal audit chain verifies (walk <code>chat_proposal_audit</code> hashes).</li>
        <li>The verifier returns a <code>VerifiedDelta</code> matching the expected metric allowlist.</li>
        <li>The reporter writes a postmortem in &lt; 60 seconds.</li>
      </ol>

      <h2 id="data-retention">Data retention</h2>
      <p>
        loop_event_log rows carry your tenant and timestamp; set retention with a scheduled
        purge per tenant. Vector memory is retained until your retention policy deletes it.
        External anchoring of a daily chain root (transparency log) is on the roadmap — until
        then, database backups are the durable record.
      </p>
    </>
  );
}
