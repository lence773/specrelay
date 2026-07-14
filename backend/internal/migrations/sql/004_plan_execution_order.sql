-- A running plan owns task execution for its project until it reaches a terminal
-- state. This timestamp records the start of the current execution round, so a
-- resumed plan is ordered by when it was actually started rather than when it
-- was originally generated.
ALTER TABLE plans ADD COLUMN execution_started_at TIMESTAMPTZ;

UPDATE plans p
SET execution_started_at = COALESCE(
  (
    SELECT max(e.occurred_at)
    FROM events e
    WHERE e.aggregate_type = 'plan'
      AND e.aggregate_id = p.id
      AND e.event_type = 'plan.running'
  ),
  p.created_at
)
WHERE p.execution_started_at IS NULL;

CREATE INDEX plans_execution_owner_idx
  ON plans(project_id, execution_started_at, created_at, id)
  WHERE status IN ('running', 'validating');
