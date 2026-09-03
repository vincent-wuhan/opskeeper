# OpsKeeper

OpsKeeper is an auditable operations platform for multi-agent incident response. It connects alert intake, evidence collection, root-cause analysis, human approval, narrowly authorized recovery, independent verification, and post-incident learning in one closed loop.

## What OpsKeeper provides

- **Agent coordination**: Manager-style routing and seven operational worker roles: alerter, investigator, critic, reviewer, repairer, verifier, and postmortem reporter.
- **AgentTeams plugin integration**: `agentteams-plugin-installer` manages Dashboard plugin installation; `opskeeper-teamharness` connects Workers to OpsKeeper MCP tools.
- **Safety boundary**: diagnosis is read-only by default. Mutating actions require a pending proposal, explicit human approval, exact target matching, command and payload hashes, and audit records.
- **Incident control plane**: durable incident timelines, recovery signals, proposal state, audit events, and replayable metrics.
- **Operations context**: PostgreSQL-backed incident memory, Qdrant vector retrieval, keyword recall, RRF ranking, and retained candidate-decision evidence.
- **Observability**: OpenTelemetry trace context, Prometheus metrics, Loki logs, Tempo traces, and Grafana dashboards.
- **Web console**: a React/Vite interface for incidents, workflows, approvals, audit, and runtime observation.

## Repository layout

| Path | Contents |
|---|---|
| `cmd/` | Go services and command-line tools |
| `internal/` | control plane, manager, MCP, incident, audit, and evaluation logic |
| `web/` | OpsKeeper web console |
| `plugins/agentteams-plugin-installer/` | AgentTeams Dashboard installer plugin |
| `plugins/opskeeper-teamharness/` | Worker/Manager integration plugin and MCP proxy |
| `deploy/` | deployment assets, synthetic incident examples, and runbooks |
| `docs/` | product, integration, deployment, and operations documentation |
| `testdata/` | deterministic end-to-end fixtures |

## Build and verify

Requirements:

- Go 1.25+
- Node.js 20+, npm 10+, and pnpm 9+
- Python 3.11+
- zip, tar, and standard POSIX shell tools

Backend:

```bash
go build ./...
go test ./... -count=1
make build
```

Web console:

```bash
cd web
pnpm install --frozen-lockfile
pnpm typecheck
pnpm test
pnpm run build
```

AgentTeams Plugin Installer:

```bash
make build-plugins
make test-plugins
make verify-plugins
```

OpsKeeper TeamHarness:

```bash
bash plugins/opskeeper-teamharness/scripts/build-package.sh
python3 -m unittest discover -s plugins/opskeeper-teamharness -p 'test_*.py'
```

Open-source admission audit:

```bash
python3 scripts/audit_open_source.py
```

## Synthetic incident examples

`deploy/incident-events/` contains reproducible PostgreSQL examples for connection-pool exhaustion, lock waits, disk I/O saturation, and replica replay lag. The data is synthetic and derived from common operations knowledge. It does not contain customer identifiers, production telemetry, credentials, private endpoints, or real incident evidence.

The examples use the neutral tenant `opskeeper-demo` and deterministic IDs so operators can exercise timelines, metrics, approval state, and audit behavior without a production system.

## Deploy and integrate

- Plugin installation: [`docs/guides/agentteams-plugin-installation.md`](docs/guides/agentteams-plugin-installation.md)
- Integration guide: [`docs/integration-guide.md`](docs/integration-guide.md)
- Operations manual: [`docs/operations-manual.md`](docs/operations-manual.md)
- Public roadmap: [`ROADMAP_PUBLIC.md`](ROADMAP_PUBLIC.md)

The root `docker-compose.yml` is intended for a local demo and integration environment. Production deployments should start from `deploy/install/` or `deploy/helm/`, supply TLS and credentials through a secret manager, and review all exposed ports before startup.

## Security model

OpsKeeper assumes agents can make mistakes and treats executable actions as high risk:

1. Diagnosis tools run read-only unless a policy explicitly grants more.
2. A mutating action must match one approved incident, manifest, resource, command, and payload hash.
3. Repairer and verifier responsibilities are separated.
4. Approval, rejection, execution, failure, recovery signal, and postmortem events are retained.
5. Unknown tools, shell/browser escape attempts, audit bypasses, and cross-resource targets fail closed.

Report security issues through the contact information in [`SECURITY.md`](SECURITY.md); do not open a public issue for an unresolved vulnerability.

## Release scope

The second-stage public release includes the Go backend, web console, AgentTeams plugins, deployment assets, public documentation, synthetic fixtures, and tests. Private delivery evidence, runtime credentials, internal project records, production topology, and non-public incident data are excluded. The machine-readable scope is recorded in [`RELEASE_VERSION.json`](RELEASE_VERSION.json).

## Acknowledgments

Thanks to GoAI AgentTeams and AgentTeams Dashboard for inspiration around multi-agent coordination, plugin extensibility, and observable interactions. Attribution, non-derivation, and non-endorsement boundaries are recorded in [`docs/ACKNOWLEDGMENTS.md`](docs/ACKNOWLEDGMENTS.md).

## License

OpsKeeper is distributed under Apache License 2.0. See [`LICENSE`](LICENSE) and [`NOTICE.md`](NOTICE.md). Brand assets are not licensed under Apache-2.0; see [`TRADEMARK.md`](TRADEMARK.md).
