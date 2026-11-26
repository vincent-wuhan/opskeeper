---
# 探索章程（explore charter）
# 字段集是封闭的：校验器（tests/e2e/explore/charter_test.go）拒绝任何
# 未知字段。新增字段必须同时改校验器与本模板。
id: <kebab-case-id>            # 必填。全目录唯一。
role: <agentteams-role>        # 必填。AgentTeams 角色集之一（alerter /
                               # investigator / critic / reviewer /
                               / repairer / verifier / reporter）。
                               # 校验器从 AgentTeamsWorkerPermissions()
                               # 枚举，不手抄名单。
worker: opskeeper-<role>       # 必填。签发令牌时的 worker 名，必须与
                               # 角色绑定（AgentTeamsWorkerForRole）。
tenant: <tenant-id>            # 必填。令牌绑定的租户。审计行里的租户
                               # 必须全部等于它（判决器 I-4）。
max_tool_calls: <positive-int> # 必填。本次探索允许的 tools/call 次数上限。
max_steps: <positive-int>      # 必填。本次探索允许的步骤数上限（含
                               # initialize 与 tools/list）。任一预算触顶
                               # 即判 incomplete，不是 pass。
catalog_refs:                  # 必填。参照的 e2e 用例编号（docs/test/
  - <domain-number>            # e2e-catalog.md 的行，如 B1、E4）。
                               # 校验器逐项解析到真实行。
---

# <章程标题>

## 目标

<一句话：这次探索要验证的链路。写客观事实（"令牌→MCP→审计"），
不写预期结论（"应该会通过"）——预期结论是判决器的事，不是章程的事。>

## 允许的工具

<列工具名。预算判决断言 trace 里的每个 tools/call ∈ 本角色矩阵；
不在 tools/list 里的工具被调用必须失败（I-2）。>

## 判决

<本章程覆盖的判决项（I-1..I-5 的子集）与各自在审计上的形状。只读
章程的 I-3 是 not-exercised，不是 pass。>
