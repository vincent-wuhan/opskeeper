import Link from 'next/link';
import { CodeBlock } from '@/components/code-block';

export const metadata = { title: 'Getting started' };

export default function GettingStartedPage() {
  return (
    <>
      <header>
        <div className="text-xs font-medium uppercase tracking-wider text-accent-300">
          Get started
        </div>
        <h1>Getting started</h1>
        <p>
          This guide walks you through running OpsKeeper locally with the closed-loop demo
          scenarios. You will need Docker, Go 1.25+, Node.js 20+, and pnpm 9+ — all of which the
          Makefile will check for you.
        </p>
      </header>

      <h2 id="prerequisites">Prerequisites</h2>
      <ul>
        <li>Docker 24+ with the Compose plugin</li>
        <li>Go 1.25+ (see <code>.tool-versions</code>)</li>
        <li>Node.js 20+ and pnpm 9+</li>
        <li>Python 3.11+ (only needed for plugin tests)</li>
        <li>zip / tar / standard POSIX shell tools</li>
      </ul>

      <h2 id="install">1. Install</h2>
      <p>Clone the repo and bring up the local stack from the repo root:</p>
      <CodeBlock language="bash" title="bootstrap">
        {`git clone https://github.com/vincent-wuhan/opskeeper.git
cd opskeeper
cp deploy/demo.env.example .env

# Build and start the full demo stack (first run takes a few minutes)
docker compose up -d --build
docker compose ps                      # wait until every service is healthy

# Verify the control plane is healthy (opskeeper serves HTTP on 8080)
curl -fsS http://localhost:8080/healthz`}
      </CodeBlock>

      <h2 id="replay-a-scenario" className="scroll-mt-24">2. Seed a scenario</h2>
      <p>
        Four reproducible PostgreSQL scenarios ship in <code>deploy/incident-events/</code>.
        Seed them into incident memory with the incident-seed tool:
      </p>
      <CodeBlock language="bash" title="seed">
        {`# Validate the datasets first (no database writes)
go run ./cmd/incident-seed -dry-run -dir deploy/incident-events

# Write all 4 scenarios into incident memory
go run ./cmd/incident-seed \\
  -dsn "postgres://opskeeper:opskeeper@localhost:5432/opskeeper?sslmode=disable" \\
  -dir deploy/incident-events`}
      </CodeBlock>
      <p>
        The seeded incidents cover connection-pool exhaustion, disk I/O saturation, lock-wait /
        long transaction, and replica replay lag. Inspect timelines, recall logs, and runbooks
        over the incident API, or browse them in the web console:
      </p>
      <CodeBlock language="bash" title="inspect">
        {`# Incident metrics + runbooks served by the control plane
curl -fsS "http://localhost:8080/v1/incidents/metrics" | jq .
curl -fsS "http://localhost:8080/v1/incidents/runbooks" | jq .`}
      </CodeBlock>

      <h2 id="open-the-web-console" className="scroll-mt-24">3. Open the web console</h2>
      <p>
        The OpsKeeper web console is a React + Vite SPA under <code>web/</code>. It talks to the
        control plane via <code>/api/v1</code>.
      </p>
      <CodeBlock language="bash" title="web">
        {`cd web
pnpm install --frozen-lockfile
pnpm dev
# → http://localhost:5173`}
      </CodeBlock>

      <h2 id="install-a-worker-plugin">4. Install a worker plugin</h2>
      <p>
        Drop the TeamHarness MCP proxy into any worker that speaks stdio MCP. It exposes 17 tools
        and authenticates with Bearer + HMAC. The server reads its configuration from environment
        variables:
      </p>
      <CodeBlock language="bash" title="plugin">
        {`cd plugins/opskeeper-teamharness

OPSKEEPER_BACKEND_URL=http://localhost:8080 \\
OPSKEEPER_GATEWAY_KEY="$GATEWAY_KEY" \\
OPSKEEPER_TENANT_ID=default \\
python3 mcp/server.py`}
      </CodeBlock>
      <p>
        For the AgentTeams Dashboard plugin package, run <code>make build-plugins</code> and
        install the resulting zip from the Dashboard (hot-deploy:{' '}
        <code>qwenpaw plugin install &lt;path&gt; --force</code>).
      </p>

      <h2 id="demos" className="scroll-mt-24">Demos</h2>
      <p>
        If you want to inspect the hosted environment first, the{' '}
        <Link href="/demo">live demo page</Link> links the OpsKeeper console, AgentTeams
        Dashboard, and AgentTeams Element room used for the end-to-end demonstration.
      </p>
      <p>
        After seeding, run the verification harness scenarios to confirm the manager, critic, and
        verifier are wired correctly. Each scenario is a self-contained compose stack with a
        runner container:
      </p>
      <CodeBlock language="bash" title="verify">
        {`# From the plugin directory — alert_storm / rca_loop / recovery_verify
bash plugins/opskeeper-teamharness/eval/scenarios/alert_storm/run.sh
bash plugins/opskeeper-teamharness/eval/scenarios/rca_loop/run.sh
bash plugins/opskeeper-teamharness/eval/scenarios/recovery_verify/run.sh`}
      </CodeBlock>

      <h2 id="next">Next</h2>
      <p>
        Read the <Link href="/docs/architecture">Architecture</Link> doc to understand the data
        plane, the manager dispatch table, and the safety boundary.
      </p>
    </>
  );
}
