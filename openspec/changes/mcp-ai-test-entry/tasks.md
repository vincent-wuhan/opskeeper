# Tasks: mcp-ai-test-entry

对应 design.md §4 切片 S1–S4 与 §3 的 D-1～D-5 裁决。每片是独立 OpenSpec 任务 + 独立 PR；
首片（S1）零外部 AI 可验证。回写口径逐片写明——「什么状态才算这条任务完成」。

切片依赖：S2 依赖 S1（判决器与读口）；S3 依赖 S2（JSONL 轨迹格式）；S4 依赖 S3（回放用例
才能谈 catalog 回写）。S1 不依赖任何后续片。

## 1. S1 — 探索通道地基（零 AI 可验证）

- [ ] 1.1 章程格式与校验器（D-1）：`docs/test/explore-charters/_template.md`（frontmatter
      闭集：`id/title/role/tenant/max_tool_calls/max_steps/catalog_refs`）+ 首份真实章程
      `explore-incident-read-path.md`（role=investigator）+ 第二份 `explore-repair-approval.md`
      （role=repairer，为 I-3 铺路）。校验器在 `tests/e2e/explore/` 内：frontmatter 缺字段、
      role 不在 `auth.AgentTeamsWorkerPermissions()` 的角色集、`max_tool_calls<=0`、
      `max_steps<=0`、`catalog_refs` 引用不存在的 catalog 条目号——均非法。
      回写口径：校验器单测覆盖以上五种非法形状各至少一例，两份章程过校验器。
- [ ] 1.2 testenv 的 DSN 访问器：`Env` 结构体存 `Start` 时拿到的 dsn，新增
      `func (e *Env) DSN() string`（`tests/e2e/testenv/env.go`，测试包，`//go:build e2e`
      门控随包自带）。**不改生产二进制任何一行。**
      回写口径：`make audit-open-source` 绿；`go build ./cmd/...` 与改动前逐字节等价。
- [ ] 1.3 审计读口（新代码，测试侧）：`tests/e2e/explore/auditreader.go`——用 `Env.DSN()`
      直连 MySQL，读 `audit_logs`（表名与列名对齐 `core/domains/model/audit/log.go:106` 的
      `Log` 模型），按 `(action = 'mcp_tool_call' 或 'mcp_tool_authorize')` + 会话时间窗过滤，
      返回行集。**不是新 HTTP 端点**——`GET /v1/admin/audit-logs` 已存在
      （`core/domains/server/audit/http.go:34`，挂在 protected 下）可作为交叉验证，但判决器
      以直读 DB 为准（避免分页上限 500 截断轨迹，见 `repo.go:69-71`）。
      回写口径：读口能读回一次真实 `tools/call` 的 start/end 行（含 `payload_json` 里的
      `tenant`、`arguments_sha256`），单测钉住。
- [ ] 1.4 探索编排（新代码）：`tests/e2e/explore/harness.go`——`testenv.Start` →
      `LoginAdmin` → `POST /api/v1/agentteams/token`（`core/manager/iam/server/http.go:159`，
      body 含 `tenant_id/worker/role/allowed_tools/ttl_seconds`，allowed_tools 取章程 role
      的全量允许集）→ 输出连接信息（base_url、token 打码显示、charter 路径、预算）。
      全部复用现有 API，服务端零改动。
      回写口径：`spec.md`「零凭据起环境」场景可执行——干净检出上（无任何 secret 环境变量）
      拉起实例、`healthz` 200、令牌与章程输出。
- [ ] 1.5 判决器（新代码）：`tests/e2e/explore/verdict.go`——design §6 的 I-1～I-5 纯函数实现，
      输入 `(audit_rows, trace, before_tools, after_tools, charter)`，输出 `Verdict`
      （`pass/fail/incomplete` + 每条不变量的 `pass/fail/not-exercised` + 证据计数）。
      **输入类型里没有放主观陈述的位置**（修订 2 的结构保证）。I-5 的凭据形状正则复用
      `scripts/audit_open_source.py:57-70` 的形状清单，落成 Go 常量并加注释指向脚本出处，
      两处漂移由 S4 的闸门任务（4.3）钉住。
      回写口径：五条不变量各有独立单测，含「not-exercised」分支。
- [ ] 1.6 首片独立验证——self-test（`spec.md` 场景「判决器对坏轨迹必须红」的可执行证明）：
      `tests/e2e/explore/harness_self_test.go` 用 `pkg/mcpclient` 扮演无 AI 的假探索者，
      固定序列：initialize → tools/list 快照 → 3 次合法 read 调用（investigator 允许集内）
      → 1 次看不见的工具名 → tools/list 复取 → 判决。断言：I-1/I-2/I-4/I-5 按 reality 判定，
      I-3 not-exercised（investigator 无 write 工具）；另一条**已知坏轨迹**（构造一条缺审计行
      的 trace 直喂判决器）必须 fail。
      回写口径：`go test -tags=e2e ./tests/e2e/explore/ -run TestExploreHarness -v` 全绿，
      且其中至少一个断言是「判决为 fail」（证明判决器不是只会绿）。
- [ ] 1.7 令牌边界测试（`spec.md` 场景「测试令牌打不到别处」）：起**两个** testenv 实例
      （注意 `sharedMySQL` 每 `go test` 进程只起一个容器、`Start` 每次随机 JWT secret——
      `env.go:160`——两实例共享 DB 但密钥不同，这正是要测的边界），用 A 的令牌打 B 的
      `/api/v1/mcp` `tools/list`：断言 401（`auth.go:603` 的 unauthorized 路径）且 B 的
      `audit_logs` 在此请求前后行数不变（鉴权失败不写审计行，`middleware/auth.go` 的
      拒绝路径只 `log.Warn` 不 Emit）。
      回写口径：上述双实例断言绿；单测注释写明依据是「每实例随机 JWT secret」而非
      「网络不可达」。
- [ ] 1.8 I-3 的通道内验证（repairer 章程的 self-test 延伸）：用 `explore-repair-approval.md`
      的 repairer 令牌重跑假探索者：调 `recovery.execute` → 断言它停在审批队列
      （`GET /v1/approvals` 有 pending，且该调用无 end 审计行＝未执行）→ 判决 I-3 = pass。
      这条放在 S1 而不是 S3，因为它验证的是判决器，不是回放。
      回写口径：repairer 假探索者测试绿，I-3 判 pass 且证据计数为 1。
- [ ] 1.9 S1 PR：`make test-e2e`（或 `go test -tags=e2e ./tests/e2e/...`）+ `make audit-open-source`
      + `openspec validate mcp-ai-test-entry --strict` 三绿；PR 说明写明「本片零外部 AI、
      服务端零改动」并附 self-test 输出摘要。

## 2. S2 — 客户端 recorder + 报告落盘

- [ ] 2.1 recorder（D-2）：`tests/e2e/explore/recorder.go`——`http.RoundTripper` 实现的记录
      代理，接入方把 MCP 客户端 base_url 指向它；转发到 testenv manager，按序写 JSONL：
      每条记 `seq/method/params 摘要/request_id/status/X-Opskeeper-Audit-ID`，`tools/list`
      应答额外记 `observed_tools` 快照。**不记 `Authorization` 头，不记完整应答体**
      （只记 digest 与工具名集）。
      回写口径：用 S1 的假探索者走一遍 recorder，产出的 JSONL 与 1.3 读口读回的审计行
      在 `(method, tool, tenant)` 三元组上一一对应。
- [ ] 2.2 报告生成（D-5）：`tests/e2e/explore/report.go`——落
      `openspec/changes/mcp-ai-test-entry/reports/<run-id>.json` + 同名 `.md`；JSON 顶层
      `verdict`（只能由判决器写）与 `leads`（只能从 recorder 的 AI note 字段收集）物理
      分段。`leads` 为空数组时报告照样完整。`.gitignore` 不排除该目录。
      回写口径：`spec.md`「线索不改变结论」场景可执行——构造 leads 里塞「疑似缺陷」而
      不变量全过的报告，断言 `verdict.outcome == "pass"` 且 leads 原样呈现；反向单测：
      判决器函数签名层面就没有 leads 参数（编译期保证）。
- [ ] 2.3 章程模板补 `leads_prompt` 正文段：告诉接入方「如果你觉得哪里卡住了/像缺陷，
      在该步 note 里写下来，它会进 leads、不影响判决」。模板与两份既有章程同步更新。
      回写口径：校验器接受含 `leads_prompt` 的章程；两份章程过校验器。
- [ ] 2.4 探索入口命令（make 目标雏形）：`make explore-run CHARTER=...`——起环境、铸令牌、
      打印连接信息（供外部 AI 手动接入）或直接跑假探索者（`--fake` 开关，复用 1.6 的
      固定序列）。预算计数在通道侧：触 `max_tool_calls/max_steps` 即终止、令牌置失效
      （等待 TTL 自然过期即天然失效，通道侧只是停止转发）、判 `incomplete`。
      回写口径：`spec.md`「跑飞即终止」场景可执行——假探索者把 `max_tool_calls` 设为 2、
      固定序列含 3 次调用，断言第 3 次未被转发、报告 `incomplete`。
- [ ] 2.5 S2 PR：S1 全部闸门 + recorder/reports 的单测；PR 说明附一份假探索者产出的
      真实报告（redact 令牌后）。

## 3. S3 — 回放固化

- [ ] 3.1 回放格式定稿（D-3）：JSONL schema 固化为 `tests/e2e/testdata/explore/` 下的
      一份带注释样例 + Go 侧 `ReplayRecord` 结构体；`observed_tools` 标注「线索字段，
      回放器不读」。
      回写口径：S2 的 recorder 输出可直接被 `ReplayRecord` 反序列化。
- [ ] 3.2 回放用例生成器：`tests/e2e/explore/gen_replay.go`（`go run` 工具，非测试）：
      JSONL → `tests/e2e/explore_replay_<charter-id>_test.go`（table-driven，
      `//go:build e2e`，`testenv.Start` + `mcpclient` 三件套）。生成前对每条
      `tools/call` 的 `arguments` 跑凭据形状扫描（复用 1.5 的正则），命中即拒绝生成并
      报出命中的记录号。
      回写口径：用 S2 假探索者的 JSONL 生成一份用例，`go test -tags=e2e` 能跑；
      构造一条带 `sk-` 形状参数的 JSONL，断言生成器拒绝。
- [ ] 3.3 回放对账：回放用例末尾读审计链，与本 JSONL 的 `tools/call` 记录做
      `(method, tool_name, tenant)` 三元组**序列**比对——数量与顺序都一致才绿；
      不比 `audit_id`（自增主键）、不比响应体。
      回写口径：`spec.md`「无 AI 复现同一条调用序列」场景可执行——生成的回放用例绿，
      且人为删掉 JSONL 一条记录再生成时，回放用例红（对账真有牙齿）。
- [ ] 3.4 S3 PR：`make e2e-manager-check`（`Makefile:230`）+ 既有三闸门绿；生成的回放
      用例文件进 PR；**此刻才允许**在 `docs/test/e2e-catalog.md` 对应条目勾 ✅ 并写
      文件路径（D-1 回写口径：探索通过不算实现，回放固化进仓才算）。

## 4. S4 — 夜间编排与 catalog 回写闸门

- [ ] 4.1 `make explore-nightly`：遍历 `docs/test/explore-charters/*.md`（排除 `_template`），
      逐份跑 2.4 的通道；报告落 `reports/`。**不进必过门禁**——CI 里挂
      `.github/workflows/ci.yml` 的 `schedule` 触发（与既有 nightly delivery job 同一
      `if: github.event_name == 'schedule'` 形状，`ci.yml:499-500`），允许失败不阻塞。
      回写口径：手动 `make explore-nightly`（全假探索者模式）端到端出全部报告；
      CI workflow 语法检查过。
- [ ] 4.2 catalog 回写闸门：脚本断言「catalog 里勾了 ✅ 的 MCP 探索相关条目，其文件路径
      必须存在于 `tests/e2e/`」——防止把只有探索报告、没有回放用例的条目勾上。
      回写口径：人为勾一个无文件的条目，闸门红。
- [ ] 4.3 凭据形状清单不漂移：闸门断言 `tests/e2e/explore` 的 Go 正则与
      `scripts/audit_open_source.py` 的形状清单覆盖同一组前缀（从脚本侧提取清单喂给
      Go 侧单测，或双向 diff）。
      回写口径：改一边漏一边，闸门红。
- [ ] 4.4 `docs/test/explore-charters/README.md`：章程怎么写（frontmatter 字段表）、
      外部 AI 怎么接（拿到连接信息后用标准 MCP 客户端指向 base_url）、报告怎么读
      （verdict vs leads）、什么算「实现」（只有回放固化后才勾 catalog）。
      回写口径：一个没读过 design.md 的人能照它发起一次探索。
- [ ] 4.5 S4 PR：全量 `make test-e2e` + `make audit-open-source` +
      `openspec validate mcp-ai-test-entry --strict` 绿。

## 5. 明确不在本 change

- 5.1 不新增/放宽任何 MCP 工具或可见性（硬约束 2）；探索暴露的契约缺陷另立 change。
- 5.2 不接 gatereport（D-5 裁决：目标相反，见 design §3 D-5）。
- 5.3 不做独立签发器或测试专用铸造端点（D-4 裁决：复用 `/v1/agentteams/token`，
      随机 JWT secret 已构成结构边界）。
- 5.4 不让探索通道进必过门禁（硬约束 5）。
- 5.5 不绑定任何模型/Agent 框架（硬约束 4）——仓内只到「输出连接信息 + JSONL 轨迹格式」
      为止，接入方自带 MCP 客户端。
