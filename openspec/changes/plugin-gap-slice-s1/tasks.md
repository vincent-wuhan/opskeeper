# Tasks: plugin-gap-slice-s1

对应 4.8 方案 §5.5 切片 S-1（G0 修复）与 §6.2 的 D-2 裁决（BaseTool 桥，一实现两面）。

## 1. OpenSpec change

- [x] 1.1 开 `openspec/changes/plugin-gap-slice-s1/`，proposal 关联 4.8（§4 / §5.5 / §6.2）
- [x] 1.2 `design.md` 记录 6 条实现约束与测试策略（作为实现输入）
- [x] 1.3 `specs/plugin-gap-slice-s1/spec.md` 写可验收的 Requirement / Scenario

## 2. BaseTool 上呼桥

- [x] 2.1 新增 `cmd/opskeeper/basetool_upcall.go`：`baseToolUpcall` 持有可服务工具与"知道但不服务"两个集合
- [x] 2.2 桥从**独立的** `Registry.BuildBaseTools()` 实例取工具，不复用 `mcpBaseBag` / 对话 bag
- [x] 2.3 收敛口径 `Info().Class == "read"`；非只读工具进入"知道但不服务"集合
- [x] 2.4 显式保留名单 `host_bash` / `ToolSearch` / `chat_to_query` / `draft_config_change`，每项写明理由
- [x] 2.5 复用 `decorators.Wrap`：`Timeout 15s` + `Limiter` + `Registerer`，`Deps.Audit` 留空，`ReviewSpawner` 不接
- [x] 2.6 不重复包 `MarkUntrustedOutput`（由 `BuildBaseTools()` 内部完成）
- [x] 2.7 miss 措辞区分"知道但不服务"与"这里没有"

## 3. 装配与分派

- [x] 3.1 桥在 `toolWiring.apply(toolsReg)` 之后、`mcpBaseBag` 同一块装配
- [x] 3.2 `agentToolUpcall` 增加 `basetool` 面
- [x] 3.3 `dispatchAgentTool` 顺序：中间件 family → 桥 → 闭包注册表；**同名以桥优先**（schema 来自 BaseTool）
- [x] 3.4 surface 由分派结果给出，不再由工具名推断
- [x] 3.5 `agenttool_audit.go` 的 `surface` 取值集合与注释更新

## 4. 对账闸门

- [x] 4.1 上呼可服务名来自真实构造（全依赖 fake → `BuildBaseTools()` → 桥）
- [x] 4.2 节点本地技能名来自真实构造（`skill.All()`）
- [x] 4.3 断言：两个包的 manifest 工具名 ∈（上呼可服务 ∪ 节点本地技能）
- [x] 4.4 负例：拿掉桥（复原 G0 状态）后闸门报出且只报出那 6 个工具——真红
- [x] 4.5 闸门范围限于 `opskeeper-sre-observability` 与 `opskeeper-sre-readonly`，范围与理由写进测试注释
- [x] 4.6 不做反向断言；`rank_edges` / `correlate_incident` 不进任何 manifest

## 5. fake tunnel 集成用例

- [x] 5.1 6 个 G0 工具各自上呼成功，返回非空 JSON
- [x] 5.2 审计行 `surface == "basetool"`
- [x] 5.3 只读闸门的镜像负例：写类工具经桥被拒（不是 "no such tool"）

## 6. 验收

- [x] 6.1 `make pi-plugin-guard && make module-check && go test ./scripts/ledgercheck`
- [x] 6.2 `go test ./...` 全量
- [x] 6.3 `make audit-open-source` 通过（受审计文件里没有内部任务编号与绝对路径）
- [x] 6.4 PR：中文说明 + 贡献指南勾选项 + Conventional Commit
- [x] 6.5 `scripts/deadcode` 棘轮不涨：桥的两个自省入口（`serviceableNames` / `withheldNames`）由启动日志调用，从生产调用图可达，因此本切片不新增任何"仅测试可达"或"完全不可达"符号

## 7. 明确不在本切片

- [ ] 7.1 5.1–5.4 的 manifest 增量（S-2/S-3）
- [ ] 7.2 "已声明但依赖缺失"的就地降级（S-3）
- [ ] 7.3 反向断言与 §7 工具的 manifest 收录
- [ ] 7.4 闭包注册表的任何新条目
