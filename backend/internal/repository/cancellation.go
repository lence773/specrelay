package repository

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/lyming99/specrelay/backend/internal/domain"
)

func (s *Store) CancelJob(ctx context.Context, id uuid.UUID, workerID string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	result, err := transitionLifecycleTx(ctx, tx, lifecycleTransitionRequest{
		LifecycleTransitionParams: LifecycleTransitionParams{
			ResourceType:        domain.LifecycleResourceJob,
			ResourceID:          id,
			Status:              "cancelled",
			StatusSource:        domain.LifecycleSourceWorker,
			ReasonCode:          domain.LifecycleReasonUserCancelled,
			Reason:              "worker acknowledged job cancellation",
			RecoveryHint:        domain.LifecycleRecoveryNone,
			ExecutionCheckpoint: mustJSON(map[string]any{"workerId": workerID}),
		},
		ExpectedStatuses: []string{"leased", "running"},
		RequireWorkerID:  &workerID,
		AllowNonContract: true,
		IgnoreTerminal:   true,
		MismatchError:    domain.ErrNotFound,
		Fields:           []lifecycleFieldUpdate{{Column: "lease_expires_at", SQL: "NULL"}},
	})
	if err != nil {
		return err
	}
	if result.Idempotent {
		return nil
	}
	return tx.Commit(ctx)
}

func currentTaskJobID(ctx context.Context, tx pgx.Tx, taskID uuid.UUID) (*uuid.UUID, error) {
	var jobID uuid.UUID
	err := tx.QueryRow(ctx, `SELECT id FROM jobs WHERE aggregate_type='task' AND aggregate_id=$1 ORDER BY created_at DESC LIMIT 1`, taskID).Scan(&jobID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &jobID, nil
}

func (s *Store) ReturnTaskPending(ctx context.Context, t domain.PlanTask, message string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	jobID, err := currentTaskJobID(ctx, tx, t.ID)
	if err != nil {
		return err
	}
	result, err := transitionLifecycleTx(ctx, tx, lifecycleTransitionRequest{
		LifecycleTransitionParams: LifecycleTransitionParams{
			ResourceType: domain.LifecycleResourceTask,
			ResourceID:   t.ID,
			Status:       "pending",
			StatusSource: domain.LifecycleSourceWorker,
			ReasonCode:   domain.LifecycleReasonUserCancelled,
			Reason:       message,
			RecoveryHint: domain.LifecycleRecoveryRetryFromStart,
			RelatedJobID: jobID,
		},
		ExpectedStatuses: []string{"running"},
		AllowNonContract: true,
		IgnoreTerminal:   true,
		MismatchError:    nil,
		Fields: []lifecycleFieldUpdate{
			{Column: "started_at", SQL: "NULL"},
			{Column: "finished_at", SQL: "NULL"},
		},
	})
	if errors.Is(err, domain.ErrInvalidTransition) {
		return nil
	}
	if err != nil {
		return err
	}
	if result.Idempotent {
		return nil
	}
	if _, err = insertEvent(ctx, tx, NewEvent{ProjectID: &t.ProjectID, Type: "task.cancelled", AggregateType: "task", AggregateID: t.ID, ResourceVersion: result.State.Version, Payload: mustJSON(map[string]any{"message": message})}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ReturnTaskQueuedForRetry keeps a task runnable while its existing job waits
// for an automatic retry. It deliberately does not create a second job.
func (s *Store) ReturnTaskQueuedForRetry(ctx context.Context, t domain.PlanTask, sessionID, message string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	jobID, err := currentTaskJobID(ctx, tx, t.ID)
	if err != nil {
		return err
	}
	result, err := transitionLifecycleTx(ctx, tx, lifecycleTransitionRequest{
		LifecycleTransitionParams: LifecycleTransitionParams{
			ResourceType: domain.LifecycleResourceTask,
			ResourceID:   t.ID,
			Status:       "queued",
			StatusSource: domain.LifecycleSourceWorker,
			ReasonCode:   domain.LifecycleReasonAutomaticRetry,
			Reason:       message,
			RecoveryHint: domain.LifecycleRecoveryAutomaticRetry,
			RelatedJobID: jobID,
		},
		ExpectedStatuses: []string{"running"},
		AllowNonContract: true,
		IgnoreTerminal:   true,
		Fields: []lifecycleFieldUpdate{
			{Column: "session_id", SQL: "coalesce(nullif(%s,''),session_id)", Args: []any{sessionID}},
			{Column: "started_at", SQL: "NULL"},
			{Column: "finished_at", SQL: "NULL"},
		},
	})
	if err != nil {
		return err
	}
	if result.Idempotent {
		return nil
	}
	if _, err = insertEvent(ctx, tx, NewEvent{ProjectID: &t.ProjectID, Type: "task.retry_wait", AggregateType: "task", AggregateID: t.ID, ResourceVersion: result.State.Version, Payload: mustJSON(map[string]any{"message": message})}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// StopTask cancels queued work transactionally. For a running task it returns
// the active job IDs so the application layer can signal the matching process;
// the worker restores the task to pending after the process exits.
func (s *Store) StopTask(ctx context.Context, taskID uuid.UUID, version int64) (domain.PlanTask, []uuid.UUID, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return domain.PlanTask{}, nil, err
	}
	defer tx.Rollback(ctx)
	t, err := scanTask(tx.QueryRow(ctx, `SELECT id,project_id,plan_id,task_key,position,title,scope,acceptance,status,coalesce(session_id,''),started_at,finished_at,created_at,updated_at,version FROM plan_tasks WHERE id=$1 FOR UPDATE`, taskID))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.PlanTask{}, nil, domain.ErrNotFound
	}
	if err != nil {
		return domain.PlanTask{}, nil, err
	}
	if t.Version != version {
		return domain.PlanTask{}, nil, domain.ErrVersionConflict
	}
	if t.Status != "queued" && t.Status != "running" {
		return domain.PlanTask{}, nil, domain.ErrInvalidTransition
	}
	rows, err := tx.Query(ctx, `SELECT id FROM jobs WHERE aggregate_type='task' AND aggregate_id=$1 AND status IN ('leased','running') FOR UPDATE`, taskID)
	if err != nil {
		return domain.PlanTask{}, nil, err
	}
	active := []uuid.UUID{}
	for rows.Next() {
		var id uuid.UUID
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return domain.PlanTask{}, nil, err
		}
		active = append(active, id)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return domain.PlanTask{}, nil, err
	}
	rows.Close()
	if t.Status == "queued" {
		jobRows, queryErr := tx.Query(ctx, `SELECT id FROM jobs WHERE aggregate_type='task' AND aggregate_id=$1 AND status IN ('queued','retry_wait','leased') FOR UPDATE`, taskID)
		if queryErr != nil {
			return domain.PlanTask{}, nil, queryErr
		}
		jobIDs := []uuid.UUID{}
		for jobRows.Next() {
			var jobID uuid.UUID
			if queryErr = jobRows.Scan(&jobID); queryErr != nil {
				jobRows.Close()
				return domain.PlanTask{}, nil, queryErr
			}
			jobIDs = append(jobIDs, jobID)
		}
		if queryErr = jobRows.Err(); queryErr != nil {
			jobRows.Close()
			return domain.PlanTask{}, nil, queryErr
		}
		jobRows.Close()
		for _, jobID := range jobIDs {
			_, err = transitionLifecycleTx(ctx, tx, lifecycleTransitionRequest{
				LifecycleTransitionParams: LifecycleTransitionParams{
					ResourceType: domain.LifecycleResourceJob,
					ResourceID:   jobID,
					Status:       "cancelled",
					StatusSource: domain.LifecycleSourceUser,
					ReasonCode:   domain.LifecycleReasonUserCancelled,
					Reason:       "task stopped by user",
					RecoveryHint: domain.LifecycleRecoveryNone,
				},
				ExpectedStatuses: []string{"queued", "retry_wait", "leased"},
				AllowNonContract: true,
				IgnoreTerminal:   true,
				Fields:           []lifecycleFieldUpdate{{Column: "lease_expires_at", SQL: "NULL"}},
			})
			if err != nil {
				return domain.PlanTask{}, nil, err
			}
		}
		var relatedJobID *uuid.UUID
		if len(jobIDs) > 0 {
			relatedJobID = &jobIDs[0]
		}
		transition, transitionErr := transitionLifecycleTx(ctx, tx, lifecycleTransitionRequest{
			LifecycleTransitionParams: LifecycleTransitionParams{
				ResourceType:    domain.LifecycleResourceTask,
				ResourceID:      taskID,
				ExpectedVersion: version,
				Status:          "pending",
				StatusSource:    domain.LifecycleSourceUser,
				ReasonCode:      domain.LifecycleReasonUserCancelled,
				Reason:          "task stopped by user",
				RecoveryHint:    domain.LifecycleRecoveryRetryFromStart,
				RelatedJobID:    relatedJobID,
			},
			ExpectedStatuses: []string{"queued"},
			Fields: []lifecycleFieldUpdate{
				{Column: "started_at", SQL: "NULL"},
				{Column: "finished_at", SQL: "NULL"},
			},
		})
		if transitionErr != nil {
			return domain.PlanTask{}, nil, transitionErr
		}
		if _, err = insertEvent(ctx, tx, NewEvent{ProjectID: &t.ProjectID, Type: "task.cancelled", AggregateType: "task", AggregateID: t.ID, ResourceVersion: transition.State.Version, Payload: mustJSON(map[string]any{"reason": "stopped by user"})}); err != nil {
			return domain.PlanTask{}, nil, err
		}
		t, err = scanTask(tx.QueryRow(ctx, `SELECT id,project_id,plan_id,task_key,position,title,scope,acceptance,status,coalesce(session_id,''),started_at,finished_at,created_at,updated_at,version FROM plan_tasks WHERE id=$1`, taskID))
		if err != nil {
			return domain.PlanTask{}, nil, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return domain.PlanTask{}, nil, err
	}
	return t, active, nil
}

func (s *Store) StopPlan(ctx context.Context, planID uuid.UUID, version int64) (domain.Plan, []uuid.UUID, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return domain.Plan{}, nil, err
	}
	defer tx.Rollback(ctx)
	planTransition, err := transitionLifecycleTx(ctx, tx, lifecycleTransitionRequest{
		LifecycleTransitionParams: LifecycleTransitionParams{
			ResourceType:    domain.LifecycleResourcePlan,
			ResourceID:      planID,
			ExpectedVersion: version,
			Status:          "cancelled",
			StatusSource:    domain.LifecycleSourceUser,
			ReasonCode:      domain.LifecycleReasonUserCancelled,
			Reason:          "plan stopped by user",
			RecoveryHint:    domain.LifecycleRecoveryNone,
		},
		ExpectedStatuses: []string{"ready", "running", "validating", "blocked"},
		AllowNonContract: true,
		IgnoreTerminal:   true,
	})
	if err != nil {
		return domain.Plan{}, nil, err
	}
	p, err := scanPlan(tx.QueryRow(ctx, `SELECT id,project_id,intake_id,title,spec,markdown,status,config_snapshot,created_at,updated_at,version FROM plans WHERE id=$1`, planID))
	if err != nil {
		return domain.Plan{}, nil, err
	}
	rows, err := tx.Query(ctx, `SELECT id,status FROM jobs WHERE aggregate_type='task' AND aggregate_id IN (SELECT id FROM plan_tasks WHERE plan_id=$1) AND status IN ('queued','retry_wait','leased','running') FOR UPDATE`, planID)
	if err != nil {
		return domain.Plan{}, nil, err
	}
	type planJob struct {
		id     uuid.UUID
		status string
	}
	jobs := []planJob{}
	active := []uuid.UUID{}
	for rows.Next() {
		var job planJob
		if err = rows.Scan(&job.id, &job.status); err != nil {
			rows.Close()
			return domain.Plan{}, nil, err
		}
		jobs = append(jobs, job)
		if job.status == "running" {
			active = append(active, job.id)
		}
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return domain.Plan{}, nil, err
	}
	rows.Close()
	for _, job := range jobs {
		if job.status == "running" {
			continue
		}
		_, err = transitionLifecycleTx(ctx, tx, lifecycleTransitionRequest{
			LifecycleTransitionParams: LifecycleTransitionParams{
				ResourceType: domain.LifecycleResourceJob,
				ResourceID:   job.id,
				Status:       "cancelled",
				StatusSource: domain.LifecycleSourceUser,
				ReasonCode:   domain.LifecycleReasonUserCancelled,
				Reason:       "plan stopped by user",
				RecoveryHint: domain.LifecycleRecoveryNone,
			},
			ExpectedStatuses: []string{"queued", "retry_wait", "leased"},
			AllowNonContract: true,
			IgnoreTerminal:   true,
			Fields:           []lifecycleFieldUpdate{{Column: "lease_expires_at", SQL: "NULL"}},
		})
		if err != nil {
			return domain.Plan{}, nil, err
		}
	}
	taskRows, queryErr := tx.Query(ctx, `SELECT id FROM plan_tasks WHERE plan_id=$1 AND status='queued' FOR UPDATE`, planID)
	if queryErr != nil {
		return domain.Plan{}, nil, queryErr
	}
	taskIDs := []uuid.UUID{}
	for taskRows.Next() {
		var taskID uuid.UUID
		if queryErr = taskRows.Scan(&taskID); queryErr != nil {
			taskRows.Close()
			return domain.Plan{}, nil, queryErr
		}
		taskIDs = append(taskIDs, taskID)
	}
	if queryErr = taskRows.Err(); queryErr != nil {
		taskRows.Close()
		return domain.Plan{}, nil, queryErr
	}
	taskRows.Close()
	for _, taskID := range taskIDs {
		jobID, lookupErr := currentTaskJobID(ctx, tx, taskID)
		if lookupErr != nil {
			return domain.Plan{}, nil, lookupErr
		}
		transition, transitionErr := transitionLifecycleTx(ctx, tx, lifecycleTransitionRequest{
			LifecycleTransitionParams: LifecycleTransitionParams{
				ResourceType: domain.LifecycleResourceTask,
				ResourceID:   taskID,
				Status:       "pending",
				StatusSource: domain.LifecycleSourceUser,
				ReasonCode:   domain.LifecycleReasonUserCancelled,
				Reason:       "plan stopped by user",
				RecoveryHint: domain.LifecycleRecoveryRetryFromStart,
				RelatedJobID: jobID,
			},
			ExpectedStatuses: []string{"queued"},
			Fields: []lifecycleFieldUpdate{
				{Column: "started_at", SQL: "NULL"},
				{Column: "finished_at", SQL: "NULL"},
			},
		})
		if transitionErr != nil {
			return domain.Plan{}, nil, transitionErr
		}
		if _, err = insertEvent(ctx, tx, NewEvent{ProjectID: &p.ProjectID, Type: "task.cancelled", AggregateType: "task", AggregateID: taskID, ResourceVersion: transition.State.Version, Payload: mustJSON(map[string]any{"reason": "plan stopped by user"})}); err != nil {
			return domain.Plan{}, nil, err
		}
	}
	if !planTransition.Idempotent {
		if _, err = insertEvent(ctx, tx, NewEvent{ProjectID: &p.ProjectID, Type: "plan.cancelled", AggregateType: "plan", AggregateID: p.ID, ResourceVersion: p.Version, Payload: mustJSON(map[string]any{"reason": "stopped by user"})}); err != nil {
			return domain.Plan{}, nil, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return domain.Plan{}, nil, err
	}
	return p, active, nil
}

func (s *Store) DeletePlan(ctx context.Context, planID uuid.UUID, version int64) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var projectID uuid.UUID
	var status string
	if err = tx.QueryRow(ctx, `SELECT project_id,status FROM plans WHERE id=$1 AND version=$2 FOR UPDATE`, planID, version).Scan(&projectID, &status); errors.Is(err, pgx.ErrNoRows) {
		return s.versionOrNotFound(ctx, tx, "plans", planID)
	}
	if err != nil {
		return err
	}
	if status == "running" || status == "validating" || status == "generating" {
		return domain.ErrInvalidTransition
	}
	if _, err = tx.Exec(ctx, `DELETE FROM jobs WHERE aggregate_type='task' AND aggregate_id IN (SELECT id FROM plan_tasks WHERE plan_id=$1)`, planID); err != nil {
		return err
	}
	if _, err = insertEvent(ctx, tx, NewEvent{ProjectID: &projectID, Type: "plan.deleted", AggregateType: "plan", AggregateID: planID, ResourceVersion: version + 1}); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM plans WHERE id=$1`, planID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// BlockTaskJobForDrift terminates a task job without retrying it and restores
// the task/plan to states from which an explicit drift disposition can resume.
// The structured report is persisted with the state transition for audit and
// diagnostics; no workspace mutation is performed here.
func (s *Store) BlockTaskJobForDrift(ctx context.Context, job domain.Job, workerID string, report json.RawMessage) error {
	if !json.Valid(report) {
		return errors.New("invalid drift report")
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	message := "execution context drift requires explicit disposition"
	_, err = transitionLifecycleTx(ctx, tx, lifecycleTransitionRequest{
		LifecycleTransitionParams: LifecycleTransitionParams{
			ResourceType:        domain.LifecycleResourceJob,
			ResourceID:          job.ID,
			Status:              "failed",
			StatusSource:        domain.LifecycleSourceWorker,
			ReasonCode:          domain.LifecycleReasonExecutionFailed,
			Reason:              message,
			RecoveryHint:        domain.LifecycleRecoveryManualReview,
			ExecutionCheckpoint: mustJSON(map[string]any{"report": json.RawMessage(report), "workerId": workerID}),
		},
		ExpectedStatuses: []string{"leased", "running"},
		RequireWorkerID:  &workerID,
		AllowNonContract: true,
		IgnoreTerminal:   true,
		MismatchError:    domain.ErrNotFound,
		Fields: []lifecycleFieldUpdate{
			{Column: "last_error", Args: []any{message}},
			{Column: "lease_expires_at", SQL: "NULL"},
		},
	})
	if err != nil {
		return err
	}
	if job.Type != "task.execute" {
		return tx.Commit(ctx)
	}
	var planID uuid.UUID
	if err = tx.QueryRow(ctx, `SELECT plan_id FROM plan_tasks WHERE id=$1`, job.AggregateID).Scan(&planID); errors.Is(err, pgx.ErrNoRows) {
		return domain.ErrNotFound
	} else if err != nil {
		return err
	}
	taskTransition, err := transitionLifecycleTx(ctx, tx, lifecycleTransitionRequest{
		LifecycleTransitionParams: LifecycleTransitionParams{
			ResourceType:        domain.LifecycleResourceTask,
			ResourceID:          job.AggregateID,
			Status:              "pending",
			StatusSource:        domain.LifecycleSourceWorker,
			ReasonCode:          domain.LifecycleReasonExecutionFailed,
			Reason:              message,
			RecoveryHint:        domain.LifecycleRecoveryManualReview,
			ExecutionCheckpoint: mustJSON(map[string]any{"report": json.RawMessage(report)}),
			RelatedJobID:        &job.ID,
		},
		ExpectedStatuses: []string{"queued", "running", "failed"},
		AllowNonContract: true,
		AllowTerminal:    true,
		Fields: []lifecycleFieldUpdate{
			{Column: "started_at", SQL: "NULL"},
			{Column: "finished_at", SQL: "NULL"},
		},
	})
	if err != nil {
		return err
	}
	payload := mustJSON(map[string]any{"jobId": job.ID, "report": json.RawMessage(report)})
	if _, err = insertEvent(ctx, tx, NewEvent{ProjectID: &taskTransition.State.ProjectID, Type: "task.drift_detected", AggregateType: "task", AggregateID: job.AggregateID, ResourceVersion: taskTransition.State.Version, Payload: payload}); err != nil {
		return err
	}
	planTransition, err := transitionLifecycleTx(ctx, tx, lifecycleTransitionRequest{
		LifecycleTransitionParams: LifecycleTransitionParams{
			ResourceType:        domain.LifecycleResourcePlan,
			ResourceID:          planID,
			Status:              "blocked",
			StatusSource:        domain.LifecycleSourceWorker,
			ReasonCode:          domain.LifecycleReasonExecutionFailed,
			Reason:              message,
			RecoveryHint:        domain.LifecycleRecoveryManualReview,
			ExecutionCheckpoint: mustJSON(map[string]any{"taskId": job.AggregateID, "report": json.RawMessage(report)}),
			RelatedJobID:        &job.ID,
		},
		ExpectedStatuses: []string{"running", "validating"},
		IgnoreTerminal:   true,
	})
	if err != nil {
		return err
	}
	if _, err = insertEvent(ctx, tx, NewEvent{ProjectID: &taskTransition.State.ProjectID, Type: "plan.drift_detected", AggregateType: "plan", AggregateID: planID, ResourceVersion: planTransition.State.Version, Payload: payload}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
