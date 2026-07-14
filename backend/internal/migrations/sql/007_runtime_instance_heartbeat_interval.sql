-- A runtime instance records its declared heartbeat cadence. Recovery waits for
-- three missed beats (with a 30-second floor), so a live desktop configured
-- with a slower lease heartbeat is never treated as a crashed process.
ALTER TABLE runtime_instances
  ADD COLUMN IF NOT EXISTS heartbeat_interval_ms BIGINT NOT NULL DEFAULT 10000
  CHECK (heartbeat_interval_ms > 0);
