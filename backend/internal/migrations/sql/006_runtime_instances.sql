-- Backend instances heartbeat while they own host-side CLI processes.  The
-- heartbeat lets another desktop launch recover only work from a dead process,
-- without touching a second live desktop instance connected to the same DB.
CREATE TABLE IF NOT EXISTS runtime_instances (
  instance_id TEXT PRIMARY KEY,
  started_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  heartbeat_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS runtime_instances_heartbeat_idx
  ON runtime_instances(heartbeat_at);
