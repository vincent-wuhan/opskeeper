import { Section, SectionHeader } from '@/components/section';
import { CodeBlock } from '@/components/code-block';
import { CheckCircle2, Boxes, Cpu, Database, Network, PlugZap, ShieldCheck } from 'lucide-react';

export const metadata = {
  title: 'Integrations',
  description:
    'Integrate OpsKeeper with the AgentTeams Dashboard, your observability stack, and any worker via the stdio MCP proxy.',
};

const groups = [
  {
    title: 'AgentTeams Dashboard',
    desc: 'The agentteams-plugin-installer turns the AgentTeams Dashboard into the OpsKeeper plugin console. Adds five extension points and an HTTP API on top of the file-system PluginRegistry.',
    bullets: [
      'Extension points: sidebar-menu, route, dashboard-widget, detail-panel, toolbar',
      'HTTP API: /v1/plugins list/get/install/uninstall/enable/disable/sync/push',
      'Hard limits: 10 MB zip, 50 MB extract, 1000 files, zip-slip protection',
    ],
    icon: Boxes,
  },
  {
    title: 'OpsKeeper TeamHarness',
    desc: 'A worker-side plugin that lets the six operational workers call OpsKeeper through a stdio MCP proxy. 17 MCP tools, Bearer + HMAC + W3C traceparent auth.',
    bullets: [
      'stdio MCP server, Streamable HTTP-compatible',
      'FastAPI router at /api/opskeeper-teamharness/{health,sync,install-plugin}',
      'Hot-deploy with qwenpaw plugin install',
    ],
    icon: PlugZap,
  },
  {
    title: 'Observability',
    desc: 'First-class integration with the Grafana stack — Prometheus scrape, Loki log streams, Tempo traces, and provisioned Grafana dashboards for the closed loop.',
    bullets: [
      'Metric scrape config ships with the repo',
      'Log/metric/trace correlation by trace_id',
      'Provisioned dashboards: closed-loop, audit ledger, skill health',
    ],
    icon: Network,
  },
  {
    title: 'Data plane',
    desc: 'PostgreSQL for incident memory and append-only ledger, Qdrant for vector retrieval, Nacos for skill registry.',
    bullets: [
      'PostgreSQL: ledger + state machine serialization via MySQL GET_LOCK',
      'Qdrant: keyword recall + RRF ranking, retained candidate-decision evidence',
      'Nacos Config: HTTP 2.x API + local fallback + 30s hot reload',
    ],
    icon: Database,
  },
  {
    title: 'Tooling',
    desc: 'W3C traceparent propagation end-to-end across worker → MCP proxy → control plane → web console. OpenTelemetry-compatible.',
    bullets: [
      'OpenTelemetry SDK with W3C traceparent',
      'Propagates across stdio MCP boundaries',
      'Drop-in OTLP adapter for AgentLoop / LoongSuite (roadmap)',
    ],
    icon: Cpu,
  },
  {
    title: 'Security',
    desc: 'Append-only event log, SHA256-chained proposal audit, Bearer + HMAC on plugin endpoints, role-based auth on every API.',
    bullets: [
      'loop_event_log append-only (DB trigger enforced)',
      'chat_proposal_audit SHA256 hash chain + in-repo verifier',
      'Bearer + HMAC on plugin endpoints',
    ],
    icon: ShieldCheck,
  },
];

const mcpExample = `{
  "jsonrpc": "2.0",
  "id": 1,
  "method": "tools/list",
  "params": {}
}
// → 17 tools exposed by opskeeper-teamharness:
//   loop.investigate
//   loop.correlate
//   recovery.verify
//   recovery.execute
//   metric.query
//   incident.list / incident.get
//   postgres.analyze_status
//   host.get_load / host.get_processes / host.restart_service
//   knowledge.query / knowledge.write
//   hitl.decide
//   state.put / state.get
//   incident.record`;

const pluginInstall = `# Build + install the AgentTeams Dashboard plugin
make build-plugins                     # zip lands in dist/plugins/

# Or run the stdio MCP proxy directly for any worker
cd plugins/opskeeper-teamharness
OPSKEEPER_BACKEND_URL=http://localhost:8080 \\
OPSKEEPER_GATEWAY_KEY="$GATEWAY_KEY" \\
python3 mcp/server.py`;

export default function IntegrationsPage() {
  return (
    <>
      <Section className="pt-20 pb-12">
        <div className="max-w-3xl">
          <div className="mb-3 inline-flex items-center gap-2 rounded-full border border-white/10 bg-white/5 px-3 py-1 text-xs font-medium text-accent-300">
            Integrations
          </div>
          <h1 className="text-balance text-4xl font-semibold tracking-tight text-white sm:text-5xl">
            Drop-in for the tools you already run.
          </h1>
          <p className="mt-5 text-lg text-ink-300">
            OpsKeeper integrates with the AgentTeams Dashboard, the Grafana stack, and the
            standard data plane. New workers join via the stdio MCP proxy — no SDK lock-in.
          </p>
        </div>
      </Section>

      <Section className="py-10">
        <div className="grid gap-4 md:grid-cols-2">
          {groups.map((g) => (
            <div
              key={g.title}
              className="rounded-2xl border border-white/10 bg-white/[0.02] p-6"
            >
              <div className="flex items-center gap-3">
                <span className="inline-flex h-10 w-10 items-center justify-center rounded-md border border-white/10 bg-white/5 text-accent-300">
                  <g.icon className="h-5 w-5" />
                </span>
                <h3 className="text-lg font-semibold text-white">{g.title}</h3>
              </div>
              <p className="mt-3 text-sm text-ink-300">{g.desc}</p>
              <ul className="mt-4 space-y-2 text-sm text-ink-300">
                {g.bullets.map((b) => (
                  <li key={b} className="flex items-start gap-2">
                    <CheckCircle2 className="mt-0.5 h-4 w-4 flex-none text-accent-400" />
                    <span>{b}</span>
                  </li>
                ))}
              </ul>
            </div>
          ))}
        </div>
      </Section>

      <Section className="py-10 md:py-16">
        <div className="grid gap-10 lg:grid-cols-12 lg:items-start">
          <div className="lg:col-span-5">
            <SectionHeader
              eyebrow="stdio MCP"
              title="Any worker. One protocol."
              description="The OpsKeeper Worker plugin speaks JSON-RPC over stdio. Discover 17 tools, propagate W3C traceparent, and authenticate with Bearer + HMAC. Streamable HTTP is on the roadmap."
            />
          </div>
          <div className="lg:col-span-7">
            <CodeBlock language="json" title="tools/list">
              {mcpExample}
            </CodeBlock>
          </div>
        </div>
      </Section>

      <Section className="py-10 md:py-16">
        <div className="grid gap-10 lg:grid-cols-12 lg:items-start">
          <div className="lg:col-span-5">
            <SectionHeader
              eyebrow="Install"
              title="Two commands and you have a worker."
              description="Install the Dashboard plugin, or run the MCP proxy directly for any worker. Hot-deploy with qwenpaw plugin install."
            />
          </div>
          <div className="lg:col-span-7">
            <CodeBlock language="bash" title="install · plugin + mcp">
              {pluginInstall}
            </CodeBlock>
          </div>
        </div>
      </Section>
    </>
  );
}
