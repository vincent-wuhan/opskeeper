# ModelScope Source and Route Baseline

- Generated (UTC): `2026-09-19T12:32:05Z`
- Generated (Asia/Shanghai): `2026-09-19T20:32:05+08:00`
- GitHub repository: https://github.com/vincent-wuhan/opskeeper
- Default branch: `main`
- Current HEAD: `0964a170d7748b8f7c256930590693971b2bbf2e`
- `origin/main`: `fadeafa9bec297096bb8f712d03a1c55a3e6a6ca`

## Route Preflight

HEAD is used first. GET is the fallback when HEAD is unreachable or returns a failing status. Redirects are recorded as redirect statuses rather than followed.

| Entry | Result | Reachable | Effective method | Probe | Route |
|---|---|---:|---|---|---|
| official_website | PASS | yes | HEAD | HEAD 200 | https://opskeeper.yueming.xin |
| opskeeper_service | PASS | yes | HEAD | HEAD 200 | https://opskeeper.yueming.xin |
| agentteams_rooms | PASS | yes | HEAD | HEAD 200 | https://rooms.yueming.xin |
| agentteams_dashboard | PASS | yes | HEAD | HEAD 200 | https://teams.yueming.xin |
| roadshow_console | PASS | yes | HEAD | HEAD 200 | https://opskeeper.yueming.xin/live-incident |

## Summary

- Total entries: 5
- Pass: 5
- Auth-protected and reachable: 0
- Fail: 0
- Required public entries unreachable: 0
- Preflight result: PASS
- Exit code: 0

Auth-protected (`401` or `403`) routes are considered reachable. HTTP `2xx` and `3xx` statuses pass. All other HTTP statuses and network errors fail.
