## Context

OpsKeeper 的治理能力在后端与**消息内嵌审批卡**(`MessageBubble.tsx` 的 `PendingApprovalCard`)已经做实:HTTP 202 语义(签名已记录、尚未裁决)、签署人计数、影响面、载荷详情、stale(404)降级都已到位。但**审批入口页**(`web/src/pages/Approvals.tsx`)是治理的门面,仍停留在最薄弱的状态:

- 危险决策走浏览器原生 `window.confirm` / `window.prompt`;
- 从不渲染 `Approval.signers`(该字段在 `web/src/api/approvals.ts` 已有类型注释,「A pending row with one signer means dual-sign is still waiting for another」),双签完全不可见;
- 首位签署后仅 `await load()` 静默重载,看不出任何变化。

底座事实(设计前提):

| 事实 | 依据 |
|---|---|
| `approveApproval(id)` 返回整行 `Approval`(非布尔) | `web/src/api/approvals.ts:44-46` |
| `request` 仅对非 2xx 抛错,2xx 全部放行 | `web/src/api/client.ts:82` |
| 因此 202 与 200 均可从**返回行的 `status`** 区分:`pending`=签名已记录待另一批准人,`executed`=已裁决执行,`failed`=执行失败 | 后端 `core/manager/server/approval/dualsign_test.go` |
| 双签策略 = 2 位批准人;一行仅 1 个签名时仍 `pending` | `dualsign_test.go` |
| `PendingApprovalCard` 已有 `signerCount()` 宽容解析(missing/malformed→1)与 `waiting` 态渲染 | `web/src/components/MessageBubble.tsx:522,603-630,739-749` |
| 后端**不暴露**每行的「所需签署人数」字段;`Approval` 无该字段 | `web/src/api/approvals.ts` |

## Goals / Non-Goals

**Goals:**

- 让审批入口页与消息内嵌卡呈现同一套治理事实:双签进度、已签署人(角色/时间)、部分签署的等待状态。
- 用应用内、结果导向、无默认肯定项的确认替换浏览器原生对话框。
- 首位签署后给出明确反馈,而非静默重载。
- 与 `PendingApprovalCard` 共用状态语义与术语,消除同一事实两处说法不一。

**Non-Goals:**

- 不改后端 API、数据结构,不新增 npm 依赖。
- 不动审批执行语义(approve/reject 的判定、哈希绑定、执行流程)。
- 不重做整个审批中心视觉(不新增列、不改布局骨架)。
- 不引入「所需签署人数」的新后端字段(见 Open Questions)。

## Decisions

### D1 双签进度:单一共享解析函数,诚实降级

新增纯函数(供入口页与消息卡共用):

```
parseSigners(signersJson?: string): { signers: Signer[]; known: boolean }
```

- 正常:解析 JSON 数组,元素取 `user_id` / `role` / `at`。
- 缺失 / 空 / 不可解析:`known: false`,**不猜测人数**,不抛错。
- 入口页呈现:
  - `known: false` → 「签署状态未知」中性标签,**不阻塞**批准/拒绝。
  - 无签署 → 「尚未签署 · 危险操作需 2 位批准人」(策略常量)。
  - 有签署(仍 pending) → 「N 人已签 / 需 2 人」+ 已签署人角色与时间。

「需 2 人」的 2 来自平台双签策略常量(与 `dualsign_test.go` 一致),而非每行下发值——这是本设计对后端缺字段的**显式妥协**,登记为 Open Question。

### D2 202 / 200 分流:以返回行 `status` 判定,不靠 HTTP 码

`approveApproval` 返回整行,故入口页按 `res.status`(行字段)分流,而非捕获异常:

| 返回 `status` | 呈现 |
|---|---|
| `pending`(202) | 「你的签名已记录,等待第二位批准人」,并刷新该行签署进度 |
| `executed`(200) | 「已执行」+ `result` |
| `failed` | 明确失败态 + `result` |

避免当前「刷新后看不出变化」。与 `PendingApprovalCard.approve()` 的 done/failed/waiting 三分支保持一致。

### D3 应用内确认:替换浏览器对话框

- 批准:先展示应用内确认步骤(描述动作与后果),**默认键盘焦点不落在批准按钮**;用户显式确认后才 `POST /approve`。
- 拒绝:应用内采集原因(可留空),确认后提交;移除 `window.prompt`。
- 复用 `ui/` 原语(Button/Card)与既有模态/内联形态,不引入新依赖。

### D4 语汇一致:共享状态映射

入口页与 `PendingApprovalCard` 对「部分签署等待」「已签署」「已执行」使用相同术语与语义,由 D1/D2 的共享函数与文案表驱动,杜绝一处「已签署」另一处「待处理」。范围限于对齐,不改 `PendingApprovalCard` 的既有交互。

## Risks / Trade-offs

- [「需 2 人」是策略常量,非每行真值] → 明确标注为平台双签策略;`known: false` 时不显示人数,只显示「签署状态未知」。后端补字段前,常量是唯一诚实来源(Open Question)。
- [返回行 `status` 与列表刷新竞态] → 以返回行结果**就地更新**该行,不依赖随后的 `load()` 时序。
- [破坏既有 `Approvals` 交互测试] → 仅替换确认入口与新增进度渲染;既有断言语义变化处**更新断言而非删除用例**。
- [与 `PendingApprovalCard` 文案漂移] → 共享 D1/D2 函数与文案表;两处引用同一来源。

## Migration Plan

纯前端,单批可回滚提交:

1. 新增共享解析/状态映射(`api/approvals.ts` 类型补充如需、共享模块)。
2. `Approvals.tsx`:渲染双签进度 + 202/200 分流 + 应用内确认。
3. `MessageBubble.tsx` 如需:接入共享函数对齐语汇(不改交互)。
4. 测试:双签渲染、202/200 分流、无签署要求标注、拒绝原因应用内输入。

回滚 = revert 该提交;无数据迁移、无接口变更。

## Open Questions

- **每行所需签署人数**:后端未暴露该字段,本设计以双签策略常量 2 呈现。后续 change 若引入该字段,应改为按行渲染,并移除常量。本 change 不扩后端范围。
- **`signers` 元素字段的稳定性**:按 `dualsign_test.go` 与类型注释取 `user_id`/`role`/`at`;解析保持宽容,未知字段忽略。
