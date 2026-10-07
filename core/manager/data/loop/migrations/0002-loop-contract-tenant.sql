-- 0002-loop-contract-tenant.sql
-- Expand phase: add tenant ownership to loop_contract.
-- GORM AutoMigrate applies the model change; this file documents the DBA path.

ALTER TABLE loop_contract
    ADD COLUMN tenant_id VARCHAR(64) NOT NULL DEFAULT '' AFTER incident_id,
    ADD KEY idx_contract_tenant (tenant_id);
