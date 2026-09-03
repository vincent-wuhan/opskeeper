# 运维手册：opskeeper 部署 / 运行 / 监控 / 应急

> **面向**：opskeeper 平台运维、SRE、DevOps
> **覆盖**：安装（Helm / install.sh）、运行（Dashboard / IM）、审计、应急
> **关联**：
> - 集成指南：[docs/integration-guide.md](integration-guide.md)
> - Helm chart：[deploy/helm/README.md](../deploy/helm/README.md)
> - 落地计划：[docs/superpowers/plans/2026-07-13-unified-platform-path-a.md](superpowers/plans/2026-07-13-unified-platform-path-a.md)

---

## 一、部署

### 1.1 快速启动（一键脚本）

```bash
# Ubuntu 22.04+ / Debian 12+ / RHEL/Rocky 9
curl -fsSL https://get.opskeeper.io/install.sh | bash -s -- \
  --domain ops.example.com \
  --admin-email admin@example.com \
  --llm-provider anthropic \
  --llm-key "$ANTHROPIC_API_KEY"
```

脚本自动完成：
- Docker + Compose 安装
- opskeeper-manager / opskeeper-edge / Web / Prometheus / Loki / Tempo / Grafana 拉起
- TLS 自签证书（或 Let's Encrypt 自动化）
- 默认 admin 账号创建
- 健康检查 + IM webhook 配置

### 1.2 Helm chart（生产推荐）

> 完整 K8s 部署指南（含 minikube 测试、HA 配置、NetworkPolicy、CI/CD 集成等）见
> [docs/deployment/k8s-install.md](../deployment/k8s-install.md)。本节仅给出最小可用片段。

```bash
# 1. 添加 helm repo
helm repo add opskeeper https://charts.opskeeper.io
helm repo update

# 2. 创建 values.yaml
cat > values.yaml <<'EOF'
domain: ops.example.com
image:
  tag: v1.0.0
ingress:
  enabled: true
  className: nginx
  tls:
    - secretName: opskeeper-tls
      hosts:
        - ops.example.com
persistence:
  size: 100Gi
  storageClass: gp3
llm:
  primary:
    provider: anthropic
    apiKey: "$ANTHROPIC_API_KEY"
  secondary:
    provider: openai
    apiKey: "$OPENAI_API_KEY"
harness:
  enabled: true
  judge:
    models:
      - claude-sonnet-4
      - gpt-4o
observability:
  prometheus:
    retention: 30d
  loki:
    retention: 30d
EOF

# 3. 安装
helm install opskeeper opskeeper/opskeeper \
  --namespace opskeeper --create-namespace \
  --values values.yaml

# 4. 验证
helm status opskeeper -n opskeeper
kubectl get pods -n opskeeper
```

### 1.3 升级

```bash
# 1. 备份数据库（前置）
opskeeper-migrate backup --output backup-$(date +%Y%m%d).sql

# 2. 升级 helm
helm upgrade opskeeper opskeeper/opskeeper \
  --namespace opskeeper \
  --values values.yaml \
  --set image.tag=v1.1.0

# 3. 运行 migration
kubectl exec -n opskeeper deploy/opskeeper-manager -- \
  opskeeper-manager --migrate up

# 4. 验证
helm test opskeeper -n opskeeper
```

### 1.4 回滚

```bash
# Helm 回滚到上一个 release
helm history opskeeper -n opskeeper
helm rollback opskeeper <REVISION> -n opskeeper

# 数据库回滚（若 migration 已执行）
opskeeper-migrate rollback --to backup-20260713.sql
```

---

## 二、运行

### 2.1 Dashboard 入口

- 主控台：https://ops.example.com/dashboard
- Web SSH：https://ops.example.com/edge
- Workflow Builder：https://ops.example.com/workflow
- Knowledge：https://ops.example.com/knowledge
- Harness Leaderboard：https://ops.example.com/harness

### 2.2 IM 双向集成

支持 5 平台双向（每平台独立配置）：

| 平台 | 启用 | Webhook URL 格式 | 关键配置 |
|---|---|---|---|
| Slack | Web → Settings → IM → Slack | `/api/v1/im/slack/events` | Signing Secret + Bot Token |
| Telegram | Web → Settings → IM → Telegram | `/api/v1/im/telegram/webhook` | Bot Token + Webhook 注册 |
| 飞书 | Web → Settings → IM → Lark | `/api/v1/im/lark/events` | App ID + App Secret + Encrypt Key |
| 钉钉 | Web → Settings → IM → DingTalk | `/api/v1/im/dingtalk/callback` | AppKey + AppSecret + Robot Code |
| 企业微信 | Web → Settings → IM → WeCom | `/api/v1/im/wecom/callback` | CorpID + AgentID + Secret |

每个平台支持：
- **双向**：用户发消息 → Agent 响应；Agent 主动推送 → 用户接收
- **每通道 locale**：中文 / 英文 / 日文 / 韩文 等
- **审批回调**：Casbin 审批工单 → IM 推送 → 用户点 "approve / reject" → 回写 opskeeper

### 2.3 关键服务组件

| 组件 | 端口 | 健康检查 | 日志 |
|---|---|---|---|
| opskeeper-manager | 8080 | `GET /healthz` | `kubectl logs -n opskeeper deploy/opskeeper-manager` |
| opskeeper-edge | 9443 | `GET /healthz` | 同上 |
| Web (nginx) | 443 | `GET /healthz` | 同上 |
| Prometheus | 9090 | `GET /-/healthy` | - |
| Loki | 3100 | `GET /ready` | - |
| Tempo | 3200 | `GET /ready` | - |
| Grafana | 3000 | `GET /api/health` | - |

### 2.4 资源占用基线（v1.0）

| 组件 | CPU 请求 | 内存请求 | CPU 限制 | 内存限制 |
|---|---|---|---|---|
| opskeeper-manager | 500m | 1Gi | 2 | 4Gi |
| opskeeper-edge | 100m | 256Mi | 500m | 512Mi |
| Web | 100m | 128Mi | 500m | 512Mi |
| Prometheus | 200m | 512Mi | 1 | 2Gi |
| Loki | 100m | 256Mi | 500m | 1Gi |
| Tempo | 100m | 256Mi | 500m | 1Gi |
| Grafana | 100m | 256Mi | 500m | 512Mi |

---

## 三、监控与告警

### 3.1 Prometheus 指标

opskeeper 暴露以下核心指标（`/metrics`）：

```
# Agent / Coordinator
opskeeper_agent_invocations_total{agent, status}
opskeeper_agent_duration_seconds{agent, status} histogram

# Specialist
opskeeper_specialist_invocations_total{specialist, status}
opskeeper_specialist_duration_seconds{specialist} histogram

# Tool
opskeeper_tool_invocations_total{tool, status}
opskeeper_tool_duration_seconds{tool} histogram

# Investigator (RCA)
opskeeper_investigator_duration_seconds{status=ready|failed} histogram
  buckets=[30, 60, 90, 120, 180, 300]

# Edge / Tunnel
opskeeper_edge_connected{edge_id}
opskeeper_edge_bytes_sent_total{edge_id}
opskeeper_edge_bytes_received_total{edge_id}

# IM
opskeeper_im_messages_total{platform, direction}

# Harness
opskeeper_harness_score{case_id} gauge
opskeeper_harness_judge_duration_seconds{model} histogram

# git-artifact
opskeeper_git_artifact_indexed_total{repo}
opskeeper_git_artifact_lookup_duration_seconds histogram
```

### 3.2 Grafana 仪表盘

预置 4 个仪表盘（Helm chart 自动导入）：

1. **Platform Overview**：CPU / 内存 / 请求量 / 错误率
2. **Agent Performance**：P50 / P95 延迟、成功率、Specialist 派发
3. **Investigator SLA**：P95 < 2min 达成率、ready/failed 分布
4. **Harness Leaderboard**：评分趋势、回归基线

### 3.3 Alertmanager 规则

```yaml
# deploy/helm/templates/alertmanager-rules.yaml
groups:
  - name: opskeeper-platform
    rules:
      - alert: InvestigatorSLABreach
        expr: histogram_quantile(0.95, opskeeper_investigator_duration_seconds) > 120
        for: 5m
        labels:
          severity: warning
        annotations:
          summary: "Investigator P95 超过 2 分钟"
          runbook: "https://wiki.opskeeper.io/runbook/investigator-sla"

      - alert: EdgeDisconnected
        expr: opskeeper_edge_connected == 0
        for: 2m
        labels:
          severity: critical
        annotations:
          summary: "所有 Edge 失联"
          runbook: "https://wiki.opskeeper.io/runbook/edge-disconnected"

      - alert: HarnessScoreDrop
        expr: opskeeper_harness_score < opskeeper_harness_score_baseline * 0.95
        for: 1h
        labels:
          severity: warning
        annotations:
          summary: "Harness 评分下降 > 5%"
```

---

## 四、审计

### 4.1 审计范围

所有**对外动作**必审计：

| 类别 | 记录字段 |
|---|---|
| 用户操作 | user_id, tenant_id, action, target, timestamp |
| Agent 决策 | session_id, coordinator_input, specialist_picks, tools_called, final_output |
| Edge 执行 | edge_id, host, command, exit_code, stdout/stderr 摘要 |
| IM 消息 | platform, channel_id, user_id, message_id, trace_id |
| Adapter 操作 | adapter_type, resource_id, operation, sql_or_cmd, approval_ticket_id |
| Approval 工单 | ticket_id, requester, approver, action, payload_preview |

### 4.2 审计日志查询

```bash
# via Loki
logcli query '{app="opskeeper-manager"} |= "audit"' --since=1h

# via Grafana
# Explore → Loki → {app="opskeeper-manager"} | json | action="kill_session"

# 导出某用户所有操作
logcli query '{app="opskeeper-manager"} | json | user_id="42"' --since=24h --output=json
```

### 4.3 合规导出

```bash
# 导出最近 90 天审计日志（用于 SOC2 / ISO27001 审计）
opskeeper-audit export \
  --since 90d \
  --output audit-export-2026-07-13.csv \
  --format csv
```

---

## 五、应急响应 Runbook

### 5.1 Edge 全失联

**症状**：所有 opskeeper-edge 状态 offline，Agent 无法执行命令。

**排查**：
1. 检查 manager → edge 隧道：`kubectl logs -n opskeeper deploy/opskeeper-manager | grep edge`
2. 检查网络：是否能连到 manager:9443
3. 检查 edge 进程：`systemctl status opskeeper-edge`

**恢复**：
```bash
# 重启 edge（自动重连）
systemctl restart opskeeper-edge

# 若仍失败，临时禁用 tunnel 走 SSH fallback
opskeeper-edge --fallback-ssh --ssh-host ops-jumpbox
```

### 5.2 Investigator SLA 持续超时

**症状**：`opskeeper_investigator_duration_seconds{status="ready"}` P95 > 5min。

**排查**：
1. 看 Grafana "Investigator SLA" 仪表盘
2. 检查 LLM provider 延迟（`opskeeper_llm_request_duration_seconds`）
3. 检查因果链长度（`opskeeper_causal_chain_nodes_count`）

**缓解**：
```bash
# 临时关闭 git-artifact 反查（慢查询场景）
opskeeper-feature-flag set investigator.git_artifact_enabled false

# 增加 judge 模型超时
helm upgrade opskeeper opskeeper/opskeeper --set investigator.timeout=300s
```

### 5.3 Harness 评分下降

**症状**：CI 中 Harness 评分对比基线下降 > 5%。

**排查**：
1. 查看 leaderboard diff：哪些 case 评分降低
2. 检查 judge 模型是否更换 / 限速
3. 检查 golden case 是否过期（中间件版本升级）

**恢复**：
```bash
# 回滚 judge 模型到上一个稳定版本
helm upgrade opskeeper opskeeper/opskeeper \
  --set harness.judge.models[0]=claude-sonnet-4-20250514

# 锁定 golden case 不更新
opskeeper-eval lock --until 2026-08-01
```

### 5.4 数据迁移失败

**症状**：`opskeeper-migrate import` 中途失败。

**回滚**：
```bash
# 自动 rollback snapshot（导入前生成）
opskeeper-migrate rollback \
  --rollback-snapshot rollback-snapshot-2026-07-13T10-30-00.json \
  --target opskeeper://opskeeper-host:8080
```

### 5.5 全量事故

**P0 / P1 事故必须产出 blameless postmortem**：

1. 24h 内：临时止损（关功能 / 回滚）
2. 72h 内：根因分析 + 短期 mitigation
3. 7d 内：blameless postmortem（无指责，关注系统改进）
4. 30d 内：长期 fix 实施 + 验证

模板：[docs/postmortem-template.md](postmortem-template.md)（Task 3.8 产出）

---

## 六、安全基线

### 6.1 凭据管理

- **禁止** LLM prompt / 日志 / 镜像中明文存储密钥
- **强制** opskeeper secrets 库加密（adapter DSN、IM token、LLM key）
- **强制** 多租户隔离（`tenant_id` 必填 SQL 过滤）

### 6.2 网络

- **零入站端口**（Edge 反向隧道）
- **TLS 1.3 强制**（自签或 Let's Encrypt）
- **RBAC**（Casbin）控制 API 访问

### 6.3 漏洞扫描

CI 包含：
- `govulncheck ./...`（Go 漏洞扫描）
- Trivy 镜像扫描（`deploy/Dockerfile.*`）
- Dependabot 自动 PR（每周一）

---

## 七、相关文档

- 集成指南：[docs/integration-guide.md](integration-guide.md)
- Harness 评测指南：[docs/harness-guide.md](harness-guide.md)
- API 文档：[docs/api/](api/)
- Helm chart README：[deploy/helm/README.md](../deploy/helm/README.md)
- Edge 安装：[docs/install/edge.md](install/edge.md)
- K8s 部署：[docs/deployment/k8s-install.md](deployment/k8s-install.md)
- E2E 测试目录：[docs/test/e2e-catalog.md](test/e2e-catalog.md)
- Workflow 目录：[docs/workflow-catalog.md](workflow-catalog.md)
