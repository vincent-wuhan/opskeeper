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
        Worker 侧插件。把 OpsKeeper 的能力暴露给任何讲 stdio MCP 的 Worker —— Bearer + HMAC + W3C <code>traceparent</code> 鉴权，17 个工具，并自带 FastAPI HTTP 路由用于插件生命周期管理。
      </p>
      <CodeBlock language="bash" title="teamharness">
        {`# 构建插件包
bash plugins/opskeeper-teamharness/scripts/build-package.sh

# 跑插件测试
python3 -m unittest discover -s plugins/opskeeper-teamharness -p 'test_*.py'

# 跑 stdio MCP 代理（环境变量配置）
cd plugins/opskeeper-teamharness
OPSKEEPER_BACKEND_URL=http://localhost:8080 \\
OPSKEEPER_GATEWAY_KEY="$GATEWAY_KEY" \\
python3 mcp/server.py`}
      </CodeBlock>

      <h3 id="mcp-tools">MCP 工具</h3>
      <p><code>opskeeper-teamharness</code> 暴露的 17 个工具：</p>
      <ul>
        <li><code>loop.investigate</code> —— 对事件触发 RCA</li>
        <li><code>loop.correlate</code> —— 把告警关联成事件</li>
        <li><code>recovery.verify</code> —— 独立恢复验证</li>
        <li><code>recovery.execute</code> —— 窄域授权的修复动作</li>
        <li><code>metric.query</code> —— 查询指标后端</li>
        <li><code>incident.list</code> / <code>incident.get</code> —— 事件记忆库</li>
        <li><code>postgres.analyze_status</code> —— PostgreSQL 状态快照</li>
        <li><code>host.get_load</code> / <code>host.get_processes</code> / <code>host.restart_service</code></li>
        <li><code>knowledge.query</code> / <code>knowledge.write</code> —— 知识库</li>
        <li><code>hitl.decide</code> —— 人工审批决策</li>
        <li><code>state.put</code> / <code>state.get</code> —— 共享状态</li>
        <li><code>incident.record</code> —— 向时间线追加证据 / 恢复信号</li>
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
