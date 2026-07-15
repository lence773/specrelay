-- Plans keep a resource version for optimistic state transitions and a separate
-- content version for execution semantics. State-only transitions must not bump
-- content_version.
ALTER TABLE plans
  ADD COLUMN content_version BIGINT NOT NULL DEFAULT 1 CHECK (content_version > 0);

CREATE TABLE plan_execution_snapshots (
  id UUID PRIMARY KEY,
  project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  plan_id UUID NOT NULL REFERENCES plans(id) ON DELETE CASCADE,
  intake_id UUID NOT NULL REFERENCES intakes(id) ON DELETE CASCADE,
  previous_snapshot_id UUID,
  task_id UUID,
  sequence BIGINT NOT NULL CHECK (sequence > 0),
  snapshot_kind TEXT NOT NULL CHECK (snapshot_kind IN ('generation_baseline','task_checkpoint','user_accepted')),
  requirement_id UUID NOT NULL,
  requirement_version BIGINT NOT NULL CHECK (requirement_version > 0),
  requirement_digest TEXT NOT NULL CHECK (requirement_digest ~ '^[0-9a-f]{64}$'),
  plan_resource_version BIGINT NOT NULL CHECK (plan_resource_version > 0),
  plan_content_version BIGINT NOT NULL CHECK (plan_content_version > 0),
  plan_spec_digest TEXT NOT NULL CHECK (plan_spec_digest ~ '^[0-9a-f]{64}$'),
  project_version BIGINT NOT NULL CHECK (project_version > 0),
  config_version BIGINT NOT NULL CHECK (config_version > 0),
  key_execution_fields JSONB NOT NULL CHECK (jsonb_typeof(key_execution_fields) = 'object'),
  generation_provider TEXT NOT NULL CHECK (length(trim(generation_provider)) > 0),
  execution_provider TEXT NOT NULL CHECK (length(trim(execution_provider)) > 0),
  workspace_path_normalized TEXT NOT NULL CHECK (length(trim(workspace_path_normalized)) > 0),
  git_root TEXT NOT NULL DEFAULT '',
  git_repository_identity TEXT NOT NULL DEFAULT '',
  git_branch TEXT NOT NULL DEFAULT '',
  git_head TEXT NOT NULL DEFAULT '',
  git_workspace_digest TEXT NOT NULL CHECK (git_workspace_digest ~ '^[0-9a-f]{64}$'),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE(plan_id, sequence),
  CHECK (previous_snapshot_id IS NULL OR previous_snapshot_id <> id)
);

CREATE INDEX plan_execution_snapshots_plan_created_idx
  ON plan_execution_snapshots(plan_id, sequence DESC);
CREATE INDEX plan_execution_snapshots_project_created_idx
  ON plan_execution_snapshots(project_id, created_at DESC);

CREATE TABLE plan_drift_audits (
  id UUID PRIMARY KEY,
  project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  plan_id UUID NOT NULL REFERENCES plans(id) ON DELETE CASCADE,
  sequence BIGINT NOT NULL CHECK (sequence > 0),
  action TEXT NOT NULL CHECK (action IN ('snapshot_updated','plan_regenerated','execution_abandoned')),
  original_snapshot_id UUID,
  new_snapshot_id UUID,
  target_plan_id UUID,
  raw_diff JSONB NOT NULL,
  channel TEXT NOT NULL CHECK (length(trim(channel)) > 0),
  reason TEXT NOT NULL CHECK (length(trim(reason)) > 0),
  occurred_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE(plan_id, sequence),
  CHECK (
    (action = 'snapshot_updated' AND original_snapshot_id IS NOT NULL AND new_snapshot_id IS NOT NULL)
    OR (action = 'plan_regenerated' AND target_plan_id IS NOT NULL)
    OR (action = 'execution_abandoned' AND target_plan_id = plan_id)
  )
);

CREATE INDEX plan_drift_audits_plan_occurred_idx
  ON plan_drift_audits(plan_id, sequence DESC);
CREATE INDEX plan_drift_audits_project_occurred_idx
  ON plan_drift_audits(project_id, occurred_at DESC);

-- Direct mutation is rejected. Cascading deletes from a plan/project are still
-- permitted so existing lifecycle semantics remain intact.
CREATE OR REPLACE FUNCTION reject_execution_history_mutation()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'UPDATE' OR pg_trigger_depth() = 1 THEN
    RAISE EXCEPTION '% is immutable', TG_TABLE_NAME USING ERRCODE = '55000';
  END IF;
  RETURN OLD;
END;
$$;

CREATE TRIGGER plan_execution_snapshots_immutable
  BEFORE UPDATE OR DELETE ON plan_execution_snapshots
  FOR EACH ROW EXECUTE FUNCTION reject_execution_history_mutation();

CREATE TRIGGER plan_drift_audits_immutable
  BEFORE UPDATE OR DELETE ON plan_drift_audits
  FOR EACH ROW EXECUTE FUNCTION reject_execution_history_mutation();


-- Existing cancellation entry points predate explicit drift APIs. Record their
-- transition as an append-only abandonment audit so every abandoned execution
-- has a durable disposition, including legacy plans without a baseline.
CREATE OR REPLACE FUNCTION audit_plan_execution_abandoned()
RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
  snapshot_id UUID;
  next_sequence BIGINT;
BEGIN
  IF OLD.status <> 'cancelled' AND NEW.status = 'cancelled' THEN
    SELECT id INTO snapshot_id
    FROM plan_execution_snapshots
    WHERE plan_id=NEW.id
    ORDER BY sequence DESC
    LIMIT 1;

    SELECT coalesce(max(sequence),0)+1 INTO next_sequence
    FROM plan_drift_audits
    WHERE plan_id=NEW.id;

    INSERT INTO plan_drift_audits(
      id,project_id,plan_id,sequence,action,original_snapshot_id,target_plan_id,
      raw_diff,channel,reason,occurred_at)
    VALUES(
      gen_random_uuid(),NEW.project_id,NEW.id,next_sequence,'execution_abandoned',
      snapshot_id,NEW.id,
      jsonb_build_object('status',jsonb_build_object('before',OLD.status,'after',NEW.status)),
      'repository','plan execution abandoned',now());
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER plans_execution_abandoned_audit
  AFTER UPDATE OF status ON plans
  FOR EACH ROW EXECUTE FUNCTION audit_plan_execution_abandoned();
