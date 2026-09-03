# Open-source gate: plugins-only release

**Status: READY FOR REVIEW**
**Gate date: 2026-09-03**
**Release:** OpsKeeper Plugins standalone initial release

This gate records the engineering admission review for publishing the AgentTeams
Plugin Installer and OpsKeeper TeamHarness. It is not legal advice.

## Scope

Only the following source scope is admitted:

- `plugins/agentteams-plugin-installer`
- `plugins/opskeeper-teamharness`

The OpsKeeper backend, AgentTeams core, AgentTeams Dashboard source, private
deployment topology, credentials, runtime evidence, and delivery material are
outside this release.

## Boundary audit

The public release is plugin-only and does not distribute backend, core
runtime, dashboard core, deployment topology, private evidence, or delivery
material. Acknowledgments describe design appreciation only and do not assert
code derivation, endorsement, or affiliation.

## AgentTeams boundary

This repository distributes plugins that integrate with GoAI AgentTeams and
AgentTeams Dashboard. It does not copy or redistribute their core source. The
plugin protocols and extension points are used through their published runtime
interfaces. Thanks are retained in `README.md` and `NOTICE.md`.

## Automated admission checks

The release must continue to pass:

1. `make verify`
2. no secret-shaped strings, task IDs, public demo endpoints, private host
   paths, or internal project-stage identifiers;
3. no OnGrid references outside the allowed acknowledgment and governance files;
4. package manifests, entry files, versions, licenses, notices, and archive
   safety checks;
5. all standalone plugin tests and the Installer self-check;
6. a release-only version record in `RELEASE_VERSION.json`.

The implementation of checks 2 and 3 is automated by
`scripts/audit_open_source.py`.
