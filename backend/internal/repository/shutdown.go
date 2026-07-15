package repository

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/lyming99/specrelay/backend/internal/domain"
)

const desktopShutdownReason = "desktop application closed; execution was interrupted"

// ReconcileInstanceShutdown persistently stops work that is owned by a single
// backend instance before that process exits. Planning jobs are read-only and
// remain runnable; code-execution jobs are deliberately not requeued because
// their CLI may have already changed the workspace.
//
// It is safe for workers to finish their cancellation callbacks after this
// method: all lifecycle writes share the same terminal-protected migration gate.
func (s *Store) ReconcileInstanceShutdown(ctx context.Context, instanceID string) error {
	instanceID = strings.TrimSpace(instanceID)
	if instanceID == "" {
		return errors.New("shutdown reconciliation requires a backend instance id")
	}

	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	type ownedJob struct {
		id          uuid.UUID
		jobType     string
		aggregateID uuid.UUID
		status      string
	}
	rows, err := tx.Query(ctx, `
		SELECT id,job_type,aggregate_id,status
		FROM jobs
		WHERE worker_id LIKE $1
			AND status IN ('leased','running')
		FOR UPDATE`, instanceID+":%")
	if err != nil {
		return err
	}
	jobs := []ownedJob{}
	for rows.Next() {
		var job ownedJob
		if err = rows.Scan(&job.id, &job.jobType, &job.aggregateID, &job.status); err != nil {
			rows.Close()
			return err
		}
		jobs = append(jobs, job)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	activeJobIDs := make([]uuid.UUID, 0, len(jobs))
	taskJobs := make([]ownedJob, 0, len(jobs))
	for _, job := range jobs {
		activeJobIDs = append(activeJobIDs, job.id)
		switch job.jobType {
		case "plan.generate":
			if _, err = transitionLifecycleTx(ctx, tx, lifecycleTransitionRequest{
				LifecycleTransitionParams: LifecycleTransitionParams{
					ResourceType: domain.LifecycleResourceJob,
					ResourceID:   job.id,
					Status:       "queued",
					StatusSource: domain.LifecycleSourceBackend,
					ReasonCode:   domain.LifecycleReasonBackendShutdown,
					Reason:       desktopShutdownReason,
					RecoveryHint: domain.LifecycleRecoveryAutomaticRetry,
				},
				ExpectedStatuses: []string{job.status},
				AllowNonContract: true,
				IgnoreTerminal:   true,
				Fields: []lifecycleFieldUpdate{
					{Column: "worker_id", SQL: "NULL"},
					{Column: "lease_expires_at", SQL: "NULL"},
					{Column: "run_after", SQL: "now()"},
					{Column: "attempt", SQL: "GREATEST(attempt-1,0)"},
					{Column: "last_error", Args: []any{desktopShutdownReason}},
				},
			}); err != nil {
				return err
			}
			if _, err = tx.Exec(ctx, `SELECT pg_notify('specrelay_jobs',$1)`, job.id.String()); err != nil {
				return err
			}
		case "task.execute":
			taskJobs = append(taskJobs, job)
			if _, err = transitionLifecycleTx(ctx, tx, lifecycleTransitionRequest{
				LifecycleTransitionParams: LifecycleTransitionParams{
					ResourceType: domain.LifecycleResourceJob,
					ResourceID:   job.id,
					Status:       "cancelled",
					StatusSource: domain.LifecycleSourceBackend,
					ReasonCode:   domain.LifecycleReasonBackendShutdown,
					Reason:       desktopShutdownReason,
					RecoveryHint: domain.LifecycleRecoveryManualReview,
				},
				ExpectedStatuses: []string{job.status},
				AllowNonContract: true,
				IgnoreTerminal:   true,
				Fields: []lifecycleFieldUpdate{
					{Column: "worker_id", SQL: "NULL"},
					{Column: "lease_expires_at", SQL: "NULL"},
					{Column: "last_error", Args: []any{desktopShutdownReason}},
				},
			}); err != nil {
				return err
			}
		}
	}

	if len(taskJobs) > 0 {
		taskJobIDs := make([]uuid.UUID, 0, len(taskJobs))
		for _, job := range taskJobs {
			taskJobIDs = append(taskJobIDs, job.id)
		}
		if _, err = tx.Exec(ctx, `DELETE FROM workspace_leases WHERE job_id=ANY($1)`, taskJobIDs); err != nil {
			return err
		}
	}

	if len(activeJobIDs) > 0 {
		runRows, queryErr := tx.Query(ctx, `SELECT id,job_id FROM agent_runs
			WHERE job_id=ANY($1) AND status='running' FOR UPDATE`, activeJobIDs)
		if queryErr != nil {
			return queryErr
		}
		type ownedRun struct{ id, jobID uuid.UUID }
		runs := []ownedRun{}
		for runRows.Next() {
			var run ownedRun
			if queryErr = runRows.Scan(&run.id, &run.jobID); queryErr != nil {
				runRows.Close()
				return queryErr
			}
			runs = append(runs, run)
		}
		if queryErr = runRows.Err(); queryErr != nil {
			runRows.Close()
			return queryErr
		}
		runRows.Close()
		for _, run := range runs {
			if _, err = transitionLifecycleTx(ctx, tx, lifecycleTransitionRequest{
				LifecycleTransitionParams: LifecycleTransitionParams{
					ResourceType: domain.LifecycleResourceAgentRun,
					ResourceID:   run.id,
					Status:       "cancelled",
					StatusSource: domain.LifecycleSourceBackend,
					ReasonCode:   domain.LifecycleReasonBackendShutdown,
					Reason:       desktopShutdownReason,
					RecoveryHint: domain.LifecycleRecoveryManualReview,
					RelatedJobID: &run.jobID,
					RelatedRunID: &run.id,
				},
				ExpectedStatuses: []string{"running"},
				AllowNonContract: true,
				IgnoreTerminal:   true,
				Fields: []lifecycleFieldUpdate{
					{Column: "termination_reason", Args: []any{desktopShutdownReason}},
					{Column: "finished_at", SQL: "now()"},
				},
			}); err != nil {
				return err
			}
		}
	}

	// Requirement discussions do not have a queue job. Their owner id makes
	// them safe to reconcile without touching another desktop instance.
	discussionRows, err := tx.Query(ctx, `SELECT id,job_id FROM agent_runs
		WHERE owner_instance_id=$1 AND status='running' FOR UPDATE`, instanceID)
	if err != nil {
		return err
	}
	type discussionRun struct {
		id    uuid.UUID
		jobID *uuid.UUID
	}
	discussions := []discussionRun{}
	for discussionRows.Next() {
		var run discussionRun
		if err = discussionRows.Scan(&run.id, &run.jobID); err != nil {
			discussionRows.Close()
			return err
		}
		discussions = append(discussions, run)
	}
	if err = discussionRows.Err(); err != nil {
		discussionRows.Close()
		return err
	}
	discussionRows.Close()
	for _, run := range discussions {
		if _, err = transitionLifecycleTx(ctx, tx, lifecycleTransitionRequest{
			LifecycleTransitionParams: LifecycleTransitionParams{
				ResourceType: domain.LifecycleResourceAgentRun,
				ResourceID:   run.id,
				Status:       "cancelled",
				StatusSource: domain.LifecycleSourceBackend,
				ReasonCode:   domain.LifecycleReasonBackendShutdown,
				Reason:       desktopShutdownReason,
				RecoveryHint: domain.LifecycleRecoveryManualReview,
				RelatedJobID: run.jobID,
				RelatedRunID: &run.id,
			},
			ExpectedStatuses: []string{"running"},
			AllowNonContract: true,
			IgnoreTerminal:   true,
			Fields: []lifecycleFieldUpdate{
				{Column: "termination_reason", Args: []any{desktopShutdownReason}},
				{Column: "finished_at", SQL: "now()"},
			},
		}); err != nil {
			return err
		}
	}

	type taskChange struct {
		id, projectID, planID, jobID uuid.UUID
		version                      int64
	}
	changes := []taskChange{}
	for _, job := range taskJobs {
		result, transitionErr := transitionLifecycleTx(ctx, tx, lifecycleTransitionRequest{
			LifecycleTransitionParams: LifecycleTransitionParams{
				ResourceType: domain.LifecycleResourceTask,
				ResourceID:   job.aggregateID,
				Status:       "pending",
				StatusSource: domain.LifecycleSourceBackend,
				ReasonCode:   domain.LifecycleReasonBackendShutdown,
				Reason:       desktopShutdownReason,
				RecoveryHint: domain.LifecycleRecoveryRetryFromStart,
				RelatedJobID: &job.id,
			},
			ExpectedStatuses: []string{"running"},
			AllowNonContract: true,
			IgnoreTerminal:   true,
			Fields: []lifecycleFieldUpdate{
				{Column: "started_at", SQL: "NULL"},
				{Column: "finished_at", SQL: "NULL"},
			},
		})
		if transitionErr != nil {
			return transitionErr
		}
		if result.Idempotent {
			continue
		}
		var planID uuid.UUID
		if err = tx.QueryRow(ctx, `SELECT plan_id FROM plan_tasks WHERE id=$1`, job.aggregateID).Scan(&planID); err != nil {
			return err
		}
		changes = append(changes, taskChange{id: job.aggregateID, projectID: result.State.ProjectID, planID: planID, jobID: job.id, version: result.State.Version})
	}

	planIDs := make(map[uuid.UUID]taskChange)
	for _, change := range changes {
		if _, err = insertEvent(ctx, tx, NewEvent{ProjectID: &change.projectID, Type: "task.cancelled", AggregateType: "task", AggregateID: change.id, ResourceVersion: change.version, Payload: mustJSON(map[string]any{"message": desktopShutdownReason})}); err != nil {
			return err
		}
		planIDs[change.planID] = change
	}
	for planID, change := range planIDs {
		result, transitionErr := transitionLifecycleTx(ctx, tx, lifecycleTransitionRequest{
			LifecycleTransitionParams: LifecycleTransitionParams{
				ResourceType: domain.LifecycleResourcePlan,
				ResourceID:   planID,
				Status:       "blocked",
				StatusSource: domain.LifecycleSourceBackend,
				ReasonCode:   domain.LifecycleReasonBackendShutdown,
				Reason:       desktopShutdownReason,
				RecoveryHint: domain.LifecycleRecoveryManualReview,
				RelatedJobID: &change.jobID,
			},
			ExpectedStatuses: []string{"running", "validating"},
			IgnoreTerminal:   true,
		})
		if transitionErr != nil {
			return transitionErr
		}
		if result.Idempotent {
			continue
		}
		if _, err = insertEvent(ctx, tx, NewEvent{ProjectID: &change.projectID, Type: "plan.blocked", AggregateType: "plan", AggregateID: planID, ResourceVersion: result.State.Version, Payload: mustJSON(map[string]any{"reason": desktopShutdownReason})}); err != nil {
			return err
		}
	}

	return tx.Commit(ctx)
}
