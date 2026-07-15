-- Extend agent_runs with compact, structured observability metadata. The new
-- classification and usage columns intentionally have no defaults: older rows
-- cannot be classified reliably and must remain NULL rather than being guessed.
ALTER TABLE agent_runs
  ADD COLUMN intake_id UUID REFERENCES intakes(id) ON DELETE SET NULL,
  ADD COLUMN plan_id UUID REFERENCES plans(id) ON DELETE SET NULL,
  ADD COLUMN logical_operation_id UUID,
  ADD COLUMN operation_type TEXT,
  ADD COLUMN job_attempt INTEGER CHECK (job_attempt > 0),
  ADD COLUMN retry_count INTEGER CHECK (retry_count >= 0),
  ADD COLUMN session_mode TEXT CHECK (session_mode IN ('new','reused','snapshot_restored','not_applicable')),
  ADD COLUMN session_invalidation_reason TEXT CHECK (session_invalidation_reason IN ('provider_switched','session_not_found','restore_failed')),
  ADD COLUMN queue_wait_ms BIGINT CHECK (queue_wait_ms >= 0),
  ADD COLUMN failure_category TEXT,
  ADD COLUMN output_bytes BIGINT CHECK (output_bytes >= 0),
  ADD COLUMN output_lines BIGINT CHECK (output_lines >= 0),
  ADD COLUMN event_count BIGINT CHECK (event_count >= 0),
  ADD COLUMN output_truncated BOOLEAN,
  ADD COLUMN input_tokens BIGINT CHECK (input_tokens >= 0),
  ADD COLUMN output_tokens BIGINT CHECK (output_tokens >= 0),
  ADD COLUMN total_tokens BIGINT CHECK (total_tokens >= 0),
  ADD COLUMN cost_amount NUMERIC CHECK (cost_amount >= 0),
  ADD COLUMN cost_currency TEXT,
  ADD CONSTRAINT agent_runs_cost_currency_pair_check CHECK (
    (cost_amount IS NULL AND cost_currency IS NULL)
    OR (cost_amount IS NOT NULL AND cost_currency IS NOT NULL AND cost_currency <> '')
  );

-- duration_ms used to default to zero while a run was active. A zero on an old
-- row is therefore ambiguous, so preserve only positive historical durations.
ALTER TABLE agent_runs
  ALTER COLUMN duration_ms DROP NOT NULL,
  ALTER COLUMN duration_ms DROP DEFAULT;
UPDATE agent_runs SET duration_ms=NULL WHERE duration_ms=0;

CREATE INDEX agent_runs_started_idx
  ON agent_runs(started_at DESC);
CREATE INDEX agent_runs_provider_started_idx
  ON agent_runs(provider, started_at DESC);
CREATE INDEX agent_runs_intake_started_idx
  ON agent_runs(intake_id, started_at DESC)
  WHERE intake_id IS NOT NULL;
CREATE INDEX agent_runs_plan_started_idx
  ON agent_runs(plan_id, started_at DESC)
  WHERE plan_id IS NOT NULL;
CREATE INDEX agent_runs_task_started_idx
  ON agent_runs(task_id, started_at DESC)
  WHERE task_id IS NOT NULL;
CREATE INDEX agent_runs_logical_operation_idx
  ON agent_runs(logical_operation_id, created_at ASC)
  WHERE logical_operation_id IS NOT NULL;
