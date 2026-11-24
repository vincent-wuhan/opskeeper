# Tasks: pig-runtime-adapter

## 1. Architecture and Contracts

- [x] 1.1 Define the `AgentRuntimeAdapter` port, runtime descriptor, lifecycle event, health, cancellation, capability, and terminal-result contracts.
- [x] 1.2 Map the adapter port to ChatRuntime and AgentTeams semantics without changing their current behavior.
- [x] 1.3 Define PiG release identity, compatibility, capability manifest, sandbox profile, and immutable execution-snapshot records.
- [x] 1.4 Add configuration for an opt-in, pinned PiG executable or Piglet artifact with ambient discovery disabled.

## 2. PiG Runtime Adapter

- [x] 2.1 Implement a PiG runtime driver that starts, probes, streams events, cancels, and terminates an isolated PiG process.
- [x] 2.2 Normalize PiG registration and lifecycle observations into OpsKeeper runtime health and task events.
  交付（#127，执行任务见对应 OPC 线程）：`biz/pigruntime/lifecycle_sink.go` 把六类事件归一为
  注册/心跳写（`RuntimeRecord`，第六类 `pig` 加入 Kind 闭集）与 `TaskResult`
  终态写（脱敏三态保守映射：applied / skipped+notes / 拒绝持久化，双闸不
  互信）。关键决策：PiG 注册走进程内直写 Store 端口，不走 `POST /v1/runtimes`
  ——PiG 是 Manager 子进程而非远端执行面，注册事实（digest/并发额度）全部
  来自 Manager 侧钉住的 descriptor，HTTP 自注册只会把进程内事实降级为网络
  声明并为子进程铸造不必要凭据；HTTP 路径保留给真远端 runtime。首个生产
  装配在 `cmd/opskeeper/pigruntime_wiring.go`：默认零环境变量零行为，
  显式启用但工件三元组缺失/摘要漂移 fail closed。deadcode 棘轮
  910/604/306 → 909/603/306（ProcessDriver 首次进入生产调用图）。
- [x] 2.3 Add capability admission that reconciles declared, observed, and allowlisted tools before exposing them to a role.
  交付（执行任务见对应 OPC 线程，基线 main@720797ef）：`biz/pigruntime/admission.go` 的
  `ReconcileCapabilities(manifest, observed, allowlist, policy)` 三方对账：
  三方交集才暴露，输出只出 StableToolID（RuntimeToolID 不出对账层）。
  规则：不在 manifest → 永不暴露 + `undeclared` drift；在 manifest 不在
  角色 allowlist → 不暴露 + `not_allowlisted`；declared 未 observed → 照常
  暴露 + `unobserved` 证据（声明优先于观察）；观察类别比声明更危险 →
  `class_mismatch`，进入副作用类即拒；同一稳定身份不一致观察 →
  `ErrDuplicateObservation` 整体拒绝。漂移裁决按 11-21 评论区默认裁决：
  未声明 write/destructive 见一即 `Rejected`（暴露集清空 + 后续准入
  fail closed），未声明 read_only → `Degraded`；`AdmissionPolicy.
  RejectAtUndeclaredReadOnly` 是只收紧不放行的绊线。**并入健康的落点**
  是 `LifecycleSink` 的注册/心跳写路径（`NewLifecycleSinkWithAdmission`
  构造，暴露集写 `CapabilitiesJSON`，drift 写 `HealthReason`），不开第二条
  健康写路径；心跳恢复逻辑同时修正为只清心跳类理由——drift 是持续性
  异常，只能由干净观察重算清除，不能被一次健康心跳抹掉。生产装配在
  `cmd/opskeeper/pigruntime_wiring.go`（declared=首切片清单，allowlisted=
  investigator 放行 host.status）。ToolClass 双拼写映射
  `toDomainToolClass`/`fromDomainToolClass` 双向钉住，闭集外 ClassUnknown
  fail closed。**裁决：allowlist 持久化本切片不建表**——无生产写入方，
  建无人写的表只会在棘轮欠债；入参是纯值，3.x 网关落地时再定表族。
  棘轮 904/610/294 → 906/610/296：`ReconcileCapabilities` 经 sink 进入
  生产调用图；+2 test-only 是 `fromDomainToolClass`（双向表交付物）与
  `NewLifecycleSink`（2.2 构造函数，wiring 改用 WithAdmission 变体后
  失去生产调用方）——两者都是切片边界，不是欠债。
- [x] 2.4 Enforce no ambient extension discovery, no provider override, no host write, and no direct production credential access in the first slice.
  交付（执行任务见对应 OPC 线程）：`Config.Validate`/`SandboxProfile.Validate` 已存在，
  本切片补验证闭环——(a) `ProcessDriver.Launch` 每次复检 Config 与
  descriptor.SandboxProfile（此前只在 NewProcessDriver 校验一次）；
  (b) `processEnvironment` 的宿主 env 注入拒绝测试（PATH/DB_*/KUBECONFIG/
  AWS/阿里云/VAULT_TOKEN/HOME 注入后仍只有 PIG_*）+ 真子进程端到端版
  （fake-PiG `/usr/bin/env > file` 回传，实证 `/bin/sh` 自设 PWD/SHLVL/_
  三个 shell 变量之外只有 PIG_*）；(c) `AllowedNetworkHosts` 从声明性死
  字段改为以 `PIG_ALLOWED_HOSTS` 逗号拼接下发，空列表不下发（「无边界
  声明」= 无网络，不解释成不受限）。
- [x] 2.5 Add deterministic fake-PiG integration fixtures for launch, registration, heartbeat, progress, cancellation, crash, and completion.

## 3. Governed Tool Gateway Integration

- [ ] 3.1 Translate PiG tool identities to stable OpsKeeper tool IDs and read/write/destructive classes.
- [ ] 3.2 Enforce tenant, incident, role, resource, capability, rate-limit, and read-only policy before dispatch.
- [ ] 3.3 Persist tool start/end, caller identity, release/capability digest, parameters hash, result state, error, and trace correlation.
- [ ] 3.4 Add tests proving undeclared, duplicate, drifted, write, destructive, cross-incident, and cross-resource tools fail closed.
- [ ] 3.5 Add proposal-only behavior for repair candidates while preserving the existing approval and exact-payload execution path.

## 4. Pi/PiG Ecosystem Inventory and Resources

- [ ] 4.1 Produce a Phase 0 inventory template and reviewed candidate list for operations-relevant Pi/PiG packages.
- [ ] 4.2 Implement immutable package/release records with source, version, digest, license, dependencies, capability manifest, and verification state.
- [ ] 4.3 Import allowlisted non-executable skills, prompts, themes, and agent declarations while skipping hooks, MCP, and executable extensions by default.
- [ ] 4.4 Handle duplicate names, provenance visibility, tenant enablement, rollback, and deterministic reload.
- [ ] 4.5 Document rejected packages and required remediation before executable admission.

## 5. Runtime Management Interop

- [ ] 5.1 Keep adapter contracts compatible with the planned runtime inventory, heartbeat, task claim, lease, workspace, credential, and event models.
- [ ] 5.2 Add a migration seam so PiG release snapshots and capability digests can move into the shared plugin rollout model without a schema fork.
- [ ] 5.3 Ensure Manager loss cancels or safely terminates unclaimed work and never retries a side-effect-capable operation without an idempotency contract.

## 6. Operations UI and Observability

- [ ] 6.1 Expose PiG runtime kind, release digest, health, capabilities, active task count, and last heartbeat in runtime inventory.
- [ ] 6.2 Emit append-only registration, capability, drift, tool-call, cancellation, rollback, and terminal events with incident/task/trace correlation.
- [ ] 6.3 Add configuration validation, artifact verification, startup, and rollout failure diagnostics.

## 7. Verification and Promotion Gates

- [ ] 7.1 Add unit and integration tests for adapter lifecycle, capability admission, policy enforcement, and event completeness.
- [ ] 7.2 Add E2E validation for one investigator-style incident using only read-only PiG tools.
- [ ] 7.3 Add process isolation, timeout, heartbeat loss, crash, cancellation, restart, and no-replay tests.
- [ ] 7.4 Add redaction, secret non-disclosure, audit bypass, cross-tenant, and sandbox constraint tests.
- [ ] 7.5 Record Phase 2 promotion evidence and define Phase 3 executable-extension acceptance criteria.

## 8. Documentation and OPC Handoff

- [ ] 8.1 Document target architecture, runtime selection, package admission, and rollback strategy.
- [ ] 8.2 Add operator runbooks for PiG registration, incident execution, drift, cancellation, and rollback.
- [ ] 8.3 Publish the Pi/PiG adoption roadmap, promotion gates, and OPC squad ownership matrix.
