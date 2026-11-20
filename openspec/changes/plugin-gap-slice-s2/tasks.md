# Tasks: plugin-gap-slice-s2

对应 4.8 方案 §5.2 / §5.4 切片 S-2（readonly 0.3.0），关闭 `plugin-gap-slice-s1` tasks.md 7.1 的 readonly 部分。

## 1. OpenSpec change

- [x] 1.1 开 `openspec/changes/plugin-gap-slice-s2/`，proposal 关联 4.8（§5.0 / §5.2 / §5.4 / §6.3）
- [x] 1.2 `design.md` 记录对照表、手工清单抄录纪律与 persona 同步口径
- [x] 1.3 `specs/plugin-gap-slice-s2/spec.md` 写可验收的 Requirement / Scenario

## 2. manifest 增量

- [x] 2.1 `plugins/pig-ops/opskeeper-sre-readonly/pig-ops.yaml` tools += `query_incidents` / `get_incident_detail` / `query_devices`（class read，排序插入，逐条注释；limits 不写——偏差与理由记在 design.md）
- [x] 2.2 `metadata.version` 0.2.0 → 0.3.0
- [x] 2.3 required_scopes 零增量（`alert.read` / `topology.read` 既有）

## 3. 工具清单（手工）

- [x] 3.1 `core/pig/extensions/opskeeper-sre-readonly/tools.go` 按三个 `Info()` 原样同步 toolSpec（schema 逐字，含 `get_incident_detail` 的批量信封）
- [x] 3.2 排序与去重由 `TestTheProfilesToolListIsTheAllowListTheHostWillBuild` 钉住
- [x] 3.3 运行 `scripts/sync-pig-ops.sh` 同步包内副本

## 4. 测试与 persona

- [x] 4.1 `core/manager/biz/aiops/tools/profile_contract_test.go` 两个方向的对照清单各 +3
- [x] 4.2 `diagnose-readonly` SKILL 的事件读与设备清单入口说明
- [x] 4.3 闸门 `plugin_gap_gate_test.go` 零改动且经 manifest 自动覆盖三个新名字（读数记录在 PR）

## 5. 验收

- [x] 5.1 `make pi-plugin-guard && make module-check && go test ./scripts/ledgercheck -count=1`
- [x] 5.2 `go test ./...` 全量
- [x] 5.3 `make audit-open-source` 通过
- [x] 5.4 `make deadcode-ratchet-check` 不涨
- [x] 5.5 PR：中文说明 + 贡献指南勾选项 + Conventional Commit
