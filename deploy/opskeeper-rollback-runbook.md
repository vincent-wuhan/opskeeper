# OpsKeeper rollback runbook

This generic runbook restores an OpsKeeper deployment after a failed demo, upgrade, or configuration change. Adapt paths, database names, backup retention, and approval requirements to your environment.

## Principles

1. Stop new mutations before recovery.
2. Record the incident ID, operator, time, release tag, and reason.
3. Restore the control database and knowledge index to the same recovery point.
4. Verify identity, MCP, incident timeline, proposal, and audit endpoints before reopening user traffic.
5. Keep failed artifacts for diagnosis; do not overwrite the rollback anchor.

## Control database rollback

1. Pause scheduled recovery and HITL timeout workers.
2. Record the current database migration version and active incident count.
3. Restore the approved PostgreSQL backup into the target control database.
4. Replay or verify the migration version recorded with that backup.
5. Confirm required tables and indexes exist.

## Knowledge index rollback

1. Stop indexing jobs.
2. Restore the approved Qdrant collection snapshot.
3. Verify collection count, dimension, distance metric, and last successful indexing job.
4. Run a known read-only recall query and retain the candidate ranking.

## Runtime rollback

1. Record the current image digest and configuration hash.
2. Restore the previous approved image and configuration pair; never mix unrelated versions.
3. Restart services and wait for health checks.
4. Verify the version endpoint, database connectivity, object storage availability, MCP authorization, and audit write path.
5. Trigger one read-only synthetic incident and confirm timeline, metrics, and audit events.

## Exit criteria

- All health checks pass.
- The web console and API version match the rollback record.
- A read-only synthetic scenario completes end to end.
- The rollback record contains operator, approval, timestamps, source and target versions, backup IDs, and verification evidence.
