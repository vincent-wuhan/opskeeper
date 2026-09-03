# Changelog

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
