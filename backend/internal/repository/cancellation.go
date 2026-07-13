package repository

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/lyming99/specrelay/backend/internal/domain"
)

func (s *Store) CancelJob(ctx context.Context, id uuid.UUID, workerID string) error {
	tag, err := s.Pool.Exec(ctx, `UPDATE jobs SET status='cancelled',lease_expires_at=NULL,updated_at=now(),version=version+1 WHERE id=$1 AND worker_id=$2 AND status IN ('leased','running')`, id, workerID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

func (s *Store) ReturnTaskPending(ctx context.Context, t domain.PlanTask, message string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var version int64
	if err = tx.QueryRow(ctx, `UPDATE plan_tasks SET status='pending',started_at=NULL,finished_at=NULL,updated_at=now(),version=version+1 WHERE id=$1 AND status='running' RETURNING version`, t.ID).Scan(&version); errors.Is(err, pgx.ErrNoRows) {
		// Automation stop may have already restored the running task before the
		// worker observes its SIGTERM. Treat that terminal reconciliation as an
		// idempotent cancellation rather than turning it into a failed job.
		return nil
	} else if err != nil {
		return err
	}
	if _, err = insertEvent(ctx, tx, NewEvent{ProjectID: &t.ProjectID, Type: "task.cancelled", AggregateType: "task", AggregateID: t.ID, ResourceVersion: version, Payload: mustJSON(map[string]any{"message": message})}); err != nil {
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
	var version int64
	if err = tx.QueryRow(ctx, `UPDATE plan_tasks SET status='queued',session_id=coalesce(nullif($2,''),session_id),started_at=NULL,finished_at=NULL,updated_at=now(),version=version+1 WHERE id=$1 AND status='running' RETURNING version`, t.ID, sessionID).Scan(&version); err != nil {
		return err
	}
	if _, err = insertEvent(ctx, tx, NewEvent{ProjectID: &t.ProjectID, Type: "task.retry_wait", AggregateType: "task", AggregateID: t.ID, ResourceVersion: version, Payload: mustJSON(map[string]any{"message": message})}); err != nil {
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
	t, err := scanTask(tx.QueryRow(ctx, `SELECT id,project_id,plan_id,task_key,position,title,scope,acceptance,status,coalesce(session_id,''),started_at,finished_at,created_at,updated_at,version FROM plan_tasks WHERE id=$1 AND version=$2 FOR UPDATE`, taskID, version))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.PlanTask{}, nil, s.versionOrNotFound(ctx, tx, "plan_tasks", taskID)
	}
	if err != nil {
		return domain.PlanTask{}, nil, err
	}
	if t.Status != "queued" && t.Status != "running" {
		return domain.PlanTask{}, nil, domain.ErrInvalidTransition
	}
	rows, err := tx.Query(ctx, `SELECT id FROM jobs WHERE aggregate_type='task' AND aggregate_id=$1 AND status IN ('leased','running') ORDER BY created_at`, taskID)
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
		if _, err = tx.Exec(ctx, `UPDATE jobs SET status='cancelled',lease_expires_at=NULL,updated_at=now(),version=version+1 WHERE aggregate_type='task' AND aggregate_id=$1 AND status IN ('queued','retry_wait','leased')`, taskID); err != nil {
			return domain.PlanTask{}, nil, err
		}
		t, err = scanTask(tx.QueryRow(ctx, `UPDATE plan_tasks SET status='pending',started_at=NULL,finished_at=NULL,updated_at=now(),version=version+1 WHERE id=$1 AND status='queued' RETURNING id,project_id,plan_id,task_key,position,title,scope,acceptance,status,coalesce(session_id,''),started_at,finished_at,created_at,updated_at,version`, taskID))
		if err != nil {
			return domain.PlanTask{}, nil, err
		}
		if _, err = insertEvent(ctx, tx, NewEvent{ProjectID: &t.ProjectID, Type: "task.cancelled", AggregateType: "task", AggregateID: t.ID, ResourceVersion: t.Version, Payload: mustJSON(map[string]any{"reason": "stopped by user"})}); err != nil {
			return domain.PlanTask{}, nil, err
		}
		active = nil
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
	p, err := scanPlan(tx.QueryRow(ctx, `UPDATE plans SET status='cancelled',updated_at=now(),version=version+1 WHERE id=$1 AND version=$2 AND status IN ('ready','running','validating','blocked') RETURNING id,project_id,intake_id,title,spec,markdown,status,config_snapshot,created_at,updated_at,version`, planID, version))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Plan{}, nil, s.versionOrNotFound(ctx, tx, "plans", planID)
	}
	if err != nil {
		return domain.Plan{}, nil, err
	}
	rows, err := tx.Query(ctx, `SELECT id FROM jobs WHERE aggregate_type='task' AND aggregate_id IN (SELECT id FROM plan_tasks WHERE plan_id=$1) AND status='running'`, planID)
	if err != nil {
		return domain.Plan{}, nil, err
	}
	active := []uuid.UUID{}
	for rows.Next() {
		var id uuid.UUID
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return domain.Plan{}, nil, err
		}
		active = append(active, id)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return domain.Plan{}, nil, err
	}
	rows.Close()
	if _, err = tx.Exec(ctx, `UPDATE jobs SET status='cancelled',lease_expires_at=NULL,updated_at=now(),version=version+1 WHERE aggregate_type='task' AND aggregate_id IN (SELECT id FROM plan_tasks WHERE plan_id=$1) AND status IN ('queued','retry_wait','leased')`, planID); err != nil {
		return domain.Plan{}, nil, err
	}
	pendingRows, err := tx.Query(ctx, `UPDATE plan_tasks SET status='pending',started_at=NULL,finished_at=NULL,updated_at=now(),version=version+1 WHERE plan_id=$1 AND status='queued' RETURNING id,version`, planID)
	if err != nil {
		return domain.Plan{}, nil, err
	}
	type taskChange struct {
		id      uuid.UUID
		version int64
	}
	changes := []taskChange{}
	for pendingRows.Next() {
		var change taskChange
		if err = pendingRows.Scan(&change.id, &change.version); err != nil {
			pendingRows.Close()
			return domain.Plan{}, nil, err
		}
		changes = append(changes, change)
	}
	if err = pendingRows.Err(); err != nil {
		pendingRows.Close()
		return domain.Plan{}, nil, err
	}
	pendingRows.Close()
	for _, change := range changes {
		if _, err = insertEvent(ctx, tx, NewEvent{ProjectID: &p.ProjectID, Type: "task.cancelled", AggregateType: "task", AggregateID: change.id, ResourceVersion: change.version, Payload: mustJSON(map[string]any{"reason": "plan stopped by user"})}); err != nil {
			return domain.Plan{}, nil, err
		}
	}
	if _, err = insertEvent(ctx, tx, NewEvent{ProjectID: &p.ProjectID, Type: "plan.cancelled", AggregateType: "plan", AggregateID: p.ID, ResourceVersion: p.Version, Payload: mustJSON(map[string]any{"reason": "stopped by user"})}); err != nil {
		return domain.Plan{}, nil, err
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
