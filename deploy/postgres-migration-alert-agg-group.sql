-- BENY-915 告警聚合组：同设备 10min 窗口内 incident 归组，组 ID = 组内最早 incident ID。
ALTER TABLE alert_incidents ADD COLUMN IF NOT EXISTS agg_group_id BIGINT NULL;
CREATE INDEX IF NOT EXISTS idx_alert_incidents_agg_group ON alert_incidents (agg_group_id);
