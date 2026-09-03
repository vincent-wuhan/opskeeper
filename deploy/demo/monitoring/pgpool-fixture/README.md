# Public PG pool logical monitoring fixture

These scripts support the public demo only. They do not replace a node_exporter,
Promtail, or edge-agent deployment, and their series are labeled
`fixture="pg-pool-demo"` plus `opskeeper_source="embedded"`.

- `node_metrics_server.py` exposes the standard metric names consumed by the
  OpsKeeper Monitor panels.
- `log_source.py` seeds one hour of logical history and pushes a heartbeat to
  Loki every 15 seconds.

## Run

Start the metrics endpoint and the logical log source with Python 3.11+:

```bash
python3 node_metrics_server.py
LOKI_URL=http://loki:3100 python3 log_source.py
```

Add the metrics endpoint to Prometheus, replacing `opskeeper-demo-node-metrics`
with the actual DNS name reachable from Prometheus:

```yaml
scrape_configs:
  - job_name: opskeeper-demo-node-logical
    static_configs:
      - targets: ["opskeeper-demo-node-metrics:8095"]
```

Verify the two health paths before using the fixture in a rehearsal:

```bash
curl http://localhost:8095/healthz
curl http://localhost:8095/metrics | head
```

The public fixture currently binds these series to device `900001`. Any
presentation or evidence document must describe the source as a logical demo
monitoring fixture rather than a real host edge agent.
