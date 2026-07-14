package repository

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const desktopShutdownReason = "desktop application closed; execution was interrupted"

// ReconcileInstanceShutdown persistently stops work that is owned by a single
// backend instance before that process exits. Planning jobs are read-only and
// remain runnable; code-execution jobs are deliberately not requeued because
// their CLI may have already changed the workspace.
//
// It is safe for workers to finish their cancellation callbacks after this
// method: all write paths only mutate their expected non-terminal state.
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

	rows, err := tx.Query(ctx, `
		SELECT id,job_type,aggregate_id
		FROM jobs
		WHERE worker_id LIKE $1
			AND status IN ('leased','running')
		FOR UPDATE`, instanceID+":%")
	if err != nil {
		return err
	}
	planJobs := make([]uuid.UUID, 0)
	taskJobs := make([]uuid.UUID, 0)
	taskIDs := make([]uuid.UUID, 0)
	activeJobs := make([]uuid.UUID, 0)
	for rows.Next() {
		var id, aggregateID uuid.UUID
		var jobType string
		if err = rows.Scan(&id, &jobType, &aggregateID); err != nil {
			rows.Close()
			return err
		}
		activeJobs = append(activeJobs, id)
		switch jobType {
		case "plan.generate":
			planJobs = append(planJobs, id)
		case "task.execute":
			taskJobs = append(taskJobs, id)
			taskIDs = append(taskIDs, aggregateID)
		}
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	if len(planJobs) > 0 {
		if _, err = tx.Exec(ctx, `
			UPDATE jobs
			SET status='queued',worker_id=NULL,lease_expires_at=NULL,run_after=now(),
				attempt=GREATEST(attempt-1,0),last_error=$2,updated_at=now(),version=version+1
			WHERE id=ANY($1)`, planJobs, desktopShutdownReason); err != nil {
			return err
		}
		for _, id := range planJobs {
			if _, err = tx.Exec(ctx, `SELECT pg_notify('specrelay_jobs',$1)`, id.String()); err != nil {
				return err
			}
		}
	}
	if len(taskJobs) > 0 {
		if _, err = tx.Exec(ctx, `
			UPDATE jobs
			SET status='cancelled',worker_id=NULL,lease_expires_at=NULL,last_error=$2,
				updated_at=now(),version=version+1
			WHERE id=ANY($1)`, taskJobs, desktopShutdownReason); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `DELETE FROM workspace_leases WHERE job_id=ANY($1)`, taskJobs); err != nil {
			return err
		}
	}
	if len(activeJobs) > 0 {
		if _, err = tx.Exec(ctx, `
			UPDATE agent_runs
			SET status='cancelled',termination_reason=$2,finished_at=now(),updated_at=now(),version=version+1
			WHERE job_id=ANY($1) AND status='running'`, activeJobs, desktopShutdownReason); err != nil {
			return err
		}
	}
	// Requirement discussions do not have a queue job. Their owner id makes
	// them safe to reconcile without touching another desktop instance.
	if _, err = tx.Exec(ctx, `
		UPDATE agent_runs
		SET status='cancelled',termination_reason=$2,finished_at=now(),updated_at=now(),version=version+1
		WHERE owner_instance_id=$1 AND status='running'`, instanceID, desktopShutdownReason); err != nil {
		return err
	}

	type taskChange struct {
		id, projectID, planID uuid.UUID
		version               int64
	}
	changes := make([]taskChange, 0)
	if len(taskIDs) > 0 {
		updated, queryErr := tx.Query(ctx, `
			UPDATE plan_tasks
			SET status='pending',started_at=NULL,finished_at=NULL,updated_at=now(),version=version+1
			WHERE id=ANY($1) AND status='running'
			RETURNING id,project_id,plan_id,version`, taskIDs)
		if queryErr != nil {
			return queryErr
		}
		for updated.Next() {
			var change taskChange
			if queryErr = updated.Scan(&change.id, &change.projectID, &change.planID, &change.version); queryErr != nil {
				updated.Close()
				return queryErr
			}
			changes = append(changes, change)
		}
		if queryErr = updated.Err(); queryErr != nil {
			updated.Close()
			return queryErr
		}
		updated.Close()
	}

	planIDs := make(map[uuid.UUID]uuid.UUID)
	for _, change := range changes {
		if _, err = insertEvent(ctx, tx, NewEvent{ProjectID: &change.projectID, Type: "task.cancelled", AggregateType: "task", AggregateID: change.id, ResourceVersion: change.version, Payload: mustJSON(map[string]any{"message": desktopShutdownReason})}); err != nil {
			return err
		}
		planIDs[change.planID] = change.projectID
	}
	for planID, projectID := range planIDs {
		var version int64
		err = tx.QueryRow(ctx, `
			UPDATE plans
			SET status='blocked',updated_at=now(),version=version+1
			WHERE id=$1 AND status IN ('running','validating')
			RETURNING version`, planID).Scan(&version)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		if _, err = insertEvent(ctx, tx, NewEvent{ProjectID: &projectID, Type: "plan.blocked", AggregateType: "plan", AggregateID: planID, ResourceVersion: version, Payload: mustJSON(map[string]any{"reason": desktopShutdownReason})}); err != nil {
			return err
		}
	}

	return tx.Commit(ctx)
}
