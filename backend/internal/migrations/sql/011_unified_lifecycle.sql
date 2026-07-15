-- P001: one lifecycle contract for plans, tasks, jobs, and agent runs.
-- timed_out remains a readable legacy Agent Run state, but the trigger below
-- prevents new executions from entering it.
ALTER TABLE plans DROP CONSTRAINT IF EXISTS plans_status_check;
ALTER TABLE plans ADD CONSTRAINT plans_status_check CHECK (status IN (
  'generating','ready','running','validating','blocked','cancelling',
  'completed','failed','interrupted','cancelled'
));

ALTER TABLE plan_tasks DROP CONSTRAINT IF EXISTS plan_tasks_status_check;
ALTER TABLE plan_tasks ADD CONSTRAINT plan_tasks_status_check CHECK (status IN (
  'pending','queued','running','cancelling','succeeded','failed','interrupted','cancelled'
));

ALTER TABLE jobs DROP CONSTRAINT IF EXISTS jobs_status_check;
ALTER TABLE jobs ADD CONSTRAINT jobs_status_check CHECK (status IN (
  'queued','leased','running','retry_wait','cancelling',
  'succeeded','failed','interrupted','cancelled'
));

ALTER TABLE agent_runs DROP CONSTRAINT IF EXISTS agent_runs_status_check;
ALTER TABLE agent_runs ADD CONSTRAINT agent_runs_status_check CHECK (status IN (
  'starting','running','cancelling','succeeded','failed','interrupted','cancelled','timed_out'
));

-- Add the shared lifecycle explanation fields as nullable first, backfill every
-- existing row, then make the contract mandatory. This sequence is safe for
-- databases upgraded from any earlier SpecRelay release.
ALTER TABLE plans
  ADD COLUMN status_source TEXT,
  ADD COLUMN reason_code TEXT,
  ADD COLUMN reason TEXT,
  ADD COLUMN last_activity_at TIMESTAMPTZ,
  ADD COLUMN recovery_hint TEXT,
  ADD COLUMN execution_checkpoint JSONB;
ALTER TABLE plan_tasks
  ADD COLUMN status_source TEXT,
  ADD COLUMN reason_code TEXT,
  ADD COLUMN reason TEXT,
  ADD COLUMN last_activity_at TIMESTAMPTZ,
  ADD COLUMN recovery_hint TEXT,
  ADD COLUMN execution_checkpoint JSONB;
ALTER TABLE jobs
  ADD COLUMN status_source TEXT,
  ADD COLUMN reason_code TEXT,
  ADD COLUMN reason TEXT,
  ADD COLUMN last_activity_at TIMESTAMPTZ,
  ADD COLUMN recovery_hint TEXT,
  ADD COLUMN execution_checkpoint JSONB;
ALTER TABLE agent_runs
  ADD COLUMN status_source TEXT,
  ADD COLUMN reason_code TEXT,
  ADD COLUMN reason TEXT,
  ADD COLUMN last_activity_at TIMESTAMPTZ,
  ADD COLUMN recovery_hint TEXT,
  ADD COLUMN execution_checkpoint JSONB;

UPDATE plans SET
  status_source='legacy',
  reason_code=CASE status
    WHEN 'completed' THEN 'completed'
    WHEN 'failed' THEN 'execution_failed'
    WHEN 'cancelled' THEN 'user_cancelled'
    ELSE 'legacy_state'
  END,
  reason=CASE status
    WHEN 'completed' THEN 'plan completed before lifecycle auditing was enabled'
    WHEN 'failed' THEN 'plan failed before lifecycle auditing was enabled'
    WHEN 'blocked' THEN 'plan requires review before execution can continue'
    WHEN 'cancelled' THEN 'plan was cancelled before lifecycle auditing was enabled'
    ELSE 'plan state imported during lifecycle migration'
  END,
  last_activity_at=COALESCE(updated_at,created_at,now()),
  recovery_hint=CASE WHEN status='blocked' THEN 'manual_review' ELSE 'none' END,
  execution_checkpoint='{}'::jsonb;

UPDATE plan_tasks SET
  status_source='legacy',
  reason_code=CASE status
    WHEN 'succeeded' THEN 'completed'
    WHEN 'failed' THEN 'execution_failed'
    WHEN 'cancelled' THEN 'user_cancelled'
    ELSE 'legacy_state'
  END,
  reason=CASE status
    WHEN 'succeeded' THEN 'task completed before lifecycle auditing was enabled'
    WHEN 'failed' THEN 'task failed before lifecycle auditing was enabled'
    WHEN 'cancelled' THEN 'task was cancelled before lifecycle auditing was enabled'
    ELSE 'task state imported during lifecycle migration'
  END,
  last_activity_at=COALESCE(updated_at,started_at,created_at,now()),
  recovery_hint=CASE WHEN status='failed' THEN 'manual_review' ELSE 'none' END,
  execution_checkpoint='{}'::jsonb;

UPDATE jobs SET
  status_source='legacy',
  reason_code=CASE status
    WHEN 'succeeded' THEN 'completed'
    WHEN 'retry_wait' THEN 'automatic_retry'
    WHEN 'failed' THEN 'execution_failed'
    WHEN 'cancelled' THEN 'user_cancelled'
    ELSE 'legacy_state'
  END,
  reason=CASE status
    WHEN 'succeeded' THEN 'job completed before lifecycle auditing was enabled'
    WHEN 'retry_wait' THEN 'job was waiting for an automatic retry during lifecycle migration'
    WHEN 'failed' THEN COALESCE(NULLIF(last_error,''),'job failed before lifecycle auditing was enabled')
    WHEN 'cancelled' THEN COALESCE(NULLIF(last_error,''),'job was cancelled before lifecycle auditing was enabled')
    ELSE 'job state imported during lifecycle migration'
  END,
  last_activity_at=COALESCE(updated_at,created_at,now()),
  recovery_hint=CASE WHEN status='retry_wait' THEN 'automatic_retry' ELSE 'none' END,
  execution_checkpoint='{}'::jsonb;

UPDATE agent_runs SET
  status_source='legacy',
  reason_code=CASE status
    WHEN 'succeeded' THEN 'completed'
    WHEN 'failed' THEN 'execution_failed'
    WHEN 'cancelled' THEN 'user_cancelled'
    WHEN 'timed_out' THEN 'historical_timeout'
    ELSE 'legacy_state'
  END,
  reason=CASE status
    WHEN 'succeeded' THEN 'agent run completed before lifecycle auditing was enabled'
    WHEN 'failed' THEN COALESCE(NULLIF(termination_reason,''),'agent run failed before lifecycle auditing was enabled')
    WHEN 'cancelled' THEN COALESCE(NULLIF(termination_reason,''),'agent run was cancelled before lifecycle auditing was enabled')
    WHEN 'timed_out' THEN COALESCE(NULLIF(termination_reason,''),'historical fixed-duration timeout')
    ELSE 'agent run state imported during lifecycle migration'
  END,
  last_activity_at=COALESCE(updated_at,finished_at,started_at,created_at,now()),
  recovery_hint=CASE WHEN status='timed_out' THEN 'manual_review' ELSE 'none' END,
  execution_checkpoint='{}'::jsonb;

ALTER TABLE plans
  ALTER COLUMN status_source SET DEFAULT 'system', ALTER COLUMN status_source SET NOT NULL,
  ALTER COLUMN reason_code SET DEFAULT 'created', ALTER COLUMN reason_code SET NOT NULL,
  ALTER COLUMN reason SET DEFAULT 'resource created', ALTER COLUMN reason SET NOT NULL,
  ALTER COLUMN last_activity_at SET DEFAULT now(), ALTER COLUMN last_activity_at SET NOT NULL,
  ALTER COLUMN recovery_hint SET DEFAULT 'none', ALTER COLUMN recovery_hint SET NOT NULL,
  ALTER COLUMN execution_checkpoint SET DEFAULT '{}'::jsonb, ALTER COLUMN execution_checkpoint SET NOT NULL;
ALTER TABLE plan_tasks
  ALTER COLUMN status_source SET DEFAULT 'system', ALTER COLUMN status_source SET NOT NULL,
  ALTER COLUMN reason_code SET DEFAULT 'created', ALTER COLUMN reason_code SET NOT NULL,
  ALTER COLUMN reason SET DEFAULT 'resource created', ALTER COLUMN reason SET NOT NULL,
  ALTER COLUMN last_activity_at SET DEFAULT now(), ALTER COLUMN last_activity_at SET NOT NULL,
  ALTER COLUMN recovery_hint SET DEFAULT 'none', ALTER COLUMN recovery_hint SET NOT NULL,
  ALTER COLUMN execution_checkpoint SET DEFAULT '{}'::jsonb, ALTER COLUMN execution_checkpoint SET NOT NULL;
ALTER TABLE jobs
  ALTER COLUMN status_source SET DEFAULT 'system', ALTER COLUMN status_source SET NOT NULL,
  ALTER COLUMN reason_code SET DEFAULT 'created', ALTER COLUMN reason_code SET NOT NULL,
  ALTER COLUMN reason SET DEFAULT 'resource created', ALTER COLUMN reason SET NOT NULL,
  ALTER COLUMN last_activity_at SET DEFAULT now(), ALTER COLUMN last_activity_at SET NOT NULL,
  ALTER COLUMN recovery_hint SET DEFAULT 'none', ALTER COLUMN recovery_hint SET NOT NULL,
  ALTER COLUMN execution_checkpoint SET DEFAULT '{}'::jsonb, ALTER COLUMN execution_checkpoint SET NOT NULL;
ALTER TABLE agent_runs
  ALTER COLUMN status_source SET DEFAULT 'system', ALTER COLUMN status_source SET NOT NULL,
  ALTER COLUMN reason_code SET DEFAULT 'created', ALTER COLUMN reason_code SET NOT NULL,
  ALTER COLUMN reason SET DEFAULT 'resource created', ALTER COLUMN reason SET NOT NULL,
  ALTER COLUMN last_activity_at SET DEFAULT now(), ALTER COLUMN last_activity_at SET NOT NULL,
  ALTER COLUMN recovery_hint SET DEFAULT 'none', ALTER COLUMN recovery_hint SET NOT NULL,
  ALTER COLUMN execution_checkpoint SET DEFAULT '{}'::jsonb, ALTER COLUMN execution_checkpoint SET NOT NULL;

DO $$
DECLARE table_name TEXT;
BEGIN
  FOREACH table_name IN ARRAY ARRAY['plans','plan_tasks','jobs','agent_runs'] LOOP
    EXECUTE format('ALTER TABLE %I ADD CONSTRAINT %I CHECK (status_source IN (''user'',''automation'',''worker'',''backend'',''recovery'',''system'',''legacy''))', table_name, table_name || '_status_source_check');
    EXECUTE format('ALTER TABLE %I ADD CONSTRAINT %I CHECK (length(trim(reason_code)) > 0)', table_name, table_name || '_reason_code_check');
    EXECUTE format('ALTER TABLE %I ADD CONSTRAINT %I CHECK (length(trim(reason)) > 0)', table_name, table_name || '_reason_check');
    EXECUTE format('ALTER TABLE %I ADD CONSTRAINT %I CHECK (recovery_hint IN (''none'',''automatic_retry'',''resume_from_checkpoint'',''retry_from_start'',''manual_review''))', table_name, table_name || '_recovery_hint_check');
    EXECUTE format('ALTER TABLE %I ADD CONSTRAINT %I CHECK (jsonb_typeof(execution_checkpoint) = ''object'')', table_name, table_name || '_execution_checkpoint_check');
  END LOOP;
END;
$$;

CREATE TABLE lifecycle_transitions (
  id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  resource_type TEXT NOT NULL CHECK (resource_type IN ('plan','task','job','agent_run')),
  resource_id UUID NOT NULL,
  resource_version BIGINT NOT NULL CHECK (resource_version > 0),
  from_status TEXT,
  to_status TEXT NOT NULL,
  status_source TEXT NOT NULL CHECK (status_source IN ('user','automation','worker','backend','recovery','system','legacy')),
  reason_code TEXT NOT NULL CHECK (length(trim(reason_code)) > 0),
  reason TEXT NOT NULL CHECK (length(trim(reason)) > 0),
  last_activity_at TIMESTAMPTZ NOT NULL,
  recovery_hint TEXT NOT NULL CHECK (recovery_hint IN ('none','automatic_retry','resume_from_checkpoint','retry_from_start','manual_review')),
  execution_checkpoint JSONB NOT NULL CHECK (jsonb_typeof(execution_checkpoint) = 'object'),
  occurred_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX lifecycle_transitions_resource_idx
  ON lifecycle_transitions(resource_type,resource_id,id DESC);
CREATE INDEX lifecycle_transitions_resource_version_idx
  ON lifecycle_transitions(resource_type,resource_id,resource_version,id DESC);
CREATE INDEX lifecycle_transitions_project_idx
  ON lifecycle_transitions(project_id,id DESC);
CREATE INDEX lifecycle_transitions_reason_idx
  ON lifecycle_transitions(reason_code,occurred_at DESC);
CREATE INDEX plans_lifecycle_activity_idx
  ON plans(project_id,status,last_activity_at DESC);
CREATE INDEX plan_tasks_lifecycle_activity_idx
  ON plan_tasks(plan_id,status,last_activity_at DESC);
CREATE INDEX jobs_lifecycle_activity_idx
  ON jobs(status,last_activity_at DESC);
CREATE INDEX agent_runs_lifecycle_activity_idx
  ON agent_runs(project_id,status,last_activity_at DESC);

-- Every pre-contract row receives an immutable baseline event. from_status is
-- intentionally NULL because the earlier value is unknowable.
INSERT INTO lifecycle_transitions(
  project_id,resource_type,resource_id,resource_version,from_status,to_status,
  status_source,reason_code,reason,last_activity_at,recovery_hint,execution_checkpoint,occurred_at)
SELECT project_id,'plan',id,version,NULL,status,status_source,reason_code,reason,
  last_activity_at,recovery_hint,execution_checkpoint,last_activity_at FROM plans
UNION ALL
SELECT project_id,'task',id,version,NULL,status,status_source,reason_code,reason,
  last_activity_at,recovery_hint,execution_checkpoint,last_activity_at FROM plan_tasks
UNION ALL
SELECT project_id,'job',id,version,NULL,status,status_source,reason_code,reason,
  last_activity_at,recovery_hint,execution_checkpoint,last_activity_at FROM jobs
UNION ALL
SELECT project_id,'agent_run',id,version,NULL,status,status_source,reason_code,reason,
  last_activity_at,recovery_hint,execution_checkpoint,last_activity_at FROM agent_runs;

CREATE OR REPLACE FUNCTION record_lifecycle_transition()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.status IS DISTINCT FROM NEW.status THEN
    INSERT INTO lifecycle_transitions(
      project_id,resource_type,resource_id,resource_version,from_status,to_status,
      status_source,reason_code,reason,last_activity_at,recovery_hint,execution_checkpoint,occurred_at)
    VALUES(
      NEW.project_id,TG_ARGV[0],NEW.id,NEW.version,OLD.status,NEW.status,
      NEW.status_source,NEW.reason_code,NEW.reason,NEW.last_activity_at,
      NEW.recovery_hint,NEW.execution_checkpoint,now());
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER plans_lifecycle_audit
  AFTER UPDATE OF status ON plans
  FOR EACH ROW EXECUTE FUNCTION record_lifecycle_transition('plan');
CREATE TRIGGER plan_tasks_lifecycle_audit
  AFTER UPDATE OF status ON plan_tasks
  FOR EACH ROW EXECUTE FUNCTION record_lifecycle_transition('task');
CREATE TRIGGER jobs_lifecycle_audit
  AFTER UPDATE OF status ON jobs
  FOR EACH ROW EXECUTE FUNCTION record_lifecycle_transition('job');
CREATE TRIGGER agent_runs_lifecycle_audit
  AFTER UPDATE OF status ON agent_runs
  FOR EACH ROW EXECUTE FUNCTION record_lifecycle_transition('agent_run');

CREATE OR REPLACE FUNCTION reject_lifecycle_transition_mutation()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'UPDATE' OR pg_trigger_depth() = 1 THEN
    RAISE EXCEPTION 'lifecycle_transitions is immutable' USING ERRCODE = '55000';
  END IF;
  RETURN OLD;
END;
$$;

CREATE TRIGGER lifecycle_transitions_immutable
  BEFORE UPDATE OR DELETE ON lifecycle_transitions
  FOR EACH ROW EXECUTE FUNCTION reject_lifecycle_transition_mutation();

CREATE OR REPLACE FUNCTION reject_new_agent_run_timeout()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'INSERT' THEN
    IF NEW.status = 'timed_out' THEN
      RAISE EXCEPTION 'timed_out is a legacy-only Agent Run state; record failed, interrupted, or cancelled instead'
        USING ERRCODE = '23514';
    END IF;
  ELSIF NEW.status = 'timed_out' AND OLD.status IS DISTINCT FROM NEW.status THEN
    RAISE EXCEPTION 'timed_out is a legacy-only Agent Run state; record failed, interrupted, or cancelled instead'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER agent_runs_no_new_timed_out
  BEFORE INSERT OR UPDATE OF status ON agent_runs
  FOR EACH ROW EXECUTE FUNCTION reject_new_agent_run_timeout();
