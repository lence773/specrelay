-- Persist the normalized PlanSpec execution graph independently from the raw
-- specification so execution, acceptance, and delivery state can evolve
-- without rewriting historical plan content.
ALTER TABLE plans
  ADD COLUMN spec_version INTEGER NOT NULL DEFAULT 1,
  ADD COLUMN compatibility_mode BOOLEAN NOT NULL DEFAULT true,
  ADD COLUMN is_executable BOOLEAN NOT NULL DEFAULT true,
  ADD COLUMN validation_problems JSONB NOT NULL DEFAULT '[]'::jsonb
    CHECK (jsonb_typeof(validation_problems) = 'array'),
  ADD COLUMN delivery_status TEXT NOT NULL DEFAULT 'pending'
    CHECK (delivery_status IN ('pending','delivered','failed','cancelled','manual_confirmation')),
  ADD COLUMN acceptance_summary JSONB NOT NULL DEFAULT '{}'::jsonb
    CHECK (jsonb_typeof(acceptance_summary) = 'object');

-- Existing PlanSpec payloads were valid enough to execute before this
-- migration. Preserve that behavior while recording whether they came from the
-- versioned format or the legacy compatibility view.
UPDATE plans
SET spec_version = CASE
      WHEN jsonb_typeof(spec->'version') = 'number'
        AND spec->>'version' ~ '^[1-9][0-9]{0,8}$'
      THEN (spec->>'version')::integer
      ELSE 1
    END,
    compatibility_mode = CASE
      WHEN jsonb_typeof(spec->'compatibilityMode') = 'boolean'
        THEN (spec->>'compatibilityMode')::boolean
      ELSE true
    END,
    is_executable = true,
    validation_problems = '[]'::jsonb,
    delivery_status = CASE status
      WHEN 'completed' THEN 'delivered'
      WHEN 'failed' THEN 'failed'
      WHEN 'cancelled' THEN 'cancelled'
      ELSE 'pending'
    END;

-- PlanSpec v2 permits stable semantic keys rather than only generated Pnnn
-- keys. Existing keys continue to satisfy the broader constraint.
ALTER TABLE plan_tasks DROP CONSTRAINT IF EXISTS plan_tasks_task_key_check;
ALTER TABLE plan_tasks ADD CONSTRAINT plan_tasks_task_key_check
  CHECK (task_key ~ '^([[:alnum:]]+)(-[[:alnum:]]+)*$');

ALTER TABLE plan_tasks
  ADD COLUMN task_type TEXT NOT NULL DEFAULT 'implementation'
    CHECK (task_type IN ('implementation','final_validation')),
  ADD COLUMN execution_order INTEGER,
  ADD COLUMN dependency_keys JSONB NOT NULL DEFAULT '[]'::jsonb
    CHECK (jsonb_typeof(dependency_keys) = 'array'),
  ADD COLUMN inputs JSONB NOT NULL DEFAULT '[]'::jsonb
    CHECK (jsonb_typeof(inputs) = 'array'),
  ADD COLUMN outputs JSONB NOT NULL DEFAULT '[]'::jsonb
    CHECK (jsonb_typeof(outputs) = 'array'),
  ADD COLUMN risks JSONB NOT NULL DEFAULT '[]'::jsonb
    CHECK (jsonb_typeof(risks) = 'array'),
  ADD COLUMN validation_commands JSONB NOT NULL DEFAULT '[]'::jsonb
    CHECK (jsonb_typeof(validation_commands) = 'array'),
  ADD COLUMN acceptance_definition JSONB NOT NULL DEFAULT '[]'::jsonb
    CHECK (jsonb_typeof(acceptance_definition) = 'array'),
  ADD COLUMN acceptance_status TEXT NOT NULL DEFAULT 'pending'
    CHECK (acceptance_status IN ('pending','passed','failed','skipped','manual_confirmation')),
  ADD COLUMN acceptance_result JSONB NOT NULL DEFAULT '{}'::jsonb
    CHECK (jsonb_typeof(acceptance_result) = 'object');

UPDATE plan_tasks t
SET task_type = CASE
      WHEN lower(trim(t.title)) = 'final validation'
        OR t.position = (SELECT max(last_task.position) FROM plan_tasks last_task WHERE last_task.plan_id=t.plan_id)
      THEN 'final_validation'
      ELSE 'implementation'
    END,
    execution_order = t.position,
    acceptance_status = CASE t.status
      WHEN 'succeeded' THEN 'passed'
      WHEN 'failed' THEN 'failed'
      WHEN 'cancelled' THEN 'skipped'
      ELSE 'pending'
    END;

-- Recover graph and task metadata from versioned specs where possible. Legacy
-- rows fall back to their original sequential ordering, preserving position,
-- status, session identifiers, timestamps, and lifecycle history.
UPDATE plan_tasks t
SET dependency_keys = CASE
      WHEN t.task_type = 'final_validation' THEN COALESCE((
        SELECT jsonb_agg(prior.task_key ORDER BY prior.execution_order)
        FROM plan_tasks prior
        WHERE prior.plan_id=t.plan_id AND prior.execution_order<t.execution_order
      ), '[]'::jsonb)
      ELSE COALESCE((
        SELECT CASE WHEN jsonb_typeof(task_spec.value->'dependsOn')='array'
          THEN task_spec.value->'dependsOn' ELSE NULL END
        FROM plans p
        CROSS JOIN LATERAL jsonb_array_elements(
          CASE WHEN jsonb_typeof(p.spec->'tasks')='array' THEN p.spec->'tasks' ELSE '[]'::jsonb END
        ) task_spec(value)
        WHERE p.id=t.plan_id AND upper(trim(task_spec.value->>'key'))=t.task_key
        LIMIT 1
      ), COALESCE((
        SELECT jsonb_build_array(prior.task_key)
        FROM plan_tasks prior
        WHERE prior.plan_id=t.plan_id AND prior.execution_order<t.execution_order
        ORDER BY prior.execution_order DESC LIMIT 1
      ), '[]'::jsonb))
    END,
    inputs = COALESCE((
      SELECT CASE WHEN jsonb_typeof(task_spec.value->'inputs')='array'
        THEN task_spec.value->'inputs' ELSE NULL END FROM plans p
      CROSS JOIN LATERAL jsonb_array_elements(
        CASE WHEN jsonb_typeof(p.spec->'tasks')='array' THEN p.spec->'tasks' ELSE '[]'::jsonb END
      ) task_spec(value)
      WHERE p.id=t.plan_id AND upper(trim(task_spec.value->>'key'))=t.task_key LIMIT 1
    ), '[]'::jsonb),
    outputs = COALESCE((
      SELECT CASE WHEN jsonb_typeof(task_spec.value->'outputs')='array'
        THEN task_spec.value->'outputs' ELSE NULL END FROM plans p
      CROSS JOIN LATERAL jsonb_array_elements(
        CASE WHEN jsonb_typeof(p.spec->'tasks')='array' THEN p.spec->'tasks' ELSE '[]'::jsonb END
      ) task_spec(value)
      WHERE p.id=t.plan_id AND upper(trim(task_spec.value->>'key'))=t.task_key LIMIT 1
    ), '[]'::jsonb),
    risks = COALESCE((
      SELECT CASE WHEN jsonb_typeof(task_spec.value->'risks')='array'
        THEN task_spec.value->'risks' ELSE NULL END FROM plans p
      CROSS JOIN LATERAL jsonb_array_elements(
        CASE WHEN jsonb_typeof(p.spec->'tasks')='array' THEN p.spec->'tasks' ELSE '[]'::jsonb END
      ) task_spec(value)
      WHERE p.id=t.plan_id AND upper(trim(task_spec.value->>'key'))=t.task_key LIMIT 1
    ), '[]'::jsonb),
    validation_commands = CASE
      WHEN t.task_type='final_validation' THEN COALESCE((
        SELECT CASE WHEN jsonb_typeof(p.spec->'finalValidation')='object'
          AND jsonb_typeof(p.spec->'finalValidation'->'commands')='array'
          THEN p.spec->'finalValidation'->'commands' ELSE '[]'::jsonb END
        FROM plans p WHERE p.id=t.plan_id
      ), '[]'::jsonb)
      ELSE COALESCE((
        SELECT CASE WHEN jsonb_typeof(task_spec.value->'validationCommands')='array'
          THEN task_spec.value->'validationCommands' ELSE NULL END FROM plans p
        CROSS JOIN LATERAL jsonb_array_elements(
          CASE WHEN jsonb_typeof(p.spec->'tasks')='array' THEN p.spec->'tasks' ELSE '[]'::jsonb END
        ) task_spec(value)
        WHERE p.id=t.plan_id AND upper(trim(task_spec.value->>'key'))=t.task_key LIMIT 1
      ), '[]'::jsonb)
    END;

UPDATE plan_tasks t
SET acceptance_definition = COALESCE(
  CASE WHEN t.task_type='final_validation' THEN (
    SELECT CASE WHEN jsonb_typeof(p.spec->'finalValidation')='object'
      AND jsonb_typeof(p.spec->'finalValidation'->'acceptance')='array'
      THEN p.spec->'finalValidation'->'acceptance' ELSE NULL END
    FROM plans p WHERE p.id=t.plan_id
  ) ELSE (
    SELECT CASE WHEN jsonb_typeof(task_spec.value->'acceptance')='array'
      AND NOT EXISTS (
        SELECT 1 FROM jsonb_array_elements(task_spec.value->'acceptance') entry
        WHERE jsonb_typeof(entry)<>'object'
      ) THEN task_spec.value->'acceptance' ELSE NULL END
    FROM plans p
    CROSS JOIN LATERAL jsonb_array_elements(
      CASE WHEN jsonb_typeof(p.spec->'tasks')='array' THEN p.spec->'tasks' ELSE '[]'::jsonb END
    ) task_spec(value)
    WHERE p.id=t.plan_id AND upper(trim(task_spec.value->>'key'))=t.task_key LIMIT 1
  ) END,
  (
    SELECT COALESCE(jsonb_agg(jsonb_build_object(
      'key', t.task_key || '-A' || lpad(item.ordinality::text, 3, '0'),
      'description', CASE WHEN jsonb_typeof(item.value)='string'
        THEN item.value #>> '{}' ELSE item.value->>'description' END
    ) ORDER BY item.ordinality), '[]'::jsonb)
    FROM jsonb_array_elements(t.acceptance) WITH ORDINALITY item(value, ordinality)
  )
);

UPDATE plan_tasks t
SET acceptance_result = jsonb_build_object(
  'status', t.acceptance_status,
  'items', COALESCE((
    SELECT jsonb_agg(jsonb_build_object(
      'key', item.value->>'key',
      'status', t.acceptance_status,
      'evidence', '[]'::jsonb,
      'reason', CASE WHEN t.status IN ('succeeded','failed','cancelled')
        THEN 'legacy_backfill' ELSE '' END
    ) ORDER BY item.ordinality)
    FROM jsonb_array_elements(t.acceptance_definition) WITH ORDINALITY item(value, ordinality)
  ), '[]'::jsonb),
  'evidence', '[]'::jsonb,
  'reason', CASE WHEN t.status IN ('succeeded','failed','cancelled') THEN 'legacy_backfill' ELSE '' END
);

-- Old writers only know the legacy position column. Fill the new order before
-- constraints are checked so post-upgrade inserts remain backward compatible.
CREATE OR REPLACE FUNCTION default_plan_task_execution_order()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.execution_order IS NULL THEN
    NEW.execution_order := NEW.position;
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER plan_tasks_execution_order_default
  BEFORE INSERT ON plan_tasks
  FOR EACH ROW EXECUTE FUNCTION default_plan_task_execution_order();

ALTER TABLE plan_tasks
  ALTER COLUMN execution_order SET NOT NULL,
  ADD CONSTRAINT plan_tasks_execution_order_check CHECK (execution_order > 0),
  ADD CONSTRAINT plan_tasks_plan_execution_order_key UNIQUE(plan_id, execution_order);

CREATE INDEX plan_tasks_execution_status_idx
  ON plan_tasks(plan_id, acceptance_status, execution_order);

UPDATE plans p
SET acceptance_summary = summary.value
FROM (
  SELECT plan_id, jsonb_build_object(
    'status', CASE
      WHEN bool_or(acceptance_status='failed') THEN 'failed'
      WHEN bool_or(acceptance_status='manual_confirmation') THEN 'manual_confirmation'
      WHEN bool_and(acceptance_status IN ('passed','skipped')) THEN 'passed'
      ELSE 'pending'
    END,
    'total', count(*),
    'pending', count(*) FILTER (WHERE acceptance_status='pending'),
    'passed', count(*) FILTER (WHERE acceptance_status='passed'),
    'failed', count(*) FILTER (WHERE acceptance_status='failed'),
    'skipped', count(*) FILTER (WHERE acceptance_status='skipped'),
    'manualConfirmation', count(*) FILTER (WHERE acceptance_status='manual_confirmation')
  ) AS value
  FROM plan_tasks GROUP BY plan_id
) summary
WHERE p.id=summary.plan_id;

UPDATE plans
SET acceptance_summary = jsonb_build_object(
  'status','pending','total',0,'pending',0,'passed',0,'failed',0,
  'skipped',0,'manualConfirmation',0
)
WHERE acceptance_summary='{}'::jsonb;

-- Keep the denormalized acceptance summary current regardless of whether a
-- result is written by the worker path or a later manual-review API.
CREATE OR REPLACE FUNCTION refresh_plan_acceptance_summary()
RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
  target_plan_id UUID;
BEGIN
  IF TG_OP = 'DELETE' THEN
    target_plan_id := OLD.plan_id;
  ELSE
    target_plan_id := NEW.plan_id;
  END IF;
  IF EXISTS (SELECT 1 FROM plans WHERE id=target_plan_id) THEN
    UPDATE plans p
    SET acceptance_summary = COALESCE((
      SELECT jsonb_build_object(
        'status', CASE
          WHEN bool_or(t.acceptance_status='failed') THEN 'failed'
          WHEN bool_or(t.acceptance_status='manual_confirmation') THEN 'manual_confirmation'
          WHEN bool_and(t.acceptance_status IN ('passed','skipped')) THEN 'passed'
          ELSE 'pending'
        END,
        'total', count(*),
        'pending', count(*) FILTER (WHERE t.acceptance_status='pending'),
        'passed', count(*) FILTER (WHERE t.acceptance_status='passed'),
        'failed', count(*) FILTER (WHERE t.acceptance_status='failed'),
        'skipped', count(*) FILTER (WHERE t.acceptance_status='skipped'),
        'manualConfirmation', count(*) FILTER (WHERE t.acceptance_status='manual_confirmation')
      ) FROM plan_tasks t WHERE t.plan_id=target_plan_id
    ), jsonb_build_object(
      'status','pending','total',0,'pending',0,'passed',0,'failed',0,
      'skipped',0,'manualConfirmation',0
    ))
    WHERE p.id=target_plan_id;
  END IF;
  IF TG_OP = 'DELETE' THEN
    RETURN OLD;
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER plan_tasks_acceptance_summary_refresh
  AFTER INSERT OR UPDATE OF acceptance_status OR DELETE ON plan_tasks
  FOR EACH ROW EXECUTE FUNCTION refresh_plan_acceptance_summary();

-- Delivery is separate from the execution lifecycle, but terminal execution
-- outcomes have an unambiguous delivery projection. Non-terminal states leave
-- the explicitly persisted delivery decision untouched.
CREATE OR REPLACE FUNCTION project_plan_delivery_status()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  NEW.delivery_status := CASE NEW.status
    WHEN 'completed' THEN 'delivered'
    WHEN 'failed' THEN 'failed'
    WHEN 'cancelled' THEN 'cancelled'
    ELSE NEW.delivery_status
  END;
  RETURN NEW;
END;
$$;

CREATE TRIGGER plans_delivery_status_projection
  BEFORE INSERT OR UPDATE OF status ON plans
  FOR EACH ROW EXECUTE FUNCTION project_plan_delivery_status();
