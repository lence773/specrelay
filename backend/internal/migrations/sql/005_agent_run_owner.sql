-- Each backend process owns the CLI runs it started.  This lets a desktop
-- instance reconcile only its own processes during a graceful application exit.
ALTER TABLE agent_runs
  ADD COLUMN IF NOT EXISTS owner_instance_id TEXT NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS agent_runs_owner_active_idx
  ON agent_runs(owner_instance_id, started_at)
  WHERE status = 'running';
