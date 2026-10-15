## Why

OpsKeeper 的差异化护城河是**治理**:HITL 双签、审批与载荷哈希绑定、证据优先的审批。后端与**消息内嵌审批卡**(`MessageBubble.tsx` 的 `PendingApprovalCard`)已经把这些能力做实——HTTP 202 语义、签署人计数、影响面与风险等级全都到位。但**审批入口页**(`web/src/pages/Approvals.tsx`)是治理的门面,却仍停留在最薄弱的状态:用浏览器原生 `window.confirm`/`window.prompt` 做危险决策、完全不渲染 `signers`(双签不可见)、首位签署后刷新列表却毫无反馈。

结果是:**最该体现「治理即产品」的页面,反而是唯一一个把治理能力藏起来的地方。** 上一个 change(`opskeeper-teamily-ui`)归档时已把这一缺口登记为 Design Doc 的 D3 偏差——"双签策略标识保留"保留了改造前后都不存在的东西;canonical spec `console-reskin` 至今仍带着这句死条款。本 change 把这个偏差真正关闭。

## What Changes

- **双签可见状态**:待确认行从 `signers` JSON 解析已签署人(角色 + 时间),呈现「N 人已签 / 需 M 人」的进度;无签署时显式标注该操作的签署要求。
- **部分签署的明确反馈**:区分 202(签名已记录、尚未裁决)与 200(已裁决),用「签名已记录,等待第二位批准人」替代当前「刷新后看不出变化」的静默行为。
- **结果导向、无默认肯定项的批准动作**:撤销 `window.confirm`/`window.prompt`,改为应用内显式确认——危险操作明示后果,默认焦点不落在肯定按钮上。
- **语汇一致**:与消息内嵌审批卡 `PendingApprovalCard` 保持同一套治理语汇与状态语义,避免同一事实两处说法不一。
- **测试**:覆盖双签渲染、202/200 反馈分流、无签署时的要求标注、拒绝原因的应用内输入。

**不改**后端 API、不改数据结构、不新增 npm 依赖、不动审批执行语义。

## Capabilities

### New Capabilities
- `approval-governance`: 审批治理表面的可见性与安全性——双签进度与签署人呈现、部分签署(202)的诚实反馈、结果导向且无默认肯定项的危险动作确认、拒绝原因的应用内采集。适用于审批入口页与消息内嵌审批卡。

### Modified Capabilities
- `console-reskin`: 「Approvals 大卡化」场景中的「双签策略标识保留」是一句无对应实现的死条款(MUST 被移除),双签呈现改由 `approval-governance` 能力规定;该能力原有的「业务逻辑零改动」范围保持不变。

## Impact

- `web/src/pages/Approvals.tsx`(主要改动)
- `web/src/components/MessageBubble.tsx`(仅对齐语汇,如需)
- `web/src/api/approvals.ts`(仅类型补充,如需)
- `openspec/specs/console-reskin/spec.md`(经 delta 合并)
- **无** `core/` 后端改动、**无** `cmd/` 改动、**无**新依赖
