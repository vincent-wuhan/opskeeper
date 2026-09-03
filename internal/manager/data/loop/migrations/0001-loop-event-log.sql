-- 0001-loop-event-log.sql
-- closed-loop orchestrator 三表 schema（loop_event_log / loop_contract /
-- loop_state）。由 store/migrate.go::Migrate 通过 gorm.AutoMigrate
-- 应用；本 SQL 文件为人工复审参考 + DBA peer review 用。
--
-- 路径 A 约束（AGENTS.md §"数据存储"）：
--   - 生产 schema 变更走 migration 文件；本文件即此目的。
--   - 大表用在线 DDL 工具；expand-contract 兼容滚动发布。
--   - PII 不入此表（incident_id / tenant_id 是审计维度，非 PII）。
--
-- 多租户：
--   loop_event_log 必须带 tenant_id 并按其过滤；
--   loop_contract 当前按 incident_id 隔离（incident 维度已天然
--   多租户；未来加 tenant_id 列时本文件扩 schema、读路径加 WHERE）。
--   loop_state 同 loop_contract。

CREATE TABLE IF NOT EXISTS loop_event_log (
    id                BIGINT AUTO_INCREMENT PRIMARY KEY,
    incident_id       VARCHAR(64)  NOT NULL,
    tenant_id         VARCHAR(64)  NOT NULL,
    event_type        VARCHAR(32)  NOT NULL,
    phase             VARCHAR(32)  NOT NULL,
    idempotency_key   VARCHAR(64)  NOT NULL,
    payload           JSON         NULL,
    trace_id          VARCHAR(64)  NULL,
    created_at        DATETIME(3)  NOT NULL,

    UNIQUE KEY uniq_idempotency_key (idempotency_key),
    KEY idx_tenant_created (tenant_id, created_at),
    KEY idx_incident (incident_id),
    KEY idx_event_type (event_type),
    KEY idx_phase (phase),
    KEY idx_trace_id (trace_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- append-only 防护：禁止 UPDATE / DELETE，应用层只能 INSERT。
-- MySQL 通过 trigger 实现。
DELIMITER $$
CREATE TRIGGER trg_loop_event_log_no_update
BEFORE UPDATE ON loop_event_log
FOR EACH ROW
BEGIN
    SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'loop_event_log is append-only';
END$$
CREATE TRIGGER trg_loop_event_log_no_delete
BEFORE DELETE ON loop_event_log
FOR EACH ROW
BEGIN
    SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'loop_event_log is append-only';
END$$
DELIMITER ;

CREATE TABLE IF NOT EXISTS loop_contract (
    id                BIGINT AUTO_INCREMENT PRIMARY KEY,
    incident_id       VARCHAR(64)  NOT NULL,
    phase             VARCHAR(32)  NOT NULL,
    type              VARCHAR(32)  NOT NULL,
    schema_version    VARCHAR(16)  NOT NULL,
    payload           JSON         NOT NULL,
    size_bytes        INT          NOT NULL DEFAULT 0,
    storage_backend   VARCHAR(16)  NOT NULL DEFAULT 'db',
    created_at        DATETIME(3)  NOT NULL,

    KEY idx_incident (incident_id),
    KEY idx_phase (phase),
    KEY idx_incident_phase_type (incident_id, phase, type, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS loop_state (
    incident_id       VARCHAR(64)  PRIMARY KEY,
    current_phase     VARCHAR(32)  NULL,
    last_event_id     BIGINT       NOT NULL DEFAULT 0,
    retry_count       INT          NOT NULL DEFAULT 0,
    updated_at        DATETIME(3)  NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
