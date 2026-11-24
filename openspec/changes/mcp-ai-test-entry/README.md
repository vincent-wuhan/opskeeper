# mcp-ai-test-entry

把「外部 AI 经 MCP 契约验证 OpsKeeper」立成仓内一条可重复的测试通道。OpsKeeper 对外
已经是一个真实的 MCP server（`docs/mcp-surface.md`），但这份契约至今只被两类东西碰过：
九条协议面测试，和仓内自己写的 e2e。**像一个真实外部 Agent 那样用这份契约**，从来没有
被系统性做过。

本 change 的立场只有一句：**AI 当探索者，不当裁判。**调用序列由外部 AI 生成；判决由
确定性代码基于审计链与调用轨迹给出。AI 觉得不对的地方记为线索，不作合并门禁。

当前阶段：**仅 proposal**。design / specs / tasks 由架构守护者出方案，设计必须回答
proposal 末尾的五个待决问题，不得绕开。
