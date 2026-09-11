import Link from 'next/link';
import { CodeBlock } from '@/components/code-block';
import { ArrowRight } from 'lucide-react';

export const metadata = {
  title: 'Documentation',
  description: 'Introduction to OpsKeeper: the closed loop, the workers, and the safety boundary.',
};

export default function DocsIntroPage() {
  return (
    <>
      <header>
        <div className="text-xs font-medium uppercase tracking-wider text-accent-300">
          Introduction
        </div>
        <h1>Welcome to OpsKeeper</h1>
        <p>
          OpsKeeper is the auditable operations platform for multi-agent incident response.
          These docs walk through how to run it, how to extend it, and how the closed loop keeps
          mutating actions under human control.
        </p>
        <div className="not-prose mt-6 flex flex-wrap gap-3">
          <Link
            href="/docs/getting-started"
            className="inline-flex items-center gap-2 rounded-md bg-white px-3 py-1.5 text-sm font-medium text-ink-950 hover:bg-ink-100"
          >
            Get started <ArrowRight className="h-4 w-4" />
          </Link>
          <Link
            href="/docs/architecture"
            className="inline-flex items-center gap-2 rounded-md border border-white/10 bg-white/5 px-3 py-1.5 text-sm text-ink-100 hover:bg-white/10"
          >
            Read the architecture
          </Link>
        </div>
      </header>

      <h2 id="what-is-opskeeper">What OpsKeeper is</h2>
      <p>
        OpsKeeper connects <strong>alert intake</strong>,{' '}
        <strong>evidence collection</strong>, <strong>root-cause analysis</strong>,{' '}
        <strong>human approval</strong>, <strong>narrowly authorized recovery</strong>,{' '}
        <strong>independent verification</strong>, and <strong>post-incident learning</strong> in
        one closed loop. Each phase is an explicit state transition with a guard. Each
        transition is an append-only ledger event.
      </p>

      <h2 id="who-it-is-for">Who it is for</h2>
      <ul>
        <li><strong>SRE / DevOps teams</strong> who want agent-driven incident response with a verifiable audit trail.</li>
        <li><strong>Platform teams</strong> building an internal incident response product on top of an open source core.</li>
        <li><strong>Security teams</strong> who need mutating actions to be authorized, scoped, and replayable.</li>
      </ul>

      <h2 id="how-to-read-these-docs">How to read these docs</h2>
      <p>
        If you are evaluating OpsKeeper, start with <Link href="/docs/getting-started">Getting started</Link>{' '}
        and the <Link href="/docs/architecture">Architecture</Link> page. If you are operating it
        in production, the <Link href="/docs/operations">Operations manual</Link> is the day-2 reference.
        If you are extending it, head straight to <Link href="/docs/plugins">Plugins</Link>.
      </p>

      <h2 id="the-closed-loop-in-one-snippet">The closed loop in one snippet</h2>
      <p>
        Every incident runs through the same eight phases. The control plane refuses to advance
        a phase until its guard is satisfied:
      </p>
      <CodeBlock language="text" title="closed-loop phases">
        {`detected → correlated → investigated → critiqued
     → approved → recovered → verified → postmortem`}
      </CodeBlock>

      <h2 id="principles">Principles</h2>
      <ol>
        <li><strong>Diagnosis reads.</strong> Read-only tools are always available.</li>
        <li><strong>Recovery writes.</strong> Mutating tools require a proposal.</li>
        <li><strong>A human approves.</strong> Approval is bound to a specific resource and payload hash.</li>
        <li><strong>An independent worker verifies.</strong> The actor is not the judge.</li>
        <li><strong>Every transition is durable.</strong> loop_event_log is append-only at the DB layer; mutating proposals are chained with SHA256.</li>
      </ol>

      <h2 id="next">Next</h2>
      <p>
        Continue with <Link href="/docs/getting-started">Getting started</Link> to run the closed loop
        locally, or jump to <Link href="/docs/architecture">Architecture</Link> for the data plane.
      </p>
    </>
  );
}
