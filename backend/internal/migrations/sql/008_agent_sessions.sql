CREATE TABLE agent_sessions (
  id UUID PRIMARY KEY,
  project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  intake_id UUID REFERENCES intakes(id) ON DELETE CASCADE,
  plan_id UUID REFERENCES plans(id) ON DELETE CASCADE,
  provider TEXT NOT NULL CHECK (provider IN ('codex','claude')),
  purpose TEXT NOT NULL CHECK (purpose IN ('requirement','execution')),
  cli_session_id TEXT NOT NULL,
  context_summary TEXT NOT NULL DEFAULT '',
  last_task_id UUID REFERENCES plan_tasks(id) ON DELETE SET NULL,
  status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active','stale')),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  version BIGINT NOT NULL DEFAULT 1,
  CHECK (
    (purpose = 'requirement' AND intake_id IS NOT NULL AND plan_id IS NULL)
    OR
    (purpose = 'execution' AND plan_id IS NOT NULL AND intake_id IS NULL)
  )
);

CREATE UNIQUE INDEX agent_sessions_requirement_intake_idx
  ON agent_sessions(intake_id)
  WHERE purpose = 'requirement';

CREATE UNIQUE INDEX agent_sessions_execution_plan_idx
  ON agent_sessions(plan_id)
  WHERE purpose = 'execution';

CREATE INDEX agent_sessions_project_updated_idx
  ON agent_sessions(project_id, updated_at DESC);
