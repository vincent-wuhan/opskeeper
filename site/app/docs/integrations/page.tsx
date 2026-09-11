import { CodeBlock } from '@/components/code-block';

export const metadata = { title: 'Integrations' };

export default function IntegrationsDocPage() {
  return (
    <>
      <header>
        <div className="text-xs font-medium uppercase tracking-wider text-accent-300">
          Build
        </div>
        <h1>Integrations</h1>
        <p>
          Connect OpsKeeper to your alert sources, observability stack, and worker fleet. The
          full reference lives at <code>docs/integration-guide.md</code>.
        </p>
      </header>

      <h2 id="alert-sources">Alert sources</h2>
      <p>
        OpsKeeper ingests alerts from Prometheus, Loki, Tempo, webhooks, and on-call channels.
        Configure each source in <code>configs/sources.yaml</code>:
      </p>
      <CodeBlock language="yaml" title="sources.yaml">
        {`sources:
  prometheus:
    endpoint: http://prometheus:9090
    rules:
      - pg_connection_pool_used_ratio > 0.9
      - disk_io_utilization > 95

  loki:
    endpoint: http://loki:3100
    labels:
      job: opskeeper

  tempo:
    endpoint: http://tempo:3200
    match_by: [service.namespace, deployment.environment]

  webhook:
    path: /api/v1/webhook/alerts
    hmac_secret: $OPSKEEPER_WEBHOOK_HMAC`}
      </CodeBlock>

      <h2 id="skills">Skills</h2>
      <p>
        A skill is a worker&apos;s declaration: name, role, phase, safety level, tool allowlist.
        Skills live in Nacos Config with a 30s polling hot-reload, with a local fallback for
        air-gapped installs.
      </p>
      <CodeBlock language="yaml" title="skill_meta.yaml">
        {`name: alerter
role: intake
phase: [detected, correlated]
safety_level: L0
max_turns: 12
tool_allowlist:
  - read:alerts
  - read:topics
  - dedup:rules
  - dedup:LLM`}
      </CodeBlock>

      <h2 id="observability">Observability</h2>
      <ul>
        <li>W3C <code>traceparent</code> propagation end-to-end.</li>
        <li>Provisioned Grafana dashboards for the closed loop, audit ledger, and skill health.</li>
        <li>Loki log streams and Tempo traces correlated by <code>trace_id</code>.</li>
      </ul>

      <h2 id="mcp">MCP</h2>
      <p>
        Workers join OpsKeeper via the stdio MCP proxy in <code>opskeeper-teamharness</code>.
        The protocol is JSON-RPC 2.0; authentication is Bearer + HMAC + W3C traceparent.
      </p>
      <CodeBlock language="json" title="tools/list">
        {`{
  "jsonrpc": "2.0",
  "id": 1,
  "method": "tools/list",
  "params": {}
}`}
      </CodeBlock>

      <h2 id="hot-reload">Hot reload</h2>
      <p>
        Edit a <code>skill_meta.yaml</code>, push it to Nacos, and the manager picks it up
        within 30 seconds. No restart required.
      </p>
    </>
  );
}
