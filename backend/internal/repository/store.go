package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lyming99/specrelay/backend/internal/domain"
)

type Store struct{ Pool *pgxpool.Pool }

func New(pool *pgxpool.Pool) *Store { return &Store{Pool: pool} }

type executionProviderContextKey struct{}

type executionProviderSelection struct {
	Provider string
}

// WithExecutionProvider records the optional provider selected for this queueing
// request. Queue writers use the context value to persist the selection in the
// job payload before the transaction becomes visible to workers.
func WithExecutionProvider(ctx context.Context, provider string) context.Context {
	return context.WithValue(ctx, executionProviderContextKey{}, executionProviderSelection{Provider: strings.TrimSpace(provider)})
}

func executionProviderFromContext(ctx context.Context) (string, bool) {
	selection, ok := ctx.Value(executionProviderContextKey{}).(executionProviderSelection)
	return selection.Provider, ok
}

const planExecutionProviderKey = "executionAgentProvider"

func planConfigSnapshotWithProvider(raw json.RawMessage, provider string) (json.RawMessage, error) {
	snapshot := map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &snapshot); err != nil {
			return nil, fmt.Errorf("invalid plan config snapshot: %w", err)
		}
	}
	if snapshot == nil {
		snapshot = map[string]any{}
	}
	provider = strings.TrimSpace(provider)
	if provider == "" {
		delete(snapshot, planExecutionProviderKey)
	} else {
		snapshot[planExecutionProviderKey] = provider
	}
	return json.Marshal(snapshot)
}

func providerFromPlanConfigSnapshot(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	var snapshot map[string]json.RawMessage
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		return "", fmt.Errorf("invalid plan config snapshot: %w", err)
	}
	rawProvider := snapshot[planExecutionProviderKey]
	if len(rawProvider) == 0 {
		return "", nil
	}
	var provider string
	if err := json.Unmarshal(rawProvider, &provider); err != nil {
		return "", fmt.Errorf("invalid plan execution provider: %w", err)
	}
	return strings.TrimSpace(provider), nil
}

func planGenerationJobPayload(intakeID uuid.UUID, provider string) json.RawMessage {
	payload := map[string]any{"intakeId": intakeID}
	if provider = strings.TrimSpace(provider); provider != "" {
		payload["provider"] = provider
	}
	return mustJSON(payload)
}

func taskExecutionJobPayload(taskID, planID uuid.UUID, provider string, providerRequested bool) json.RawMessage {
	payload := map[string]any{"taskId": taskID, "planId": planID}
	if provider = strings.TrimSpace(provider); provider != "" {
		payload["provider"] = provider
	}
	if providerRequested {
		payload["providerRequested"] = true
	}
	return mustJSON(payload)
}

type CreateProjectParams struct{ Name, Description, WorkspacePath, NormalizedWorkspace string }
type UpdateProjectParams struct {
	Name, Description, WorkspacePath, NormalizedWorkspace string
	Version                                               int64
}
type CreateIntakeParams struct {
	ProjectID      uuid.UUID
	Kind           string
	ParentIntakeID *uuid.UUID
	Title, Body    string
	ConfigSnapshot json.RawMessage
	QueuePlan      bool
}
type UpdateIntakeParams struct {
	Title, Body, Status string
	Version             int64
}
type NewJob struct {
	ID                    uuid.UUID
	ProjectID             uuid.UUID
	Type, AggregateType   string
	AggregateID           uuid.UUID
	Payload               json.RawMessage
	Priority, MaxAttempts int
	RunAfter              time.Time
	IdempotencyKey        string
}
type NewEvent struct {
	ProjectID           *uuid.UUID
	Type, AggregateType string
	AggregateID         uuid.UUID
	ResourceVersion     int64
	Payload             json.RawMessage
}

func (s *Store) Ping(ctx context.Context) error { return s.Pool.Ping(ctx) }

func (s *Store) CreateProject(ctx context.Context, p CreateProjectParams) (domain.Project, error) {
	id := uuid.New()
	settingsID := uuid.New()
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return domain.Project{}, err
	}
	defer tx.Rollback(ctx)
	var out domain.Project
	err = tx.QueryRow(ctx, `INSERT INTO projects(id,name,description,workspace_path,workspace_path_normalized) VALUES($1,$2,$3,$4,$5) RETURNING id,name,description,workspace_path,automation_enabled,created_at,updated_at,version`, id, p.Name, p.Description, p.WorkspacePath, p.NormalizedWorkspace).Scan(&out.ID, &out.Name, &out.Description, &out.WorkspacePath, &out.AutomationEnabled, &out.CreatedAt, &out.UpdatedAt, &out.Version)
	if err != nil {
		return domain.Project{}, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO project_settings(id,project_id) VALUES($1,$2)`, settingsID, id); err != nil {
		return domain.Project{}, err
	}
	if _, err = insertEvent(ctx, tx, NewEvent{ProjectID: &id, Type: "project.created", AggregateType: "project", AggregateID: id, ResourceVersion: out.Version, Payload: json.RawMessage(`{}`)}); err != nil {
		return domain.Project{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return domain.Project{}, err
	}
	return out, nil
}

func scanProject(row pgx.Row) (domain.Project, error) {
	var p domain.Project
	err := row.Scan(&p.ID, &p.Name, &p.Description, &p.WorkspacePath, &p.AutomationEnabled, &p.CreatedAt, &p.UpdatedAt, &p.Version)
	return p, err
}
func (s *Store) GetProject(ctx context.Context, id uuid.UUID) (domain.Project, error) {
	p, err := scanProject(s.Pool.QueryRow(ctx, `SELECT id,name,description,workspace_path,automation_enabled,created_at,updated_at,version FROM projects WHERE id=$1`, id))
	return p, mapNotFound(err)
}
func (s *Store) ListProjects(ctx context.Context) ([]domain.Project, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id,name,description,workspace_path,automation_enabled,created_at,updated_at,version FROM projects ORDER BY updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.Project{}
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
func (s *Store) UpdateProject(ctx context.Context, id uuid.UUID, p UpdateProjectParams) (domain.Project, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return domain.Project{}, err
	}
	defer tx.Rollback(ctx)
	out, err := scanProject(tx.QueryRow(ctx, `UPDATE projects SET name=$2,description=$3,workspace_path=$4,workspace_path_normalized=$5,updated_at=now(),version=version+1 WHERE id=$1 AND version=$6 RETURNING id,name,description,workspace_path,automation_enabled,created_at,updated_at,version`, id, p.Name, p.Description, p.WorkspacePath, p.NormalizedWorkspace, p.Version))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Project{}, s.versionOrNotFound(ctx, tx, "projects", id)
	}
	if err != nil {
		return domain.Project{}, err
	}
	_, err = insertEvent(ctx, tx, NewEvent{ProjectID: &id, Type: "project.updated", AggregateType: "project", AggregateID: id, ResourceVersion: out.Version, Payload: json.RawMessage(`{}`)})
	if err != nil {
		return domain.Project{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return domain.Project{}, err
	}
	return out, nil
}
func (s *Store) DeleteProject(ctx context.Context, id uuid.UUID, version int64) error {
	tag, err := s.Pool.Exec(ctx, `DELETE FROM projects WHERE id=$1 AND version=$2`, id, version)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return s.versionOrNotFound(ctx, s.Pool, "projects", id)
	}
	return nil
}
func (s *Store) SetAutomation(ctx context.Context, id uuid.UUID, enabled bool, version int64) (domain.Project, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return domain.Project{}, err
	}
	defer tx.Rollback(ctx)
	out, err := scanProject(tx.QueryRow(ctx, `UPDATE projects SET automation_enabled=$2,updated_at=now(),version=version+1 WHERE id=$1 AND version=$3 RETURNING id,name,description,workspace_path,automation_enabled,created_at,updated_at,version`, id, enabled, version))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Project{}, s.versionOrNotFound(ctx, tx, "projects", id)
	}
	if err != nil {
		return domain.Project{}, err
	}
	if !enabled {
		type cancelledWork struct {
			jobID       uuid.UUID
			jobType     string
			aggregateID uuid.UUID
		}
		// The queue state is not sufficient to describe cancellation: a worker may
		// already be running a local CLI when automation is turned off. Record every
		// affected job so its task and agent-run state can be reconciled in this same
		// transaction, before the process termination signal is sent by the API layer.
		rows, queryErr := tx.Query(ctx, `UPDATE jobs SET status='cancelled',lease_expires_at=NULL,updated_at=now(),version=version+1 WHERE project_id=$1 AND status IN ('queued','retry_wait','leased','running') RETURNING id,job_type,aggregate_id`, id)
		if queryErr != nil {
			return domain.Project{}, queryErr
		}
		cancelled := []cancelledWork{}
		for rows.Next() {
			var work cancelledWork
			if err = rows.Scan(&work.jobID, &work.jobType, &work.aggregateID); err != nil {
				rows.Close()
				return domain.Project{}, err
			}
			cancelled = append(cancelled, work)
		}
		if err = rows.Err(); err != nil {
			rows.Close()
			return domain.Project{}, err
		}
		rows.Close()
		for _, work := range cancelled {
			switch work.jobType {
			case "task.execute":
				var resourceVersion int64
				err = tx.QueryRow(ctx, `UPDATE plan_tasks SET status='pending',started_at=NULL,finished_at=NULL,updated_at=now(),version=version+1 WHERE id=$1 AND status IN ('queued','running') RETURNING version`, work.aggregateID).Scan(&resourceVersion)
				if errors.Is(err, pgx.ErrNoRows) {
					err = nil
				} else if err == nil {
					_, err = insertEvent(ctx, tx, NewEvent{ProjectID: &id, Type: "task.cancelled", AggregateType: "task", AggregateID: work.aggregateID, ResourceVersion: resourceVersion, Payload: mustJSON(map[string]any{"reason": "project automation stopped"})})
				}
			case "plan.generate":
				var resourceVersion int64
				err = tx.QueryRow(ctx, `UPDATE intakes SET status='open',updated_at=now(),version=version+1 WHERE id=$1 AND status='planning' RETURNING version`, work.aggregateID).Scan(&resourceVersion)
				if errors.Is(err, pgx.ErrNoRows) {
					err = nil
				} else if err == nil {
					_, err = insertEvent(ctx, tx, NewEvent{ProjectID: &id, Type: "intake.plan_cancelled", AggregateType: "intake", AggregateID: work.aggregateID, ResourceVersion: resourceVersion, Payload: mustJSON(map[string]any{"reason": "project automation stopped"})})
				}
			}
			if err != nil {
				return domain.Project{}, err
			}
		}
		// A task may have been left queued after its job failed before the task
		// could start (for example, while waiting for the workspace lease). Those
		// failed jobs are not in the cancellation query above. Reconcile every plan
		// that still has queued/running work, even if legacy state incorrectly marked
		// it ready or completed after later tasks were skipped. Returning it to ready
		// preserves succeeded tasks and lets automation resume at the first unfinished
		// task in order.
		planRows, queryErr := tx.Query(ctx, `UPDATE plans p
			SET status='ready',updated_at=now(),version=version+1
			WHERE p.project_id=$1
			  AND (p.status IN ('running','validating') OR EXISTS (
				SELECT 1 FROM plan_tasks t
				WHERE t.plan_id=p.id AND t.status IN ('queued','running')
			  ))
			RETURNING p.id,p.version`, id)
		if queryErr != nil {
			return domain.Project{}, queryErr
		}
		type pausedPlan struct {
			id      uuid.UUID
			version int64
		}
		pausedPlans := []pausedPlan{}
		for planRows.Next() {
			var plan pausedPlan
			if err = planRows.Scan(&plan.id, &plan.version); err != nil {
				planRows.Close()
				return domain.Project{}, err
			}
			pausedPlans = append(pausedPlans, plan)
		}
		if err = planRows.Err(); err != nil {
			planRows.Close()
			return domain.Project{}, err
		}
		planRows.Close()
		for _, plan := range pausedPlans {
			if _, err = insertEvent(ctx, tx, NewEvent{ProjectID: &id, Type: "plan.ready", AggregateType: "plan", AggregateID: plan.id, ResourceVersion: plan.version, Payload: mustJSON(map[string]any{"reason": "project automation stopped"})}); err != nil {
				return domain.Project{}, err
			}
			rows, resetErr := tx.Query(ctx, `UPDATE plan_tasks
				SET status='pending',started_at=NULL,finished_at=NULL,updated_at=now(),version=version+1
				WHERE plan_id=$1 AND status IN ('queued','running')
				RETURNING id,version`, plan.id)
			if resetErr != nil {
				return domain.Project{}, resetErr
			}
			type resetTask struct {
				id      uuid.UUID
				version int64
			}
			resetTasks := []resetTask{}
			for rows.Next() {
				var task resetTask
				if err = rows.Scan(&task.id, &task.version); err != nil {
					rows.Close()
					return domain.Project{}, err
				}
				resetTasks = append(resetTasks, task)
			}
			if err = rows.Err(); err != nil {
				rows.Close()
				return domain.Project{}, err
			}
			rows.Close()
			for _, task := range resetTasks {
				if _, err = insertEvent(ctx, tx, NewEvent{ProjectID: &id, Type: "task.cancelled", AggregateType: "task", AggregateID: task.id, ResourceVersion: task.version, Payload: mustJSON(map[string]any{"reason": "project automation stopped"})}); err != nil {
					return domain.Project{}, err
				}
			}
		}

		// Mark recorded CLI executions as cancelled now. The process is signalled
		// immediately after commit; doing this here prevents a killed or detached
		// CLI from leaving the UI permanently in the running state.
		jobIDs := make([]uuid.UUID, 0, len(cancelled))
		for _, work := range cancelled {
			jobIDs = append(jobIDs, work.jobID)
		}
		if len(jobIDs) > 0 {
			if _, err = tx.Exec(ctx, `UPDATE agent_runs
				SET status='cancelled',
					termination_reason='project automation stopped',
					duration_ms=GREATEST(0, FLOOR(EXTRACT(EPOCH FROM now()-started_at)*1000)::bigint),
					finished_at=now(),updated_at=now(),version=version+1
				WHERE job_id=ANY($1) AND status='running'`, jobIDs); err != nil {
				return domain.Project{}, err
			}
		}
	}
	eventType := "project.automation_stopped"
	if enabled {
		eventType = "project.automation_started"
	}
	if _, err = insertEvent(ctx, tx, NewEvent{ProjectID: &id, Type: eventType, AggregateType: "project", AggregateID: id, ResourceVersion: out.Version, Payload: json.RawMessage(`{}`)}); err != nil {
		return domain.Project{}, err
	}
	if enabled {
		type pendingIntake struct {
			id      uuid.UUID
			version int64
		}
		rows, queryErr := tx.Query(ctx, `UPDATE intakes SET status='planning',updated_at=now(),version=version+1 WHERE project_id=$1 AND status IN ('open','plan_failed') RETURNING id,version`, id)
		if queryErr != nil {
			return domain.Project{}, queryErr
		}
		pending := []pendingIntake{}
		for rows.Next() {
			var intake pendingIntake
			if err = rows.Scan(&intake.id, &intake.version); err != nil {
				rows.Close()
				return domain.Project{}, err
			}
			pending = append(pending, intake)
		}
		if err = rows.Err(); err != nil {
			rows.Close()
			return domain.Project{}, err
		}
		rows.Close()
		if len(pending) > 0 {
			maxAttempts, maxErr := projectMaxAttempts(ctx, tx, id)
			if maxErr != nil {
				return domain.Project{}, maxErr
			}
			for _, intake := range pending {
				job, insertErr := insertJob(ctx, tx, NewJob{ID: uuid.New(), ProjectID: id, Type: "plan.generate", AggregateType: "intake", AggregateID: intake.id, Payload: planGenerationJobPayload(intake.id, ""), Priority: 100, MaxAttempts: maxAttempts, RunAfter: time.Now(), IdempotencyKey: fmt.Sprintf("plan.generate:%s:%d", intake.id, intake.version)})
				if insertErr != nil {
					return domain.Project{}, insertErr
				}
				if _, err = insertEvent(ctx, tx, NewEvent{ProjectID: &id, Type: "intake.planning", AggregateType: "intake", AggregateID: intake.id, ResourceVersion: intake.version, Payload: mustJSON(map[string]any{"jobId": job.ID, "trigger": "automation_started"})}); err != nil {
					return domain.Project{}, err
				}
			}
		}
		type readyPlan struct {
			id      uuid.UUID
			version int64
		}
		readyRows, queryErr := tx.Query(ctx, `SELECT id,version FROM plans WHERE project_id=$1 AND status='ready' ORDER BY created_at FOR UPDATE`, id)
		if queryErr != nil {
			return domain.Project{}, queryErr
		}
		readyPlans := []readyPlan{}
		for readyRows.Next() {
			var plan readyPlan
			if err = readyRows.Scan(&plan.id, &plan.version); err != nil {
				readyRows.Close()
				return domain.Project{}, err
			}
			readyPlans = append(readyPlans, plan)
		}
		if err = readyRows.Err(); err != nil {
			readyRows.Close()
			return domain.Project{}, err
		}
		readyRows.Close()
		for _, plan := range readyPlans {
			if _, err = s.queuePlanTx(ctx, tx, plan.id, plan.version, true); err != nil {
				return domain.Project{}, err
			}
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return domain.Project{}, err
	}
	return out, nil
}

func (s *Store) GetProjectSettings(ctx context.Context, id uuid.UUID) (domain.ProjectSettings, error) {
	var p domain.ProjectSettings
	err := s.Pool.QueryRow(ctx, `SELECT project_id,validation_command,agent_provider,codex_command,codex_args,claude_command,claude_args,plan_generation_timeout_seconds,task_execution_timeout_seconds,max_retries,allowed_env,created_at,updated_at,version FROM project_settings WHERE project_id=$1`, id).Scan(&p.ProjectID, &p.ValidationCommand, &p.AgentProvider, &p.CodexCommand, &p.CodexArgs, &p.ClaudeCommand, &p.ClaudeArgs, &p.PlanGenerationTimeoutSecs, &p.TaskExecutionTimeoutSecs, &p.MaxRetries, &p.AllowedEnv, &p.CreatedAt, &p.UpdatedAt, &p.Version)
	return p, mapNotFound(err)
}
func (s *Store) UpdateProjectSettings(ctx context.Context, p domain.ProjectSettings) (domain.ProjectSettings, error) {
	var out domain.ProjectSettings
	err := s.Pool.QueryRow(ctx, `UPDATE project_settings SET validation_command=$2,agent_provider=$3,codex_command=$4,codex_args=$5,claude_command=$6,claude_args=$7,plan_generation_timeout_seconds=$8,task_execution_timeout_seconds=$9,max_retries=$10,allowed_env=$11,updated_at=now(),version=version+1 WHERE project_id=$1 AND version=$12 RETURNING project_id,validation_command,agent_provider,codex_command,codex_args,claude_command,claude_args,plan_generation_timeout_seconds,task_execution_timeout_seconds,max_retries,allowed_env,created_at,updated_at,version`, p.ProjectID, p.ValidationCommand, p.AgentProvider, p.CodexCommand, p.CodexArgs, p.ClaudeCommand, p.ClaudeArgs, p.PlanGenerationTimeoutSecs, p.TaskExecutionTimeoutSecs, p.MaxRetries, p.AllowedEnv, p.Version).Scan(&out.ProjectID, &out.ValidationCommand, &out.AgentProvider, &out.CodexCommand, &out.CodexArgs, &out.ClaudeCommand, &out.ClaudeArgs, &out.PlanGenerationTimeoutSecs, &out.TaskExecutionTimeoutSecs, &out.MaxRetries, &out.AllowedEnv, &out.CreatedAt, &out.UpdatedAt, &out.Version)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, domain.ErrVersionConflict
	}
	return out, err
}

func (s *Store) CreateIntake(ctx context.Context, p CreateIntakeParams) (domain.Intake, *domain.Job, error) {
	p.Kind = strings.TrimSpace(p.Kind)
	id := uuid.New()
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return domain.Intake{}, nil, err
	}
	defer tx.Rollback(ctx)
	if err = validateIntakeParent(ctx, tx, p); err != nil {
		return domain.Intake{}, nil, err
	}
	if len(p.ConfigSnapshot) == 0 {
		p.ConfigSnapshot = json.RawMessage(`{}`)
	}
	status := "open"
	if p.QueuePlan {
		status = "planning"
	}
	var out domain.Intake
	err = tx.QueryRow(ctx, `INSERT INTO intakes(id,project_id,kind,parent_intake_id,title,body,status,config_snapshot) VALUES($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id,project_id,kind,parent_intake_id,title,body,status,config_snapshot,created_at,updated_at,version`, id, p.ProjectID, p.Kind, p.ParentIntakeID, p.Title, p.Body, status, p.ConfigSnapshot).Scan(&out.ID, &out.ProjectID, &out.Kind, &out.ParentIntakeID, &out.Title, &out.Body, &out.Status, &out.ConfigSnapshot, &out.CreatedAt, &out.UpdatedAt, &out.Version)
	if err != nil {
		return domain.Intake{}, nil, err
	}
	_, err = insertEvent(ctx, tx, NewEvent{ProjectID: &p.ProjectID, Type: "intake.created", AggregateType: "intake", AggregateID: id, ResourceVersion: out.Version, Payload: mustJSON(map[string]any{"kind": p.Kind, "status": status})})
	if err != nil {
		return domain.Intake{}, nil, err
	}
	var job *domain.Job
	if p.QueuePlan {
		maxAttempts, err := projectMaxAttempts(ctx, tx, p.ProjectID)
		if err != nil {
			return domain.Intake{}, nil, err
		}
		provider, _ := executionProviderFromContext(ctx)
		j, err := insertJob(ctx, tx, NewJob{ID: uuid.New(), ProjectID: p.ProjectID, Type: "plan.generate", AggregateType: "intake", AggregateID: id, Payload: planGenerationJobPayload(id, provider), Priority: 100, MaxAttempts: maxAttempts, RunAfter: time.Now(), IdempotencyKey: fmt.Sprintf("plan.generate:%s:%d", id, out.Version)})
		if err != nil {
			return domain.Intake{}, nil, err
		}
		job = &j
	}
	if err = tx.Commit(ctx); err != nil {
		return domain.Intake{}, nil, err
	}
	return out, job, nil
}

func validateIntakeParent(ctx context.Context, tx pgx.Tx, p CreateIntakeParams) error {
	switch p.Kind {
	case "requirement":
		if p.ParentIntakeID != nil {
			return errors.New("a requirement cannot have a parent intake")
		}
		return nil
	case "feedback":
		if p.ParentIntakeID == nil {
			return errors.New("feedback must be linked to a requirement")
		}
		var projectID uuid.UUID
		var kind string
		err := tx.QueryRow(ctx, `SELECT project_id,kind FROM intakes WHERE id=$1`, *p.ParentIntakeID).Scan(&projectID, &kind)
		if errors.Is(err, pgx.ErrNoRows) {
			return errors.New("feedback parent requirement was not found")
		}
		if err != nil {
			return err
		}
		if projectID != p.ProjectID {
			return errors.New("feedback parent requirement belongs to another project")
		}
		if kind != "requirement" {
			return errors.New("feedback must be linked directly to a requirement")
		}
		return nil
	default:
		return fmt.Errorf("unsupported intake kind %q", p.Kind)
	}
}

func scanIntake(row pgx.Row) (domain.Intake, error) {
	var i domain.Intake
	err := row.Scan(&i.ID, &i.ProjectID, &i.Kind, &i.ParentIntakeID, &i.Title, &i.Body, &i.Status, &i.ConfigSnapshot, &i.CreatedAt, &i.UpdatedAt, &i.Version)
	return i, err
}
func (s *Store) GetIntake(ctx context.Context, id uuid.UUID) (domain.Intake, error) {
	i, err := scanIntake(s.Pool.QueryRow(ctx, `SELECT id,project_id,kind,parent_intake_id,title,body,status,config_snapshot,created_at,updated_at,version FROM intakes WHERE id=$1`, id))
	return i, mapNotFound(err)
}
func (s *Store) ListIntakes(ctx context.Context, projectID uuid.UUID) ([]domain.Intake, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id,project_id,kind,parent_intake_id,title,body,status,config_snapshot,created_at,updated_at,version FROM intakes WHERE project_id=$1 ORDER BY created_at DESC`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.Intake{}
	for rows.Next() {
		i, err := scanIntake(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}
func (s *Store) UpdateIntake(ctx context.Context, id uuid.UUID, p UpdateIntakeParams) (domain.Intake, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return domain.Intake{}, err
	}
	defer tx.Rollback(ctx)
	out, err := scanIntake(tx.QueryRow(ctx, `UPDATE intakes SET title=$2,body=$3,status=$4,updated_at=now(),version=version+1 WHERE id=$1 AND version=$5 RETURNING id,project_id,kind,parent_intake_id,title,body,status,config_snapshot,created_at,updated_at,version`, id, p.Title, p.Body, p.Status, p.Version))
	if errors.Is(err, pgx.ErrNoRows) {
		return out, s.versionOrNotFound(ctx, tx, "intakes", id)
	}
	if err != nil {
		return out, err
	}
	_, err = insertEvent(ctx, tx, NewEvent{ProjectID: &out.ProjectID, Type: "intake.updated", AggregateType: "intake", AggregateID: id, ResourceVersion: out.Version, Payload: mustJSON(map[string]any{"status": out.Status})})
	if err != nil {
		return out, err
	}
	if err = tx.Commit(ctx); err != nil {
		return out, err
	}
	return out, nil
}
func (s *Store) QueuePlanGeneration(ctx context.Context, intakeID uuid.UUID, version int64) (domain.Job, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return domain.Job{}, err
	}
	defer tx.Rollback(ctx)
	var projectID uuid.UUID
	var nextVersion int64
	err = tx.QueryRow(ctx, `UPDATE intakes SET status='planning',updated_at=now(),version=version+1 WHERE id=$1 AND version=$2 AND status IN ('open','plan_failed') RETURNING project_id,version`, intakeID, version).Scan(&projectID, &nextVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		var currentVersion int64
		checkErr := tx.QueryRow(ctx, `SELECT version FROM intakes WHERE id=$1`, intakeID).Scan(&currentVersion)
		if errors.Is(checkErr, pgx.ErrNoRows) {
			return domain.Job{}, domain.ErrNotFound
		}
		if checkErr != nil {
			return domain.Job{}, checkErr
		}
		if currentVersion != version {
			return domain.Job{}, domain.ErrVersionConflict
		}
		return domain.Job{}, domain.ErrInvalidTransition
	}
	if err != nil {
		return domain.Job{}, err
	}
	maxAttempts, err := projectMaxAttempts(ctx, tx, projectID)
	if err != nil {
		return domain.Job{}, err
	}
	provider, _ := executionProviderFromContext(ctx)
	job, err := insertJob(ctx, tx, NewJob{ID: uuid.New(), ProjectID: projectID, Type: "plan.generate", AggregateType: "intake", AggregateID: intakeID, Payload: planGenerationJobPayload(intakeID, provider), Priority: 100, MaxAttempts: maxAttempts, RunAfter: time.Now(), IdempotencyKey: fmt.Sprintf("plan.generate:%s:%d", intakeID, nextVersion)})
	if err != nil {
		return domain.Job{}, err
	}
	_, err = insertEvent(ctx, tx, NewEvent{ProjectID: &projectID, Type: "intake.planning", AggregateType: "intake", AggregateID: intakeID, ResourceVersion: nextVersion, Payload: mustJSON(map[string]any{"jobId": job.ID})})
	if err != nil {
		return domain.Job{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return domain.Job{}, err
	}
	return job, nil
}

func scanPlan(row pgx.Row) (domain.Plan, error) {
	var p domain.Plan
	err := row.Scan(&p.ID, &p.ProjectID, &p.IntakeID, &p.Title, &p.Spec, &p.Markdown, &p.Status, &p.ConfigSnapshot, &p.CreatedAt, &p.UpdatedAt, &p.Version)
	return p, err
}
func (s *Store) GetPlan(ctx context.Context, id uuid.UUID) (domain.Plan, error) {
	p, err := scanPlan(s.Pool.QueryRow(ctx, `SELECT id,project_id,intake_id,title,spec,markdown,status,config_snapshot,created_at,updated_at,version FROM plans WHERE id=$1`, id))
	return p, mapNotFound(err)
}
func (s *Store) ListPlansForIntake(ctx context.Context, intakeID uuid.UUID) ([]domain.Plan, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id,project_id,intake_id,title,spec,markdown,status,config_snapshot,created_at,updated_at,version FROM plans WHERE intake_id=$1 ORDER BY created_at DESC`, intakeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.Plan{}
	for rows.Next() {
		p, err := scanPlan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) ListPlans(ctx context.Context, projectID uuid.UUID) ([]domain.Plan, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id,project_id,intake_id,title,spec,markdown,status,config_snapshot,created_at,updated_at,version FROM plans WHERE project_id=$1 ORDER BY created_at DESC`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.Plan{}
	for rows.Next() {
		p, err := scanPlan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
func scanTask(row pgx.Row) (domain.PlanTask, error) {
	var t domain.PlanTask
	err := row.Scan(&t.ID, &t.ProjectID, &t.PlanID, &t.TaskKey, &t.Position, &t.Title, &t.Scope, &t.Acceptance, &t.Status, &t.SessionID, &t.StartedAt, &t.FinishedAt, &t.CreatedAt, &t.UpdatedAt, &t.Version)
	return t, err
}
func (s *Store) ListTasks(ctx context.Context, planID uuid.UUID) ([]domain.PlanTask, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id,project_id,plan_id,task_key,position,title,scope,acceptance,status,coalesce(session_id,''),started_at,finished_at,created_at,updated_at,version FROM plan_tasks WHERE plan_id=$1 ORDER BY position`, planID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.PlanTask{}
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
func (s *Store) GetTask(ctx context.Context, id uuid.UUID) (domain.PlanTask, error) {
	t, err := scanTask(s.Pool.QueryRow(ctx, `SELECT id,project_id,plan_id,task_key,position,title,scope,acceptance,status,coalesce(session_id,''),started_at,finished_at,created_at,updated_at,version FROM plan_tasks WHERE id=$1`, id))
	return t, mapNotFound(err)
}

type EventPage struct {
	Items      []domain.Event `json:"items"`
	HasMore    bool           `json:"hasMore"`
	NextBefore *int64         `json:"nextBefore,omitempty"`
}

// ListEventPage returns a project's visible events from newest to oldest. The
// before cursor is exclusive and should be the NextBefore value from the
// previous page.
func (s *Store) ListEventPage(ctx context.Context, projectID uuid.UUID, before *int64, limit int) (EventPage, error) {
	if limit <= 0 || limit > 1000 {
		limit = 10
	}
	query := `SELECT id,project_id,event_type,aggregate_type,aggregate_id,resource_version,payload,occurred_at
		FROM events
		WHERE project_id=$1 AND event_type <> 'agent.output'`
	args := []any{projectID}
	if before != nil {
		query += ` AND id<$2`
		args = append(args, *before)
	}
	query += ` ORDER BY id DESC LIMIT ` + fmt.Sprint(limit+1)
	rows, err := s.Pool.Query(ctx, query, args...)
	if err != nil {
		return EventPage{}, err
	}
	defer rows.Close()
	items, err := scanEvents(rows)
	if err != nil {
		return EventPage{}, err
	}
	page := EventPage{Items: items}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		page.HasMore = true
		nextBefore := page.Items[len(page.Items)-1].ID
		page.NextBefore = &nextBefore
	}
	return page, nil
}

// ListEvents returns visible events after an exclusive SSE cursor in ascending
// ID order.
func (s *Store) ListEvents(ctx context.Context, projectID *uuid.UUID, after int64, limit int) ([]domain.Event, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	query := `SELECT id,project_id,event_type,aggregate_type,aggregate_id,resource_version,payload,occurred_at FROM events WHERE id>$1 AND event_type <> 'agent.output'`
	args := []any{after}
	if projectID != nil {
		query += ` AND project_id=$2`
		args = append(args, *projectID)
	}
	query += ` ORDER BY id ASC LIMIT ` + fmt.Sprint(limit)
	rows, err := s.Pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEvents(rows)
}

func scanEvents(rows pgx.Rows) ([]domain.Event, error) {
	out := []domain.Event{}
	for rows.Next() {
		var e domain.Event
		if err := rows.Scan(&e.ID, &e.ProjectID, &e.Type, &e.AggregateType, &e.AggregateID, &e.ResourceVersion, &e.Payload, &e.OccurredAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func insertJob(ctx context.Context, tx pgx.Tx, p NewJob) (domain.Job, error) {
	if p.ID == uuid.Nil {
		p.ID = uuid.New()
	}
	if p.MaxAttempts < 1 {
		p.MaxAttempts = 3
	}
	if p.RunAfter.IsZero() {
		p.RunAfter = time.Now()
	}
	if len(p.Payload) == 0 {
		p.Payload = json.RawMessage(`{}`)
	}
	var j domain.Job
	err := tx.QueryRow(ctx, `INSERT INTO jobs(id,project_id,job_type,aggregate_type,aggregate_id,payload,priority,max_attempts,run_after,idempotency_key) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) ON CONFLICT(idempotency_key) DO UPDATE SET idempotency_key=EXCLUDED.idempotency_key RETURNING id,project_id,job_type,aggregate_type,aggregate_id,payload,priority,status,run_after,coalesce(worker_id,''),lease_expires_at,attempt,max_attempts,coalesce(last_error,''),idempotency_key,created_at,updated_at,version`, p.ID, p.ProjectID, p.Type, p.AggregateType, p.AggregateID, p.Payload, p.Priority, p.MaxAttempts, p.RunAfter, p.IdempotencyKey).Scan(&j.ID, &j.ProjectID, &j.Type, &j.AggregateType, &j.AggregateID, &j.Payload, &j.Priority, &j.Status, &j.RunAfter, &j.WorkerID, &j.LeaseExpiresAt, &j.Attempt, &j.MaxAttempts, &j.LastError, &j.IdempotencyKey, &j.CreatedAt, &j.UpdatedAt, &j.Version)
	if err == nil {
		_, err = tx.Exec(ctx, `SELECT pg_notify('specrelay_jobs',$1)`, j.ID.String())
	}
	return j, err
}

func projectMaxAttempts(ctx context.Context, tx pgx.Tx, projectID uuid.UUID) (int, error) {
	var maxRetries int
	if err := tx.QueryRow(ctx, `SELECT max_retries FROM project_settings WHERE project_id=$1`, projectID).Scan(&maxRetries); err != nil {
		return 0, err
	}
	if maxRetries < 0 {
		maxRetries = 0
	}
	return maxRetries + 1, nil
}

func insertEvent(ctx context.Context, tx pgx.Tx, p NewEvent) (int64, error) {
	if p.Type == "agent.output" {
		return 0, nil
	}
	if len(p.Payload) == 0 {
		p.Payload = json.RawMessage(`{}`)
	}
	var id int64
	err := tx.QueryRow(ctx, `INSERT INTO events(project_id,event_type,aggregate_type,aggregate_id,resource_version,payload) VALUES($1,$2,$3,$4,$5,$6) RETURNING id`, p.ProjectID, p.Type, p.AggregateType, p.AggregateID, p.ResourceVersion, p.Payload).Scan(&id)
	if err == nil {
		_, err = tx.Exec(ctx, `SELECT pg_notify('specrelay_events',$1)`, fmt.Sprint(id))
	}
	return id, err
}

// AppendEvent persists a non-state-changing event such as an agent log
// reference. Domain state transitions should continue to insert their event in
// the same transaction as the resource update.
func (s *Store) AppendEvent(ctx context.Context, p NewEvent) (int64, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	id, err := insertEvent(ctx, tx, p)
	if err != nil {
		return 0, err
	}
	if err = tx.Commit(ctx); err != nil {
		return 0, err
	}
	return id, nil
}
func mapNotFound(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ErrNotFound
	}
	return err
}

type queryRower interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func (s *Store) versionOrNotFound(ctx context.Context, q queryRower, table string, id uuid.UUID) error {
	var exists bool
	err := q.QueryRow(ctx, fmt.Sprintf(`SELECT EXISTS(SELECT 1 FROM %s WHERE id=$1)`, table), id).Scan(&exists)
	if err != nil {
		return err
	}
	if exists {
		return domain.ErrVersionConflict
	}
	return domain.ErrNotFound
}
func mustJSON(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
