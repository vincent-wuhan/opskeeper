## ADDED Requirements

### Requirement: 插件化恢复使用统一 main 基线

OpsKeeper 插件化能力恢复 MUST 从最新远端 `main` 拉出执行分支。执行 PR MUST 使用中文注释和中文提交说明，并关联 OPC 任务、统筹 change 与具体任务编号。

#### Scenario: 从最新 main 创建执行分支

- **WHEN** OPC 小队开始实施插件化恢复计划
- **THEN** 系统记录远端 `main` commit，并从该 commit 创建 `codex/plugin-architecture-recovery-plan` 分支
- **AND** 执行 PR 的注释、提交说明和任务回写使用中文

#### Scenario: 发现基线漂移

- **WHEN** 执行过程中远端 `main` 出现新提交
- **THEN** 执行分支先 rebase 到最新远端 `main`
- **AND** 重新运行阶段验收命令后才能继续交付

### Requirement: 统筹计划不取代既有活跃 change

统筹计划 MUST 只负责排序、依赖、阶段闸门和 OPC 交接；`runtime-management-gap`、`pig-runtime-adapter`、`opskeeper-paperclip-feasibility`、`team-memory-and-collaboration` 和 `ops-daily-brief` MUST 继续作为各自能力范围的权威任务清单。

#### Scenario: 排序阶段工作

- **WHEN** 统筹计划准备进入下一阶段
- **THEN** 系统先检查该阶段依赖的活跃 change 任务与验收结果
- **AND** 只有依赖满足或明确记录豁免理由后才能推进

#### Scenario: 发现能力缺口

- **WHEN** 旧 release 业务能力在当前插件架构中没有对应恢复项
- **THEN** 统筹计划将其映射到 `tool`、`adapter`、`datasource`、`collector`、`provider` 或 `notifier` 扩展点
- **AND** 为需要实施的能力补充权威 change 任务而不是复制验收标准

### Requirement: OPC 小队线上执行可追踪

移交 OPC 小队前，项目 MUST 清理已过时的决赛与旧发布任务。每个线上执行任务 MUST 记录负责人、范围、依赖、验收命令、测试证据、部署状态和回滚方式。

#### Scenario: 移交执行计划

- **WHEN** 统筹计划通过 OpenSpec 与 Comet 校验
- **THEN** OPC 创建总执行任务和阶段里程碑
- **AND** 总任务关联统筹 change 与五个既有活跃 change

#### Scenario: 线上验收失败

- **WHEN** staging 或生产验证发现回归
- **THEN** 执行者先回滚或关闭功能开关
- **AND** 创建中文修复任务记录根因、影响、恢复时间和复验证据
