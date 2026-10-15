## ADDED Requirements

### Requirement: 每日值班简报生成
系统 SHALL 复用既有 report 调度能力(`ReportKind='daily'` + `cron_spec` + `agent_persona` + `prompt_override`)提供每日值班简报:调度触发生成器时,以既有 `reporter` persona 渲染三节模板——昨夜事件摘要、待审批项、告警趋势与今日关注。三节模板 SHALL 经 schedule `prompt_override` 注入,MUST NOT 复制或新建 persona 文件。简报生成 MUST 遵循报表防幻觉契约:数字无来源时对应内容 MUST 留空,MUST NOT 编造。当日无事件与待审批时,简报 SHALL 呈现「平静」态。简报 MUST NOT 执行任何变更,只汇总与建议。系统 MUST NOT 在夜间推送。

#### Scenario: 调度触发生成三节简报
- **WHEN** 调度触发
- **THEN** 生成含三节(昨夜事件摘要/待审批项/告警趋势与今日关注)的简报交付物

#### Scenario: 数字无来源留空
- **WHEN** 某项统计数字无数据来源
- **THEN** 该处留空,MUST NOT 编造

#### Scenario: 平静态
- **WHEN** 当日无事件与待审批
- **THEN** 简报显示「平静」态而非空白或推测内容

#### Scenario: 简报不执行变更
- **WHEN** 简报生成或被阅读
- **THEN** 简报仅呈现汇总与建议,不触发任何变更执行

#### Scenario: 历史简报回看
- **WHEN** 用户打开历史简报
- **THEN** 可回看往期简报,复用报表列表

### Requirement: 当前待审批队列事实
报表事实层(`core/manager/biz/report/facts.go`)SHALL 新增「当前待审批队列」事实:当前 pending 审批计数(总数 / 零签署 / 部分已签),以纯 SQL 计算,与既有 ReportFacts 同风格注入。该事实只带计数,MUST NOT 暴露「所需签署人数」(需签口径由前端策略常量 `DUAL_SIGN_REQUIRED` 负责),MUST NOT 由 LLM 产出该数字。

#### Scenario: 待审批事实注入
- **WHEN** 简报或任何 report 生成
- **THEN** 待审批节使用 SQL 计算的 pending 计数事实(总数 / 零签署 / 部分已签),不使用 LLM 生成的数字

### Requirement: 无源小节不渲染
结晶自愈次数与今日值班两节无持久数据源(结晶 Ledger 为刻意内存态、无 oncall 排班数据),本期模板 SHALL 不渲染这两节,MUST NOT 出现「数据源建设中」之外的占位或编造数字;待对应数据源落地后另立 change 补节。

#### Scenario: 无源节不出现
- **WHEN** 每日简报生成
- **THEN** 简报不包含结晶自愈次数与今日值班小节

### Requirement: 每日简报预设入口
报表调度管理面(`web/src/pages/Tasks.tsx`)SHALL 提供一键「创建每日值班简报」预设:预填 kind=daily、默认 09:00 cron、默认飞书渠道、persona=reporter、prompt_override=三节模板;经既有 schedule 创建接口提交,预设参数 SHALL 可在既有 schedule 编辑界面修改。系统 MUST NOT 为预设新建后端端点。

#### Scenario: 一键创建预设
- **WHEN** 用户点击「创建每日值班简报」预设
- **THEN** 预填表单经既有创建接口提交,生成 daily 调度

#### Scenario: 预设可修改
- **WHEN** 用户编辑已创建的简报调度
- **THEN** cron 时间与渠道可在既有 schedule 编辑界面修改

### Requirement: 简报 IM 推送
简报生成完成后,系统 SHALL 复用既有 report delivery 通道(`core/manager/biz/report/delivery.go`)经已配置的 IM 渠道(feishu/dingtalk/telegram/slack)推送一句话摘要与简报回链;正文 SHALL 留在平台内呈现。IM 渠道未配置时 SHALL 仅生成简报不推送,MUST NOT 报错失败。推送时间 MUST 在日间(不在夜间推送)。系统 MUST NOT 新建推送通道。

#### Scenario: 生成后推送摘要与回链
- **WHEN** 简报生成完成且已配置 IM 渠道
- **THEN** 经该渠道推送摘要与回链,正文留在平台内

#### Scenario: 渠道未配置降级
- **WHEN** 未配置任何 IM 渠道
- **THEN** 仅生成简报不推送,简报仍可平台内回看,不报错