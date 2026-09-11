import { CodeBlock } from '@/components/code-block';

export const metadata = { title: '插件' };

export default function PluginsZhPage() {
  return (
    <>
      <header>
        <div className="text-xs font-medium uppercase tracking-wider text-accent-300">
          扩展
        </div>
        <h1>插件</h1>
        <p>OpsKeeper 自带两个插件，均按项目许可证开源。</p>
      </header>

      <h2 id="agentteams-plugin-installer">agentteams-plugin-installer</h2>
      <p>
        Dashboard 侧插件，把 AgentTeams Dashboard 变成 OpsKeeper 的插件控制台。暴露 5 个扩展点和一套文件系统 PluginRegistry 的 HTTP API。
      </p>
      <h3 id="extension-points">扩展点</h3>
      <ul>
        <li><code>sidebar-menu</code> —— 增加导航条目</li>
        <li><code>route</code> —— 挂载插件路由</li>
        <li><code>dashboard-widget</code> —— 在看板渲染小组件</li>
        <li><code>detail-panel</code> —— 在详情页附加面板</li>
        <li><code>toolbar</code> —— 增加工具栏动作</li>
      </ul>
      <h3 id="registry-limits">注册表限制</h3>
      <ul>
        <li>10 MB 上传 zip</li>
        <li>50 MB 解压后大小</li>
        <li>每个包 1000 个文件</li>
        <li>带 zip-slip 防护</li>
      </ul>

      <h2 id="opskeeper-teamharness">opskeeper-teamharness</h2>
      <p>
        Worker 侧插件。把 OpsKeeper 的能力暴露给任何讲 stdio MCP 的 Worker —— Bearer + HMAC + W3C <code>traceparent</code> 鉴权，14 个工具，并自带 FastAPI HTTP 路由用于插件生命周期管理。
      </p>
      <CodeBlock language="bash" title="teamharness">
        {`# 构建插件包
bash plugins/opskeeper-teamharness/scripts/build-package.sh

# 跑插件测试
python3 -m unittest discover -s plugins/opskeeper-teamharness -p 'test_*.py'

# 跑 stdio MCP 代理
opskeeper-teamharness serve \\
  --mcp-transport stdio \\
  --opskeeper-endpoint http://localhost:8090 \\
  --hmac-secret "$OPSKEEPER_PLUGIN_HMAC"`}
      </CodeBlock>

      <h3 id="mcp-tools">MCP 工具</h3>
      <p><code>opskeeper-teamharness</code> 暴露的 14 个工具：</p>
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

      <h2 id="auth">鉴权</h2>
      <ul>
        <li><strong>Bearer</strong> —— 短期 JWT，用于 HTTP API。</li>
        <li><strong>HMAC</strong> —— 每个插件调用都使用共享密钥签名，覆盖 method、path、body。</li>
        <li><strong>W3C traceparent</strong> —— 在 stdio MCP 边界间传递。</li>
      </ul>

      <h2 id="verifying-plugins">验证插件</h2>
      <CodeBlock language="bash" title="verify">
        {`make build-plugins
make test-plugins
make verify-plugins`}
      </CodeBlock>
    </>
  );
}
