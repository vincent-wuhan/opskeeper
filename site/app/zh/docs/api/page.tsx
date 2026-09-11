import { CodeBlock } from '@/components/code-block';

export const metadata = { title: 'API 参考' };

export default function ApiZhPage() {
  return (
    <>
      <header>
        <div className="text-xs font-medium uppercase tracking-wider text-accent-300">
          扩展
        </div>
        <h1>API 参考</h1>
        <p>
          OpsKeeper 的 HTTP API 是控制平面的规范接口。<code>opskeeper-teamharness</code> 中的 MCP 工具与下面的端点一一对应。
        </p>
      </header>

      <h2 id="base">基础 URL 与鉴权</h2>
      <p>
        所有端点都在 <code>/api/v1</code> 下。鉴权方式：<code>Authorization: Bearer &lt;jwt&gt;</code>。JWT 由{' '}
        <code>POST /api/v1/auth/login</code> 颁发，通过 <code>POST /api/v1/auth/refresh</code> 刷新。
      </p>

      <h2 id="incidents">事件</h2>
      <CodeBlock language="http" title="incidents">
        {`GET    /api/v1/incidents
GET    /api/v1/incidents/:id
GET    /api/v1/incidents/:id/timeline
GET    /api/v1/incidents/:id/evidence
GET    /api/v1/incidents/:id/proposals
GET    /api/v1/incidents/:id/audit`}
      </CodeBlock>

      <h2 id="proposals">提案</h2>
      <CodeBlock language="http" title="proposals">
        {`POST   /api/v1/proposals
GET    /api/v1/proposals/:id
POST   /api/v1/proposals/:id/approve
POST   /api/v1/proposals/:id/reject
GET    /api/v1/proposals/pending`}
      </CodeBlock>
      <p>
        审批一份提案会把审批绑定到该提案的资源、命令、payload 哈希。控制平面会拒绝派发与已被篡改的提案。
      </p>

      <h2 id="skills">技能</h2>
      <CodeBlock language="http" title="skills">
        {`GET    /api/v1/skills
GET    /api/v1/skills/:name
POST   /api/v1/skills/:name/deploy
DELETE /api/v1/skills/:name
POST   /api/v1/skills/:name/rotate-secret`}
      </CodeBlock>

      <h2 id="audit">审计</h2>
      <CodeBlock language="http" title="audit">
        {`GET    /api/v1/audit?from=&to=&incident_id=&limit=
POST   /api/v1/audit/replay    # 端到端走完整条 HMAC 链
GET    /api/v1/audit/events/:id`}
      </CodeBlock>

      <h2 id="plugins">插件</h2>
      <CodeBlock language="http" title="plugins">
        {`GET    /v1/plugins
GET    /v1/plugins/:id
POST   /v1/plugins/:id/install
POST   /v1/plugins/:id/uninstall
POST   /v1/plugins/:id/enable
POST   /v1/plugins/:id/disable
POST   /v1/plugins/:id/sync
POST   /v1/plugins/:id/push`}
      </CodeBlock>

      <h2 id="webhooks">Webhook</h2>
      <CodeBlock language="http" title="webhooks">
        {`POST   /api/v1/webhook/alerts   # 由源端用 HMAC 签名
POST   /api/v1/webhook/git        # 用于变更事件关联`}
      </CodeBlock>

      <h2 id="errors">错误</h2>
      <p>
        所有错误都返回带 <code>code</code>、<code>message</code>，以及可选 <code>details</code> 的 JSON：
      </p>
      <CodeBlock language="json" title="error">
        {`{
  "code": "proposal_hash_mismatch",
  "message": "资源、命令或 payload 哈希与已审批提案不一致",
  "details": {
    "proposal_id": "prop-...",
    "expected_payload_hash": "sha256:...",
    "actual_payload_hash": "sha256:..."
  }
}`}
      </CodeBlock>

      <h2 id="middleware">中间件</h2>
      <p>
        中间件层文档在仓库的 <code>docs/api/middleware.md</code>。涵盖限流、请求签名，以及在每次变更类调用上发账本事件的审计中间件。
      </p>
    </>
  );
}
