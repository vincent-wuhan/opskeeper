# Open-source gate: full release

**Status: LOCAL PASS; REMOTE VERIFICATION PENDING**
**Gate date: 2026-09-03**
**Release:** OpsKeeper full public source release, stage 2

This gate records the admission review for publishing the OpsKeeper backend, web console, plugins, public deployment assets, synthetic fixtures, documentation, and tests. It is not legal advice.

## Scope

`RELEASE_VERSION.json` is the machine-readable source scope. Private delivery evidence, credentials, production topology, private project records, and non-public incident data are outside the release.

## Product and competition boundary

The public repository is product documentation, not event documentation. It must not contain event-stage language, judge-facing private evidence, internal task IDs, real incident markers, private execution records, or competition delivery state. Operational scenarios are published as reproducible synthetic examples.

## Attribution boundary

OnGrid is allowed only in the compliance and acknowledgment layer. It must not appear as a product name, current runtime component, package name, UI label, or endorsement. References to AgentTeams and AgentTeams Dashboard describe integration and inspiration; they do not imply redistribution of their core source or endorsement by those projects.

## Automated checks

Run before tagging:

```bash
gofmt_check="$(gofmt -l $(find cmd internal -name '*.go' -type f | tr '\n' ' '))"
test -z "$gofmt_check"
go build ./...
go test ./... -count=1
python3 scripts/audit_open_source.py
```

The admission audit checks private identifiers, internal task IDs, event-stage language, private paths and IPs, prohibited repository references, hard secret shapes, disallowed OnGrid placement, private evidence directories, and required release files.

## Build and functional checks

Before tagging, a clean tree must also pass web type checking/tests when tooling is available, plugin package construction, plugin self-checks, and the TeamHarness test suite. Any environment-specific skip must be recorded in the release validation log rather than silently treated as a pass.

## Release decision

A release may be tagged only when:

1. automated and build checks pass;
2. the source tree is a sanitized snapshot with no private Git history;
3. the public repository remains `vincent-wuhan/opskeeper`;
4. release metadata and acknowledgments are final;
5. the resulting tag is verified from the public remote before packages are distributed.
