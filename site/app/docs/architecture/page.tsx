import { CodeBlock } from '@/components/code-block';

export const metadata = { title: 'Architecture' };

export default function ArchitecturePage() {
  return (
    <>
      <header>
        <div className="text-xs font-medium uppercase tracking-wider text-accent-300">
          Get started
        </div>
        <h1>Architecture</h1>
        <p>
          OpsKeeper is one control plane, one data plane, and one plugin surface. This page
          explains how they fit together.
        </p>
      </header>

      <h2 id="control-plane">Control plane</h2>
      <p>
        The control plane runs incidents through an explicit state machine — the closed loop.
        Each transition is guarded and produces an append-only ledger event.
      </p>
      <CodeBlock language="text" title="control plane">
        {`┌──────────────────────────────────────────────────────────────┐
│                    OpsKeeper control plane                    │
│                                                              │
│   ┌─────────────┐  ┌─────────────┐  ┌────────────────────┐    │
│   │ loopbiz     │  │ manager     │  │ safety boundary    │    │
│   │ (state mch) │→ │ (dispatch)  │→ │ (proposal + audit) │    │
│   └─────┬───────┘  └─────┬───────┘  └─────────┬──────────┘    │
│         │                │                    │               │
└─────────┼────────────────┼────────────────────┼───────────────┘
          ▼                ▼                    ▼
       (ledger)        (workers)            (proposals)`}
      </CodeBlock>

      <h2 id="the-closed-loop">The closed loop</h2>
      <p>
        Eight phases, in order. The loop refuses to advance without a satisfied guard:
      </p>
      <ol>
        <li><strong>detected</strong> — alerts ingested from Prometheus, Loki, Tempo, webhooks.</li>
        <li><strong>correlated</strong> — semantic dedup across sources (rules + LLM with circuit breaker).</li>
        <li><strong>investigated</strong> — read-only RCA across metrics, logs, traces, git, hosts, topology.</li>
        <li><strong>critiqued</strong> — on severity ≥ critical, a peer critic audits the RCA.</li>
        <li><strong>approved</strong> — human-in-the-loop on a pending proposal.</li>
        <li><strong>recovered</strong> — narrow mutator runs the approved action.</li>
        <li><strong>verified</strong> — independent verifier returns a VerifiedDelta.</li>
        <li><strong>postmortem</strong> — reporter writes from pre-computed ReportFacts.</li>
      </ol>

      <h2 id="manager-dispatch">Manager dispatch</h2>
      <p>
        The manager owns a decision table that maps <code>(phase, severity, role)</code> to a
        worker. Every worker declares a <code>safety_level</code> from <code>L0</code> (read-only)
        to <code>L3</code> (mutating-with-proposal). The manager refuses to assign a worker whose
        level is above what the phase allows.
      </p>
      <CodeBlock language="yaml" title="dispatch table (excerpt)">
        {`- phase: detected
  severity: [info, warn, error, critical]
  worker: alerter
  safety_level: L0

- phase: investigated
  severity: [info, warn, error, critical]
  worker: investigator
  safety_level: L0

- phase: approved
  severity: [info, warn, error, critical]
  worker: repairer
  safety_level: L3   # requires pending proposal + human approver`}
      </CodeBlock>

      <h2 id="data-plane">Data plane</h2>
      <ul>
        <li><strong>PostgreSQL</strong> — incident memory, append-only ledger (<code>loop_event_log</code>, <code>loop_state</code>, <code>loop_contract</code>), MySQL <code>GET_LOCK</code> advisory locks for orchestrator serialization.</li>
        <li><strong>Qdrant</strong> — vector retrieval over historical incidents. Keyword recall + RRF ranking. Retained candidate-decision evidence per query.</li>
        <li><strong>Nacos Config</strong> — skill registry with HTTP 2.x API, local fallback, 30s polling hot-reload.</li>
        <li><strong>OpenTelemetry</strong> — W3C <code>traceparent</code> propagation end-to-end.</li>
      </ul>

      <h2 id="plugin-surface">Plugin surface</h2>
      <p>
        Two plugins ship in the repo:
      </p>
      <ul>
        <li><strong>agentteams-plugin-installer</strong> — turns the AgentTeams Dashboard into the OpsKeeper plugin console (5 extension points, HTTP API).</li>
        <li><strong>opskeeper-teamharness</strong> — worker-side plugin that exposes 14 MCP tools over stdio MCP. Bearer + HMAC + W3C traceparent auth.</li>
      </ul>

      <h2 id="ledger">Append-only ledger</h2>
      <p>
        Every dispatch and completion appends to <code>loop_event_log</code>. Each event is
        HMAC-chained: <code>hash_n = HMAC(hash_prev || event_n)</code>. On completion the event
        is sealed and exported to daily ndjson, with an optional Nacos history sync.
      </p>

      <h2 id="observability">Observability</h2>
      <ul>
        <li>Prometheus scrape config ships with the repo.</li>
        <li>Loki log streams and Tempo traces are correlated by <code>trace_id</code>.</li>
        <li>Grafana dashboards are provisioned for the closed loop, audit ledger, and skill health.</li>
      </ul>
    </>
  );
}
