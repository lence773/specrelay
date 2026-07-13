CREATE TABLE IF NOT EXISTS schema_migrations (
  version BIGINT PRIMARY KEY,
  name TEXT NOT NULL,
  applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE projects (
  id UUID PRIMARY KEY,
  name TEXT NOT NULL CHECK (length(trim(name)) > 0),
  description TEXT NOT NULL DEFAULT '',
  workspace_path TEXT NOT NULL,
  workspace_path_normalized TEXT NOT NULL UNIQUE,
  automation_enabled BOOLEAN NOT NULL DEFAULT false,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  version BIGINT NOT NULL DEFAULT 1 CHECK (version > 0)
);
CREATE INDEX projects_updated_idx ON projects(updated_at DESC);

CREATE TABLE project_settings (
  id UUID PRIMARY KEY,
  project_id UUID NOT NULL UNIQUE REFERENCES projects(id) ON DELETE CASCADE,
  validation_command TEXT NOT NULL DEFAULT '',
  agent_provider TEXT NOT NULL DEFAULT 'codex' CHECK (agent_provider IN ('codex','claude')),
  codex_command TEXT NOT NULL DEFAULT 'codex',
  codex_args JSONB NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(codex_args) = 'array'),
  claude_command TEXT NOT NULL DEFAULT 'claude',
  claude_args JSONB NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(claude_args) = 'array'),
  plan_generation_timeout_seconds INTEGER NOT NULL DEFAULT 600 CHECK (plan_generation_timeout_seconds BETWEEN 1 AND 86400),
  task_execution_timeout_seconds INTEGER NOT NULL DEFAULT 3600 CHECK (task_execution_timeout_seconds BETWEEN 1 AND 86400),
  max_retries INTEGER NOT NULL DEFAULT 2 CHECK (max_retries BETWEEN 0 AND 10),
  allowed_env JSONB NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(allowed_env) = 'array'),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  version BIGINT NOT NULL DEFAULT 1 CHECK (version > 0)
);

CREATE TABLE intakes (
  id UUID PRIMARY KEY,
  project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  kind TEXT NOT NULL CHECK (kind IN ('requirement','feedback')),
  parent_intake_id UUID REFERENCES intakes(id) ON DELETE SET NULL,
  title TEXT NOT NULL CHECK (length(trim(title)) > 0),
  body TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open','planning','planned','closed','plan_failed')),
  config_snapshot JSONB NOT NULL DEFAULT '{}'::jsonb,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  version BIGINT NOT NULL DEFAULT 1 CHECK (version > 0),
  CHECK ((kind = 'requirement' AND parent_intake_id IS NULL) OR kind = 'feedback')
);
CREATE INDEX intakes_project_updated_idx ON intakes(project_id, updated_at DESC);
CREATE INDEX intakes_parent_idx ON intakes(parent_intake_id) WHERE parent_intake_id IS NOT NULL;

CREATE TABLE attachments (
  id UUID PRIMARY KEY,
  project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  intake_id UUID NOT NULL REFERENCES intakes(id) ON DELETE CASCADE,
  original_name TEXT NOT NULL,
  mime_type TEXT NOT NULL DEFAULT 'application/octet-stream',
  size_bytes BIGINT NOT NULL CHECK (size_bytes >= 0),
  sha256 TEXT NOT NULL CHECK (sha256 ~ '^[0-9a-f]{64}$'),
  storage_path TEXT NOT NULL UNIQUE,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  version BIGINT NOT NULL DEFAULT 1 CHECK (version > 0)
);
CREATE INDEX attachments_intake_idx ON attachments(intake_id, created_at);

CREATE TABLE plans (
  id UUID PRIMARY KEY,
  project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  intake_id UUID NOT NULL REFERENCES intakes(id) ON DELETE CASCADE,
  title TEXT NOT NULL,
  spec JSONB NOT NULL CHECK (jsonb_typeof(spec) = 'object'),
  markdown TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'generating' CHECK (status IN ('generating','ready','running','validating','completed','blocked','cancelled')),
  config_snapshot JSONB NOT NULL DEFAULT '{}'::jsonb,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  version BIGINT NOT NULL DEFAULT 1 CHECK (version > 0)
);
CREATE INDEX plans_project_updated_idx ON plans(project_id, updated_at DESC);
CREATE INDEX plans_intake_idx ON plans(intake_id, created_at DESC);

CREATE TABLE plan_tasks (
  id UUID PRIMARY KEY,
  project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  plan_id UUID NOT NULL REFERENCES plans(id) ON DELETE CASCADE,
  task_key TEXT NOT NULL CHECK (task_key ~ '^P[0-9]{3,}$'),
  position INTEGER NOT NULL CHECK (position > 0),
  title TEXT NOT NULL,
  scope JSONB NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(scope) = 'array'),
  acceptance JSONB NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(acceptance) = 'array'),
  status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','queued','running','succeeded','failed','cancelled')),
  session_id TEXT,
  started_at TIMESTAMPTZ,
  finished_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  version BIGINT NOT NULL DEFAULT 1 CHECK (version > 0),
  UNIQUE(plan_id, task_key), UNIQUE(plan_id, position)
);
CREATE INDEX plan_tasks_status_idx ON plan_tasks(plan_id, status, position);

CREATE TABLE jobs (
  id UUID PRIMARY KEY,
  project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  job_type TEXT NOT NULL,
  aggregate_type TEXT NOT NULL,
  aggregate_id UUID NOT NULL,
  payload JSONB NOT NULL DEFAULT '{}'::jsonb,
  priority INTEGER NOT NULL DEFAULT 100,
  status TEXT NOT NULL DEFAULT 'queued' CHECK (status IN ('queued','leased','running','retry_wait','succeeded','failed','cancelled')),
  run_after TIMESTAMPTZ NOT NULL DEFAULT now(),
  worker_id TEXT,
  lease_expires_at TIMESTAMPTZ,
  attempt INTEGER NOT NULL DEFAULT 0 CHECK (attempt >= 0),
  max_attempts INTEGER NOT NULL DEFAULT 3 CHECK (max_attempts >= 1),
  last_error TEXT,
  idempotency_key TEXT NOT NULL UNIQUE,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  version BIGINT NOT NULL DEFAULT 1 CHECK (version > 0)
);
CREATE INDEX jobs_claim_idx ON jobs(priority, run_after, created_at) WHERE status = 'queued';
CREATE INDEX jobs_project_status_idx ON jobs(project_id, status, created_at);

CREATE TABLE workspace_leases (
  id UUID PRIMARY KEY,
  project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  workspace_path_normalized TEXT NOT NULL UNIQUE,
  worker_id TEXT NOT NULL,
  job_id UUID NOT NULL UNIQUE REFERENCES jobs(id) ON DELETE CASCADE,
  heartbeat_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  expires_at TIMESTAMPTZ NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  version BIGINT NOT NULL DEFAULT 1 CHECK (version > 0)
);
CREATE INDEX workspace_leases_expiry_idx ON workspace_leases(expires_at);

CREATE TABLE events (
  id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  project_id UUID REFERENCES projects(id) ON DELETE CASCADE,
  event_type TEXT NOT NULL,
  aggregate_type TEXT NOT NULL,
  aggregate_id UUID NOT NULL,
  resource_version BIGINT NOT NULL,
  payload JSONB NOT NULL DEFAULT '{}'::jsonb,
  occurred_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX events_project_id_idx ON events(project_id, id);
CREATE INDEX events_aggregate_idx ON events(aggregate_type, aggregate_id, id);

CREATE TABLE agent_runs (
  id UUID PRIMARY KEY,
  project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  job_id UUID NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
  task_id UUID REFERENCES plan_tasks(id) ON DELETE SET NULL,
  provider TEXT NOT NULL CHECK (provider IN ('codex','claude','validation')),
  command_summary TEXT NOT NULL,
  pid INTEGER,
  session_id TEXT,
  status TEXT NOT NULL CHECK (status IN ('starting','running','succeeded','failed','cancelled','timed_out')),
  exit_code INTEGER,
  duration_ms BIGINT NOT NULL DEFAULT 0,
  log_path TEXT NOT NULL,
  termination_reason TEXT,
  started_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  finished_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  version BIGINT NOT NULL DEFAULT 1 CHECK (version > 0)
);
CREATE INDEX agent_runs_job_idx ON agent_runs(job_id, created_at DESC);

CREATE TABLE settings (
  id UUID PRIMARY KEY,
  key TEXT NOT NULL UNIQUE,
  value JSONB NOT NULL DEFAULT '{}'::jsonb,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  version BIGINT NOT NULL DEFAULT 1 CHECK (version > 0)
);

CREATE TABLE access_tokens (
  id UUID PRIMARY KEY,
  name TEXT NOT NULL,
  token_hash TEXT NOT NULL UNIQUE,
  kind TEXT NOT NULL CHECK (kind IN ('browser_bootstrap','mcp')),
  expires_at TIMESTAMPTZ,
  revoked_at TIMESTAMPTZ,
  last_used_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  version BIGINT NOT NULL DEFAULT 1 CHECK (version > 0)
);
