# OpsKeeper brand governance

OpsKeeper uses a three-layer naming policy:

1. **Product layer**: public product names, documentation, UI, demos, packages, and release assets use OpsKeeper.
2. **Compliance layer**: attribution, trademark, non-endorsement, and license facts remain in `NOTICE.md`, `TRADEMARK.md`, and `docs/ACKNOWLEDGMENTS.md`.
3. **Evolution layer**: compatibility identifiers and migration mappings are documented without becoming public brand claims.

## Release gates

Before publishing source, packages, documentation, or hosted demos:

1. The product identity is OpsKeeper.
2. OnGrid appears only in approved legal and acknowledgment context, never as the current product name.
3. No asset implies operation, sponsorship, endorsement, or affiliation by AgentTeams, AgentTeams Dashboard, or OnGrid.
4. Historical compatibility names are documented and retain an upgrade and rollback path.
5. `scripts/audit_open_source.py`, build checks, and release checks pass.

When a naming issue is found, classify it by user perception. If users could infer the wrong product or affiliation, fix it before release. If it is a required legal fact, preserve it in the compliance layer. If it is only an upgrade identifier, document it in the evolution layer.
