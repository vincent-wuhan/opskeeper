# Synthetic incident data sources

The incident examples in `deploy/incident-events/` are synthetic datasets created from common PostgreSQL operations knowledge. They are designed to exercise alert intake, timeline replay, diagnosis, approval, recovery, and metrics without depending on a named production incident.

## Data boundary

- No customer, employee, operator, or business identifier is included.
- No production telemetry, private endpoint, credential, token, or secret is included.
- Incident IDs, trace IDs, timestamps, and actors are deterministic demo values.
- The examples use tenant `opskeeper-demo`.
- Results from synthetic data must not be presented as a public benchmark or customer outcome.

## Included scenarios

| Scenario | Purpose |
|---|---|
| PostgreSQL connection-pool exhaustion | Exercise saturation diagnosis, exact-target approval, recovery, and probe verification |
| PostgreSQL lock wait and long transaction | Exercise dependency analysis, blast-radius reasoning, and rollback decisions |
| PostgreSQL disk I/O saturation | Exercise evidence correlation and capacity checks |
| PostgreSQL replica replay lag | Exercise topology-aware verification and recovery signals |

## Example query

```bash
curl -H "Authorization: Bearer $OPSKEEPER_TOKEN" \
  "$OPSKEEPER_BACKEND_URL/api/v1/incidents/metrics?tenant_id=opskeeper-demo"
```
