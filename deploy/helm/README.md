# OpsKeeper Helm Chart（兼容名 `opskeeper`）

OpsKeeper Helm chart — 集成中间件 Adapter + Harness 评测
+ git-artifact 反查 + 5 平台 IM + 可观测性栈。

## 快速开始

### 最小安装（仅 manager + edge agent + web）

```bash
helm install opskeeper ./deploy/helm
```

### 完整安装（manager + 可观测性栈）

```bash
helm dep update ./deploy/helm
helm install opskeeper ./deploy/helm \
  --set qdrant.enabled=true \
  --set prometheus.enabled=true \
  --set loki.enabled=true \
  --set tempo.enabled=true \
  --set grafana.enabled=true \
  --set grafana.adminPassword="YOUR_SECURE_PASSWORD"
```

## 路径 A 集成

chart 默认启用路径 A 继承的 3 个核心能力（通过 manager.config 控制）：

| 能力 | 字段 | 默认 | 说明 |
|------|------|------|------|
| Harness 评测 | `manager.config.enableHarness` | `true` | judge + leaderboard + 20 黄金事故 |
| 中间件 Adapter | `manager.config.enableMiddleware` | `true` | PG/Redis/MQ/K8s/Git 5 类 |
| git-artifact | `manager.config.enableGitArtifact` | `true` | Linker 4 类 + CI 回调 |

## 可观测性栈

5 个 chart 依赖均默认 disabled — 按需启用：

| 组件 | value 字段 | 用途 | 集成点 |
|------|------------|------|---------------|
| Qdrant | `qdrant.enabled` | 向量数据库 | 知识检索基础 |
| Prometheus | `prometheus.enabled` | 指标采集 | ServiceMonitor 自动 scrape manager |
| Loki | `loki.enabled` | 日志聚合 | manager JSON 日志收集 |
| Tempo | `tempo.enabled` | Trace 聚合 | OTel 端到端 trace |
| Grafana | `grafana.enabled` | 可视化 | 自动接入 Prom/Loki/Tempo 数据源 |

## Chart 版本对齐

| 项 | 值 | 说明 |
|------|------|------|
| Chart version | `1.0.0` | helm chart 自身版本 |
| App version | `v1.0.0` | 兼容期应用版本 |
| Go | `1.25+` | manager 二进制要求 |

## 与 ops-keeper 兼容

Chart value 命名遵循 ops-keeper helm/ 约定：
- `manager.config.enableXxx` 风格
- `qdrant/prometheus/loki/tempo/grafana.enabled` 风格
- 迁移用户零学习成本

## 验收

```bash
# 本地 dry-run
helm template opskeeper ./deploy/helm --set grafana.enabled=true

# 完整安装
helm install opskeeper ./deploy/helm --dry-run

# 验证资源
kubectl get all -l app.kubernetes.io/name=opskeeper
```

## 关联

- Design Doc: `docs/superpowers/specs/2026-07-13-unified-platform-path-a-design.md`
- Build Plan: `docs/superpowers/plans/2026-07-13-unified-platform-path-a.md` 任务 3.1
- 协议：`openspec/changes/unified-platform-base-selection/protocols/git-artifact-v0.md`
