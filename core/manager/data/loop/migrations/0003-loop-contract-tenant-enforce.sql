-- 0003-loop-contract-tenant-enforce.sql
-- ⑥ tenant_id 强制（多租户安全）：
--   1) 把历史 tenant_id='' 的行（0001 / 0002 迁移前残留）打到 '__legacy__'
--      桶，让它们孤立、可审计、不参与跨租户读路径。线上脚本必须先 SELECT
--      看 affected_rows 量级；>0 时人工 review 后再跑。
--   2) DROP DEFAULT '' —— 写入时不再兜底为空字符串；写路径已经有
--      "tenantID required" 校验（WriteContract），DB 层再加一层防呆。
--   3) NOT NULL 已经从 0001 起生效；这里只调整 default。
--
-- 注意：本迁移是 schema-only 的，执行后需要 GORM AutoMigrate 同步。
-- 应用层 Contract model 的 GORM tag 同步更新：
--   `default:''`  → 不带 default
--
-- 回滚：手动 ALTER ... SET DEFAULT ''，并 UPDATE loop_contract SET
-- tenant_id='' WHERE tenant_id='__legacy__'；不在本迁移里写回滚脚本以
-- 避免和上面的强制语义冲突。

-- Step 1: backfill empty tenant_id to '__legacy__' bucket.
-- Pre-check: SELECT COUNT(*) FROM loop_contract WHERE tenant_id = '';
--          if >0, log and review before applying.
UPDATE loop_contract
   SET tenant_id = '__legacy__'
 WHERE tenant_id = '';

-- Step 2: drop the empty default (writeContract already enforces non-empty).
ALTER TABLE loop_contract
    ALTER COLUMN tenant_id DROP DEFAULT;

-- Step 3: tighten the index. The 0002 idx_contract_tenant(tenant_id) is
-- already in place; if it were missing, add it:
--   ALTER TABLE loop_contract ADD KEY idx_contract_tenant (tenant_id);
-- 0002 already added it, so this is a no-op guard for environments that
-- only ran 0001.

-- Step 4: a CHECK constraint would be ideal but MySQL <8.0.16 ignores
-- them. App layer (WriteContract) is the enforcement point; do NOT
-- remove that check.