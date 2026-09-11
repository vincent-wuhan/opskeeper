import { CodeBlock } from '@/components/code-block';

export const metadata = { title: 'Deployment' };

export default function DeploymentPage() {
  return (
    <>
      <header>
        <div className="text-xs font-medium uppercase tracking-wider text-accent-300">
          Operate
        </div>
        <h1>Deployment</h1>
        <p>
          OpsKeeper supports standalone, high-availability, and Kubernetes deployments. This
          page covers the canonical layouts; detailed manifests live under{' '}
          <code>deploy/</code> in the repo.
        </p>
      </header>

      <h2 id="standalone">Standalone</h2>
      <p>
        Single-host install for development or small teams. One binary, one PostgreSQL, one
        Qdrant instance.
      </p>
      <CodeBlock language="bash" title="standalone">
        {`# 1. Configure
cp .env.example .env
$EDITOR .env   # set POSTGRES_DSN, QDRANT_URL, OPSKEEPER_PLUGIN_HMAC

# 2. Boot
docker compose up -d

# 3. Verify
opskeeper health
opskeeper incident list --limit 5`}
      </CodeBlock>

      <h2 id="ha">High availability</h2>
      <p>
        Run multiple OpsKeeper control plane instances behind a TCP load balancer. The
        orchestrator serializes per-incident via MySQL <code>GET_LOCK</code> advisory locks,
        so the leader is implicit, not elected.
      </p>
      <CodeBlock language="text" title="ha topology">
        {`              ┌──────────────┐
              │  client / web │
              └──────┬───────┘
                     ▼
            ┌──────────────────┐
            │ TCP load balancer │
            └──────┬───────────┘
                   ▼
   ┌───────────┬──┴──┬───────────┐
   ▼           ▼     ▼           ▼
opskeeper-1 opskeeper-2 opskeeper-3 opskeeper-N
   └───────────┴──┬──┴───────────┘
                  ▼
        ┌──────────────────────┐
        │  PostgreSQL primary  │
        │   + read replicas    │
        └──────────────────────┘
                  ▼
        ┌──────────────────────┐
        │  Qdrant cluster      │
        └──────────────────────┘`}
      </CodeBlock>
      <p>
        See <code>docs/deployment/ha.md</code> for the canonical Compose and Kubernetes
        manifests, plus backup/restore runbooks.
      </p>

      <h2 id="kubernetes">Kubernetes</h2>
      <p>
        A Kubernetes Operator is on the Q1 2027 roadmap. Today, the supported approach is the
        Helm chart under <code>deploy/k8s/</code>:
      </p>
      <CodeBlock language="bash" title="kubernetes">
        {`helm upgrade --install opskeeper deploy/k8s/charts/opskeeper \\
  --namespace opskeeper --create-namespace \\
  --set persistence.postgres.size=200Gi \\
  --set persistence.qdrant.size=100Gi`}
      </CodeBlock>

      <h2 id="edge">Edge install</h2>
      <p>
        The <code>opskeeper-edge</code> binary runs the worker side at the edge (factory floor,
        retail POS, regional POP). It forwards evidence to a central control plane over the
        plugin stdio MCP channel.
      </p>
      <CodeBlock language="bash" title="edge">
        {`# Provision a new edge
opskeeper-edge provision \\
  --endpoint https://opskeeper.internal.example.com \\
  --name factory-floor-7

# Run the edge daemon
opskeeper-edge serve --config /etc/opskeeper/edge.yaml`}
      </CodeBlock>

      <h2 id="upgrade">Upgrade</h2>
      <p>
        Follow the upgrade runbook for your layout. The general pattern is:
      </p>
      <ol>
        <li>Drain active incidents to <code>verified</code> or <code>postmortem</code>.</li>
        <li>Apply the new image / binary.</li>
        <li>Roll the control plane one instance at a time.</li>
        <li>Replay the most recent ledger events to confirm no event was lost.</li>
      </ol>

      <h2 id="external-deps">External dependencies</h2>
      <p>
        See <code>docs/deployment/external-deps.md</code> for the full table of required and
        optional services, including minimum versions and the failure modes if each is
        unavailable.
      </p>
    </>
  );
}
