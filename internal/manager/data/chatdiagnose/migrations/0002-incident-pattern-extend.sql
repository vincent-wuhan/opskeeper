-- 0002-incident-pattern-extend.sql
-- ALTER 现有 incident_pattern 表，新增 fingerprint / severity / confidence。
-- 不新建表，不动现有字段；path A 约束（AGENTS.md §"数据存储"）：
--   - 兼容滚动发布（仅 ADD COLUMN，旧部署仍可读旧 schema）
--   - 多租户：UNIQUE INDEX 复合 (tenant_id, fingerprint) 由 GORM AutoMigrate 建（不在这里手动建，避免冲突）

ALTER TABLE incident_pattern
    ADD COLUMN fingerprint VARCHAR(64) NULL AFTER source_postmortem_id,
    ADD COLUMN severity    VARCHAR(16) NULL AFTER fingerprint,
    ADD COLUMN confidence  DECIMAL(3,2) NULL AFTER severity;

-- 注意：UNIQUE INDEX uniq_tenant_fingerprint 由 GORM AutoMigrate 根据
-- IncidentPattern.Fingerprint 的 gorm:"uniqueIndex:uniq_tenant_fingerprint" tag
-- 自动创建。本文件不重复 CREATE INDEX。
