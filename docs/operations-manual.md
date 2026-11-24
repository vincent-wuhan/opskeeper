# 运维手册：opskeeper 部署 / 运行 / 监控 / 应急

> **面向**：opskeeper 平台运维、SRE、DevOps
> **覆盖**：安装（Helm / install.sh）、运行（Dashboard / IM）、审计、应急
> **关联**：
> - 集成指南：[docs/integration-guide.md](integration-guide.md)
> - Helm chart：[deploy/helm/README.md](../deploy/helm/README.md)
> - 升级（含破坏性变更与环境面变化）：[docs/deployment/upgrade.md](deployment/upgrade.md)
> - 模块架构：[docs/module-architecture.md](module-architecture.md)

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
opskeeper-migrate export --output backup-$(date +%Y%m%d).json

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
opskeeper-migrate rollback \
  --rollback-snapshot backup-20260713.json \
  --target opskeeper://opskeeper-host:8080
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

### 2.5 闭环修复动作的适配器装配

approved phase 要把一个修复动作真正打到工具上，需要该动作所属的中间件适配器
被连上。六个适配器各自独立，按环境变量装配到同一个工具 registry；**没配 DSN
的适配器只是不出现**（对应的动作会被明确拒绝并说明原因，不会静默跳过或猜参数）。
配了但连不上的会被记录并跳过，不会阻止 manager 启动。

| 环境变量 | 适配器 | 工具数 | DSN 形态 |
|---|---|---|---|
| `OPSKEEPER_LOOP_PG_DSN` | PostgreSQL | 22 | `postgres://user:pass@host:5432/db` |
| `OPSKEEPER_LOOP_REDIS_DSN` | Redis | 17 | `redis://[:pass@]host:6379/0`（cluster 亦可） |
| `OPSKEEPER_LOOP_K8S_DSN` | Kubernetes | 19 | `kubeconfig:///path/to/kubeconfig`、`incluster://`、`https://host:port?token=...` |
| `OPSKEEPER_LOOP_MQ_DSN` | 消息队列 | 6 | `amqp(s)://`（RabbitMQ，走 management API）或 `kafka://`（broker 列表） |
| `OPSKEEPER_LOOP_HOST_DSN` | 主机 | 7 | `local://` 或 `ssh://user@host[:port]?key=/path/to/key` |
| `OPSKEEPER_LOOP_GIT_DSN` | Git 仓库 | 8 | `/path/to/checkout`、`/path/to/checkout#branch`、`https://host/owner/repo.git`、`git@host:owner/repo.git` |

Git 适配器**只读**：`git.connect` / `list_repos` / `commit_history` /
`file_at_commit` / `blame` / `diff` / `search_code` / `find_runtime_link`。
`Execute` 即使收到 `approved_by` 也会拒绝全部 `push` / `tag` / `reset`——
平台没有经过审批闸门的 git 写路径，一个只看"审批人字段非空"就执行 push 的
入口比没有入口更危险。远端 DSN 会 clone 到临时目录（**全量 fetch，不是
`--depth=1`**：浅克隆会让 `git blame` 把每一行都归给切点），`Close` 时删除；
本地路径就地读取，不会被清理。

主机适配器的写操作还有一层独立的闸门：`OPSKEEPER_HOST_UNIT_ALLOWLIST`
（逗号分隔的 systemd unit 名单）。**它为空时拒绝所有重启请求**，不是允许所有——
这是一个需要显式配置才能打开的开关。

DSN 支持 `secret://` 前缀（`core/manager/pkg/secretbox`），也接受明文（兼容读取）。

检查装配结果：manager 启动日志里每个成功的适配器各有一行
`loop: remediation adapter wired`；一个都没连上时是一条 `Warn`，并附上可用变量名。

### 2.6 Runtime 任务生命周期（/v1/runtime-tasks）

控制面提供一套跨 runtime 共享的任务生命周期：诊断、修复预览、AgentTeams
报告桥都走同一状态机。运维视角的关键事实：

**六类 runtime 注册闭集**：`node_edge`（边端节点代理）、`chat_worker`
（manager 内聊天工作器）、`agentteams_worker`（团队工作器）、
`plugin_manager`（TeamHarness/插件宿主）、`pool_preview`（一次性预览
执行面）、`pig`（治理下的 PiG 子进程运行时，见 §2.7）。

**十二条路由**（前缀 `/v1/runtime-tasks`；`/v1/tasks` 已被 report 包占用）：

```
POST /                      入队（内联准入）
GET  /                      列表
GET  /{id}                  详情
POST /{id}/claim            认领（单条条件 UPDATE，并发双认领恰一成功）
POST /{id}/lease-renewal    续租
POST /{id}/start            开始
POST /{id}/checkpoint       检查点
POST /{id}/progress         进度
POST /{id}/result           结果
POST /{id}/failure          失败
POST /{id}/cancel           取消
POST /stale-recovery        陈旧恢复（租约过期的认领行接管或终态）
```

**准入三闸**（入队时内联评估；无 selector 的任务只过 server limit 单闸，
认领时还有第二道懒重评）：

1. server limit：全局 in-flight 达到 `OPSKEEPER_RUNTIME_TASK_SERVER_LIMIT`
   （默认 64）→ 拒绝 `no_capacity`。
2. runtime 健康：认领者健康不得为 `offline` 或 `recently_lost` → 拒绝
   `runtime_not_ready`。
3. 槽位：runtime 投影空闲槽 `ProjectedFreeSlots > 0` → 否则拒绝
   `no_capacity`。

**幂等双闸**：mutating 任务必须有幂等契约行——入队时缺契约 fail-fast；
认领时缺契约是终态失败 `no_idempotency_contract`，零重试。

**认领/续租纪律**：认领与终态写入全部是单条条件 UPDATE（state 前置），
零 read-modify-write；runtime 代际不匹配返回 `ErrClaimLost`，旧代际的
心跳/进度会被拒收。陈旧恢复按安全级别分类：可重试失败
（`IsRetryable ∧ HasRetryBudget ∧ 指纹匹配`）回 `admitted` 重排队，
否则终态 `failed`（失败类别 `lease_expired` / `runtime_lost`）。
失败接管矩阵保证重启 / failover 下不重复执行只读与一次性预览工作。

**环境变量**：

| 变量 | 默认 | 语义 |
|---|---|---|
| `OPSKEEPER_RUNTIME_TASK_WORKERS` | 空（全关） | 逗号分隔：`manager` = 诊断 worker + repair-preview；`agentteams` = 只读报告桥。默认部署零行为 |
| `OPSKEEPER_RUNTIME_TASK_TENANT` | 空 | agentteams 桥的租户钉扎；不设则桥不认领任务 |
| `OPSKEEPER_RUNTIME_TASK_OUTPUT_DIR` | `/var/lib/opskeeper/runtime-tasks` | 诊断 worker 输出落盘目录；**改 env 必须同步改挂载**（DB 里的 `output_ref` 是绝对路径，容器忘了快照就是悬空引用） |
| `OPSKEEPER_RUNTIME_TASK_SERVER_LIMIT` | 64 | 全局 in-flight 上限，装配期读一次 |

**排查**：任务长期停在 `admitted` → 检查 worker env 是否配置、runtime
是否健康（`/api/v1/runtimes`）、槽位是否为 0。任务反复被接管 → 看失败
类别与 attempt 行，认领者心跳是否在租约窗口内。

### 2.7 PiG 子进程运行时（opt-in，默认零行为）

PiG 作为第六类 runtime 接入控制面治理。默认（零环境变量）不建进程、
不写注册表、零行为——这是**双闸 opt-in**：

```
OPSKEEPER_PIG_RUNTIME_ENABLED=true          # 显式开启
OPSKEEPER_PIG_RUNTIME_ARTIFACT=<路径>       # 固定二进制工件
OPSKEEPER_PIG_RUNTIME_ARTIFACT_DIGEST=<sha256>
OPSKEEPER_PIG_RUNTIME_ID=<runtime id>
```

四件齐备才装配驱动；显式启用但工件缺失/摘要不匹配是**启动错误**
（fail closed）——一个自认启用了 PiG 却悄悄没有它的部署比启动失败更
危险。禁用方式 = 删掉全部四个 env，回落到既有 runtime 集合。

PiG 生命周期事件（启动、注册、心跳、进度、取消、崩溃、完成）经进程内
Store 端口归一到 runtime health 与任务事件——它是 Manager 子进程，
不走 HTTP 自注册（HTTP 自注册留给真远端 runtime）。

**三方工具对账**：declared（发布清单 CapabilityManifest）∩ observed
（运行期上报）∩ allowlisted（角色准入表）三方交集才暴露一个工具。
不在 manifest → 永不暴露、记 drift；在 manifest 不在角色 allowlist →
不暴露、记 drift；observed 缺席 declared → 记证据影响健康。
未批准的工具不会暴露给任何角色。

**七道闸治理工具网关**（Dispatch 之前依次执行，任一拒绝都先落 denied
调用行与审计行再返回——拒绝是留痕的事实，不是静默丢弃）：

1. 身份翻译（undeclared / duplicate / drifted 拦截）
2. tenant 闸（租户与快照一致）
3. role 闸（角色准入表命中该工具）
4. incident 闸（事件落在任务钉死的作用域内）
5. resource 闸（ResourceID 命中工具声明作用域）
6. read-only 硬闸（`ReadOnly=true` 发布里任何非 read 调用直接拒绝，
   **不进审批**——只读发布里的变更请求本身就是策略违规）
7. 限流（subject = tenant/role/worker 三元组）

**proposal-only**：write / destructive 调用在身份与作用域闸全过后
不直接分发，交给 proposal 台账待人工批准（proposalSink 接缝，nil 时
fail closed：不执行、不落 proposal）。每次调用持久化开始/结束、调用者、
release/capability digest、参数 SHA-256、结果状态、错误与 trace 关联
（表 `runtimemgmt_pig_tool_calls`）。

**排查**：启动即失败 → 检查四件 env 是否齐、digest 是否与工件一致。
工具不出现 → 看 drift 证据行（三方哪一方缺席）。调用被拒 → deny_reason
码回答死在哪道闸（`undeclared_tool`/`duplicate_tool`/`drifted_class`/
`tenant_mismatch`/`role_not_allowed`/`cross_incident`/`cross_resource`/
`read_only_hard_gate`/`rate_limited`/`ledger_write_failed`）。

### 2.8 出站通知渠道（notifier 扩展点）

六个内置渠道（webhook、slack、feishu、dingtalk、wecom、telegram）是
**预装插件**：编译进 manager 二进制，但经与第三方包相同的准入闸
（`notifybiz.Wire`，装配序在 `NewFromConfig` 之前）。渠道激活失败是
启动失败（fatal），不是悄悄少一个渠道。

升级注意（BREAKING）：`BuildChannel` 只读启动期安装的 `ChannelResolver`
视图——**视图未安装的进程对任何渠道类型一律返回 `unknown channel
type`**。独立编译且 import `core/base/pkg/notify` 的进程须自装视图；
详见升级手册。改渠道集合 = 改部署 + 重启，运行期没有第二个入口。

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

### 4.3 防篡改链（HMAC chain）

`audit_logs` 是一条**带密钥的哈希链**：第 N 行携带第 N-1 行的摘要，所以改动或
删除任何一行，都会让它之后所有行一起校验失败。链的密钥只在控制面（manager）里，
节点、插件、模型都只能读、不能续。

**开启（生产必做）**：

```bash
# 生成一次性密钥，写进 manager 的 Secret，不要进 git
export OPSKEEPER_AUDIT_HMAC_KEY=$(openssl rand -hex 32)
```

未配置时账本**照常记录**，只是行里没有摘要；manager 启动时会打一条
`audit: OPSKEEPER_AUDIT_HMAC_KEY is not set` 的 WARN。

**查看链状态**：

```bash
curl -H "Authorization: Bearer $TOKEN" \
  https://opskeeper.example.com/v1/admin/audit-logs/chain
```

```json
{
  "enabled": true,
  "intact": false,
  "head_seq": 184213,
  "anchor_seq": 91204,
  "broken_at_seq": 150337,
  "reason": "link mismatch: PrevHash is 9f2c… but the preceding entry hashes to 41ab…"
}
```

| 字段 | 含义 |
|---|---|
| `enabled` | 是否配置了 key。未配置时 `intact` 是 `null`，**不是** `true` |
| `intact` | `true` 全链通过；`false` 有断点；`null` 没有链 |
| `head_seq` | 最新一条链上记录的位置 |
| `anchor_seq` | 现存最老一条的位置。**低于它的已被保留策略裁掉**，所以「校验通过」只覆盖 `anchor_seq` 之后 |
| `broken_at_seq` | 第一个对不上的位置。从这里往后的记录都不可信 |

该接口恒返回 200——「有篡改」是一个答案，不是服务端错误。轮询它不会写审计行。

**保留策略只裁前缀**：链中段的行不会因为 `occurred_at` 早于 cutoff 被删。
这意味着**表里最老的一行可能比 cutoff 还老**——它前面有更新的行，删它会在链中间
挖洞。判定标准是 `anchor_seq`，不是行数。

**轮换密钥会让旧行的摘要全部失效**。轮换是一次保留级事件：要么先让旧窗口过期，
要么保留旧 key 以便按时间段分别校验。

### 4.4 合规导出

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

# 重跑 leaderboard，确认回滚后的评分已恢复
opskeeper-eval leaderboard
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

### 5.5 Runtime 任务全部停在 admitted

**症状**：`/v1/runtime-tasks` 列表里任务长期 `admitted`，无人认领。

**排查**：

1. worker 是否装配：`OPSKEEPER_RUNTIME_TASK_WORKERS` 是否按角色设置
   （空 = 零 worker，这是默认值，不是故障）。
2. runtime 是否健康：`GET /api/v1/runtimes`——`offline` / `recently_lost`
   状态的 runtime 不能认领（准入第二闸）。
3. 槽位是否为 0：认领者 `ProjectedFreeSlots <= 0` 时拒绝认领。
4. agentteams 桥是否钉了租户：`OPSKEEPER_RUNTIME_TASK_TENANT` 未设则
   桥不认领跨租户任务。

**恢复**：

```bash
# 手动触发一轮陈旧恢复（幂等，可重复调）
curl -X POST -H "Authorization: Bearer $ADMIN_TOKEN" \
  https://ops.example.com/v1/runtime-tasks/stale-recovery
```

可重试失败会回 `admitted` 重新排队；不可重试的会终态 `failed` 并带
失败类别（`lease_expired` / `runtime_lost`）。修好 runtime 健康后任务
自然被下一轮认领，不需要人工重放。

### 5.6 全量事故

**P0 / P1 事故必须产出 blameless postmortem**：

1. 24h 内：临时止损（关功能 / 回滚）
2. 72h 内：根因分析 + 短期 mitigation
3. 7d 内：blameless postmortem（无指责，关注系统改进）
4. 30d 内：长期 fix 实施 + 验证

模板：postmortem 文档（blameless 结构：时间线 / 影响面 / 根因 / 行动项，
无指责原则）。

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
- 升级手册（含破坏性变更）：[docs/deployment/upgrade.md](deployment/upgrade.md)
- E2E 测试目录：[docs/test/e2e-catalog.md](test/e2e-catalog.md)
- Workflow 目录：[docs/workflow-catalog.md](workflow-catalog.md)
