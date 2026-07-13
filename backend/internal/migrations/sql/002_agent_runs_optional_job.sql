ALTER TABLE agent_runs ALTER COLUMN job_id DROP NOT NULL;
CREATE INDEX agent_runs_project_created_idx ON agent_runs(project_id, created_at DESC);
