# Changelog

## 1.0.75 — 2026-10-07

- Bump the TeamHarness plugin version to 1.0.75 across `plugin.yaml`, the
  dashboard package/manifest (including the versioned `dist/main-1.0.75.js`
  entry), and the qwenpaw adapter (`plugin.json` + `plugin.py`).
- No runtime behaviour change in this plugin. The accompanying backend work
  is a bounded file inventory on the read-only node package: it accounts for
  the space under a path (directory subtotals rolled up to the root, plus the
  largest files), which is what makes `host/disk-full` diagnosable from a
  node. That package goes 0.1.2 -> 0.1.3.

## 1.0.74 — 2026-10-07

- Bump the TeamHarness plugin version to 1.0.74 across `plugin.yaml`, the
  dashboard package/manifest (including the versioned `dist/main-1.0.74.js`
  entry), and the qwenpaw adapter (`plugin.json` + `plugin.py`).
- No runtime behaviour change in this plugin. The accompanying backend work
  is the node-side process ranking: `opskeeper-sre-readonly` now ships
  `host_top_processes`, which ranks a node's own processes by CPU or memory
  straight out of `/proc`, so `host/cpu-spike` is diagnosable from the node
  rather than only from the control plane. That package goes 0.1.1 -> 0.1.2.

## 1.0.73 — 2026-10-07

- Bump the TeamHarness plugin version to 1.0.73 across `plugin.yaml`, the
  dashboard package/manifest (including the versioned `dist/main-1.0.73.js`
  entry), and the qwenpaw adapter (`plugin.json` + `plugin.py`).
- No runtime behaviour change in this plugin. The accompanying backend work
  is the node agent plugin tool result fix: the five `pig-ops` packages
  returned their payload in their own JSON shape rather than in the shape the
  agent runtime reads a tool result in, so the runtime decoded it into an
  empty result and told the model "(no tool output)". The fix and its
  end-to-end proof live in the Go tree; those packages go 0.1.0 -> 0.1.1.

## 1.0.72 — 2026-10-07

- Bump the TeamHarness plugin version to 1.0.72 across `plugin.yaml`, the
  dashboard package/manifest (including the versioned `dist/main-1.0.72.js`
  entry), and the qwenpaw adapter (`plugin.json` + `plugin.py`).
- No runtime behaviour change in this release: the accompanying backend work
  is the LLM gateway streaming `delta.tool_calls` fix, which lives in the Go
  tree, not in this plugin. This bump only re-aligns the plugin version with
  the release boundary so signing does not read as a version drift.

## 1.0.71 — 2026-10-07

- Infer the PostgreSQL RCA resource type when archive evidence omits it.
- Warn instead of blocking when the worker/manifest trail version does not match the expected version.
- Repoint MCP tool-name mapping and the harness self-check at the `core/` layout after the `internal/` -> `core` module move.

## 1.0.70 — 2026-09-19

- Retry transient OpsKeeper 5xx responses after a real Matrix approval, keeping recovery deterministic when model quota is unavailable.

## 1.0.69 — 2026-09-19

- Bind final-demo approvals to the current Matrix message so accumulated room history cannot redirect or suppress a new incident approval.
- Enable manager-gate INFO readback for consumed approvals.

## 1.0.68 — 2026-09-19

- Accept Matrix display-name prefixes on human approval and rejection commands.
- Log ignored non-admin decisions and successfully consumed final-demo approvals.

## 1.0.67 — 2026-09-19

- Keep final-demo approval authoritative in Matrix while routing the approved incident directly to the Manager recovery API, avoiding quota-sensitive LLM retries after human approval.
- Extend the scenario approval window to 180 seconds and persist the complete closed-loop evidence chain for Archive readback.

## 1.0.66 — 2026-09-18

- Combine authoritative final-demo workflow projection, preview evidence, business impact, and public HITL validation.
- Enforce the final PostgreSQL pool demonstration contract: `4/4` saturation and recovery to capacity `8`.

## 1.0.65 — 2026-09-18

- Align the authoritative Manager wire contract with real baseline/A/B candidate readback and deterministic fingerprints.
- Publish the compact approval gate and full archive comparison for the controlled fixed-workload preview flow.

## 1.0.61 — 2026-09-18

- Integrate authoritative repair-preview readback with the final-demo scenario, compact approval gate, and workflow projections.
- Preserve the full A/B comparison table and controlled fixed-workload boundary in Archive.

## 1.0.64 — 2026-09-18

- Make the Archive page responsive at half-screen widths so long rules, timestamps, metrics, evidence references, trace IDs, and incident summaries wrap within their cards.
- Keep selector inputs, timeline rows, panels, and summary cards aligned without horizontal overflow.

## 1.0.63 — 2026-09-18

- Extract knowledge references directly from RCA evidence chains when structured `kb_hits` are unavailable.
- Use the Manager `/knowledge/search` contract, normalize nested `items[].doc` results, and pass the business incident ID to archive readback.
- Gracefully degrade when the Manager knowledge search API is not deployed, while still showing evidence-backed references and postmortem outputs.
- Derive the unified integration preflight version expectation from the active Dashboard manifest.

## 1.0.60 — 2026-09-18

- Add a read-only Manager archive index and make the Dashboard Archive selector use actual closed-loop incident IDs.
- Keep numeric alert-only records out of the archive selector and explain when an incident has no archived evidence chain.

## 1.0.59 — 2026-09-18

- Project stable OpsKeeper workflow stages into Matrix as `agentteams.workflow` events.
- Synchronize Element notices with AgentTeams Dashboard task-board progression by `runId`, without requiring host-application changes.

## 1.0.58 — 2026-09-18

- Scope high-contrast text tokens across the unified route, diagnostics, Runtime, Archive, plugin list, Overview widget, and Worker detail panel.
- Restore the Worker gateway-key fallback and safe default tenant so the signed Dashboard RCA proxy does not fail with an undefined tenant resolver.

## 1.0.57 — 2026-09-17

- Increase installed-plugin secondary text contrast with the adaptive foreground token and bounded opacity.

## 1.0.56 — 2026-09-17

- Add a read-only Dashboard integration preflight that verifies the target Matrix room, session, AgentTeams, OpsKeeper, Worker plugin, and Dashboard manifest chain.
- Keep the check side-effect free so it is safe to run before a live demonstration.

## 1.0.55 — 2026-09-17

- Use the Dashboard foreground token for muted plugin text so descriptions remain readable in light and dark themes.
- Add a regression check that rejects using the muted background token as text color.
- Isolate Manager identity, prompt injection, and continuation state from Worker and standalone runtimes.
- Sanitize outbound thinking content, bound successful reply/message bursts, reject cross-role targets, and add an admin incident STOP circuit breaker.

## 1.0.54 — 2026-09-17

- Add the Dashboard Archive tab for read-only incident evidence-chain, completeness, timing, trace, similar-incident, and postmortem readback.
- Keep Manager as the tenant-isolated authority for archive data and expose only sanitized control-plane evidence references.

## 1.0.53 — 2026-09-17

- Add a reviewer-only `incident.timeline` MCP tool backed by the durable business-incident event API, so string incident IDs no longer require numeric `incident.get` conversion.

## 1.0.52 — 2026-09-17

- Treat the matching in-room `OPSKEEPER_RESULT` as the authoritative Worker phase signal. Missing optional `state.put` support no longer blocks Manager progression.
- Remove unavailable `state.put`/`state.get` MCP advertisements and Worker completion requirements while retaining `incident.record` as the durable evidence trail.

## 1.0.51 — 2026-09-17

- Remove the obsolete alerter `spec.md` completion dependency. Alerter now returns its structured result directly in the project room, and Manager waits for the matching `OPSKEEPER_RESULT` marker.
- Extend the Manager dispatch guard to reject any task requiring `spec.md` in addition to `plan.md` and `result.md`.
- Align Manager/team role wording with the six deployed actors; reporter executes the postmortem skill.

## 1.0.50 — 2026-09-17

- Keep the QwenPaw runtime health version constant synchronized with package metadata.

## 1.0.49 — 2026-09-16

- Bind the finals PG pool workflow to the six deployed Worker actors. Reviewer now performs the evidence-chain audit before HITL, and reporter executes the postmortem skill after verifier passes.

## 1.0.48 — 2026-09-16

- Persist Manager request origins across runtime reloads with atomic TTL-scoped state.
- Relay matched Worker results deterministically and terminate the Manager turn after a successful completion send; retain prompt fallback when Matrix delivery fails.

## 1.0.47 — 2026-09-15

- Derive readonly and sanitizer middleware from the AgentScope middleware protocol so real QwenPaw agents initialize.

## 1.0.46 — 2026-09-15

- Allow read-only Workers to report task coordination state and results through the dedicated OpsKeeper state tool.
- Build Manager TAR, Dashboard ZIP, QwenPaw ZIP, and installer ZIP artifacts deterministically.
- Respect an explicit QwenPaw runtime when CoPaw-compatible plugin APIs are present, and resolve the task-trace signing helper in both source and installed layouts.

## 1.0.45 — 2026-09-14

- Route prefixed `teamharness__message` calls through the Manager dispatch gate and marker recording path.
- Reject Manager dispatches that require Workers to create `plan.md` or `result.md`, and restrict Worker reporting to the current project room.
- Terminate repeated read-only denials after three identical invalid calls so Workers cannot loop indefinitely.
- Adapt QwenPaw middleware to the real CoPaw Toolkit async-generator and response APIs so the Manager gate executes in the Manager runtime.

## 1.0.44 — 2026-09-14

- Register CoPaw capability diagnostics through the supported startup-hook API instead of misusing the LLM provider registration surface.
- Align toolkit middleware and native-tool registration with the real AgentScope CoPaw signatures, including explicit function schemas for the six OpsKeeper MCP proxies.
- Emit an assertion-friendly capability JSON log when the first real CoPaw toolkit passes readonly, gate, marker, and native-tool validation.

## 1.0.43 — 2026-09-14

- Add a CoPaw Plugin API compatibility lifecycle that idempotently wraps `CoPawAgent._create_toolkit`, preserves AgentTeams collaboration tools, and appends the six required OpsKeeper native tool proxies.
- Enforce readonly middleware, Manager gate state, marker handling, and toolkit registration at the correct install/toolkit-creation lifecycle stages, with loud health diagnostics for installed and missing capabilities.
- Hard-fail when the CoPaw runtime, toolkit hook, middleware/tool registration, AgentTeams base tools, or OpsKeeper tools are unavailable instead of silently loading without safety guards.

## 1.0.42 — 2026-09-12

- Allow reporter-only knowledge persistence and incident closure through the Worker boundary while retaining backend role enforcement.
- Document that Manager agent mirrors are authoritative for Worker FileSync to prevent version rollback after restart.

## 1.0.41 — 2026-09-12

- Allow reporter-only `knowledge.write` through the Worker boundary while retaining backend role-token enforcement.
- Keep unrelated filesystem, shell, browser, and mutating tools denied in read-only mode.

## 1.0.40 — 2026-09-12

- Allow the backend's canonical `query_promql`, incident, database status, and
  knowledge tool names in Worker read-only mode while retaining fail-closed
  enforcement for unlisted and mutating tools.

## 1.0.39 — 2026-09-12

- Keep Worker read-only mode fail-closed while allowing only complete,
  approved-proposal-bound `recovery.execute` calls through the backend gate.
- Require stage Workers to append `incident.record` evidence, including repair
  fingerprints, verifier recovery signals, reporter closure, and linked alert
  resolution.
- Send the incident API `page_size` contract and propagate PG pool incident,
  target, manifest, and fault-family hints into Dashboard RCA requests.
- Add safe Tempo business attributes for tool, tenant, worker, audit, incident,
  manifest, and proposal correlation without exposing tool parameters.

## 1.0.38 — 2026-09-08

- Normalize Dashboard RCA reports from `root_cause_object` and render the
  evidence-backed summary, type, detail, confidence, and evidence chain.
- Use a high-contrast report card and a versioned Dashboard entry URL to avoid
  stale module caching after plugin upgrades.

## 1.0.36 — 2026-09-08

- Deduplicate concurrent Dashboard RCA requests by incident ID across the unified entry and worker detail panel.
- Surface FastAPI `detail` messages and disable incident clicks while an RCA is in flight.

## 1.0.35 — 2026-09-07

- Send Dashboard Runtime readback requests through same-origin `XMLHttpRequest`.
- Document that AgentTeams gateway deployments must accept both exact and
  trailing-slash forms of the four read-only OpsKeeper proxy paths.

## 1.0.34 — 2026-09-07

- Fix the Dashboard RCA proxy for QwenPaw runtimes that inject
  `OPSKEEPER_GATEWAY_KEY` only into the stdio MCP child process.
- Read the existing `mcp/opskeeper` credential binding when the HTTP-router
  plugin process lacks the environment variable, without exposing the key to
  the browser.
- Derive backend-required alert-group and correlation hints from each incident
  when triggering RCA from the unified Dashboard entry.

## 1.0.33 — 2026-09-07

- Add a server-side signed Dashboard RCA proxy for `loop.investigate`.
- Keep the AgentTeams GatewayKey outside the browser and unwrap MCP text content into the Dashboard report shape.
- Consolidate diagnostics, Runtime, and plugin management into one OpsKeeper entry with internal tabs.

## 1.0.32 — 2026-09-07

- Route the Dashboard install view through the public Plugin Manager endpoint on port-13000 deployments.
- Use the Plugin Manager `file` upload field and accept its tar.gz package format.

## 1.0.31 — 2026-09-06

- Normalize OpsKeeper incident-list responses across `items`, `incidents`, `data`, and direct-array shapes.
- Align Dashboard, plugin-manager, QwenPaw, and runtime middleware versions.

## 1.0.30 — 2026-09-06

- Add the Dashboard Runtime bypass view for service health, dependency checks, manager version, judge metrics, and recent incidents.
- Add service health, localization latency, and audit completeness readback to the Dashboard overview widget.
- Keep AgentTeams collaboration state and OpsKeeper execution evidence in separate sources of truth, correlated by `incident_id`, `task_id`, and `trace_id`.

## 1.0.23 — 2026-09-03

- Record the originating Matrix room for each dispatched OpsKeeper task.
- Inject a mandatory Manager relay instruction when a Worker-room result wakes Manager.
- Add the `OPSKEEPER_COMPLETE` marker and prevent completion notices from being treated as new dispatches.

## 1.0.24 — 2026-09-03

- Relay `OPSKEEPER_COMPLETE` deterministically through Manager's Matrix client API.
- Keep prompt-based relay only as a fallback when the direct Matrix send fails.

## 1.0.25 — 2026-09-03

- Persist the original request room from the Manager `PRE_EXECUTE` input before dispatch middleware changes context.

## 1.0.26 — 2026-09-03

- Accept Markdown-emphasized `OPSKEEPER_RESULT` lines from Worker runtime output.
- Add non-sensitive marker/session logging around request-origin recording, result consumption, and dispatch registration.

## 1.0.27 — 2026-09-03

- Accept Markdown-emphasized Manager mention prefixes in Worker result lines.
- Consume a matching result marker across dispatch and execution session IDs, then relay to the original entry room.

## 1.0.28 — 2026-09-03

- Relay matching Worker results from the reliable PRE_EXECUTE request-origin map even when dispatch middleware leaves no pending state.

## 1.0.29 — 2026-09-03

- Give an explicit `OPSKEEPER TASK` contract precedence over an embedded `OPSKEEPER_RESULT` instruction.

# [1.0.22] - 2026-09-03

### Fixed

- Make the Matrix wake-up address explicit in the Worker result contract.
- Require Manager dispatches to include `@manager:<server>` and Worker results to address Manager on the same line as `OPSKEEPER_RESULT`, preventing unmentioned group replies from remaining cached without waking Manager.

# [1.0.21] - 2026-09-02

### Fixed

- Allow the QwenPaw-normalized `teamharness.message` coordination tool in read-only mode.
- Add a regression test for `teamharness__message`, ensuring Worker result reporting remains read-only.

# [1.0.20] - 2026-09-02

### Fixed

- Align `postgres.analyze_status` MCP schema with backend `analyze_database_status` array arguments.
- Add an end-to-end MCP proxy test proving PostgreSQL filters are forwarded unchanged.

# [1.0.19] - 2026-09-02

### Fixed

- Allow the QwenPaw-normalized `opskeeper.postgres.analyze.status` tool in read-only mode.
- Add a regression test for `opskeeper__postgres_analyze_status`, preventing future PG diagnostics from being denied by name normalization.

# [1.0.18] - 2026-09-02

### Added

- Add a case-owned PostgreSQL connection-pool exhaustion playbook to the coordination contract.
- Require pool capacity, waiters, probe, and PostgreSQL-side evidence in investigator output.
- Expose approved-proposal-bound `recovery.execute` to the repairer skill.
- Require independent probe/capacity/waiter/latency recovery signals before verifier passes.

### Security

- Restrict PG pool repair to `resize_pool` on the incident-owned `pool_manifest_id`.
- Keep the default Worker read-only boundary; mutation requires runtime `standard` mode plus approved proposal and audit.
- Explicitly forbid shared PostgreSQL restart, unrelated session termination, shell/browser execution, and business file writes.

# [1.0.17] - 2026-09-01

- Accept manager-directed result lines where the runtime renders the wake-up prefix as `manager` without the full Matrix ID.
- Keep the complete `@manager:<server>` prefix as the required/recommended Worker protocol.

# [1.0.16] - 2026-09-01

- Prevent result-only Worker messages from being classified as new tasks by the long task-ID fallback.
- Require either an explicit `OPSKEEPER TASK` marker or a non-result message before fallback task IDs can wake a pending Manager.

# [1.0.15] - 2026-09-01

- Consume matching Worker results when the required Matrix `@manager` wake-up prefix is on the same result line.
- Prevents a current result from leaving its dispatch marker pending when historical context also contains an older result.

# [1.0.14] - 2026-09-01

- Add an audit warning when a successful dispatch queues the native ReAct pending stop.

# [1.0.13] - 2026-09-01

- Queue QwenPaw's native ReAct pending-stop state directly after successful Manager dispatch.
- Keeps the turn boundary effective when runtime hook registration is not attached to an existing workspace during plugin reload.

# [1.0.12] - 2026-09-01

- Record Manager task markers only after a successful `message` dispatch.
- Fixes `1.0.11` self-locking where an admin input marker was treated as an already dispatched duplicate before the Manager could forward it.

# [1.0.11] - 2026-09-01

- Register a QwenPaw agent stop gate that terminates the Manager turn immediately after dispatch.
- Prevent `SKIP_AGENT` empty turns from being counted as repeated `NO_REPLY` output.

# [1.0.10] - 2026-09-01

- Register explicit task markers when they enter the runtime, before LLM rewriting.
- Fall back to extracting long `OPSKEEPER-...` task IDs from rewritten dispatch messages.

# [1.0.9] - 2026-09-01

- Remove unreliable runtime identity inference from the dispatch gate.
- Activate the gate only for explicit `OPSKEEPER TASK` markers and their matching result continuations.

# [1.0.8] - 2026-09-01

- Record dispatched task markers in both the Manager source session and the message-tool target room.
- Skip every non-result, non-new-task, non-admin continuation while a task is pending, including rich historical context.

# [1.0.7] - 2026-09-01

- Add a plugin-native Manager continuation gate using the QwenPaw `PRE_EXECUTE` hook.
- Register dispatched `OPSKEEPER TASK` markers and skip empty/self continuations until a matching `OPSKEEPER_RESULT`, a new task, or an admin instruction arrives.
- Enforce one task dispatch per Manager turn and deny duplicate pending task markers.

# [1.0.6] - 2026-08-31

- Allow the AgentTeams `message` coordination primitive under read-only mode.
- Keep file writes, shell/browser execution, unknown tools, and mutating OpsKeeper tools denied before execution.
- Fixes Manager delegation being blocked after read-only enforcement, which previously led to doom-loop protection.
- Adds an explicit Manager one-dispatch-per-turn rule so delegation success ends the turn and Worker replies start a new turn.

## [1.0.5] - 2026-08-31

### Security

- 为 QwenPaw Worker 增加运行时只读硬边界：默认 `read_only`，未知与变更类工具在执行前返回 `DENIED`
- 只读模式使用显式白名单；可信变更型 Worker 必须显式设置 `OPSKEEPER_PERMISSION_MODE=standard`
- 拒绝事件写入 Worker 日志，并尽力同步 OpsKeeper audit，避免自审计结果成为唯一事实

## [1.0.4] - 2026-08-30

### Fixed
- 同步 `plugin.yaml` MCP 工具白名单与 `mcp/tools.py`，补齐 `recovery.execute` 与 `incident.record`
- 修正 `incident.get` 单 ID 到后端批量 `incident_ids` 的参数转换
- 修正 `loop.investigate` 工具 Schema，显式声明后端必需的告警组与关联提示
- 统一插件构建产物到 `plugins/opskeeper-teamharness/dist/`

## [1.0.0] - 2026-08-25

### Added
- 初始发布 opskeeper-teamharness AgentTeams 插件（v1alpha1 协议）
- 6 Worker skill：`opskeeper-{alerter,investigator,critic,reviewer,repairer,verifier}`
- 1 Manager skill：`opskeeper-coordination`（派活决策树）
- qwenpaw adapter（plugin.json + plugin.py + task_trace.py + install/uninstall/build/validate）
- claude-code adapter（占位；与 AgentTeams 原生一致）
- stdio MCP server `mcp/server.py`（proxy → opskeeper HTTP /v1/mcp）
- 14 tools catalog `mcp/tools.py`
- plugin ↔ backend 名字对齐 `mcp/names.py`（NAME_REMAP + PLUGIN_NATIVE）
- Bearer GatewayKey + HMAC-SHA256 + LoongSuite trace 透传 `mcp/auth.py`
- LoongSuite 兼容 `loongsuite/agents.d/opskeeper-teamharness.json`
- out-of-band 桥接 `examples/{higress-setup.sh,review-and-run.sh}`
- CI 自检 `scripts/self_check.py`（7 checks）
- Python 测试套件：`mcp/test_alignment.py` 22 个 + `adapters/qwenpaw/test_task_trace.py` 5 个
- plugin.yaml `mcp.servers.tools` 与 tools.py 同步（14 个）

### Compatibility
- AgentTeams ≥ 2.0.1
- QwenPaw ≥ 2.0.1，< 2.1.0
- opskeeper-v2 backend `/v1/mcp` v1 协议 + `/v1/{state,hitl,skills}/*` REST
- Python ≥ 3.9（urllib 标准库）

### Notes
- AgentTeams 仓库 0 修改
- 详细设计：见 `openspec/changes/agentteams-opskeeper-integration/`
- 架构决策：ADR-020
