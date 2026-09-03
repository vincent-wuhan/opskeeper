# opskeeper Public Roadmap

> **面向社区与生态的公开版路线图。** 内部代号、内部 deadline、敏感商业信息均不在此出现。
> 完整内部路线图（含 ADR / HLD 引用）见 `ROADMAP.md`（仓库维护者可见）。

_Last updated: 2026-08-26_

## 项目定位

opskeeper 是一个面向 SRE / DevOps 团队的 AI Agent 系统：

- **多源告警聚合 → 自动根因定位 → 修复建议生成 → 服务恢复验证 → 事故复盘** 全链路
- 通过 `opskeeper-teamharness` 插件接入 [AgentTeams](https://github.com/agentscope-ai/AgentTeams) 生态
- 后端 Apache-2.0（核心 opskeeper 二进制）；插件与团队模板同 LICENSE

## 三大支柱

| 支柱 | 当前状态 | 公开路线 |
|---|---|---|
| **Agent 协作框架** | 7 Worker Skill（alerter/investigator/critic/reviewer/repairer/verifier/postmortem）+ Manager 决策表 + L0–L3 SafetyLevel | 下一里程碑：Manager dispatch 双轨（规则 + LLM 仲裁）；新 Worker Skill 按 RFC 流程接纳 |
| **Skill 生态** | Nacos Skill Registry + `skill_meta.yaml` schema + 本地降级 | 下一里程碑：Aliyun Skills 适配（sre-aliyun-mcp / ack-mcp / arms-mcp 直接挂为 backend MCP tool group）；第三方 Skill 提交指南 |
| **可观测 & 审计** | OTel + Loki + Tempo + Prometheus 全栈；append-only HMAC-chain audit ledger；W3C traceparent 透传 | 下一里程碑：AgentLoop / LoongSuite OTLP adapter；red-team 安全剧本（tool injection / role escape / blast_radius bypass） |

## 已落地（recent shipped）

- ✅ 7 Worker Skill + Manager 决策表（含 L0–L3 SafetyLevel）
- ✅ Nacos Skill Registry（HTTP 2.x Config API，本地降级 fallback，30s polling 热加载）
- ✅ append-only 审计 ledger（HMAC chain hash，每日 ndjson，可选 Nacos 历史版本同步）
- ✅ 真实环境验证 harness（3 剧本：`alert_storm` / `rca_loop` / `recovery_verify`，含 docker-compose 一键跑）
- ✅ Plugin stdio MCP server（Bearer + HMAC + W3C traceparent 透传）
- ✅ 多语言 README（英 / 中 / 日 / 韩 / 西 / 法 / 德 / 葡 / 俄）
- ✅ `CONTRIBUTING.md` + `CODE_OF_CONDUCT.md`

## 近期计划（next quarters，公开项）

### Q3 2026（产品发布与验证）

- ☐ 公开源码发布与干净环境构建验证
- ☐ 产品化 demo 视频（≤ 3 min，剧本：连接池耗尽 → 自动 RCA → 受批恢复 → 自动复盘）
- ☐ AgentLoop / LoongSuite OTLP adapter（环境变量切换 ingest endpoint）
- ☐ 安全 red-team 4 类攻击剧本（tool injection / role escape / blast_radius bypass / replan loop）

### Q4 2026

- ☐ Aliyun Skills 适配：把 `sre-aliyun-mcp` / `ack-mcp` / `arms-mcp` 直接挂为 plugin backend MCP tool group
- ☐ 跨云迁移模板 v1：金融核心交易场景（强一致 + 5 个 blast_radius 等级）；SaaS 多租户场景（按租户粒度灰度）
- ☐ LLM-as-evaluator Skill 评估机制（基于 8-case 黄金集）
- ☐ Manager dispatch 双轨：规则集 + LLM 仲裁拆分（`prompts/manager/manager_judge.md`）

### Q1 2027（远景）

- ☐ 多 LLM 后端支持（vLLM / OpenAI / 阿里云 DashScope / Anthropic 共存）
- ☐ AgentTeams `awesome-plugins` 生态清单 PR
- ☐ Kubernetes Operator（CRD 化部署 opskeeper + plugin）
- ☐ Skill marketplace 公开版（基于 Nacos Config + Web UI）

## 不在公开路线图（社区外项目）

- ❌ 内部代号 / 客户代号 / 商业合同相关 timeline
- ❌ 内部 SRE 值班流程（仅 opskeeper-teamharness 决策表）
- ❌ 内部 SLO 数字 / 商业指标

## 如何参与

- 提交 issue：bug / feature request / docs 改进
- 提交 PR：参考 `CONTRIBUTING.md`（含加 Skill 流程）
- 安全问题：邮件（地址见 `CONTRIBUTING.md` 末尾），不要在公开 issue 暴露
- 讨论：Telegram / Slack 群（见 README 顶部 badge）

## 反馈

路线图如有遗漏或调整建议，欢迎 issue / PR。本文件会随版本迭代同步更新。
