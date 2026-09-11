import { CodeBlock } from '@/components/code-block';

export const metadata = { title: 'Migration' };

export default function MigrationPage() {
  return (
    <>
      <header>
        <div className="text-xs font-medium uppercase tracking-wider text-accent-300">
          Run as a team
        </div>
        <h1>Migration</h1>
        <p>
          Migrate to current OpsKeeper from earlier OnGrid-lineage installations. The
          authoritative guide is <code>docs/migration/opskeeper-user-transition.md</code>.
        </p>
      </header>

      <h2 id="before-you-start">Before you start</h2>
      <ul>
        <li>Read <code>docs/PROVENANCE.md</code> for the compatibility map.</li>
        <li>Read <code>docs/BRAND_GOVERNANCE.md</code> for the naming policy.</li>
        <li>Back up your existing PostgreSQL database before running any migration.</li>
      </ul>

      <h2 id="step-1">Step 1 · Run the schema migration</h2>
      <CodeBlock language="bash" title="schema">
        {`opskeeper-migrate --from ongrid-lineage-1.x \\
  --target opskeeper-v2026.09.03 \\
  --dsn "$POSTGRES_DSN"`}
      </CodeBlock>

      <h2 id="step-2">Step 2 · Translate skill metadata</h2>
      <p>
        Older <code>skill_manifest.json</code> files are translated to the new{' '}
        <code>skill_meta.yaml</code> schema in dry-run mode. Inspect, then apply:
      </p>
      <CodeBlock language="bash" title="skill migration">
        {`# Dry-run
opskeeper migrate skills --dry-run --in ./skills-legacy

# Apply
opskeeper migrate skills --in ./skills-legacy --out ./skills-v2`}
      </CodeBlock>

      <h2 id="step-3">Step 3 · Re-issue HMAC secrets</h2>
      <p>
        The HMAC chain root is regenerated under the new key. Old keys are honored for 24 hours
        via the <code>--hmac-rotate-grace</code> flag.
      </p>
      <CodeBlock language="bash" title="hmac">
        {`opskeeper audit rekey \\
  --new-secret "$OPSKEEPER_NEW_HMAC" \\
  --grace 24h`}
      </CodeBlock>

      <h2 id="step-4">Step 4 · Verify</h2>
      <p>
        Walk the new ledger end-to-end. The replay should succeed with no break.
      </p>
      <CodeBlock language="bash" title="verify">
        {`opskeeper audit replay --from GENESIS --to now`}
      </CodeBlock>

      <h2 id="rollback">Rollback</h2>
      <p>
        The migration tool records every step in <code>migration_log</code>. Use{' '}
        <code>opskeeper migrate rollback</code> with the step ID to undo a single step. A full
        rollback is possible if the schema has not been promoted past <code>v2026.09.03</code>.
      </p>

      <h2 id="help">Help</h2>
      <p>
        File an issue at <a href="https://github.com/louloulin/opskeeper/issues">github.com/louloulin/opskeeper/issues</a>{' '}
        with the <code>migration</code> label and the output of <code>opskeeper migrate
        doctor</code>.
      </p>
    </>
  );
}
