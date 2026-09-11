import { CodeBlock } from '@/components/code-block';

export const metadata = { title: '集成' };

export default function IntegrationsDocZhPage() {
  return (
    <>
      <header>
        <div className="text-xs font-medium uppercase tracking-wider text-accent-300">
          扩展
        </div>
        <h1>集成</h1>
        <p>
          把 OpsKeeper 接入你的告警源、可观测栈和 Worker 集群。完整参考在 <code>docs/integration-guide.md</code>。
        </p>
      </header>

      <h2 id="alert-sources">告警源</h2>
      <p>
        OpsKeeper 从 Prometheus / Loki / Tempo / Webhook / On-call 通道接入告警。在 <code>configs/sources.yaml</code> 中配置每个源：
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

      <h2 id="skills">技能</h2>
      <p>
        一个技能就是一个 Worker 的声明：名称、角色、阶段、安全级别、工具白名单。技能放在 Nacos Config 中，30 秒轮询热加载；离线安装时本地降级。
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

      <h2 id="observability">可观测</h2>
      <ul>
        <li>端到端 W3C <code>traceparent</code> 传递。</li>
        <li>为闭环、审计账本、技能健康度预置 Grafana 仪表盘。</li>
        <li>Loki 日志流和 Tempo 追踪按 <code>trace_id</code> 关联。</li>
      </ul>

      <h2 id="mcp">MCP</h2>
      <p>
        Worker 通过 <code>opskeeper-teamharness</code> 中的 stdio MCP 代理接入 OpsKeeper。协议是 JSON-RPC 2.0；鉴权是 Bearer + HMAC + W3C traceparent。
      </p>
      <CodeBlock language="json" title="tools/list">
        {`{
  "jsonrpc": "2.0",
  "id": 1,
  "method": "tools/list",
  "params": {}
}`}
      </CodeBlock>

      <h2 id="hot-reload">热加载</h2>
      <p>
        编辑 <code>skill_meta.yaml</code>、推送到 Nacos，Manager 会在 30 秒内接住。无需重启。
      </p>
    </>
  );
}
