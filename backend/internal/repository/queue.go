package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/lyming99/specrelay/backend/internal/domain"
	"github.com/lyming99/specrelay/backend/internal/planspec"
)

func scanJob(row pgx.Row) (domain.Job, error) {
	var j domain.Job
	err := row.Scan(&j.ID, &j.ProjectID, &j.Type, &j.AggregateType, &j.AggregateID, &j.Payload, &j.Priority, &j.Status, &j.RunAfter, &j.WorkerID, &j.LeaseExpiresAt, &j.Attempt, &j.MaxAttempts, &j.LastError, &j.IdempotencyKey, &j.CreatedAt, &j.UpdatedAt, &j.Version)
	return j, err
}
func (s *Store) ClaimJob(ctx context.Context, workerID string, lease time.Duration) (domain.Job, error) {
	return scanJob(s.Pool.QueryRow(ctx, `WITH candidate AS (SELECT j.id FROM jobs j JOIN projects p ON p.id=j.project_id WHERE j.status IN ('queued','retry_wait') AND j.run_after<=now() AND p.automation_enabled=true ORDER BY j.priority ASC,j.created_at ASC FOR UPDATE OF j SKIP LOCKED LIMIT 1) UPDATE jobs j SET status='leased',worker_id=$1,lease_expires_at=now()+$2::interval,attempt=attempt+1,updated_at=now(),version=version+1 FROM candidate WHERE j.id=candidate.id RETURNING j.id,j.project_id,j.job_type,j.aggregate_type,j.aggregate_id,j.payload,j.priority,j.status,j.run_after,coalesce(j.worker_id,''),j.lease_expires_at,j.attempt,j.max_attempts,coalesce(j.last_error,''),j.idempotency_key,j.created_at,j.updated_at,j.version`, workerID, lease.String()))
}
func (s *Store) MarkJobRunning(ctx context.Context, id uuid.UUID, workerID string) (domain.Job, error) {
	j, err := scanJob(s.Pool.QueryRow(ctx, `UPDATE jobs SET status='running',updated_at=now(),version=version+1 WHERE id=$1 AND worker_id=$2 AND status='leased' RETURNING id,project_id,job_type,aggregate_type,aggregate_id,payload,priority,status,run_after,coalesce(worker_id,''),lease_expires_at,attempt,max_attempts,coalesce(last_error,''),idempotency_key,created_at,updated_at,version`, id, workerID))
	return j, mapNotFound(err)
}
func (s *Store) CompleteJob(ctx context.Context, id uuid.UUID, workerID string) error {
	tag, err := s.Pool.Exec(ctx, `UPDATE jobs SET status='succeeded',lease_expires_at=NULL,updated_at=now(),version=version+1 WHERE id=$1 AND worker_id=$2 AND status='running'`, id, workerID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}
func (s *Store) FailJob(ctx context.Context, j domain.Job, workerID, message string, retryable bool) error {
	status := "failed"
	runAfter := time.Now()
	if retryable && j.Attempt < j.MaxAttempts {
		status = "retry_wait"
		delay := time.Duration(1<<min(j.Attempt, 6)) * time.Second
		runAfter = runAfter.Add(delay)
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, `UPDATE jobs SET status=$3,last_error=$4,run_after=$5,lease_expires_at=NULL,updated_at=now(),version=version+1 WHERE id=$1 AND worker_id=$2 AND status IN ('leased','running')`, j.ID, workerID, status, message, runAfter)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	if status == "retry_wait" {
		_, err = tx.Exec(ctx, `SELECT pg_notify('specrelay_jobs',$1)`, j.ID.String())
	}
	if status == "failed" && j.Type == "plan.generate" {
		var projectID uuid.UUID
		var version int64
		if scanErr := tx.QueryRow(ctx, `UPDATE intakes SET status='plan_failed',updated_at=now(),version=version+1 WHERE id=$1 AND status='planning' RETURNING project_id,version`, j.AggregateID).Scan(&projectID, &version); scanErr == nil {
			_, err = insertEvent(ctx, tx, NewEvent{ProjectID: &projectID, Type: "intake.plan_failed", AggregateType: "intake", AggregateID: j.AggregateID, ResourceVersion: version, Payload: mustJSON(map[string]any{"error": message})})
		} else if !errors.Is(scanErr, pgx.ErrNoRows) {
			return scanErr
		}
	}
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (s *Store) RecoverJobs(ctx context.Context) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	_, err = tx.Exec(ctx, `UPDATE jobs SET status='queued',worker_id=NULL,lease_expires_at=NULL,run_after=now(),updated_at=now(),version=version+1 WHERE status IN ('leased','running') AND lease_expires_at<now()`)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `DELETE FROM workspace_leases WHERE expires_at<now()`)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (s *Store) AcquireWorkspaceLease(ctx context.Context, projectID, jobID uuid.UUID, workspace, workerID string, duration time.Duration) error {
	id := uuid.New()
	tag, err := s.Pool.Exec(ctx, `INSERT INTO workspace_leases(id,project_id,workspace_path_normalized,worker_id,job_id,expires_at) VALUES($1,$2,$3,$4,$5,now()+$6::interval) ON CONFLICT(workspace_path_normalized) DO UPDATE SET id=EXCLUDED.id,project_id=EXCLUDED.project_id,worker_id=EXCLUDED.worker_id,job_id=EXCLUDED.job_id,heartbeat_at=now(),expires_at=EXCLUDED.expires_at,updated_at=now(),version=workspace_leases.version+1 WHERE workspace_leases.expires_at<now() OR workspace_leases.job_id=EXCLUDED.job_id`, id, projectID, workspace, workerID, jobID, duration.String())
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errors.New("workspace is leased by another worker")
	}
	return nil
}
func (s *Store) RenewWorkspaceLease(ctx context.Context, jobID uuid.UUID, workerID string, duration time.Duration) error {
	tag, err := s.Pool.Exec(ctx, `UPDATE workspace_leases SET heartbeat_at=now(),expires_at=now()+$3::interval,updated_at=now(),version=version+1 WHERE job_id=$1 AND worker_id=$2 AND expires_at>now()`, jobID, workerID, duration.String())
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errors.New("workspace lease lost")
	}
	_, err = s.Pool.Exec(ctx, `UPDATE jobs SET lease_expires_at=now()+$3::interval,updated_at=now() WHERE id=$1 AND worker_id=$2 AND status IN ('leased','running')`, jobID, workerID, duration.String())
	return err
}
func (s *Store) ReleaseWorkspaceLease(ctx context.Context, jobID uuid.UUID, workerID string) error {
	_, err := s.Pool.Exec(ctx, `DELETE FROM workspace_leases WHERE job_id=$1 AND worker_id=$2`, jobID, workerID)
	return err
}

func (s *Store) SaveGeneratedPlan(ctx context.Context, intake domain.Intake, spec planspec.Spec, markdown string) (domain.Plan, []domain.PlanTask, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return domain.Plan{}, nil, err
	}
	defer tx.Rollback(ctx)
	planID := uuid.New()
	specJSON, _ := json.Marshal(spec)
	var p domain.Plan
	err = tx.QueryRow(ctx, `INSERT INTO plans(id,project_id,intake_id,title,spec,markdown,status,config_snapshot) VALUES($1,$2,$3,$4,$5,$6,'generating',$7) RETURNING id,project_id,intake_id,title,spec,markdown,status,config_snapshot,created_at,updated_at,version`, planID, intake.ProjectID, intake.ID, spec.Title, specJSON, markdown, intake.ConfigSnapshot).Scan(&p.ID, &p.ProjectID, &p.IntakeID, &p.Title, &p.Spec, &p.Markdown, &p.Status, &p.ConfigSnapshot, &p.CreatedAt, &p.UpdatedAt, &p.Version)
	if err != nil {
		return p, nil, err
	}
	tasks := []domain.PlanTask{}
	for _, item := range planspec.Tasks(spec) {
		id := uuid.New()
		scope, _ := json.Marshal(item.Task.Scope)
		acceptance, _ := json.Marshal(item.Task.Acceptance)
		t, err := scanTask(tx.QueryRow(ctx, `INSERT INTO plan_tasks(id,project_id,plan_id,task_key,position,title,scope,acceptance) VALUES($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id,project_id,plan_id,task_key,position,title,scope,acceptance,status,coalesce(session_id,''),started_at,finished_at,created_at,updated_at,version`, id, intake.ProjectID, planID, item.Key, item.Position, item.Task.Title, scope, acceptance))
		if err != nil {
			return p, nil, err
		}
		tasks = append(tasks, t)
	}
	err = tx.QueryRow(ctx, `UPDATE plans SET status='ready',updated_at=now(),version=version+1 WHERE id=$1 AND status='generating' RETURNING status,updated_at,version`, p.ID).Scan(&p.Status, &p.UpdatedAt, &p.Version)
	if err != nil {
		return p, nil, err
	}
	var intakeVersion int64
	err = tx.QueryRow(ctx, `UPDATE intakes SET status='planned',updated_at=now(),version=version+1 WHERE id=$1 AND status='planning' RETURNING version`, intake.ID).Scan(&intakeVersion)
	if err != nil {
		return p, nil, err
	}
	_, err = insertEvent(ctx, tx, NewEvent{ProjectID: &p.ProjectID, Type: "plan.ready", AggregateType: "plan", AggregateID: p.ID, ResourceVersion: p.Version, Payload: mustJSON(map[string]any{"intakeId": intake.ID, "taskCount": len(tasks)})})
	if err != nil {
		return p, nil, err
	}
	_, err = insertEvent(ctx, tx, NewEvent{ProjectID: &p.ProjectID, Type: "intake.planned", AggregateType: "intake", AggregateID: intake.ID, ResourceVersion: intakeVersion, Payload: mustJSON(map[string]any{"planId": p.ID})})
	if err != nil {
		return p, nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return p, nil, err
	}
	return p, tasks, nil
}

func (s *Store) QueuePlan(ctx context.Context, planID uuid.UUID, version int64) (domain.Job, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return domain.Job{}, err
	}
	defer tx.Rollback(ctx)
	job, err := s.queuePlanTx(ctx, tx, planID, version, false)
	if err != nil {
		return domain.Job{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return domain.Job{}, err
	}
	return job, nil
}

// QueuePlanAutomatically queues a newly ready plan only while its project is
// still automated. It locks both records so stopping automation cannot race
// with the task job being created.
func (s *Store) QueuePlanAutomatically(ctx context.Context, planID uuid.UUID) (domain.Job, bool, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return domain.Job{}, false, err
	}
	defer tx.Rollback(ctx)

	var version int64
	err = tx.QueryRow(ctx, `SELECT p.version
		FROM plans p
		JOIN projects pr ON pr.id=p.project_id
		WHERE p.id=$1 AND p.status='ready' AND pr.automation_enabled=true
		FOR UPDATE OF p, pr`, planID).Scan(&version)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Job{}, false, nil
	}
	if err != nil {
		return domain.Job{}, false, err
	}
	job, err := s.queuePlanTx(ctx, tx, planID, version, true)
	if err != nil {
		return domain.Job{}, false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return domain.Job{}, false, err
	}
	return job, true, nil
}

// queuePlanTx moves a plan to running, queues its next task, and creates the
// corresponding task job. automatic limits the transition to ready plans so
// automation never restarts a blocked plan without user action.
func (s *Store) queuePlanTx(ctx context.Context, tx pgx.Tx, planID uuid.UUID, version int64, automatic bool) (domain.Job, error) {
	planQuery := `UPDATE plans SET status='running',updated_at=now(),version=version+1 WHERE id=$1 AND version=$2 AND status IN ('ready','blocked') RETURNING project_id,version,config_snapshot`
	if automatic {
		planQuery = `UPDATE plans SET status='running',updated_at=now(),version=version+1 WHERE id=$1 AND version=$2 AND status='ready' RETURNING project_id,version,config_snapshot`
	}
	var projectID uuid.UUID
	var nextVersion int64
	var configSnapshot json.RawMessage
	err := tx.QueryRow(ctx, planQuery, planID, version).Scan(&projectID, &nextVersion, &configSnapshot)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Job{}, s.versionOrNotFound(ctx, tx, "plans", planID)
	}
	if err != nil {
		return domain.Job{}, err
	}
	provider, providerRequested := executionProviderFromContext(ctx)
	if automatic {
		provider = ""
		providerRequested = false
	}
	configSnapshot, err = planConfigSnapshotWithProvider(configSnapshot, provider)
	if err != nil {
		return domain.Job{}, err
	}
	if _, err = tx.Exec(ctx, `UPDATE plans SET config_snapshot=$2 WHERE id=$1`, planID, configSnapshot); err != nil {
		return domain.Job{}, err
	}
	var taskID uuid.UUID
	var taskVersion int64
	err = tx.QueryRow(ctx, `UPDATE plan_tasks SET status='queued',updated_at=now(),version=version+1 WHERE id=(SELECT id FROM plan_tasks WHERE plan_id=$1 AND status IN ('pending','failed','cancelled') ORDER BY position LIMIT 1 FOR UPDATE) RETURNING id,version`, planID).Scan(&taskID, &taskVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Job{}, errors.New("plan has no runnable task")
	}
	if err != nil {
		return domain.Job{}, err
	}
	maxAttempts, err := projectMaxAttempts(ctx, tx, projectID)
	if err != nil {
		return domain.Job{}, err
	}
	job, err := insertJob(ctx, tx, NewJob{ID: uuid.New(), ProjectID: projectID, Type: "task.execute", AggregateType: "task", AggregateID: taskID, Payload: taskExecutionJobPayload(taskID, planID, provider, providerRequested), Priority: 100, MaxAttempts: maxAttempts, RunAfter: time.Now(), IdempotencyKey: fmt.Sprintf("task.execute:%s:%d", taskID, taskVersion)})
	if err != nil {
		return domain.Job{}, err
	}
	_, err = insertEvent(ctx, tx, NewEvent{ProjectID: &projectID, Type: "plan.running", AggregateType: "plan", AggregateID: planID, ResourceVersion: nextVersion, Payload: mustJSON(map[string]any{"jobId": job.ID, "taskId": taskID})})
	if err != nil {
		return domain.Job{}, err
	}
	return job, nil
}

func (s *Store) QueueTask(ctx context.Context, taskID uuid.UUID, version int64) (domain.Job, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return domain.Job{}, err
	}
	defer tx.Rollback(ctx)
	t, err := scanTask(tx.QueryRow(ctx, `UPDATE plan_tasks target SET status='queued',updated_at=now(),version=target.version+1 WHERE target.id=$1 AND target.version=$2 AND target.status IN ('pending','failed','cancelled') AND NOT EXISTS (SELECT 1 FROM plan_tasks earlier WHERE earlier.plan_id=target.plan_id AND earlier.position<target.position AND earlier.status<>'succeeded') RETURNING target.id,target.project_id,target.plan_id,target.task_key,target.position,target.title,target.scope,target.acceptance,target.status,coalesce(target.session_id,''),target.started_at,target.finished_at,target.created_at,target.updated_at,target.version`, taskID, version))
	if errors.Is(err, pgx.ErrNoRows) {
		var exists bool
		if checkErr := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM plan_tasks WHERE id=$1)`, taskID).Scan(&exists); checkErr != nil {
			return domain.Job{}, checkErr
		}
		if !exists {
			return domain.Job{}, domain.ErrNotFound
		}
		var currentVersion int64
		if checkErr := tx.QueryRow(ctx, `SELECT version FROM plan_tasks WHERE id=$1`, taskID).Scan(&currentVersion); checkErr != nil {
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
	var planVersion int64
	planChanged := true
	err = tx.QueryRow(ctx, `UPDATE plans SET status='running',updated_at=now(),version=version+1 WHERE id=$1 AND status IN ('ready','blocked') RETURNING version`, t.PlanID).Scan(&planVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		planChanged = false
		var status string
		if err = tx.QueryRow(ctx, `SELECT status FROM plans WHERE id=$1`, t.PlanID).Scan(&status); err != nil {
			return domain.Job{}, err
		}
		if status != "running" {
			return domain.Job{}, domain.ErrInvalidTransition
		}
	} else if err != nil {
		return domain.Job{}, err
	}
	maxAttempts, err := projectMaxAttempts(ctx, tx, t.ProjectID)
	if err != nil {
		return domain.Job{}, err
	}
	provider, providerRequested := executionProviderFromContext(ctx)
	job, err := insertJob(ctx, tx, NewJob{ID: uuid.New(), ProjectID: t.ProjectID, Type: "task.execute", AggregateType: "task", AggregateID: t.ID, Payload: taskExecutionJobPayload(t.ID, t.PlanID, provider, providerRequested), Priority: 100, MaxAttempts: maxAttempts, RunAfter: time.Now(), IdempotencyKey: fmt.Sprintf("task.execute:%s:%d", t.ID, t.Version)})
	if err != nil {
		return domain.Job{}, err
	}
	if _, err = insertEvent(ctx, tx, NewEvent{ProjectID: &t.ProjectID, Type: "task.queued", AggregateType: "task", AggregateID: t.ID, ResourceVersion: t.Version, Payload: mustJSON(map[string]any{"jobId": job.ID})}); err != nil {
		return domain.Job{}, err
	}
	if planChanged {
		if _, err = insertEvent(ctx, tx, NewEvent{ProjectID: &t.ProjectID, Type: "plan.running", AggregateType: "plan", AggregateID: t.PlanID, ResourceVersion: planVersion, Payload: mustJSON(map[string]any{"jobId": job.ID, "taskId": t.ID})}); err != nil {
			return domain.Job{}, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return domain.Job{}, err
	}
	return job, nil
}

func (s *Store) StartTask(ctx context.Context, taskID uuid.UUID) (domain.PlanTask, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return domain.PlanTask{}, err
	}
	defer tx.Rollback(ctx)
	t, err := scanTask(tx.QueryRow(ctx, `UPDATE plan_tasks SET status='running',started_at=now(),updated_at=now(),version=version+1 WHERE id=$1 AND status='queued' RETURNING id,project_id,plan_id,task_key,position,title,scope,acceptance,status,coalesce(session_id,''),started_at,finished_at,created_at,updated_at,version`, taskID))
	if err != nil {
		return t, mapNotFound(err)
	}
	if t.Title == "Final validation" {
		var version int64
		if err = tx.QueryRow(ctx, `UPDATE plans SET status='validating',updated_at=now(),version=version+1 WHERE id=$1 AND status='running' RETURNING version`, t.PlanID).Scan(&version); err != nil {
			return t, err
		}
		if _, err = insertEvent(ctx, tx, NewEvent{ProjectID: &t.ProjectID, Type: "plan.validating", AggregateType: "plan", AggregateID: t.PlanID, ResourceVersion: version, Payload: json.RawMessage(`{}`)}); err != nil {
			return t, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return t, err
	}
	return t, nil
}
func (s *Store) FinishTask(ctx context.Context, t domain.PlanTask, sessionID string, success bool, message string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	status := "failed"
	eventType := "task.failed"
	if success {
		status = "succeeded"
		eventType = "task.succeeded"
	}
	var nextVersion int64
	err = tx.QueryRow(ctx, `UPDATE plan_tasks SET status=$2,session_id=nullif($3,''),finished_at=now(),updated_at=now(),version=version+1 WHERE id=$1 AND status='running' RETURNING version`, t.ID, status, sessionID).Scan(&nextVersion)
	if err != nil {
		return err
	}
	if _, err = insertEvent(ctx, tx, NewEvent{ProjectID: &t.ProjectID, Type: eventType, AggregateType: "task", AggregateID: t.ID, ResourceVersion: nextVersion, Payload: mustJSON(map[string]any{"message": message})}); err != nil {
		return err
	}
	if success {
		var planStatus string
		var automationEnabled bool
		var configSnapshot json.RawMessage
		if err = tx.QueryRow(ctx, `SELECT p.status,pr.automation_enabled,p.config_snapshot FROM plans p JOIN projects pr ON pr.id=p.project_id WHERE p.id=$1 FOR UPDATE OF p`, t.PlanID).Scan(&planStatus, &automationEnabled, &configSnapshot); err != nil {
			return err
		}
		if planStatus == "cancelled" {
			return tx.Commit(ctx)
		}
		if !automationEnabled {
			var planVersion int64
			if err = tx.QueryRow(ctx, `UPDATE plans SET status='cancelled',updated_at=now(),version=version+1 WHERE id=$1 AND status IN ('running','validating') RETURNING version`, t.PlanID).Scan(&planVersion); errors.Is(err, pgx.ErrNoRows) {
				err = nil
			} else if err == nil {
				_, err = insertEvent(ctx, tx, NewEvent{ProjectID: &t.ProjectID, Type: "plan.cancelled", AggregateType: "plan", AggregateID: t.PlanID, ResourceVersion: planVersion, Payload: mustJSON(map[string]any{"reason": "project automation stopped"})})
			}
			if err != nil {
				return err
			}
			return tx.Commit(ctx)
		}
		provider, providerErr := providerFromPlanConfigSnapshot(configSnapshot)
		if providerErr != nil {
			return providerErr
		}
		var nextID uuid.UUID
		var nextTaskVersion int64
		err = tx.QueryRow(ctx, `UPDATE plan_tasks SET status='queued',updated_at=now(),version=version+1 WHERE id=(SELECT id FROM plan_tasks WHERE plan_id=$1 AND status='pending' ORDER BY position LIMIT 1 FOR UPDATE) RETURNING id,version`, t.PlanID).Scan(&nextID, &nextTaskVersion)
		if err == nil {
			maxAttempts, attemptsErr := projectMaxAttempts(ctx, tx, t.ProjectID)
			if attemptsErr != nil {
				return attemptsErr
			}
			job, insertErr := insertJob(ctx, tx, NewJob{ID: uuid.New(), ProjectID: t.ProjectID, Type: "task.execute", AggregateType: "task", AggregateID: nextID, Payload: taskExecutionJobPayload(nextID, t.PlanID, provider, false), Priority: 100, MaxAttempts: maxAttempts, RunAfter: time.Now(), IdempotencyKey: fmt.Sprintf("task.execute:%s:%d", nextID, nextTaskVersion)})
			if insertErr != nil {
				return insertErr
			}
			if _, err = insertEvent(ctx, tx, NewEvent{ProjectID: &t.ProjectID, Type: "task.queued", AggregateType: "task", AggregateID: nextID, ResourceVersion: nextTaskVersion, Payload: mustJSON(map[string]any{"jobId": job.ID})}); err != nil {
				return err
			}
		} else if errors.Is(err, pgx.ErrNoRows) {
			var planVersion int64
			var intakeID uuid.UUID
			err = tx.QueryRow(ctx, `UPDATE plans SET status='completed',updated_at=now(),version=version+1 WHERE id=$1 AND status='validating' RETURNING version,intake_id`, t.PlanID).Scan(&planVersion, &intakeID)
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrInvalidTransition
			}
			if err != nil {
				return err
			}
			if _, err = insertEvent(ctx, tx, NewEvent{ProjectID: &t.ProjectID, Type: "plan.completed", AggregateType: "plan", AggregateID: t.PlanID, ResourceVersion: planVersion, Payload: json.RawMessage(`{}`)}); err != nil {
				return err
			}
			var intakeVersion int64
			err = tx.QueryRow(ctx, `UPDATE intakes SET status='closed',updated_at=now(),version=version+1 WHERE id=$1 AND status='planned' RETURNING version`, intakeID).Scan(&intakeVersion)
			if errors.Is(err, pgx.ErrNoRows) {
				err = nil
			} else if err == nil {
				_, err = insertEvent(ctx, tx, NewEvent{ProjectID: &t.ProjectID, Type: "intake.closed", AggregateType: "intake", AggregateID: intakeID, ResourceVersion: intakeVersion, Payload: mustJSON(map[string]any{"planId": t.PlanID})})
			}
		}
		if err != nil {
			return err
		}
	} else {
		var planVersion int64
		if err = tx.QueryRow(ctx, `UPDATE plans SET status='blocked',updated_at=now(),version=version+1 WHERE id=$1 AND status IN ('running','validating') RETURNING version`, t.PlanID).Scan(&planVersion); err != nil {
			return err
		}
		if _, err = insertEvent(ctx, tx, NewEvent{ProjectID: &t.ProjectID, Type: "plan.blocked", AggregateType: "plan", AggregateID: t.PlanID, ResourceVersion: planVersion, Payload: mustJSON(map[string]any{"taskId": t.ID, "message": message})}); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
