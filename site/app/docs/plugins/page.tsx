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
        stdio MCP — Bearer + HMAC + W3C <code>traceparent</code> auth, 14 tools, FastAPI HTTP
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
      <p>The 14 tools exposed by <code>opskeeper-teamharness</code>:</p>
      <ul>
        <li><code>opskeeper.incident.list</code></li>
        <li><code>opskeeper.incident.show</code></li>
        <li><code>opskeeper.incident.timeline</code></li>
        <li><code>opskeeper.incident.evidence</code></li>
        <li><code>opskeeper.proposal.create</code></li>
        <li><code>opskeeper.proposal.show</code></li>
        <li><code>opskeeper.proposal.approve</code></li>
        <li><code>opskeeper.proposal.reject</code></li>
        <li><code>opskeeper.skill.list</code></li>
        <li><code>opskeeper.skill.deploy</code></li>
        <li><code>opskeeper.skill.uninstall</code></li>
        <li><code>opskeeper.audit.query</code></li>
        <li><code>opskeeper.audit.replay</code></li>
        <li><code>opskeeper.health</code></li>
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
