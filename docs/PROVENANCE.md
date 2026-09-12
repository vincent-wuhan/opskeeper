# OpsKeeper provenance and evolution boundary

OpsKeeper is the public product name for this repository. The Go control plane, web console, plugin packages, public documentation, synthetic fixtures, and tests in this release are maintained as one Apache-2.0 project.

## Attribution boundary

The project thanks GoAI AgentTeams and AgentTeams Dashboard for ideas around multi-agent coordination, extension points, and observable interactions. It also thanks OnGrid for early inspiration around operational workflows and auditable execution. These are conceptual acknowledgments; they are not claims of code derivation, ownership, management, sponsorship, or endorsement.

This repository does not redistribute AgentTeams core source or the AgentTeams Dashboard core. The plugins use their runtime extension interfaces and communicate over documented plugin, HTTP, MCP, and audit contracts.

## Compatibility identifiers

Some historical module paths, command names, image labels, environment variables, database keys, and package names remain when renaming would break upgrades. Such identifiers are engineering compatibility names, not brand claims. Contributors must preserve license notices, provide an upgrade path, document rollback behavior, and record public compatibility mappings when introducing a rename.

## Release provenance

Every public release records its source scope in `RELEASE_VERSION.json` and identifies the immutable release tag in the release package. Private delivery evidence, production topology, credentials, and non-public incident records are not release provenance and must not be distributed from this repository.

## Release v2026.09.13-rc1 provenance

`RELEASE_VERSION.json` binds the release candidate to backend source commit `d659b022bcc6f5d286050fdcc3fd216504779033`, TeamHarness `1.0.42`, installer plugin `1.4.2`, and web Git tree `fdf7d7332162fe240c608087d63eb1b4ac9b0e4a`. The tagged release commit may add only release metadata, documentation, or release verification tooling and must descend from that backend source commit.
