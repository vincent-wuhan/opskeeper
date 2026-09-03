# Acknowledgments and distribution boundary

## Release boundary

This repository is a standalone, plugin-only release. It contains the AgentTeams
Plugin Installer and OpsKeeper TeamHarness; it does not contain the OpsKeeper
backend, AgentTeams core, private deployment topology, or delivery material.

The release is distributed under Apache License 2.0. `RELEASE_VERSION.json` is
the machine-readable release record for auditing.

## Acknowledgments

OpsKeeper's authors thank GoAI AgentTeams and AgentTeams Dashboard for
inspiration around multi-agent coordination, plugin extensions, and observable
interactions, and OnGrid for early help with operations scenarios and audited
execution ideas. The OpsKeeper-specific plugin release provides:

- Dashboard plugin lifecycle management and Worker distribution;
- QwenPaw/AgentTeams TeamHarness integration;
- MCP proxying, signed backend requests, and trace propagation;
- fail-closed read-only tool boundaries;
- seven operational worker roles and Manager coordination rules;
- safety levels, approval boundaries, and audit ledgers.

This acknowledgment is not a claim that OnGrid maintains, endorses, operates, or
owns OpsKeeper.

## Distribution policy

The repository and generated source packages retain `LICENSE` and `NOTICE.md`.
Compatibility identifiers, historical names, and package internals must not be
used to imply an official relationship with another project.
