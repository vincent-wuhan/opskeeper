-- 0003-incident-pattern-lexical-candidates.sql
-- 为 BM25 候选预过滤提供稳定的租户扫描顺序。
-- 仅新增二级索引，不改表结构和数据，兼容滚动发布。

CREATE INDEX idx_incident_pattern_tenant_updated_at_id
    ON incident_pattern (tenant_id, updated_at, id);

-- 局限：生产词法召回仍使用跨方言参数化 LIKE，不创建 MySQL FULLTEXT/ngram
-- 索引。该索引只保证 tenant_id 过滤和 updated_at/id 排序；LIKE 条件仍需
-- 在 bounded candidate limit 内做行过滤，SQLite/PostgreSQL 部署无需方言分支。
--
-- Rollback (expand-contract: only after the previous application version is retired):
-- DROP INDEX idx_incident_pattern_tenant_updated_at_id;
