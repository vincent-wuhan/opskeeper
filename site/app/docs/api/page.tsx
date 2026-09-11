import { CodeBlock } from '@/components/code-block';

export const metadata = { title: 'API reference' };

export default function ApiPage() {
  return (
    <>
      <header>
        <div className="text-xs font-medium uppercase tracking-wider text-accent-300">
          Build
        </div>
        <h1>API reference</h1>
        <p>
          The OpsKeeper HTTP API is the canonical interface for the control plane. The MCP
          tools in <code>opskeeper-teamharness</code> map one-to-one onto the endpoints below.
        </p>
      </header>

      <h2 id="base">Base URL and authentication</h2>
      <p>
        All endpoints live under <code>/api/v1</code>. Authentication is{' '}
        <code>Authorization: Bearer &lt;jwt&gt;</code>. The JWT is issued by{' '}
        <code>POST /api/v1/auth/login</code> and refreshed via{' '}
        <code>POST /api/v1/auth/refresh</code>.
      </p>

      <h2 id="incidents">Incidents</h2>
      <CodeBlock language="http" title="incidents">
        {`GET    /api/v1/incidents
GET    /api/v1/incidents/:id
GET    /api/v1/incidents/:id/timeline
GET    /api/v1/incidents/:id/evidence
GET    /api/v1/incidents/:id/proposals
GET    /api/v1/incidents/:id/audit`}
      </CodeBlock>

      <h2 id="proposals">Proposals</h2>
      <CodeBlock language="http" title="proposals">
        {`POST   /api/v1/proposals
GET    /api/v1/proposals/:id
POST   /api/v1/proposals/:id/approve
POST   /api/v1/proposals/:id/reject
GET    /api/v1/proposals/pending`}
      </CodeBlock>
      <p>
        Approving a proposal binds the approval to the proposal&apos;s resource, command, and
        payload hash. The control plane will refuse to dispatch a recovery against a mutated
        proposal.
      </p>

      <h2 id="skills">Skills</h2>
      <CodeBlock language="http" title="skills">
        {`GET    /api/v1/skills
GET    /api/v1/skills/:name
POST   /api/v1/skills/:name/deploy
DELETE /api/v1/skills/:name
POST   /api/v1/skills/:name/rotate-secret`}
      </CodeBlock>

      <h2 id="audit">Audit</h2>
      <CodeBlock language="http" title="audit">
        {`GET    /api/v1/audit?from=&to=&incident_id=&limit=
POST   /api/v1/audit/replay    # walks the HMAC chain end-to-end
GET    /api/v1/audit/events/:id`}
      </CodeBlock>

      <h2 id="plugins">Plugins</h2>
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

      <h2 id="webhooks">Webhooks</h2>
      <CodeBlock language="http" title="webhooks">
        {`POST   /api/v1/webhook/alerts   # HMAC-signed by the source
POST   /api/v1/webhook/git        # for change-event correlation`}
      </CodeBlock>

      <h2 id="errors">Errors</h2>
      <p>
        All errors return a JSON body with <code>code</code>, <code>message</code>, and
        optionally <code>details</code>:
      </p>
      <CodeBlock language="json" title="error">
        {`{
  "code": "proposal_hash_mismatch",
  "message": "resource, command, or payload hash does not match the approved proposal",
  "details": {
    "proposal_id": "prop-...",
    "expected_payload_hash": "sha256:...",
    "actual_payload_hash": "sha256:..."
  }
}`}
      </CodeBlock>

      <h2 id="middleware">Middleware</h2>
      <p>
        The middleware layer is documented at <code>docs/api/middleware.md</code> in the repo.
        It covers rate limiting, request signing, and the audit middleware that emits a
        ledger event on every state-changing call.
      </p>
    </>
  );
}
