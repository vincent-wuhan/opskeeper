# OPC Squad Architecture Guardrails

These rules apply to every change in this repository. They encode the agreed
Pient/OPC/plugin topology. If a change needs an exception, record the reason and
the replacement control in `docs/ARCHITECTURE_GUARDRAILS.md` before review.

## Non-negotiable topology

1. **OPC is the control plane.** It owns projects, tasks, membership, plugin
   catalog metadata, compatibility policy, rollout, and audit. It does not load
   or execute Pi plugin implementation code in its web/server runtime.
2. **Pient is the runtime base.** It owns the Android runtime, official Pi
   process lifecycle, `pi --mode rpc`, package installation, permissions, secret
   injection, diagnostics, and rollback. Vertical business logic stays out of
   Pient.
3. **UpUp and QingNexus are independent Pi plugin packages.** Develop them under
   `plugins/pi/<plugin-id>/`, with their own package manifest, version, tests,
   license, and release history. Do not compile them into OPC server images and
   do not fork Pient for a vertical.
4. **One runtime process model.** A vertical plugin executes inside the existing
   Pi turn. It must not start a resident daemon, browser, UpUp Bun runtime,
   QingNexus server, worker pool, or a second agent.
5. **No floating installation sources.** OPC metadata references an exact
   plugin package version. `latest`, bare tags, and mutable branches are not
   release references.

## Required plugin structure

Every Pi vertical plugin directory must contain:

- `package.json` with name, exact version, license, test/typecheck scripts, and
  a Pi runtime peer dependency.
- `pient-plugin.json` describing capabilities, Pient/Pi compatibility,
  network hosts, secrets, filesystem scope, process model, and install policy.
- `LICENSE` and attribution notices.
- README installation, capability, credential, rollback, and troubleshooting
  documentation.
- Fixture-based tests that do not call live providers.

Run `make pi-plugin-guard` before requesting review. CI runs the same gate.

## Review checklist

- Confirm the change preserves OPC control-plane / Pient runtime / vertical
  plugin boundaries.
- Confirm plugin metadata is authoritative for capabilities, permissions, and
  compatibility.
- Confirm secrets are environment-scoped, never persisted, and never included in
  logs, errors, transcripts, or task results.
- Confirm QingNexus medical scenarios remain opt-in, synthetic-first, audited,
  deidentified, and physician-confirmation-gated.
- Confirm UpUp remains mobile-safe and does not claim investment advice.
- Confirm task execution returns to OPC as status/result/audit events rather
  than requiring OPC to import plugin code.
