-- 0001-agentteams-proposal-idempotency.sql
-- Expand phase: AgentTeams create retries use a deterministic nullable key.

ALTER TABLE proposal
    ADD COLUMN idempotency_key VARCHAR(191) NULL AFTER message_id,
    ADD COLUMN execution_lease_expires_at DATETIME(6) NULL AFTER matrix_event_id,
    ADD UNIQUE KEY idx_proposal_idempotency_key (idempotency_key),
    ADD KEY idx_proposal_execution_lease_expires_at (execution_lease_expires_at);

-- Contract phase after old application versions retire:
-- DROP INDEX idx_proposal_execution_lease_expires_at ON proposal;
-- DROP INDEX idx_proposal_idempotency_key ON proposal;
-- ALTER TABLE proposal
--     DROP COLUMN execution_lease_expires_at,
--     DROP COLUMN idempotency_key;
