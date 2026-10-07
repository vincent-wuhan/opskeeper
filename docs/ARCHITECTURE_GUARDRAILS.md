# Pient / OPC / Vertical Plugin Architecture Guardrails

Status: **active architecture decision**

## Decision

OpsKeeper/OPC is the control plane, Pient is the mobile runtime base, and
vertical products are independently versioned Pi plugins. This repository may
host plugin source packages, but it must not turn those packages into server
runtime dependencies.

```text
OPC / Multica
  projects, tasks, membership, plugin catalog, compatibility, rollout, audit
        |
Pient Android base
  official Pi runtime, PRoot/Node environment, RPC adapter, permissions,
  plugin install/update/remove, lifecycle, diagnostics, rollback
        |
Pi vertical plugins
  UpUp investment research, QingNexus scenario/MDT capabilities
```

## Responsibility matrix

| Concern | OPC | Pient | Vertical plugin |
|---|---|---|---|
| Projects and task assignment | Owns | Reports state | Executes capability |
| Plugin catalog metadata | Owns | Consumes | Declares |
| Compatibility matrix | Owns policy | Enforces install | Declares ranges |
| Pi process and RPC | Observes | Owns | Uses Pi APIs only |
| Android runtime environment | Observes | Owns | Does not assume |
| Plugin install/update/remove | Policies | Executes | Supports rollback |
| Domain tool/skill logic | Does not import | Does not contain | Owns |
| Network allowlist | Displays/policies | Enforces | Declares |
| Secret handling | Never sees plaintext | Injects environment | Reads only at execution |
| Audit events | Stores | Forwards | Emits bounded metadata |
| Heavy domain services | May integrate APIs | Does not host | Optional external service |

## Plugin workspace

New Pi vertical plugins use:

```text
plugins/pi/<plugin-id>/
  package.json
  pient-plugin.json
  LICENSE
  README.md
  extensions/
  skills/
  src/
  tests/
```

`plugins/pi/README.md` is the workspace contract. A directory is treated as a
Pi vertical plugin when it contains `package.json` or `pient-plugin.json`.

## Required `pient-plugin.json` model

```json
{
  "schemaVersion": 1,
  "id": "upup",
  "package": "npm:@example/upup-pi-plugin@0.1.0",
  "runtime": {
    "pient": ">=0.2.0",
    "pi": "0.85.x"
  },
  "capabilities": ["investment.research"],
  "permissions": {
    "network": ["api.example.com"],
    "secrets": ["UPUP_API_TOKEN"],
    "filesystem": "plugin-sandbox-only"
  },
  "process": {
    "daemon": false,
    "background": "pi-turn-only"
  },
  "install": {
    "mode": "user-initiated",
    "bundled": false
  }
}
```

Rules:

- `package` must be exact `npm:<package-name>@<exact-semver>`.
- `id`, capabilities, and network hosts must be stable machine identifiers.
- Network is HTTPS-only and every literal production URL host must be declared.
- Filesystem scope is exactly `plugin-sandbox-only`.
- Process model is exactly no daemon and `pi-turn-only`.
- Install is user-initiated and not bundled by default.
- A plugin must carry its own license and must not copy Pient GPL code. Plugins
  that need closed commercial licensing must remain independently publishable.

## Product-specific constraints

### UpUp

Phase-one scope is bounded, read-only investment research: snapshots, news,
financial summaries, deterministic screening, valuation, and research workflow.
It must not import or launch the full UpUp Bun agent, browser stack, daemon,
portfolio accounting, or realtime monitoring. Results are observations, not
investment advice.

### QingNexus

Start with scenario-pack contracts, validation, explanation tools, and workflow
guidance. Do not migrate the QingNexus web/database/Docker stack into this
repository. Medical/MDT capabilities are disabled by default and require:

- explicit workspace/user opt-in;
- synthetic-first data policy;
- deidentification;
- visible uncertainty and conflicts;
- mandatory physician confirmation;
- append-only audit;
- no autonomous clinical decision.

## Enforcement

Run:

```bash
make pi-plugin-guard
```

The gate enforces structure, manifest shape, exact package identity, plugin
isolation, production source network declarations, forbidden daemon/browser
dependencies, and the OPC control-plane no-import rule. CI runs it on every push
and pull request.

## Exception process

An exception must be a written architecture decision that identifies:

1. the constraint being relaxed;
2. why the topology cannot satisfy the requirement;
3. the compensating security, compatibility, rollback, and audit controls;
4. owner and expiry/review date.

Do not bypass the gate by renaming files or placing plugin code outside
`plugins/pi`. Extend the guard when a legitimate new package shape is added.
