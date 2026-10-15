## 1. 前置核查

- [x] 1.1 核查 report-schedule 既有能力(`api/reports.ts:163-219`、`cron_spec` :135、`ReportKind='daily'` :8):调度已支持 cron_spec/timezone/channel_ids/agent_persona/prompt_override/in_app_visible + run-now + toggle,`ReportKind='daily'` 已存在,调度 UI 在 `web/src/pages/Tasks.tsx`;生成链路 `generator.go`(persona 正文 + schedule prompt_override + ContentJSON 脚手架 + LANGUAGE 本地化)零改动可支撑简报。结论写回设计
- [x] 1.2 核查 IM 推送:`core/manager/biz/report/delivery.go` 已实现 `Deliver(ctx, DeliverySummary, channelIDs)` 逐渠道 fan-out + DeliveryRecord 持久化 + MarkdownSummary;渠道未配置降级为既有行为。结论写回设计
- [x] 1.3 Q4 默认配置落定:09:00 + 默认飞书渠道(2026-10-08 立项确认),进 prompt/预设设计
- [x] 1.4 设计前核查真缺口:待审批项无「当前 pending 队列」事实(ActionsSummary 仅周期计数);结晶自愈次数无持久源(`crystallize.go:342-345` Ledger 刻意内存态,release 未落地);今日值班无 oncall 数据。定案(2026-10-08 用户确认):待审批节小后端增量 facts;无源两节收缩不渲染

## 2. 待审批队列事实(后端唯一增量)

- [x] 2.1 `core/manager/biz/report/facts.go` 新增「当前待审批队列」事实:当前 pending 审批计数(总数 / 零签署 / 部分已签,不含需签数),纯 SQL 计算注入,与既有 ReportFacts 同风格;不引入 LLM 产出数字

## 3. 三节模板与生成

- [x] 3.1 复用既有 `reporter` persona,不新建 persona 文件;三节模板经 schedule `prompt_override` 注入(昨夜事件摘要/待审批项/告警趋势与今日关注),与 ContentJSON schema 脚手架兼容,不引入新 Content 字段
- [x] 3.2 平静态:当日无事件与待审批时 Hero 呈现「平静」;数字无来源留空(防幻觉契约);无源两节不渲染
- [x] 3.3 简报作为 report 交付物落库,复用报表列表回看与 D1 交付物卡呈现(零额外工作,验证回归)

## 4. 调度预设与推送

- [x] 4.1 Tasks.tsx 报表调度页新增一键「创建每日值班简报」预设:预填 kind=daily / cron 09:00 / 默认飞书渠道 / persona=reporter / prompt_override 三节模板,经既有 schedule 创建接口提交;预设参数可在既有 schedule 编辑界面修改;不新建后端端点
- [x] 4.2 推送复用 `delivery.go` channel fan-out:生成完成后经默认飞书渠道推送一句话摘要+回链,正文留在平台内;渠道未配置仅生成不推送、不报错(既有行为);夜间不推送由 09:00 cron 保证

## 5. 测试

- [x] 5.1 后端测试:pending 审批队列 SQL 事实单测;三节生成断言;无事件平静态;渠道未配置降级(既有行为回归)
- [x] 5.2 前端测试:预设入口创建 schedule 参数断言(kind/cron/渠道/persona/prompt_override);既有 Tasks/Reports 测试只更新不删,不得删除任何既有测试用例

## 6. 验证与走查

- [ ] 6.1 `cd web && pnpm test` 全绿(exit 0),`pnpm typecheck` exit 0,`pnpm build` exit 0;`go build` + 相关 go test 通过
- [ ] 6.2 双主题 + 中英双语走查:预设入口、简报卡片(D1 呈现)、平静态、推送文案正确
- [ ] 6.3 复核 Non-Goals 全部未破:未新建推送通道/persona/调度模型/端点;无源两节未渲染;简报未执行任何变更;夜间不推送