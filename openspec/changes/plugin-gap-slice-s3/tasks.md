# Tasks: plugin-gap-slice-s3

对应 4.8 方案 §5.1 / §5.3 切片 S-3（observability 0.3.0），关闭 `plugin-gap-slice-s1` tasks.md 7.1 的 observability 部分与 7.2 的就地降级。

## 1. OpenSpec change

- [x] 1.1 开 `openspec/changes/plugin-gap-slice-s3/`，proposal 关联 4.8（§5.0 / §5.1 / §5.3 / §4 措辞）
- [x] 1.2 `design.md` 记录生成链、scope 三处同步、桥第三种 miss 的设计与计数同步口径
- [x] 1.3 `specs/plugin-gap-slice-s3/spec.md` 写可验收的 Requirement / Scenario

## 2. scope 增量（唯一一处动 profile）

- [x] 2.1 `core/domain/plugin.go` 新增 `ScopeKnowledgeRO Scope = "knowledge.read"`（注释写明不折进 observability.read 的凭据边界理由）
- [x] 2.2 `core/floor/pluginmanifest/profiles.go`：finance 与 SaaS 两 profile `Granted` 均加 `domain.ScopeKnowledgeRO`
- [x] 2.3 既有守卫全绿：`TestEveryComposedPackageIsActuallyAdmittedByItsProfile` / `TestTheFinanceProfileIsNotMerelyAStricterSaaS` / NotGranted 清单零改动

## 3. manifest 与生成链

- [x] 3.1 `plugins/pig-ops/opskeeper-sre-observability/pig-ops.yaml`：tools += `get_host_processes` / `query_knowledge`（class read，排序插入，逐条注释；limits 不写——偏差与理由记在 design.md）；`required_scopes` += `knowledge.read`；version 0.2.0 → 0.3.0；package.json 同步
- [x] 3.2 `observabilityTools()` 清单 +2 条，跑 `OPSKEEPER_UPDATE_TOOLSET=1` 重生成 canonical tools.go
- [x] 3.3 `scripts/sync-pig-ops.sh` 同步包内副本

## 4. 桥的就地降级（7.2）

- [x] 4.1 manager 侧：`Registry.KnownToolNames()`（闭包面 ∪ `baseToolCandidateNames` 静态候选清单）+ 候选清单覆盖测试（全依赖 BuildBaseTools 产出 ⊆ 候选）
- [x] 4.2 桥：`newBaseToolUpcall` 注入 known 清单；`invoke` 增加第三种 miss——命中 known 而 bag 无此名 → "存在但未配置"（denied + reason，非 errBaseToolNotHere）
- [x] 4.3 `main.go` 装配点传入 `toolsReg.KnownToolNames()`
- [x] 4.4 用例：known+bag 缺失 → 措辞含 not configured 且不含 no such tool；不在 known → 仍 errBaseToolNotHere
- [x] 4.5 对账闸门 `plugin_gap_gate_test.go` 零改动（manifest 加名自动覆盖）

## 5. 测试、persona 与计数

- [x] 5.1 `observability_profile_test.go`："twelve tools" → "fourteen tools"；scope 正向清单 += knowledge.read、哲学注释同步
- [x] 5.2 `opskeeper-observability` SKILL 工具说明 += query_knowledge / get_host_processes
- [x] 5.3 capability 表：query_knowledge → 新 `knowledge` 家族；get_host_processes → host
- [x] 5.4 released 指纹表（实算）、marketplace roundtrip 0.3.0
- [x] 5.5 ledger 计数：observability 12 → 14、总数 97 → 99、upcall 汇总 68 → 70

## 6. 验收

- [x] 6.1 `make pi-plugin-guard && make module-check && go test ./scripts/ledgercheck -count=1`
- [x] 6.2 `go test ./...` 全量（三模块）
- [x] 6.3 `make audit-open-source` 通过
- [x] 6.4 `make deadcode-ratchet-check` 不涨
- [x] 6.5 PR：中文说明 + 贡献指南勾选项 + Conventional Commit
