-- Per-task execution evidence is append-only and keyed by an execution attempt,
-- not by task_id. Manual retries and queue retries therefore retain independent
-- checkpoints, diffs, validation output, and rollback history.
CREATE TABLE IF NOT EXISTS task_execution_attempts (
  id UUID PRIMARY KEY,
  project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  plan_id UUID NOT NULL REFERENCES plans(id) ON DELETE CASCADE,
  task_id UUID NOT NULL REFERENCES plan_tasks(id) ON DELETE CASCADE,
  job_id UUID NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
  agent_run_id UUID NOT NULL REFERENCES agent_runs(id) ON DELETE CASCADE,
  attempt_sequence BIGINT NOT NULL CHECK (attempt_sequence > 0),
  attempt_origin TEXT NOT NULL CHECK (attempt_origin IN ('initial','manual_retry','queue_retry','recovery')),
  queue_attempt INTEGER NOT NULL CHECK (queue_attempt > 0),
  supersedes_attempt_id UUID REFERENCES task_execution_attempts(id) ON DELETE CASCADE,
  outcome TEXT NOT NULL CHECK (outcome IN ('succeeded','failed','interrupted','cancelled')),
  started_at TIMESTAMPTZ NOT NULL,
  finished_at TIMESTAMPTZ NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE(task_id, attempt_sequence),
  UNIQUE(agent_run_id),
  CHECK (finished_at >= started_at),
  CHECK (supersedes_attempt_id IS NULL OR supersedes_attempt_id <> id)
);

CREATE TABLE IF NOT EXISTS task_execution_checkpoints (
  id UUID PRIMARY KEY,
  attempt_id UUID NOT NULL REFERENCES task_execution_attempts(id) ON DELETE CASCADE,
  project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  plan_id UUID NOT NULL REFERENCES plans(id) ON DELETE CASCADE,
  task_id UUID NOT NULL REFERENCES plan_tasks(id) ON DELETE CASCADE,
  job_id UUID NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
  agent_run_id UUID NOT NULL REFERENCES agent_runs(id) ON DELETE CASCADE,
  checkpoint_sequence BIGINT NOT NULL CHECK (checkpoint_sequence > 0),
  checkpoint_phase TEXT NOT NULL CHECK (checkpoint_phase IN ('before_execution','after_execution','before_rollback','after_rollback')),
  git_reference_state TEXT NOT NULL CHECK (git_reference_state IN ('branch','detached','unborn')),
  current_branch TEXT NOT NULL DEFAULT '',
  head_oid TEXT NOT NULL DEFAULT '',
  index_tree_oid TEXT NOT NULL CHECK (index_tree_oid ~ '^[0-9a-f]{40}([0-9a-f]{24})?$'),
  workspace_tree_oid TEXT NOT NULL CHECK (workspace_tree_oid ~ '^[0-9a-f]{40}([0-9a-f]{24})?$'),
  git_status_summary JSONB NOT NULL CHECK (jsonb_typeof(git_status_summary) = 'object'),
  git_status_fingerprint TEXT NOT NULL CHECK (git_status_fingerprint ~ '^[0-9a-f]{64}$'),
  task_key TEXT NOT NULL CHECK (task_key ~ '^P[0-9]{3,}$'),
  project_config_snapshot JSONB NOT NULL CHECK (jsonb_typeof(project_config_snapshot) = 'object'),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE(attempt_id, checkpoint_sequence),
  CHECK (
    (git_reference_state = 'branch' AND length(trim(current_branch)) > 0 AND head_oid <> '')
    OR (git_reference_state = 'detached' AND current_branch = '' AND head_oid <> '')
    OR (git_reference_state = 'unborn' AND head_oid = '')
  ),
  CHECK (head_oid = '' OR head_oid ~ '^[0-9a-f]{40}([0-9a-f]{24})?$')
);

CREATE TABLE IF NOT EXISTS task_execution_file_changes (
  id UUID PRIMARY KEY,
  attempt_id UUID NOT NULL REFERENCES task_execution_attempts(id) ON DELETE CASCADE,
  project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  plan_id UUID NOT NULL REFERENCES plans(id) ON DELETE CASCADE,
  task_id UUID NOT NULL REFERENCES plan_tasks(id) ON DELETE CASCADE,
  job_id UUID NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
  agent_run_id UUID NOT NULL REFERENCES agent_runs(id) ON DELETE CASCADE,
  change_sequence BIGINT NOT NULL CHECK (change_sequence > 0),
  path TEXT NOT NULL CHECK (length(path) > 0),
  previous_path TEXT NOT NULL DEFAULT '',
  change_kind TEXT NOT NULL CHECK (change_kind IN ('added','modified','deleted','renamed','copied','type_changed','unmerged','untracked')),
  staged BOOLEAN NOT NULL DEFAULT false,
  is_binary BOOLEAN NOT NULL DEFAULT false,
  additions INTEGER CHECK (additions >= 0),
  deletions INTEGER CHECK (deletions >= 0),
  before_blob_oid TEXT NOT NULL DEFAULT '',
  after_blob_oid TEXT NOT NULL DEFAULT '',
  patch_fingerprint TEXT NOT NULL DEFAULT '' CHECK (patch_fingerprint = '' OR patch_fingerprint ~ '^[0-9a-f]{64}$'),
  summary JSONB NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(summary) = 'object'),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE(attempt_id, change_sequence),
  CHECK (before_blob_oid = '' OR before_blob_oid ~ '^[0-9a-f]{40}([0-9a-f]{24})?$'),
  CHECK (after_blob_oid = '' OR after_blob_oid ~ '^[0-9a-f]{40}([0-9a-f]{24})?$')
);

CREATE TABLE IF NOT EXISTS task_execution_validations (
  id UUID PRIMARY KEY,
  attempt_id UUID NOT NULL REFERENCES task_execution_attempts(id) ON DELETE CASCADE,
  project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  plan_id UUID NOT NULL REFERENCES plans(id) ON DELETE CASCADE,
  task_id UUID NOT NULL REFERENCES plan_tasks(id) ON DELETE CASCADE,
  job_id UUID NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
  agent_run_id UUID NOT NULL REFERENCES agent_runs(id) ON DELETE CASCADE,
  validation_sequence BIGINT NOT NULL CHECK (validation_sequence > 0),
  command TEXT NOT NULL CHECK (length(trim(command)) > 0),
  working_directory TEXT NOT NULL CHECK (length(trim(working_directory)) > 0),
  status TEXT NOT NULL CHECK (status IN ('passed','failed','timed_out','skipped','error')),
  exit_code INTEGER,
  started_at TIMESTAMPTZ,
  finished_at TIMESTAMPTZ,
  stdout_fingerprint TEXT NOT NULL DEFAULT '' CHECK (stdout_fingerprint = '' OR stdout_fingerprint ~ '^[0-9a-f]{64}$'),
  stderr_fingerprint TEXT NOT NULL DEFAULT '' CHECK (stderr_fingerprint = '' OR stderr_fingerprint ~ '^[0-9a-f]{64}$'),
  output_summary JSONB NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(output_summary) = 'object'),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE(attempt_id, validation_sequence),
  CHECK (finished_at IS NULL OR started_at IS NOT NULL),
  CHECK (finished_at IS NULL OR finished_at >= started_at)
);

-- A rollback operation is immutable. Its current status is the final associated
-- rollback event, so status changes never rewrite the operation or its evidence.
CREATE TABLE IF NOT EXISTS task_execution_rollbacks (
  id UUID PRIMARY KEY,
  attempt_id UUID NOT NULL REFERENCES task_execution_attempts(id) ON DELETE CASCADE,
  project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  plan_id UUID NOT NULL REFERENCES plans(id) ON DELETE CASCADE,
  task_id UUID NOT NULL REFERENCES plan_tasks(id) ON DELETE CASCADE,
  job_id UUID NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
  agent_run_id UUID NOT NULL REFERENCES agent_runs(id) ON DELETE CASCADE,
  rollback_sequence BIGINT NOT NULL CHECK (rollback_sequence > 0),
  source_checkpoint_id UUID NOT NULL REFERENCES task_execution_checkpoints(id) ON DELETE CASCADE,
  target_checkpoint_id UUID NOT NULL REFERENCES task_execution_checkpoints(id) ON DELETE CASCADE,
  rollback_kind TEXT NOT NULL CHECK (rollback_kind IN ('manual','automatic','recovery')),
  command_summary TEXT NOT NULL DEFAULT '',
  reason TEXT NOT NULL CHECK (length(trim(reason)) > 0),
  requested_by TEXT NOT NULL CHECK (length(trim(requested_by)) > 0),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE(attempt_id, rollback_sequence),
  CHECK (source_checkpoint_id <> target_checkpoint_id)
);

CREATE TABLE IF NOT EXISTS task_execution_rollback_events (
  id UUID PRIMARY KEY,
  rollback_id UUID NOT NULL REFERENCES task_execution_rollbacks(id) ON DELETE CASCADE,
  attempt_id UUID NOT NULL REFERENCES task_execution_attempts(id) ON DELETE CASCADE,
  project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  plan_id UUID NOT NULL REFERENCES plans(id) ON DELETE CASCADE,
  task_id UUID NOT NULL REFERENCES plan_tasks(id) ON DELETE CASCADE,
  job_id UUID NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
  agent_run_id UUID NOT NULL REFERENCES agent_runs(id) ON DELETE CASCADE,
  event_sequence BIGINT NOT NULL CHECK (event_sequence > 0),
  status TEXT NOT NULL CHECK (status IN ('requested','running','succeeded','failed','cancelled')),
  message TEXT NOT NULL DEFAULT '',
  details JSONB NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(details) = 'object'),
  occurred_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE(rollback_id, event_sequence)
);

CREATE INDEX IF NOT EXISTS task_execution_attempts_plan_task_idx
  ON task_execution_attempts(plan_id, task_id, attempt_sequence DESC);
CREATE INDEX IF NOT EXISTS task_execution_attempts_project_created_idx
  ON task_execution_attempts(project_id, created_at DESC);
CREATE INDEX IF NOT EXISTS task_execution_checkpoints_attempt_idx
  ON task_execution_checkpoints(attempt_id, checkpoint_sequence);
CREATE INDEX IF NOT EXISTS task_execution_file_changes_attempt_idx
  ON task_execution_file_changes(attempt_id, change_sequence);
CREATE INDEX IF NOT EXISTS task_execution_validations_attempt_idx
  ON task_execution_validations(attempt_id, validation_sequence);
CREATE INDEX IF NOT EXISTS task_execution_rollbacks_attempt_idx
  ON task_execution_rollbacks(attempt_id, rollback_sequence);
CREATE INDEX IF NOT EXISTS task_execution_rollback_events_rollback_idx
  ON task_execution_rollback_events(rollback_id, event_sequence);

-- Enforce that every attempt points at one coherent project/plan/task/job/run
-- chain. Existing tables remain unchanged; the rule applies only when evidence
-- is recorded.
CREATE OR REPLACE FUNCTION validate_task_execution_attempt_links()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NOT EXISTS (
    SELECT 1
    FROM plans p
    JOIN plan_tasks t ON t.plan_id=p.id AND t.project_id=p.project_id
    JOIN jobs j ON j.project_id=p.project_id
      AND j.job_type='task.execute' AND j.aggregate_type='task' AND j.aggregate_id=t.id
    JOIN agent_runs ar ON ar.project_id=p.project_id
      AND ar.plan_id=p.id AND ar.task_id=t.id AND ar.job_id=j.id
    WHERE p.id=NEW.plan_id AND p.project_id=NEW.project_id
      AND t.id=NEW.task_id AND j.id=NEW.job_id AND ar.id=NEW.agent_run_id
  ) THEN
    RAISE EXCEPTION 'task execution attempt references an inconsistent project/plan/task/job/agent run chain'
      USING ERRCODE = '23503';
  END IF;
  IF NEW.supersedes_attempt_id IS NOT NULL AND NOT EXISTS (
    SELECT 1 FROM task_execution_attempts previous
    WHERE previous.id=NEW.supersedes_attempt_id
      AND previous.project_id=NEW.project_id
      AND previous.plan_id=NEW.plan_id
      AND previous.task_id=NEW.task_id
      AND previous.attempt_sequence < NEW.attempt_sequence
  ) THEN
    RAISE EXCEPTION 'superseded execution attempt must be an earlier attempt for the same task'
      USING ERRCODE = '23503';
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS task_execution_attempts_validate_links ON task_execution_attempts;
CREATE TRIGGER task_execution_attempts_validate_links
  BEFORE INSERT ON task_execution_attempts
  FOR EACH ROW EXECUTE FUNCTION validate_task_execution_attempt_links();

CREATE OR REPLACE FUNCTION task_execution_evidence_links_match(
  evidence_attempt_id UUID,
  evidence_project_id UUID,
  evidence_plan_id UUID,
  evidence_task_id UUID,
  evidence_job_id UUID,
  evidence_agent_run_id UUID
) RETURNS BOOLEAN LANGUAGE sql STABLE AS $$
  SELECT EXISTS (
    SELECT 1 FROM task_execution_attempts a
    WHERE a.id=evidence_attempt_id AND a.project_id=evidence_project_id
      AND a.plan_id=evidence_plan_id AND a.task_id=evidence_task_id
      AND a.job_id=evidence_job_id AND a.agent_run_id=evidence_agent_run_id
  );
$$;

CREATE OR REPLACE FUNCTION validate_task_execution_evidence_links()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NOT task_execution_evidence_links_match(
    NEW.attempt_id, NEW.project_id, NEW.plan_id, NEW.task_id, NEW.job_id, NEW.agent_run_id
  ) THEN
    RAISE EXCEPTION '% references a different execution attempt chain', TG_TABLE_NAME
      USING ERRCODE = '23503';
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS task_execution_checkpoints_validate_links ON task_execution_checkpoints;
CREATE TRIGGER task_execution_checkpoints_validate_links
  BEFORE INSERT ON task_execution_checkpoints
  FOR EACH ROW EXECUTE FUNCTION validate_task_execution_evidence_links();
DROP TRIGGER IF EXISTS task_execution_file_changes_validate_links ON task_execution_file_changes;
CREATE TRIGGER task_execution_file_changes_validate_links
  BEFORE INSERT ON task_execution_file_changes
  FOR EACH ROW EXECUTE FUNCTION validate_task_execution_evidence_links();
DROP TRIGGER IF EXISTS task_execution_validations_validate_links ON task_execution_validations;
CREATE TRIGGER task_execution_validations_validate_links
  BEFORE INSERT ON task_execution_validations
  FOR EACH ROW EXECUTE FUNCTION validate_task_execution_evidence_links();

CREATE OR REPLACE FUNCTION validate_task_execution_rollback_links()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NOT task_execution_evidence_links_match(
    NEW.attempt_id, NEW.project_id, NEW.plan_id, NEW.task_id, NEW.job_id, NEW.agent_run_id
  ) THEN
    RAISE EXCEPTION 'task_execution_rollbacks references a different execution attempt chain'
      USING ERRCODE = '23503';
  END IF;
  IF NOT EXISTS (
    SELECT 1 FROM task_execution_checkpoints source
    JOIN task_execution_checkpoints target ON target.id=NEW.target_checkpoint_id
    WHERE source.id=NEW.source_checkpoint_id
      AND source.attempt_id=NEW.attempt_id AND target.attempt_id=NEW.attempt_id
  ) THEN
    RAISE EXCEPTION 'rollback checkpoints must belong to the same execution attempt'
      USING ERRCODE = '23503';
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS task_execution_rollbacks_validate_links ON task_execution_rollbacks;
CREATE TRIGGER task_execution_rollbacks_validate_links
  BEFORE INSERT ON task_execution_rollbacks
  FOR EACH ROW EXECUTE FUNCTION validate_task_execution_rollback_links();

CREATE OR REPLACE FUNCTION validate_task_execution_rollback_event_links()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NOT task_execution_evidence_links_match(
    NEW.attempt_id, NEW.project_id, NEW.plan_id, NEW.task_id, NEW.job_id, NEW.agent_run_id
  ) THEN
    RAISE EXCEPTION 'task_execution_rollback_events references a different execution attempt chain'
      USING ERRCODE = '23503';
  END IF;
  IF NOT EXISTS (
    SELECT 1 FROM task_execution_rollbacks rollback
    WHERE rollback.id=NEW.rollback_id AND rollback.attempt_id=NEW.attempt_id
      AND rollback.project_id=NEW.project_id AND rollback.plan_id=NEW.plan_id
      AND rollback.task_id=NEW.task_id AND rollback.job_id=NEW.job_id
      AND rollback.agent_run_id=NEW.agent_run_id
  ) THEN
    RAISE EXCEPTION 'rollback event references a different rollback operation chain'
      USING ERRCODE = '23503';
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS task_execution_rollback_events_validate_links ON task_execution_rollback_events;
CREATE TRIGGER task_execution_rollback_events_validate_links
  BEFORE INSERT ON task_execution_rollback_events
  FOR EACH ROW EXECUTE FUNCTION validate_task_execution_rollback_event_links();

-- Direct updates/deletes are forbidden. Cascades caused by deleting an owning
-- project/plan/task/job/run remain allowed for safe local metadata cleanup.
CREATE OR REPLACE FUNCTION reject_task_execution_evidence_mutation()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'UPDATE' OR pg_trigger_depth() = 1 THEN
    RAISE EXCEPTION '% is immutable', TG_TABLE_NAME USING ERRCODE = '55000';
  END IF;
  RETURN OLD;
END;
$$;

DROP TRIGGER IF EXISTS task_execution_attempts_immutable ON task_execution_attempts;
CREATE TRIGGER task_execution_attempts_immutable
  BEFORE UPDATE OR DELETE ON task_execution_attempts
  FOR EACH ROW EXECUTE FUNCTION reject_task_execution_evidence_mutation();
DROP TRIGGER IF EXISTS task_execution_checkpoints_immutable ON task_execution_checkpoints;
CREATE TRIGGER task_execution_checkpoints_immutable
  BEFORE UPDATE OR DELETE ON task_execution_checkpoints
  FOR EACH ROW EXECUTE FUNCTION reject_task_execution_evidence_mutation();
DROP TRIGGER IF EXISTS task_execution_file_changes_immutable ON task_execution_file_changes;
CREATE TRIGGER task_execution_file_changes_immutable
  BEFORE UPDATE OR DELETE ON task_execution_file_changes
  FOR EACH ROW EXECUTE FUNCTION reject_task_execution_evidence_mutation();
DROP TRIGGER IF EXISTS task_execution_validations_immutable ON task_execution_validations;
CREATE TRIGGER task_execution_validations_immutable
  BEFORE UPDATE OR DELETE ON task_execution_validations
  FOR EACH ROW EXECUTE FUNCTION reject_task_execution_evidence_mutation();
DROP TRIGGER IF EXISTS task_execution_rollbacks_immutable ON task_execution_rollbacks;
CREATE TRIGGER task_execution_rollbacks_immutable
  BEFORE UPDATE OR DELETE ON task_execution_rollbacks
  FOR EACH ROW EXECUTE FUNCTION reject_task_execution_evidence_mutation();
DROP TRIGGER IF EXISTS task_execution_rollback_events_immutable ON task_execution_rollback_events;
CREATE TRIGGER task_execution_rollback_events_immutable
  BEFORE UPDATE OR DELETE ON task_execution_rollback_events
  FOR EACH ROW EXECUTE FUNCTION reject_task_execution_evidence_mutation();
