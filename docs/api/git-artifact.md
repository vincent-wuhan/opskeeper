# API 文档：git-artifact 制品溯源

> **范围**：从运行时事件（PG query / Redis cmd / K8s image / HTTP route）反查到 commit + file:line，并按需带上提交作者。
> **协议版本**：仅 v0。请求头 `X-GitArtifact-Version` 缺省按 v0 处理，显式给别的值一律 400。
> **实现**：`core/manager/knowledge/gitartifact/{server.go,linker.go}` + `.../gitartifact/api/indexer.go`；挂载点 `cmd/opskeeper/gitartifact_runtime.go`（`/api/v1` JWT 保护组内）。
> **本文与代码的关系**：本文描述的是**代码当前实际行为**，包括未交付项（见文末「未交付」）。契约由 `server_test.go` / `git_test.go` / `indexer_test.go` 三组测试钉住，改形状必须先改测试再改本文。

---

## 一、协议头

```
X-GitArtifact-Version: v0   # 可省略；给 v1 等其它值 → 400 unsupported protocol version
Authorization: Bearer <JWT> # 由挂载方的 JWT 中间件校验，本包不解析 token
```

鉴权与租户注入属于挂载方职责：`Server.Handler()` 只负责协议解析，`TenantID` 一律从 `context` 读（`tenantctx`）。

---

## 二、上报制品

```
POST /api/v1/git-artifacts
Content-Type: application/json
```

**Body**（注意是 `artifact` 包裹对象，不是平铺）：

```json
{
  "artifact": {
    "repo_url": "https://github.com/example/order-svc",
    "commit": "0123456789abcdef0123456789abcdef01234567",
    "branch": "main",
    "artifact_url": "s3://builds/order-svc/v1.2.3.tar.gz",
    "build_at": "2026-07-13T10:00:00Z",
    "meta": {
      "build_id": "ci-build-12345",
      "commit_author": "alice@opskeeper.io",
      "commit_message": "fix: optimize pending order query",
      "extracted_symbols": [
        {
          "type": "pg_query",
          "input": { "query": "SELECT * FROM orders WHERE status = 'pending'" },
          "file_path": "src/db/queries.go",
          "line_start": 142,
          "line_end": 158,
          "confidence": 0.95
        },
        {
          "type": "redis_cmd",
          "input": { "cmd": "GET", "key": "user:profile:42" },
          "file_path": "src/cache/user.go",
          "line_start": 88,
          "line_end": 92,
          "confidence": 0.9
        }
      ]
    }
  }
}
```

### 2.1 字段来源与必填校验

必填（缺任一 → 400，`code` 即 HTTP 状态码，`message` 为英文原因）：

| 字段 | 校验 |
|---|---|
| `artifact.repo_url` | 非空 |
| `artifact.commit` | 长度必须为 40 或 64 |
| `artifact.branch` | 非空 |
| `artifact.artifact_url` | 非空 |
| `artifact.build_at` | 非零值 |
| `artifact.meta.build_id` | 必须存在 |

**符号（symbols）不是独立的请求字段**，而是由 CI 预提取后放在 `artifact.meta.extracted_symbols`，Indexer 的 `MetaExtractor` 从这里读。原因：反查索引的键就是符号本身，符号必须与制品同批落库才谈得上一致性。

`commit_sha` 可选：缺省时由 `MetaExtractor` 注入 `artifact.commit`。

### 2.2 响应

**201 Created**：

```json
{
  "code": 0,
  "message": "ok",
  "data": { "id": "ga-<hash>", "index_status": "queued" }
}
```

`id` 由 `repo_url + commit` 派生（`generatePublicID`），因此同 commit 同仓库重复上报是幂等的：第二次直接 **409 Conflict**，`message` 为 `artifact already exists: <id>`。要重跑索引请走 `Indexer.Rebuild`。

**400**（JSON body，`code` = HTTP 状态码）：

```json
{ "code": 400, "message": "missing required field: meta.build_id" }
```

---

## 三、查询制品

```
GET /api/v1/git-artifacts/{id}
```

**200**：`data` 为制品对象（`repo_url` / `commit` / `branch` / `artifact_url` / `meta` / `build_at` / `indexed_at` / `index_status` / `index_error`）。

**状态机**：`queued → running → completed / failed`；`indexed_at` 为 null 表示未完成，`index_error` 非空即最近一次失败原因。

**404**：id 不存在。**403**：请求租户与制品租户都非 0 且不相等。

---

## 四、运行时反查

```
POST /api/v1/runtime-link
```

**Body**：

```json
{
  "query": {
    "symbol_type": "pg_query",
    "symbol": "SELECT * FROM orders WHERE status = 'pending'",
    "tenant_id": 42
  }
}
```

`symbol` 是单字符串，形状随 `symbol_type` 变（解析见 `parseSymbolInput`）：

| symbol_type | symbol 形状 | 索引键 |
|---|---|---|
| `pg_query` | SQL 原文 | 标准化后的 SQL（去空白/注释） |
| `redis_cmd` | `GET user:profile:42`（首个空格分隔 cmd 与 key） | `CMD:KEY` |
| `k8s_image` | 完整镜像引用；带 tag 时再试一次仅 tag | 完整引用 / tag |
| `http_route` | `GET /orders/{id}`（首个空格分隔 method 与 path） | `METHOD PATH` |

`tenant_id` 可选，仅当 ctx 里没有租户（即超管路径）时用来指定查询租户；ctx 已有租户且与 body 不一致 → **403**。

### 4.1 命中响应

```json
{
  "code": 0,
  "message": "ok",
  "data": {
    "link": {
      "commit": "0123456789abcdef0123456789abcdef01234567",
      "repo": "https://github.com/example/order-svc",
      "file_path": "src/db/queries.go",
      "line_start": 142,
      "line_end": 158,
      "author": "alice@opskeeper.io",
      "commit_msg": "fix: optimize pending order query",
      "confidence": 0.95,
      "needs_human_confirm": false
    }
  }
}
```

- `author` / `commit_msg` 是 **optional 键**（omitempty）：来自 `meta.commit_author` / `meta.commit_message`，CI 没透传就缺席。**缺席的含义是「这条流水线没透传这两项」，不是「这个提交没有作者」**——消费方不要把缺席读成空值事实。
- `needs_human_confirm` 由 `confidence` 推导（`LinkResult.NeedsHumanConfirm()`，阈值 0.7 常量 `ConfidenceThreshold`），是人工确认信号的**唯一真值源**。它曾经还有一个孪生键 `flag`（HTTP 通道）/`needs_human_confirm`（工具通道）各写一份，两条通道互不相交导致其中一份恒空；现已删除，只保留这个布尔。

### 4.2 未命中响应

```json
{
  "code": 0,
  "message": "ok",
  "data": { "link": null, "reason": "no_match_in_index" }
}
```

### 4.3 低置信度

`confidence < 0.7` 时 `needs_human_confirm: true`，其余列不变。没有 `reason` 字符串、没有 `needs_human_review`、没有 `match_type`——判断只有一个键承载，避免模型看到两个键 disagree 而其中一个是恒空的。

消费方（Coordinator / RCA 报告）在该位为 true 时标注「代码位置不明确」。

---

## 五、Agent 工具面（模型看到的形状）

HTTP 协议之外，同一个 linker 图通过 BaseTool `git.find_runtime_link` 暴露给模型（`cmd/opskeeper/gitartifact_runtime.go`）：

```json
{
  "name": "git.find_runtime_link",
  "input_schema": {
    "type": "object",
    "properties": {
      "symbol_type": { "type": "string", "enum": ["pg_query", "redis_cmd", "k8s_image", "http_route"] },
      "input": { "type": "object", "additionalProperties": true }
    },
    "required": ["symbol_type", "input"],
    "additionalProperties": false
  }
}
```

返回（`dispatchLink`）：

```json
{
  "hit": true,
  "symbol_type": "pg_query",
  "confidence": 0.95,
  "author": "alice@opskeeper.io",
  "commit_msg": "fix: optimize pending order query",
  "needs_human_confirm": false
}
```

未命中时返回 `{"hit": false, "symbol_type": "..."}`。工具面与 HTTP 面的 `needs_human_confirm` 同源同义，只是键值载体不同（map vs 结构体投影）。

---

## 六、租户隔离

索引键物理隔离：`"<tenant_id>\x00<symbol>"`（`scopedIndexKey`）。

- 同租户查询：先查该租户作用域；
- ctx 无租户（超管）：body 的 `tenant_id` 指定作用域；仍可 fallback 到 `0\x00<symbol>` 全局索引；
- 模糊匹配（仅 PG query 有：去数字字面量 / IN-list / 字面常量后前缀比对）同样先租户作用域再全局，避免跨租户泄漏。

---

## 七、异步索引

上报后**异步**触发反向索引构建（`triggerIndex`，`go idx.Index(...)`），失败只记日志不回滚上报——上报成功与索引成功是两件事。

未挂 Indexer 时 fallback 到 `buildIndex` 占位实现：仅把 `index_status` 置 `completed`、写 `indexed_at`，不解析符号。生产挂载（`newGitArtifactRuntime`）走真实 Indexer。

`indexed_symbols` / `total_symbols` / `eta_s` 这类细粒度进度**没有暴露成端点**；要查进度只能 `GET /api/v1/git-artifacts/{id}` 读 `index_status` / `indexed_at` / `index_error`。

性能目标「1000 commit / 5 min」来自 spec，**当前无基准测试支撑**，属于未验证目标。

---

## 八、错误响应

统一形状 `{ "code": <HTTP 状态码>, "message": "<英文原因>" }`（`writeError`）。没有独立的业务错误码枚举。

| HTTP | 触发条件 |
|---|---|
| 400 | JSON 解析失败 / 缺必填 / commit 长度错 / 缺 symbol_type 或 symbol / 未知 symbol_type / 协议版本不支持 |
| 401 | 由挂载方 JWT 中间件产生，本包不产生 |
| 403 | 请求租户与 ctx 租户冲突（runtime-link）、查询他人制品（GET） |
| 404 | 制品不存在（GET） |
| 405 | 方法不匹配 |
| 409 | 同 id 制品重复上报 |
| 500 | store 读写失败 |

---

## 九、未交付

以下能力在旧版本文里出现过，但**代码从未实现**，文档已删除对应描述：

| 项 | 状态 | 缺什么 |
|---|---|---|
| `meta.commit_author` / `commit_message` | ✅ 已接线 | 需要 CI 在 `meta` 里透传这两个键，否则 `author` / `commit_msg` 键缺席 |
| `evidence` 列（证据片段） | ❌ 已删除 | 需要索引期采集匹配依据，无生产者；缺席会被模型读成「无证据」而非「未实现」 |
| `match_type`（exact / fuzzy 命中方式） | ❌ 从未实现 | PG 模糊匹配确实存在，但命中方式没有落到结果里 |
| `needs_human_review` / `reason` 字符串 | ❌ 由 `needs_human_confirm` 取代 | 单键布尔，避免同一判断两处表达 |
| 协议 v1（sbom / provenance / signatures） | ❌ 未实现 | 服务端只接受 v0；v1 头会 400 |
| `GET .../index-status` 端点 | ❌ 未实现 | 用 `GET /api/v1/git-artifacts/{id}` 代替 |
| URL query / header 传租户 | ❌ 未实现 | 租户只来自 ctx（`tenantctx`）与 runtime-link 的 body `tenant_id` |
| 独立业务错误码（4000 / 4003 / …） | ❌ 未实现 | `code` 就是 HTTP 状态码 |

---

## 十、相关

- 闸门：`make apidoc-check` —— 本文件里每条 `METHOD /path` 必须**被源码里真实的路由注册服务**（决策 269 起要求「注册过」，而不只是「树里出现过这个字符串」）
- 实现：`core/manager/knowledge/gitartifact/`（协议 + linker）、`.../api/`（索引器）
- 模型可见工具面：`core/manager/middleware/adapter/git/git.go`
- Middleware API：[docs/api/middleware.md](middleware.md)
- Harness 评测：[docs/api/harness.md](harness.md)
