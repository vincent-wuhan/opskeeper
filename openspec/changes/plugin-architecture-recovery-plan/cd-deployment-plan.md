# 6.x 附：tag → 生产环境自动更新 CD 方案（触发 / 信任链 / 凭据治理 / 灰度回滚）

> 状态：**方案产出，待用户裁决**。对应 `tasks.md` 第 6 节（P2 发布准备）的 CD 子方案，不替代 6.1–6.8 的任何验收项；6.2"新的 release tag 命名和签名流程"与 6.6"部署 / 迁移 / 回滚演练"落地时应引用本文的决策点 D1–D6。
>
> 基线：`main@bc054167`。本文只出方案与决策点；所有实现切片（§8）均需另开 change 承载，本任务不改任何一行代码、不改 `release.yml` / `ci.yml`、不动 `core/domains/*/systemupgrade`。
>
> 两个待澄清项按假设分支处理，不阻塞本文：生产形态（§5.1 H1 / H2）、可接受更新窗口（D4）。全文代码引用均为只读审计证据。

---

## 0. 结论摘要

| 问题 | 结论 | 依据 |
|---|---|---|
| 今天有没有任何自动部署路径 | **没有。** 四个 workflow 中 `deploy / aliyun / acr / ssh / scp / kube / restart` 零命中，`release.yml` 在 `gh release create` 处终止 | §2.1 |
| 生产更新现在靠什么 | 人工：登机 wget tarball + `sudo ./install.sh`，或产品内 systemupgrade 域"检查 + 复制命令"后半人工执行 | §2.2 / §2.3 |
| systemupgrade 能否被外部触发去部署 | **不能。** 它是只读检查器 + 升级文案生成器：仅 `GET/POST /v1/system/upgrade(/check)`，admin-only，无 apply 端点，不下载、不校验哈希、不落盘 | §2.3 |
| 推送式 vs 拉取式 | **推荐拉取式（生产侧轮询 latest.json + 验签后自升级）**，凭据不对称性与暴露面全面占优；推送式三变体作为正式对比候选保留在 §4 | D1 |
| 签名怎么做 | 复用仓内 pluginmanifest 已有的 ed25519 detached + TrustStore 语义，对 `latest.json`（含资产 sha256）签名；私钥只进 GitHub Environment secret | D2 |
| 灰度怎么做 | H1 单机：组件级分步 + 三层健康门 + 自动回滚；H2 多机：逐字段对齐 `pluginmanifest.PlanRollout` 的 rolling / canary / pin 波次语义 | §5 |
| 生产凭据要不要进 GitHub Secrets | **基线方案下不需要任何生产机密出生产**（这是选拉取式的核心原因）；仅签名私钥（发布凭据，非生产凭据）入 GitHub Environment | §6 / §7 |

一个贯穿全文的判断：**这个仓库已经把"安全的分发包升级"在 edge 侧做对了一遍**（fetch + sha256 校验 + staged 目录 + `.previous` 备份 + 健康标记 + 启动期自动回滚，§2.4.1），也把"签名与信任锚"在插件体系里做对了一遍（ed25519 detached + TrustStore + revoked，§2.4.2），还把"波次灰度"在 pluginmanifest 里做对了一遍（§2.4.3）。本方案不发明第四套机制，而是把这三件已有资产按"发布对象从插件包换成平台自身"平移重组。新引入的唯一新组件是生产侧一个拉取式升级执行器。

---

## 1. 目标与边界

### 1.1 目标

补上发布链路"产物就绪即止"的最后一环，设计 tag → 生产（阿里云）自动更新的 CD 方案：

1. **现状盘点**：发布链路、人工更新路径、产品内 systemupgrade 域能力边界（含代码证据）。
2. **触发与信任链**：tag（已有 VERSION 一致性校验）→ 产物签名 → 部署准入的完整链条；部署触发器凭据治理的方案对比（推送式三变体 vs 拉取式）。
3. **灰度与回滚**：单机 / 多机两分支的滚动策略、健康检查门、失败自动回退；与 pluginmanifest rollout 语义对齐。
4. **安全性论证**：凭据不落 CI 日志、通道最小暴露面、审计记录。
5. **实施切片建议**（独立 change 承载）与 secrets 配置清单。

### 1.2 非目标

- 不实现任何代码；不改 `release.yml` / `ci.yml` / `audit-open-source.yml` / `contribution-terms.yml`。
- 不改 `core/domains/server/systemupgrade`、`core/domains/service/systemupgrade` 的任何行为。
- 不做 6.x 的全量发布验收（那是 `tasks.md` 6.1–6.8 本体）。
- 不解决"构建可复现性"（当前构建含 frontier 指定 tag clone 与嵌入模型下载，非 bit-for-bit 可复现；列为已知限制 R7，不在本方案内收口）。

### 1.3 与既有文档的关系

| 文档 | 关系 |
|---|---|
| `tasks.md` 6.2（tag 命名与签名流程） | 本文 D2 是它的签名子方案；tag 命名沿用 `release.yml` 既有 `vMAJOR.MINOR.PATCH(-rcN)` 正则，不新造 |
| `tasks.md` 6.6（部署 / 迁移 / 回滚演练） | 本文 §5 / §8 的 Slice 2 演练是它的 CD 部分 |
| `docs/deployment/upgrade.md`（K8s 滚动 / 蓝绿） | 描述的是 Helm/K8s 形态的目标态；本文 H2 分支与其衔接，H1 分支（当前实际形态）是对它的补全而非替代 |
| `deploy/opskeeper-rollback-runbook.md` | 手工回滚总纲；本文 §5.3 的自动回滚只覆盖"新版本起不来"这一最小失败面，其余仍走该 runbook |
| `docs/deployment/ha.md`、`deploy/install/README.md` | 形态事实来源（§5.1） |

---

## 2. 现状盘点（代码证据）

### 2.1 发布链路：产物就绪即止

`.github/workflows/release.yml` 由 `push tag v*.*.*` 触发，两个 job：

- **build**（amd64/arm64 矩阵）：tag 正则校验（`v[0-9]+\.[0-9]+\.[0-9]+(-rc\.?[0-9]+)?`）→ `VERSION` 文件与 tag 一致性校验 → `make package` → `make version-check`（`RELEASE_VERSION.json` 两个树哈希对 HEAD 的断言，Makefile:102 注释说明这是发布期门槛）→ tarball sha256 自校验 → artifact 上传。
- **release**：重复同一 tag/VERSION 门 → 下载 artifact → 双架构 sha256 复验 → 生成 `latest.json`（version / tag_name / release_url / published_at / download_base / 双架构资产与 sha256 文件名）→ `gh release create` 挂 5 个资产。

**链路终点就是 GitHub Release。** 四个 workflow 全文检索 `deploy`、`aliyun`、`acr`、`ssh`、`scp`、`kube`、`restart`：零命中。不存在任何自动部署路径——这与任务背景描述一致，属于已核实事实而非推测。

信任链现状：tag → VERSION 一致（有）、树哈希对账（有）、sha256（有）；**签名（无）**、部署准入（无）、生产侧验签（无）。6.2 要求的"签名流程"今天在 release 资产层面是空白，这是信任链的第一缺口。

另一个已核实缺口：`latest.json` 的 `download_base` 写死为 `https://opskeeper.cloud/dl`，而资产实际挂在 GitHub Release 上。产品内升级检查（§2.3）把命令文案指向该 base——如果该镜像域名未实际就绪，生成的 `wget` 命令会失败。镜像可用性与资产真源的一致性需要在 Slice 1 里一并收口（要么镜像就绪并同步签名，要么 download_base 指回 GitHub Release）。

### 2.2 生产侧人工更新路径

`deploy/install/README.md`：两种安装形态——`--mode=compose`（默认，docker compose 起 mysql/prom/loki/tempo/qdrant/nginx/grafana/opskeeper）与 `--mode=systemd`（裸 systemd 单元）。目标主机单机，数据在宿主 bind mount（`/var/lib/opskeeper`、`/var/log/opskeeper`，`state-dirs.sh` 统一声明）。

`deploy/install/upgrade.sh`（446 行）的完整序列：

1. 读新 tarball 的 `VERSION`、`.env` 里的旧版本号；
2. `docker compose down`（**整栈停止，停机窗口开始**，upgrade.sh:166）；
3. 重放 host 数据目录声明与属主；
4. 覆盖 `docker-compose.yml` / `frontier.yaml` / `nginx.conf` / prometheus / edge / `VERSION`（**不动 `.env` 与 `certs/`**）；
5. `.env` 缺失键回填（密钥自动生成）；
6. `docker compose up -d`（upgrade.sh:330，gorm AutoMigrate 吸收 schema diff）；
7. 等 `https://localhost:${OPSKEEPER_HTTP_PORT}/healthz` 最多 90s（upgrade.sh:364-378）——**超时只告警不回滚**；
8. 健康后才清理：删旧解压目录、删旧 docker 镜像、**保留最近两个 tarball 供人工回滚**（upgrade.sh:398-428）。

人工路径的缺口，即 CD 化要补的洞：脚本不校验下载 tarball 的 sha256（依赖操作员手工执行 sidecar 校验）、无签名校验、健康失败无自动回滚（栈已 down、新版本未起来时处于不可用中间态，恢复靠人工）、无结构化审计（只有 stdout 日志）、停机窗口 = down→up 全间隙。

### 2.3 systemupgrade 域能力边界（重点审计）

产品内自升级域分三层，逐一给出边界结论。

**API 面（`core/domains/server/systemupgrade/http.go`）**

- 路由仅两条：`GET /v1/system/upgrade` 与 `POST /v1/system/upgrade/check`（http.go:32-33），**两者是同一个 handler `h.check`，都是只读检查**。
- 认证：挂在 `protected` 路由组（`cmd/opskeeper/main.go:3828`，JWT 租户上下文）之上，再叠 `requireAdmin`——非 `RoleAdmin` 一律 403（http.go:57-68）。
- **没有 apply / download / rollback 端点。** 结论：*systemupgrade 不能被任何外部 API 触发去执行部署*，它暴露的最大能力就是"返回一份最新版本信息 + 三条可复制的 shell 命令"。这个边界对 CD 方案是个优点（见 D6：保持它）。

**服务层（`core/domains/service/systemupgrade/service.go`）**

- 数据源：默认依次尝试 `https://opskeeper.cloud/dl/latest.json` → GitHub Releases API（service.go:18-19, 206-225），可配置覆盖；响应体限 1MiB，默认 5s 超时。
- 版本比较：三段整数比较（service.go:284-323）。**`parseVersion` 在首个 `+`/`-` 处截断**（service.go:308-310），即 `v1.2.3-rc1` 与 `v1.2.3` 视为相等——rc 通道语义在比较层不存在，发布 rc 时"有无更新"的判断会失真。CD 方案的 channel 概念（§3.4）需要意识到这一点：rc 与正式版同基线号时，现有比较器无法区分先后。
- 输出的 `Commands`：`curl -fL -O …tar.xz || wget …` + `tar xf` + `sudo ./upgrade.sh`（service.go:251-282）。**文案里没有 sha256 校验步骤，更没有验签**——操作员照抄执行的话，完整性依赖完全落在传输层 TLS 上。
- **不下载产物、不校验哈希、不执行升级、无灰度概念。**

**后台循环（`core/domains/service/systemupgrade/supervisor.go`）**

- leader-only：通过 `leader.Manager.Register(leader.Role("upgrade:checker"))` 接入选举（main.go:3955），单副本时内联启动；间隔 1 小时（main.go:2988-2991）。
- 一个值得指出的实现与注释的偏差：文件头注释说结果"broadcast to followers / 供 /api/v1/upgrade 读出缓存"（supervisor.go:4-8, 18-21），但 `tick` 实际丢弃 `Check` 的返回值、只留错误日志（supervisor.go:141-148），HTTP handler 每次请求都现场调 `svc.Check`。即 supervisor 目前只是元数据端点的定时探活，不是缓存。这不影响本方案（检查器本来就是只读的），但 6.x 收口文档时应修正注释或补缓存，避免后来者按注释推断存在推送能力。

**边界总结**：systemupgrade = 检查器 + 升级文案生成器。可被外部（admin 身份）触发的只有"检查"这个只读动作；认证边界清晰（admin-only）；灰度支持为零。它适合继续当"人看的状态页"，不适合当 CD 执行器（D6）。

### 2.4 既有可复用的机制资产（本方案的重组素材）

#### 2.4.1 edge 分发包升级链：拉取 + staged + 自动回滚的完整先例

manager 侧：`POST /v1/edges/{id}/upgrade-package`（`core/manager/server/edge/http.go:199`，`edge:*` 写权限）由 `PackageResolver` 从镜像内 baked bundle 自动解析 (url, sha256)，再经 tunnel RPC `FetchPackage(url, sha256, version)` → 确认后 `ApplyPackage`（http.go:90-97, 103-111）；nginx 以 `/edge-bundle/` 提供下载。

edge 侧：`deploy/install/apply-pending-upgrade.sh`（root oneshot unit，每次 agent 启动前运行）实现了完整的安全序列：

1. **回滚优先**：上次升级后从未出现健康标记 → 逐文件恢复 `<dest>.previous`，并清 `last_upgrade_at` 防回滚循环；
2. **staged apply**：`incoming/MANIFEST.txt` 存在 → 逐文件 sha256 校验 → 备份 `.previous` → 原子 rename 就位；
3. 单文件 legacy 路径兼容；
4. 校验期任何失败 exit 0 不阻塞启动（升级侧错误不代价服务可用性）。

这就是"拉取式 + 健康门 + 自动回滚"在仓内的既成事实标准。§5 的生产侧执行器直接按这个模式做，架构上是平移而非新设计。

#### 2.4.2 pluginmanifest 签名体系：信任锚的既成语义

- `TreeDigest`：长度前缀的规范化树摘要，"签名的对象先规范化"（`core/floor/pluginmanifest/digest.go:15-56`）；签名 sidecar 自身排除在树外（detached，digest.go:118-125）。
- ed25519 + `TrustStore`（key id → 公钥，支持 revoked 语义：被吊销的 key 报"revoked"而非"unknown"，trust.go:17-46）+ `Verify/VerifyDir`（trust.go:165-253）。
- 准入决策流里签名是第一步、`AllowUnsigned` 仅限显式声明的开发节点，且 Decision 带空 envelope 防止下游误认（review.go:195-224）。
- federation 接收侧对 sidecar 与 envelope 的 KeyID/TreeDigest 对账（`core/floor/federation/receiver.go:317-319`）——"两处独立声明必须一致"的先例。

release 资产签名直接复用这套语义：对 `latest.json` 规范化字节 + 资产 sha256 做 ed25519 detached 签名，公钥以 key id 进版本化 trust 文件。

#### 2.4.3 pluginmanifest rollout：波次灰度的既成语义

`core/floor/pluginmanifest/rollout.go`：

- 策略词表只有 `rolling` / `pin` 两个（`core/domain/plugin.go:154-161`），**canary 不是第三种策略，而是 rolling 的第一波**（`CanaryFractionTenths = 1`，不足一台补一台，rollout.go:33-41, 107-116）。
- 波宽 `WaveSize = 10`；`Advance` 的门是"当前波全部 confirmed **或** failed"，纯 pending 阻塞（rollout.go:209-227）；失败不自动停全局，停不停由调用方决策（rollout.go:60-65 注释）。
- canary 选择是**确定性的**：按 (plugin, version, node) 哈希排序（rollout.go:77, 259-289），同一 release 永远同一批先上，重试不换样本。
- `pin` = 单波全量且不再自动升级（rollout.go:99-104）。

§5.2 的多机灰度逐字段对齐这套词表，不新造词汇。

#### 2.4.4 其他

- 健康探针：`/healthz` 恒 200（进程存活），`/readyz` 并行探 DB + Redis + workers（main.go:445-446, 3681）——健康门的现成素材。
- `deploy/opskeeper-rollback-runbook.md`：控制库 / 知识索引 / 运行时三段手工回滚总纲，含退出标准（合成只读场景验证）。
- 开源门禁：`.github/workflows/audit-open-source.yml` + `scripts/audit_open_source.py` 的禁止模式（生产 IP、内部任务号、凭据形态等）——本方案自身的文档与后续实现切片都在它的扫描范围内，等价于一条硬约束：**CD 相关入仓内容不得携带生产环境标识**（本文即按此约束书写）。

---

## 3. 触发与信任链设计

### 3.1 链路总览

```mermaid
title="tag → 生产 信任链（拉取式基线）"
flowchart LR
  A["annotated tag<br/>(保护分支规则)"] --> B["release.yml build<br/>tag 正则 + VERSION 一致<br/>+ version-check 树哈希"]
  B --> C["release.yml release<br/>双架构 sha256 复验"]
  C --> D["签名步骤(新增, Slice 1)<br/>ed25519 detached over<br/>latest.json 规范化字节"]
  D --> E["GitHub Release<br/>资产 + latest.json + .sig"]
  E -. "出站 HTTPS 轮询<br/>(仅此一条通道)" .-> F["生产侧升级执行器(新增, Slice 2)<br/>验签 → 准入 → staged apply"]
  F --> G["健康门<br/>healthz + readyz + 版本回读 + 合成探针"]
  G -->|通过| H["确认 + 审计落账"]
  G -->|失败| I["自动回滚 .previous<br/>+ 告警 + 冻结本版本"]
```

### 3.2 触发器

- **上游触发不变**：`push tag v*.*.*`（已有）。tag 建议加保护规则（仅维护者可推、禁 force push）；annotated tag 顺带承载签名者身份。
- **部署触发 = 生产侧轮询发现新版本**（拉取式，D1）。轮询间隔默认 15 分钟、带抖动；收敛时延 = 间隔 + 下载时间，若未来要求分钟级，可加"发布后轻量通知"（见 §4 D 的代价栏），但基线不引入任何入站通道。
- **人工仍然可以随时介入**：执行器是幂等的 stage-then-apply 结构，人工 `sudo upgrade.sh` 与它共存（谁先完成，另一个发现版本一致即跳过）。

### 3.3 签名（D2）

- 签名对象：`latest.json` 的规范化字节（字段排序、无多余空白）——它已经包含双架构资产的文件名与 sha256 文件名；Slice 1 再把各资产 sha256 值内联进 `latest.json`，于是**一份签名同时钉住元数据与产物哈希**，验签 + sha256 复验构成完整完整性链。
- 算法与载体：ed25519，detached sidecar（`latest.json.sig`），envelope 含 KeyID 与 TreeDigest 风格的摘要声明——语义与 pluginmanifest 的 `Envelope`/`SignatureFile` 同构，生产侧 trust 文件复用 `TrustStore` 的 key id → 公钥 + revoked 结构。
- 密钥治理：私钥仅存在于 GitHub **Environment** secret（如 `release-signing` environment，开 required reviewers——发布动作本身获得一次人工审批，这同时满足"生产稳定期任何发布需人工批准"的治理要求）；公钥与 trust 文件入仓（公钥不是机密）。
- 轮换：双 key 并存期 → 新 release 用新 key → 旧 key 标记 revoked（TrustStore 语义现成）。轮换 PR 本身走代码审查，即"信任锚变更需要人审"。
- 备选记录：cosign/sigstore（H2 容器镜像场景的自然演进，见 D2）、GPG（生态旧、实现重，弃）。

### 3.4 部署准入（验签之后、apply 之前）

执行器在本机声明的准入规则，全部可配置、默认保守：

1. **验签通过**（trust 文件中的 key，且未 revoked）且资产 sha256 复验通过；
2. **防降级**：目标版本 > 当前版本（复用三段比较；明确例外：带 `rollback` 标记的 channel 消息允许显式回退，见 §5.3）；
3. **channel 匹配**：`latest.json` 增加可选 `channel` 字段（`stable` / `rc`）；执行器声明自己订阅的 channel，`rc` 只上订阅了 rc 的主机。注意 §2.3 指出的比较器 rc 截断问题：channel 字段是元数据层的解决法，不依赖比较器分辨 rc 先后；
4. **更新窗口**：可配置 `允许的星期/时段`（D4 待澄清后填入）；窗口外只下载不 apply（staged，窗口开启即切换）；
5. **pin**：本机 pin 文件存在 → 冻结在当前版本，只告警不升级（对齐 `InstallPin` 语义）。

---

## 4. 推送式 vs 拉取式：正式对比（D1）

四个候选，评估维度：凭据暴露面、网络暴露面、失败模式、审计归属、实现与维护成本。

| | A. CI 推送<br/>(GitHub Actions + SSH) | B. 自托管 runner in VPC | C. 函数式 webhook<br/>(阿里云 FC/OOS) | **D. 拉取式自升级（推荐）** |
|---|---|---|---|---|
| 数据流 | release 成功 → deploy job 持 `PROD_SSH_KEY` → 登机 wget + upgrade.sh | 同 A，但 runner 常驻生产网内，凭据不出网 | GitHub webhook → 函数（HMAC 验签）→ 凭据取自 KMS → OOS/SSH 执行 | 生产侧执行器轮询 latest.json → 验签 → staged apply → 健康门 → 确认/回滚 |
| 需要出生产的机密 | **生产 SSH 私钥进 GitHub Secrets** | 无（但 runner 注册 token + 常驻代理面） | webhook secret + 执行凭据集中在阿里云侧 | **零**。仅持有公钥（非机密） |
| 网络方向 | 生产需公网**入站** 22（或放行 GitHub runner IP 段——该段巨大且动态，实际做不到收紧） | runner 主动出站连 GitHub 拉作业；生产网内多一个能执行任意 workflow 的执行器 | 公网入站 webhook 端点（需 HMAC + IP allowlist） | **仅出站 443**，零新增入站 |
| 信任边界含义 | GitHub 账号/workflow 注入面 = 生产失守面（恶意 PR 改 workflow 即可能窃取 secret） | 公网仓库的作业执行器进生产网，供应链风险内移 | 新增一个常驻组件及其密钥生命周期 | 信任锚收敛为"入仓公钥 + 发布 environment 审批" |
| 失败模式 | 凭据泄漏 = 全线失守（最差） | runner 被利用 = 生产网内横移 | 函数/webhook 配置错误误触发 | 最坏 = 单机升级失败并自动回滚，**不升级 = 保持现状**（fail-safe） |
| 收敛时延 | 分钟级（最优） | 分钟级 | 分钟级 | 轮询间隔级（默认 ~15min） |
| 审计 | 在 CI 日志，与生产审计域割裂 | 同 A | 在云侧，需搬运 | **在生产本地落账**（§6.3），天然归属正确 |
| 多机扩展 | 需跳板/循环 + 每台凭据 | 需 runner 组 | 需 OOS 批量编排 | 每台独立拉取，天然波次化（§5.2） |
| 新增维护面 | 小（但最危险） | 中（常驻机 + 升级 runner 本身） | 中（新组件 + 密钥轮换） | 小（一个执行器 + trust 文件） |

**结论：D 为基线。** 决定性的是三条与"熟悉度"无关的性质：

1. **凭据不对称性**：A 必须把"能登录生产的私钥"交给 CI；D 只需要"能验证发布者的公钥"。前者泄漏即失守，后者泄漏无意义。
2. **方向性**：D 把生产暴露面缩减为出站 HTTPS，A/C 各自打开一条公网入站通道且都很难真正收紧。
3. **失败模式**：拉取式天然 fail-safe（拉不到/验不过 = 维持现状）；推送式的失败面是凭据与通道本身。

**元认知检查**：选拉取式是否因为仓内已有同构实现（edge 链）而把熟悉当最优？反证：上表三性质独立于实现熟悉度成立；且仓内同构实现恰恰降低了引入成本（§5 执行器 = §2.4.1 的模式平移），边界收益（消灭生产凭据出域）大于引入成本（一个执行器 + 签名步骤）。

**A/B/C 的去留**：A 记录为"零改造临时路径"（现状人工路径的脚本化版），**禁止**将其配上生产 SSH secret 上线——那等于为了省一次登录引入最大暴露面；B 在组织已有 VPC 内 CI 基础设施时重评；C 的 OOS 审批能力若未来需要"云侧强审批"再重评。三者均不进入基线实施切片。

---

## 5. 灰度与回滚

### 5.1 形态假设分支（D3，待用户澄清）

**H1：ECS 单机直接部署（compose 或 systemd）——现有全部证据支持的默认形态。**
证据：`deploy/install/README.md` 两种模式均单机；`upgrade.sh` 全程序面向一台主机；项目环境说明中的正式环境为单入口登录页。单机上"多机滚动"不存在，灰度的等价物是三件事：**组件级分步、三层健康门、自动回滚**（§5.2 表格中"单机等价物"列）。停机窗口 = compose down→up 间隙（实测约 1–3 分钟，含镜像载入）；缩短手段：apply 前先把新镜像 `docker load` 预载（staged 阶段完成）、数据卷不动、仅 recreate 受影响容器。窗口与停机容忍待 D4 澄清后写进准入配置。

**H2：多机 ECS 或容器服务（ACK）集群。**
`docs/deployment/upgrade.md` 已描述 Helm 滚动 / 蓝绿目标态（`maxUnavailable=0` + `/readyz`）。此分支下 CD 的正确形态：K8s 侧用镜像 tag 滚动（签名演进走 cosign，见 D2），自建灰度只发生在"还来不及上 K8s 的多台裸机"场景。两分支共用 §3 信任链与 §5.3 回滚语义，仅编排层不同。

### 5.2 灰度策略：与 pluginmanifest rollout 语义对齐

| pluginmanifest 语义 | rollout.go 证据 | H2 多机映射 | H1 单机等价物 |
|---|---|---|---|
| `rolling`（默认策略） | plugin.go:155 | 生产实例按波次升级 | 组件按序升级：edge agent → 观察 → manager → 观察 → web（顺序理由：edge 失败影响面最小且可独立回滚，manager 是唯一有状态迁移的组件） |
| canary = rolling 首波 1/10，确定性哈希选取 | rollout.go:33-41, 77, 259-289 | 第一波实例按 (release, 实例 id) 哈希确定性选出，重试同批 | staging/DevBox 主机就是 canary：同 tag 先在非生产验证（6.4 的公网 staging E2E 即此角色），生产只接已验证 tag |
| 波宽 WaveSize=10 | rollout.go:31 | 每波 ≤10 台 | N/A |
| 确认门：当前波全部 confirmed/failed 才 Advance | rollout.go:209-227 | 下一波开始条件 = 本波所有实例健康确认（或失败） | 下一组件开始条件 = 上一组件健康门通过 |
| 失败不自动停全局，调用方决策 | rollout.go:60-65 | **canary 波任何失败 → 立即暂停全量**；后续波失败 ≥1 → 暂停并告警（阈值可配） | 任一组件健康门失败 → 停止序列 + 自动回滚该组件 |
| `pin`（单波、不再自动升） | rollout.go:99-104 | channel 冻结 / 紧急停机阀 | 本机 pin 文件（§3.4 规则 5） |

实现注意：多机波次的"confirmed"回执需要一个回报通道。基线用最朴素的形式——每台执行器把结果写本地审计账（§6.3）并暴露只读状态端点（本机 localhost，不新增公网入站），编排侧（人工或后续切片的编排器）按台账推进波次；**不为此提前引入中心化控制面**，那是 H2 确认后的独立决策。

### 5.3 回滚

分三层，覆盖递进的失败面：

1. **启动失败自动回滚（新版本起不来）**：平移 `apply-pending-upgrade.sh` 模式——apply 前逐项备份 `.previous`（compose 文件、systemd 单元、镜像引用、`VERSION`），健康门超时 → 恢复 `.previous` → 重启 → 记录"本版本已回滚"状态防升级循环（对应脚本里清 `last_upgrade_at` 的防环设计）。镜像层回滚依赖 upgrade.sh 既有的"保留最近两个 tarball / 不删在用镜像"策略，Slice 2 只需保证失败路径**不触发**清理。
2. **显式降级（新版本健康但需退回）**：执行器默认防降级（§3.4）；显式回退走带 `rollback` 标记的 channel 消息或人工 pin + 手工执行 `upgrade.sh`（旧 tarball 仍在）。涉及 schema 时受第 3 层约束。
3. **数据层回滚**：gorm AutoMigrate 只有前进语义。规则：apply 前对控制库做快照（`pg_dump`，保留 N 份），回滚运行时**可以**不带库回退（AutoMigrate 新增列对旧代码无害的假设需在 6.6 迁移演练里逐版本验证），一旦演练证明某版本迁移不可逆，该版本的准入规则必须标注"仅前进"。完整手工路径按 `deploy/opskeeper-rollback-runbook.md` 执行。

### 5.4 健康门定义（三层，全部通过才算 confirmed）

1. **存活**：`/healthz` 200（进程起来）；
2. **就绪**：`/readyz` 200（DB + Redis + workers 并行探针，main.go:445-446）；
3. **版本回读 + 合成只读探针**：管理面版本接口回读值 == 目标 tag；一条只读合成场景（如一次只读诊断流转）端到端成功——对齐 rollback runbook 的退出标准（"read-only synthetic scenario completes end to end"）。

超时预算：90s 存活 + 120s 就绪 + 300s 探针（数值入配置，演练后定稿）。健康证据（各层耗时与结果）写入审计账。

---

## 6. 安全性论证

### 6.1 凭据治理

- **CI 侧持有的全部机密 = 签名私钥**（一个 GitHub Environment secret，开 required reviewers）。生产 SSH / 云账号 / IAM 凭据**不进 GitHub Secrets**——这是选拉取式的直接结果（§4 表第 2 行）。
- 凭据不落 CI 日志：签名步骤只输出 KeyID 与摘要，不 echo 私钥；workflow 遵循既有惯例不打印 secret 派生值；`audit_open_source.py` 的凭据形态扫描对 workflow 文件同样生效，等于多一道静态兜底。
- 生产侧执行器**不持有任何机密**：公钥与轮询 URL 都是可公开信息（公开仓库 Release 资产匿名可读，无需 GitHub token）。

### 6.2 通道最小暴露面

- 生产 → 公网：仅出站 HTTPS（github.com 与镜像域名，可选出站代理）。
- 公网 → 生产：**零新增**。没有 webhook 端点、没有 CI 入站、不开放新的管理端口。执行器状态端点只绑 localhost。
- 产物完整性不依赖通道：即使 DNS/镜像被劫持，验签失败 = 拒绝 apply（fail-safe）。

### 6.3 审计（谁 / 何时 / 哪个版本 / 结果）

每次升级尝试在生产本地追加 JSONL 审计账（`/var/log/opskeeper/upgrades/ledger.jsonl`，仅追加），字段：时间戳、from/to 版本、触发方式（poll / manual）、决策（skip / staged / applied / rolled_back / rejected + 原因）、签名 KeyID 与资产 sha256、健康门三层证据、channel 与准入规则版本。这一账本满足"谁何时哪个版本结果"四问，且**归属在生产**——对比推送式把审计留在 CI 日志（§4 表）。产品内 audit 域的接入（可选回传）列为 Slice 5，不在基线内。

### 6.4 开源与合规边界

CD 相关的一切入仓内容（本文、Slice 1 的 trust 文件与 workflow 增量、Slice 2 的执行器）都在开源门禁扫描范围内：不得出现生产 IP、内部任务编号、真实凭据、私有路径。生产侧私有配置（轮询目标、窗口、pin）全部留在生产机的配置文件，不回流入仓。

---

## 7. secrets 与配置清单（按方案对比给出）

### 7.1 基线（拉取式 D）——需要管理的东西

| 项 | 位置 | 性质 | 轮换方式 |
|---|---|---|---|
| `OPSKEEPER_RELEASE_SIGNING_KEY`（ed25519 私钥） | GitHub Environment `release-signing` secret，required reviewers 开启 | **机密**（发布凭据，非生产凭据） | 双 key 并存 → 新 release 用新 key → 旧 key revoked（trust 文件 PR） |
| `release-keys.json`（KeyID → 公钥，含 revoked 区） | 仓库内（Slice 1） | 非机密，版本化 | 走代码审查 |
| 执行器配置：轮询 URL / 间隔 / channel / 窗口 / pin | 生产机本地配置文件 | 非机密 | 运维变更，留审计 |
| `PROD_*` SSH / 云凭据 | **不存在于 CD 链路**（仍属日常运维，不进 CI） | — | — |

### 7.2 对照：若将来采纳推送式（A/C）需要额外引入的机密

| 方案 | 额外机密 | 额外暴露 |
|---|---|---|
| A（CI+SSH） | `PROD_SSH_KEY`（+ 每台主机条目）入 GitHub Secrets；须配 environment protection + OIDC 才能缓解 workflow 注入窃取 | 生产公网入站 22 |
| C（webhook） | webhook HMAC secret（GitHub 侧 + 函数侧）、ECS 执行凭据（KMS 托管） | 公网入站 webhook 端点 |

列出此表的目的：让 D1 的取舍可见——选 D 省掉的不只是实现量，而是整类机密的生命周期管理义务。

---

## 8. 实施切片建议（各自独立 change 承载）

| 切片 | 内容 | 前置 | 验收要点 | 回滚方式 |
|---|---|---|---|---|
| **Slice 0** 决策澄清 | 用户裁决 D1–D6（尤其 D3 形态、D4 窗口）；确认 `opskeeper.cloud/dl` 镜像真源策略 | 本文 | 决策点清单回填 | — |
| **Slice 1** release 签名 | `release.yml` 增加签名步骤；`latest.json` 内联资产 sha256 + `channel` + `signature` 字段；入仓 `release-keys.json`；CI 增加验签自测；收口 download_base 真源 | Slice 0 | CI 里"签名→验签→篡改必败"三态自测；对历史 release 不回溯 | revert 该 change；旧 latest.json 无签名字段时执行器按"未签名 channel"拒绝（不误升级） |
| **Slice 2** 生产侧执行器 | `deploy/install/` 下新增拉取式执行器（shell 优先，模式 = §2.4.1 平移）：轮询 → 验签 → 准入 → 镜像预载 staged → apply → 三层健康门 → 自动回滚 → JSONL 审计。compose/systemd 两模式各验证 | Slice 1 | DevBox/staging 全链路演练一轮升级 + 一轮"坏签名拒收" + 一轮"健康门失败自动回滚"（即 6.6 的 CD 部分） | 执行器停用即回到纯人工路径；系统状态无残留 |
| **Slice 3** 准入与 channel 完善 | 窗口/pin/防降级/显式 rollback 标记的配置化与测试；rc channel 语义（含 §2.3 比较器截断问题的文档化） | Slice 2 | 准入规则单测（降级/窗口外/pin/坏 channel 四态拒绝） | 同上 |
| **Slice 4** 多机灰度编排（仅 H2） | 波次编排对齐 §5.2 表；confirmed 回执收集 | H2 确认 | 两台以上环境演练 canary 暂停与恢复 | 编排器只读停用，各机仍独立拉取 |
| **Slice 5** 观测回传（可选） | 升级状态进产品健康页 / audit 域；systemupgrade 状态页对接执行器台账 | Slice 2 | 状态页只读展示 from/to/结果 | 功能开关关闭 |

顺序依赖：1 → 2 → 3 严格串行；4、5 条件触发。Slice 1 是唯一动 workflow 的切片，且只在 release job 内追加步骤，满足"不改构建语义"。

---

## 9. 风险分级与验证方式

| # | 风险 | 级别 | 缓解 | 残余 |
|---|---|---|---|---|
| R1 | 签名私钥泄漏（GitHub 侧） | 高影响/低概率 | Environment 审批 + 双 key 轮换 + revoked 语义 + 生产 pin 可紧急冻结 | 攻击者可签恶意版本；需 trust 文件 PR 才能换 key，检出路径存在 |
| R2 | 轮询源不可用 / 限流 / 阻断 | 中 | 双源（latest.json 镜像 + GitHub API，systemupgrade 已有先例）+ 指数退避；**拉不到 = 维持现状** | 升级延迟，无可用性影响 |
| R3 | 健康门过松（带病确认）或过严（误回滚） | 中 | 三层门 + 版本回读 + 合成探针；阈值可配并在演练中定稿；回滚必告警人工复核 | 语义级误判需观察期数据修正 |
| R4 | schema 迁移不可逆导致回滚受限 | 高 | apply 前 pg_dump 快照 + 6.6 迁移演练逐版本标注"仅前进" | 声明为仅前进的版本无法在线回退，走 runbook |
| R5 | 单机停机窗口超容忍 | 中 | 镜像预载 + 窗口准入（D4 定容差） | 窗口数据待实测 |
| R6 | 执行器自身 bug 影响运行中服务 | 低 | 只读轮询 + staged 隔离 + 校验期失败不阻塞（对齐 apply-pending-upgrade.sh 的 fail-open 设计：升级侧错误不代价可用性） | — |
| R7 | 构建非 bit-for-bit 可复现 | 低（本方案范围外） | 信任链锚定 sha256 而非可复现性；可复现性另立议题 | — |

**验证方式**：Slice 1/2/3 各自的单测与 CI 自测；DevBox staging 全链路演练（正常升级、坏签名、坏 sha256、降级 tag、半下载中断、健康门超时六剧本）；演练证据回填 6.6。**ADR 回滚思路**：整个 CD 方案的可撤销性由切片隔离保证——任一 slice 独立 revert 后系统退回"产物就绪即止 + 人工升级"现状，生产运行不受任何影响。

---

## 10. 决策点清单（请用户裁决）

| # | 决策 | 选项与推荐 | 影响 |
|---|---|---|---|
| **D1** | 部署触发模式 | A CI+SSH / B VPC runner / C 函数 webhook / **D 拉取式（推荐，§4）** | 决定是否引入生产凭据出域与入站通道；D 下 CI 零生产凭据 |
| **D2** | 签名方案 | **ed25519 detached + 入仓 trust 文件（推荐，与 pluginmanifest 同构）** / cosign（H2 容器镜像场景演进项）/ GPG（弃） | Slice 1 的实现形态与密钥治理流程 |
| **D3** | 目标形态假设 | **H1 单机（现有证据默认）** / H2 多机或 ACK —— 待用户输入：ECS 直接部署还是容器服务、几台/几集群 | 决定 Slice 4 是否存在、灰度语义取 §5.2 哪一列 |
| **D4** | 更新窗口与停机容忍 | 待用户输入；方案默认：窗口准入可配 + 发布 environment 人工审批兜底 | 准入规则参数；H1 停机窗口实测目标（建议 ≤3 分钟） |
| **D5** | 自动回滚阈值 | 推荐：健康门任一层超时即回滚；H2 下 canary 失败即暂停全量 | 回滚灵敏度 vs 误回滚率 |
| **D6** | systemupgrade 域边界 | 推荐：**保持只读检查器，不加 apply 端点**（避免制造公网可触发的自升级 API；执行器独立于产品进程）；后续仅把状态页对接执行器台账（Slice 5） | 产品 API 暴露面与 CD 执行面的边界 |

---

## 附：验收对照

- 现状审计含 systemupgrade 能力边界（代码证据）：§2.1 / §2.2 / §2.3（逐文件行号）。
- 触发 / 信任链 / 凭据治理 / 灰度回滚 / 安全论证完整：§3 / §4 / §7 / §5 / §6。
- 推送式 vs 拉取式正式对比（含暴露面分析）：§4 四候选全维表 + 三性质论证 + 元认知检查。
- 实施切片与 secrets 清单：§8 / §7。
- 决策点清单（推送 vs 拉取、目标形态假设分支）：§10（D1 / D3）。
