# datasource-victoriametrics-slice

Land the first third-party datasource package — VictoriaMetrics — as a real
`plugins/<pkg>/` Go module, activated through the same kernel gate the
first-party Prometheus package is.

Implements §3 候选 1 and the §6 skeleton of
`openspec/changes/plugin-architecture-recovery-plan/datasource-plugin-plan.md`.
Decisions, rejected alternatives, and the risk register are in `design.md`.