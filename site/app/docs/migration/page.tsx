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

      <h2 id="step-1">Step 1 · Export a snapshot from the source system</h2>
      <p>
        The self-service migration tool is <code>opskeeper-migrate</code> (
        <code>cmd/opskeeper-migrate</code>). It supports 9 entity types with idempotent writes,
        rollback, rate limiting, and tenant isolation.
      </p>
      <CodeBlock language="bash" title="export">
        {`go build -o opskeeper-migrate ./cmd/opskeeper-migrate

opskeeper-migrate export \\
  --source "$OPSKEEPER_OLD_URL" \\
  --output ./snapshot.json`}
      </CodeBlock>

      <h2 id="step-2">Step 2 · Import into OpsKeeper</h2>
      <CodeBlock language="bash" title="import">
        {`opskeeper-migrate import \\
  --source ./snapshot.json \\
  --target "$OPSKEEPER_NEW_URL" \\
  --tenant-mapping "$TENANT_MAP"`}
      </CodeBlock>

      <h2 id="step-3">Step 3 · Verify source vs target</h2>
      <p>
        The <code>verify</code> subcommand diffs the exported snapshot against the target per
        entity and reports drift (optionally as an HTML report). Expect zero mismatches before
        cutover.
      </p>
      <CodeBlock language="bash" title="verify">
        {`opskeeper-migrate verify \\
  --source ./snapshot.json \\
  --target "$OPSKEEPER_NEW_URL" \\
  --tenant-mapping "$TENANT_MAP" \\
  --report ./verify-report.html`}
      </CodeBlock>

      <h2 id="rollback">Rollback</h2>
      <p>
        Every import is recorded. Undo a migration with the rollback snapshot the import wrote:
      </p>
      <CodeBlock language="bash" title="rollback">
        {`opskeeper-migrate rollback \\
  --rollback-snapshot ./rollback-20260911.json \\
  --target "$OPSKEEPER_NEW_URL"`}
      </CodeBlock>
      <p>
        List the supported entity types any time with <code>opskeeper-migrate list-entities</code>.
      </p>

      <h2 id="help">Help</h2>
      <p>
        File an issue at <a href="https://github.com/louloulin/opskeeper/issues">github.com/louloulin/opskeeper/issues</a>{' '}
        with the <code>migration</code> label and the output of <code>opskeeper-migrate --help</code>.
      </p>
    </>
  );
}
