# Tasks: plugin-architecture-security-review

对应 `plugin-architecture-recovery-plan` 任务 6.5。基线 `main@f4b76a36` 干净检出。

## 1. 盘点既有防线

- [x] 1.1 定位八项各自对应的仓内防线文件与测试（报告 §1–§8 的「已覆盖」小节）
- [x] 1.2 复跑全部引用的闸门与测试命令并记录结果（报告 §0）
- [x] 1.3 确认 `tenantkey` 推导闸门、`routeaudit` 裁决表、`plansecurity` 三断言的实际覆盖范围

## 2. 缺口登记

- [x] 2.1 逐项列出未被既有防线覆盖的缺口（报告各节「缺口」小节）
- [x] 2.2 按标准分档：S 级（正式版阻塞）3 项、F 级（后续版本）9 项
- [x] 2.3 每个缺口给出整改建议与（可固化的）闸门化路径

## 3. 收口

- [x] 3.1 开本 change：proposal / design / tasks / specs / 报告齐备
- [x] 3.2 报告走 patch 附件通道交付（Multica issue 附件），正文不贴全文
- [x] 3.3 内部任务编号不入正文（报告与 change 均不含内部编号）

## 4. 后续（不在本 change 内，登记给正式版与后续版本）

- [ ] 4.1 正式版前处置 S-1（marketplace 生产默认 DevMode）
- [ ] 4.2 正式版前处置 S-2（IM 会话以超管身份运行）
- [ ] 4.3 正式版前处置 S-3（飞书明文事件无 verify_token 校验）
- [ ] 4.4 F 级缺口按报告各节的建议排期
