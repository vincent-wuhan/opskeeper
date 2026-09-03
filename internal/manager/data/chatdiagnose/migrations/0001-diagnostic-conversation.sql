-- 0001-diagnostic-conversation.sql
-- conversational-diagnosis (D13-D16) 数据 schema。三张表：
--   - diagnostic_conversation：会话事实源（DB 单一源 + rehydration）
--   - diagnostic_turn：append-only turn 流（含 llm_context_snapshot JSONB）
--   - incident_pattern：KB-first 检索表（feature kb_first 开启后）
--
-- 路径 A 约束（AGENTS.md §"数据存储"）：
--   - PII：turn.content 含用户/助手对话原文，需在 data-guard-classification
--     落地时脱敏；本 schema 不做脱敏，由上游 redact 层负责。
--   - 单调追加：turn 表 append-only；linked_loop_event_id / linked_root_cause_id
--     是仅有的两个允许 UPDATE 的字段（spec §"Append-only 契约例外"）。
--   - 多租户：所有表带 tenant_id 并按其过滤。
--
-- 由 store/migrate.go::Migrate 通过 gorm.AutoMigrate 应用；本文件为
-- DBA peer review 参考。

CREATE TABLE IF NOT EXISTS diagnostic_conversation (
    id                VARCHAR(64)  PRIMARY KEY,
    tenant_id         VARCHAR(64)  NOT NULL,
    user_id           VARCHAR(64)  NOT NULL,
    title             VARCHAR(256) NULL,
    root_incident_id  VARCHAR(64)  NULL,
    metadata          JSON         NULL,
    created_at        DATETIME(3)  NOT NULL,
    updated_at        DATETIME(3)  NOT NULL,

    KEY idx_tenant (tenant_id),
    KEY idx_user (user_id),
    KEY idx_root_incident (root_incident_id),
    KEY idx_created_at (created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS diagnostic_turn (
    id                      BIGINT       AUTO_INCREMENT PRIMARY KEY,
    conversation_id         VARCHAR(64)  NOT NULL,
    role                    VARCHAR(16)  NOT NULL,
    content                 TEXT         NOT NULL,
    tool_calls              JSON         NULL,
    tool_results            JSON         NULL,
    linked_loop_event_id    BIGINT       NULL,
    linked_root_cause_id    BIGINT       NULL,
    llm_context_snapshot    JSON         NULL,
    trace_id                VARCHAR(64)  NULL,
    created_at              DATETIME(3)  NOT NULL,

    KEY idx_conversation (conversation_id),
    KEY idx_linked_loop_event (linked_loop_event_id),
    KEY idx_linked_root_cause (linked_root_cause_id),
    KEY idx_trace_id (trace_id),
    KEY idx_created_at (created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- turn 表 append-only 防护（spec §"Append-only 契约"）。
-- 仅允许 linked_loop_event_id / linked_root_cause_id 两个字段 UPDATE。
-- 应用层用 Updates(map[string]any{...}) 显式控制；这里不挂 trigger
-- 以免阻断合法的"反向引用闭环"写。

CREATE TABLE IF NOT EXISTS incident_pattern (
    id                     BIGINT       AUTO_INCREMENT PRIMARY KEY,
    tenant_id              VARCHAR(64)  NOT NULL,
    resource_type          VARCHAR(32)  NOT NULL,
    symptom                VARCHAR(128) NULL,
    root_cause_object      VARCHAR(128) NULL,
    signature              TEXT         NULL,
    embedding              JSON         NULL,
    hit_count              INT          NOT NULL DEFAULT 0,
    last_hit_at            DATETIME(3)  NULL,
    source_postmortem_id   VARCHAR(64)  NULL,
    created_at             DATETIME(3)  NOT NULL,
    updated_at             DATETIME(3)  NOT NULL,

    KEY idx_tenant_resource (tenant_id, resource_type),
    KEY idx_source_postmortem (source_postmortem_id),
    KEY idx_created_at (created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
