## Why

`release-v2026.09.20-patch.2` 的业务入口已经在当前 `main` 上完整保留并增强，但插件化后的企业级闭环仍未恢复完整：运行时治理、PiG 适配、Paperclip 集成、团队记忆与协同、发布元数据仍是主要缺口。需要一份跨 change 的实施计划，把这些工作按依赖关系串行或并行推进，并在计划完成后交给 OPC 小队线上落地。

## What Changes

- 建立插件化架构能力恢复的总路线图，明确 P0/P1/P2 优先级、依赖关系和阶段出口条件。
- 统筹以下既有活跃 change，不替代它们的权威任务清单：
  - `runtime-management-gap`
  - `pig-runtime-adapter`
  - `opskeeper-paperclip-feasibility`
  - `team-memory-and-collaboration`
  - `ops-daily-brief`
- 以当前 `main` 的微内核插件架构为唯一基线：
  - 六个封闭扩展点：`tool`、`adapter`、`datasource`、`collector`、`provider`、`notifier`。
  - Manager 与 Edge 双侧插件宿主。
  - manifest、能力清单、准入、发布、回滚和审计作为统一治理面。
- 先修复当前能力展示和发布元数据风险，再完成运行时治理，随后推进 PiG adapter、插件能力丰富、Paperclip 与团队协同。
- 定义每个阶段的代码、测试、E2E、安全、发布和回滚验收。
- 计划确认后生成 OPC 小队执行包：任务分解、负责人、里程碑、验收命令、风险、回滚和线上进度回写要求。
- 不引入破坏性 API 变更；所有生产启用均采用显式配置、灰度与可回滚路径。

## Capabilities

### New Capabilities

- 无。本 change 是跨 change 实施计划与交接治理，不新增独立产品能力规格；具体能力继续由既有活跃 change 和后续专项 change 承载。

### Modified Capabilities

- 无。本 change 不直接修改既有需求；实现阶段如涉及行为变化，必须在对应活跃 change 中补充 delta spec 后再实施。

## Impact

- 计划产物：`openspec/changes/plugin-architecture-recovery-plan/`。
- 后续可能影响的实现域：
  - `core/floor/kernel`
  - `core/manager/middleware/pluginhost`
  - `core/edge/pluginhost`
  - `core/pig`
  - `plugins/pig-ops`
  - `web/src/pages/admin/Runtime.tsx`
  - 插件市场、插件发布、运行时清单和事件协同相关页面
  - `VERSION`、`RELEASE_VERSION.json` 与发布脚本
- 协作影响：计划确认后需要在 OPC 中创建或更新小队任务、里程碑和验收记录。
- 部署影响：实施分阶段灰度；Paperclip 审批桥默认关闭；PiG 与新插件必须先通过只读和沙箱验收。
