# console-reskin Specification

## Purpose
TBD - created by archiving change opskeeper-teamily-ui. Update Purpose after archive.
## Requirements
### Requirement: 控制台页面容器层换肤
Dashboard、Approvals 等专业控制台页面 SHALL 将容器层(页面标题区、统计卡、列表卡、状态标签)替换为新卡片语言:Card 使用 rounded-2xl + shadow-card,状态 pill 使用 rounded-full + 呼吸点,数字统计卡保持信息密度;换肤 MUST 仅涉及容器与样式层。

#### Scenario: Dashboard 统计卡换肤
- **WHEN** 用户打开 Dashboard 页
- **THEN** 统计卡、图表容器、列表容器均呈新卡片语言(大圆角 + 柔和阴影),数据与图表渲染结果与改造前一致

#### Scenario: Approvals 大卡化
- **WHEN** 用户打开审批中心
- **THEN** 待审批项以大卡呈现(动作、来源事件、影响面、操作按钮);双签进度与签署人的呈现改由 `approval-governance` 能力规定,本能力不再要求「双签策略标识保留」

### Requirement: 业务逻辑零改动
控制台页面换肤 MUST 不改变任何业务逻辑:数据获取、筛选、分页、排序、审批操作、导出的行为与接口调用与改造前一致;现有交互测试断言 MUST 通过。

#### Scenario: 换肤后逻辑回归
- **WHEN** 运行既有控制台页面交互测试并人工走查审批操作
- **THEN** 全部数据操作与审批流转行为与改造前一致,测试全绿

