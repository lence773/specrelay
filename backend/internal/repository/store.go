package repository

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lyming99/specrelay/backend/internal/domain"
	"github.com/lyming99/specrelay/backend/internal/security"
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
	ProjectID                  uuid.UUID
	Kind                       string
	ParentIntakeID             *uuid.UUID
	Title, Body                string
	ConfigSnapshot             json.RawMessage
	QueuePlan                  bool
	RequirementSessionID       string
	RequirementSessionProvider string
	Feedback                   *FeedbackAssociationParams
}

type FeedbackAssociationParams struct {
	PlanID        *uuid.UUID
	TaskID        *uuid.UUID
	CheckpointID  *uuid.UUID
	FileID        *uuid.UUID
	DiffHunkID    *uuid.UUID
	DiffLineSide  string
	DiffLineStart *int
	DiffLineEnd   *int
}

type PlanExecutionCheckpointParams struct {
	ProjectID     uuid.UUID
	PlanID        uuid.UUID
	TaskID        uuid.UUID
	ChangeSummary json.RawMessage
	Files         []PlanExecutionCheckpointFile
}

type PlanExecutionCheckpointFile struct {
	Path         string
	PreviousPath string
	Status       string
	Staged       bool
	Binary       bool
	Additions    int
	Deletions    int
	Hunks        []PlanExecutionCheckpointHunk
}

type PlanExecutionCheckpointHunk struct {
	Header       string
	Patch        string
	OldStartLine int
	OldLineCount int
	NewStartLine int
	NewLineCount int
}

type RecordFeedbackRevisionParams struct {
	ProjectID        uuid.UUID
	FeedbackID       uuid.UUID
	RevisionIntakeID uuid.UUID
	RevisionPlanID   *uuid.UUID
}

type FeedbackQuery struct {
	RequirementID *uuid.UUID
	PlanID        *uuid.UUID
	TaskID        *uuid.UUID
	CheckpointID  *uuid.UUID
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
		const automationStopReason = "project automation stopped"
		type cancelledWork struct {
			jobID       uuid.UUID
			jobType     string
			aggregateID uuid.UUID
			status      string
		}
		// Lock the complete work set first, then route every status change through
		// the lifecycle gate so job, task, plan, and run audit rows commit together.
		rows, queryErr := tx.Query(ctx, `SELECT id,job_type,aggregate_id,status
			FROM jobs WHERE project_id=$1 AND status IN ('queued','retry_wait','leased','running')
			FOR UPDATE`, id)
		if queryErr != nil {
			return domain.Project{}, queryErr
		}
		cancelled := []cancelledWork{}
		for rows.Next() {
			var work cancelledWork
			if err = rows.Scan(&work.jobID, &work.jobType, &work.aggregateID, &work.status); err != nil {
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
			if _, err = transitionLifecycleTx(ctx, tx, lifecycleTransitionRequest{
				LifecycleTransitionParams: LifecycleTransitionParams{
					ResourceType: domain.LifecycleResourceJob,
					ResourceID:   work.jobID,
					Status:       "cancelled",
					StatusSource: domain.LifecycleSourceAutomation,
					ReasonCode:   domain.LifecycleReasonAutomationDisabled,
					Reason:       automationStopReason,
					RecoveryHint: domain.LifecycleRecoveryNone,
				},
				ExpectedStatuses: []string{work.status},
				AllowNonContract: true,
				IgnoreTerminal:   true,
				Fields:           []lifecycleFieldUpdate{{Column: "lease_expires_at", SQL: "NULL"}},
			}); err != nil {
				return domain.Project{}, err
			}
			if work.jobType != "plan.generate" {
				continue
			}
			var resourceVersion int64
			err = tx.QueryRow(ctx, `UPDATE intakes SET status='open',updated_at=now(),version=version+1
				WHERE id=$1 AND status='planning' RETURNING version`, work.aggregateID).Scan(&resourceVersion)
			if errors.Is(err, pgx.ErrNoRows) {
				err = nil
			} else if err == nil {
				_, err = insertEvent(ctx, tx, NewEvent{ProjectID: &id, Type: "intake.plan_cancelled", AggregateType: "intake", AggregateID: work.aggregateID, ResourceVersion: resourceVersion, Payload: mustJSON(map[string]any{"reason": automationStopReason})})
			}
			if err != nil {
				return domain.Project{}, err
			}
		}

		// Capture plans before tasks are reset, including plans whose queue state is
		// inconsistent with their projection. Durable terminal plans are protected
		// by the gate and therefore cannot be reopened by this reconciliation.
		planRows, queryErr := tx.Query(ctx, `SELECT p.id
			FROM plans p
			WHERE p.project_id=$1
			  AND (p.status IN ('running','validating') OR EXISTS (
				SELECT 1 FROM plan_tasks t
				WHERE t.plan_id=p.id AND t.status IN ('queued','running')
			  ))
			FOR UPDATE`, id)
		if queryErr != nil {
			return domain.Project{}, queryErr
		}
		pausedPlans := []uuid.UUID{}
		for planRows.Next() {
			var planID uuid.UUID
			if err = planRows.Scan(&planID); err != nil {
				planRows.Close()
				return domain.Project{}, err
			}
			pausedPlans = append(pausedPlans, planID)
		}
		if err = planRows.Err(); err != nil {
			planRows.Close()
			return domain.Project{}, err
		}
		planRows.Close()
		for _, planID := range pausedPlans {
			planTransition, transitionErr := transitionLifecycleTx(ctx, tx, lifecycleTransitionRequest{
				LifecycleTransitionParams: LifecycleTransitionParams{
					ResourceType: domain.LifecycleResourcePlan,
					ResourceID:   planID,
					Status:       "ready",
					StatusSource: domain.LifecycleSourceAutomation,
					ReasonCode:   domain.LifecycleReasonAutomationDisabled,
					Reason:       automationStopReason,
					RecoveryHint: domain.LifecycleRecoveryRetryFromStart,
				},
				ExpectedStatuses: []string{"ready", "running", "validating", "completed", "failed", "cancelled"},
				AllowNonContract: true,
				IgnoreTerminal:   true,
			})
			if transitionErr != nil {
				return domain.Project{}, transitionErr
			}
			if !planTransition.Idempotent {
				if _, err = insertEvent(ctx, tx, NewEvent{ProjectID: &id, Type: "plan.ready", AggregateType: "plan", AggregateID: planID, ResourceVersion: planTransition.State.Version, Payload: mustJSON(map[string]any{"reason": automationStopReason})}); err != nil {
					return domain.Project{}, err
				}
			}

			taskRows, resetErr := tx.Query(ctx, `SELECT id FROM plan_tasks
				WHERE plan_id=$1 AND status IN ('queued','running') FOR UPDATE`, planID)
			if resetErr != nil {
				return domain.Project{}, resetErr
			}
			resetTasks := []uuid.UUID{}
			for taskRows.Next() {
				var taskID uuid.UUID
				if err = taskRows.Scan(&taskID); err != nil {
					taskRows.Close()
					return domain.Project{}, err
				}
				resetTasks = append(resetTasks, taskID)
			}
			if err = taskRows.Err(); err != nil {
				taskRows.Close()
				return domain.Project{}, err
			}
			taskRows.Close()
			for _, taskID := range resetTasks {
				relatedJobID, lookupErr := currentTaskJobID(ctx, tx, taskID)
				if lookupErr != nil {
					return domain.Project{}, lookupErr
				}
				taskTransition, transitionErr := transitionLifecycleTx(ctx, tx, lifecycleTransitionRequest{
					LifecycleTransitionParams: LifecycleTransitionParams{
						ResourceType: domain.LifecycleResourceTask,
						ResourceID:   taskID,
						Status:       "pending",
						StatusSource: domain.LifecycleSourceAutomation,
						ReasonCode:   domain.LifecycleReasonAutomationDisabled,
						Reason:       automationStopReason,
						RecoveryHint: domain.LifecycleRecoveryRetryFromStart,
						RelatedJobID: relatedJobID,
					},
					ExpectedStatuses: []string{"queued", "running"},
					AllowNonContract: true,
					IgnoreTerminal:   true,
					Fields: []lifecycleFieldUpdate{
						{Column: "started_at", SQL: "NULL"},
						{Column: "finished_at", SQL: "NULL"},
					},
				})
				if transitionErr != nil {
					return domain.Project{}, transitionErr
				}
				if !taskTransition.Idempotent {
					if _, err = insertEvent(ctx, tx, NewEvent{ProjectID: &id, Type: "task.cancelled", AggregateType: "task", AggregateID: taskID, ResourceVersion: taskTransition.State.Version, Payload: mustJSON(map[string]any{"reason": automationStopReason})}); err != nil {
						return domain.Project{}, err
					}
				}
			}
		}

		jobIDs := make([]uuid.UUID, 0, len(cancelled))
		for _, work := range cancelled {
			jobIDs = append(jobIDs, work.jobID)
		}
		if len(jobIDs) > 0 {
			runRows, queryErr := tx.Query(ctx, `SELECT id,job_id FROM agent_runs
				WHERE job_id=ANY($1) AND status='running' FOR UPDATE`, jobIDs)
			if queryErr != nil {
				return domain.Project{}, queryErr
			}
			type cancelledRun struct{ id, jobID uuid.UUID }
			runs := []cancelledRun{}
			for runRows.Next() {
				var run cancelledRun
				if err = runRows.Scan(&run.id, &run.jobID); err != nil {
					runRows.Close()
					return domain.Project{}, err
				}
				runs = append(runs, run)
			}
			if err = runRows.Err(); err != nil {
				runRows.Close()
				return domain.Project{}, err
			}
			runRows.Close()
			for _, run := range runs {
				if _, err = transitionLifecycleTx(ctx, tx, lifecycleTransitionRequest{
					LifecycleTransitionParams: LifecycleTransitionParams{
						ResourceType: domain.LifecycleResourceAgentRun,
						ResourceID:   run.id,
						Status:       "cancelled",
						StatusSource: domain.LifecycleSourceAutomation,
						ReasonCode:   domain.LifecycleReasonAutomationDisabled,
						Reason:       automationStopReason,
						RecoveryHint: domain.LifecycleRecoveryNone,
						RelatedJobID: &run.jobID,
						RelatedRunID: &run.id,
					},
					ExpectedStatuses: []string{"running"},
					AllowNonContract: true,
					IgnoreTerminal:   true,
					Fields: []lifecycleFieldUpdate{
						{Column: "termination_reason", Args: []any{automationStopReason}},
						{Column: "failure_category", Args: []any{"cancelled"}},
						{Column: "duration_ms", SQL: "GREATEST(0,FLOOR(EXTRACT(EPOCH FROM now()-started_at)*1000)::bigint)"},
						{Column: "finished_at", SQL: "now()"},
					},
				}); err != nil {
					return domain.Project{}, err
				}
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
	association := FeedbackAssociationParams{}
	if p.Kind == "feedback" {
		if p.Feedback != nil {
			association = *p.Feedback
		}
		if err = validateFeedbackAssociationTx(ctx, tx, p.ProjectID, *p.ParentIntakeID, association); err != nil {
			return domain.Intake{}, nil, err
		}
	} else if p.Feedback != nil {
		return domain.Intake{}, nil, fmt.Errorf("feedback association on requirement: %w", domain.ErrInvalidFeedbackLink)
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
	if p.Kind == "feedback" {
		if _, err = tx.Exec(ctx, `INSERT INTO feedback_links(
			feedback_id,project_id,requirement_id,plan_id,task_id,checkpoint_id,file_id,diff_hunk_id,
			diff_line_side,diff_line_start,diff_line_end)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,nullif($9,''),$10,$11)`,
			id, p.ProjectID, *p.ParentIntakeID, association.PlanID, association.TaskID, association.CheckpointID,
			association.FileID, association.DiffHunkID, strings.TrimSpace(association.DiffLineSide),
			association.DiffLineStart, association.DiffLineEnd); err != nil {
			return domain.Intake{}, nil, err
		}
	}
	_, err = insertEvent(ctx, tx, NewEvent{ProjectID: &p.ProjectID, Type: "intake.created", AggregateType: "intake", AggregateID: id, ResourceVersion: out.Version, Payload: mustJSON(map[string]any{"kind": p.Kind, "status": status})})
	if err != nil {
		return domain.Intake{}, nil, err
	}
	if p.Kind == "requirement" && strings.TrimSpace(p.RequirementSessionID) != "" {
		_, err = tx.Exec(ctx, `INSERT INTO agent_sessions(id,project_id,intake_id,provider,purpose,cli_session_id,context_summary,status)
			VALUES($1,$2,$3,$4,'requirement',$5,$6,'active')`, uuid.New(), p.ProjectID, id, p.RequirementSessionProvider, strings.TrimSpace(p.RequirementSessionID), truncateIntakeSessionSummary(out.Title, out.Body))
		if err != nil {
			return domain.Intake{}, nil, err
		}
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

func truncateIntakeSessionSummary(title, body string) string {
	summary := strings.TrimSpace("需求标题：" + title + "\n\n已确认需求：\n" + body)
	const limit = 12000
	runes := []rune(summary)
	if len(runes) <= limit {
		return summary
	}
	const note = "\n[上下文快照已截断]"
	noteRunes := []rune(note)
	if limit <= len(noteRunes) {
		return string(runes[:limit])
	}
	return string(runes[:limit-len(noteRunes)]) + note
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
			return fmt.Errorf("feedback must be linked to a requirement: %w", domain.ErrInvalidFeedbackLink)
		}
		var projectID uuid.UUID
		var kind string
		err := tx.QueryRow(ctx, `SELECT project_id,kind FROM intakes WHERE id=$1`, *p.ParentIntakeID).Scan(&projectID, &kind)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("feedback parent requirement was not found: %w", domain.ErrNotFound)
		}
		if err != nil {
			return err
		}
		if projectID != p.ProjectID {
			return fmt.Errorf("feedback parent requirement belongs to another project: %w", domain.ErrForbidden)
		}
		if kind != "requirement" {
			return fmt.Errorf("feedback must be linked directly to a requirement: %w", domain.ErrInvalidFeedbackLink)
		}
		return nil
	default:
		return fmt.Errorf("unsupported intake kind %q", p.Kind)
	}
}

func validateFeedbackAssociationTx(ctx context.Context, tx pgx.Tx, projectID, requirementID uuid.UUID, association FeedbackAssociationParams) error {
	invalid := func(message string) error {
		return fmt.Errorf("%s: %w", message, domain.ErrInvalidFeedbackLink)
	}
	if association.PlanID == nil && (association.TaskID != nil || association.CheckpointID != nil || association.FileID != nil || association.DiffHunkID != nil) {
		return invalid("feedback plan is required for the selected execution location")
	}
	if association.TaskID == nil && (association.CheckpointID != nil || association.FileID != nil || association.DiffHunkID != nil) {
		return invalid("feedback task is required for the selected checkpoint")
	}
	if association.CheckpointID == nil && (association.FileID != nil || association.DiffHunkID != nil) {
		return invalid("feedback checkpoint is required for the selected file")
	}
	if association.FileID == nil && association.DiffHunkID != nil {
		return invalid("feedback file is required for the selected diff hunk")
	}
	hasLine := association.DiffLineStart != nil || association.DiffLineEnd != nil || strings.TrimSpace(association.DiffLineSide) != ""
	if hasLine && (association.DiffHunkID == nil || association.DiffLineStart == nil || association.DiffLineEnd == nil) {
		return fmt.Errorf("a complete diff hunk side and line range is required: %w", domain.ErrInvalidDiffRange)
	}

	if association.PlanID != nil {
		var linkedProject, intakeID uuid.UUID
		err := tx.QueryRow(ctx, `SELECT project_id,intake_id FROM plans WHERE id=$1`, *association.PlanID).Scan(&linkedProject, &intakeID)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("feedback plan was not found: %w", domain.ErrNotFound)
		}
		if err != nil {
			return err
		}
		if linkedProject != projectID {
			return fmt.Errorf("feedback plan belongs to another project: %w", domain.ErrForbidden)
		}
		if intakeID != requirementID {
			return invalid("feedback plan does not belong to the parent requirement")
		}
	}
	if association.TaskID != nil {
		var linkedProject, planID uuid.UUID
		err := tx.QueryRow(ctx, `SELECT project_id,plan_id FROM plan_tasks WHERE id=$1`, *association.TaskID).Scan(&linkedProject, &planID)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("feedback task was not found: %w", domain.ErrNotFound)
		}
		if err != nil {
			return err
		}
		if linkedProject != projectID {
			return fmt.Errorf("feedback task belongs to another project: %w", domain.ErrForbidden)
		}
		if association.PlanID == nil || planID != *association.PlanID {
			return invalid("feedback task does not belong to the selected plan")
		}
	}
	if association.CheckpointID != nil {
		var linkedProject, planID uuid.UUID
		var taskID *uuid.UUID
		var kind string
		err := tx.QueryRow(ctx, `SELECT project_id,plan_id,task_id,snapshot_kind FROM plan_execution_snapshots WHERE id=$1`, *association.CheckpointID).Scan(&linkedProject, &planID, &taskID, &kind)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("feedback checkpoint was not found: %w", domain.ErrNotFound)
		}
		if err != nil {
			return err
		}
		if linkedProject != projectID {
			return fmt.Errorf("feedback checkpoint belongs to another project: %w", domain.ErrForbidden)
		}
		if association.PlanID == nil || association.TaskID == nil || planID != *association.PlanID || taskID == nil || *taskID != *association.TaskID || kind != domain.PlanSnapshotKindTaskCheckpoint {
			return invalid("feedback checkpoint does not belong to the selected plan task")
		}
	}
	if association.FileID != nil {
		var snapshotID, linkedProject uuid.UUID
		err := tx.QueryRow(ctx, `SELECT files.snapshot_id,snapshots.project_id
			FROM plan_execution_snapshot_files files
			JOIN plan_execution_snapshots snapshots ON snapshots.id=files.snapshot_id
			WHERE files.id=$1`, *association.FileID).Scan(&snapshotID, &linkedProject)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("feedback changed file was not found: %w", domain.ErrNotFound)
		}
		if err != nil {
			return err
		}
		if linkedProject != projectID {
			return fmt.Errorf("feedback changed file belongs to another project: %w", domain.ErrForbidden)
		}
		if association.CheckpointID == nil || snapshotID != *association.CheckpointID {
			return invalid("feedback changed file does not belong to the selected checkpoint")
		}
	}
	if association.DiffHunkID != nil {
		var fileID, linkedProject uuid.UUID
		var oldStart, oldCount, newStart, newCount int
		err := tx.QueryRow(ctx, `SELECT hunks.file_id,snapshots.project_id,hunks.old_start_line,hunks.old_line_count,hunks.new_start_line,hunks.new_line_count
			FROM plan_execution_snapshot_diff_hunks hunks
			JOIN plan_execution_snapshot_files files ON files.id=hunks.file_id
			JOIN plan_execution_snapshots snapshots ON snapshots.id=files.snapshot_id
			WHERE hunks.id=$1`, *association.DiffHunkID).Scan(&fileID, &linkedProject, &oldStart, &oldCount, &newStart, &newCount)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("feedback diff hunk was not found: %w", domain.ErrNotFound)
		}
		if err != nil {
			return err
		}
		if linkedProject != projectID {
			return fmt.Errorf("feedback diff hunk belongs to another project: %w", domain.ErrForbidden)
		}
		if association.FileID == nil || fileID != *association.FileID {
			return invalid("feedback diff hunk does not belong to the selected file")
		}
		if hasLine {
			start, end := *association.DiffLineStart, *association.DiffLineEnd
			side := strings.TrimSpace(association.DiffLineSide)
			valid := start > 0 && end >= start
			switch side {
			case "old":
				valid = valid && oldCount > 0 && start >= oldStart && end < oldStart+oldCount
			case "new":
				valid = valid && newCount > 0 && start >= newStart && end < newStart+newCount
			default:
				valid = false
			}
			if !valid {
				return fmt.Errorf("feedback diff line range is outside the selected hunk: %w", domain.ErrInvalidDiffRange)
			}
		}
	}
	return nil
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
func scanFeedbackLink(row pgx.Row) (domain.FeedbackLink, error) {
	var link domain.FeedbackLink
	err := row.Scan(&link.FeedbackID, &link.ProjectID, &link.RequirementID, &link.PlanID, &link.TaskID,
		&link.CheckpointID, &link.FileID, &link.DiffHunkID, &link.DiffLineSide,
		&link.DiffLineStart, &link.DiffLineEnd, &link.CreatedAt)
	return link, err
}

const feedbackLinkColumns = `feedback_id,project_id,requirement_id,plan_id,task_id,checkpoint_id,file_id,diff_hunk_id,
	coalesce(diff_line_side,''),diff_line_start,diff_line_end,created_at`

func (s *Store) GetFeedbackTrace(ctx context.Context, projectID, feedbackID uuid.UUID) (domain.FeedbackTrace, error) {
	trace := domain.FeedbackTrace{Revisions: []domain.FeedbackRevision{}}
	link, err := scanFeedbackLink(s.Pool.QueryRow(ctx, `SELECT `+feedbackLinkColumns+` FROM feedback_links WHERE feedback_id=$1`, feedbackID))
	if errors.Is(err, pgx.ErrNoRows) {
		var actualProject uuid.UUID
		lookupErr := s.Pool.QueryRow(ctx, `SELECT project_id FROM intakes WHERE id=$1 AND kind='feedback'`, feedbackID).Scan(&actualProject)
		if errors.Is(lookupErr, pgx.ErrNoRows) {
			return trace, domain.ErrNotFound
		}
		if lookupErr != nil {
			return trace, lookupErr
		}
		if actualProject != projectID {
			return trace, domain.ErrForbidden
		}
		return trace, domain.ErrNotFound
	}
	if err != nil {
		return trace, err
	}
	if link.ProjectID != projectID {
		return trace, domain.ErrForbidden
	}
	trace.Link = link
	if trace.Feedback, err = s.GetIntake(ctx, link.FeedbackID); err != nil {
		return domain.FeedbackTrace{}, err
	}
	if trace.Requirement, err = s.GetIntake(ctx, link.RequirementID); err != nil {
		return domain.FeedbackTrace{}, err
	}
	if link.PlanID != nil {
		plan, planErr := s.GetPlan(ctx, *link.PlanID)
		if planErr != nil {
			return domain.FeedbackTrace{}, planErr
		}
		trace.Plan = &plan
	}
	if link.TaskID != nil {
		task, taskErr := s.GetTask(ctx, *link.TaskID)
		if taskErr != nil {
			return domain.FeedbackTrace{}, taskErr
		}
		trace.Task = &task
	}
	if link.CheckpointID != nil {
		checkpoint, checkpointErr := s.GetPlanExecutionSnapshot(ctx, *link.CheckpointID)
		if checkpointErr != nil {
			return domain.FeedbackTrace{}, checkpointErr
		}
		trace.Checkpoint = &checkpoint
		if link.FileID != nil {
			for fileIndex := range checkpoint.Files {
				if checkpoint.Files[fileIndex].ID != *link.FileID {
					continue
				}
				file := checkpoint.Files[fileIndex]
				trace.File = &file
				if link.DiffHunkID != nil {
					for hunkIndex := range file.Hunks {
						if file.Hunks[hunkIndex].ID == *link.DiffHunkID {
							hunk := file.Hunks[hunkIndex]
							trace.DiffHunk = &hunk
							break
						}
					}
				}
				break
			}
		}
	}
	trace.Revisions, err = s.listFeedbackRevisions(ctx, link)
	if err != nil {
		return domain.FeedbackTrace{}, err
	}
	return trace, nil
}

func (s *Store) listFeedbackRevisions(ctx context.Context, link domain.FeedbackLink) ([]domain.FeedbackRevision, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id,revision_intake_id,created_at FROM feedback_revisions WHERE feedback_id=$1 ORDER BY created_at,id`, link.FeedbackID)
	if err != nil {
		return nil, err
	}
	type revisionRow struct {
		id       uuid.UUID
		intakeID uuid.UUID
		created  time.Time
	}
	rowValues := []revisionRow{}
	for rows.Next() {
		var value revisionRow
		if err = rows.Scan(&value.id, &value.intakeID, &value.created); err != nil {
			rows.Close()
			return nil, err
		}
		rowValues = append(rowValues, value)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	out := make([]domain.FeedbackRevision, 0, len(rowValues))
	for _, value := range rowValues {
		revision := domain.FeedbackRevision{ID: value.id, FeedbackID: link.FeedbackID, ProjectID: link.ProjectID, RequirementID: link.RequirementID, CreatedAt: value.created}
		revision.RevisionIntake, err = s.GetIntake(ctx, value.intakeID)
		if err != nil {
			return nil, err
		}
		var planID uuid.UUID
		planErr := s.Pool.QueryRow(ctx, `SELECT revision_plan_id FROM feedback_revision_plans WHERE feedback_revision_id=$1 ORDER BY created_at DESC,id DESC LIMIT 1`, value.id).Scan(&planID)
		if planErr == nil {
			plan, getErr := s.GetPlan(ctx, planID)
			if getErr != nil {
				return nil, getErr
			}
			revision.RevisionPlan = &plan
		} else if !errors.Is(planErr, pgx.ErrNoRows) {
			return nil, planErr
		}
		out = append(out, revision)
	}
	return out, nil
}

func (s *Store) RecordFeedbackRevision(ctx context.Context, params RecordFeedbackRevisionParams) (domain.FeedbackRevision, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return domain.FeedbackRevision{}, err
	}
	defer tx.Rollback(ctx)
	var linkProject, requirementID uuid.UUID
	err = tx.QueryRow(ctx, `SELECT project_id,requirement_id FROM feedback_links WHERE feedback_id=$1`, params.FeedbackID).Scan(&linkProject, &requirementID)
	if errors.Is(err, pgx.ErrNoRows) {
		var feedbackProject uuid.UUID
		lookupErr := tx.QueryRow(ctx, `SELECT project_id FROM intakes WHERE id=$1 AND kind='feedback'`, params.FeedbackID).Scan(&feedbackProject)
		if errors.Is(lookupErr, pgx.ErrNoRows) {
			return domain.FeedbackRevision{}, domain.ErrNotFound
		}
		if lookupErr != nil {
			return domain.FeedbackRevision{}, lookupErr
		}
		if feedbackProject != params.ProjectID {
			return domain.FeedbackRevision{}, domain.ErrForbidden
		}
		return domain.FeedbackRevision{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.FeedbackRevision{}, err
	}
	if linkProject != params.ProjectID {
		return domain.FeedbackRevision{}, domain.ErrForbidden
	}
	var revisionProject uuid.UUID
	var revisionKind string
	err = tx.QueryRow(ctx, `SELECT project_id,kind FROM intakes WHERE id=$1`, params.RevisionIntakeID).Scan(&revisionProject, &revisionKind)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.FeedbackRevision{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.FeedbackRevision{}, err
	}
	if revisionProject != params.ProjectID {
		return domain.FeedbackRevision{}, domain.ErrForbidden
	}
	if revisionKind != "requirement" || params.RevisionIntakeID == requirementID {
		return domain.FeedbackRevision{}, domain.ErrInvalidFeedbackLink
	}
	if params.RevisionPlanID != nil {
		var planProject, planIntake uuid.UUID
		err = tx.QueryRow(ctx, `SELECT project_id,intake_id FROM plans WHERE id=$1`, *params.RevisionPlanID).Scan(&planProject, &planIntake)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.FeedbackRevision{}, domain.ErrNotFound
		}
		if err != nil {
			return domain.FeedbackRevision{}, err
		}
		if planProject != params.ProjectID {
			return domain.FeedbackRevision{}, domain.ErrForbidden
		}
		if planIntake != params.RevisionIntakeID {
			return domain.FeedbackRevision{}, domain.ErrInvalidFeedbackLink
		}
	}

	revision := domain.FeedbackRevision{FeedbackID: params.FeedbackID, ProjectID: params.ProjectID, RequirementID: requirementID}
	err = tx.QueryRow(ctx, `INSERT INTO feedback_revisions(id,feedback_id,project_id,requirement_id,revision_intake_id)
		VALUES($1,$2,$3,$4,$5) ON CONFLICT(feedback_id,revision_intake_id) DO NOTHING
		RETURNING id,created_at`, uuid.New(), params.FeedbackID, params.ProjectID, requirementID, params.RevisionIntakeID).Scan(&revision.ID, &revision.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		err = tx.QueryRow(ctx, `SELECT id,created_at FROM feedback_revisions WHERE feedback_id=$1 AND revision_intake_id=$2`, params.FeedbackID, params.RevisionIntakeID).Scan(&revision.ID, &revision.CreatedAt)
	}
	if err != nil {
		return domain.FeedbackRevision{}, err
	}
	if params.RevisionPlanID != nil {
		if _, err = tx.Exec(ctx, `INSERT INTO feedback_revision_plans(id,feedback_revision_id,project_id,revision_plan_id)
			VALUES($1,$2,$3,$4) ON CONFLICT(feedback_revision_id,revision_plan_id) DO NOTHING`, uuid.New(), revision.ID, params.ProjectID, *params.RevisionPlanID); err != nil {
			return domain.FeedbackRevision{}, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return domain.FeedbackRevision{}, err
	}
	revision.RevisionIntake, err = s.GetIntake(ctx, params.RevisionIntakeID)
	if err != nil {
		return domain.FeedbackRevision{}, err
	}
	if params.RevisionPlanID != nil {
		plan, planErr := s.GetPlan(ctx, *params.RevisionPlanID)
		if planErr != nil {
			return domain.FeedbackRevision{}, planErr
		}
		revision.RevisionPlan = &plan
	}
	return revision, nil
}

func validateFeedbackQueryObject(ctx context.Context, q snapshotQueryer, table string, id, projectID uuid.UUID) error {
	var actualProject uuid.UUID
	err := q.QueryRow(ctx, fmt.Sprintf(`SELECT project_id FROM %s WHERE id=$1`, table), id).Scan(&actualProject)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ErrNotFound
	}
	if err != nil {
		return err
	}
	if actualProject != projectID {
		return domain.ErrForbidden
	}
	return nil
}

func (s *Store) ListFeedbackTraces(ctx context.Context, projectID uuid.UUID, query FeedbackQuery) ([]domain.FeedbackTrace, error) {
	conditions := []string{"project_id=$1"}
	args := []any{projectID}
	appendFilter := func(column string, value *uuid.UUID) {
		if value == nil {
			return
		}
		args = append(args, *value)
		conditions = append(conditions, fmt.Sprintf("%s=$%d", column, len(args)))
	}
	if query.RequirementID != nil {
		if err := validateFeedbackQueryObject(ctx, s.Pool, "intakes", *query.RequirementID, projectID); err != nil {
			return nil, err
		}
	}
	if query.PlanID != nil {
		if err := validateFeedbackQueryObject(ctx, s.Pool, "plans", *query.PlanID, projectID); err != nil {
			return nil, err
		}
	}
	if query.TaskID != nil {
		if err := validateFeedbackQueryObject(ctx, s.Pool, "plan_tasks", *query.TaskID, projectID); err != nil {
			return nil, err
		}
	}
	if query.CheckpointID != nil {
		if err := validateFeedbackQueryObject(ctx, s.Pool, "plan_execution_snapshots", *query.CheckpointID, projectID); err != nil {
			return nil, err
		}
	}
	appendFilter("requirement_id", query.RequirementID)
	appendFilter("plan_id", query.PlanID)
	appendFilter("task_id", query.TaskID)
	appendFilter("checkpoint_id", query.CheckpointID)
	rows, err := s.Pool.Query(ctx, `SELECT feedback_id FROM feedback_links WHERE `+strings.Join(conditions, " AND ")+` ORDER BY created_at DESC,feedback_id`, args...)
	if err != nil {
		return nil, err
	}
	feedbackIDs := []uuid.UUID{}
	for rows.Next() {
		var feedbackID uuid.UUID
		if err = rows.Scan(&feedbackID); err != nil {
			rows.Close()
			return nil, err
		}
		feedbackIDs = append(feedbackIDs, feedbackID)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	out := make([]domain.FeedbackTrace, 0, len(feedbackIDs))
	for _, feedbackID := range feedbackIDs {
		trace, traceErr := s.GetFeedbackTrace(ctx, projectID, feedbackID)
		if traceErr != nil {
			return nil, traceErr
		}
		out = append(out, trace)
	}
	return out, nil
}

func (s *Store) ListFeedbackForRequirement(ctx context.Context, projectID, requirementID uuid.UUID) ([]domain.FeedbackTrace, error) {
	return s.ListFeedbackTraces(ctx, projectID, FeedbackQuery{RequirementID: &requirementID})
}

func (s *Store) ListFeedbackForPlan(ctx context.Context, projectID, planID uuid.UUID) ([]domain.FeedbackTrace, error) {
	return s.ListFeedbackTraces(ctx, projectID, FeedbackQuery{PlanID: &planID})
}

func (s *Store) ListFeedbackForTask(ctx context.Context, projectID, taskID uuid.UUID) ([]domain.FeedbackTrace, error) {
	return s.ListFeedbackTraces(ctx, projectID, FeedbackQuery{TaskID: &taskID})
}

func (s *Store) ListFeedbackForCheckpoint(ctx context.Context, projectID, checkpointID uuid.UUID) ([]domain.FeedbackTrace, error) {
	return s.ListFeedbackTraces(ctx, projectID, FeedbackQuery{CheckpointID: &checkpointID})
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

const planRecordColumns = `id,project_id,intake_id,title,spec,spec_version,compatibility_mode,is_executable,
	validation_problems,markdown,status,delivery_status,acceptance_summary,config_snapshot,
	created_at,updated_at,version,content_version`

// scanPlan retains the pre-graph projection for older transactional call sites
// that intentionally select the historical column set.
func scanPlan(row pgx.Row) (domain.Plan, error) {
	var p domain.Plan
	err := row.Scan(&p.ID, &p.ProjectID, &p.IntakeID, &p.Title, &p.Spec, &p.Markdown, &p.Status, &p.ConfigSnapshot, &p.CreatedAt, &p.UpdatedAt, &p.Version)
	return p, err
}

func scanPlanRecord(row pgx.Row) (domain.Plan, error) {
	var p domain.Plan
	err := row.Scan(
		&p.ID, &p.ProjectID, &p.IntakeID, &p.Title, &p.Spec, &p.SpecVersion,
		&p.CompatibilityMode, &p.Executable, &p.ValidationProblems, &p.Markdown,
		&p.Status, &p.DeliveryStatus, &p.AcceptanceSummary, &p.ConfigSnapshot,
		&p.CreatedAt, &p.UpdatedAt, &p.Version, &p.ContentVersion,
	)
	return p, err
}

func (s *Store) GetPlan(ctx context.Context, id uuid.UUID) (domain.Plan, error) {
	p, err := scanPlanRecord(s.Pool.QueryRow(ctx, `SELECT `+planRecordColumns+` FROM plans WHERE id=$1`, id))
	if err != nil {
		return p, mapNotFound(err)
	}
	if err = s.decoratePlanExecutionState(ctx, &p); err != nil {
		return domain.Plan{}, err
	}
	return p, nil
}

func (s *Store) ListPlansForIntake(ctx context.Context, intakeID uuid.UUID) ([]domain.Plan, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+planRecordColumns+` FROM plans WHERE intake_id=$1 ORDER BY created_at DESC`, intakeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.Plan{}
	for rows.Next() {
		p, err := scanPlanRecord(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	for i := range out {
		if err = s.decoratePlanExecutionState(ctx, &out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *Store) ListPlans(ctx context.Context, projectID uuid.UUID) ([]domain.Plan, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+planRecordColumns+` FROM plans WHERE project_id=$1 ORDER BY created_at DESC`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.Plan{}
	for rows.Next() {
		p, err := scanPlanRecord(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	for i := range out {
		if err = s.decoratePlanExecutionState(ctx, &out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

const planTaskRecordColumns = `id,project_id,plan_id,task_key,task_type,position,execution_order,
	dependency_keys,title,scope,inputs,outputs,risks,validation_commands,acceptance,
	acceptance_definition,acceptance_status,acceptance_result,status,coalesce(session_id,''),
	started_at,finished_at,created_at,updated_at,version`

// scanTask retains the historical projection for call sites outside the graph
// repository surface. New reads use scanTaskRecord.
func scanTask(row pgx.Row) (domain.PlanTask, error) {
	var t domain.PlanTask
	err := row.Scan(&t.ID, &t.ProjectID, &t.PlanID, &t.TaskKey, &t.Position, &t.Title, &t.Scope, &t.Acceptance, &t.Status, &t.SessionID, &t.StartedAt, &t.FinishedAt, &t.CreatedAt, &t.UpdatedAt, &t.Version)
	return t, err
}

func scanTaskRecord(row pgx.Row) (domain.PlanTask, error) {
	var t domain.PlanTask
	err := row.Scan(
		&t.ID, &t.ProjectID, &t.PlanID, &t.TaskKey, &t.TaskType, &t.Position,
		&t.ExecutionOrder, &t.DependencyKeys, &t.Title, &t.Scope, &t.Inputs,
		&t.Outputs, &t.Risks, &t.ValidationCommands, &t.Acceptance,
		&t.AcceptanceDefinition, &t.AcceptanceStatus, &t.AcceptanceResult, &t.Status,
		&t.SessionID, &t.StartedAt, &t.FinishedAt, &t.CreatedAt, &t.UpdatedAt, &t.Version,
	)
	return t, err
}

func (s *Store) ListTasks(ctx context.Context, planID uuid.UUID) ([]domain.PlanTask, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+planTaskRecordColumns+` FROM plan_tasks WHERE plan_id=$1 ORDER BY execution_order`, planID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.PlanTask{}
	for rows.Next() {
		t, err := scanTaskRecord(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) GetTask(ctx context.Context, id uuid.UUID) (domain.PlanTask, error) {
	t, err := scanTaskRecord(s.Pool.QueryRow(ctx, `SELECT `+planTaskRecordColumns+` FROM plan_tasks WHERE id=$1`, id))
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

type snapshotQueryer interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

type planSnapshotState struct {
	ProjectID               uuid.UUID
	PlanID                  uuid.UUID
	IntakeID                uuid.UUID
	PlanResourceVersion     int64
	PlanContentVersion      int64
	PlanSpec                json.RawMessage
	PlanConfigSnapshot      json.RawMessage
	RequirementVersion      int64
	RequirementKind         string
	RequirementParentID     *uuid.UUID
	RequirementTitle        string
	RequirementBody         string
	ProjectVersion          int64
	WorkspacePathNormalized string
	ConfigVersion           int64
	ValidationCommand       string
	AgentProvider           string
	CodexCommand            string
	CodexArgs               json.RawMessage
	ClaudeCommand           string
	ClaudeArgs              json.RawMessage
	PlanTimeoutSeconds      int
	TaskTimeoutSeconds      int
	MaxRetries              int
	AllowedEnv              json.RawMessage
	GenerationProvider      string
	ExecutionProvider       string
	KeyExecutionFields      json.RawMessage
	RequirementDigest       string
	PlanSpecDigest          string
	GitRoot                 string
	GitRepositoryIdentity   string
	GitBranch               string
	GitHead                 string
	GitWorkspaceDigest      string
}

func digestBytes(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

func canonicalJSONDigest(raw json.RawMessage) string {
	var value any
	if len(raw) > 0 && json.Unmarshal(raw, &value) == nil {
		if normalized, err := json.Marshal(value); err == nil {
			return digestBytes(normalized)
		}
	}
	return digestBytes(raw)
}

func gitCommand(ctx context.Context, workspace string, args ...string) string {
	if strings.TrimSpace(workspace) == "" {
		return ""
	}
	commandArgs := append([]string{"-C", workspace}, args...)
	commandCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	output, err := exec.CommandContext(commandCtx, "git", commandArgs...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}

func readGitSnapshot(ctx context.Context, workspace string) (root, identity, branch, head, workspaceDigest string) {
	root = gitCommand(ctx, workspace, "rev-parse", "--show-toplevel")
	if root == "" {
		return "", "", "", "", digestBytes(nil)
	}
	identity = gitCommand(ctx, workspace, "config", "--get", "remote.origin.url")
	if identity == "" {
		identity = root
	}
	branch = gitCommand(ctx, workspace, "symbolic-ref", "--quiet", "--short", "HEAD")
	head = gitCommand(ctx, workspace, "rev-parse", "HEAD")
	status := gitCommand(ctx, workspace, "status", "--porcelain=v1", "--untracked-files=all")
	workspaceDigest = digestBytes([]byte(status))
	return
}

const (
	maxTaskCheckpointFiles       = 500
	maxTaskCheckpointPatchBytes  = 512 * 1024
	maxTaskCheckpointPatchLines  = 12000
	maxTaskCheckpointFileBytes   = int64(4 * 1024 * 1024)
	maxTaskCheckpointHunks       = 512
	maxTaskCheckpointCommandText = 16 * 1024
	maxTaskCheckpointPathBytes   = 4 * 1024 * 1024
)

const oversizedCheckpointBlobPrefix = "SPECRELAY-OMITTED-V1\n"

type repositoryGitCheckpoint struct {
	Available          bool
	Reason             string
	WorkspaceRoot      string
	RepositoryIdentity string
	Head               string
	WorktreeTree       string
	ProtectionRef      string
	FileCount          int
	OversizedCount     int
	SpecialFileCount   int
}

type checkpointIndexEntry struct {
	Mode  string
	OID   string
	Stage int
}

type oversizedCheckpointBlob struct {
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
	Lines  int64  `json:"lines"`
	Binary bool   `json:"binary"`
}

type checkpointNameStatus struct {
	Status       string
	Path         string
	PreviousPath string
}

func captureRepositoryGitCheckpoint(ctx context.Context, workspace string, snapshotID uuid.UUID) repositoryGitCheckpoint {
	result := repositoryGitCheckpoint{Reason: "not_git_workspace"}
	identity, err := security.InspectExistingPath(workspace)
	if err != nil {
		result.Reason = "workspace_unavailable"
		return result
	}
	info, err := os.Stat(identity.Real)
	if err != nil || !info.IsDir() {
		result.Reason = "workspace_unavailable"
		return result
	}
	inside, err := repositoryGitOutput(ctx, identity.Real, nil, "rev-parse", "--is-inside-work-tree")
	if err != nil || strings.TrimSpace(string(inside)) != "true" {
		return result
	}
	top, err := repositoryGitOutput(ctx, identity.Real, nil, "rev-parse", "--show-toplevel")
	if err != nil {
		result.Reason = "git_unreadable"
		return result
	}
	topIdentity, err := security.InspectExistingPath(strings.TrimSpace(string(top)))
	if err != nil || topIdentity.Real != identity.Real {
		result.Reason = "git_root_mismatch"
		return result
	}
	result.WorkspaceRoot = identity.Real
	origin, originErr := repositoryGitOutput(ctx, identity.Real, nil, "config", "--get", "remote.origin.url")
	if originErr == nil && strings.TrimSpace(string(origin)) != "" {
		result.RepositoryIdentity = strings.TrimSpace(string(origin))
	} else {
		common, commonErr := repositoryGitOutput(ctx, identity.Real, nil, "rev-parse", "--git-common-dir")
		if commonErr != nil {
			result.Reason = "git_unreadable"
			return result
		}
		commonPath := strings.TrimSpace(string(common))
		if !filepath.IsAbs(commonPath) {
			commonPath = filepath.Join(identity.Real, commonPath)
		}
		commonIdentity, commonErr := security.InspectExistingPath(commonPath)
		if commonErr != nil {
			result.Reason = "git_unreadable"
			return result
		}
		result.RepositoryIdentity = commonIdentity.Real
	}
	head, _ := repositoryGitOutput(ctx, identity.Real, nil, "rev-parse", "--verify", "HEAD")
	result.Head = strings.TrimSpace(string(head))

	indexOutput, err := repositoryGitOutput(ctx, identity.Real, nil, "ls-files", "--stage", "-z")
	if err != nil {
		result.Reason = "git_index_unreadable"
		return result
	}
	indexEntries, conflicted, err := parseCheckpointIndex(indexOutput)
	if err != nil {
		result.Reason = "git_index_unreadable"
		return result
	}
	if conflicted {
		result.Reason = "git_index_conflicted"
		return result
	}
	candidateOutput, err := repositoryGitOutput(ctx, identity.Real, nil, "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	if err != nil {
		result.Reason = "git_workspace_unreadable"
		return result
	}
	candidateSet := map[string]struct{}{}
	for _, raw := range bytes.Split(candidateOutput, []byte{0}) {
		if len(raw) == 0 {
			continue
		}
		candidateSet[string(raw)] = struct{}{}
	}
	candidates := make([]string, 0, len(candidateSet))
	for candidate := range candidateSet {
		candidates = append(candidates, candidate)
	}
	sort.Strings(candidates)

	temporaryIndex, err := os.CreateTemp("", "specrelay-execution-index-*")
	if err != nil {
		result.Reason = "checkpoint_unavailable"
		return result
	}
	temporaryIndexPath := temporaryIndex.Name()
	_ = temporaryIndex.Close()
	_ = os.Remove(temporaryIndexPath)
	defer os.Remove(temporaryIndexPath)
	if _, err = repositoryGitOutputWithEnv(ctx, identity.Real, nil, []string{"GIT_INDEX_FILE=" + temporaryIndexPath}, "read-tree", "--empty"); err != nil {
		result.Reason = "checkpoint_unavailable"
		return result
	}

	var indexInfo bytes.Buffer
	for _, candidate := range candidates {
		normalized, normalizeErr := security.NormalizeWorkspaceRelativePath(identity.Real, candidate)
		if normalizeErr != nil || normalized != candidate {
			result.Reason = "unsafe_git_path"
			return result
		}
		absolute, resolveErr := security.ResolveRelativePath(identity.Real, normalized)
		if errors.Is(resolveErr, os.ErrNotExist) {
			continue
		}
		if resolveErr != nil {
			result.Reason = "unsafe_git_path"
			return result
		}
		fileInfo, statErr := os.Lstat(absolute)
		if errors.Is(statErr, os.ErrNotExist) {
			continue
		}
		if statErr != nil {
			result.Reason = "git_workspace_unreadable"
			return result
		}
		entry := indexEntries[normalized]
		if fileInfo.IsDir() {
			if entry.Mode == "160000" && isRepositoryGitObjectID(entry.OID) {
				writeCheckpointIndexInfo(&indexInfo, entry.Mode, entry.OID, normalized)
				result.FileCount++
			}
			continue
		}
		mode := "100644"
		var oid string
		switch {
		case fileInfo.Mode()&os.ModeSymlink != 0:
			target, readErr := os.Readlink(absolute)
			if readErr != nil {
				result.Reason = "git_workspace_unreadable"
				return result
			}
			oid, err = hashCheckpointContent(ctx, identity.Real, strings.NewReader(target))
			mode = "120000"
		case fileInfo.Mode().IsRegular():
			if fileInfo.Mode().Perm()&0o111 != 0 {
				mode = "100755"
			}
			if fileInfo.Size() > maxTaskCheckpointFileBytes {
				metadata, metadataErr := inspectOversizedCheckpointFile(absolute)
				if metadataErr != nil {
					result.Reason = "git_workspace_unreadable"
					return result
				}
				encoded, encodeErr := json.Marshal(metadata)
				if encodeErr != nil {
					result.Reason = "checkpoint_unavailable"
					return result
				}
				oid, err = hashCheckpointContent(ctx, identity.Real, strings.NewReader(oversizedCheckpointBlobPrefix+string(encoded)+"\n"))
				result.OversizedCount++
			} else {
				file, openErr := os.Open(absolute)
				if openErr != nil {
					result.Reason = "git_workspace_unreadable"
					return result
				}
				oid, err = hashCheckpointContent(ctx, identity.Real, file)
				_ = file.Close()
			}
		default:
			result.SpecialFileCount++
			continue
		}
		if err != nil || !isRepositoryGitObjectID(oid) {
			result.Reason = "checkpoint_unavailable"
			return result
		}
		writeCheckpointIndexInfo(&indexInfo, mode, oid, normalized)
		result.FileCount++
	}
	if indexInfo.Len() > 0 {
		if _, err = repositoryGitOutputWithEnv(ctx, identity.Real, bytes.NewReader(indexInfo.Bytes()), []string{"GIT_INDEX_FILE=" + temporaryIndexPath}, "update-index", "-z", "--index-info"); err != nil {
			result.Reason = "checkpoint_unavailable"
			return result
		}
	}
	treeOutput, err := repositoryGitOutputWithEnv(ctx, identity.Real, nil, []string{"GIT_INDEX_FILE=" + temporaryIndexPath}, "write-tree")
	if err != nil {
		result.Reason = "checkpoint_unavailable"
		return result
	}
	result.WorktreeTree = strings.TrimSpace(string(treeOutput))
	if !isRepositoryGitObjectID(result.WorktreeTree) {
		result.Reason = "checkpoint_unavailable"
		return result
	}
	result.ProtectionRef = "refs/specrelay/execution-snapshots/" + snapshotID.String() + "/worktree"
	if _, err = repositoryGitOutput(ctx, identity.Real, nil, "update-ref", result.ProtectionRef, result.WorktreeTree); err != nil {
		result.Reason = "checkpoint_unavailable"
		return result
	}
	result.Available = true
	result.Reason = ""
	return result
}

func parseCheckpointIndex(raw []byte) (map[string]checkpointIndexEntry, bool, error) {
	entries := map[string]checkpointIndexEntry{}
	conflicted := false
	for _, record := range bytes.Split(raw, []byte{0}) {
		if len(record) == 0 {
			continue
		}
		tab := bytes.IndexByte(record, '\t')
		if tab <= 0 || tab == len(record)-1 {
			return nil, false, errors.New("invalid Git index record")
		}
		fields := strings.Fields(string(record[:tab]))
		if len(fields) != 3 {
			return nil, false, errors.New("invalid Git index metadata")
		}
		stage, err := strconv.Atoi(fields[2])
		if err != nil {
			return nil, false, err
		}
		candidate := string(record[tab+1:])
		if err = security.ValidateRelativePath(candidate); err != nil {
			return nil, false, err
		}
		if stage != 0 {
			conflicted = true
			continue
		}
		entries[candidate] = checkpointIndexEntry{Mode: fields[0], OID: fields[1], Stage: stage}
	}
	return entries, conflicted, nil
}

func writeCheckpointIndexInfo(builder *bytes.Buffer, mode, oid, relative string) {
	builder.WriteString(mode)
	builder.WriteByte(' ')
	builder.WriteString(oid)
	builder.WriteByte('\t')
	builder.WriteString(relative)
	builder.WriteByte(0)
}

func hashCheckpointContent(ctx context.Context, workspace string, input io.Reader) (string, error) {
	output, err := repositoryGitOutput(ctx, workspace, input, "hash-object", "-w", "--no-filters", "--stdin")
	return strings.TrimSpace(string(output)), err
}

func inspectOversizedCheckpointFile(absolute string) (oversizedCheckpointBlob, error) {
	file, err := os.Open(absolute)
	if err != nil {
		return oversizedCheckpointBlob{}, err
	}
	defer file.Close()
	hash := sha256.New()
	buffer := make([]byte, 64*1024)
	metadata := oversizedCheckpointBlob{}
	for {
		count, readErr := file.Read(buffer)
		if count > 0 {
			chunk := buffer[:count]
			metadata.Bytes += int64(count)
			metadata.Lines += int64(bytes.Count(chunk, []byte{'\n'}))
			if !metadata.Binary && bytes.IndexByte(chunk, 0) >= 0 {
				metadata.Binary = true
			}
			_, _ = hash.Write(chunk)
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return oversizedCheckpointBlob{}, readErr
		}
	}
	metadata.SHA256 = hex.EncodeToString(hash.Sum(nil))
	return metadata, nil
}

func repositoryGitOutput(ctx context.Context, workspace string, input io.Reader, args ...string) ([]byte, error) {
	return repositoryGitOutputWithEnv(ctx, workspace, input, nil, args...)
}

func repositoryGitOutputWithEnv(ctx context.Context, workspace string, input io.Reader, extraEnv []string, args ...string) ([]byte, error) {
	commandContext, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	commandArgs := []string{"--literal-pathspecs", "-c", "core.quotePath=false", "-c", "diff.external=", "-c", "core.hooksPath=/dev/null", "-C", workspace}
	commandArgs = append(commandArgs, args...)
	command := exec.CommandContext(commandContext, "git", commandArgs...)
	command.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "GIT_EXTERNAL_DIFF=", "LC_ALL=C")
	command.Env = append(command.Env, extraEnv...)
	command.Stdin = input
	var stdout bytes.Buffer
	var stderr checkpointBoundedWriter
	stderr.maxBytes = maxTaskCheckpointCommandText
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		if commandContext.Err() != nil {
			return nil, commandContext.Err()
		}
		message := strings.TrimSpace(string(stderr.bytes))
		if message == "" {
			message = err.Error()
		}
		return nil, errors.New(message)
	}
	return stdout.Bytes(), nil
}

func repositoryGitOutputLimited(ctx context.Context, workspace string, maxBytes int, maxLines int, args ...string) ([]byte, bool, error) {
	commandContext, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	commandArgs := []string{"--literal-pathspecs", "-c", "core.quotePath=false", "-c", "diff.external=", "-c", "core.hooksPath=/dev/null", "-C", workspace}
	commandArgs = append(commandArgs, args...)
	command := exec.CommandContext(commandContext, "git", commandArgs...)
	command.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "GIT_EXTERNAL_DIFF=", "LC_ALL=C")
	writer := checkpointBoundedWriter{maxBytes: maxBytes, maxLines: maxLines}
	var stderr checkpointBoundedWriter
	stderr.maxBytes = maxTaskCheckpointCommandText
	command.Stdout = &writer
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		if commandContext.Err() != nil {
			return writer.bytes, writer.truncated, commandContext.Err()
		}
		message := strings.TrimSpace(string(stderr.bytes))
		if message == "" {
			message = err.Error()
		}
		return writer.bytes, writer.truncated, errors.New(message)
	}
	return writer.bytes, writer.truncated, nil
}

type checkpointBoundedWriter struct {
	bytes     []byte
	maxBytes  int
	maxLines  int
	lines     int
	truncated bool
}

func (writer *checkpointBoundedWriter) Write(value []byte) (int, error) {
	original := len(value)
	for _, character := range value {
		if (writer.maxBytes > 0 && len(writer.bytes) >= writer.maxBytes) || (writer.maxLines > 0 && writer.lines >= writer.maxLines) {
			writer.truncated = true
			continue
		}
		writer.bytes = append(writer.bytes, character)
		if character == '\n' {
			writer.lines++
		}
	}
	return original, nil
}

func isRepositoryGitObjectID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func repositoryCheckpointMetadata(capture repositoryGitCheckpoint) map[string]any {
	metadata := map[string]any{
		"version": 1, "available": capture.Available,
		"fileCount": capture.FileCount, "oversizedFileCount": capture.OversizedCount,
		"specialFileCount": capture.SpecialFileCount,
	}
	if !capture.Available {
		metadata["reason"] = capture.Reason
		return metadata
	}
	metadata["worktreeTree"] = capture.WorktreeTree
	metadata["protectionRef"] = capture.ProtectionRef
	metadata["repositoryIdentity"] = capture.RepositoryIdentity
	metadata["head"] = capture.Head
	return metadata
}

func extractRepositoryCheckpoint(raw json.RawMessage) repositoryGitCheckpoint {
	var summary map[string]json.RawMessage
	if json.Unmarshal(raw, &summary) != nil {
		return repositoryGitCheckpoint{}
	}
	var metadata struct {
		Available          bool   `json:"available"`
		WorktreeTree       string `json:"worktreeTree"`
		ProtectionRef      string `json:"protectionRef"`
		RepositoryIdentity string `json:"repositoryIdentity"`
		Head               string `json:"head"`
	}
	if json.Unmarshal(summary["gitCheckpoint"], &metadata) != nil || !metadata.Available || !isRepositoryGitObjectID(metadata.WorktreeTree) {
		return repositoryGitCheckpoint{}
	}
	return repositoryGitCheckpoint{Available: true, WorktreeTree: metadata.WorktreeTree, ProtectionRef: metadata.ProtectionRef, RepositoryIdentity: metadata.RepositoryIdentity, Head: metadata.Head}
}

func collectRepositoryTaskDiff(ctx context.Context, before, after repositoryGitCheckpoint) ([]PlanExecutionCheckpointFile, map[string]any) {
	summary := map[string]any{"gitCheckpoint": repositoryCheckpointMetadata(after)}
	summary["diffAvailable"] = false
	if !after.Available {
		summary["diffReason"] = after.Reason
		return nil, summary
	}
	if !before.Available {
		summary["diffReason"] = "previous_checkpoint_unavailable"
		return nil, summary
	}
	if before.RepositoryIdentity != after.RepositoryIdentity {
		summary["diffReason"] = "repository_identity_changed"
		return nil, summary
	}
	if _, err := repositoryGitOutput(ctx, after.WorkspaceRoot, nil, "cat-file", "-e", before.WorktreeTree+"^{tree}"); err != nil {
		summary["diffReason"] = "previous_checkpoint_expired"
		return nil, summary
	}
	shortstat, _ := repositoryGitOutput(ctx, after.WorkspaceRoot, nil, "diff", "--shortstat", "--no-ext-diff", "--no-textconv", before.WorktreeTree, after.WorktreeTree)
	filesChanged, additions, deletions := parseCheckpointShortstat(string(shortstat))
	summary["filesChanged"] = filesChanged
	summary["additions"] = additions
	summary["deletions"] = deletions

	nameStatus, outputTruncated, err := repositoryGitOutputLimited(ctx, after.WorkspaceRoot, maxTaskCheckpointPathBytes, 0, "diff", "--name-status", "-z", "--find-renames", "--find-copies", "--no-ext-diff", "--no-textconv", before.WorktreeTree, after.WorktreeTree)
	if err != nil {
		summary["diffReason"] = "git_diff_unavailable"
		return nil, summary
	}
	changes, parseTruncated, err := parseCheckpointNameStatus(nameStatus, maxTaskCheckpointFiles)
	if err != nil {
		summary["diffReason"] = "git_diff_invalid"
		return nil, summary
	}
	files := make([]PlanExecutionCheckpointFile, 0, len(changes))
	remainingBytes, remainingLines := maxTaskCheckpointPatchBytes, maxTaskCheckpointPatchLines
	binaryFiles, oversizedFiles := 0, 0
	omitted := make([]map[string]any, 0)
	diffTruncated := outputTruncated || parseTruncated
	for _, change := range changes {
		normalized, normalizeErr := security.NormalizeWorkspaceRelativePath(after.WorkspaceRoot, change.Path)
		if normalizeErr != nil {
			summary["diffReason"] = "unsafe_git_path"
			return nil, summary
		}
		previous := ""
		if change.PreviousPath != "" {
			previous, normalizeErr = security.NormalizeWorkspaceRelativePath(after.WorkspaceRoot, change.PreviousPath)
			if normalizeErr != nil {
				summary["diffReason"] = "unsafe_git_path"
				return nil, summary
			}
		}
		file := PlanExecutionCheckpointFile{Path: normalized, PreviousPath: previous, Status: change.Status}
		file.Additions, file.Deletions, file.Binary = checkpointFileNumstat(ctx, after.WorkspaceRoot, before.WorktreeTree, after.WorktreeTree, file)
		oldMarker := readOversizedCheckpointMarker(ctx, after.WorkspaceRoot, before.WorktreeTree, checkpointOldPath(file))
		newMarker := readOversizedCheckpointMarker(ctx, after.WorkspaceRoot, after.WorktreeTree, file.Path)
		if file.Binary {
			binaryFiles++
			omitted = appendCheckpointOmission(omitted, file.Path, "binary", oldMarker, newMarker)
		} else if oldMarker != nil || newMarker != nil {
			oversizedFiles++
			file.Additions, file.Deletions = 0, 0
			omitted = appendCheckpointOmission(omitted, file.Path, "oversized", oldMarker, newMarker)
		} else if remainingBytes > 0 && remainingLines > 0 {
			paths := []string{file.Path}
			if file.PreviousPath != "" && file.PreviousPath != file.Path {
				paths = append(paths, file.PreviousPath)
			}
			args := []string{"diff", "--no-ext-diff", "--no-textconv", "--find-renames", "--unified=3", before.WorktreeTree, after.WorktreeTree, "--"}
			args = append(args, paths...)
			patch, truncated, patchErr := repositoryGitOutputLimited(ctx, after.WorkspaceRoot, remainingBytes, remainingLines, args...)
			if patchErr == nil {
				file.Hunks = parseCheckpointUnifiedHunks(patch)
				remainingBytes -= len(patch)
				remainingLines -= bytes.Count(patch, []byte{'\n'})
				diffTruncated = diffTruncated || truncated || checkpointUnifiedHunkCount(patch) > len(file.Hunks)
			} else {
				diffTruncated = true
			}
		} else {
			diffTruncated = true
		}
		files = append(files, file)
	}
	summary["diffAvailable"] = true
	summary["capturedFiles"] = len(files)
	summary["binaryFiles"] = binaryFiles
	summary["oversizedFiles"] = oversizedFiles
	summary["filesTruncated"] = outputTruncated || parseTruncated || filesChanged > len(files)
	summary["diffTruncated"] = diffTruncated
	if len(omitted) > 0 {
		summary["omittedFiles"] = omitted
	}
	return files, summary
}

func parseCheckpointShortstat(raw string) (files, additions, deletions int) {
	for _, part := range strings.Split(raw, ",") {
		fields := strings.Fields(strings.TrimSpace(part))
		if len(fields) < 2 {
			continue
		}
		value, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		switch {
		case strings.HasPrefix(fields[1], "file"):
			files = value
		case strings.HasPrefix(fields[1], "insertion"):
			additions = value
		case strings.HasPrefix(fields[1], "deletion"):
			deletions = value
		}
	}
	return
}

func parseCheckpointNameStatus(raw []byte, limit int) ([]checkpointNameStatus, bool, error) {
	parts := bytes.Split(raw, []byte{0})
	changes := make([]checkpointNameStatus, 0)
	truncated := false
	for index := 0; index < len(parts); {
		if len(changes) >= limit {
			return changes, true, nil
		}
		if len(parts[index]) == 0 {
			index++
			continue
		}
		code := string(parts[index])
		index++
		if len(code) == 0 {
			return nil, truncated, errors.New("empty Git diff status")
		}
		change := checkpointNameStatus{}
		switch code[0] {
		case 'A':
			change.Status = "added"
		case 'M':
			change.Status = "modified"
		case 'D':
			change.Status = "deleted"
		case 'T':
			change.Status = "type_changed"
		case 'U':
			change.Status = "unmerged"
		case 'R', 'C':
			if code[0] == 'R' {
				change.Status = "renamed"
			} else {
				change.Status = "copied"
			}
			if index+1 >= len(parts) {
				return nil, truncated, errors.New("incomplete rename record")
			}
			change.PreviousPath = string(parts[index])
			change.Path = string(parts[index+1])
			index += 2
		default:
			return nil, truncated, fmt.Errorf("unsupported Git diff status %q", code)
		}
		if change.Path == "" {
			if index >= len(parts) || len(parts[index]) == 0 {
				return nil, truncated, errors.New("missing Git diff path")
			}
			change.Path = string(parts[index])
			index++
		}
		if err := security.ValidateRelativePath(change.Path); err != nil {
			return nil, truncated, err
		}
		if change.PreviousPath != "" {
			if err := security.ValidateRelativePath(change.PreviousPath); err != nil {
				return nil, truncated, err
			}
		}
		changes = append(changes, change)
	}
	return changes, truncated, nil
}

func checkpointFileNumstat(ctx context.Context, workspace, beforeTree, afterTree string, file PlanExecutionCheckpointFile) (additions, deletions int, binary bool) {
	paths := []string{file.Path}
	if file.PreviousPath != "" && file.PreviousPath != file.Path {
		paths = append(paths, file.PreviousPath)
	}
	args := []string{"diff", "--numstat", "--find-renames", "--no-ext-diff", "--no-textconv", beforeTree, afterTree, "--"}
	args = append(args, paths...)
	output, err := repositoryGitOutput(ctx, workspace, nil, args...)
	if err != nil {
		return 0, 0, false
	}
	for _, line := range bytes.Split(bytes.TrimSpace(output), []byte{'\n'}) {
		fields := bytes.SplitN(line, []byte{'\t'}, 3)
		if len(fields) < 2 {
			continue
		}
		if string(fields[0]) == "-" || string(fields[1]) == "-" {
			binary = true
			continue
		}
		added, addErr := strconv.Atoi(string(fields[0]))
		removed, removeErr := strconv.Atoi(string(fields[1]))
		if addErr == nil {
			additions += added
		}
		if removeErr == nil {
			deletions += removed
		}
	}
	return
}

func checkpointOldPath(file PlanExecutionCheckpointFile) string {
	if file.PreviousPath != "" {
		return file.PreviousPath
	}
	return file.Path
}

func readOversizedCheckpointMarker(ctx context.Context, workspace, tree, relative string) *oversizedCheckpointBlob {
	if relative == "" || !isRepositoryGitObjectID(tree) {
		return nil
	}
	spec := tree + ":" + relative
	sizeOutput, err := repositoryGitOutput(ctx, workspace, nil, "cat-file", "-s", spec)
	if err != nil {
		return nil
	}
	size, err := strconv.Atoi(strings.TrimSpace(string(sizeOutput)))
	if err != nil || size > 1024 {
		return nil
	}
	content, err := repositoryGitOutput(ctx, workspace, nil, "cat-file", "blob", spec)
	if err != nil || !bytes.HasPrefix(content, []byte(oversizedCheckpointBlobPrefix)) {
		return nil
	}
	var marker oversizedCheckpointBlob
	if json.Unmarshal(bytes.TrimSpace(content[len(oversizedCheckpointBlobPrefix):]), &marker) != nil || marker.SHA256 == "" {
		return nil
	}
	return &marker
}

func appendCheckpointOmission(items []map[string]any, relative, reason string, before, after *oversizedCheckpointBlob) []map[string]any {
	if len(items) >= maxTaskCheckpointFiles {
		return items
	}
	item := map[string]any{"path": relative, "reason": reason}
	if before != nil {
		item["before"] = before
	}
	if after != nil {
		item["after"] = after
	}
	return append(items, item)
}

func checkpointUnifiedHunkCount(patch []byte) int {
	count := 0
	for _, line := range bytes.Split(patch, []byte{'\n'}) {
		if bytes.HasPrefix(line, []byte("@@ ")) {
			count++
		}
	}
	return count
}

func parseCheckpointUnifiedHunks(patch []byte) []PlanExecutionCheckpointHunk {
	lines := bytes.SplitAfter(patch, []byte{'\n'})
	hunks := make([]PlanExecutionCheckpointHunk, 0)
	var current *PlanExecutionCheckpointHunk
	for _, rawLine := range lines {
		line := strings.ToValidUTF8(string(rawLine), "�")
		if strings.HasPrefix(line, "@@ ") {
			oldStart, oldCount, newStart, newCount, ok := parseCheckpointHunkHeader(strings.TrimSpace(line))
			if !ok || len(hunks) >= maxTaskCheckpointHunks {
				current = nil
				continue
			}
			hunks = append(hunks, PlanExecutionCheckpointHunk{Header: strings.TrimSpace(line), OldStartLine: oldStart, OldLineCount: oldCount, NewStartLine: newStart, NewLineCount: newCount})
			current = &hunks[len(hunks)-1]
			continue
		}
		if current != nil {
			current.Patch += line
		}
	}
	return hunks
}

func parseCheckpointHunkHeader(header string) (oldStart, oldCount, newStart, newCount int, ok bool) {
	end := strings.Index(header[3:], " @@")
	if !strings.HasPrefix(header, "@@ ") || end < 0 {
		return 0, 0, 0, 0, false
	}
	fields := strings.Fields(header[3 : 3+end])
	if len(fields) != 2 || !strings.HasPrefix(fields[0], "-") || !strings.HasPrefix(fields[1], "+") {
		return 0, 0, 0, 0, false
	}
	oldStart, oldCount, ok = parseCheckpointHunkRange(fields[0][1:])
	if !ok {
		return 0, 0, 0, 0, false
	}
	newStart, newCount, ok = parseCheckpointHunkRange(fields[1][1:])
	return
}

func parseCheckpointHunkRange(value string) (start, count int, ok bool) {
	parts := strings.SplitN(value, ",", 2)
	start, err := strconv.Atoi(parts[0])
	if err != nil || start < 0 {
		return 0, 0, false
	}
	count = 1
	if len(parts) == 2 {
		count, err = strconv.Atoi(parts[1])
		if err != nil || count < 0 {
			return 0, 0, false
		}
	}
	return start, count, true
}

func loadPlanSnapshotState(ctx context.Context, q snapshotQueryer, planID uuid.UUID, generationProvider string) (planSnapshotState, error) {
	var state planSnapshotState
	err := q.QueryRow(ctx, `SELECT
		p.project_id,p.id,p.intake_id,p.version,p.content_version,p.spec,p.config_snapshot,
		i.version,i.kind,i.parent_intake_id,i.title,i.body,
		pr.version,pr.workspace_path_normalized,
		ps.version,ps.validation_command,ps.agent_provider,ps.codex_command,ps.codex_args,
		ps.claude_command,ps.claude_args,ps.plan_generation_timeout_seconds,
		ps.task_execution_timeout_seconds,ps.max_retries,ps.allowed_env
		FROM plans p
		JOIN intakes i ON i.id=p.intake_id
		JOIN projects pr ON pr.id=p.project_id
		JOIN project_settings ps ON ps.project_id=p.project_id
		WHERE p.id=$1`, planID).Scan(
		&state.ProjectID, &state.PlanID, &state.IntakeID, &state.PlanResourceVersion,
		&state.PlanContentVersion, &state.PlanSpec, &state.PlanConfigSnapshot,
		&state.RequirementVersion, &state.RequirementKind, &state.RequirementParentID,
		&state.RequirementTitle, &state.RequirementBody, &state.ProjectVersion,
		&state.WorkspacePathNormalized, &state.ConfigVersion, &state.ValidationCommand,
		&state.AgentProvider, &state.CodexCommand, &state.CodexArgs, &state.ClaudeCommand,
		&state.ClaudeArgs, &state.PlanTimeoutSeconds, &state.TaskTimeoutSeconds,
		&state.MaxRetries, &state.AllowedEnv)
	if err != nil {
		return state, mapNotFound(err)
	}

	state.GenerationProvider = strings.TrimSpace(generationProvider)
	if state.GenerationProvider == "" {
		var provider string
		err = q.QueryRow(ctx, `SELECT coalesce(payload->>'provider','') FROM jobs
			WHERE job_type='plan.generate' AND aggregate_type='intake' AND aggregate_id=$1
			ORDER BY created_at DESC LIMIT 1`, state.IntakeID).Scan(&provider)
		if err == nil {
			state.GenerationProvider = strings.TrimSpace(provider)
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return state, err
		}
	}
	if state.GenerationProvider == "" {
		state.GenerationProvider = state.AgentProvider
	}
	state.ExecutionProvider, err = providerFromPlanConfigSnapshot(state.PlanConfigSnapshot)
	if err != nil {
		return state, err
	}
	if state.ExecutionProvider == "" {
		state.ExecutionProvider = state.AgentProvider
	}

	requirementContent, err := json.Marshal(map[string]any{
		"kind": state.RequirementKind, "parentIntakeId": state.RequirementParentID,
		"title": state.RequirementTitle, "body": state.RequirementBody,
	})
	if err != nil {
		return state, err
	}
	state.RequirementDigest = canonicalJSONDigest(requirementContent)
	state.PlanSpecDigest = canonicalJSONDigest(state.PlanSpec)
	state.KeyExecutionFields, err = json.Marshal(map[string]any{
		"validationCommand": state.ValidationCommand,
		"codexCommand":      state.CodexCommand, "codexArgs": state.CodexArgs,
		"claudeCommand": state.ClaudeCommand, "claudeArgs": state.ClaudeArgs,
		"planGenerationTimeoutSeconds": state.PlanTimeoutSeconds,
		"taskExecutionTimeoutSeconds":  state.TaskTimeoutSeconds,
		"maxRetries":                   state.MaxRetries, "allowedEnv": state.AllowedEnv,
		"planConfigSnapshot": state.PlanConfigSnapshot,
	})
	if err != nil {
		return state, err
	}
	state.GitRoot, state.GitRepositoryIdentity, state.GitBranch, state.GitHead, state.GitWorkspaceDigest = readGitSnapshot(ctx, state.WorkspacePathNormalized)
	return state, nil
}

func scanPlanExecutionSnapshot(row pgx.Row) (domain.PlanExecutionSnapshot, error) {
	var snapshot domain.PlanExecutionSnapshot
	err := row.Scan(&snapshot.ID, &snapshot.ProjectID, &snapshot.PlanID, &snapshot.IntakeID,
		&snapshot.PreviousSnapshotID, &snapshot.TaskID, &snapshot.Sequence, &snapshot.Kind,
		&snapshot.RequirementID, &snapshot.RequirementVersion, &snapshot.RequirementDigest,
		&snapshot.PlanResourceVersion, &snapshot.PlanContentVersion, &snapshot.PlanSpecDigest,
		&snapshot.ProjectVersion, &snapshot.ConfigVersion, &snapshot.KeyExecutionFields,
		&snapshot.GenerationProvider, &snapshot.ExecutionProvider, &snapshot.WorkspacePathNormalized,
		&snapshot.GitRoot, &snapshot.GitRepositoryIdentity, &snapshot.GitBranch, &snapshot.GitHead,
		&snapshot.GitWorkspaceDigest, &snapshot.CreatedAt)
	return snapshot, err
}

const planExecutionSnapshotColumns = `id,project_id,plan_id,intake_id,previous_snapshot_id,task_id,sequence,snapshot_kind,
	requirement_id,requirement_version,requirement_digest,plan_resource_version,plan_content_version,
	plan_spec_digest,project_version,config_version,key_execution_fields,generation_provider,execution_provider,
	workspace_path_normalized,git_root,git_repository_identity,git_branch,git_head,git_workspace_digest,created_at`

func latestPlanExecutionSnapshot(ctx context.Context, q snapshotQueryer, planID uuid.UUID) (domain.PlanExecutionSnapshot, error) {
	return scanPlanExecutionSnapshot(q.QueryRow(ctx, `SELECT `+planExecutionSnapshotColumns+`
		FROM plan_execution_snapshots WHERE plan_id=$1 ORDER BY sequence DESC LIMIT 1`, planID))
}

func (s *Store) capturePlanExecutionSnapshotTx(ctx context.Context, tx pgx.Tx, planID uuid.UUID, kind string, taskID *uuid.UUID, generationProvider string) (domain.PlanExecutionSnapshot, error) {
	return s.capturePlanExecutionSnapshotWithWorkspaceTx(ctx, tx, planID, kind, taskID, generationProvider, nil)
}

// capturePlanExecutionSnapshotWithWorkspaceTx writes a complete immutable snapshot in
// one INSERT. A caller-provided workspace state comes from the lifecycle boundary,
// where it is captured at the same point as plan generation or task completion.
func (s *Store) capturePlanExecutionSnapshotWithWorkspaceTx(ctx context.Context, tx pgx.Tx, planID uuid.UUID, kind string, taskID *uuid.UUID, generationProvider string, workspace *PlanWorkspaceState) (domain.PlanExecutionSnapshot, error) {
	return s.capturePlanExecutionSnapshotWithChangesAndWorkspaceTx(ctx, tx, planID, kind, taskID, generationProvider, PlanExecutionCheckpointParams{}, workspace)
}

func (s *Store) capturePlanExecutionSnapshotWithChangesTx(ctx context.Context, tx pgx.Tx, planID uuid.UUID, kind string, taskID *uuid.UUID, generationProvider string, changes PlanExecutionCheckpointParams) (domain.PlanExecutionSnapshot, error) {
	return s.capturePlanExecutionSnapshotWithChangesAndWorkspaceTx(ctx, tx, planID, kind, taskID, generationProvider, changes, nil)
}

func (s *Store) capturePlanExecutionSnapshotWithChangesAndWorkspaceTx(ctx context.Context, tx pgx.Tx, planID uuid.UUID, kind string, taskID *uuid.UUID, generationProvider string, changes PlanExecutionCheckpointParams, workspace *PlanWorkspaceState) (domain.PlanExecutionSnapshot, error) {
	if kind != domain.PlanSnapshotKindGenerationBaseline && kind != domain.PlanSnapshotKindTaskCheckpoint && kind != domain.PlanSnapshotKindUserAccepted {
		return domain.PlanExecutionSnapshot{}, domain.ErrInvalidPlanDriftAudit
	}
	if _, err := tx.Exec(ctx, `SELECT 1 FROM plans WHERE id=$1 FOR UPDATE`, planID); err != nil {
		return domain.PlanExecutionSnapshot{}, err
	}
	var previousID *uuid.UUID
	var previousSnapshotID uuid.UUID
	var previousGenerationProvider string
	var previousChangeSummary json.RawMessage
	var sequence int64
	err := tx.QueryRow(ctx, `SELECT id,sequence,generation_provider,change_summary FROM plan_execution_snapshots WHERE plan_id=$1 ORDER BY sequence DESC LIMIT 1`, planID).Scan(&previousSnapshotID, &sequence, &previousGenerationProvider, &previousChangeSummary)
	if errors.Is(err, pgx.ErrNoRows) {
		sequence = 0
	} else if err != nil {
		return domain.PlanExecutionSnapshot{}, err
	} else {
		previousID = &previousSnapshotID
		if strings.TrimSpace(generationProvider) == "" {
			generationProvider = previousGenerationProvider
		}
	}
	if kind == domain.PlanSnapshotKindGenerationBaseline && sequence != 0 {
		return domain.PlanExecutionSnapshot{}, domain.ErrInvalidPlanDriftAudit
	}
	if kind == domain.PlanSnapshotKindTaskCheckpoint && sequence == 0 {
		return domain.PlanExecutionSnapshot{}, domain.ErrPlanExecutionBaselineMissing
	}
	state, err := loadPlanSnapshotState(ctx, tx, planID, generationProvider)
	if err != nil {
		return domain.PlanExecutionSnapshot{}, err
	}
	if workspace != nil {
		state.WorkspacePathNormalized = workspace.NormalizedPath
		state.GitRoot = workspace.GitRoot
		state.GitRepositoryIdentity = workspace.GitRepositoryIdentity
		state.GitBranch = workspace.GitBranch
		state.GitHead = workspace.GitHead
		state.GitWorkspaceDigest = workspace.GitWorkspaceDigest
	}
	automaticTaskCheckpoint := kind == domain.PlanSnapshotKindTaskCheckpoint && len(changes.Files) == 0 && len(changes.ChangeSummary) == 0
	if !automaticTaskCheckpoint {
		changes, err = preparePlanExecutionChanges(state.WorkspacePathNormalized, changes)
		if err != nil {
			return domain.PlanExecutionSnapshot{}, err
		}
	}
	id := uuid.New()
	currentGit := captureRepositoryGitCheckpoint(ctx, state.WorkspacePathNormalized, id)
	if automaticTaskCheckpoint {
		previousGit := extractRepositoryCheckpoint(previousChangeSummary)
		previousGit.WorkspaceRoot = currentGit.WorkspaceRoot
		var diffSummary map[string]any
		changes.Files, diffSummary = collectRepositoryTaskDiff(ctx, previousGit, currentGit)
		changes.ChangeSummary, err = json.Marshal(diffSummary)
		if err != nil {
			return domain.PlanExecutionSnapshot{}, err
		}
	} else {
		changes.ChangeSummary, err = mergePlanExecutionChangeSummary(changes.ChangeSummary, map[string]any{"gitCheckpoint": repositoryCheckpointMetadata(currentGit)})
		if err != nil {
			return domain.PlanExecutionSnapshot{}, err
		}
	}
	changes, err = preparePlanExecutionChanges(state.WorkspacePathNormalized, changes)
	if err != nil {
		return domain.PlanExecutionSnapshot{}, err
	}
	changeSummary, additions, deletions, err := normalizePlanExecutionChanges(changes)
	if err != nil {
		return domain.PlanExecutionSnapshot{}, err
	}
	snapshot, err := scanPlanExecutionSnapshot(tx.QueryRow(ctx, `INSERT INTO plan_execution_snapshots(
		id,project_id,plan_id,intake_id,previous_snapshot_id,task_id,sequence,snapshot_kind,
		requirement_id,requirement_version,requirement_digest,plan_resource_version,plan_content_version,
		plan_spec_digest,project_version,config_version,key_execution_fields,generation_provider,execution_provider,
		workspace_path_normalized,git_root,git_repository_identity,git_branch,git_head,git_workspace_digest,
		change_summary,additions,deletions)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28)
		RETURNING `+planExecutionSnapshotColumns,
		id, state.ProjectID, state.PlanID, state.IntakeID, previousID, taskID, sequence+1, kind,
		state.IntakeID, state.RequirementVersion, state.RequirementDigest, state.PlanResourceVersion,
		state.PlanContentVersion, state.PlanSpecDigest, state.ProjectVersion, state.ConfigVersion,
		state.KeyExecutionFields, state.GenerationProvider, state.ExecutionProvider,
		state.WorkspacePathNormalized, state.GitRoot, state.GitRepositoryIdentity, state.GitBranch,
		state.GitHead, state.GitWorkspaceDigest, changeSummary, additions, deletions))
	if err != nil {
		return domain.PlanExecutionSnapshot{}, err
	}
	snapshot.ChangeSummary = changeSummary
	snapshot.Additions = additions
	snapshot.Deletions = deletions
	snapshot.Files, err = insertPlanExecutionSnapshotFilesTx(ctx, tx, snapshot.ID, changes.Files)
	if err != nil {
		return domain.PlanExecutionSnapshot{}, err
	}
	return snapshot, nil
}

func mergePlanExecutionChangeSummary(raw json.RawMessage, values map[string]any) (json.RawMessage, error) {
	summary := map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &summary); err != nil || summary == nil {
			return nil, fmt.Errorf("checkpoint change summary must be a JSON object: %w", domain.ErrInvalidFeedbackLink)
		}
	}
	for key, value := range values {
		summary[key] = value
	}
	return json.Marshal(summary)
}

func preparePlanExecutionChanges(workspace string, changes PlanExecutionCheckpointParams) (PlanExecutionCheckpointParams, error) {
	summary := map[string]any{}
	if len(changes.ChangeSummary) > 0 {
		if err := json.Unmarshal(changes.ChangeSummary, &summary); err != nil || summary == nil {
			return changes, fmt.Errorf("checkpoint change summary must be a JSON object: %w", domain.ErrInvalidFeedbackLink)
		}
	}
	if len(changes.Files) > maxTaskCheckpointFiles {
		summary["filesTruncated"] = true
		summary["omittedFileCount"] = len(changes.Files) - maxTaskCheckpointFiles
		changes.Files = append([]PlanExecutionCheckpointFile(nil), changes.Files[:maxTaskCheckpointFiles]...)
	}
	workspaceExists := false
	if info, err := os.Stat(workspace); err == nil && info.IsDir() {
		workspaceExists = true
	}
	seen := map[string]struct{}{}
	remainingBytes, remainingLines, remainingHunks := maxTaskCheckpointPatchBytes, maxTaskCheckpointPatchLines, maxTaskCheckpointHunks
	contentTruncated := false
	for fileIndex := range changes.Files {
		file := &changes.Files[fileIndex]
		normalized, err := normalizeCheckpointRelativePath(workspace, file.Path, workspaceExists)
		if err != nil {
			return changes, fmt.Errorf("invalid checkpoint path %q: %w", file.Path, domain.ErrInvalidFeedbackLink)
		}
		file.Path = normalized
		if file.PreviousPath != "" {
			file.PreviousPath, err = normalizeCheckpointRelativePath(workspace, file.PreviousPath, workspaceExists)
			if err != nil {
				return changes, fmt.Errorf("invalid checkpoint previous path %q: %w", file.PreviousPath, domain.ErrInvalidFeedbackLink)
			}
		}
		key := file.Status + "\x00" + file.PreviousPath + "\x00" + file.Path
		if _, duplicate := seen[key]; duplicate {
			return changes, fmt.Errorf("duplicate checkpoint file %q: %w", file.Path, domain.ErrInvalidFeedbackLink)
		}
		seen[key] = struct{}{}
		boundedHunks := make([]PlanExecutionCheckpointHunk, 0, len(file.Hunks))
		for _, hunk := range file.Hunks {
			if remainingBytes <= 0 || remainingLines <= 0 || remainingHunks <= 0 {
				contentTruncated = true
				break
			}
			hunk.Header = strings.TrimSpace(strings.ToValidUTF8(hunk.Header, "�"))
			if len(hunk.Header) > 1024 {
				hunk.Header = hunk.Header[:1024]
				contentTruncated = true
			}
			hunk.Patch = strings.ToValidUTF8(hunk.Patch, "�")
			boundedPatch, truncated := boundCheckpointText(hunk.Patch, remainingBytes, remainingLines)
			hunk.Patch = boundedPatch
			remainingBytes -= len(hunk.Patch)
			remainingLines -= strings.Count(hunk.Patch, "\n")
			remainingHunks--
			contentTruncated = contentTruncated || truncated
			boundedHunks = append(boundedHunks, hunk)
			if truncated {
				break
			}
		}
		file.Hunks = boundedHunks
	}
	if contentTruncated {
		summary["diffTruncated"] = true
	}
	changes.ChangeSummary, _ = json.Marshal(summary)
	return changes, nil
}

func normalizeCheckpointRelativePath(workspace, relative string, inspectWorkspace bool) (string, error) {
	if err := security.ValidateRelativePath(relative); err != nil {
		return "", err
	}
	normalized := path.Clean(relative)
	if inspectWorkspace {
		return security.NormalizeWorkspaceRelativePath(workspace, normalized)
	}
	return normalized, nil
}

func boundCheckpointText(value string, maxBytes, maxLines int) (string, bool) {
	if maxBytes <= 0 || maxLines <= 0 {
		return "", value != ""
	}
	var builder strings.Builder
	builder.Grow(min(len(value), maxBytes))
	lines := 0
	truncated := false
	for _, character := range value {
		encodedLength := len(string(character))
		if builder.Len()+encodedLength > maxBytes || lines >= maxLines {
			truncated = true
			break
		}
		builder.WriteRune(character)
		if character == '\n' {
			lines++
		}
	}
	return builder.String(), truncated
}

func normalizePlanExecutionChanges(changes PlanExecutionCheckpointParams) (json.RawMessage, int, int, error) {
	summary := changes.ChangeSummary
	if len(summary) == 0 {
		summary = json.RawMessage(`{}`)
	}
	var summaryObject map[string]any
	if err := json.Unmarshal(summary, &summaryObject); err != nil || summaryObject == nil {
		return nil, 0, 0, fmt.Errorf("checkpoint change summary must be a JSON object: %w", domain.ErrInvalidFeedbackLink)
	}
	normalizedSummary, err := json.Marshal(summaryObject)
	if err != nil {
		return nil, 0, 0, err
	}
	allowedStatuses := map[string]struct{}{
		"added": {}, "modified": {}, "deleted": {}, "renamed": {}, "copied": {},
		"type_changed": {}, "unmerged": {}, "untracked": {},
	}
	additions, deletions := 0, 0
	for fileIndex, file := range changes.Files {
		if strings.TrimSpace(file.Path) == "" || file.Additions < 0 || file.Deletions < 0 {
			return nil, 0, 0, fmt.Errorf("invalid checkpoint file at position %d: %w", fileIndex+1, domain.ErrInvalidFeedbackLink)
		}
		if _, ok := allowedStatuses[file.Status]; !ok {
			return nil, 0, 0, fmt.Errorf("invalid checkpoint file status %q: %w", file.Status, domain.ErrInvalidFeedbackLink)
		}
		if (file.Status == "renamed" || file.Status == "copied") && strings.TrimSpace(file.PreviousPath) == "" {
			return nil, 0, 0, fmt.Errorf("checkpoint %s file requires previous path: %w", file.Status, domain.ErrInvalidFeedbackLink)
		}
		for hunkIndex, hunk := range file.Hunks {
			if strings.TrimSpace(hunk.Header) == "" || hunk.OldStartLine < 0 || hunk.OldLineCount < 0 || hunk.NewStartLine < 0 || hunk.NewLineCount < 0 || (hunk.OldLineCount == 0 && hunk.NewLineCount == 0) {
				return nil, 0, 0, fmt.Errorf("invalid diff hunk %d for checkpoint file %d: %w", hunkIndex+1, fileIndex+1, domain.ErrInvalidDiffRange)
			}
		}
		additions += file.Additions
		deletions += file.Deletions
	}
	return normalizedSummary, additions, deletions, nil
}

func insertPlanExecutionSnapshotFilesTx(ctx context.Context, tx pgx.Tx, snapshotID uuid.UUID, files []PlanExecutionCheckpointFile) ([]domain.PlanExecutionSnapshotFile, error) {
	out := make([]domain.PlanExecutionSnapshotFile, 0, len(files))
	for fileIndex, input := range files {
		file := domain.PlanExecutionSnapshotFile{Hunks: []domain.PlanExecutionSnapshotHunk{}}
		err := tx.QueryRow(ctx, `INSERT INTO plan_execution_snapshot_files(
			id,snapshot_id,file_sequence,path,previous_path,file_status,staged,is_binary,additions,deletions)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
			RETURNING id,snapshot_id,file_sequence,path,previous_path,file_status,staged,is_binary,additions,deletions,created_at`,
			uuid.New(), snapshotID, fileIndex+1, input.Path, input.PreviousPath, input.Status, input.Staged, input.Binary, input.Additions, input.Deletions).Scan(
			&file.ID, &file.SnapshotID, &file.Sequence, &file.Path, &file.PreviousPath, &file.Status,
			&file.Staged, &file.Binary, &file.Additions, &file.Deletions, &file.CreatedAt)
		if err != nil {
			return nil, err
		}
		for hunkIndex, inputHunk := range input.Hunks {
			hunk := domain.PlanExecutionSnapshotHunk{}
			err = tx.QueryRow(ctx, `INSERT INTO plan_execution_snapshot_diff_hunks(
				id,file_id,hunk_sequence,hunk_header,patch,old_start_line,old_line_count,new_start_line,new_line_count)
				VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)
				RETURNING id,file_id,hunk_sequence,hunk_header,patch,old_start_line,old_line_count,new_start_line,new_line_count,created_at`,
				uuid.New(), file.ID, hunkIndex+1, inputHunk.Header, inputHunk.Patch,
				inputHunk.OldStartLine, inputHunk.OldLineCount, inputHunk.NewStartLine, inputHunk.NewLineCount).Scan(
				&hunk.ID, &hunk.FileID, &hunk.Sequence, &hunk.Header, &hunk.Patch,
				&hunk.OldStartLine, &hunk.OldLineCount, &hunk.NewStartLine, &hunk.NewLineCount, &hunk.CreatedAt)
			if err != nil {
				return nil, err
			}
			file.Hunks = append(file.Hunks, hunk)
		}
		out = append(out, file)
	}
	return out, nil
}

const planExecutionSnapshotDetailColumns = planExecutionSnapshotColumns + `,change_summary,additions,deletions`

func scanPlanExecutionSnapshotWithDetails(row pgx.Row) (domain.PlanExecutionSnapshot, error) {
	var snapshot domain.PlanExecutionSnapshot
	err := row.Scan(&snapshot.ID, &snapshot.ProjectID, &snapshot.PlanID, &snapshot.IntakeID,
		&snapshot.PreviousSnapshotID, &snapshot.TaskID, &snapshot.Sequence, &snapshot.Kind,
		&snapshot.RequirementID, &snapshot.RequirementVersion, &snapshot.RequirementDigest,
		&snapshot.PlanResourceVersion, &snapshot.PlanContentVersion, &snapshot.PlanSpecDigest,
		&snapshot.ProjectVersion, &snapshot.ConfigVersion, &snapshot.KeyExecutionFields,
		&snapshot.GenerationProvider, &snapshot.ExecutionProvider, &snapshot.WorkspacePathNormalized,
		&snapshot.GitRoot, &snapshot.GitRepositoryIdentity, &snapshot.GitBranch, &snapshot.GitHead,
		&snapshot.GitWorkspaceDigest, &snapshot.CreatedAt, &snapshot.ChangeSummary, &snapshot.Additions, &snapshot.Deletions)
	return snapshot, err
}

func loadPlanExecutionSnapshotFiles(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, snapshotID uuid.UUID) ([]domain.PlanExecutionSnapshotFile, error) {
	rows, err := q.Query(ctx, `SELECT id,snapshot_id,file_sequence,path,previous_path,file_status,staged,is_binary,additions,deletions,created_at
		FROM plan_execution_snapshot_files WHERE snapshot_id=$1 ORDER BY file_sequence`, snapshotID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	files := []domain.PlanExecutionSnapshotFile{}
	for rows.Next() {
		file := domain.PlanExecutionSnapshotFile{Hunks: []domain.PlanExecutionSnapshotHunk{}}
		if err = rows.Scan(&file.ID, &file.SnapshotID, &file.Sequence, &file.Path, &file.PreviousPath, &file.Status,
			&file.Staged, &file.Binary, &file.Additions, &file.Deletions, &file.CreatedAt); err != nil {
			return nil, err
		}
		files = append(files, file)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	for index := range files {
		hunkRows, queryErr := q.Query(ctx, `SELECT id,file_id,hunk_sequence,hunk_header,patch,old_start_line,old_line_count,new_start_line,new_line_count,created_at
			FROM plan_execution_snapshot_diff_hunks WHERE file_id=$1 ORDER BY hunk_sequence`, files[index].ID)
		if queryErr != nil {
			return nil, queryErr
		}
		for hunkRows.Next() {
			hunk := domain.PlanExecutionSnapshotHunk{}
			if err = hunkRows.Scan(&hunk.ID, &hunk.FileID, &hunk.Sequence, &hunk.Header, &hunk.Patch,
				&hunk.OldStartLine, &hunk.OldLineCount, &hunk.NewStartLine, &hunk.NewLineCount, &hunk.CreatedAt); err != nil {
				hunkRows.Close()
				return nil, err
			}
			files[index].Hunks = append(files[index].Hunks, hunk)
		}
		if err = hunkRows.Err(); err != nil {
			hunkRows.Close()
			return nil, err
		}
		hunkRows.Close()
	}
	return files, nil
}

func (s *Store) CapturePlanExecutionCheckpoint(ctx context.Context, params PlanExecutionCheckpointParams) (domain.PlanExecutionSnapshot, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return domain.PlanExecutionSnapshot{}, err
	}
	defer tx.Rollback(ctx)
	var planProject uuid.UUID
	if err = tx.QueryRow(ctx, `SELECT project_id FROM plans WHERE id=$1`, params.PlanID).Scan(&planProject); errors.Is(err, pgx.ErrNoRows) {
		return domain.PlanExecutionSnapshot{}, domain.ErrNotFound
	} else if err != nil {
		return domain.PlanExecutionSnapshot{}, err
	}
	if planProject != params.ProjectID {
		return domain.PlanExecutionSnapshot{}, domain.ErrForbidden
	}
	var taskProject, taskPlan uuid.UUID
	if err = tx.QueryRow(ctx, `SELECT project_id,plan_id FROM plan_tasks WHERE id=$1`, params.TaskID).Scan(&taskProject, &taskPlan); errors.Is(err, pgx.ErrNoRows) {
		return domain.PlanExecutionSnapshot{}, domain.ErrNotFound
	} else if err != nil {
		return domain.PlanExecutionSnapshot{}, err
	}
	if taskProject != params.ProjectID {
		return domain.PlanExecutionSnapshot{}, domain.ErrForbidden
	}
	if taskPlan != params.PlanID {
		return domain.PlanExecutionSnapshot{}, domain.ErrInvalidFeedbackLink
	}
	snapshot, err := s.capturePlanExecutionSnapshotWithChangesTx(ctx, tx, params.PlanID, domain.PlanSnapshotKindTaskCheckpoint, &params.TaskID, "", params)
	if err != nil {
		return domain.PlanExecutionSnapshot{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return domain.PlanExecutionSnapshot{}, err
	}
	return snapshot, nil
}

func (s *Store) GetPlanExecutionSnapshot(ctx context.Context, id uuid.UUID) (domain.PlanExecutionSnapshot, error) {
	snapshot, err := scanPlanExecutionSnapshotWithDetails(s.Pool.QueryRow(ctx, `SELECT `+planExecutionSnapshotDetailColumns+` FROM plan_execution_snapshots WHERE id=$1`, id))
	if err = mapNotFound(err); err != nil {
		return domain.PlanExecutionSnapshot{}, err
	}
	snapshot.Files, err = loadPlanExecutionSnapshotFiles(ctx, s.Pool, snapshot.ID)
	return snapshot, err
}

func (s *Store) GetLatestPlanExecutionSnapshot(ctx context.Context, planID uuid.UUID) (domain.PlanExecutionSnapshot, error) {
	snapshot, err := scanPlanExecutionSnapshotWithDetails(s.Pool.QueryRow(ctx, `SELECT `+planExecutionSnapshotDetailColumns+` FROM plan_execution_snapshots WHERE plan_id=$1 ORDER BY sequence DESC LIMIT 1`, planID))
	if err = mapNotFound(err); err != nil {
		return domain.PlanExecutionSnapshot{}, err
	}
	snapshot.Files, err = loadPlanExecutionSnapshotFiles(ctx, s.Pool, snapshot.ID)
	return snapshot, err
}

func (s *Store) ListPlanExecutionSnapshots(ctx context.Context, planID uuid.UUID) ([]domain.PlanExecutionSnapshot, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+planExecutionSnapshotDetailColumns+` FROM plan_execution_snapshots WHERE plan_id=$1 ORDER BY sequence`, planID)
	if err != nil {
		return nil, err
	}
	items := []domain.PlanExecutionSnapshot{}
	for rows.Next() {
		snapshot, scanErr := scanPlanExecutionSnapshotWithDetails(rows)
		if scanErr != nil {
			rows.Close()
			return nil, scanErr
		}
		items = append(items, snapshot)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	for index := range items {
		items[index].Files, err = loadPlanExecutionSnapshotFiles(ctx, s.Pool, items[index].ID)
		if err != nil {
			return nil, err
		}
	}
	return items, nil
}

func driftDifference(field string, baseline, current any) map[string]any {
	return map[string]any{"field": field, "baseline": baseline, "current": current}
}

func (s *Store) GetPlanDrift(ctx context.Context, planID uuid.UUID) (domain.PlanDrift, error) {
	drift := domain.PlanDrift{PlanID: planID, Status: domain.PlanDriftStatusMissingBaseline, RequiresExplicitDisposition: true}
	latest, err := latestPlanExecutionSnapshot(ctx, s.Pool, planID)
	if errors.Is(err, pgx.ErrNoRows) {
		var exists bool
		if checkErr := s.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM plans WHERE id=$1)`, planID).Scan(&exists); checkErr != nil {
			return drift, checkErr
		}
		if !exists {
			return drift, domain.ErrNotFound
		}
		drift.Differences = mustJSON([]map[string]any{driftDifference("executionSnapshot", nil, "missing")})
		return drift, nil
	}
	if err != nil {
		return drift, err
	}
	current, err := loadPlanSnapshotState(ctx, s.Pool, planID, latest.GenerationProvider)
	if err != nil {
		return drift, err
	}
	differences := []map[string]any{}
	compare := func(field string, baseline, value any) {
		if fmt.Sprint(baseline) != fmt.Sprint(value) {
			differences = append(differences, driftDifference(field, baseline, value))
		}
	}
	compare("requirementVersion", latest.RequirementVersion, current.RequirementVersion)
	compare("requirementDigest", latest.RequirementDigest, current.RequirementDigest)
	compare("planContentVersion", latest.PlanContentVersion, current.PlanContentVersion)
	compare("planSpecDigest", latest.PlanSpecDigest, current.PlanSpecDigest)
	compare("projectVersion", latest.ProjectVersion, current.ProjectVersion)
	compare("configVersion", latest.ConfigVersion, current.ConfigVersion)
	compare("keyExecutionFieldsDigest", canonicalJSONDigest(latest.KeyExecutionFields), canonicalJSONDigest(current.KeyExecutionFields))
	compare("executionProvider", latest.ExecutionProvider, current.ExecutionProvider)
	compare("workspacePathNormalized", latest.WorkspacePathNormalized, current.WorkspacePathNormalized)
	compare("gitRoot", latest.GitRoot, current.GitRoot)
	compare("gitRepositoryIdentity", latest.GitRepositoryIdentity, current.GitRepositoryIdentity)
	compare("gitBranch", latest.GitBranch, current.GitBranch)
	compare("gitHead", latest.GitHead, current.GitHead)
	compare("gitWorkspaceDigest", latest.GitWorkspaceDigest, current.GitWorkspaceDigest)
	drift.BaselineSnapshotID = &latest.ID
	drift.BaselineSnapshotSequence = latest.Sequence
	drift.Differences = mustJSON(differences)
	if len(differences) == 0 {
		drift.Status = domain.PlanDriftStatusClean
		drift.RequiresExplicitDisposition = false
	} else {
		drift.Status = domain.PlanDriftStatusDetected
		drift.RequiresExplicitDisposition = true
	}
	return drift, nil
}

func (s *Store) decoratePlanExecutionState(ctx context.Context, plan *domain.Plan) error {
	if err := s.Pool.QueryRow(ctx, `SELECT content_version FROM plans WHERE id=$1`, plan.ID).Scan(&plan.ContentVersion); err != nil {
		return err
	}
	drift, err := s.GetPlanDrift(ctx, plan.ID)
	if err != nil {
		return err
	}
	plan.ExecutionSnapshotID = drift.BaselineSnapshotID
	plan.ExecutionSnapshotSequence = drift.BaselineSnapshotSequence
	plan.DriftStatus = drift.Status
	plan.DriftResolutionRequired = drift.RequiresExplicitDisposition
	return nil
}

func validateDriftAuditInput(action, channel, reason string, rawDiff json.RawMessage) error {
	return domain.ValidatePlanDriftAudit(action, channel, reason, rawDiff)
}

func scanPlanDriftAudit(row pgx.Row) (domain.PlanDriftAudit, error) {
	var audit domain.PlanDriftAudit
	err := row.Scan(&audit.ID, &audit.ProjectID, &audit.PlanID, &audit.Sequence, &audit.Action,
		&audit.OriginalSnapshotID, &audit.NewSnapshotID, &audit.TargetPlanID, &audit.RawDiff,
		&audit.Channel, &audit.Reason, &audit.OccurredAt)
	return audit, err
}

const planDriftAuditColumns = `id,project_id,plan_id,sequence,action,original_snapshot_id,new_snapshot_id,target_plan_id,raw_diff,channel,reason,occurred_at`

func insertPlanDriftAuditTx(ctx context.Context, tx pgx.Tx, projectID, planID uuid.UUID, action string, originalSnapshotID, newSnapshotID, targetPlanID *uuid.UUID, rawDiff json.RawMessage, channel, reason string) (domain.PlanDriftAudit, error) {
	if err := validateDriftAuditInput(action, channel, reason, rawDiff); err != nil {
		return domain.PlanDriftAudit{}, err
	}
	var sequence int64
	if err := tx.QueryRow(ctx, `SELECT coalesce(max(sequence),0)+1 FROM plan_drift_audits WHERE plan_id=$1`, planID).Scan(&sequence); err != nil {
		return domain.PlanDriftAudit{}, err
	}
	return scanPlanDriftAudit(tx.QueryRow(ctx, `INSERT INTO plan_drift_audits(
		id,project_id,plan_id,sequence,action,original_snapshot_id,new_snapshot_id,target_plan_id,raw_diff,channel,reason)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) RETURNING `+planDriftAuditColumns,
		uuid.New(), projectID, planID, sequence, action, originalSnapshotID, newSnapshotID,
		targetPlanID, rawDiff, strings.TrimSpace(channel), strings.TrimSpace(reason)))
}

func (s *Store) AcceptPlanExecutionSnapshot(ctx context.Context, planID uuid.UUID, originalSnapshotID uuid.UUID, rawDiff json.RawMessage, channel, reason string) (domain.PlanExecutionSnapshot, domain.PlanDriftAudit, error) {
	if err := validateDriftAuditInput(domain.PlanDriftAuditSnapshotUpdated, channel, reason, rawDiff); err != nil {
		return domain.PlanExecutionSnapshot{}, domain.PlanDriftAudit{}, err
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return domain.PlanExecutionSnapshot{}, domain.PlanDriftAudit{}, err
	}
	defer tx.Rollback(ctx)
	var projectID uuid.UUID
	if err = tx.QueryRow(ctx, `SELECT project_id FROM plans WHERE id=$1 FOR UPDATE`, planID).Scan(&projectID); errors.Is(err, pgx.ErrNoRows) {
		return domain.PlanExecutionSnapshot{}, domain.PlanDriftAudit{}, domain.ErrNotFound
	} else if err != nil {
		return domain.PlanExecutionSnapshot{}, domain.PlanDriftAudit{}, err
	}
	latest, err := latestPlanExecutionSnapshot(ctx, tx, planID)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.PlanExecutionSnapshot{}, domain.PlanDriftAudit{}, domain.ErrPlanExecutionBaselineMissing
	}
	if err != nil {
		return domain.PlanExecutionSnapshot{}, domain.PlanDriftAudit{}, err
	}
	if originalSnapshotID == uuid.Nil {
		originalSnapshotID = latest.ID
	}
	if latest.ID != originalSnapshotID {
		return domain.PlanExecutionSnapshot{}, domain.PlanDriftAudit{}, domain.ErrVersionConflict
	}
	accepted, err := s.capturePlanExecutionSnapshotTx(ctx, tx, planID, domain.PlanSnapshotKindUserAccepted, nil, latest.GenerationProvider)
	if err != nil {
		return domain.PlanExecutionSnapshot{}, domain.PlanDriftAudit{}, err
	}
	audit, err := insertPlanDriftAuditTx(ctx, tx, projectID, planID, domain.PlanDriftAuditSnapshotUpdated,
		&originalSnapshotID, &accepted.ID, nil, rawDiff, channel, reason)
	if err != nil {
		return domain.PlanExecutionSnapshot{}, domain.PlanDriftAudit{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return domain.PlanExecutionSnapshot{}, domain.PlanDriftAudit{}, err
	}
	return accepted, audit, nil
}

func (s *Store) RecordPlanRegenerationAudit(ctx context.Context, planID, originalSnapshotID, targetPlanID uuid.UUID, rawDiff json.RawMessage, channel, reason string) (domain.PlanDriftAudit, error) {
	if err := validateDriftAuditInput(domain.PlanDriftAuditPlanRegenerated, channel, reason, rawDiff); err != nil {
		return domain.PlanDriftAudit{}, err
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return domain.PlanDriftAudit{}, err
	}
	defer tx.Rollback(ctx)
	var projectID, targetProjectID uuid.UUID
	if err = tx.QueryRow(ctx, `SELECT project_id FROM plans WHERE id=$1 FOR UPDATE`, planID).Scan(&projectID); errors.Is(err, pgx.ErrNoRows) {
		return domain.PlanDriftAudit{}, domain.ErrNotFound
	} else if err != nil {
		return domain.PlanDriftAudit{}, err
	}
	if err = tx.QueryRow(ctx, `SELECT project_id FROM plans WHERE id=$1`, targetPlanID).Scan(&targetProjectID); errors.Is(err, pgx.ErrNoRows) {
		return domain.PlanDriftAudit{}, domain.ErrNotFound
	} else if err != nil {
		return domain.PlanDriftAudit{}, err
	}
	if projectID != targetProjectID {
		return domain.PlanDriftAudit{}, domain.ErrInvalidPlanDriftAudit
	}
	var snapshotExists bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM plan_execution_snapshots WHERE id=$1 AND plan_id=$2)`, originalSnapshotID, planID).Scan(&snapshotExists); err != nil {
		return domain.PlanDriftAudit{}, err
	}
	if !snapshotExists {
		return domain.PlanDriftAudit{}, domain.ErrNotFound
	}
	audit, err := insertPlanDriftAuditTx(ctx, tx, projectID, planID, domain.PlanDriftAuditPlanRegenerated,
		&originalSnapshotID, nil, &targetPlanID, rawDiff, channel, reason)
	if err != nil {
		return domain.PlanDriftAudit{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return domain.PlanDriftAudit{}, err
	}
	return audit, nil
}

func (s *Store) RecordPlanExecutionAbandonedAudit(ctx context.Context, planID, originalSnapshotID uuid.UUID, rawDiff json.RawMessage, channel, reason string) (domain.PlanDriftAudit, error) {
	if err := validateDriftAuditInput(domain.PlanDriftAuditExecutionAbandoned, channel, reason, rawDiff); err != nil {
		return domain.PlanDriftAudit{}, err
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return domain.PlanDriftAudit{}, err
	}
	defer tx.Rollback(ctx)
	var projectID uuid.UUID
	if err = tx.QueryRow(ctx, `SELECT project_id FROM plans WHERE id=$1 FOR UPDATE`, planID).Scan(&projectID); errors.Is(err, pgx.ErrNoRows) {
		return domain.PlanDriftAudit{}, domain.ErrNotFound
	} else if err != nil {
		return domain.PlanDriftAudit{}, err
	}
	var snapshotExists bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM plan_execution_snapshots WHERE id=$1 AND plan_id=$2)`, originalSnapshotID, planID).Scan(&snapshotExists); err != nil {
		return domain.PlanDriftAudit{}, err
	}
	if !snapshotExists {
		return domain.PlanDriftAudit{}, domain.ErrNotFound
	}
	targetPlanID := planID
	audit, err := insertPlanDriftAuditTx(ctx, tx, projectID, planID, domain.PlanDriftAuditExecutionAbandoned,
		&originalSnapshotID, nil, &targetPlanID, rawDiff, channel, reason)
	if err != nil {
		return domain.PlanDriftAudit{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return domain.PlanDriftAudit{}, err
	}
	return audit, nil
}

func (s *Store) ListPlanDriftAudits(ctx context.Context, planID uuid.UUID) ([]domain.PlanDriftAudit, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+planDriftAuditColumns+` FROM plan_drift_audits WHERE plan_id=$1 ORDER BY sequence`, planID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []domain.PlanDriftAudit{}
	for rows.Next() {
		audit, scanErr := scanPlanDriftAudit(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		items = append(items, audit)
	}
	return items, rows.Err()
}

type UpdatePlanExecutionSnapshotParams struct {
	PlanID             uuid.UUID
	Version            int64
	OriginalSnapshotID uuid.UUID
	Title              string
	Spec               json.RawMessage
	Markdown           string
	ConfigSnapshot     json.RawMessage
	RawDiff            json.RawMessage
	Channel            string
	Reason             string
}

// UpdatePlanExecutionSnapshot replaces execution-relevant plan content, bumps
// both the optimistic resource version and the independent content version,
// then atomically records the user-accepted snapshot and its audit record.
func (s *Store) UpdatePlanExecutionSnapshot(ctx context.Context, params UpdatePlanExecutionSnapshotParams) (domain.Plan, domain.PlanExecutionSnapshot, domain.PlanDriftAudit, error) {
	if err := validateDriftAuditInput(domain.PlanDriftAuditSnapshotUpdated, params.Channel, params.Reason, params.RawDiff); err != nil {
		return domain.Plan{}, domain.PlanExecutionSnapshot{}, domain.PlanDriftAudit{}, err
	}
	if !json.Valid(params.Spec) || !json.Valid(params.ConfigSnapshot) {
		return domain.Plan{}, domain.PlanExecutionSnapshot{}, domain.PlanDriftAudit{}, domain.ErrInvalidPlanDriftAudit
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return domain.Plan{}, domain.PlanExecutionSnapshot{}, domain.PlanDriftAudit{}, err
	}
	defer tx.Rollback(ctx)
	latest, err := latestPlanExecutionSnapshot(ctx, tx, params.PlanID)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Plan{}, domain.PlanExecutionSnapshot{}, domain.PlanDriftAudit{}, domain.ErrPlanExecutionBaselineMissing
	}
	if err != nil {
		return domain.Plan{}, domain.PlanExecutionSnapshot{}, domain.PlanDriftAudit{}, err
	}
	originalSnapshotID := params.OriginalSnapshotID
	if originalSnapshotID == uuid.Nil {
		originalSnapshotID = latest.ID
	}
	if latest.ID != originalSnapshotID {
		return domain.Plan{}, domain.PlanExecutionSnapshot{}, domain.PlanDriftAudit{}, domain.ErrVersionConflict
	}
	var plan domain.Plan
	err = tx.QueryRow(ctx, `UPDATE plans SET title=$2,spec=$3,markdown=$4,config_snapshot=$5,
		updated_at=now(),version=version+1,content_version=content_version+1
		WHERE id=$1 AND version=$6 AND status IN ('ready','blocked','cancelled')
		RETURNING id,project_id,intake_id,title,spec,markdown,status,config_snapshot,created_at,updated_at,version,content_version`,
		params.PlanID, params.Title, params.Spec, params.Markdown, params.ConfigSnapshot, params.Version).Scan(
		&plan.ID, &plan.ProjectID, &plan.IntakeID, &plan.Title, &plan.Spec, &plan.Markdown,
		&plan.Status, &plan.ConfigSnapshot, &plan.CreatedAt, &plan.UpdatedAt, &plan.Version, &plan.ContentVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Plan{}, domain.PlanExecutionSnapshot{}, domain.PlanDriftAudit{}, s.versionOrNotFound(ctx, tx, "plans", params.PlanID)
	}
	if err != nil {
		return domain.Plan{}, domain.PlanExecutionSnapshot{}, domain.PlanDriftAudit{}, err
	}
	accepted, err := s.capturePlanExecutionSnapshotTx(ctx, tx, plan.ID, domain.PlanSnapshotKindUserAccepted, nil, latest.GenerationProvider)
	if err != nil {
		return domain.Plan{}, domain.PlanExecutionSnapshot{}, domain.PlanDriftAudit{}, err
	}
	audit, err := insertPlanDriftAuditTx(ctx, tx, plan.ProjectID, plan.ID, domain.PlanDriftAuditSnapshotUpdated,
		&originalSnapshotID, &accepted.ID, nil, params.RawDiff, params.Channel, params.Reason)
	if err != nil {
		return domain.Plan{}, domain.PlanExecutionSnapshot{}, domain.PlanDriftAudit{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return domain.Plan{}, domain.PlanExecutionSnapshot{}, domain.PlanDriftAudit{}, err
	}
	plan.ExecutionSnapshotID = &accepted.ID
	plan.ExecutionSnapshotSequence = accepted.Sequence
	plan.DriftStatus = domain.PlanDriftStatusClean
	plan.DriftResolutionRequired = false
	return plan, accepted, audit, nil
}

// LifecycleTransitionParams applies one state-machine transition and persists
// its explanation. ExpectedStatus and ExpectedVersion are optional optimistic
// concurrency guards. Replaying the current status is idempotent and does not
// overwrite metadata, increment the version, or append another audit row.
type LifecycleTransitionParams struct {
	ResourceType        domain.LifecycleResource
	ResourceID          uuid.UUID
	ExpectedStatus      string
	ExpectedVersion     int64
	Status              string
	StatusSource        domain.LifecycleStatusSource
	ReasonCode          domain.LifecycleReasonCode
	Reason              string
	LastActivityAt      time.Time
	RecoveryHint        domain.LifecycleRecoveryHint
	ExecutionCheckpoint json.RawMessage
	RelatedJobID        *uuid.UUID
	RelatedRunID        *uuid.UUID
}

type LifecycleTransitionResult struct {
	State      domain.LifecycleState       `json:"state"`
	Transition *domain.LifecycleTransition `json:"transition,omitempty"`
	Idempotent bool                        `json:"idempotent"`
}

// lifecycleFieldUpdate describes non-lifecycle columns that must change in the
// same UPDATE as the state migration. SQL is repository-owned and may contain
// one %s marker per argument; markers are replaced with positional parameters.
type lifecycleFieldUpdate struct {
	Column string
	SQL    string
	Args   []any
}

type lifecycleTransitionRequest struct {
	LifecycleTransitionParams
	ExpectedStatuses []string
	RequireWorkerID  *string
	AllowNonContract bool
	AllowTerminal    bool
	IgnoreTerminal   bool
	MismatchError    error
	Fields           []lifecycleFieldUpdate
}

func lifecycleTable(resource domain.LifecycleResource) (string, error) {
	switch resource {
	case domain.LifecycleResourcePlan:
		return "plans", nil
	case domain.LifecycleResourceTask:
		return "plan_tasks", nil
	case domain.LifecycleResourceJob:
		return "jobs", nil
	case domain.LifecycleResourceAgentRun:
		return "agent_runs", nil
	default:
		return "", domain.ErrInvalidTransition
	}
}

func scanLifecycleState(resource domain.LifecycleResource, row pgx.Row) (domain.LifecycleState, error) {
	state := domain.LifecycleState{ResourceType: resource}
	err := row.Scan(
		&state.ProjectID, &state.ResourceID, &state.Status, &state.Version,
		&state.StatusSource, &state.ReasonCode, &state.Reason, &state.LastActivityAt,
		&state.RecoveryHint, &state.ExecutionCheckpoint,
	)
	return state, err
}

func (s *Store) GetLifecycleState(ctx context.Context, resource domain.LifecycleResource, id uuid.UUID) (domain.LifecycleState, error) {
	table, err := lifecycleTable(resource)
	if err != nil {
		return domain.LifecycleState{}, err
	}
	state, err := scanLifecycleState(resource, s.Pool.QueryRow(ctx, fmt.Sprintf(`SELECT
		project_id,id,status,version,status_source,reason_code,reason,last_activity_at,
		recovery_hint,execution_checkpoint FROM %s WHERE id=$1`, table), id))
	return state, mapNotFound(err)
}

func scanLifecycleTransition(row pgx.Row) (domain.LifecycleTransition, error) {
	var transition domain.LifecycleTransition
	err := row.Scan(
		&transition.ID, &transition.ProjectID, &transition.ResourceType,
		&transition.ResourceID, &transition.ResourceVersion, &transition.FromStatus,
		&transition.ToStatus, &transition.StatusSource, &transition.ReasonCode,
		&transition.Reason, &transition.LastActivityAt, &transition.RecoveryHint,
		&transition.ExecutionCheckpoint, &transition.OccurredAt,
	)
	return transition, err
}

const lifecycleTransitionColumns = `id,project_id,resource_type,resource_id,resource_version,
	coalesce(from_status,''),to_status,status_source,reason_code,reason,last_activity_at,
	recovery_hint,execution_checkpoint,occurred_at`

func (s *Store) ListLifecycleTransitions(ctx context.Context, resource domain.LifecycleResource, id uuid.UUID) ([]domain.LifecycleTransition, error) {
	if _, err := lifecycleTable(resource); err != nil {
		return nil, err
	}
	rows, err := s.Pool.Query(ctx, `SELECT `+lifecycleTransitionColumns+`
		FROM lifecycle_transitions WHERE resource_type=$1 AND resource_id=$2 ORDER BY id`, resource, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]domain.LifecycleTransition, 0)
	for rows.Next() {
		transition, scanErr := scanLifecycleTransition(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		items = append(items, transition)
	}
	return items, rows.Err()
}

func lifecycleContains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func lifecycleCheckpoint(params LifecycleTransitionParams) (json.RawMessage, error) {
	checkpoint := map[string]any{}
	if len(params.ExecutionCheckpoint) > 0 {
		if err := json.Unmarshal(params.ExecutionCheckpoint, &checkpoint); err != nil || checkpoint == nil {
			return nil, domain.ErrInvalidLifecycleMetadata
		}
	}
	checkpoint["resourceId"] = params.ResourceID
	switch params.ResourceType {
	case domain.LifecycleResourceJob:
		checkpoint["jobId"] = params.ResourceID
	case domain.LifecycleResourceAgentRun:
		checkpoint["agentRunId"] = params.ResourceID
	}
	if params.RelatedJobID != nil {
		checkpoint["jobId"] = *params.RelatedJobID
	}
	if params.RelatedRunID != nil {
		checkpoint["agentRunId"] = *params.RelatedRunID
	}
	return json.Marshal(checkpoint)
}

func lifecycleFieldColumns(resource domain.LifecycleResource) map[string]struct{} {
	columns := map[string][]string{
		string(domain.LifecycleResourcePlan): {
			"execution_started_at", "delivery_status", "acceptance_summary",
		},
		string(domain.LifecycleResourceTask): {
			"session_id", "started_at", "finished_at", "acceptance_status", "acceptance_result",
		},
		string(domain.LifecycleResourceJob): {
			"worker_id", "lease_expires_at", "run_after", "attempt", "last_error",
		},
		string(domain.LifecycleResourceAgentRun): {
			"exit_code", "session_id", "session_mode", "session_invalidation_reason",
			"termination_reason", "failure_category", "duration_ms", "output_bytes",
			"output_lines", "event_count", "output_truncated", "input_tokens",
			"output_tokens", "total_tokens", "cost_amount", "cost_currency", "finished_at",
		},
	}
	allowed := make(map[string]struct{}, len(columns[string(resource)]))
	for _, column := range columns[string(resource)] {
		allowed[column] = struct{}{}
	}
	return allowed
}

func renderLifecycleFields(resource domain.LifecycleResource, fields []lifecycleFieldUpdate, args *[]any) (string, error) {
	allowed := lifecycleFieldColumns(resource)
	var builder strings.Builder
	for _, field := range fields {
		if _, ok := allowed[field.Column]; !ok {
			return "", fmt.Errorf("unsupported lifecycle field %q: %w", field.Column, domain.ErrInvalidTransition)
		}
		expression := field.SQL
		if expression == "" {
			expression = "%s"
		}
		for _, value := range field.Args {
			marker := fmt.Sprintf("$%d", len(*args)+1)
			if !strings.Contains(expression, "%s") {
				return "", fmt.Errorf("missing lifecycle field placeholder for %q: %w", field.Column, domain.ErrInvalidTransition)
			}
			expression = strings.Replace(expression, "%s", marker, 1)
			*args = append(*args, value)
		}
		if strings.Contains(expression, "%s") {
			return "", fmt.Errorf("unbound lifecycle field placeholder for %q: %w", field.Column, domain.ErrInvalidTransition)
		}
		builder.WriteString(",")
		builder.WriteString(field.Column)
		builder.WriteString("=")
		builder.WriteString(expression)
	}
	return builder.String(), nil
}

// transitionLifecycleTx is the single write gate for plan, task, job, and
// Agent Run status changes. It locks the current row, protects durable terminal
// states, writes lifecycle metadata and related execution identifiers, and
// relies on the lifecycle trigger to append the immutable audit row in the same
// transaction.
func transitionLifecycleTx(ctx context.Context, tx pgx.Tx, request lifecycleTransitionRequest) (LifecycleTransitionResult, error) {
	table, err := lifecycleTable(request.ResourceType)
	if err != nil {
		return LifecycleTransitionResult{}, err
	}
	current, err := scanLifecycleState(request.ResourceType, tx.QueryRow(ctx, fmt.Sprintf(`SELECT
		project_id,id,status,version,status_source,reason_code,reason,last_activity_at,
		recovery_hint,execution_checkpoint FROM %s WHERE id=$1 FOR UPDATE`, table), request.ResourceID))
	if errors.Is(err, pgx.ErrNoRows) {
		return LifecycleTransitionResult{}, domain.ErrNotFound
	}
	if err != nil {
		return LifecycleTransitionResult{}, err
	}
	if current.Status == request.Status {
		return LifecycleTransitionResult{State: current, Idempotent: true}, nil
	}
	if domain.IsTerminalStatus(request.ResourceType, current.Status) && !request.AllowTerminal {
		if request.IgnoreTerminal {
			return LifecycleTransitionResult{State: current, Idempotent: true}, nil
		}
		if !domain.IsRecoverableTerminalStatus(request.ResourceType, current.Status) {
			return LifecycleTransitionResult{}, domain.ErrInvalidTransition
		}
	}
	if request.ExpectedVersion > 0 && current.Version != request.ExpectedVersion {
		return LifecycleTransitionResult{}, domain.ErrVersionConflict
	}
	expected := request.ExpectedStatuses
	if request.ExpectedStatus != "" {
		expected = []string{request.ExpectedStatus}
	}
	if len(expected) > 0 && !lifecycleContains(expected, current.Status) {
		if request.MismatchError != nil {
			return LifecycleTransitionResult{}, request.MismatchError
		}
		return LifecycleTransitionResult{}, domain.ErrInvalidTransition
	}
	if request.RequireWorkerID != nil {
		if request.ResourceType != domain.LifecycleResourceJob {
			return LifecycleTransitionResult{}, domain.ErrInvalidTransition
		}
		var workerID string
		if err = tx.QueryRow(ctx, `SELECT coalesce(worker_id,'') FROM jobs WHERE id=$1`, request.ResourceID).Scan(&workerID); err != nil {
			return LifecycleTransitionResult{}, err
		}
		if workerID != *request.RequireWorkerID {
			if request.MismatchError != nil {
				return LifecycleTransitionResult{}, request.MismatchError
			}
			return LifecycleTransitionResult{}, domain.ErrInvalidTransition
		}
	}
	if _, err = domain.EvaluateTransition(string(request.ResourceType), current.Status, request.Status); err != nil && !request.AllowNonContract {
		return LifecycleTransitionResult{}, err
	}

	checkpoint, err := lifecycleCheckpoint(request.LifecycleTransitionParams)
	if err != nil {
		return LifecycleTransitionResult{}, err
	}
	metadata := domain.LifecycleMetadata{
		StatusSource:        request.StatusSource,
		ReasonCode:          request.ReasonCode,
		Reason:              strings.TrimSpace(request.Reason),
		LastActivityAt:      request.LastActivityAt,
		RecoveryHint:        request.RecoveryHint,
		ExecutionCheckpoint: checkpoint,
	}
	if metadata.LastActivityAt.IsZero() {
		metadata.LastActivityAt = time.Now().UTC()
	}
	if err = domain.ValidateLifecycleMetadata(metadata); err != nil {
		return LifecycleTransitionResult{}, err
	}

	args := []any{
		request.ResourceID, request.Status, metadata.StatusSource, metadata.ReasonCode,
		metadata.Reason, metadata.LastActivityAt, metadata.RecoveryHint, metadata.ExecutionCheckpoint,
	}
	extraSet, err := renderLifecycleFields(request.ResourceType, request.Fields, &args)
	if err != nil {
		return LifecycleTransitionResult{}, err
	}
	updated, err := scanLifecycleState(request.ResourceType, tx.QueryRow(ctx, fmt.Sprintf(`UPDATE %s SET
		status=$2,status_source=$3,reason_code=$4,reason=$5,last_activity_at=$6,
		recovery_hint=$7,execution_checkpoint=$8%s,updated_at=now(),version=version+1
		WHERE id=$1 AND status=%s RETURNING project_id,id,status,version,status_source,
		reason_code,reason,last_activity_at,recovery_hint,execution_checkpoint`, table, extraSet, fmt.Sprintf("$%d", len(args)+1)), append(args, current.Status)...))
	if errors.Is(err, pgx.ErrNoRows) {
		if request.MismatchError != nil {
			return LifecycleTransitionResult{}, request.MismatchError
		}
		return LifecycleTransitionResult{}, domain.ErrInvalidTransition
	}
	if err != nil {
		return LifecycleTransitionResult{}, err
	}
	transition, err := scanLifecycleTransition(tx.QueryRow(ctx, `SELECT `+lifecycleTransitionColumns+`
		FROM lifecycle_transitions WHERE resource_type=$1 AND resource_id=$2 AND resource_version=$3
		ORDER BY id DESC LIMIT 1`, request.ResourceType, request.ResourceID, updated.Version))
	if err != nil {
		return LifecycleTransitionResult{}, err
	}
	return LifecycleTransitionResult{State: updated, Transition: &transition}, nil
}

func (s *Store) TransitionLifecycle(ctx context.Context, params LifecycleTransitionParams) (LifecycleTransitionResult, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return LifecycleTransitionResult{}, err
	}
	defer tx.Rollback(ctx)
	result, err := transitionLifecycleTx(ctx, tx, lifecycleTransitionRequest{LifecycleTransitionParams: params})
	if err != nil {
		return LifecycleTransitionResult{}, err
	}
	if result.Idempotent {
		return result, nil
	}
	if err = tx.Commit(ctx); err != nil {
		return LifecycleTransitionResult{}, err
	}
	return result, nil
}
