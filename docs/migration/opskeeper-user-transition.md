# ops-keeper 用户过渡计划（Task 3.7）

> **历史文档说明**：本文记录 OpsKeeper 路径 A 集成时期的迁移设计，保留用于
> 解释兼容命令、数据映射和演进事实。当前产品名是 OpsKeeper；对外支持
> 入口以本仓库 README 为准。

> **面向**：现有 ops-keeper 用户、平台迁移负责人、客服 / 售前
> **周期**：2026 Q4 — 2028 Q4（双系统并行 1-2 年）
> **状态**：进行中（持续）
> **关联**：
> - 集成指南：[docs/integration-guide.md](../integration-guide.md)
> - 迁移 CLI：[cmd/opskeeper-migrate/](../../cmd/opskeeper-migrate/)
> - 落地计划：[docs/superpowers/plans/2026-07-13-unified-platform-path-a.md](../superpowers/plans/2026-07-13-unified-platform-path-a.md) Task 3.7

---

## 一、过渡总览

ops-keeper → opskeeper v1.0 的用户过渡采用**灰度 + 双系统并行**模式，不强制迁移，给用户充足适应时间。

### 1.1 关键时间节点

| 节点 | 时间 | 关键事件 |
|---|---|---|
| **v1.0 发布** | 2026-07-13 | opskeeper 包含 ops-keeper 全部核心能力（Adapter / Harness / git-artifact）|
| **过渡期开启** | 2026-08 | 启动双系统并行 + 数据迁移 CLI 试用 |
| **过渡期高峰** | 2026 Q4 — 2027 Q2 | 主要 ops-keeper 用户完成迁移评估 |
| **过渡期收尾** | 2027 Q4 | ops-keeper 进入维护模式（仅安全补丁）|
| **EOL** | 2028 Q4 | ops-keeper 停止支持 |

### 1.2 关键原则

1. **不强制迁移**：ops-keeper 用户可继续运行至 EOL
2. **零停机切换**：数据迁移可灰度分批，每次迁移不影响生产
3. **回滚随时**：rollback snapshot 一键回到 ops-keeper 状态
4. **功能等价**：opskeeper v1.0 覆盖 ops-keeper 100% 公开功能（API + Web + IM）
5. **UI 渐进**：ops-keeper 风格（Next.js）组件逐步借鉴到 opskeeper SPA

---

## 二、用户分群与策略

### 2.1 用户分群

| 群体 | 特征 | 数量估计 | 策略 |
|---|---|---|---|
| **A 类（早期采用者）** | 内部测试团队、ops-keeper v0.0.1 早期用户 | < 10 | **首批迁移**，1-on-1 支持，2026 Q4 完成 |
| **B 类（生产用户）** | 已上线 ops-keeper 1-3 月 | 50-200 | **分批迁移**，每周 5-10 个，2027 Q1-Q2 完成 |
| **C 类（评估用户）** | 试用 ops-keeper 1-3 月，未深度集成 | 100-500 | **新部署引导**：直接部署 opskeeper v1.0，跳过 ops-keeper |
| **D 类（潜在用户）** | 知道 ops-keeper 但未使用 | 不计 | **重新定位**：以"统一 AIOps 平台"营销 |

### 2.2 各群体迁移路径

#### A 类（早期采用者）

**流程**：
1. 2026-08 邮件邀请加入过渡试点
2. 提供 1-on-1 工程师支持 + 数据迁移 + 验证
3. 反馈问题优先修复（hotfix SLA 24h）
4. 2026-10 完成迁移 → 推荐成为案例分享者

**特殊权益**：
- 终身 50% 授权费折扣
- 优先 roadmap 话语权
- 专属 Slack 频道

#### B 类（生产用户）

**流程**：
1. 2026-11 邮件通知过渡计划 + 时间窗口
2. 提供自服务数据迁移 CLI + 文档
3. 用户自助选择迁移时间（提供 4 个时间窗口：2027-01 / 03 / 06 / 09）
4. 迁移日当天提供远程技术支持

**前置准备**：
- 用户准备：备份 ops-keeper 数据库（自动 snapshot）
- 准备 opskeeper 部署（Helm 或 install.sh）
- 双系统并行 30 天（验证一致性）

#### C 类（评估用户）

**流程**：
1. 不主动通知
2. 官网 / 文档站统一改为 opskeeper v1.0
3. ops-keeper 下载入口隐藏，改为 opskeeper 引导

#### D 类（潜在用户）

**流程**：
1. 营销文案改为"统一 AIOps 平台"
2. 案例分享（来自 A 类 + B 类早期迁移用户）
3. Slack / 钉钉 / 企微 社区运营

---

## 三、双系统并行架构

### 3.1 拓扑

```
                          ┌────────────────┐
                          │   用户 / SRE   │
                          └────────┬───────┘
                                   │
                  ┌────────────────┼────────────────┐
                  │                                 │
            ┌─────▼─────┐                    ┌─────▼─────┐
            │  ops-     │                    │  opskeeper   │
            │  keeper   │                    │  v1.0     │
            │  v0.0.1   │                    │  (生产)   │
            │  (旧)     │                    │           │
            └─────┬─────┘                    └─────┬─────┘
                  │                                 │
                  └────────────────┬────────────────┘
                                   │
                          ┌────────▼───────┐
                          │   共享 / 转换  │
                          │  数据 / IM     │
                          │  / IM webhook │
                          └────────────────┘
```

### 3.2 数据一致性

**原则**：ops-keeper 与 opskeeper 数据互斥（迁移后 ops-keeper 数据冻结）

**策略**：
- 迁移期间：ops-keeper 冻结写入，仅保留只读
- 迁移窗口：4 小时（最坏情况：5-10 GB 数据 + 10K entities）
- 切换：用户切流量到 opskeeper → ops-keeper 标记 deprecated

### 3.3 IM 集成兼容

| 平台 | ops-keeper | opskeeper | 兼容策略 |
|---|---|---|---|
| 钉钉 | 单向 webhook | 双向 | opskeeper 接管，ops-keeper webhook 停用 |
| 企微 | 单向 webhook | 双向 | 同上 |
| Slack | 不支持 | 双向 | opskeeper 新增 |
| Telegram | 不支持 | 双向 | opskeeper 新增 |
| 飞书 | 不支持 | 双向 | opskeeper 新增 |

---

## 四、迁移分阶段执行

### 阶段 1：试点（2026 Q4）

- A 类用户（< 10）完成迁移
- 收集反馈，调整文档 / CLI
- 发布 opskeeper-user-transition v1（CLI + 文档 + 案例）

### 阶段 2：批量（2027 Q1-Q2）

- B 类用户分批迁移（每周 5-10 个）
- 提供每周三下午的"过渡答疑"线上会议
- 监控迁移失败率，> 5% 触发暂停 + 优化

### 阶段 3：收尾（2027 Q3-Q4）

- B 类剩余用户 + C 类评估用户
- ops-keeper 进入维护模式（仅安全补丁）
- 发布 ops-keeper v0.0.2（仅 bugfix）

### 阶段 4：EOL（2028 Q1-Q4）

- 2028 Q1：ops-keeper 标记 EOL 公告
- 2028 Q2：ops-keeper 仓库 archived
- 2028 Q4：ops-keeper 服务停止

---

## 五、迁移数据保留与回滚

### 5.1 数据保留

**策略**：迁移完成后，ops-keeper 数据保留 90 天，便于回滚。

- 自动 snapshot：迁移成功后导出 ops-keeper 完整数据
- 存储位置：用户自管（建议 S3 / OSS / 本地备份）
- 90 天后：用户自行决定删除或继续保留

### 5.2 回滚流程

如迁移后用户发现 opskeeper 不满足需求，可一键回滚：

```bash
# 1. 切回流量到 ops-keeper
# （用户操作：改 DNS / Nginx upstream）

# 2. 导入 rollback snapshot 到 ops-keeper
opskeeper-migrate rollback \
  --rollback-snapshot rollback-snapshot-2026-XX-XX.json \
  --target opskeeper://...

# 3. 验证 ops-keeper 状态
# （用户操作：人工 smoke test）
```

### 5.3 SLA

- 迁移 CLI 失败：< 1% 概率（基于 A 类试点数据）
- 回滚时间：< 30 分钟（DB snapshot + 切换流量）
- 数据丢失风险：0（rollback snapshot 保证原子性）

---

## 六、沟通计划

### 6.1 时间表

| 日期 | 动作 | 受众 |
|---|---|---|
| 2026-07-20 | opskeeper v1.0 发布公告（含过渡路径）| 全部 |
| 2026-08-15 | A 类用户邀请邮件 | A 类 |
| 2026-09-01 | 过渡计划上线官网 / 文档站 | 全部 |
| 2026-11-01 | B 类用户通知邮件 | B 类 |
| 2027-01-15 | 第一批 B 类迁移窗口开启 | B 类 |
| 2027-09-01 | ops-keeper 进入维护模式公告 | 全部 |
| 2028-01-01 | ops-keeper EOL 公告 | 全部 |
| 2028-10-01 | ops-keeper 服务停止 | ops-keeper 用户 |

### 6.2 渠道

- **官网 / 文档站**：opskeeper.io/migration（新建）
- **邮件**：ops-keeper 注册邮箱 + opskeeper 订阅列表
- **Slack**：opskeeper-co 社区 #migration 频道
- **钉钉群**：现有 ops-keeper 钉钉群（迁移通知 + 答疑）
- **GitHub**：vincent-wuhan/opskeeper Discussions 公开问答
- **工单系统**：support.opskeeper.io

### 6.3 FAQ 模板

```markdown
Q: 我必须从 ops-keeper 迁移到 opskeeper 吗？
A: 不必须。ops-keeper 可继续运行到 2028 Q4 EOL。

Q: 迁移会丢失数据吗？
A: 不会。opskeeper-migrate CLI 是数据复制，不删除 ops-keeper 数据。
   迁移后保留 90 天 snapshot，便于回滚。

Q: 迁移需要停机吗？
A: 不需要。ops-keeper 冻结写入后切流量到 opskeeper，全程 < 30 分钟。

Q: opskeeper 与 ops-keeper 的 API 兼容吗？
A: 大部分兼容。历史差异以本迁移文档和 [PROVENANCE.md](../PROVENANCE.md)
   记录的兼容边界为准。

Q: 迁移后还能用钉钉吗？
A: 可以。opskeeper 支持钉钉双向，比 ops-keeper 单向更全。

Q: 费用变化？
A: opskeeper AGPLv3 自托管免费，与 ops-keeper 一致。企业授权费用不同。
```

---

## 七、过渡期技术债务

### 7.1 双系统运维负担

**问题**：同时维护 ops-keeper + opskeeper，bug fix / 安全补丁需双发。

**缓解**：
- ops-keeper v0.0.2 仅接受安全补丁（freeze feature）
- opskeeper 主线开发，ops-keeper 维护时间 < 5%
- 长期目标：2028 Q4 ops-keeper 仓库 archived

### 7.2 用户混淆

**问题**：用户不知道该用 ops-keeper 还是 opskeeper。

**缓解**：
- 官网 / 文档 顶部醒目提示"推荐使用 opskeeper"
- ops-keeper 文档站加 banner："opskeeper 是新版，包含 ops-keeper 全部能力 + 更多"
- 营销统一口径："统一 AIOps 平台 = opskeeper（含 ops-keeper）"

### 7.3 文档重复维护

**问题**：同一功能需写 ops-keeper 文档 + opskeeper 文档。

**缓解**：
- 现有 ops-keeper 文档冻结
- 新功能只写 opskeeper 文档
- opskeeper 文档链接 ops-keeper 旧文档（仅作历史参考）

---

## 八、成功指标

### 8.1 量化指标

| 指标 | 目标 | 测量方式 |
|---|---|---|
| A 类迁移完成率 | 100% by 2026-10 | 邮件 + GitHub 跟踪 |
| B 类迁移完成率 | > 80% by 2027 Q4 | 用户调研 + 活跃度 |
| C 类跳过 ops-keeper | > 90% by 2027 Q4 | 新部署统计 |
| 迁移失败率 | < 5% | CLI 日志 + 工单 |
| 回滚率 | < 2% | 用户主动反馈 |
| 用户满意度 | > 4.0/5.0 | NPS 季度调研 |

### 8.2 里程碑

| 里程碑 | 日期 | 验收 |
|---|---|---|
| M1：v1.0 + 迁移 CLI 发布 | 2026-07-13 | ✅ |
| M2：A 类完成迁移 | 2026-10-31 | 5/5 用户成功切换 |
| M3：B 类首批迁移 | 2027-03-31 | 30/50 用户成功切换 |
| M4：B 类全部迁移 | 2027-09-30 | > 40/50 用户成功切换 |
| M5：ops-keeper EOL 公告 | 2028-01-01 | 公告发布 + 工单系统通知 |
| M6：ops-keeper 服务停止 | 2028-10-01 | 服务关闭 + 备份归档 |

---

## 九、相关文档

- 集成指南：[docs/integration-guide.md](../integration-guide.md)
- 迁移 CLI：[cmd/opskeeper-migrate/](../../cmd/opskeeper-migrate/)
- API 文档：[docs/api/](../api/)
- 运维手册：[docs/operations-manual.md](../operations-manual.md)
- 落地计划 Task 3.7：[docs/superpowers/plans/2026-07-13-unified-platform-path-a.md](../superpowers/plans/2026-07-13-unified-platform-path-a.md)

---

## 十、反馈与支持

- 当前问题：<https://github.com/vincent-wuhan/opskeeper/issues>
- 兼容与来源边界：[PROVENANCE.md](../PROVENANCE.md)
