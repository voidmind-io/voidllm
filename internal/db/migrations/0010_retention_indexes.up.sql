-- Single-column timestamp indexes for retention cleanup queries.
-- The existing composite indexes (idx_usage_org_time, idx_audit_logs_org_time)
-- have org_id as leading column, so they are not usable by the cleanup query
-- which filters only by timestamp.
CREATE INDEX IF NOT EXISTS idx_usage_events_created_at
  ON usage_events (created_at);
CREATE INDEX IF NOT EXISTS idx_audit_logs_timestamp
  ON audit_logs (timestamp);
