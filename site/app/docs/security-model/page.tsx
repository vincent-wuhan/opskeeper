import { CodeBlock } from '@/components/code-block';

export const metadata = { title: 'Security model' };

export default function SecurityModelPage() {
  return (
    <>
      <header>
        <div className="text-xs font-medium uppercase tracking-wider text-accent-300">
          Operate
        </div>
        <h1>Security model</h1>
        <p>
          OpsKeeper&apos;s safety guarantees are properties of the orchestrator, not the LLM. This
          page is the canonical statement; the higher-level overview lives on the{' '}
          <a href="/security">marketing Security page</a>.
        </p>
      </header>

      <h2 id="trust-boundary">Trust boundary</h2>
      <p>
        The trust boundary runs around the OpsKeeper control plane. Workers, webhooks, and
        plugin endpoints sit outside it. Every crossing is authenticated and audited.
      </p>
      <ul>
        <li><strong>Inbound</strong>: HTTP API (Bearer JWT), MCP plugin (Bearer + HMAC), webhooks (HMAC-signed).</li>
        <li><strong>Outbound</strong>: read-only by default. Mutating calls require a sealed proposal.</li>
      </ul>

      <h2 id="safety-levels">Safety levels</h2>
      <p>
        Every worker declares a <code>safety_level</code>:
      </p>
      <ul>
        <li><strong>L0</strong> — read-only. No proposal required. Examples: alerter, investigator, critic, reviewer, verifier.</li>
        <li><strong>L1</strong> — annotates state. No external side effects. Examples: reporter (writes reports, never touches infra).</li>
        <li><strong>L2</strong> — proposes; never executes. Used by planning workers.</li>
        <li><strong>L3</strong> — mutates with proposal + human approver. The only L3 worker today is <code>repairer</code>.</li>
      </ul>
      <p>
        The manager refuses to assign a worker whose level exceeds what the phase allows.
      </p>

      <h2 id="proposal-contract">Proposal contract</h2>
      <p>
        A proposal is a contract. The control plane dispatches a recovery only when:
      </p>
      <ol>
        <li>Resource, command, and payload hash match the approval exactly.</li>
        <li>The tool is in the worker&apos;s <code>tool_allowlist</code>.</li>
        <li>A human approver signature is present on the proposal.</li>
        <li>The proposal is not expired (default TTL: 30 minutes).</li>
      </ol>
      <p>
        Otherwise the call fails closed and an audit event is appended.
      </p>
      <CodeBlock language="yaml" title="loop_contract.yaml">
        {`phase: approved
proposal:
  incident_id: INC-PG-POOL-001
  worker: repairer
  blast_radius: pg.connection_pool / one-db
  ttl: 30m
guard:
  required_approval: human
  exact_match: [resource, command, payload_hash]
  tool_allowlist:
    - execute:approved-only
audit:
  on_dispatch: ledger.append
  on_complete: ledger.seal`}
      </CodeBlock>

      <h2 id="audit-ledger">Audit ledger</h2>
      <p>
        Every dispatch and completion appends to <code>loop_event_log</code>, which the database
        enforces as append-only (a trigger rejects UPDATE/DELETE; corrections are new events).
        Separately, every mutating-proposal transition appends to <code>chat_proposal_audit</code>,
        a SHA256 hash chain:
      </p>
      <CodeBlock language="text" title="hash chaining">
        {`hash_n = SHA256(prev_hash || canonical_json(payload) || proposal_id || action)`}
      </CodeBlock>
      <p>
        The in-repo verifier walks the chain in <code>created_at</code> order, recomputes each
        hash, and reports the first tampered entry. External anchoring of a daily root hash
        (transparency log) is a roadmap item, not a shipped feature.
      </p>

      <h2 id="threat-model">Threat model</h2>
      <p>
        The mitigations below are part of the manager, not the prompt. They hold even if a
        worker is fully compromised.
      </p>
      <table>
        <thead>
          <tr>
            <th>Threat</th>
            <th>Mitigation</th>
          </tr>
        </thead>
        <tbody>
          <tr>
            <td>Tool injection</td>
            <td>Explicit per-skill <code>tool_allowlist</code></td>
          </tr>
          <tr>
            <td>Role escape</td>
            <td>Phase → role binding in the manager dispatch table</td>
          </tr>
          <tr>
            <td>Blast-radius bypass</td>
            <td>Exact match on resource, command, payload hash</td>
          </tr>
          <tr>
            <td>Replan loop</td>
            <td>max_turns budgets + verified-only rule on recovery.verify</td>
          </tr>
        </tbody>
      </table>

      <h2 id="disclosure">Vulnerability disclosure</h2>
      <p>
        See <code>SECURITY.md</code> in the repo for the disclosure timeline and contact
        information.
      </p>
    </>
  );
}
