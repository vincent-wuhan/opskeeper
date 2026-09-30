# Tasks

## 1. Research Baseline

- [x] 1.1 Confirm OpsKeeper repository branch and clean working state.
- [x] 1.2 Identify Paperclip upstream repository metadata, license, default branch, and latest master commit.
- [x] 1.3 Read the latest Paperclip plugin spec, manifest types, validators, webhook schema, and approval schema.
- [x] 1.4 Inventory OpsKeeper incident, approval, MCP, recovery, audit, and notification APIs.
- [x] 1.5 Identify Paperclip plugin maturity and deployment constraints.
- [x] 1.6 Confirm license compatibility and attribution requirements.

## 2. Feasibility Deliverables

- [x] 2.1 Compare connector plugin, OpsKeeper adapter, and AgentTeams replacement alternatives.
- [x] 2.2 Select the phased connector-plugin route.
- [x] 2.3 Define Phase A read-only scope.
- [x] 2.4 Define Phase B approval-bridge scope and authority boundary.
- [x] 2.5 Define Phase C read-only agent-tool scope.
- [x] 2.6 Define incident, event, RCA, approval, audit, and verification mappings.
- [x] 2.7 Define security, failure, rollout, and go/no-go criteria.

## 3. Future Implementation Validation

- [ ] 3.1 Pin a Paperclip release or commit for the first implementation and record its plugin API surface.
- [ ] 3.2 Confirm or add an OpsKeeper scoped service account with read-only and approval-only scopes.
- [ ] 3.3 Build a fake OpsKeeper contract server covering incident list/detail, archive, approval, and failure paths.
- [ ] 3.4 Scaffold a Paperclip plugin manifest with minimal read-only capabilities.
- [ ] 3.5 Implement incident polling, idempotent mapping, cursor persistence, and stale-state display.
- [ ] 3.6 Add Paperclip UI surfaces for incident list, timeline, RCA summary, evidence links, and OpsKeeper deep links.
- [ ] 3.7 Add read-only agent tools with schema validation, timeout, rate limit, and audit logging.
- [ ] 3.8 Implement the approval bridge behind `approvalBridgeEnabled=false` and verify it remains disabled by default.
- [ ] 3.9 Test approval idempotency, timeout, replay, invalid target hash, invalid payload hash, and actor mismatch.
- [ ] 3.10 Run local Paperclip + OpsKeeper E2E for incident sync, stale state, approval bridge, and audit readback.
- [ ] 3.11 Review plugin capabilities, secret handling, logs, UI trust boundary, and network egress.
- [ ] 3.12 Package and install the plugin in a self-hosted Paperclip environment.
- [ ] 3.13 Write an operator runbook covering configuration, sync recovery, approval errors, disable/rollback, and upstream upgrade.
- [ ] 3.14 Produce the final go/no-go implementation report with effort, risks, and recommended phase boundaries.
