# Multi-agent 7-stage E2E (real-world ops scenario)

`runner.py` 模拟 7 个 AgentTeams Worker 身份，每个 stage 用专属 Higress apiKey + HMAC + Worker-Identity：

| Stage | Role | Consumer | Tools |
|---|---|---|---|
| 1 | alerter | opskeeper-alerter | （内部） |
| 2 | investigator | opskeeper-investigator | loop.correlate, loop.investigate |
| 3 | critic | opskeeper-critic | （内部 RCA 复核） |
| 4 | reviewer | opskeeper-reviewer | HITL approve |
| 5 | repairer | opskeeper-repairer | recovery.execute |
| 6 | verifier | opskeeper-verifier | recovery.verify |
| 7 | reporter | opskeeper-reporter | postmortem 写盘 |

`evidence/` 是 2026-08-27 真实运行产物：verify_report.passed=true，verify_warning_level=pass，audit_log 全成功。

运行（在容器内）：
```
docker exec opskeeper-test python3 /tmp/7stage_multiagent.py
```