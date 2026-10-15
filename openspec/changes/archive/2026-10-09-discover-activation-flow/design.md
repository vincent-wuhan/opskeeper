## Context

Discover 页(`web/src/pages/Discover.tsx`)聚合技能/插件/结晶三个 Tab,但全部只读。可复用的既有原语:`installPack`(`api/marketplace.ts:222`)、promoteCrystallized(`api/crystallized.ts:91-95`,仅落盘草稿)、审批治理已建的应用内 Modal 确认原语(禁止 window.confirm 的项目约束)。

关键治理约束:启用/安装/晋升是**写操作**,若属危险或影响共享状态,MUST 落回审批生成 proposal 走双签——这是 OpsKeeper「治理即产品」的硬边界,不能为了动线顺滑而绕开。

## Goals / Non-Goals

**Goals:**
- 发现→一键启用→沉淀闭环:三类产物统一启用动线,卡片就地状态迁移
- 危险写操作回退审批治理(生成 proposal 走双签),不直接执行
- 结晶晋升动线:历史证据展示 → 提交 → 转自治
- 卡片内联三态 installing/enabled/failed,失败可重试

**Non-Goals:**
- 不自建第二套插件/发布模型,强制复用 pig-runtime-adapter 与 runtime-management-gap
- 不新建 Discover Tab(沿用已有 skills/plugins/crystals 三 Tab)
- 不做付费/计费
- remix/fork 仅在后端支持复制为本地草稿时做,无 API 则不做并标注后续

## Decisions

- **D1 统一启用原语**:一个共享动线组件(确认弹窗 → 调接口 → 就地状态迁移),三类产物以参数差异复用;确认一律走审批治理 Modal 原语,禁止 window.confirm。
- **D2 治理分流**:动线内先判操作风险——低风险(如安装到个人技能)直接执行;危险或影响共享状态的操作 MUST 生成审批 proposal 走双签,UI 引导至审批页而非执行。分流判据在设计阶段细化。
- **D3 结晶晋升走 promote + 审批**:promoteCrystallized 仅落盘草稿(Q2 待核实接口语义),晋升为自治按「promote 提交 → 审批 → 生效」动线;promote 接口不可用时入口禁用并说明原因。
- **D4 发布模型前置核查**:pig-runtime-adapter 不可变发布模型未落地时(Q3),本 change 只做前端动线 + 只读态,不伪造安装成功态。

## Risks / Trade-offs

- [promote 接口语义不确定(Q2)] → 设计阶段核查后定动线;不可用则入口禁用并说明,不伪造可用性
- [发布模型未落地(Q3)导致安装动作无法真实完成] → 降级为前端动线 + 只读态,明确标注后续
- [危险操作误直执行] → 治理分流为硬规则;分流判据宁可过严,可疑一律走审批
- [三类产物动线分叉变成三套代码] → 统一原语组件,差异以参数表达

## Migration Plan

纯前端动线 + 既有接口复用;promote/安装接口不可用的部分以禁用态呈现。回滚即还原 Discover 组件,无数据迁移。

## Open Questions

- Q2:promoteCrystallized 仅落盘草稿。晋升为自治走 promote+审批还是另有接口?
- Q3:pig-runtime-adapter 的不可变发布模型是否落地?未落地则只做前端动线 + 只读态