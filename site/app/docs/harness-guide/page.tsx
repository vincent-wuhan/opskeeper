import { CodeBlock } from '@/components/code-block';

export const metadata = { title: 'Harness guide' };

export default function HarnessGuidePage() {
  return (
    <>
      <header>
        <div className="text-xs font-medium uppercase tracking-wider text-accent-300">
          Run as a team
        </div>
        <h1>Harness guide</h1>
        <p>
          The verification harness is how OpsKeeper proves that a change did not regress the
          closed loop. It runs three scenarios against a real Postgres + Qdrant + control plane
          stack: <code>alert_storm</code>, <code>rca_loop</code>, and <code>recovery_verify</code>.
        </p>
      </header>

      <h2 id="running">Running the harness</h2>
      <CodeBlock language="bash" title="harness">
        {`# Bring up the harness stack
make harness-up

# Run all scenarios
make harness

# Run a single scenario
make harness SCENARIO=rca_loop

# Tear down
make harness-down`}
      </CodeBlock>

      <h2 id="scenarios">Scenarios</h2>

      <h3 id="alert-storm">alert_storm</h3>
      <p>
        Floods the alerter with 10,000 synthetic alerts over 60 seconds and asserts that:
      </p>
      <ul>
        <li>The dedup circuit breaker never trips more than twice.</li>
        <li>No more than 50 distinct incidents are created.</li>
        <li>The critic is invoked on the right severity tier.</li>
      </ul>

      <h3 id="rca-loop">rca_loop</h3>
      <p>
        Replays <code>pg-connection-pool-exhaustion</code> end-to-end and asserts that:
      </p>
      <ul>
        <li>The loop reaches <code>verified</code> within 90 seconds.</li>
        <li>The investigator returns evidence from at least 3 sources.</li>
        <li>The proposal carries a valid payload hash and blast radius.</li>
      </ul>

      <h3 id="recovery-verify">recovery_verify</h3>
      <p>
        Mutates a synthetic target via an approved proposal and asserts that:
      </p>
      <ul>
        <li>The verifier returns a <code>VerifiedDelta</code> matching the allowlist.</li>
        <li>A proposal with a mutated payload hash is rejected with{' '}
        <code>proposal_hash_mismatch</code>.</li>
        <li>The audit ledger is HMAC-valid after the run.</li>
      </ul>

      <h2 id="assertions">Writing a new assertion</h2>
      <p>
        Assertions live under <code>tests/harness/assert/</code>. Each is a Go test that
        receives the live control plane handle and a recorded run.
      </p>
      <CodeBlock language="go" title="assertion_test.go">
        {`package assert

import (
  "testing"
  "github.com/louloulin/opskeeper/harness"
)

func TestRecoveryAppliesOnlyApprovedPayloadHash(t *testing.T) {
  run := harness.LoadRun("recovery_verify")
  for _, evt := range run.Audit {
    if evt.Action == "recovery.dispatch" {
      if evt.PayloadHash != run.Proposal.PayloadHash {
        t.Fatalf("dispatch payload hash %s != approved %s",
          evt.PayloadHash, run.Proposal.PayloadHash)
      }
    }
  }
}`}
      </CodeBlock>

      <h2 id="ci">CI</h2>
      <p>
        The harness runs in CI on every PR. A red harness is a release blocker.
      </p>
    </>
  );
}
