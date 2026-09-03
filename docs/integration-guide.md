# 集成指南：opskeeper × ops-keeper 路径 A 落地

> **面向**：opskeeper / ops-keeper 用户、平台运维、SRE / DBA 负责人
> **变更**：从单一项目升级到统一 AIOps 平台
> **基础**：opskeeper（v1.0+）+ 集成 ops-keeper 中间件能力
> **状态**：P0 文档（Task 3.8）
> **关联**：
> - 战略选型：[openspec/changes/archive/2026-07-13-unified-platform-base-selection/design.md](../openspec/changes/archive/2026-07-13-unified-platform-base-selection/design.md)
> - AIOps 平台分析：[docs/superpowers/analysis/2026-07-13-opskeeper-opskeeper-aiops-platform.md](superpowers/analysis/2026-07-13-opskeeper-opskeeper-aiops-platform.md)
> - 落地计划：[docs/superpowers/plans/2026-07-13-unified-platform-path-a.md](superpowers/plans/2026-07-13-unified-platform-path-a.md)
- 用户过渡计划：[docs/migration/opskeeper-user-transition.md](migration/opskeeper-user-transition.md)

---

## 一、为什么是路径 A（opskeeper 作基础）

两个项目**不是替代关系**，而是**广度 vs 深度互补**：

| 维度 | opskeeper | ops-keeper | 路径 A 取舍 |
|---|---|---|---|
| 资源域 | host / network / container / cloud | **PG / Redis / MQ / K8s / Git** | 合并：6 类资源统一接入 |
| Agent 编排 | Coordinator + 7 Specialist（ReAct） | Manager + Worker + 4 引擎 Harness | 取 opskeeper 编排 + ops-keeper Harness 评测 |
| 代码溯源 | 无（文本搜索） | **git-artifact**（commit ↔ file:line） | 移植 git-artifact |
| 评测平台 | ROADMAP D.3（空） | **Harness** fault-injector + judge + leaderboard | 移植 Harness |
| 自动化 | Workflow Builder（DAG） | 调度 + 巡检 + 报告 | 统一：DAG + 周期 + 一次性 |
| 部署 | Compose + systemd | Helm + migration-runtime | 统一：Compose/Helm/systemd + migration-runtime |

**评分结果**：
- 方案 A（opskeeper 作基础）：**4.075 / 5** ✅ 采纳
- 方案 B（ops-keeper 作基础）：3.350 / 5 ❌ 弃
- 方案 C（新建统一平台）：3.700 / 5 ⏸ 暂缓（团队规模扩大后评估）

**核心理由**：只需把 ops-keeper **30%** 的独特能力迁入，保留 opskeeper 80% 的核心能力（Edge 隧道、80+ BaseTool、5 平台双向 IM、Workflow Builder）。

---

## 二、统一后平台能力地图

```
┌──────────────────────────────────────────────────────────────────┐
│  入口层   Chat (Web SPA) │ IM 双向 (5 平台) │ Dashboard │ API    │
└──────────────────────────────────────────────────────────────────┘
                              ↓
┌──────────────────────────────────────────────────────────────────┐
│  L5 反馈   Harness 评测 (judge + leaderboard + golden case 库)    │
└──────────────────────────────────────────────────────────────────┘
        ↑                                      ↓
┌──────────────────────────────────────────────────────────────────┐
│  L4 行动   DAG 工作流 + 受限执行 (Edge host/cloud + Adapter      │
│            middleware) + Casbin 审批 + 周期任务                  │
└──────────────────────────────────────────────────────────────────┘
                              ↓
┌──────────────────────────────────────────────────────────────────┐
│  L3 决策   Coordinator (Eino ReAct) + Specialist + Harness        │
└──────────────────────────────────────────────────────────────────┘
                              ↓
┌──────────────────────────────────────────────────────────────────┐
│  L2 感知   因果链 + git-artifact 反向索引 + 知识库匹配 + judge    │
└──────────────────────────────────────────────────────────────────┘
                              ↓
┌──────────────────────────────────────────────────────────────────┐
│  L1 采集   OTel SDK + Adapter (PG/Redis/MQ/K8s/Git)              │
└──────────────────────────────────────────────────────────────────┘
                              ↓
┌──────────────────────────────────────────────────────────────────┐
│  资源层   6 类资源统一：host │ network │ container │ cloud │      │
│           middleware (PG/Redis/MQ/K8s/Git)                       │
└──────────────────────────────────────────────────────────────────┘
```

### 三类核心闭环

#### 闭环 ① 告警 → 根因 → 修复

```
Alertmanager 告警（PG 长事务 / Redis 大 key / K8s pod OOM）
   ↓
L2 Awareness:  因果链 + 中间件诊断 + Git commit 反查
   ↓
L3 Decision:   Coordinator → specialist-db → Adapter → git-artifact
   ↓
L4 Action:     DAG 工作流 → 审批 → Edge/Adapter 执行
   ↓
L5 Feedback:   judge 评分 → leaderboard → 知识库回流
```

#### 闭环 ② 巡检 → 诊断 → 报告

```
调度器 → Adapter 拉取快照 → 健康度评分 → Specialist 诊断
   ↓
Harness Verifier 复评 + git-artifact 关联 → 生成 PDF/Markdown 报告
   ↓
Dashboard + 钉钉/企微推送 → 历史库 + 趋势分析
```

#### 闭环 ③ 评测 → 迭代 → 优化

```
CI → cmd/opskeeper-eval → fault-injector → Coordinator 自主响应
   ↓
双模型 judge 评分（Claude Sonnet 4 + GPT-4o）→ leaderboard 增量
   ↓
评分下降 → 阻止 merge / 触发 prompt 调优 → 下一轮评测得分上升
```

---

## 三、迁移路径（按场景分线）

### 场景 A：纯 opskeeper 用户（无 ops-keeper 部署）

**无需迁移**，opskeeper v1.0+ 自动获得 ops-keeper 全部中间件能力。

启用步骤：

1. **升级 opskeeper** 到 v1.0+（Helm 或 `install.sh`）
2. **添加中间件资源**（Web → Middleware → Add Connection）
   - PG / Redis / RabbitMQ / Kafka / K8s Cluster / Git Repository
   - 凭据加密存储（opskeeper secrets 库）
3. **启用 git-artifact 反查**（Web → Settings → git-artifact）
   - 配置 GitHub / GitLab webhook
   - 推送 `X-GitArtifact-Version: v1` header + `meta.build_id`
4. **启用 Harness 评测**（可选）
   - `opskeeper-eval run --suite middleware-baseline`
   - 配置双模型 judge（默认 Claude Sonnet 4 + GPT-4o）

### 场景 B：纯 ops-keeper 用户（无 opskeeper 部署）

**需数据迁移**，按下方"数据迁移"章节。

### 场景 C：双系统并行（已有 opskeeper + ops-keeper）

**灰度并行 1-2 年**，见 Task 3.7。

---

## 四、数据迁移（仅 ops-keeper → opskeeper）

### 4.1 迁移工具

`opskeeper-migrate-from-opskeeper`（独立 CLI，Task 3.3 产出）。

### 4.2 支持的实体（9 类）

| # | ops-keeper 实体 | opskeeper 目标 | 字段映射 |
|---|---|---|---|
| 1 | `users` | `users` | 1:1 |
| 2 | `projects` | `tenants` | name → name, owner → owner_id |
| 3 | `pg_connections` | `middleware_resources`（type=postgres） | DSN 加密重存 |
| 4 | `redis_connections` | `middleware_resources`（type=redis） | 同上 |
| 5 | `mq_connections` | `middleware_resources`（type=rabbitmq/kafka） | 同上 |
| 6 | `k8s_clusters` | `middleware_resources`（type=k8s） | kubeconfig 重加密 |
| 7 | `git_repos` | `middleware_resources`（type=git） | URL + token 加密 |
| 8 | `inspection_schedules` | `schedules` | cron 表达式保留 |
| 9 | `alert_rules` | `alert_rules` | 表达式翻译 |

### 4.3 迁移流程

```bash
# 1. 导出 ops-keeper 快照
opskeeper-migrate export \
  --source opskeeper://user:pass@ops-keeper-host:5432/db \
  --output snapshot-2026-07-13.json \
  --rate 1000  # 限速 1000 行/秒

# 2. 在 opskeeper 端校验（dry-run）
opskeeper-migrate import \
  --source snapshot-2026-07-13.json \
  --target opskeeper://opskeeper-host:8080 \
  --tenant-mapping opskeeper-project-id=42:opskeeper-tenant-id=42 \
  --dry-run

# 3. 实际导入
opskeeper-migrate import \
  --source snapshot-2026-07-13.json \
  --target opskeeper://opskeeper-host:8080 \
  --tenant-mapping opskeeper-project-id=42:opskeeper-tenant-id=42

# 4. 验证
opskeeper-migrate verify \
  --source opskeeper://... \
  --target opskeeper://... \
  --report verify-2026-07-13.html
```

### 4.4 幂等 + 回滚

- **幂等**：每条记录带 `source_id`，重复导入跳过
- **回滚**：导入前自动生成 `rollback-snapshot-{timestamp}.json`，可一键回滚

```bash
# 回滚到导入前状态
opskeeper-migrate rollback \
  --rollback-snapshot rollback-snapshot-2026-07-13T10-30-00.json \
  --target opskeeper://opskeeper-host:8080
```

### 4.5 限速 + 多租户隔离

- 限速默认 **1000 行/秒**（`--rate` 可调），避免压垮目标库
- 多租户强制隔离：`--tenant-mapping` 必须显式指定映射，禁止跨租户写入

---

## 五、回滚方案

### 5.1 单变更回滚（PR 级）

opskeeper 任何合并到 `main` 的 PR 都可回滚：

```bash
git revert <commit-hash>
# 触发 CI 重新构建镜像
# helm rollback opskeeper <revision>  # Helm 部署
# docker compose down && git checkout <previous-tag> && docker compose up -d  # Compose
```

### 5.2 集成回滚（Task 级）

若集成变更（如 git-artifact linker）导致生产故障，按以下顺序：

1. **关闭新功能开关**（feature flag 模式，详见 Task 3.x）
2. **回滚到 opskeeper 上一个稳定版本**（v0.9.1）
3. **保留 ops-keeper 不动**（双系统并行期）

### 5.3 全量回滚（罕见）

若需完全回到"双系统独立运行"状态：

1. 在 opskeeper 端关闭所有 Adapter（Web → Middleware → Disable）
2. 停止 opskeeper Helm release：`helm uninstall opskeeper`
3. 恢复 ops-keeper 独立运行
4. 数据无需迁移回 ops-keeper（opskeeper 数据保留以备再次升级）

---

## 六、能力对照表（ops-keeper → opskeeper）

| ops-keeper 功能 | opskeeper 对应 | 升级方式 |
|---|---|---|
| PG 巡检 + 诊断 | Adapter.pg.*（10+ 工具方法） | 自动获得 |
| Redis 巡检 + 诊断 | Adapter.redis.*（10+ 工具） | 自动获得 |
| MQ 监控 | Adapter.mq.*（RabbitMQ + Kafka） | 自动获得 |
| K8s 事件 + 巡检 | Adapter.k8s.*（client-go） | 自动获得 |
| git-artifact 反查 | `git.find_runtime_link` + Indexer | 启用 webhook |
| Harness 评测 | cmd/opskeeper-eval + judge + leaderboard | 启用即可 |
| 工作流调度 | Scheduler + Workflow Builder | 启用即可 |
| 钉钉 / 企微通知 | 5 平台 IM 双向（已含钉钉/企微） | 已在 opskeeper 路由 |
| AI 助手（@agent） | Coordinator + 7 Specialist | 已在 opskeeper |

---

## 七、版本节奏

| 版本 | 节点 | 关键能力 |
|---|---|---|
| v1.0 | 2026 Q4 | Task 3.5（git-artifact 生产接通）+ Task 3.8（本文档） |
| v1.1 | 2027 Q1 | Task 3.3（数据迁移 CLI）+ Task 3.2（K8s 文档） |
| v1.2 | 2027 Q2 | Task 3.4（migration-runtime 重构）+ Task 3.7（用户过渡） |
| v2.0 | 2027 Q3+ | Task 3.6（Next.js 评估后决定）+ Skill Marketplace |

---

## 八、相关文档

- 运维手册：[docs/operations-manual.md](operations-manual.md)
- Harness 评测指南：[docs/harness-guide.md](harness-guide.md)
- API 文档：[docs/api/middleware.md](api/middleware.md) / [harness.md](api/harness.md) / [git-artifact.md](api/git-artifact.md)
- AIOps 平台分析：[docs/superpowers/analysis/2026-07-13-opskeeper-opskeeper-aiops-platform.md](superpowers/analysis/2026-07-13-opskeeper-opskeeper-aiops-platform.md)
- 落地计划：[docs/superpowers/plans/2026-07-13-unified-platform-path-a.md](superpowers/plans/2026-07-13-unified-platform-path-a.md)

---

## 九、反馈与支持

- GitHub Issues: <https://github.com/vincent-wuhan/opskeeper/issues>
- 法律来源与商标边界：[NOTICE.md](../NOTICE.md) / [TRADEMARK.md](../TRADEMARK.md)
- 品牌治理规则：[BRAND_GOVERNANCE.md](BRAND_GOVERNANCE.md)
