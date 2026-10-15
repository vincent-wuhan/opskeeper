## Why

运维团队每天早上的第一件事是「昨晚发生了什么、有什么等我处理」。对标 teamily 的 AI 每日简报(daily brief),OpsKeeper 需要一个低成本、自动生成的每日值班简报触点——让值班工程师不打开平台也能掌握态势,并给平台一个日常留驻点。这是 Phase 4 功能形态深化的 D5 方向。

## What Changes

- **复用既有 report 机器做预设化**:`ReportKind='daily'` + `cron_spec`(默认 09:00,Q4 定案)+ `agent_persona`(复用既有 `reporter`,不新建 persona 文件)+ `prompt_override`(三节模板)全部是既有调度字段,零调度模型改动
- **简报模板三节**:昨夜事件摘要(KeyIncidents)/ 待审批项(pending 队列事实 + 周期计数)/ 告警趋势与今日关注(AlertCounts + Advice);无源两节(结晶自愈次数、今日值班)不渲染,待数据源落地另立 change
- **后端唯一增量**:报表事实层(`core/manager/biz/report/facts.go`)新增「当前待审批队列」SQL 事实(pending 计数:总数 / 零签署 / 部分已签,不含需签数)
- **一键预设入口**:Tasks.tsx 报表调度页预填 kind=daily / 09:00 / 默认飞书渠道(Q4 定案)/ persona reporter / prompt_override 三节模板,经既有 schedule 创建接口提交,零新端点
- **推送复用**既有 report delivery 通道(`delivery.go` channel fan-out),未配置渠道仅生成不推送;夜间不推送由 09:00 cron 保证

## Capabilities

### New Capabilities

- `ops-daily-brief`:每日值班简报能力——三节模板(经 prompt_override)、pending 审批队列事实、report 调度复用、一键预设入口、IM 推送摘要+回链、平静态、无源节不渲染

### Modified Capabilities

(无)

## Impact

- `core/manager/biz/report/facts.go` 增补 pending 审批队列 SQL 事实;generator/delivery/scheduler 零改动
- `web/src/pages/Tasks.tsx` 新增一键预设按钮(预填表单,复用既有创建接口)
- `api/reports.ts` 调度既有能力直接复用,不改后端调度模型
- 复用 `reporter` persona(`agents/reporter.md`)与 report delivery IM 渠道,不新建 persona/推送通道
- 简报卡片呈现复用 D1(deliverable-surface-deepening)的交付物卡

## Open Questions(设计阶段需澄清)

(无——Q4 定案「默认 09:00 + 默认飞书」(2026-10-08 立项确认);待审批节定案「小后端增量 facts」;无源两节定案「收缩为三节不渲染」,均 2026-10-08 用户确认)