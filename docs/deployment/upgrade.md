# 升级策略

> Platform-base-ha：零停机滚动升级 / 蓝绿 / 金丝雀

## 升级到 2026.11.0 之前必读（本版本的破坏性变更与环境面变化）

### BREAKING：notifier 旧渠道 map 已删除

`core/base/pkg/notify` 的旧 `channelFactories` map 与回落路径已删除
（变更 `notifier-legacy-map-removal`，PR #131）。`BuildChannel`
只读启动期安装的渠道视图（`ChannelResolver`）：

- **视图未安装的进程对任何渠道类型（含 webhook 等六个内置）一律返回
  `unknown channel type`**——此前该状态回落旧 map 可建六渠道。
- 导出符号 `RegisterChannel` 与 `RegisteredChannels` 已删除（零生产
  调用者）。以库形式 import 本包且不经装配根装视图的下游，须改为自装
  `ChannelResolver` 视图（实现 `Factory(typ string) (ChannelFactory,
  bool)` 并在启动期调用 `notify.InstallChannelResolver`）。
- 仓库内生产路径不受影响：manager 装配序已同步修正——
  `notifybiz.Wire(ph.Kernel(), notifybiz.Builtins(), ...)` 先装视图，
  `notify.NewFromConfig`（及其首个消费者 `alertSvc`）移到 `Wire`
  之后；渠道激活失败是**启动失败**（fatal），不是六个渠道悄悄变五个。
- 该视图是进程内渠道工厂的**唯一**来源，且是启动期快照：运行期停用
  一个插件包不会改变已装视图的回答；改渠道集合 = 改部署 + 重启。
  回滚此行为 = revert 删除该 map 的那个切片，不存在「恢复到迁移前
  注册表」的开关。
- 错误文本 `unknown channel type %q` / `built an unnamed sender`
  一字未改——依赖错误文本做判断的脚本可以继续用。

**升级自检**：升级后在 manager 日志确认 notification channel activation
无 Error；对一个配置了通知渠道的环境发一次测试通知。若你的部署里
有独立编译的进程 import 了 `core/base/pkg/notify` 并直接调
`BuildChannel`，升级前先给它装视图，否则它的一切通知都会变
`unknown channel type`。

### Compose 侧环境变量与挂载变化

以下变化已在 `deploy/install/docker-compose.yml` 与
`deploy/install/state-dirs.sh` 落位，**自管 compose 文件的部署需要
手工对齐**：

| 项 | 内容 | 不对齐的后果 |
|---|---|---|
| `OPSKEEPER_VECTOR_BACKEND` | compose 默认 `qdrant`（`core/floor/config` 的裸默认仍是 `chromem`）。生产数据目录不可写时 chromem 落盘失败会 crash-loop（rc.1 部署实测）；且沿用既有 qdrant 数据，不是新建空库 | 升级后向量化写入失败或向量数据「消失」（其实在另一后端） |
| runtime-tasks 挂载 | `- ${OPSKEEPER_DATA_DIR:-/var/lib/opskeeper}/runtime-tasks:/var/lib/opskeeper/runtime-tasks`，宿主目录属主 `65532:65532`（`state-dirs.sh` 已含该条目） | 进程内诊断 worker 的 `<task-id>.json` 快照随容器删除而丢失，DB 里的 `TaskResult.output_ref` 变成任务列表会照常吐出的悬空引用 |
| `OPSKEEPER_RUNTIME_TASK_WORKERS` | 逗号分隔 worker 闭集：`manager`（诊断 worker + repair-preview）/ `agentteams`（只读报告桥）。**空 = 全关（默认部署零行为）** | 不配置就没有进程内 worker；任务停在 admitted，需要外部 runtime 认领 |
| `OPSKEEPER_RUNTIME_TASK_OUTPUT_DIR` | 默认 `/var/lib/opskeeper/runtime-tasks`；改这个 env 就要同步改挂载点 | 同上一行挂载项 |
| `OPSKEEPER_RUNTIME_TASK_SERVER_LIMIT` | 全局 in-flight 上限，默认 64，装配期读一次 | 不设即 64，一般无需动 |
| `OPSKEEPER_PIG_RUNTIME_*` | 双闸 opt-in：`_ENABLED=true` 且 `_ARTIFACT`+`_ARTIFACT_DIGEST`（SHA-256）+`_ID` 三件齐备才装配 PiG 驱动；显式启用但工件缺失/摘要不匹配是**启动错误**（fail closed） | 默认全空 = 不建进程、不写注册表、零行为；配一半 = 起不来（这是刻意的） |
| `OPSKEEPER_RUNTIME_TASK_TENANT` | agentteams 只读报告桥的租户钉扎；不设则桥不认领跨租户任务 | 桥静默不工作，任务留在队列 |

### 数据库迁移

本版本 migrator 计数 25 → 27，全部纯增量建表，无列变更：

- `runtimemgmt_runtimes`（runtime 注册表，§1.1）
- 任务生命周期八表：`runtimemgmt_tasks/_claims/_leases/_checkpoints/
  _attempt_records/_task_results/_admission_decisions/
  _idempotency_contracts`（§2.2+§2.3）
- `runtimemgmt_pig_tool_calls`（PiG 治理工具调用证据行，§3.3）

回滚到旧版本时这些表残留无害（旧代码不读）；无需手工 drop。

## 滚动升级（推荐）

opskeeper HA 部署默认支持零停机滚动升级。K8s 的 `maxUnavailable=0` + `maxSurge=1` 配合 `/readyz` probe 确保升级期间始终有可用副本。

### 操作

```bash
helm upgrade opskeeper ./deploy/helm \
  --set manager.image.tag=v1.1.0
```

### 升级序列（每个副本）

1. K8s 启动新副本，等待 `/readyz` = 200
2. 新副本完成 leader 注册（等 leader 锁可用）
3. K8s 发送 SIGTERM 到旧副本
4. 旧副本 MarkDraining → `/readyz` = 503
5. K8s 从 Service endpoints 摘除旧副本
6. 旧副本 HTTP drain（30s）→ ResignAll（25s）→ 关闭 DB/Redis
7. 旧副本退出

### 验证零停机

```bash
# 升级期间持续 curl（应全程 200）
while true; do
  curl -s -o /dev/null -w "%{http_code}\n" http://opskeeper-manager:8080/healthz
  sleep 1
done
```

### K8s Deployment 策略

```yaml
strategy:
  type: RollingUpdate
  rollingUpdate:
    maxUnavailable: 0    # 不允许同时减少可用副本
    maxSurge: 1          # 允许临时多 1 个副本
```

## 蓝绿部署

```bash
# 1. 部署新版本（green）
helm install opskeeper-green ./deploy/helm \
  --set manager.image.tag=v2.0.0

# 2. 验证 green 副本健康
kubectl get pods -l app.kubernetes.io/instance=opskeeper-green

# 3. 切换 Service selector
kubectl patch svc opskeeper-manager -p \
  '{"spec":{"selector":{"app.kubernetes.io/instance":"opskeeper-green"}}}'

# 4. 观察流量切换，确认无误后删除 blue
helm uninstall opskeeper-blue
```

## 金丝雀发布

```bash
# 1. 主版本保持不变
helm install opskeeper ./deploy/helm --set manager.image.tag=v1.0.0

# 2. 金丝雀副本（10% 流量）
helm install opskeeper-canary ./deploy/helm \
  --set manager.image.tag=v2.0.0 \
  --set manager.replicaCount=1

# 3. 通过 Ingress 权重或 Service Mesh 路由 10% 流量到 canary
# 4. 观察 30 分钟后决定全量或回滚
```

## 回滚

```bash
# Helm 回滚到上一版本
helm rollback opskeeper 0

# 或指定版本
helm rollback opskeeper <REVISION_NUMBER>

# 查看历史版本
helm history opskeeper
```

## 升级前检查清单

- [ ] `go test -race ./...` 全通过
- [ ] `helm lint ./deploy/helm` 通过
- [ ] DB migration 已在 staging 验证（expand-contract）
- [ ] Redis 兼容性确认（版本 + maxmemory policy）
- [ ] terminationGracePeriodSeconds ≥ 60s
- [ ] PDB minAvailable ≥ 1
- [ ] 至少 2 个节点满足 podAntiAffinity
- [ ] 自管 compose：`OPSKEEPER_VECTOR_BACKEND` 已显式设为部署实际后端
- [ ] 自管 compose：runtime-tasks 挂载与 `state-dirs.sh` 属主（65532:65532）已对齐
- [ ] 有独立进程 import `core/base/pkg/notify` 的部署：该进程已自装 `ChannelResolver` 视图（见上文 BREAKING 节）
- [ ] 不打算启用 PiG 子进程运行时：确认未设置任何 `OPSKEEPER_PIG_RUNTIME_*`（设置了配不齐会起不来）
- [ ] 启用进程内任务 worker：`OPSKEEPER_RUNTIME_TASK_WORKERS` 与
      `OPSKEEPER_RUNTIME_TASK_TENANT`（agentteams 桥）已按角色配置
