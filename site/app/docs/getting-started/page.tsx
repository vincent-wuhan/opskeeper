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
      <p>Clone the repo and bring up the local stack:</p>
      <CodeBlock language="bash" title="bootstrap">
        {`git clone https://github.com/louloulin/opskeeper.git
cd opskeeper

# Start Postgres, Qdrant, and the OpsKeeper control plane
docker compose up -d opskeeper postgres qdrant

# Verify the control plane is healthy
curl -fsS http://localhost:8090/healthz`}
      </CodeBlock>

      <h2 id="replay-a-scenario" className="scroll-mt-24">2. Replay a scenario</h2>
      <p>
        Four reproducible PostgreSQL scenarios ship in <code>deploy/incident-events/</code>.
        Pick one and replay it end-to-end:
      </p>
      <CodeBlock language="bash" title="replay">
        {`# Connection-pool exhaustion
opskeeper demo replay pg-connection-pool-exhaustion

# Disk I/O saturation
opskeeper demo replay pg-disk-io-saturation

# Lock-wait / long transaction
opskeeper demo replay pg-lock-wait-long-transaction

# Replica replay lag
opskeeper demo replay pg-replica-replay-lag`}
      </CodeBlock>
      <p>
        The CLI drives the scenario through every phase of the closed loop. Inspect any incident:
      </p>
      <CodeBlock language="bash" title="inspect">
        {`opskeeper incident show INC-PG-POOL-001 \\
  --include timeline,evidence,proposals,audit`}
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
        Drop the TeamHarness MCP proxy into any worker that speaks stdio MCP. It exposes 14 tools
        and authenticates with Bearer + HMAC.
      </p>
      <CodeBlock language="bash" title="plugin">
        {`opskeeper plugin install agentteams-plugin-installer
opskeeper-teamharness serve \\
  --mcp-transport stdio \\
  --opskeeper-endpoint http://localhost:8090 \\
  --hmac-secret "$OPSKEEPER_PLUGIN_HMAC"`}
      </CodeBlock>

      <h2 id="demos" className="scroll-mt-24">Demos</h2>
      <p>
        After the first replay, run the verification harness to confirm the manager, critic, and
        verifier are wired correctly:
      </p>
      <CodeBlock language="bash" title="verify">
        {`make verify
# alert_storm / rca_loop / recovery_verify scenarios`}
      </CodeBlock>

      <h2 id="next">Next</h2>
      <p>
        Read the <Link href="/docs/architecture">Architecture</Link> doc to understand the data
        plane, the manager dispatch table, and the safety boundary.
      </p>
    </>
  );
}
