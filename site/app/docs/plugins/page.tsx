import { CodeBlock } from '@/components/code-block';

export const metadata = { title: 'Plugins' };

export default function PluginsPage() {
  return (
    <>
      <header>
        <div className="text-xs font-medium uppercase tracking-wider text-accent-300">
          Build
        </div>
        <h1>Plugins</h1>
        <p>
          Two plugins ship with OpsKeeper. Both are open source under the project license.
        </p>
      </header>

      <h2 id="agentteams-plugin-installer">agentteams-plugin-installer</h2>
      <p>
        A Dashboard-side plugin that turns the AgentTeams Dashboard into the OpsKeeper plugin
        console. It exposes five extension points and an HTTP API for the file-system
        PluginRegistry.
      </p>
      <h3 id="extension-points">Extension points</h3>
      <ul>
        <li><code>sidebar-menu</code> — add a navigation entry</li>
        <li><code>route</code> — mount a plugin route</li>
        <li><code>dashboard-widget</code> — render a widget on the dashboard</li>
        <li><code>detail-panel</code> — attach a panel to a detail view</li>
        <li><code>toolbar</code> — add a toolbar action</li>
      </ul>
      <h3 id="registry-limits">Registry limits</h3>
      <ul>
        <li>10 MB upload zip</li>
        <li>50 MB extracted size</li>
        <li>1000 files per package</li>
        <li>Zip-slip protection on extraction</li>
      </ul>

      <h2 id="opskeeper-teamharness">opskeeper-teamharness</h2>
      <p>
        The worker-side plugin. It exposes OpsKeeper&apos;s capabilities to any worker that speaks
        stdio MCP — Bearer + HMAC + W3C <code>traceparent</code> auth, 17 tools, FastAPI HTTP
        router for plugin lifecycle.
      </p>
      <CodeBlock language="bash" title="teamharness">
        {`# Build the plugin package
bash plugins/opskeeper-teamharness/scripts/build-package.sh

# Run the plugin tests
python3 -m unittest discover -s plugins/opskeeper-teamharness -p 'test_*.py'

# Serve the stdio MCP proxy
opskeeper-teamharness serve \\
  --mcp-transport stdio \\
  --opskeeper-endpoint http://localhost:8090 \\
  --hmac-secret "$OPSKEEPER_PLUGIN_HMAC"`}
      </CodeBlock>

      <h3 id="mcp-tools">MCP tools</h3>
      <p>The 17 tools exposed by <code>opskeeper-teamharness</code>:</p>
      <ul>
        <li><code>loop.investigate</code> &mdash; trigger RCA on an incident</li>
        <li><code>loop.correlate</code> &mdash; correlate alerts into an incident</li>
        <li><code>recovery.verify</code> &mdash; independent recovery verification</li>
        <li><code>recovery.execute</code> &mdash; narrowly-authorized repair action</li>
        <li><code>metric.query</code> &mdash; query the metrics backend</li>
        <li><code>incident.list</code> / <code>incident.get</code> &mdash; incident memory</li>
        <li><code>postgres.analyze_status</code> &mdash; PostgreSQL status snapshot</li>
        <li><code>host.get_load</code> / <code>host.get_processes</code> / <code>host.restart_service</code></li>
        <li><code>knowledge.query</code> / <code>knowledge.write</code> &mdash; knowledge vault</li>
        <li><code>hitl.decide</code> &mdash; human-in-the-loop decision</li>
        <li><code>state.put</code> / <code>state.get</code> &mdash; shared state</li>
        <li><code>incident.record</code> &mdash; append evidence / recovery signals to the timeline</li>
      </ul>

      <h2 id="auth">Authentication</h2>
      <ul>
        <li><strong>Bearer</strong> — short-lived JWT for the HTTP API.</li>
        <li><strong>HMAC</strong> — every plugin call is signed with the shared secret. The signature covers method, path, and body.</li>
        <li><strong>W3C traceparent</strong> — propagated across stdio MCP boundaries.</li>
      </ul>

      <h2 id="verifying-plugins">Verifying plugins</h2>
      <CodeBlock language="bash" title="verify">
        {`make build-plugins
make test-plugins
make verify-plugins`}
      </CodeBlock>
    </>
  );
}
