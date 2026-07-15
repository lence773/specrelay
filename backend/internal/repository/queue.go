package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/lyming99/specrelay/backend/internal/agent"
	"github.com/lyming99/specrelay/backend/internal/domain"
	"github.com/lyming99/specrelay/backend/internal/planspec"
)

func scanJob(row pgx.Row) (domain.Job, error) {
	var j domain.Job
	err := row.Scan(&j.ID, &j.ProjectID, &j.Type, &j.AggregateType, &j.AggregateID, &j.Payload, &j.Priority, &j.Status, &j.RunAfter, &j.WorkerID, &j.LeaseExpiresAt, &j.Attempt, &j.MaxAttempts, &j.LastError, &j.IdempotencyKey, &j.CreatedAt, &j.UpdatedAt, &j.Version)
	return j, err
}
func (s *Store) ClaimJob(ctx context.Context, workerID string, lease time.Duration) (domain.Job, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return domain.Job{}, err
	}
	defer tx.Rollback(ctx)
	var id uuid.UUID
	err = tx.QueryRow(ctx, `
		SELECT j.id
		FROM jobs j
		JOIN projects p ON p.id=j.project_id
		LEFT JOIN plan_tasks task
			ON j.job_type='task.execute' AND task.id=j.aggregate_id
		WHERE j.status IN ('queued','retry_wait')
			AND j.run_after<=now()
			AND p.automation_enabled=true
			AND (
				j.job_type<>'task.execute'
				OR task.plan_id=(
					SELECT owner.id
					FROM plans owner
					WHERE owner.project_id=j.project_id
						AND owner.status IN ('running','validating')
						AND owner.is_executable=true
						AND EXISTS (
							SELECT 1 FROM plan_tasks owner_task
							WHERE owner_task.plan_id=owner.id AND owner_task.status<>'succeeded'
						)
					ORDER BY owner.execution_started_at ASC NULLS LAST,owner.created_at ASC,owner.id ASC
					LIMIT 1
				)
			)
		ORDER BY j.priority ASC,j.created_at ASC
		FOR UPDATE OF j SKIP LOCKED
		LIMIT 1`).Scan(&id)
	if err != nil {
		return domain.Job{}, err
	}
	_, err = transitionLifecycleTx(ctx, tx, lifecycleTransitionRequest{
		LifecycleTransitionParams: LifecycleTransitionParams{
			ResourceType:        domain.LifecycleResourceJob,
			ResourceID:          id,
			Status:              "leased",
			StatusSource:        domain.LifecycleSourceWorker,
			ReasonCode:          domain.LifecycleReasonCode("worker_claimed"),
			Reason:              "worker claimed queued job",
			RecoveryHint:        domain.LifecycleRecoveryAutomaticRetry,
			ExecutionCheckpoint: mustJSON(map[string]any{"workerId": workerID}),
		},
		ExpectedStatuses: []string{"queued", "retry_wait"},
		MismatchError:    domain.ErrNotFound,
		Fields: []lifecycleFieldUpdate{
			{Column: "worker_id", Args: []any{workerID}},
			{Column: "lease_expires_at", SQL: "now()+%s::interval", Args: []any{lease.String()}},
			{Column: "attempt", SQL: "attempt+1"},
		},
	})
	if err != nil {
		return domain.Job{}, err
	}
	job, err := scanJob(tx.QueryRow(ctx, `SELECT id,project_id,job_type,aggregate_type,aggregate_id,payload,priority,status,run_after,coalesce(worker_id,''),lease_expires_at,attempt,max_attempts,coalesce(last_error,''),idempotency_key,created_at,updated_at,version FROM jobs WHERE id=$1`, id))
	if err != nil {
		return domain.Job{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return domain.Job{}, err
	}
	return job, nil
}

func (s *Store) MarkJobRunning(ctx context.Context, id uuid.UUID, workerID string) (domain.Job, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return domain.Job{}, err
	}
	defer tx.Rollback(ctx)
	_, err = transitionLifecycleTx(ctx, tx, lifecycleTransitionRequest{
		LifecycleTransitionParams: LifecycleTransitionParams{
			ResourceType:        domain.LifecycleResourceJob,
			ResourceID:          id,
			Status:              "running",
			StatusSource:        domain.LifecycleSourceWorker,
			ReasonCode:          domain.LifecycleReasonCode("worker_started"),
			Reason:              "worker started job execution",
			RecoveryHint:        domain.LifecycleRecoveryAutomaticRetry,
			ExecutionCheckpoint: mustJSON(map[string]any{"workerId": workerID}),
		},
		ExpectedStatuses: []string{"leased"},
		RequireWorkerID:  &workerID,
		MismatchError:    domain.ErrNotFound,
	})
	if err != nil {
		return domain.Job{}, err
	}
	job, err := scanJob(tx.QueryRow(ctx, `SELECT id,project_id,job_type,aggregate_type,aggregate_id,payload,priority,status,run_after,coalesce(worker_id,''),lease_expires_at,attempt,max_attempts,coalesce(last_error,''),idempotency_key,created_at,updated_at,version FROM jobs WHERE id=$1`, id))
	if err != nil {
		return domain.Job{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return domain.Job{}, err
	}
	return job, nil
}

// DeferJobForWorkspace returns a job to the queue after it discovers that
// another task currently owns the same workspace. ClaimJob increments attempts
// before execution begins, so this method compensates for that claim: waiting
// for a lock is not an execution attempt and must never exhaust max_attempts.
func (s *Store) DeferJobForWorkspace(ctx context.Context, id uuid.UUID, workerID string, delay time.Duration) error {
	if delay < 0 {
		delay = 0
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	_, err = transitionLifecycleTx(ctx, tx, lifecycleTransitionRequest{
		LifecycleTransitionParams: LifecycleTransitionParams{
			ResourceType:        domain.LifecycleResourceJob,
			ResourceID:          id,
			Status:              "queued",
			StatusSource:        domain.LifecycleSourceWorker,
			ReasonCode:          domain.LifecycleReasonAutomaticRetry,
			Reason:              "workspace is busy; job returned to queue",
			RecoveryHint:        domain.LifecycleRecoveryAutomaticRetry,
			ExecutionCheckpoint: mustJSON(map[string]any{"workerId": workerID, "delay": delay.String()}),
		},
		ExpectedStatuses: []string{"leased", "running"},
		RequireWorkerID:  &workerID,
		AllowNonContract: true,
		MismatchError:    domain.ErrNotFound,
		Fields: []lifecycleFieldUpdate{
			{Column: "worker_id", SQL: "NULL"},
			{Column: "lease_expires_at", SQL: "NULL"},
			{Column: "run_after", SQL: "now()+%s::interval", Args: []any{delay.String()}},
			{Column: "attempt", SQL: "GREATEST(attempt-1,0)"},
			{Column: "last_error", Args: []any{""}},
		},
	})
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `SELECT pg_notify('specrelay_jobs',$1)`, id.String()); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ReleaseLeasedJob is used when graceful shutdown starts after a worker has
// claimed a job but before it begins execution. Releasing it does not consume
// a retry attempt and preserves the task's queued state.
func (s *Store) ReleaseLeasedJob(ctx context.Context, id uuid.UUID, workerID string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	_, err = transitionLifecycleTx(ctx, tx, lifecycleTransitionRequest{
		LifecycleTransitionParams: LifecycleTransitionParams{
			ResourceType:        domain.LifecycleResourceJob,
			ResourceID:          id,
			Status:              "queued",
			StatusSource:        domain.LifecycleSourceBackend,
			ReasonCode:          domain.LifecycleReasonBackendShutdown,
			Reason:              "worker released leased job before execution",
			RecoveryHint:        domain.LifecycleRecoveryAutomaticRetry,
			ExecutionCheckpoint: mustJSON(map[string]any{"workerId": workerID}),
		},
		ExpectedStatuses: []string{"leased"},
		RequireWorkerID:  &workerID,
		MismatchError:    domain.ErrNotFound,
		Fields: []lifecycleFieldUpdate{
			{Column: "worker_id", SQL: "NULL"},
			{Column: "lease_expires_at", SQL: "NULL"},
			{Column: "run_after", SQL: "now()"},
			{Column: "attempt", SQL: "GREATEST(attempt-1,0)"},
		},
	})
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `SELECT pg_notify('specrelay_jobs',$1)`, id.String()); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) CompleteJob(ctx context.Context, id uuid.UUID, workerID string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	result, err := transitionLifecycleTx(ctx, tx, lifecycleTransitionRequest{
		LifecycleTransitionParams: LifecycleTransitionParams{
			ResourceType:        domain.LifecycleResourceJob,
			ResourceID:          id,
			Status:              "succeeded",
			StatusSource:        domain.LifecycleSourceWorker,
			ReasonCode:          domain.LifecycleReasonCompleted,
			Reason:              "worker completed job",
			RecoveryHint:        domain.LifecycleRecoveryNone,
			ExecutionCheckpoint: mustJSON(map[string]any{"workerId": workerID}),
		},
		ExpectedStatuses: []string{"running"},
		RequireWorkerID:  &workerID,
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

func (s *Store) FailJob(ctx context.Context, j domain.Job, workerID, message string, retryable bool) error {
	status := "failed"
	runAfter := time.Now()
	reasonCode := domain.LifecycleReasonExecutionFailed
	recoveryHint := domain.LifecycleRecoveryNone
	if retryable && j.Attempt < j.MaxAttempts {
		status = "retry_wait"
		delay := time.Duration(1<<min(j.Attempt, 6)) * time.Second
		runAfter = runAfter.Add(delay)
		reasonCode = domain.LifecycleReasonAutomaticRetry
		recoveryHint = domain.LifecycleRecoveryAutomaticRetry
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	_, err = transitionLifecycleTx(ctx, tx, lifecycleTransitionRequest{
		LifecycleTransitionParams: LifecycleTransitionParams{
			ResourceType:        domain.LifecycleResourceJob,
			ResourceID:          j.ID,
			Status:              status,
			StatusSource:        domain.LifecycleSourceWorker,
			ReasonCode:          reasonCode,
			Reason:              message,
			RecoveryHint:        recoveryHint,
			ExecutionCheckpoint: mustJSON(map[string]any{"workerId": workerID, "attempt": j.Attempt, "maxAttempts": j.MaxAttempts}),
		},
		ExpectedStatuses: []string{"leased", "running"},
		RequireWorkerID:  &workerID,
		AllowNonContract: true,
		IgnoreTerminal:   true,
		MismatchError:    domain.ErrNotFound,
		Fields: []lifecycleFieldUpdate{
			{Column: "last_error", Args: []any{message}},
			{Column: "run_after", Args: []any{runAfter}},
			{Column: "lease_expires_at", SQL: "NULL"},
		},
	})
	if err != nil {
		return err
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

// ReconcileRuntimeState is the single idempotent recovery center used both at
// backend startup and by periodic inspection. It combines durable runtime,
// process, job, workspace-lock, and Agent Run evidence. No decision is based on
// total CLI duration or on the time since the last output fragment.
type recoveryJob struct {
	ID, ProjectID, AggregateID uuid.UUID
	Type, Status, WorkerID     string
	LeaseExpiresAt             *time.Time
	Attempt, MaxAttempts       int
	ReasonCode                 string
	TaskStatus, PlanStatus     string
	PlanID                     *uuid.UUID
}

type recoveryRun struct {
	ID                  uuid.UUID
	Status              string
	PID                 *int
	OwnerInstanceID     string
	LogPath             string
	LastActivityAt      time.Time
	ExecutionCheckpoint json.RawMessage
	ReasonCode          string
}

type agentRunRecoveryCheckpoint struct {
	ProcessIdentity     string    `json:"processIdentity"`
	HeartbeatAt         time.Time `json:"heartbeatAt"`
	LastOutputAt        time.Time `json:"lastOutputAt"`
	LogActivityAt       time.Time `json:"logActivityAt"`
	HeartbeatIntervalMS int64     `json:"heartbeatIntervalMs"`
	Phase               string    `json:"phase"`
}

type recoveryEvidence struct {
	OwnerID             string
	OwnerFresh          bool
	JobLeaseValid       bool
	WorkspaceLeaseValid bool
	WorkspaceLockOwned  bool
	ProcessInspectable  bool
	ProcessKnown        bool
	ProcessAlive        bool
	ProcessIdentity     string
	HeartbeatFresh      bool
	LastActivityAt      time.Time
	LastOutputAt        time.Time
	LogActivityAt       time.Time
}

type recoveryOutcome struct {
	Status       string
	ReasonCode   domain.LifecycleReasonCode
	Reason       string
	RecoveryHint domain.LifecycleRecoveryHint
	Failure      string
}

func (s *Store) RecoverJobs(ctx context.Context) error {
	return s.ReconcileRuntimeState(ctx, "")
}

func (s *Store) ReconcileRuntimeState(ctx context.Context, observerInstanceID string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var now time.Time
	if err = tx.QueryRow(ctx, `SELECT now()`).Scan(&now); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT
		j.id,j.project_id,j.job_type,j.aggregate_id,j.status,coalesce(j.worker_id,''),
		j.lease_expires_at,j.attempt,j.max_attempts,j.reason_code,
		coalesce(t.status,''),t.plan_id,coalesce(p.status,'')
		FROM jobs j
		LEFT JOIN plan_tasks t ON j.job_type='task.execute' AND t.id=j.aggregate_id
		LEFT JOIN plans p ON p.id=t.plan_id
		WHERE j.status IN ('leased','running','cancelling')
		FOR UPDATE OF j`)
	if err != nil {
		return err
	}
	jobs := make([]recoveryJob, 0)
	for rows.Next() {
		var job recoveryJob
		if err = rows.Scan(&job.ID, &job.ProjectID, &job.Type, &job.AggregateID, &job.Status,
			&job.WorkerID, &job.LeaseExpiresAt, &job.Attempt, &job.MaxAttempts, &job.ReasonCode,
			&job.TaskStatus, &job.PlanID, &job.PlanStatus); err != nil {
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

	for _, job := range jobs {
		run, runErr := activeRecoveryRun(ctx, tx, job.ID)
		if runErr != nil {
			return runErr
		}
		evidence, evidenceErr := s.collectRecoveryEvidence(ctx, tx, now, observerInstanceID, job, run)
		if evidenceErr != nil {
			// An inconclusive host query is not process-exit evidence. Skip this
			// candidate and retry on the next pass rather than forging a terminal.
			continue
		}
		if recoveryCandidateActive(job, run, evidence) {
			continue
		}
		cancelIntent := recoveryCancellationIntent(job, run)
		if !cancelIntent && run == nil && job.TaskStatus != "running" && job.TaskStatus != "cancelling" {
			if err = s.requeueUnstartedJob(ctx, tx, now, job, evidence, observerInstanceID); err != nil {
				return err
			}
			continue
		}
		outcome := chooseRecoveryOutcome(job.Status, job.TaskStatus, run, cancelIntent, evidence)
		if err = s.convergeLostJob(ctx, tx, now, job, run, evidence, outcome, observerInstanceID); err != nil {
			return err
		}
	}

	if err = s.reconcileStandaloneRuns(ctx, tx, now, observerInstanceID); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM workspace_leases lease
		WHERE lease.expires_at<now()
			AND NOT EXISTS (SELECT 1 FROM jobs j WHERE j.id=lease.job_id AND j.status IN ('leased','running','cancelling'))`); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM runtime_instances owner
		WHERE owner.heartbeat_at<now()-interval '24 hours'
			AND NOT EXISTS (SELECT 1 FROM agent_runs run WHERE run.owner_instance_id=owner.instance_id AND run.status IN ('starting','running','cancelling'))
			AND NOT EXISTS (SELECT 1 FROM jobs j WHERE split_part(coalesce(j.worker_id,''),':',1)=owner.instance_id AND j.status IN ('leased','running','cancelling'))`); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func activeRecoveryRun(ctx context.Context, tx pgx.Tx, jobID uuid.UUID) (*recoveryRun, error) {
	var run recoveryRun
	err := tx.QueryRow(ctx, `SELECT id,status,pid,owner_instance_id,log_path,last_activity_at,execution_checkpoint,reason_code
		FROM agent_runs WHERE job_id=$1 AND status IN ('starting','running','cancelling')
		ORDER BY created_at DESC LIMIT 1 FOR UPDATE`, jobID).Scan(
		&run.ID, &run.Status, &run.PID, &run.OwnerInstanceID, &run.LogPath,
		&run.LastActivityAt, &run.ExecutionCheckpoint, &run.ReasonCode,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &run, nil
}

func (s *Store) collectRecoveryEvidence(ctx context.Context, tx pgx.Tx, now time.Time, observerInstanceID string, job recoveryJob, run *recoveryRun) (recoveryEvidence, error) {
	evidence := recoveryEvidence{
		JobLeaseValid:       job.LeaseExpiresAt != nil && !job.LeaseExpiresAt.Before(now),
		WorkspaceLeaseValid: job.Type != "task.execute" || job.TaskStatus == "" || job.TaskStatus == "pending" || job.TaskStatus == "queued",
		WorkspaceLockOwned:  job.Type != "task.execute" || job.TaskStatus == "" || job.TaskStatus == "pending" || job.TaskStatus == "queued",
	}
	if job.Type == "task.execute" && (job.TaskStatus == "running" || job.TaskStatus == "cancelling") {
		var expiresAt time.Time
		var workerID string
		err := tx.QueryRow(ctx, `SELECT expires_at,worker_id FROM workspace_leases WHERE job_id=$1`, job.ID).Scan(&expiresAt, &workerID)
		if err == nil {
			evidence.WorkspaceLeaseValid = !expiresAt.Before(now)
			evidence.WorkspaceLockOwned = workerID == job.WorkerID
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return recoveryEvidence{}, err
		}
	}
	if run != nil {
		evidence.OwnerID = strings.TrimSpace(run.OwnerInstanceID)
	}
	if evidence.OwnerID == "" {
		evidence.OwnerID = workerInstanceID(job.WorkerID)
	}
	if evidence.OwnerID != "" {
		owner, err := runtimeInstanceLiveness(ctx, tx, evidence.OwnerID, now)
		if err != nil {
			return recoveryEvidence{}, err
		}
		evidence.OwnerFresh = owner.Fresh
	}
	if run == nil {
		return evidence, nil
	}

	checkpoint := agentRunRecoveryCheckpoint{}
	_ = json.Unmarshal(run.ExecutionCheckpoint, &checkpoint)
	evidence.LastActivityAt = run.LastActivityAt
	evidence.LastOutputAt = checkpoint.LastOutputAt
	evidence.LogActivityAt = checkpoint.LogActivityAt
	for _, observed := range []time.Time{checkpoint.HeartbeatAt, checkpoint.LastOutputAt, checkpoint.LogActivityAt} {
		if observed.After(evidence.LastActivityAt) {
			evidence.LastActivityAt = observed
		}
	}
	if info, statErr := os.Stat(run.LogPath); statErr == nil {
		modified := info.ModTime().UTC()
		if modified.After(evidence.LogActivityAt) {
			evidence.LogActivityAt = modified
		}
		if modified.After(evidence.LastActivityAt) {
			evidence.LastActivityAt = modified
		}
	}
	heartbeatInterval := time.Duration(checkpoint.HeartbeatIntervalMS) * time.Millisecond
	if heartbeatInterval <= 0 {
		heartbeatInterval = 5 * time.Second
	}
	staleAfter := heartbeatInterval * 3
	if staleAfter < minimumRuntimeInstanceStaleAfter {
		staleAfter = minimumRuntimeInstanceStaleAfter
	}
	heartbeatEvidence := checkpoint.HeartbeatAt
	if evidence.LogActivityAt.After(heartbeatEvidence) {
		heartbeatEvidence = evidence.LogActivityAt
	}
	if heartbeatEvidence.IsZero() {
		heartbeatEvidence = run.LastActivityAt
	}
	evidence.HeartbeatFresh = !heartbeatEvidence.Before(now.Add(-staleAfter))

	// A PID is only meaningful on the runtime instance that started it. A
	// recovery pass from another desktop must not compare that PID with an
	// unrelated local process that happens to use the same number. The empty
	// observer is retained for the legacy RecoverJobs wrapper and tests.
	evidence.ProcessInspectable = observerInstanceID == "" || evidence.OwnerID == "" || evidence.OwnerID == observerInstanceID
	if evidence.ProcessInspectable && run.PID != nil && *run.PID > 0 {
		evidence.ProcessKnown = true
		process, inspectErr := agent.InspectProcess(*run.PID)
		if inspectErr != nil {
			return recoveryEvidence{}, inspectErr
		}
		evidence.ProcessIdentity = process.Identity
		evidence.ProcessAlive = process.Running
		if evidence.ProcessAlive && checkpoint.ProcessIdentity != "" && process.Identity != "" && checkpoint.ProcessIdentity != process.Identity {
			// PID reuse is explicit process-exit evidence for the original CLI.
			evidence.ProcessAlive = false
		}
	}
	return evidence, nil
}

func workerInstanceID(workerID string) string {
	workerID = strings.TrimSpace(workerID)
	if before, _, ok := strings.Cut(workerID, ":"); ok {
		return strings.TrimSpace(before)
	}
	return ""
}

func recoveryCandidateActive(job recoveryJob, run *recoveryRun, evidence recoveryEvidence) bool {
	// A positively identified process always wins over stale database clocks or
	// leases. This is the key protection for a long, silent CLI during a brief
	// database outage and for another still-active desktop instance.
	if evidence.ProcessKnown && evidence.ProcessAlive {
		return true
	}
	ownerHealthy := evidence.OwnerFresh
	if evidence.OwnerID == "" {
		ownerHealthy = evidence.JobLeaseValid
	}
	if run == nil {
		return ownerHealthy && evidence.JobLeaseValid
	}
	// Fresh runtime and Agent Run heartbeats are direct evidence that another
	// desktop still owns the execution. Do not rewrite its state merely because
	// a lease renewal is racing this transaction; stale leases remain evidence
	// and become decisive once either heartbeat is lost.
	if evidence.OwnerID != "" {
		return ownerHealthy && evidence.HeartbeatFresh
	}
	return evidence.JobLeaseValid && evidence.WorkspaceLeaseValid && evidence.WorkspaceLockOwned && evidence.HeartbeatFresh
}

func recoveryCancellationIntent(job recoveryJob, run *recoveryRun) bool {
	if job.Status == "cancelling" || job.TaskStatus == "cancelling" || job.TaskStatus == "cancelled" || job.PlanStatus == "cancelling" || job.PlanStatus == "cancelled" {
		return true
	}
	if job.ReasonCode == string(domain.LifecycleReasonCancellationRequested) || job.ReasonCode == string(domain.LifecycleReasonUserCancelled) {
		return true
	}
	return run != nil && (run.Status == "cancelling" || run.ReasonCode == string(domain.LifecycleReasonCancellationRequested) || run.ReasonCode == string(domain.LifecycleReasonUserCancelled))
}

func chooseRecoveryOutcome(jobStatus, taskStatus string, run *recoveryRun, cancelIntent bool, evidence recoveryEvidence) recoveryOutcome {
	if cancelIntent {
		return recoveryOutcome{
			Status: "cancelled", ReasonCode: domain.LifecycleReasonUserCancelled,
			Reason:       "persisted cancellation intent was confirmed after execution lost contact",
			RecoveryHint: domain.LifecycleRecoveryNone, Failure: "cancellation",
		}
	}
	if run != nil && run.PID == nil {
		return recoveryOutcome{
			Status: "cancelled", ReasonCode: domain.LifecycleReasonProcessLost,
			Reason:       "runtime owner disappeared before the agent process was durably started",
			RecoveryHint: domain.LifecycleRecoveryAutomaticRetry, Failure: "startup_abandoned",
		}
	}
	reason := "agent process exited or durable runtime and lease evidence was lost"
	if evidence.ProcessKnown && !evidence.ProcessAlive {
		reason = "persisted agent process is no longer running"
	}
	return recoveryOutcome{
		Status: "interrupted", ReasonCode: domain.LifecycleReasonProcessLost,
		Reason: reason, RecoveryHint: domain.LifecycleRecoveryRetryFromStart, Failure: "process_interrupted",
	}
}

func recoveryCheckpoint(evidence recoveryEvidence, outcome recoveryOutcome, observerInstanceID string, now time.Time) json.RawMessage {
	checkpoint := map[string]any{
		"recoveredAt": now.UTC(), "recoveryObserverInstanceId": observerInstanceID,
		"ownerInstanceId": evidence.OwnerID, "ownerHeartbeatFresh": evidence.OwnerFresh,
		"jobLeaseValid": evidence.JobLeaseValid, "workspaceLeaseValid": evidence.WorkspaceLeaseValid,
		"workspaceLockOwned": evidence.WorkspaceLockOwned,
		"processInspectable": evidence.ProcessInspectable, "processKnown": evidence.ProcessKnown,
		"processAlive": evidence.ProcessAlive, "processIdentityObserved": evidence.ProcessIdentity,
		"agentHeartbeatFresh":    evidence.HeartbeatFresh,
		"recoveryRecommendation": outcome.RecoveryHint,
	}
	if !evidence.LastActivityAt.IsZero() {
		checkpoint["lastObservedActivityAt"] = evidence.LastActivityAt.UTC()
	}
	if !evidence.LastOutputAt.IsZero() {
		checkpoint["lastOutputAt"] = evidence.LastOutputAt.UTC()
	}
	if !evidence.LogActivityAt.IsZero() {
		checkpoint["logActivityAt"] = evidence.LogActivityAt.UTC()
	}
	return mustJSON(checkpoint)
}

func (s *Store) requeueUnstartedJob(ctx context.Context, tx pgx.Tx, now time.Time, job recoveryJob, evidence recoveryEvidence, observerInstanceID string) error {
	const reason = "job ownership expired before execution reached a durable running phase"
	_, err := transitionLifecycleTx(ctx, tx, lifecycleTransitionRequest{
		LifecycleTransitionParams: LifecycleTransitionParams{
			ResourceType: domain.LifecycleResourceJob, ResourceID: job.ID, Status: "queued",
			StatusSource: domain.LifecycleSourceRecovery, ReasonCode: domain.LifecycleReasonProcessLost,
			Reason: reason, RecoveryHint: domain.LifecycleRecoveryAutomaticRetry,
			ExecutionCheckpoint: recoveryCheckpoint(evidence, recoveryOutcome{RecoveryHint: domain.LifecycleRecoveryAutomaticRetry}, observerInstanceID, now),
		},
		ExpectedStatuses: []string{"leased", "running", "cancelling"}, AllowNonContract: true, IgnoreTerminal: true,
		Fields: []lifecycleFieldUpdate{
			{Column: "worker_id", SQL: "NULL"}, {Column: "lease_expires_at", SQL: "NULL"},
			{Column: "run_after", SQL: "now()"}, {Column: "attempt", SQL: "GREATEST(attempt-1,0)"},
			{Column: "last_error", Args: []any{reason}},
		},
	})
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `SELECT pg_notify('specrelay_jobs',$1)`, job.ID.String())
	return err
}

func (s *Store) convergeLostJob(ctx context.Context, tx pgx.Tx, now time.Time, job recoveryJob, run *recoveryRun, evidence recoveryEvidence, outcome recoveryOutcome, observerInstanceID string) error {
	checkpoint := recoveryCheckpoint(evidence, outcome, observerInstanceID, now)
	if run != nil {
		runCheckpoint := mergeAgentRunCheckpoint(run.ExecutionCheckpoint, map[string]any{})
		var merged map[string]any
		_ = json.Unmarshal(runCheckpoint, &merged)
		var recovered map[string]any
		_ = json.Unmarshal(checkpoint, &recovered)
		for key, value := range recovered {
			merged[key] = value
		}
		runCheckpoint = mustJSON(merged)
		_, err := transitionLifecycleTx(ctx, tx, lifecycleTransitionRequest{
			LifecycleTransitionParams: LifecycleTransitionParams{
				ResourceType: domain.LifecycleResourceAgentRun, ResourceID: run.ID, Status: outcome.Status,
				StatusSource: domain.LifecycleSourceRecovery, ReasonCode: outcome.ReasonCode,
				Reason: outcome.Reason, LastActivityAt: evidence.LastActivityAt, RecoveryHint: outcome.RecoveryHint,
				ExecutionCheckpoint: runCheckpoint, RelatedJobID: &job.ID, RelatedRunID: &run.ID,
			},
			ExpectedStatuses: []string{"starting", "running", "cancelling"}, AllowNonContract: true, IgnoreTerminal: true,
			Fields: []lifecycleFieldUpdate{
				{Column: "termination_reason", Args: []any{outcome.Reason}},
				{Column: "failure_category", Args: []any{outcome.Failure}},
				{Column: "finished_at", SQL: "now()"},
			},
		})
		if err != nil {
			return err
		}
	}
	_, err := transitionLifecycleTx(ctx, tx, lifecycleTransitionRequest{
		LifecycleTransitionParams: LifecycleTransitionParams{
			ResourceType: domain.LifecycleResourceJob, ResourceID: job.ID, Status: outcome.Status,
			StatusSource: domain.LifecycleSourceRecovery, ReasonCode: outcome.ReasonCode,
			Reason: outcome.Reason, LastActivityAt: evidence.LastActivityAt, RecoveryHint: outcome.RecoveryHint,
			ExecutionCheckpoint: checkpoint, RelatedRunID: recoveryRunID(run),
		},
		ExpectedStatuses: []string{"leased", "running", "cancelling"}, AllowNonContract: true, IgnoreTerminal: true,
		Fields: []lifecycleFieldUpdate{
			{Column: "worker_id", SQL: "NULL"}, {Column: "lease_expires_at", SQL: "NULL"},
			{Column: "last_error", Args: []any{outcome.Reason}},
		},
	})
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM workspace_leases WHERE job_id=$1`, job.ID); err != nil {
		return err
	}
	return s.convergeRecoveredTask(ctx, tx, job, evidence, outcome, checkpoint)
}

func recoveryRunID(run *recoveryRun) *uuid.UUID {
	if run == nil {
		return nil
	}
	return &run.ID
}

func (s *Store) convergeRecoveredTask(ctx context.Context, tx pgx.Tx, job recoveryJob, evidence recoveryEvidence, outcome recoveryOutcome, checkpoint json.RawMessage) error {
	if job.Type != "task.execute" || job.TaskStatus == "" || job.TaskStatus == "succeeded" || job.TaskStatus == "failed" || job.TaskStatus == "interrupted" || job.TaskStatus == "cancelled" {
		return nil
	}
	taskStatus := outcome.Status
	taskFields := []lifecycleFieldUpdate{{Column: "finished_at", SQL: "now()"}}
	// A vanished runtime before a PID was persisted never durably started the
	// task. Keep the job/run cancellation for observability, but make the task
	// runnable again and block the plan for an explicit review/resume.
	recoverPendingTask := outcome.Status == "cancelled" && outcome.ReasonCode == domain.LifecycleReasonProcessLost && outcome.RecoveryHint == domain.LifecycleRecoveryAutomaticRetry
	if recoverPendingTask {
		taskStatus = "pending"
		taskFields = []lifecycleFieldUpdate{{Column: "started_at", SQL: "NULL"}, {Column: "finished_at", SQL: "NULL"}}
	}
	result, err := transitionLifecycleTx(ctx, tx, lifecycleTransitionRequest{
		LifecycleTransitionParams: LifecycleTransitionParams{
			ResourceType: domain.LifecycleResourceTask, ResourceID: job.AggregateID, Status: taskStatus,
			StatusSource: domain.LifecycleSourceRecovery, ReasonCode: outcome.ReasonCode,
			Reason: outcome.Reason, LastActivityAt: evidence.LastActivityAt, RecoveryHint: outcome.RecoveryHint,
			ExecutionCheckpoint: checkpoint, RelatedJobID: &job.ID, RelatedRunID: nil,
		},
		ExpectedStatuses: []string{"queued", "running", "cancelling"}, AllowNonContract: true, IgnoreTerminal: true,
		Fields: taskFields,
	})
	if err != nil {
		return err
	}
	if !result.Idempotent {
		eventType := "task." + taskStatus
		if _, err = insertEvent(ctx, tx, NewEvent{ProjectID: &job.ProjectID, Type: eventType, AggregateType: "task", AggregateID: job.AggregateID, ResourceVersion: result.State.Version, Payload: mustJSON(map[string]any{"reason": outcome.Reason})}); err != nil {
			return err
		}
	}
	if (outcome.Status == "cancelled" && !recoverPendingTask) || job.PlanID == nil || (job.PlanStatus != "running" && job.PlanStatus != "validating") {
		return nil
	}
	planResult, err := transitionLifecycleTx(ctx, tx, lifecycleTransitionRequest{
		LifecycleTransitionParams: LifecycleTransitionParams{
			ResourceType: domain.LifecycleResourcePlan, ResourceID: *job.PlanID, Status: "blocked",
			StatusSource: domain.LifecycleSourceRecovery, ReasonCode: outcome.ReasonCode,
			Reason: outcome.Reason, LastActivityAt: evidence.LastActivityAt,
			RecoveryHint: domain.LifecycleRecoveryManualReview, ExecutionCheckpoint: checkpoint,
		},
		ExpectedStatuses: []string{"running", "validating", "cancelling"}, AllowNonContract: true, IgnoreTerminal: true,
	})
	if err != nil {
		return err
	}
	if !planResult.Idempotent {
		_, err = insertEvent(ctx, tx, NewEvent{ProjectID: &job.ProjectID, Type: "plan.blocked", AggregateType: "plan", AggregateID: *job.PlanID, ResourceVersion: planResult.State.Version, Payload: mustJSON(map[string]any{"reason": outcome.Reason})})
	}
	return err
}

func (s *Store) reconcileStandaloneRuns(ctx context.Context, tx pgx.Tx, now time.Time, observerInstanceID string) error {
	rows, err := tx.Query(ctx, `SELECT run.id,run.status,run.pid,run.owner_instance_id,run.log_path,run.last_activity_at,run.execution_checkpoint,run.reason_code
		FROM agent_runs run
		LEFT JOIN jobs j ON j.id=run.job_id
		WHERE run.status IN ('starting','running','cancelling')
			AND (run.job_id IS NULL OR j.status NOT IN ('leased','running','cancelling'))
		FOR UPDATE OF run`)
	if err != nil {
		return err
	}
	runs := make([]recoveryRun, 0)
	for rows.Next() {
		var run recoveryRun
		if err = rows.Scan(&run.ID, &run.Status, &run.PID, &run.OwnerInstanceID, &run.LogPath, &run.LastActivityAt, &run.ExecutionCheckpoint, &run.ReasonCode); err != nil {
			rows.Close()
			return err
		}
		runs = append(runs, run)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, run := range runs {
		job := recoveryJob{Status: "running"}
		evidence, evidenceErr := s.collectRecoveryEvidence(ctx, tx, now, observerInstanceID, job, &run)
		if evidenceErr != nil {
			continue
		}
		if (evidence.ProcessKnown && evidence.ProcessAlive) || (evidence.OwnerFresh && evidence.HeartbeatFresh) {
			continue
		}
		outcome := chooseRecoveryOutcome("running", "", &run, run.Status == "cancelling", evidence)
		checkpoint := recoveryCheckpoint(evidence, outcome, observerInstanceID, now)
		var existing map[string]any
		_ = json.Unmarshal(run.ExecutionCheckpoint, &existing)
		var recovered map[string]any
		_ = json.Unmarshal(checkpoint, &recovered)
		for key, value := range recovered {
			existing[key] = value
		}
		_, err = transitionLifecycleTx(ctx, tx, lifecycleTransitionRequest{
			LifecycleTransitionParams: LifecycleTransitionParams{
				ResourceType: domain.LifecycleResourceAgentRun, ResourceID: run.ID, Status: outcome.Status,
				StatusSource: domain.LifecycleSourceRecovery, ReasonCode: outcome.ReasonCode,
				Reason: outcome.Reason, LastActivityAt: evidence.LastActivityAt, RecoveryHint: outcome.RecoveryHint,
				ExecutionCheckpoint: mustJSON(existing), RelatedRunID: &run.ID,
			},
			ExpectedStatuses: []string{"starting", "running", "cancelling"}, AllowNonContract: true, IgnoreTerminal: true,
			Fields: []lifecycleFieldUpdate{
				{Column: "termination_reason", Args: []any{outcome.Reason}}, {Column: "failure_category", Args: []any{outcome.Failure}},
				{Column: "finished_at", SQL: "now()"},
			},
		})
		if err != nil {
			return err
		}
	}
	return nil
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

var ErrLeaseLost = errors.New("execution lease lost")

// RenewJobLease keeps an active job owned by its worker. Jobs that only read
// the workspace (such as plan generation) do not need a workspace lease, but
// still need their queue lease renewed while the CLI is running.
func (s *Store) RenewJobLease(ctx context.Context, jobID uuid.UUID, workerID string, duration time.Duration) error {
	tag, err := s.Pool.Exec(ctx, `UPDATE jobs SET lease_expires_at=now()+$3::interval,updated_at=now() WHERE id=$1 AND worker_id=$2 AND status IN ('leased','running')`, jobID, workerID, duration.String())
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrLeaseLost
	}
	return nil
}

func (s *Store) RenewWorkspaceLease(ctx context.Context, jobID uuid.UUID, workerID string, duration time.Duration) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, `UPDATE workspace_leases SET heartbeat_at=now(),expires_at=now()+$3::interval,updated_at=now(),version=version+1 WHERE job_id=$1 AND worker_id=$2 AND expires_at>now()`, jobID, workerID, duration.String())
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrLeaseLost
	}
	tag, err = tx.Exec(ctx, `UPDATE jobs SET lease_expires_at=now()+$3::interval,updated_at=now() WHERE id=$1 AND worker_id=$2 AND status IN ('leased','running')`, jobID, workerID, duration.String())
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrLeaseLost
	}
	return tx.Commit(ctx)
}
func (s *Store) ReleaseWorkspaceLease(ctx context.Context, jobID uuid.UUID, workerID string) error {
	_, err := s.Pool.Exec(ctx, `DELETE FROM workspace_leases WHERE job_id=$1 AND worker_id=$2`, jobID, workerID)
	return err
}

type PlanWorkspaceState struct {
	NormalizedPath        string
	GitRoot               string
	GitRepositoryIdentity string
	GitBranch             string
	GitHead               string
	GitWorkspaceDigest    string
}

func (s *Store) SaveGeneratedPlan(ctx context.Context, intake domain.Intake, spec planspec.Spec, markdown string) (domain.Plan, []domain.PlanTask, error) {
	return s.saveGeneratedPlan(ctx, intake, spec, markdown, nil)
}

func (s *Store) SaveGeneratedPlanWithWorkspace(ctx context.Context, intake domain.Intake, spec planspec.Spec, markdown string, workspace PlanWorkspaceState) (domain.Plan, []domain.PlanTask, error) {
	return s.saveGeneratedPlan(ctx, intake, spec, markdown, &workspace)
}

func (s *Store) saveGeneratedPlan(ctx context.Context, intake domain.Intake, spec planspec.Spec, markdown string, workspace *PlanWorkspaceState) (domain.Plan, []domain.PlanTask, error) {
	validationProblems := planspec.ValidateProblems(&spec)
	executable := len(validationProblems) == 0
	specJSON, err := json.Marshal(spec)
	if err != nil {
		return domain.Plan{}, nil, err
	}
	problemsJSON, err := json.Marshal(validationProblems)
	if err != nil {
		return domain.Plan{}, nil, err
	}
	numberedTasks := []planspec.NumberedTask{}
	if executable {
		numberedTasks = planspec.Tasks(spec)
	}
	acceptanceSummary := mustJSON(map[string]any{
		"status": "pending", "total": len(numberedTasks), "pending": len(numberedTasks),
		"passed": 0, "failed": 0, "skipped": 0, "manualConfirmation": 0,
	})

	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return domain.Plan{}, nil, err
	}
	defer tx.Rollback(ctx)

	planID := uuid.New()
	p, err := scanPlanRecord(tx.QueryRow(ctx, `INSERT INTO plans(
		id,project_id,intake_id,title,spec,spec_version,compatibility_mode,is_executable,
		validation_problems,markdown,status,delivery_status,acceptance_summary,config_snapshot)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'generating',$11,$12,$13)
		RETURNING `+planRecordColumns,
		planID, intake.ProjectID, intake.ID, spec.Title, specJSON, spec.Version,
		spec.CompatibilityMode, executable, problemsJSON, markdown,
		domain.PlanDeliveryStatusPending, acceptanceSummary, intake.ConfigSnapshot))
	if err != nil {
		return p, nil, err
	}

	originalPositions := make(map[string]int, len(spec.Tasks))
	for i, task := range spec.Tasks {
		originalPositions[task.Key] = i + 1
	}
	tasks := make([]domain.PlanTask, 0, len(numberedTasks))
	for i, item := range numberedTasks {
		marshal := func(value any) (json.RawMessage, error) {
			encoded, marshalErr := json.Marshal(value)
			return json.RawMessage(encoded), marshalErr
		}
		marshalStrings := func(values []string) (json.RawMessage, error) {
			if values == nil {
				values = []string{}
			}
			return marshal(values)
		}
		marshalAcceptance := func(values []planspec.AcceptanceItem) (json.RawMessage, error) {
			if values == nil {
				values = []planspec.AcceptanceItem{}
			}
			return marshal(values)
		}
		scope, marshalErr := marshalStrings(item.Task.Scope)
		if marshalErr != nil {
			return p, nil, marshalErr
		}
		acceptance, marshalErr := marshalStrings(item.Task.Acceptance)
		if marshalErr != nil {
			return p, nil, marshalErr
		}
		dependencies, marshalErr := marshalStrings(item.Task.DependsOn)
		if marshalErr != nil {
			return p, nil, marshalErr
		}
		inputs, marshalErr := marshalStrings(item.Task.Inputs)
		if marshalErr != nil {
			return p, nil, marshalErr
		}
		outputs, marshalErr := marshalStrings(item.Task.Outputs)
		if marshalErr != nil {
			return p, nil, marshalErr
		}
		risks, marshalErr := marshalStrings(item.Task.Risks)
		if marshalErr != nil {
			return p, nil, marshalErr
		}
		commands, marshalErr := marshalStrings(item.Task.ValidationCommands)
		if marshalErr != nil {
			return p, nil, marshalErr
		}
		definition, marshalErr := marshalAcceptance(item.Task.AcceptanceItems)
		if marshalErr != nil {
			return p, nil, marshalErr
		}
		resultItems := make([]map[string]any, 0, len(item.Task.AcceptanceItems))
		for _, criterion := range item.Task.AcceptanceItems {
			resultItems = append(resultItems, map[string]any{
				"key": criterion.Key, "status": domain.AcceptanceStatusPending,
				"evidence": []any{}, "reason": "",
			})
		}
		acceptanceResult := mustJSON(map[string]any{
			"status": domain.AcceptanceStatusPending, "items": resultItems,
			"evidence": []any{}, "reason": "",
		})
		taskType := domain.PlanTaskTypeImplementation
		position := originalPositions[item.Key]
		if i == len(numberedTasks)-1 {
			taskType = domain.PlanTaskTypeFinalValidation
			position = len(spec.Tasks) + 1
		}
		if position == 0 {
			position = item.Position
		}
		t, insertErr := scanTaskRecord(tx.QueryRow(ctx, `INSERT INTO plan_tasks(
			id,project_id,plan_id,task_key,task_type,position,execution_order,dependency_keys,
			title,scope,inputs,outputs,risks,validation_commands,acceptance,
			acceptance_definition,acceptance_status,acceptance_result)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)
			RETURNING `+planTaskRecordColumns,
			uuid.New(), intake.ProjectID, planID, item.Key, taskType, position,
			item.Position, dependencies, item.Task.Title, scope, inputs, outputs, risks,
			commands, acceptance, definition, domain.AcceptanceStatusPending, acceptanceResult))
		if insertErr != nil {
			return p, nil, insertErr
		}
		tasks = append(tasks, t)
	}

	targetStatus := "ready"
	reasonCode := domain.LifecycleReasonCompleted
	reason := "plan generation completed"
	allowNonContract := false
	if !executable {
		targetStatus = "blocked"
		reasonCode = domain.LifecycleReasonValidationFailed
		reason = "plan execution graph validation failed"
		allowNonContract = true
	}
	transition, err := transitionLifecycleTx(ctx, tx, lifecycleTransitionRequest{
		LifecycleTransitionParams: LifecycleTransitionParams{
			ResourceType: domain.LifecycleResourcePlan,
			ResourceID:   p.ID,
			Status:       targetStatus,
			StatusSource: domain.LifecycleSourceWorker,
			ReasonCode:   reasonCode,
			Reason:       reason,
			RecoveryHint: domain.LifecycleRecoveryNone,
		},
		ExpectedStatuses: []string{"generating"},
		AllowNonContract: allowNonContract,
	})
	if err != nil {
		return p, nil, err
	}
	p.Status = transition.State.Status
	p.Version = transition.State.Version
	if err = tx.QueryRow(ctx, `SELECT updated_at,content_version FROM plans WHERE id=$1`, p.ID).Scan(&p.UpdatedAt, &p.ContentVersion); err != nil {
		return p, nil, err
	}

	var intakeVersion int64
	err = tx.QueryRow(ctx, `UPDATE intakes SET status='planned',updated_at=now(),version=version+1 WHERE id=$1 AND status='planning' RETURNING version`, intake.ID).Scan(&intakeVersion)
	if err != nil {
		return p, nil, err
	}

	var baseline *domain.PlanExecutionSnapshot
	if executable {
		captured, captureErr := s.capturePlanExecutionSnapshotTx(ctx, tx, p.ID, domain.PlanSnapshotKindGenerationBaseline, nil, "")
		if captureErr != nil {
			return p, nil, captureErr
		}
		if workspace != nil {
			captured, captureErr = updatePlanSnapshotWorkspaceTx(ctx, tx, captured, *workspace)
			if captureErr != nil {
				return p, nil, captureErr
			}
		}
		baseline = &captured
		p.ExecutionSnapshotID = &captured.ID
		p.ExecutionSnapshotSequence = captured.Sequence
		p.DriftStatus = domain.PlanDriftStatusClean
		p.DriftResolutionRequired = false

		var previousPlanID uuid.UUID
		err = tx.QueryRow(ctx, `SELECT id FROM plans WHERE intake_id=$1 AND id<>$2 ORDER BY created_at DESC LIMIT 1 FOR UPDATE`, intake.ID, p.ID).Scan(&previousPlanID)
		if err == nil {
			previousSnapshot, snapshotErr := latestPlanExecutionSnapshot(ctx, tx, previousPlanID)
			if snapshotErr != nil && !errors.Is(snapshotErr, pgx.ErrNoRows) {
				return p, nil, snapshotErr
			}
			var originalSnapshotID *uuid.UUID
			var previousSpecDigest any
			if snapshotErr == nil {
				originalSnapshotID = &previousSnapshot.ID
				previousSpecDigest = previousSnapshot.PlanSpecDigest
			}
			diff := mustJSON(map[string]any{
				"planSpecDigest": map[string]any{"before": previousSpecDigest, "after": captured.PlanSpecDigest},
				"targetPlanId":   p.ID,
			})
			if _, auditErr := insertPlanDriftAuditTx(ctx, tx, p.ProjectID, previousPlanID, domain.PlanDriftAuditPlanRegenerated, originalSnapshotID, nil, &p.ID, diff, "repository", "plan regenerated by a later generation request"); auditErr != nil {
				return p, nil, auditErr
			}
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return p, nil, err
		}
	} else {
		p.DriftStatus = domain.PlanDriftStatusMissingBaseline
		p.DriftResolutionRequired = true
	}

	planEventType := "plan.ready"
	planPayload := map[string]any{"intakeId": intake.ID, "taskCount": len(tasks)}
	if baseline != nil {
		planPayload["executionSnapshotId"] = baseline.ID
	} else {
		planEventType = "plan.blocked"
		planPayload["validationProblems"] = validationProblems
	}
	if _, err = insertEvent(ctx, tx, NewEvent{ProjectID: &p.ProjectID, Type: planEventType, AggregateType: "plan", AggregateID: p.ID, ResourceVersion: p.Version, Payload: mustJSON(planPayload)}); err != nil {
		return p, nil, err
	}
	if _, err = insertEvent(ctx, tx, NewEvent{ProjectID: &p.ProjectID, Type: "intake.planned", AggregateType: "intake", AggregateID: intake.ID, ResourceVersion: intakeVersion, Payload: mustJSON(map[string]any{"planId": p.ID, "executable": executable})}); err != nil {
		return p, nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return p, nil, err
	}
	return p, tasks, nil
}

func updatePlanSnapshotWorkspaceTx(ctx context.Context, tx pgx.Tx, snapshot domain.PlanExecutionSnapshot, workspace PlanWorkspaceState) (domain.PlanExecutionSnapshot, error) {
	err := tx.QueryRow(ctx, `UPDATE plan_execution_snapshots
		SET workspace_path_normalized=$2,git_root=$3,git_repository_identity=$4,
			git_branch=$5,git_head=$6,git_workspace_digest=$7
		WHERE id=$1
		RETURNING `+planExecutionSnapshotColumns,
		snapshot.ID, workspace.NormalizedPath, workspace.GitRoot, workspace.GitRepositoryIdentity,
		workspace.GitBranch, workspace.GitHead, workspace.GitWorkspaceDigest).Scan(
		&snapshot.ID, &snapshot.ProjectID, &snapshot.PlanID, &snapshot.IntakeID,
		&snapshot.PreviousSnapshotID, &snapshot.TaskID, &snapshot.Sequence, &snapshot.Kind,
		&snapshot.RequirementID, &snapshot.RequirementVersion, &snapshot.RequirementDigest,
		&snapshot.PlanResourceVersion, &snapshot.PlanContentVersion, &snapshot.PlanSpecDigest,
		&snapshot.ProjectVersion, &snapshot.ConfigVersion, &snapshot.KeyExecutionFields,
		&snapshot.GenerationProvider, &snapshot.ExecutionProvider, &snapshot.WorkspacePathNormalized,
		&snapshot.GitRoot, &snapshot.GitRepositoryIdentity, &snapshot.GitBranch, &snapshot.GitHead,
		&snapshot.GitWorkspaceDigest, &snapshot.CreatedAt)
	return snapshot, err
}

type planExecutionState struct {
	PlanID             uuid.UUID
	ProjectID          uuid.UUID
	Version            int64
	Status             string
	Executable         bool
	CompatibilityMode  bool
	ValidationProblems json.RawMessage
	ConfigSnapshot     json.RawMessage
	Tasks              []domain.PlanTask
}

func loadPlanExecutionStateTx(ctx context.Context, tx pgx.Tx, planID uuid.UUID) (planExecutionState, error) {
	state := planExecutionState{PlanID: planID}
	err := tx.QueryRow(ctx, `SELECT project_id,version,status,is_executable,compatibility_mode,validation_problems,config_snapshot
		FROM plans WHERE id=$1 FOR UPDATE`, planID).Scan(
		&state.ProjectID, &state.Version, &state.Status, &state.Executable,
		&state.CompatibilityMode, &state.ValidationProblems, &state.ConfigSnapshot,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return state, domain.ErrNotFound
	}
	if err != nil {
		return state, err
	}
	order := "execution_order,position,id"
	if state.CompatibilityMode {
		order = "position,id"
	}
	rows, err := tx.Query(ctx, `SELECT `+planTaskRecordColumns+` FROM plan_tasks WHERE plan_id=$1 ORDER BY `+order+` FOR UPDATE`, planID)
	if err != nil {
		return state, err
	}
	defer rows.Close()
	for rows.Next() {
		task, scanErr := scanTaskRecord(rows)
		if scanErr != nil {
			return state, scanErr
		}
		state.Tasks = append(state.Tasks, task)
	}
	if err = rows.Err(); err != nil {
		return state, err
	}
	if state.CompatibilityMode {
		for i := range state.Tasks {
			task := &state.Tasks[i]
			if task.Status != "succeeded" || task.AcceptanceStatus != domain.AcceptanceStatusPending || !legacyPendingAcceptanceResult(task.AcceptanceResult) {
				continue
			}
			result := buildAcceptanceResult(task.AcceptanceDefinition, domain.AcceptanceStatusPassed, "legacy succeeded task acceptance recovered")
			if err = tx.QueryRow(ctx, `UPDATE plan_tasks
				SET acceptance_status=$2,acceptance_result=$3,updated_at=now(),version=version+1
				WHERE id=$1 AND status='succeeded' AND acceptance_status='pending'
				RETURNING acceptance_status,acceptance_result,updated_at,version`,
				task.ID, domain.AcceptanceStatusPassed, result,
			).Scan(&task.AcceptanceStatus, &task.AcceptanceResult, &task.UpdatedAt, &task.Version); err != nil {
				return state, err
			}
		}
	}
	return state, nil
}

func legacyPendingAcceptanceResult(raw json.RawMessage) bool {
	var result struct {
		Status string `json:"status"`
		Items  []struct {
			Status   string            `json:"status"`
			Reason   string            `json:"reason"`
			Evidence []json.RawMessage `json:"evidence"`
		} `json:"items"`
		Reason   string            `json:"reason"`
		Evidence []json.RawMessage `json:"evidence"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &result) != nil || result.Status != domain.AcceptanceStatusPending || strings.TrimSpace(result.Reason) != "" || len(result.Evidence) != 0 {
		return false
	}
	for _, item := range result.Items {
		if item.Status != domain.AcceptanceStatusPending || strings.TrimSpace(item.Reason) != "" || len(item.Evidence) != 0 {
			return false
		}
	}
	return true
}

func schedulingBlocked(planID uuid.UUID, taskID *uuid.UUID, blockers ...domain.PlanExecutionBlocker) error {
	return &domain.PlanExecutionBlockedError{PlanID: planID, TaskID: taskID, Blockers: blockers}
}

func taskSchedulingBlocker(code string, task domain.PlanTask, reason string) domain.PlanExecutionBlocker {
	id := task.ID
	return domain.PlanExecutionBlocker{
		Code: code, TaskID: &id, TaskKey: task.TaskKey, TaskTitle: task.Title,
		TaskStatus: task.Status, AcceptanceStatus: task.AcceptanceStatus, Reason: reason,
	}
}

func planValidationSchedulingBlocker(state planExecutionState, reason string) domain.PlanExecutionBlocker {
	problems := append(json.RawMessage(nil), state.ValidationProblems...)
	return domain.PlanExecutionBlocker{
		Code: "plan_invalid", Reason: reason, ValidationProblems: problems,
	}
}

func validationProblemSummary(raw json.RawMessage) string {
	var problems []struct {
		Code    string `json:"code"`
		Path    string `json:"path"`
		Message string `json:"message"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &problems) != nil || len(problems) == 0 {
		return "plan validation is invalid"
	}
	parts := make([]string, 0, len(problems))
	for _, problem := range problems {
		label := strings.TrimSpace(problem.Path)
		if label == "" {
			label = strings.TrimSpace(problem.Code)
		}
		message := strings.TrimSpace(problem.Message)
		if label != "" && message != "" {
			parts = append(parts, label+": "+message)
		} else if message != "" {
			parts = append(parts, message)
		} else if label != "" {
			parts = append(parts, label)
		}
	}
	if len(parts) == 0 {
		return "plan validation is invalid"
	}
	return "plan validation is invalid: " + strings.Join(parts, ", ")
}

func acceptanceStatusPassed(status string) bool {
	return status == domain.AcceptanceStatusPassed || status == domain.AcceptanceStatusSkipped
}

func acceptanceSchedulingBlocker(task domain.PlanTask) (domain.PlanExecutionBlocker, bool) {
	blocker := taskSchedulingBlocker(
		"acceptance_not_passed", task,
		fmt.Sprintf("task %s acceptance is %s", task.TaskKey, task.AcceptanceStatus),
	)
	blocked := !acceptanceStatusPassed(task.AcceptanceStatus)
	var definition []struct {
		Key string `json:"key"`
	}
	if len(task.AcceptanceDefinition) == 0 || json.Unmarshal(task.AcceptanceDefinition, &definition) != nil {
		blocker.Code = "acceptance_invalid"
		blocker.Reason += "; acceptance definition is invalid"
		return blocker, true
	}
	var result struct {
		Reason string `json:"reason"`
		Items  []struct {
			Key    string `json:"key"`
			Status string `json:"status"`
			Reason string `json:"reason"`
		} `json:"items"`
	}
	if len(task.AcceptanceResult) == 0 || json.Unmarshal(task.AcceptanceResult, &result) != nil {
		blocker.Code = "acceptance_invalid"
		blocker.Reason += "; acceptance result is invalid"
		return blocker, true
	}
	resultByKey := make(map[string]domain.PlanExecutionAcceptanceBlocker, len(result.Items))
	for _, item := range result.Items {
		resultByKey[item.Key] = domain.PlanExecutionAcceptanceBlocker{Key: item.Key, Status: item.Status, Reason: item.Reason}
	}
	for _, criterion := range definition {
		item, exists := resultByKey[criterion.Key]
		if !exists {
			blocked = true
			blocker.AcceptanceBlockers = append(blocker.AcceptanceBlockers, domain.PlanExecutionAcceptanceBlocker{
				Key: criterion.Key, Status: "missing", Reason: "required acceptance item has no persisted result",
			})
			continue
		}
		if acceptanceStatusPassed(item.Status) {
			continue
		}
		blocked = true
		blocker.AcceptanceBlockers = append(blocker.AcceptanceBlockers, item)
	}
	if !blocked {
		return domain.PlanExecutionBlocker{}, false
	}
	if reason := strings.TrimSpace(result.Reason); reason != "" {
		blocker.Reason += ": " + reason
	}
	if len(blocker.AcceptanceBlockers) > 0 {
		items := make([]string, 0, len(blocker.AcceptanceBlockers))
		for _, item := range blocker.AcceptanceBlockers {
			itemReason := item.Key + "=" + item.Status
			if strings.TrimSpace(item.Reason) != "" {
				itemReason += " (" + strings.TrimSpace(item.Reason) + ")"
			}
			items = append(items, itemReason)
		}
		blocker.Reason += "; blocking acceptance items: " + strings.Join(items, ", ")
	}
	return blocker, true
}

func validatePlanExecutionState(state planExecutionState) []domain.PlanExecutionBlocker {
	if !state.Executable {
		return []domain.PlanExecutionBlocker{planValidationSchedulingBlocker(state, validationProblemSummary(state.ValidationProblems))}
	}
	var persistedProblems []json.RawMessage
	if len(state.ValidationProblems) > 0 && json.Unmarshal(state.ValidationProblems, &persistedProblems) == nil && len(persistedProblems) > 0 {
		return []domain.PlanExecutionBlocker{planValidationSchedulingBlocker(state, validationProblemSummary(state.ValidationProblems))}
	}
	if len(state.Tasks) == 0 {
		return []domain.PlanExecutionBlocker{planValidationSchedulingBlocker(state, "plan has no executable tasks")}
	}
	if state.CompatibilityMode {
		return nil
	}
	byKey := make(map[string]int, len(state.Tasks))
	finalValidationCount := 0
	for i, task := range state.Tasks {
		if _, exists := byKey[task.TaskKey]; exists {
			return []domain.PlanExecutionBlocker{planValidationSchedulingBlocker(state, fmt.Sprintf("plan execution graph contains duplicate task key %s", task.TaskKey))}
		}
		byKey[task.TaskKey] = i
		if task.TaskType == domain.PlanTaskTypeFinalValidation {
			finalValidationCount++
			if i != len(state.Tasks)-1 {
				return []domain.PlanExecutionBlocker{taskSchedulingBlocker("plan_invalid", task, fmt.Sprintf("final validation task %s is not last in topological execution order", task.TaskKey))}
			}
		}
	}
	if finalValidationCount != 1 {
		return []domain.PlanExecutionBlocker{planValidationSchedulingBlocker(state, "plan execution graph must contain exactly one final validation task")}
	}
	for i, task := range state.Tasks {
		var acceptance []struct {
			Key string `json:"key"`
		}
		if len(task.AcceptanceDefinition) == 0 || json.Unmarshal(task.AcceptanceDefinition, &acceptance) != nil || len(acceptance) == 0 {
			return []domain.PlanExecutionBlocker{taskSchedulingBlocker("plan_invalid", task, fmt.Sprintf("task %s has invalid or empty required acceptance metadata", task.TaskKey))}
		}
		acceptanceKeys := make(map[string]bool, len(acceptance))
		for _, criterion := range acceptance {
			key := strings.TrimSpace(criterion.Key)
			if key == "" || acceptanceKeys[key] {
				return []domain.PlanExecutionBlocker{taskSchedulingBlocker("plan_invalid", task, fmt.Sprintf("task %s has invalid required acceptance item key %q", task.TaskKey, key))}
			}
			acceptanceKeys[key] = true
		}
		var dependencies []string
		if len(task.DependencyKeys) == 0 || json.Unmarshal(task.DependencyKeys, &dependencies) != nil {
			return []domain.PlanExecutionBlocker{taskSchedulingBlocker("plan_invalid", task, fmt.Sprintf("task %s has invalid dependency metadata", task.TaskKey))}
		}
		seen := make(map[string]bool, len(dependencies))
		for _, dependency := range dependencies {
			if seen[dependency] {
				return []domain.PlanExecutionBlocker{taskSchedulingBlocker("plan_invalid", task, fmt.Sprintf("task %s repeats dependency %s", task.TaskKey, dependency))}
			}
			seen[dependency] = true
			dependencyIndex, exists := byKey[dependency]
			if !exists {
				return []domain.PlanExecutionBlocker{taskSchedulingBlocker("plan_invalid", task, fmt.Sprintf("task %s depends on missing task %s", task.TaskKey, dependency))}
			}
			if dependencyIndex >= i {
				return []domain.PlanExecutionBlocker{taskSchedulingBlocker("plan_invalid", task, fmt.Sprintf("task %s dependency %s is not earlier in topological execution order", task.TaskKey, dependency))}
			}
		}
	}
	return nil
}

func activeTaskBlockers(state planExecutionState, allowedTaskID *uuid.UUID) []domain.PlanExecutionBlocker {
	blockers := []domain.PlanExecutionBlocker{}
	for _, task := range state.Tasks {
		if task.Status != "queued" && task.Status != "running" {
			continue
		}
		if allowedTaskID != nil && task.ID == *allowedTaskID {
			continue
		}
		blockers = append(blockers, taskSchedulingBlocker(
			"active_task", task,
			fmt.Sprintf("task %s is already %s; a plan may have only one active task", task.TaskKey, task.Status),
		))
	}
	return blockers
}

func dependencyBlockers(state planExecutionState, task domain.PlanTask) []domain.PlanExecutionBlocker {
	if state.CompatibilityMode {
		return nil
	}
	var dependencies []string
	if json.Unmarshal(task.DependencyKeys, &dependencies) != nil {
		return []domain.PlanExecutionBlocker{taskSchedulingBlocker("plan_invalid", task, fmt.Sprintf("task %s has invalid dependency metadata", task.TaskKey))}
	}
	byKey := make(map[string]domain.PlanTask, len(state.Tasks))
	for _, candidate := range state.Tasks {
		byKey[candidate.TaskKey] = candidate
	}
	blockers := []domain.PlanExecutionBlocker{}
	for _, dependencyKey := range dependencies {
		dependency := byKey[dependencyKey]
		if dependency.Status != "succeeded" {
			blockers = append(blockers, taskSchedulingBlocker(
				"dependency_not_succeeded", dependency,
				fmt.Sprintf("dependency task %s for task %s is %s, not succeeded", dependency.TaskKey, task.TaskKey, dependency.Status),
			))
			continue
		}
		if blocker, blocked := acceptanceSchedulingBlocker(dependency); blocked {
			blockers = append(blockers, blocker)
		}
	}
	return blockers
}

// nextPlanTask applies the same persisted graph gate for plan start, automatic
// continuation, manual retry, and worker start. automatic is deliberately more
// conservative: failed/interrupted/cancelled tasks require an explicit retry.
func nextPlanTask(state planExecutionState, automatic bool, allowedActiveTaskID *uuid.UUID) (*domain.PlanTask, bool, []domain.PlanExecutionBlocker) {
	if blockers := validatePlanExecutionState(state); len(blockers) > 0 {
		return nil, false, blockers
	}
	if blockers := activeTaskBlockers(state, allowedActiveTaskID); len(blockers) > 0 {
		return nil, false, blockers
	}
	for i := range state.Tasks {
		task := state.Tasks[i]
		if task.Status == "succeeded" {
			if blocker, blocked := acceptanceSchedulingBlocker(task); blocked {
				return nil, false, []domain.PlanExecutionBlocker{blocker}
			}
			continue
		}
		if blockers := dependencyBlockers(state, task); len(blockers) > 0 {
			return nil, false, blockers
		}
		if automatic && task.Status != "pending" {
			return nil, false, []domain.PlanExecutionBlocker{taskSchedulingBlocker(
				"manual_retry_required", task,
				fmt.Sprintf("task %s is %s and requires an explicit retry", task.TaskKey, task.Status),
			)}
		}
		return &task, false, nil
	}
	return nil, true, nil
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
	var status string
	err = tx.QueryRow(ctx, `SELECT p.version,p.status
		FROM plans p
		JOIN projects pr ON pr.id=p.project_id
		WHERE p.id=$1 AND pr.automation_enabled=true
		FOR UPDATE OF p, pr`, planID).Scan(&version, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Job{}, false, nil
	}
	if err != nil {
		return domain.Job{}, false, err
	}
	if status != "ready" && status != "blocked" {
		return domain.Job{}, false, nil
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

func requirePlanExecutionBaseline(ctx context.Context, q snapshotQueryer, planID uuid.UUID) error {
	var planExists, baselineExists bool
	if err := q.QueryRow(ctx, `SELECT
		EXISTS(SELECT 1 FROM plans WHERE id=$1),
		EXISTS(SELECT 1 FROM plan_execution_snapshots WHERE plan_id=$1)`, planID).Scan(&planExists, &baselineExists); err != nil {
		return err
	}
	if !planExists {
		return domain.ErrNotFound
	}
	if !baselineExists {
		return errors.Join(domain.ErrPlanDriftResolutionRequired, domain.ErrPlanExecutionBaselineMissing)
	}
	return nil
}

// queuePlanTx moves a plan to running, queues its next task, and creates the
// corresponding task job. automatic limits the transition to ready plans so
// automation never restarts a blocked plan without user action.
func (s *Store) queuePlanTx(ctx context.Context, tx pgx.Tx, planID uuid.UUID, version int64, automatic bool) (domain.Job, error) {
	state, err := loadPlanExecutionStateTx(ctx, tx, planID)
	if err != nil {
		return domain.Job{}, err
	}
	if state.Version != version {
		return domain.Job{}, domain.ErrVersionConflict
	}
	allowedPlanStatuses := []string{"ready", "blocked"}
	if automatic {
		allowedPlanStatuses = []string{"ready"}
	}
	if !lifecycleContains(allowedPlanStatuses, state.Status) {
		if automatic && state.Status == "blocked" {
			nextTask, complete, blockers := nextPlanTask(state, true, nil)
			if len(blockers) > 0 {
				return domain.Job{}, schedulingBlocked(planID, nil, blockers...)
			}
			if complete || nextTask == nil {
				return domain.Job{}, schedulingBlocked(planID, nil, domain.PlanExecutionBlocker{Code: "no_runnable_task", Reason: "blocked plan has no unfinished runnable task"})
			}
			return domain.Job{}, schedulingBlocked(planID, nil, taskSchedulingBlocker(
				"manual_resume_required", *nextTask,
				fmt.Sprintf("plan is blocked; task %s requires an explicit manual resume", nextTask.TaskKey),
			))
		}
		return domain.Job{}, domain.ErrInvalidTransition
	}
	nextTask, complete, blockers := nextPlanTask(state, automatic, nil)
	if len(blockers) > 0 {
		return domain.Job{}, schedulingBlocked(planID, nil, blockers...)
	}
	if complete || nextTask == nil {
		return domain.Job{}, schedulingBlocked(planID, nil, domain.PlanExecutionBlocker{Code: "no_runnable_task", Reason: "plan has no unfinished runnable task"})
	}
	if !lifecycleContains([]string{"pending", "failed", "cancelled", "interrupted"}, nextTask.Status) {
		return domain.Job{}, domain.ErrInvalidTransition
	}
	if err = requirePlanExecutionBaseline(ctx, tx, planID); err != nil {
		return domain.Job{}, err
	}

	provider, providerRequested := executionProviderFromContext(ctx)
	if automatic {
		provider = ""
		providerRequested = false
	}
	configSnapshot, err := planConfigSnapshotWithProvider(state.ConfigSnapshot, provider)
	if err != nil {
		return domain.Job{}, err
	}
	jobID := uuid.New()
	statusSource := domain.LifecycleSourceUser
	if automatic {
		statusSource = domain.LifecycleSourceAutomation
	}
	planTransition, err := transitionLifecycleTx(ctx, tx, lifecycleTransitionRequest{
		LifecycleTransitionParams: LifecycleTransitionParams{
			ResourceType:        domain.LifecycleResourcePlan,
			ResourceID:          planID,
			ExpectedVersion:     version,
			Status:              "running",
			StatusSource:        statusSource,
			ReasonCode:          domain.LifecycleReasonResumeRequested,
			Reason:              "plan execution queued",
			RecoveryHint:        domain.LifecycleRecoveryNone,
			ExecutionCheckpoint: mustJSON(map[string]any{"taskId": nextTask.ID}),
			RelatedJobID:        &jobID,
		},
		ExpectedStatuses: allowedPlanStatuses,
		AllowNonContract: true,
		Fields:           []lifecycleFieldUpdate{{Column: "execution_started_at", SQL: "now()"}},
	})
	if err != nil {
		return domain.Job{}, err
	}
	taskTransition, err := transitionLifecycleTx(ctx, tx, lifecycleTransitionRequest{
		LifecycleTransitionParams: LifecycleTransitionParams{
			ResourceType: domain.LifecycleResourceTask,
			ResourceID:   nextTask.ID,
			Status:       "queued",
			StatusSource: planTransition.State.StatusSource,
			ReasonCode:   domain.LifecycleReasonResumeRequested,
			Reason:       "earliest dependency-ready task queued",
			RecoveryHint: domain.LifecycleRecoveryNone,
			RelatedJobID: &jobID,
		},
		ExpectedStatuses: []string{nextTask.Status},
		AllowNonContract: true,
		AllowTerminal:    true,
	})
	if err != nil {
		return domain.Job{}, err
	}
	if _, err = tx.Exec(ctx, `UPDATE plans SET config_snapshot=$2 WHERE id=$1`, planID, configSnapshot); err != nil {
		return domain.Job{}, err
	}
	if canonicalJSONDigest(state.ConfigSnapshot) != canonicalJSONDigest(configSnapshot) {
		previous, snapshotErr := latestPlanExecutionSnapshot(ctx, tx, planID)
		if snapshotErr != nil {
			return domain.Job{}, snapshotErr
		}
		accepted, snapshotErr := s.capturePlanExecutionSnapshotTx(ctx, tx, planID, domain.PlanSnapshotKindUserAccepted, nil, previous.GenerationProvider)
		if snapshotErr != nil {
			return domain.Job{}, snapshotErr
		}
		providerBefore, providerErr := providerFromPlanConfigSnapshot(state.ConfigSnapshot)
		if providerErr != nil {
			return domain.Job{}, providerErr
		}
		providerAfter, providerErr := providerFromPlanConfigSnapshot(configSnapshot)
		if providerErr != nil {
			return domain.Job{}, providerErr
		}
		if _, snapshotErr = insertPlanDriftAuditTx(ctx, tx, state.ProjectID, planID, domain.PlanDriftAuditSnapshotUpdated,
			&previous.ID, &accepted.ID, nil, mustJSON(map[string]any{
				"executionProvider": map[string]string{"before": providerBefore, "after": providerAfter},
			}), "queue", "execution provider selected when plan queued"); snapshotErr != nil {
			return domain.Job{}, snapshotErr
		}
	}
	maxAttempts, err := projectMaxAttempts(ctx, tx, state.ProjectID)
	if err != nil {
		return domain.Job{}, err
	}
	job, err := insertJob(ctx, tx, NewJob{ID: jobID, ProjectID: state.ProjectID, Type: "task.execute", AggregateType: "task", AggregateID: nextTask.ID, Payload: taskExecutionJobPayload(nextTask.ID, planID, provider, providerRequested), Priority: 100, MaxAttempts: maxAttempts, RunAfter: time.Now(), IdempotencyKey: fmt.Sprintf("task.execute:%s:%d", nextTask.ID, taskTransition.State.Version)})
	if err != nil {
		return domain.Job{}, err
	}
	_, err = insertEvent(ctx, tx, NewEvent{ProjectID: &state.ProjectID, Type: "plan.running", AggregateType: "plan", AggregateID: planID, ResourceVersion: planTransition.State.Version, Payload: mustJSON(map[string]any{"jobId": job.ID, "taskId": nextTask.ID})})
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

	var planID uuid.UUID
	if err = tx.QueryRow(ctx, `SELECT plan_id FROM plan_tasks WHERE id=$1`, taskID).Scan(&planID); errors.Is(err, pgx.ErrNoRows) {
		return domain.Job{}, domain.ErrNotFound
	} else if err != nil {
		return domain.Job{}, err
	}
	state, err := loadPlanExecutionStateTx(ctx, tx, planID)
	if err != nil {
		return domain.Job{}, err
	}
	var task *domain.PlanTask
	for i := range state.Tasks {
		if state.Tasks[i].ID == taskID {
			task = &state.Tasks[i]
			break
		}
	}
	if task == nil {
		return domain.Job{}, domain.ErrNotFound
	}
	if task.Version != version {
		return domain.Job{}, domain.ErrVersionConflict
	}
	if !lifecycleContains([]string{"running", "ready", "blocked", "interrupted"}, state.Status) {
		return domain.Job{}, domain.ErrInvalidTransition
	}
	nextTask, complete, blockers := nextPlanTask(state, false, nil)
	if len(blockers) > 0 {
		return domain.Job{}, schedulingBlocked(planID, &taskID, blockers...)
	}
	if complete || nextTask == nil {
		return domain.Job{}, schedulingBlocked(planID, &taskID, domain.PlanExecutionBlocker{Code: "no_runnable_task", Reason: "plan has no unfinished runnable task"})
	}
	if nextTask.ID != taskID {
		return domain.Job{}, schedulingBlocked(planID, &taskID, taskSchedulingBlocker(
			"earlier_task", *nextTask,
			fmt.Sprintf("task %s is the earliest unfinished task and must run before task %s", nextTask.TaskKey, task.TaskKey),
		))
	}
	if !lifecycleContains([]string{"pending", "failed", "cancelled", "interrupted"}, task.Status) {
		return domain.Job{}, domain.ErrInvalidTransition
	}
	if err = requirePlanExecutionBaseline(ctx, tx, planID); err != nil {
		return domain.Job{}, err
	}

	provider, providerRequested := executionProviderFromContext(ctx)
	jobID := uuid.New()
	taskTransition, err := transitionLifecycleTx(ctx, tx, lifecycleTransitionRequest{
		LifecycleTransitionParams: LifecycleTransitionParams{
			ResourceType:    domain.LifecycleResourceTask,
			ResourceID:      task.ID,
			ExpectedVersion: version,
			Status:          "queued",
			StatusSource:    domain.LifecycleSourceUser,
			ReasonCode:      domain.LifecycleReasonResumeRequested,
			Reason:          "earliest dependency-ready task explicitly queued",
			RecoveryHint:    domain.LifecycleRecoveryNone,
			RelatedJobID:    &jobID,
		},
		ExpectedStatuses: []string{task.Status},
		AllowNonContract: true,
		AllowTerminal:    true,
	})
	if err != nil {
		return domain.Job{}, err
	}
	planChanged := false
	var planVersion int64
	if state.Status != "running" {
		planTransition, transitionErr := transitionLifecycleTx(ctx, tx, lifecycleTransitionRequest{
			LifecycleTransitionParams: LifecycleTransitionParams{
				ResourceType:        domain.LifecycleResourcePlan,
				ResourceID:          planID,
				Status:              "running",
				StatusSource:        domain.LifecycleSourceUser,
				ReasonCode:          domain.LifecycleReasonResumeRequested,
				Reason:              "plan resumed for explicit task retry",
				RecoveryHint:        domain.LifecycleRecoveryNone,
				ExecutionCheckpoint: mustJSON(map[string]any{"taskId": task.ID}),
				RelatedJobID:        &jobID,
			},
			ExpectedStatuses: []string{state.Status},
			AllowNonContract: true,
			AllowTerminal:    true,
			Fields:           []lifecycleFieldUpdate{{Column: "execution_started_at", SQL: "now()"}},
		})
		if transitionErr != nil {
			return domain.Job{}, transitionErr
		}
		planChanged = !planTransition.Idempotent
		planVersion = planTransition.State.Version
	}
	maxAttempts, err := projectMaxAttempts(ctx, tx, state.ProjectID)
	if err != nil {
		return domain.Job{}, err
	}
	job, err := insertJob(ctx, tx, NewJob{ID: jobID, ProjectID: state.ProjectID, Type: "task.execute", AggregateType: "task", AggregateID: task.ID, Payload: taskExecutionJobPayload(task.ID, planID, provider, providerRequested), Priority: 100, MaxAttempts: maxAttempts, RunAfter: time.Now(), IdempotencyKey: fmt.Sprintf("task.execute:%s:%d", task.ID, taskTransition.State.Version)})
	if err != nil {
		return domain.Job{}, err
	}
	if _, err = insertEvent(ctx, tx, NewEvent{ProjectID: &state.ProjectID, Type: "task.queued", AggregateType: "task", AggregateID: task.ID, ResourceVersion: taskTransition.State.Version, Payload: mustJSON(map[string]any{"jobId": job.ID})}); err != nil {
		return domain.Job{}, err
	}
	if planChanged {
		if _, err = insertEvent(ctx, tx, NewEvent{ProjectID: &state.ProjectID, Type: "plan.running", AggregateType: "plan", AggregateID: planID, ResourceVersion: planVersion, Payload: mustJSON(map[string]any{"jobId": job.ID, "taskId": task.ID})}); err != nil {
			return domain.Job{}, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return domain.Job{}, err
	}
	return job, nil
}

func blockQueuedTaskForSchedulingTx(ctx context.Context, tx pgx.Tx, state planExecutionState, task domain.PlanTask, jobID *uuid.UUID, blockers []domain.PlanExecutionBlocker) error {
	blockedErr := schedulingBlocked(state.PlanID, &task.ID, blockers...)
	reason, _ := boundCheckpointText(blockedErr.Error(), maxTaskResultSummaryBytes, maxTaskResultSummaryLines)
	if task.Status == "queued" {
		if _, err := transitionLifecycleTx(ctx, tx, lifecycleTransitionRequest{
			LifecycleTransitionParams: LifecycleTransitionParams{
				ResourceType: domain.LifecycleResourceTask,
				ResourceID:   task.ID,
				Status:       "pending",
				StatusSource: domain.LifecycleSourceWorker,
				ReasonCode:   domain.LifecycleReasonValidationFailed,
				Reason:       reason,
				RecoveryHint: domain.LifecycleRecoveryManualReview,
				RelatedJobID: jobID,
			},
			ExpectedStatuses: []string{"queued"},
			Fields: []lifecycleFieldUpdate{
				{Column: "started_at", SQL: "NULL"},
				{Column: "finished_at", SQL: "NULL"},
			},
		}); err != nil {
			return err
		}
	}
	if lifecycleContains([]string{"ready", "running", "validating", "blocked"}, state.Status) {
		transition, err := transitionLifecycleTx(ctx, tx, lifecycleTransitionRequest{
			LifecycleTransitionParams: LifecycleTransitionParams{
				ResourceType:        domain.LifecycleResourcePlan,
				ResourceID:          state.PlanID,
				Status:              "blocked",
				StatusSource:        domain.LifecycleSourceWorker,
				ReasonCode:          domain.LifecycleReasonValidationFailed,
				Reason:              reason,
				RecoveryHint:        domain.LifecycleRecoveryManualReview,
				ExecutionCheckpoint: mustJSON(map[string]any{"taskId": task.ID, "blockers": blockers}),
				RelatedJobID:        jobID,
			},
			ExpectedStatuses: []string{state.Status},
			AllowNonContract: true,
			IgnoreTerminal:   true,
		})
		if err != nil {
			return err
		}
		if !transition.Idempotent {
			if _, err = insertEvent(ctx, tx, NewEvent{ProjectID: &state.ProjectID, Type: "plan.blocked", AggregateType: "plan", AggregateID: state.PlanID, ResourceVersion: transition.State.Version, Payload: mustJSON(map[string]any{"taskId": task.ID, "message": reason, "blockers": blockers})}); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Store) StartTask(ctx context.Context, taskID uuid.UUID) (domain.PlanTask, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return domain.PlanTask{}, err
	}
	defer tx.Rollback(ctx)

	var planID uuid.UUID
	if err = tx.QueryRow(ctx, `SELECT plan_id FROM plan_tasks WHERE id=$1`, taskID).Scan(&planID); errors.Is(err, pgx.ErrNoRows) {
		return domain.PlanTask{}, domain.ErrNotFound
	} else if err != nil {
		return domain.PlanTask{}, err
	}
	state, err := loadPlanExecutionStateTx(ctx, tx, planID)
	if err != nil {
		return domain.PlanTask{}, err
	}
	var task *domain.PlanTask
	for i := range state.Tasks {
		if state.Tasks[i].ID == taskID {
			task = &state.Tasks[i]
			break
		}
	}
	if task == nil {
		return domain.PlanTask{}, domain.ErrNotFound
	}
	var jobID *uuid.UUID
	var relatedJobID uuid.UUID
	if scanErr := tx.QueryRow(ctx, `SELECT id FROM jobs WHERE aggregate_type='task' AND aggregate_id=$1 AND status IN ('leased','running') ORDER BY created_at DESC LIMIT 1 FOR UPDATE`, taskID).Scan(&relatedJobID); scanErr == nil {
		jobID = &relatedJobID
	} else if !errors.Is(scanErr, pgx.ErrNoRows) {
		return domain.PlanTask{}, scanErr
	}
	blockers := []domain.PlanExecutionBlocker{}
	nextTask, complete, gateBlockers := nextPlanTask(state, false, &taskID)
	blockers = append(blockers, gateBlockers...)
	if state.Status != "running" {
		blockers = append(blockers, taskSchedulingBlocker("plan_not_running", *task, fmt.Sprintf("plan is %s while task %s is queued", state.Status, task.TaskKey)))
	}
	if len(blockers) == 0 && (complete || nextTask == nil) {
		blockers = append(blockers, domain.PlanExecutionBlocker{Code: "no_runnable_task", Reason: "plan has no unfinished runnable task"})
	}
	if len(blockers) == 0 && nextTask.ID != taskID {
		blockers = append(blockers, taskSchedulingBlocker(
			"earlier_task", *nextTask,
			fmt.Sprintf("task %s is the earliest unfinished task and must run before task %s", nextTask.TaskKey, task.TaskKey),
		))
	}
	if len(blockers) > 0 {
		if blockErr := blockQueuedTaskForSchedulingTx(ctx, tx, state, *task, jobID, blockers); blockErr != nil {
			return domain.PlanTask{}, blockErr
		}
		if err = tx.Commit(ctx); err != nil {
			return domain.PlanTask{}, err
		}
		return domain.PlanTask{}, schedulingBlocked(planID, &taskID, blockers...)
	}
	if task.Status != "queued" {
		return domain.PlanTask{}, domain.ErrNotFound
	}
	if err = requirePlanExecutionBaseline(ctx, tx, planID); err != nil {
		return domain.PlanTask{}, err
	}
	transition, err := transitionLifecycleTx(ctx, tx, lifecycleTransitionRequest{
		LifecycleTransitionParams: LifecycleTransitionParams{
			ResourceType: domain.LifecycleResourceTask,
			ResourceID:   taskID,
			Status:       "running",
			StatusSource: domain.LifecycleSourceWorker,
			ReasonCode:   domain.LifecycleReasonCode("worker_started"),
			Reason:       "worker started task execution after dependency gate recheck",
			RecoveryHint: domain.LifecycleRecoveryRetryFromStart,
			RelatedJobID: jobID,
		},
		ExpectedStatuses: []string{"queued"},
		MismatchError:    domain.ErrNotFound,
		Fields:           []lifecycleFieldUpdate{{Column: "started_at", SQL: "now()"}},
	})
	if err != nil {
		return domain.PlanTask{}, err
	}
	t, err := scanTaskRecord(tx.QueryRow(ctx, `SELECT `+planTaskRecordColumns+` FROM plan_tasks WHERE id=$1`, taskID))
	if err != nil {
		return t, err
	}
	if t.TaskType == domain.PlanTaskTypeFinalValidation {
		planTransition, transitionErr := transitionLifecycleTx(ctx, tx, lifecycleTransitionRequest{
			LifecycleTransitionParams: LifecycleTransitionParams{
				ResourceType:        domain.LifecycleResourcePlan,
				ResourceID:          t.PlanID,
				Status:              "validating",
				StatusSource:        domain.LifecycleSourceWorker,
				ReasonCode:          domain.LifecycleReasonCode("validation_started"),
				Reason:              "final validation task started",
				RecoveryHint:        domain.LifecycleRecoveryRetryFromStart,
				ExecutionCheckpoint: mustJSON(map[string]any{"taskId": t.ID}),
				RelatedJobID:        jobID,
			},
			ExpectedStatuses: []string{"running"},
		})
		if transitionErr != nil {
			return t, transitionErr
		}
		if _, err = insertEvent(ctx, tx, NewEvent{ProjectID: &t.ProjectID, Type: "plan.validating", AggregateType: "plan", AggregateID: t.PlanID, ResourceVersion: planTransition.State.Version, Payload: json.RawMessage(`{}`)}); err != nil {
			return t, err
		}
	}
	_ = transition
	if err = tx.Commit(ctx); err != nil {
		return t, err
	}
	return t, nil
}

func (s *Store) FinishTask(ctx context.Context, t domain.PlanTask, sessionID string, success bool, message string) error {
	return s.finishTask(ctx, t, sessionID, success, message, nil, true)
}

func (s *Store) FinishTaskWithCheckpoint(ctx context.Context, t domain.PlanTask, sessionID string, success bool, message string, workspace *PlanWorkspaceState) error {
	return s.finishTask(ctx, t, sessionID, success, message, workspace, true)
}

const (
	maxTaskResultSummaryBytes = 16 * 1024
	maxTaskResultSummaryLines = 200
)

func blockPlanAfterSchedulingGateTx(ctx context.Context, tx pgx.Tx, state planExecutionState, taskID uuid.UUID, jobID *uuid.UUID, blockers []domain.PlanExecutionBlocker) error {
	blockedErr := schedulingBlocked(state.PlanID, &taskID, blockers...)
	reason, summaryTruncated := boundCheckpointText(blockedErr.Error(), maxTaskResultSummaryBytes, maxTaskResultSummaryLines)
	transition, err := transitionLifecycleTx(ctx, tx, lifecycleTransitionRequest{
		LifecycleTransitionParams: LifecycleTransitionParams{
			ResourceType:        domain.LifecycleResourcePlan,
			ResourceID:          state.PlanID,
			Status:              "blocked",
			StatusSource:        domain.LifecycleSourceAutomation,
			ReasonCode:          domain.LifecycleReasonValidationFailed,
			Reason:              reason,
			RecoveryHint:        domain.LifecycleRecoveryManualReview,
			ExecutionCheckpoint: mustJSON(map[string]any{"taskId": taskID, "blockers": blockers}),
			RelatedJobID:        jobID,
		},
		ExpectedStatuses: []string{state.Status},
		AllowNonContract: true,
		IgnoreTerminal:   true,
	})
	if err != nil {
		return err
	}
	if !transition.Idempotent {
		_, err = insertEvent(ctx, tx, NewEvent{ProjectID: &state.ProjectID, Type: "plan.blocked", AggregateType: "plan", AggregateID: state.PlanID, ResourceVersion: transition.State.Version, Payload: mustJSON(map[string]any{
			"taskId": taskID, "message": reason, "summaryTruncated": summaryTruncated, "blockers": blockers,
		})})
	}
	return err
}

func (s *Store) finishTask(ctx context.Context, t domain.PlanTask, sessionID string, success bool, message string, workspace *PlanWorkspaceState, captureCheckpoint bool) error {
	summary, summaryTruncated := boundCheckpointText(strings.ToValidUTF8(message, "�"), maxTaskResultSummaryBytes, maxTaskResultSummaryLines)
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	state, err := loadPlanExecutionStateTx(ctx, tx, t.PlanID)
	if err != nil {
		return err
	}
	var currentTask *domain.PlanTask
	for i := range state.Tasks {
		if state.Tasks[i].ID == t.ID {
			currentTask = &state.Tasks[i]
			break
		}
	}
	if currentTask == nil {
		return domain.ErrNotFound
	}
	t = *currentTask
	if t.Status != "running" {
		if domain.IsTerminalStatus(domain.LifecycleResourceTask, t.Status) || t.Status == "pending" {
			return nil
		}
		return domain.ErrInvalidTransition
	}
	var jobID *uuid.UUID
	var relatedJobID uuid.UUID
	if scanErr := tx.QueryRow(ctx, `SELECT id FROM jobs WHERE aggregate_type='task' AND aggregate_id=$1 AND status IN ('leased','running','retry_wait') ORDER BY created_at DESC LIMIT 1 FOR UPDATE`, t.ID).Scan(&relatedJobID); scanErr == nil {
		jobID = &relatedJobID
	} else if !errors.Is(scanErr, pgx.ErrNoRows) {
		return scanErr
	}
	status := "failed"
	eventType := "task.failed"
	reasonCode := domain.LifecycleReasonExecutionFailed
	recoveryHint := domain.LifecycleRecoveryManualReview
	if success {
		status = "succeeded"
		eventType = "task.succeeded"
		reasonCode = domain.LifecycleReasonCompleted
		recoveryHint = domain.LifecycleRecoveryNone
	}
	acceptanceStatus := domain.AcceptanceStatusFailed
	if success {
		acceptanceStatus = domain.AcceptanceStatusPassed
	}
	acceptanceResult := buildAcceptanceResult(t.AcceptanceDefinition, acceptanceStatus, summary)
	taskTransition, err := transitionLifecycleTx(ctx, tx, lifecycleTransitionRequest{
		LifecycleTransitionParams: LifecycleTransitionParams{
			ResourceType: domain.LifecycleResourceTask,
			ResourceID:   t.ID,
			Status:       status,
			StatusSource: domain.LifecycleSourceWorker,
			ReasonCode:   reasonCode,
			Reason:       summary,
			RecoveryHint: recoveryHint,
			RelatedJobID: jobID,
		},
		ExpectedStatuses: []string{"running"},
		IgnoreTerminal:   true,
		Fields: []lifecycleFieldUpdate{
			{Column: "session_id", SQL: "nullif(%s,'')", Args: []any{sessionID}},
			{Column: "finished_at", SQL: "now()"},
			{Column: "acceptance_status", Args: []any{acceptanceStatus}},
			{Column: "acceptance_result", Args: []any{acceptanceResult}},
		},
	})
	if err != nil {
		return err
	}
	if taskTransition.Idempotent {
		return nil
	}
	eventPayload := map[string]any{
		"message": summary, "summaryTruncated": summaryTruncated, "success": success,
		"finalValidation": t.TaskType == domain.PlanTaskTypeFinalValidation, "acceptanceStatus": acceptanceStatus,
	}
	if _, err = insertEvent(ctx, tx, NewEvent{ProjectID: &t.ProjectID, Type: eventType, AggregateType: "task", AggregateID: t.ID, ResourceVersion: taskTransition.State.Version, Payload: mustJSON(eventPayload)}); err != nil {
		return err
	}
	if !success {
		planTransition, transitionErr := transitionLifecycleTx(ctx, tx, lifecycleTransitionRequest{
			LifecycleTransitionParams: LifecycleTransitionParams{
				ResourceType:        domain.LifecycleResourcePlan,
				ResourceID:          t.PlanID,
				Status:              "blocked",
				StatusSource:        domain.LifecycleSourceWorker,
				ReasonCode:          domain.LifecycleReasonExecutionFailed,
				Reason:              summary,
				RecoveryHint:        domain.LifecycleRecoveryManualReview,
				ExecutionCheckpoint: mustJSON(map[string]any{"taskId": t.ID}),
				RelatedJobID:        jobID,
			},
			ExpectedStatuses: []string{"running", "validating"},
			IgnoreTerminal:   true,
		})
		if transitionErr != nil {
			return transitionErr
		}
		if !planTransition.Idempotent {
			if _, err = insertEvent(ctx, tx, NewEvent{ProjectID: &t.ProjectID, Type: "plan.blocked", AggregateType: "plan", AggregateID: t.PlanID, ResourceVersion: planTransition.State.Version, Payload: mustJSON(map[string]any{"taskId": t.ID, "message": summary, "summaryTruncated": summaryTruncated, "finalValidation": t.TaskType == domain.PlanTaskTypeFinalValidation})}); err != nil {
				return err
			}
		}
		return tx.Commit(ctx)
	}

	if captureCheckpoint {
		checkpoint, captureErr := s.capturePlanExecutionSnapshotTx(ctx, tx, t.PlanID, domain.PlanSnapshotKindTaskCheckpoint, &t.ID, "")
		if captureErr != nil {
			return captureErr
		}
		if workspace != nil {
			if _, captureErr = updatePlanSnapshotWorkspaceTx(ctx, tx, checkpoint, *workspace); captureErr != nil {
				return captureErr
			}
		}
	}
	updatedState, err := loadPlanExecutionStateTx(ctx, tx, t.PlanID)
	if err != nil {
		return err
	}
	if updatedState.Status == "cancelled" || updatedState.Status == "completed" {
		return tx.Commit(ctx)
	}
	var automationEnabled bool
	var intakeID uuid.UUID
	if err = tx.QueryRow(ctx, `SELECT pr.automation_enabled,p.intake_id FROM plans p JOIN projects pr ON pr.id=p.project_id WHERE p.id=$1`, t.PlanID).Scan(&automationEnabled, &intakeID); err != nil {
		return err
	}
	if !automationEnabled {
		planTransition, transitionErr := transitionLifecycleTx(ctx, tx, lifecycleTransitionRequest{
			LifecycleTransitionParams: LifecycleTransitionParams{
				ResourceType:        domain.LifecycleResourcePlan,
				ResourceID:          t.PlanID,
				Status:              "cancelled",
				StatusSource:        domain.LifecycleSourceAutomation,
				ReasonCode:          domain.LifecycleReasonAutomationDisabled,
				Reason:              "project automation stopped",
				RecoveryHint:        domain.LifecycleRecoveryNone,
				ExecutionCheckpoint: mustJSON(map[string]any{"taskId": t.ID}),
				RelatedJobID:        jobID,
			},
			ExpectedStatuses: []string{"running", "validating"},
			AllowNonContract: true,
			IgnoreTerminal:   true,
		})
		if transitionErr != nil {
			return transitionErr
		}
		if !planTransition.Idempotent {
			if _, err = insertEvent(ctx, tx, NewEvent{ProjectID: &t.ProjectID, Type: "plan.cancelled", AggregateType: "plan", AggregateID: t.PlanID, ResourceVersion: planTransition.State.Version, Payload: mustJSON(map[string]any{"reason": "project automation stopped"})}); err != nil {
				return err
			}
		}
		return tx.Commit(ctx)
	}
	provider, providerErr := providerFromPlanConfigSnapshot(updatedState.ConfigSnapshot)
	if providerErr != nil {
		return providerErr
	}
	nextTask, complete, blockers := nextPlanTask(updatedState, true, nil)
	if len(blockers) > 0 {
		if err = blockPlanAfterSchedulingGateTx(ctx, tx, updatedState, t.ID, jobID, blockers); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	if complete || nextTask == nil {
		planTransition, transitionErr := transitionLifecycleTx(ctx, tx, lifecycleTransitionRequest{
			LifecycleTransitionParams: LifecycleTransitionParams{
				ResourceType:        domain.LifecycleResourcePlan,
				ResourceID:          t.PlanID,
				Status:              "completed",
				StatusSource:        domain.LifecycleSourceWorker,
				ReasonCode:          domain.LifecycleReasonCompleted,
				Reason:              "all plan tasks and acceptance gates completed successfully",
				RecoveryHint:        domain.LifecycleRecoveryNone,
				ExecutionCheckpoint: mustJSON(map[string]any{"taskId": t.ID}),
				RelatedJobID:        jobID,
			},
			ExpectedStatuses: []string{"running", "validating"},
			IgnoreTerminal:   true,
		})
		if transitionErr != nil {
			return transitionErr
		}
		if !planTransition.Idempotent {
			if _, err = insertEvent(ctx, tx, NewEvent{ProjectID: &t.ProjectID, Type: "plan.completed", AggregateType: "plan", AggregateID: t.PlanID, ResourceVersion: planTransition.State.Version, Payload: json.RawMessage(`{}`)}); err != nil {
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
		return tx.Commit(ctx)
	}
	if nextTask.Status != "pending" {
		return errors.New("automatic scheduler selected a non-pending task without a blocker")
	}
	nextJobID := uuid.New()
	nextTransition, transitionErr := transitionLifecycleTx(ctx, tx, lifecycleTransitionRequest{
		LifecycleTransitionParams: LifecycleTransitionParams{
			ResourceType: domain.LifecycleResourceTask,
			ResourceID:   nextTask.ID,
			Status:       "queued",
			StatusSource: domain.LifecycleSourceAutomation,
			ReasonCode:   domain.LifecycleReasonResumeRequested,
			Reason:       "earliest dependency-ready task queued",
			RecoveryHint: domain.LifecycleRecoveryNone,
			RelatedJobID: &nextJobID,
		},
		ExpectedStatuses: []string{"pending"},
	})
	if transitionErr != nil {
		return transitionErr
	}
	maxAttempts, attemptsErr := projectMaxAttempts(ctx, tx, t.ProjectID)
	if attemptsErr != nil {
		return attemptsErr
	}
	job, insertErr := insertJob(ctx, tx, NewJob{ID: nextJobID, ProjectID: t.ProjectID, Type: "task.execute", AggregateType: "task", AggregateID: nextTask.ID, Payload: taskExecutionJobPayload(nextTask.ID, t.PlanID, provider, false), Priority: 100, MaxAttempts: maxAttempts, RunAfter: time.Now(), IdempotencyKey: fmt.Sprintf("task.execute:%s:%d", nextTask.ID, nextTransition.State.Version)})
	if insertErr != nil {
		return insertErr
	}
	if _, err = insertEvent(ctx, tx, NewEvent{ProjectID: &t.ProjectID, Type: "task.queued", AggregateType: "task", AggregateID: nextTask.ID, ResourceVersion: nextTransition.State.Version, Payload: mustJSON(map[string]any{"jobId": job.ID})}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func buildAcceptanceResult(definition json.RawMessage, status, message string) json.RawMessage {
	var criteria []struct {
		Key string `json:"key"`
	}
	_ = json.Unmarshal(definition, &criteria)
	evidence := []any{}
	reason := ""
	if status == domain.AcceptanceStatusPassed {
		if message != "" {
			evidence = append(evidence, map[string]any{"type": "execution", "message": message})
		}
	} else {
		reason = message
	}
	items := make([]map[string]any, 0, len(criteria))
	for _, criterion := range criteria {
		items = append(items, map[string]any{
			"key": criterion.Key, "status": status, "evidence": evidence, "reason": reason,
		})
	}
	return mustJSON(map[string]any{
		"status": status, "items": items, "evidence": evidence, "reason": reason,
	})
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
