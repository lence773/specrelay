-- P001 extends immutable plan execution checkpoints with reviewable change
-- evidence and adds append-only feedback/revision traceability. Historical
-- migrations are intentionally left untouched.
ALTER TABLE plan_execution_snapshots
  ADD COLUMN change_summary JSONB NOT NULL DEFAULT '{}'::jsonb
    CHECK (jsonb_typeof(change_summary) = 'object'),
  ADD COLUMN additions INTEGER NOT NULL DEFAULT 0 CHECK (additions >= 0),
  ADD COLUMN deletions INTEGER NOT NULL DEFAULT 0 CHECK (deletions >= 0);

CREATE TABLE plan_execution_snapshot_files (
  id UUID PRIMARY KEY,
  snapshot_id UUID NOT NULL REFERENCES plan_execution_snapshots(id) ON DELETE CASCADE,
  file_sequence INTEGER NOT NULL CHECK (file_sequence > 0),
  path TEXT NOT NULL CHECK (length(trim(path)) > 0),
  previous_path TEXT NOT NULL DEFAULT '',
  file_status TEXT NOT NULL CHECK (file_status IN ('added','modified','deleted','renamed','copied','type_changed','unmerged','untracked')),
  staged BOOLEAN NOT NULL DEFAULT false,
  is_binary BOOLEAN NOT NULL DEFAULT false,
  additions INTEGER NOT NULL DEFAULT 0 CHECK (additions >= 0),
  deletions INTEGER NOT NULL DEFAULT 0 CHECK (deletions >= 0),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE(snapshot_id, file_sequence),
  CHECK ((file_status IN ('renamed','copied') AND length(trim(previous_path)) > 0) OR file_status NOT IN ('renamed','copied'))
);
CREATE INDEX plan_execution_snapshot_files_snapshot_idx
  ON plan_execution_snapshot_files(snapshot_id, file_sequence);

CREATE TABLE plan_execution_snapshot_diff_hunks (
  id UUID PRIMARY KEY,
  file_id UUID NOT NULL REFERENCES plan_execution_snapshot_files(id) ON DELETE CASCADE,
  hunk_sequence INTEGER NOT NULL CHECK (hunk_sequence > 0),
  hunk_header TEXT NOT NULL CHECK (length(trim(hunk_header)) > 0),
  patch TEXT NOT NULL,
  old_start_line INTEGER NOT NULL CHECK (old_start_line >= 0),
  old_line_count INTEGER NOT NULL CHECK (old_line_count >= 0),
  new_start_line INTEGER NOT NULL CHECK (new_start_line >= 0),
  new_line_count INTEGER NOT NULL CHECK (new_line_count >= 0),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE(file_id, hunk_sequence),
  CHECK (old_line_count > 0 OR new_line_count > 0)
);
CREATE INDEX plan_execution_snapshot_diff_hunks_file_idx
  ON plan_execution_snapshot_diff_hunks(file_id, hunk_sequence);

ALTER TABLE intakes
  ADD CONSTRAINT intakes_feedback_parent_required
  CHECK ((kind='requirement' AND parent_intake_id IS NULL) OR (kind='feedback' AND parent_intake_id IS NOT NULL));

CREATE OR REPLACE FUNCTION validate_feedback_parent_intake()
RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
  parent_project UUID;
  parent_kind TEXT;
BEGIN
  IF NEW.kind='requirement' THEN
    IF NEW.parent_intake_id IS NOT NULL THEN
      RAISE EXCEPTION 'requirement intake cannot have a parent' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
  END IF;
  SELECT project_id,kind INTO parent_project,parent_kind FROM intakes WHERE id=NEW.parent_intake_id;
  IF NOT FOUND OR parent_kind <> 'requirement' OR parent_project <> NEW.project_id THEN
    RAISE EXCEPTION 'feedback parent must be a requirement in the same project' USING ERRCODE='23503';
  END IF;
  RETURN NEW;
END;
$$;
CREATE TRIGGER intakes_validate_feedback_parent
  BEFORE INSERT OR UPDATE OF project_id,kind,parent_intake_id ON intakes
  FOR EACH ROW EXECUTE FUNCTION validate_feedback_parent_intake();

CREATE TABLE feedback_links (
  feedback_id UUID PRIMARY KEY REFERENCES intakes(id) ON DELETE CASCADE,
  project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  requirement_id UUID NOT NULL REFERENCES intakes(id) ON DELETE CASCADE,
  plan_id UUID REFERENCES plans(id) ON DELETE CASCADE,
  task_id UUID REFERENCES plan_tasks(id) ON DELETE CASCADE,
  checkpoint_id UUID REFERENCES plan_execution_snapshots(id) ON DELETE CASCADE,
  file_id UUID REFERENCES plan_execution_snapshot_files(id) ON DELETE CASCADE,
  diff_hunk_id UUID REFERENCES plan_execution_snapshot_diff_hunks(id) ON DELETE CASCADE,
  diff_line_side TEXT CHECK (diff_line_side IN ('old','new')),
  diff_line_start INTEGER,
  diff_line_end INTEGER,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  CHECK (plan_id IS NOT NULL OR (task_id IS NULL AND checkpoint_id IS NULL AND file_id IS NULL AND diff_hunk_id IS NULL)),
  CHECK (task_id IS NOT NULL OR (checkpoint_id IS NULL AND file_id IS NULL AND diff_hunk_id IS NULL)),
  CHECK (checkpoint_id IS NOT NULL OR (file_id IS NULL AND diff_hunk_id IS NULL)),
  CHECK (file_id IS NOT NULL OR diff_hunk_id IS NULL),
  CHECK (
    (diff_line_side IS NULL AND diff_line_start IS NULL AND diff_line_end IS NULL)
    OR (diff_hunk_id IS NOT NULL AND diff_line_side IS NOT NULL
        AND diff_line_start IS NOT NULL AND diff_line_end IS NOT NULL
        AND diff_line_start > 0 AND diff_line_end >= diff_line_start)
  )
);
CREATE INDEX feedback_links_requirement_idx ON feedback_links(requirement_id, created_at DESC);
CREATE INDEX feedback_links_plan_idx ON feedback_links(plan_id, created_at DESC) WHERE plan_id IS NOT NULL;
CREATE INDEX feedback_links_task_idx ON feedback_links(task_id, created_at DESC) WHERE task_id IS NOT NULL;
CREATE INDEX feedback_links_checkpoint_idx ON feedback_links(checkpoint_id, created_at DESC) WHERE checkpoint_id IS NOT NULL;

CREATE OR REPLACE FUNCTION validate_feedback_link()
RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
  feedback_parent UUID;
  feedback_project UUID;
  requirement_kind TEXT;
  requirement_project UUID;
  linked_plan RECORD;
  linked_task RECORD;
  linked_checkpoint RECORD;
  linked_file_snapshot UUID;
  linked_hunk RECORD;
BEGIN
  SELECT project_id,parent_intake_id INTO feedback_project,feedback_parent
  FROM intakes WHERE id=NEW.feedback_id AND kind='feedback';
  IF NOT FOUND THEN
    RAISE EXCEPTION 'feedback link must reference a feedback intake' USING ERRCODE='23503';
  END IF;
  SELECT project_id,kind INTO requirement_project,requirement_kind
  FROM intakes WHERE id=NEW.requirement_id;
  IF NOT FOUND OR requirement_kind <> 'requirement' THEN
    RAISE EXCEPTION 'feedback link must reference a requirement intake' USING ERRCODE='23503';
  END IF;
  IF feedback_project <> NEW.project_id OR requirement_project <> NEW.project_id
     OR feedback_parent IS DISTINCT FROM NEW.requirement_id THEN
    RAISE EXCEPTION 'feedback and parent requirement must belong to the linked project' USING ERRCODE='23503';
  END IF;

  IF NEW.plan_id IS NOT NULL THEN
    SELECT project_id,intake_id INTO linked_plan FROM plans WHERE id=NEW.plan_id;
    IF NOT FOUND OR linked_plan.project_id <> NEW.project_id OR linked_plan.intake_id <> NEW.requirement_id THEN
      RAISE EXCEPTION 'feedback plan does not belong to its parent requirement' USING ERRCODE='23503';
    END IF;
  END IF;
  IF NEW.task_id IS NOT NULL THEN
    SELECT project_id,plan_id INTO linked_task FROM plan_tasks WHERE id=NEW.task_id;
    IF NOT FOUND OR linked_task.project_id <> NEW.project_id OR linked_task.plan_id <> NEW.plan_id THEN
      RAISE EXCEPTION 'feedback task does not belong to its linked plan' USING ERRCODE='23503';
    END IF;
  END IF;
  IF NEW.checkpoint_id IS NOT NULL THEN
    SELECT project_id,plan_id,task_id,snapshot_kind INTO linked_checkpoint
    FROM plan_execution_snapshots WHERE id=NEW.checkpoint_id;
    IF NOT FOUND OR linked_checkpoint.project_id <> NEW.project_id
       OR linked_checkpoint.plan_id <> NEW.plan_id
       OR linked_checkpoint.task_id IS DISTINCT FROM NEW.task_id
       OR linked_checkpoint.snapshot_kind <> 'task_checkpoint' THEN
      RAISE EXCEPTION 'feedback checkpoint does not belong to its linked task' USING ERRCODE='23503';
    END IF;
  END IF;
  IF NEW.file_id IS NOT NULL THEN
    SELECT snapshot_id INTO linked_file_snapshot FROM plan_execution_snapshot_files WHERE id=NEW.file_id;
    IF NOT FOUND OR linked_file_snapshot <> NEW.checkpoint_id THEN
      RAISE EXCEPTION 'feedback file does not belong to its linked checkpoint' USING ERRCODE='23503';
    END IF;
  END IF;
  IF NEW.diff_hunk_id IS NOT NULL THEN
    SELECT file_id,old_start_line,old_line_count,new_start_line,new_line_count
      INTO linked_hunk FROM plan_execution_snapshot_diff_hunks WHERE id=NEW.diff_hunk_id;
    IF NOT FOUND OR linked_hunk.file_id <> NEW.file_id THEN
      RAISE EXCEPTION 'feedback diff hunk does not belong to its linked file' USING ERRCODE='23503';
    END IF;
    IF NEW.diff_line_side = 'old' AND
       (linked_hunk.old_line_count = 0 OR NEW.diff_line_start < linked_hunk.old_start_line
        OR NEW.diff_line_end >= linked_hunk.old_start_line + linked_hunk.old_line_count) THEN
      RAISE EXCEPTION 'feedback old-side line range is outside the linked diff hunk' USING ERRCODE='22023';
    ELSIF NEW.diff_line_side = 'new' AND
       (linked_hunk.new_line_count = 0 OR NEW.diff_line_start < linked_hunk.new_start_line
        OR NEW.diff_line_end >= linked_hunk.new_start_line + linked_hunk.new_line_count) THEN
      RAISE EXCEPTION 'feedback new-side line range is outside the linked diff hunk' USING ERRCODE='22023';
    END IF;
  END IF;
  RETURN NEW;
END;
$$;
CREATE TRIGGER feedback_links_validate
  BEFORE INSERT OR UPDATE ON feedback_links
  FOR EACH ROW EXECUTE FUNCTION validate_feedback_link();

-- Backfill the direct requirement edge for feedback created before this
-- migration. Optional execution-location fields remain null.
INSERT INTO feedback_links(feedback_id,project_id,requirement_id)
SELECT feedback.id,feedback.project_id,requirement.id
FROM intakes feedback
JOIN intakes requirement ON requirement.id=feedback.parent_intake_id
WHERE feedback.kind='feedback' AND requirement.kind='requirement'
ON CONFLICT (feedback_id) DO NOTHING;

CREATE TABLE feedback_revisions (
  id UUID PRIMARY KEY,
  feedback_id UUID NOT NULL REFERENCES feedback_links(feedback_id) ON DELETE CASCADE,
  project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  requirement_id UUID NOT NULL REFERENCES intakes(id) ON DELETE CASCADE,
  revision_intake_id UUID NOT NULL REFERENCES intakes(id) ON DELETE CASCADE,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE(feedback_id, revision_intake_id),
  CHECK (revision_intake_id <> requirement_id)
);
CREATE INDEX feedback_revisions_requirement_idx ON feedback_revisions(requirement_id, created_at DESC);
CREATE INDEX feedback_revisions_intake_idx ON feedback_revisions(revision_intake_id, created_at DESC);

CREATE TABLE feedback_revision_plans (
  id UUID PRIMARY KEY,
  feedback_revision_id UUID NOT NULL REFERENCES feedback_revisions(id) ON DELETE CASCADE,
  project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  revision_plan_id UUID NOT NULL REFERENCES plans(id) ON DELETE CASCADE,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE(feedback_revision_id, revision_plan_id)
);
CREATE INDEX feedback_revision_plans_plan_idx ON feedback_revision_plans(revision_plan_id, created_at DESC);

CREATE OR REPLACE FUNCTION validate_feedback_revision()
RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
  linked_feedback RECORD;
  revision_intake RECORD;
BEGIN
  SELECT project_id,requirement_id INTO linked_feedback FROM feedback_links WHERE feedback_id=NEW.feedback_id;
  IF NOT FOUND OR linked_feedback.project_id <> NEW.project_id OR linked_feedback.requirement_id <> NEW.requirement_id THEN
    RAISE EXCEPTION 'feedback revision does not match its feedback requirement chain' USING ERRCODE='23503';
  END IF;
  SELECT project_id,kind INTO revision_intake FROM intakes WHERE id=NEW.revision_intake_id;
  IF NOT FOUND OR revision_intake.project_id <> NEW.project_id OR revision_intake.kind <> 'requirement' THEN
    RAISE EXCEPTION 'feedback revision intake must be a requirement in the linked project' USING ERRCODE='23503';
  END IF;
  RETURN NEW;
END;
$$;
CREATE TRIGGER feedback_revisions_validate
  BEFORE INSERT OR UPDATE ON feedback_revisions
  FOR EACH ROW EXECUTE FUNCTION validate_feedback_revision();

CREATE OR REPLACE FUNCTION validate_feedback_revision_plan()
RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
  linked_revision RECORD;
  linked_plan RECORD;
BEGIN
  SELECT project_id,revision_intake_id INTO linked_revision FROM feedback_revisions WHERE id=NEW.feedback_revision_id;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'revision plan references a missing feedback revision' USING ERRCODE='23503';
  END IF;
  SELECT project_id,intake_id INTO linked_plan FROM plans WHERE id=NEW.revision_plan_id;
  IF NOT FOUND OR linked_revision.project_id <> NEW.project_id OR linked_plan.project_id <> NEW.project_id
     OR linked_plan.intake_id <> linked_revision.revision_intake_id THEN
    RAISE EXCEPTION 'revision plan does not belong to the feedback revision intake' USING ERRCODE='23503';
  END IF;
  RETURN NEW;
END;
$$;
CREATE TRIGGER feedback_revision_plans_validate
  BEFORE INSERT OR UPDATE ON feedback_revision_plans
  FOR EACH ROW EXECUTE FUNCTION validate_feedback_revision_plan();

CREATE TRIGGER plan_execution_snapshot_files_immutable
  BEFORE UPDATE OR DELETE ON plan_execution_snapshot_files
  FOR EACH ROW EXECUTE FUNCTION reject_execution_history_mutation();
CREATE TRIGGER plan_execution_snapshot_diff_hunks_immutable
  BEFORE UPDATE OR DELETE ON plan_execution_snapshot_diff_hunks
  FOR EACH ROW EXECUTE FUNCTION reject_execution_history_mutation();
CREATE TRIGGER feedback_links_immutable
  BEFORE UPDATE OR DELETE ON feedback_links
  FOR EACH ROW EXECUTE FUNCTION reject_execution_history_mutation();
CREATE TRIGGER feedback_revisions_immutable
  BEFORE UPDATE OR DELETE ON feedback_revisions
  FOR EACH ROW EXECUTE FUNCTION reject_execution_history_mutation();
CREATE TRIGGER feedback_revision_plans_immutable
  BEFORE UPDATE OR DELETE ON feedback_revision_plans
  FOR EACH ROW EXECUTE FUNCTION reject_execution_history_mutation();
