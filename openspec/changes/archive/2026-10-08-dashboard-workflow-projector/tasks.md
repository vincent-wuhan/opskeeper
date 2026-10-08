# Dashboard Workflow Projector Tasks

- [x] 1.1 Define the incident-derived run model, fixed stages, Worker mapping, and persisted task-to-run index.
- [x] 1.2 Emit human-readable Matrix notices with a complete `agentteams.workflow` payload.
- [x] 1.3 Hook request acceptance, successful dispatch, matching Worker result, and explicit admin approval/rejection transitions.
- [x] 1.4 Add focused tests for payload shape, stage progression, duplicate suppression, approval transitions, and Matrix failure isolation.
- [x] 1.5 Rebase to latest `main`, run focused Python/dashboard/package validation, and bump TeamHarness to `1.0.59`.
- [x] 1.6 Build packages locally, deploy to the public environment, and verify Element + Dashboard synchronized display and task-board stage advancement in a real E2E.（公网证据：run `201` 五阶段 Matrix 事件、Element “已恢复”截图、Dashboard “已完成”任务板截图。）
