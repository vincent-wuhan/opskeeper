# ModelScope Source and Route Baseline

- Generated (UTC): `2026-09-19T07:23:39Z`
- Generated (Asia/Shanghai): `2026-09-19T15:23:39+08:00`
- GitHub repository: https://github.com/vincent-wuhan/opskeeper
- Default branch: `main`
- Current HEAD: `1cc5e3de6c229ee937219d25d80195d559907e26`
- `origin/main`: `be44e36c7f1dd987e863a8732965db25aea1db28`

## Route Preflight

HEAD is used first. GET is the fallback when HEAD is unreachable or returns a failing status. Redirects are recorded as redirect statuses rather than followed.

| Entry | Result | Reachable | Effective method | Probe | Route |
|---|---|---:|---|---|---|
| official_website | PASS | yes | HEAD | HEAD 200 | https://opskeeper.yueming.xin/home |
| opskeeper_service | PASS | yes | HEAD | HEAD 200 | https://opskeeper.yueming.xin |
| agentteams_rooms | PASS | yes | HEAD | HEAD 200 | https://rooms.yueming.xin |
| agentteams_dashboard | PASS | yes | HEAD | HEAD 200 | https://teams.yueming.xin |
| roadshow_console | PASS | yes | HEAD | HEAD 302 | https://home.yueming.xin |

## Summary

- Total entries: 5
- Pass: 5
- Auth-protected and reachable: 0
- Fail: 0
- Required public entries unreachable: 0
- Preflight result: PASS
- Exit code: 0

Auth-protected (`401` or `403`) routes are considered reachable. HTTP `2xx` and `3xx` statuses pass. All other HTTP statuses and network errors fail.
