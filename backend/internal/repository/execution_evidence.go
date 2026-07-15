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
	"github.com/lyming99/specrelay/backend/internal/domain"
)

type TaskExecutionCheckpointInput struct {
	ID                    uuid.UUID
	GitReferenceState     string
	CurrentBranch         string
	HeadOID               string
	IndexTreeOID          string
	WorkspaceTreeOID      string
	GitStatusSummary      json.RawMessage
	GitStatusFingerprint  string
	ProjectConfigSnapshot json.RawMessage
	CreatedAt             time.Time
}

type TaskExecutionFileChangeInput struct {
	ID               uuid.UUID
	Path             string
	PreviousPath     string
	ChangeKind       string
	Staged           bool
	Binary           bool
	Additions        *int
	Deletions        *int
	BeforeBlobOID    string
	AfterBlobOID     string
	PatchFingerprint string
	Summary          json.RawMessage
	CreatedAt        time.Time
}

type TaskExecutionValidationInput struct {
	ID                uuid.UUID
	Command           string
	WorkingDirectory  string
	Status            string
	ExitCode          *int
	StartedAt         *time.Time
	FinishedAt        *time.Time
	StdoutFingerprint string
	StderrFingerprint string
	OutputSummary     json.RawMessage
	CreatedAt         time.Time
}

type SaveTaskExecutionClosureParams struct {
	ID                  uuid.UUID
	ProjectID           uuid.UUID
	PlanID              uuid.UUID
	TaskID              uuid.UUID
	JobID               uuid.UUID
	AgentRunID          uuid.UUID
	AttemptOrigin       string
	QueueAttempt        int
	SupersedesAttemptID *uuid.UUID
	Outcome             string
	StartedAt           time.Time
	FinishedAt          time.Time
	CreatedAt           time.Time
	BeforeCheckpoint    TaskExecutionCheckpointInput
	AfterCheckpoint     TaskExecutionCheckpointInput
	FileChanges         []TaskExecutionFileChangeInput
	ValidationEvidence  []TaskExecutionValidationInput
}

type RecordTaskExecutionRollbackParams struct {
	ID                 uuid.UUID
	AttemptID          uuid.UUID
	SourceCheckpointID uuid.UUID
	TargetCheckpointID uuid.UUID
	RollbackKind       string
	CommandSummary     string
	Reason             string
	RequestedBy        string
	Details            json.RawMessage
	OccurredAt         time.Time
}

type AppendTaskExecutionRollbackEventParams struct {
	ID         uuid.UUID
	RollbackID uuid.UUID
	Status     string
	Message    string
	Details    json.RawMessage
	OccurredAt time.Time
}

func evidenceJSON(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage(`{}`)
	}
	return raw
}

func evidenceID(id uuid.UUID) uuid.UUID {
	if id == uuid.Nil {
		return uuid.New()
	}
	return id
}

func evidenceTime(value time.Time) time.Time {
	if value.IsZero() {
		return time.Now().UTC()
	}
	return value
}

// SaveTaskExecutionClosure atomically appends one complete task attempt. It
// deliberately allocates a task-scoped sequence instead of using task_id as an
// upsert key, so manual retries and queue retries cannot overwrite each other.
func (s *Store) SaveTaskExecutionClosure(ctx context.Context, p SaveTaskExecutionClosureParams) (domain.TaskExecutionClosure, error) {
	if p.ProjectID == uuid.Nil || p.PlanID == uuid.Nil || p.TaskID == uuid.Nil || p.JobID == uuid.Nil || p.AgentRunID == uuid.Nil {
		return domain.TaskExecutionClosure{}, errors.New("task execution closure requires project, plan, task, job, and agent run IDs")
	}
	if p.StartedAt.IsZero() || p.FinishedAt.IsZero() {
		return domain.TaskExecutionClosure{}, errors.New("task execution closure requires start and finish times")
	}
	if !json.Valid(evidenceJSON(p.BeforeCheckpoint.GitStatusSummary)) || !json.Valid(evidenceJSON(p.BeforeCheckpoint.ProjectConfigSnapshot)) ||
		!json.Valid(evidenceJSON(p.AfterCheckpoint.GitStatusSummary)) || !json.Valid(evidenceJSON(p.AfterCheckpoint.ProjectConfigSnapshot)) {
		return domain.TaskExecutionClosure{}, errors.New("task execution checkpoint contains invalid JSON")
	}

	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return domain.TaskExecutionClosure{}, err
	}
	defer tx.Rollback(ctx)

	var taskKey string
	if err = tx.QueryRow(ctx, `SELECT task_key FROM plan_tasks
		WHERE id=$1 AND project_id=$2 AND plan_id=$3 FOR UPDATE`, p.TaskID, p.ProjectID, p.PlanID).Scan(&taskKey); err != nil {
		return domain.TaskExecutionClosure{}, mapNotFound(err)
	}

	var attemptSequence int64
	if err = tx.QueryRow(ctx, `SELECT coalesce(max(attempt_sequence),0)+1
		FROM task_execution_attempts WHERE task_id=$1`, p.TaskID).Scan(&attemptSequence); err != nil {
		return domain.TaskExecutionClosure{}, err
	}
	if p.SupersedesAttemptID == nil {
		var latestAttemptID uuid.UUID
		latestErr := tx.QueryRow(ctx, `SELECT id FROM task_execution_attempts
			WHERE task_id=$1 ORDER BY attempt_sequence DESC LIMIT 1`, p.TaskID).Scan(&latestAttemptID)
		if latestErr == nil {
			p.SupersedesAttemptID = &latestAttemptID
		} else if !errors.Is(latestErr, pgx.ErrNoRows) {
			return domain.TaskExecutionClosure{}, latestErr
		}
	}
	p.ID = evidenceID(p.ID)
	p.CreatedAt = evidenceTime(p.CreatedAt)

	_, err = tx.Exec(ctx, `INSERT INTO task_execution_attempts(
		id,project_id,plan_id,task_id,job_id,agent_run_id,attempt_sequence,
		attempt_origin,queue_attempt,supersedes_attempt_id,outcome,started_at,finished_at,created_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
		p.ID, p.ProjectID, p.PlanID, p.TaskID, p.JobID, p.AgentRunID, attemptSequence,
		p.AttemptOrigin, p.QueueAttempt, p.SupersedesAttemptID, p.Outcome, p.StartedAt, p.FinishedAt, p.CreatedAt)
	if err != nil {
		return domain.TaskExecutionClosure{}, err
	}

	for index, checkpoint := range []struct {
		phase string
		input TaskExecutionCheckpointInput
	}{
		{phase: domain.TaskExecutionCheckpointBeforeExecution, input: p.BeforeCheckpoint},
		{phase: domain.TaskExecutionCheckpointAfterExecution, input: p.AfterCheckpoint},
	} {
		checkpoint.input.ID = evidenceID(checkpoint.input.ID)
		checkpoint.input.CreatedAt = evidenceTime(checkpoint.input.CreatedAt)
		_, err = tx.Exec(ctx, `INSERT INTO task_execution_checkpoints(
			id,attempt_id,project_id,plan_id,task_id,job_id,agent_run_id,
			checkpoint_sequence,checkpoint_phase,git_reference_state,current_branch,
			head_oid,index_tree_oid,workspace_tree_oid,git_status_summary,
			git_status_fingerprint,task_key,project_config_snapshot,created_at)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19)`,
			checkpoint.input.ID, p.ID, p.ProjectID, p.PlanID, p.TaskID, p.JobID, p.AgentRunID,
			index+1, checkpoint.phase, checkpoint.input.GitReferenceState, checkpoint.input.CurrentBranch,
			checkpoint.input.HeadOID, checkpoint.input.IndexTreeOID, checkpoint.input.WorkspaceTreeOID,
			evidenceJSON(checkpoint.input.GitStatusSummary), checkpoint.input.GitStatusFingerprint, taskKey,
			evidenceJSON(checkpoint.input.ProjectConfigSnapshot), checkpoint.input.CreatedAt)
		if err != nil {
			return domain.TaskExecutionClosure{}, err
		}
	}

	for index, change := range p.FileChanges {
		if !json.Valid(evidenceJSON(change.Summary)) {
			return domain.TaskExecutionClosure{}, fmt.Errorf("file change %d contains invalid JSON", index+1)
		}
		change.ID = evidenceID(change.ID)
		change.CreatedAt = evidenceTime(change.CreatedAt)
		_, err = tx.Exec(ctx, `INSERT INTO task_execution_file_changes(
			id,attempt_id,project_id,plan_id,task_id,job_id,agent_run_id,change_sequence,
			path,previous_path,change_kind,staged,is_binary,additions,deletions,before_blob_oid,
			after_blob_oid,patch_fingerprint,summary,created_at)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20)`,
			change.ID, p.ID, p.ProjectID, p.PlanID, p.TaskID, p.JobID, p.AgentRunID, index+1,
			change.Path, change.PreviousPath, change.ChangeKind, change.Staged, change.Binary,
			change.Additions, change.Deletions, change.BeforeBlobOID, change.AfterBlobOID,
			change.PatchFingerprint, evidenceJSON(change.Summary), change.CreatedAt)
		if err != nil {
			return domain.TaskExecutionClosure{}, err
		}
	}

	for index, validation := range p.ValidationEvidence {
		if !json.Valid(evidenceJSON(validation.OutputSummary)) {
			return domain.TaskExecutionClosure{}, fmt.Errorf("validation evidence %d contains invalid JSON", index+1)
		}
		validation.ID = evidenceID(validation.ID)
		validation.CreatedAt = evidenceTime(validation.CreatedAt)
		_, err = tx.Exec(ctx, `INSERT INTO task_execution_validations(
			id,attempt_id,project_id,plan_id,task_id,job_id,agent_run_id,validation_sequence,
			command,working_directory,status,exit_code,started_at,finished_at,stdout_fingerprint,
			stderr_fingerprint,output_summary,created_at)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)`,
			validation.ID, p.ID, p.ProjectID, p.PlanID, p.TaskID, p.JobID, p.AgentRunID, index+1,
			validation.Command, validation.WorkingDirectory, validation.Status, validation.ExitCode,
			validation.StartedAt, validation.FinishedAt, validation.StdoutFingerprint,
			validation.StderrFingerprint, evidenceJSON(validation.OutputSummary), validation.CreatedAt)
		if err != nil {
			return domain.TaskExecutionClosure{}, err
		}
	}

	if err = tx.Commit(ctx); err != nil {
		return domain.TaskExecutionClosure{}, err
	}
	return s.GetTaskExecutionClosure(ctx, p.ID)
}

const taskExecutionAttemptColumns = `id,project_id,plan_id,task_id,job_id,agent_run_id,
	attempt_sequence,attempt_origin,queue_attempt,supersedes_attempt_id,outcome,
	started_at,finished_at,created_at`

func scanTaskExecutionAttempt(row pgx.Row) (domain.TaskExecutionAttempt, error) {
	var attempt domain.TaskExecutionAttempt
	err := row.Scan(&attempt.ID, &attempt.ProjectID, &attempt.PlanID, &attempt.TaskID,
		&attempt.JobID, &attempt.AgentRunID, &attempt.AttemptSequence, &attempt.AttemptOrigin,
		&attempt.QueueAttempt, &attempt.SupersedesAttemptID, &attempt.Outcome,
		&attempt.StartedAt, &attempt.FinishedAt, &attempt.CreatedAt)
	return attempt, mapNotFound(err)
}

func scanTaskExecutionCheckpoint(row pgx.Row) (domain.TaskExecutionCheckpoint, error) {
	var checkpoint domain.TaskExecutionCheckpoint
	err := row.Scan(&checkpoint.ID, &checkpoint.AttemptID, &checkpoint.ProjectID,
		&checkpoint.PlanID, &checkpoint.TaskID, &checkpoint.JobID, &checkpoint.AgentRunID,
		&checkpoint.CheckpointSequence, &checkpoint.Phase, &checkpoint.GitReferenceState,
		&checkpoint.CurrentBranch, &checkpoint.HeadOID, &checkpoint.IndexTreeOID,
		&checkpoint.WorkspaceTreeOID, &checkpoint.GitStatusSummary,
		&checkpoint.GitStatusFingerprint, &checkpoint.TaskKey,
		&checkpoint.ProjectConfigSnapshot, &checkpoint.CreatedAt)
	return checkpoint, err
}

func scanTaskExecutionFileChange(row pgx.Row) (domain.TaskExecutionFileChange, error) {
	var change domain.TaskExecutionFileChange
	err := row.Scan(&change.ID, &change.AttemptID, &change.ProjectID, &change.PlanID,
		&change.TaskID, &change.JobID, &change.AgentRunID, &change.ChangeSequence,
		&change.Path, &change.PreviousPath, &change.ChangeKind, &change.Staged,
		&change.Binary, &change.Additions, &change.Deletions, &change.BeforeBlobOID,
		&change.AfterBlobOID, &change.PatchFingerprint, &change.Summary, &change.CreatedAt)
	return change, err
}

func scanTaskExecutionValidation(row pgx.Row) (domain.TaskExecutionValidation, error) {
	var validation domain.TaskExecutionValidation
	err := row.Scan(&validation.ID, &validation.AttemptID, &validation.ProjectID,
		&validation.PlanID, &validation.TaskID, &validation.JobID, &validation.AgentRunID,
		&validation.ValidationSequence, &validation.Command, &validation.WorkingDirectory,
		&validation.Status, &validation.ExitCode, &validation.StartedAt, &validation.FinishedAt,
		&validation.StdoutFingerprint, &validation.StderrFingerprint,
		&validation.OutputSummary, &validation.CreatedAt)
	return validation, err
}

func scanTaskExecutionRollback(row pgx.Row) (domain.TaskExecutionRollback, error) {
	var rollback domain.TaskExecutionRollback
	err := row.Scan(&rollback.ID, &rollback.AttemptID, &rollback.ProjectID,
		&rollback.PlanID, &rollback.TaskID, &rollback.JobID, &rollback.AgentRunID,
		&rollback.RollbackSequence, &rollback.SourceCheckpointID,
		&rollback.TargetCheckpointID, &rollback.RollbackKind, &rollback.CommandSummary,
		&rollback.Reason, &rollback.RequestedBy, &rollback.CreatedAt)
	return rollback, err
}

func scanTaskExecutionRollbackEvent(row pgx.Row) (domain.TaskExecutionRollbackEvent, error) {
	var event domain.TaskExecutionRollbackEvent
	err := row.Scan(&event.ID, &event.RollbackID, &event.AttemptID, &event.ProjectID,
		&event.PlanID, &event.TaskID, &event.JobID, &event.AgentRunID,
		&event.EventSequence, &event.Status, &event.Message, &event.Details, &event.OccurredAt)
	return event, err
}

func (s *Store) GetTaskExecutionClosure(ctx context.Context, attemptID uuid.UUID) (domain.TaskExecutionClosure, error) {
	attempt, err := scanTaskExecutionAttempt(s.Pool.QueryRow(ctx,
		`SELECT `+taskExecutionAttemptColumns+` FROM task_execution_attempts WHERE id=$1`, attemptID))
	if err != nil {
		return domain.TaskExecutionClosure{}, err
	}
	closure := domain.TaskExecutionClosure{
		Attempt:            attempt,
		Checkpoints:        make([]domain.TaskExecutionCheckpoint, 0),
		FileChanges:        make([]domain.TaskExecutionFileChange, 0),
		ValidationEvidence: make([]domain.TaskExecutionValidation, 0),
		Rollbacks:          make([]domain.TaskExecutionRollbackAudit, 0),
	}

	checkpointRows, err := s.Pool.Query(ctx, `SELECT id,attempt_id,project_id,plan_id,task_id,job_id,agent_run_id,
		checkpoint_sequence,checkpoint_phase,git_reference_state,current_branch,head_oid,index_tree_oid,
		workspace_tree_oid,git_status_summary,git_status_fingerprint,task_key,project_config_snapshot,created_at
		FROM task_execution_checkpoints WHERE attempt_id=$1 ORDER BY checkpoint_sequence`, attemptID)
	if err != nil {
		return domain.TaskExecutionClosure{}, err
	}
	for checkpointRows.Next() {
		checkpoint, scanErr := scanTaskExecutionCheckpoint(checkpointRows)
		if scanErr != nil {
			checkpointRows.Close()
			return domain.TaskExecutionClosure{}, scanErr
		}
		closure.Checkpoints = append(closure.Checkpoints, checkpoint)
	}
	if err = checkpointRows.Err(); err != nil {
		checkpointRows.Close()
		return domain.TaskExecutionClosure{}, err
	}
	checkpointRows.Close()

	changeRows, err := s.Pool.Query(ctx, `SELECT id,attempt_id,project_id,plan_id,task_id,job_id,agent_run_id,
		change_sequence,path,previous_path,change_kind,staged,is_binary,additions,deletions,before_blob_oid,
		after_blob_oid,patch_fingerprint,summary,created_at
		FROM task_execution_file_changes WHERE attempt_id=$1 ORDER BY change_sequence`, attemptID)
	if err != nil {
		return domain.TaskExecutionClosure{}, err
	}
	for changeRows.Next() {
		change, scanErr := scanTaskExecutionFileChange(changeRows)
		if scanErr != nil {
			changeRows.Close()
			return domain.TaskExecutionClosure{}, scanErr
		}
		closure.FileChanges = append(closure.FileChanges, change)
	}
	if err = changeRows.Err(); err != nil {
		changeRows.Close()
		return domain.TaskExecutionClosure{}, err
	}
	changeRows.Close()

	validationRows, err := s.Pool.Query(ctx, `SELECT id,attempt_id,project_id,plan_id,task_id,job_id,agent_run_id,
		validation_sequence,command,working_directory,status,exit_code,started_at,finished_at,
		stdout_fingerprint,stderr_fingerprint,output_summary,created_at
		FROM task_execution_validations WHERE attempt_id=$1 ORDER BY validation_sequence`, attemptID)
	if err != nil {
		return domain.TaskExecutionClosure{}, err
	}
	for validationRows.Next() {
		validation, scanErr := scanTaskExecutionValidation(validationRows)
		if scanErr != nil {
			validationRows.Close()
			return domain.TaskExecutionClosure{}, scanErr
		}
		closure.ValidationEvidence = append(closure.ValidationEvidence, validation)
	}
	if err = validationRows.Err(); err != nil {
		validationRows.Close()
		return domain.TaskExecutionClosure{}, err
	}
	validationRows.Close()

	rollbackRows, err := s.Pool.Query(ctx, `SELECT id,attempt_id,project_id,plan_id,task_id,job_id,agent_run_id,
		rollback_sequence,source_checkpoint_id,target_checkpoint_id,rollback_kind,command_summary,
		reason,requested_by,created_at
		FROM task_execution_rollbacks WHERE attempt_id=$1 ORDER BY rollback_sequence`, attemptID)
	if err != nil {
		return domain.TaskExecutionClosure{}, err
	}
	rollbacks := make([]domain.TaskExecutionRollback, 0)
	for rollbackRows.Next() {
		rollback, scanErr := scanTaskExecutionRollback(rollbackRows)
		if scanErr != nil {
			rollbackRows.Close()
			return domain.TaskExecutionClosure{}, scanErr
		}
		rollbacks = append(rollbacks, rollback)
	}
	if err = rollbackRows.Err(); err != nil {
		rollbackRows.Close()
		return domain.TaskExecutionClosure{}, err
	}
	rollbackRows.Close()
	for _, rollback := range rollbacks {
		audit := domain.TaskExecutionRollbackAudit{Operation: rollback, Events: make([]domain.TaskExecutionRollbackEvent, 0)}
		eventRows, queryErr := s.Pool.Query(ctx, `SELECT id,rollback_id,attempt_id,project_id,plan_id,task_id,job_id,agent_run_id,
			event_sequence,status,message,details,occurred_at
			FROM task_execution_rollback_events WHERE rollback_id=$1 ORDER BY event_sequence`, rollback.ID)
		if queryErr != nil {
			return domain.TaskExecutionClosure{}, queryErr
		}
		for eventRows.Next() {
			event, eventErr := scanTaskExecutionRollbackEvent(eventRows)
			if eventErr != nil {
				eventRows.Close()
				return domain.TaskExecutionClosure{}, eventErr
			}
			audit.Events = append(audit.Events, event)
			audit.Status = event.Status
		}
		if queryErr = eventRows.Err(); queryErr != nil {
			eventRows.Close()
			return domain.TaskExecutionClosure{}, queryErr
		}
		eventRows.Close()
		closure.Rollbacks = append(closure.Rollbacks, audit)
	}
	return closure, nil
}

func (s *Store) GetLatestTaskExecutionClosure(ctx context.Context, planID, taskID uuid.UUID) (domain.TaskExecutionClosure, error) {
	var attemptID uuid.UUID
	err := s.Pool.QueryRow(ctx, `SELECT id FROM task_execution_attempts
		WHERE plan_id=$1 AND task_id=$2 ORDER BY attempt_sequence DESC LIMIT 1`, planID, taskID).Scan(&attemptID)
	if err != nil {
		return domain.TaskExecutionClosure{}, mapNotFound(err)
	}
	return s.GetTaskExecutionClosure(ctx, attemptID)
}

func (s *Store) ListTaskExecutionClosures(ctx context.Context, planID, taskID uuid.UUID) ([]domain.TaskExecutionClosure, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id FROM task_execution_attempts
		WHERE plan_id=$1 AND task_id=$2 ORDER BY attempt_sequence`, planID, taskID)
	if err != nil {
		return nil, err
	}
	attemptIDs := make([]uuid.UUID, 0)
	for rows.Next() {
		var attemptID uuid.UUID
		if err = rows.Scan(&attemptID); err != nil {
			rows.Close()
			return nil, err
		}
		attemptIDs = append(attemptIDs, attemptID)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	closures := make([]domain.TaskExecutionClosure, 0, len(attemptIDs))
	for _, attemptID := range attemptIDs {
		closure, getErr := s.GetTaskExecutionClosure(ctx, attemptID)
		if getErr != nil {
			return nil, getErr
		}
		closures = append(closures, closure)
	}
	return closures, nil
}

func (s *Store) RecordTaskExecutionRollback(ctx context.Context, p RecordTaskExecutionRollbackParams) (domain.TaskExecutionRollbackAudit, error) {
	if !json.Valid(evidenceJSON(p.Details)) {
		return domain.TaskExecutionRollbackAudit{}, errors.New("rollback details contain invalid JSON")
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return domain.TaskExecutionRollbackAudit{}, err
	}
	defer tx.Rollback(ctx)

	attempt, err := scanTaskExecutionAttempt(tx.QueryRow(ctx, `SELECT `+taskExecutionAttemptColumns+`
		FROM task_execution_attempts WHERE id=$1 FOR UPDATE`, p.AttemptID))
	if err != nil {
		return domain.TaskExecutionRollbackAudit{}, err
	}
	var rollbackSequence int64
	if err = tx.QueryRow(ctx, `SELECT coalesce(max(rollback_sequence),0)+1
		FROM task_execution_rollbacks WHERE attempt_id=$1`, p.AttemptID).Scan(&rollbackSequence); err != nil {
		return domain.TaskExecutionRollbackAudit{}, err
	}
	p.ID = evidenceID(p.ID)
	p.OccurredAt = evidenceTime(p.OccurredAt)
	_, err = tx.Exec(ctx, `INSERT INTO task_execution_rollbacks(
		id,attempt_id,project_id,plan_id,task_id,job_id,agent_run_id,rollback_sequence,
		source_checkpoint_id,target_checkpoint_id,rollback_kind,command_summary,reason,requested_by,created_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`,
		p.ID, attempt.ID, attempt.ProjectID, attempt.PlanID, attempt.TaskID, attempt.JobID,
		attempt.AgentRunID, rollbackSequence, p.SourceCheckpointID, p.TargetCheckpointID,
		p.RollbackKind, p.CommandSummary, p.Reason, p.RequestedBy, p.OccurredAt)
	if err != nil {
		return domain.TaskExecutionRollbackAudit{}, err
	}
	eventID := uuid.New()
	_, err = tx.Exec(ctx, `INSERT INTO task_execution_rollback_events(
		id,rollback_id,attempt_id,project_id,plan_id,task_id,job_id,agent_run_id,
		event_sequence,status,message,details,occurred_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,1,$9,$10,$11,$12)`,
		eventID, p.ID, attempt.ID, attempt.ProjectID, attempt.PlanID, attempt.TaskID,
		attempt.JobID, attempt.AgentRunID, domain.TaskExecutionRollbackRequested,
		p.Reason, evidenceJSON(p.Details), p.OccurredAt)
	if err != nil {
		return domain.TaskExecutionRollbackAudit{}, err
	}
	if _, err = insertEvent(ctx, tx, NewEvent{
		ProjectID:       &attempt.ProjectID,
		Type:            "task.rollback.requested",
		AggregateType:   "task",
		AggregateID:     attempt.TaskID,
		ResourceVersion: attempt.AttemptSequence,
		Payload: mustJSON(map[string]any{
			"attemptId": p.AttemptID, "rollbackId": p.ID,
			"sourceCheckpointId": p.SourceCheckpointID, "targetCheckpointId": p.TargetCheckpointID,
		}),
	}); err != nil {
		return domain.TaskExecutionRollbackAudit{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return domain.TaskExecutionRollbackAudit{}, err
	}
	closure, err := s.GetTaskExecutionClosure(ctx, attempt.ID)
	if err != nil {
		return domain.TaskExecutionRollbackAudit{}, err
	}
	return closure.Rollbacks[len(closure.Rollbacks)-1], nil
}

func validRollbackTransition(from, to string) bool {
	if from == domain.TaskExecutionRollbackRequested {
		return to == domain.TaskExecutionRollbackRunning || to == domain.TaskExecutionRollbackSucceeded ||
			to == domain.TaskExecutionRollbackFailed || to == domain.TaskExecutionRollbackCancelled
	}
	if from == domain.TaskExecutionRollbackRunning {
		return to == domain.TaskExecutionRollbackSucceeded || to == domain.TaskExecutionRollbackFailed ||
			to == domain.TaskExecutionRollbackCancelled
	}
	return false
}

func (s *Store) AppendTaskExecutionRollbackEvent(ctx context.Context, p AppendTaskExecutionRollbackEventParams) (domain.TaskExecutionRollbackEvent, error) {
	if !json.Valid(evidenceJSON(p.Details)) {
		return domain.TaskExecutionRollbackEvent{}, errors.New("rollback event details contain invalid JSON")
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return domain.TaskExecutionRollbackEvent{}, err
	}
	defer tx.Rollback(ctx)

	rollback, err := scanTaskExecutionRollback(tx.QueryRow(ctx, `SELECT id,attempt_id,project_id,plan_id,task_id,job_id,agent_run_id,
		rollback_sequence,source_checkpoint_id,target_checkpoint_id,rollback_kind,command_summary,
		reason,requested_by,created_at FROM task_execution_rollbacks WHERE id=$1 FOR UPDATE`, p.RollbackID))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.TaskExecutionRollbackEvent{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.TaskExecutionRollbackEvent{}, err
	}
	var eventSequence int64
	var previousStatus string
	if err = tx.QueryRow(ctx, `SELECT event_sequence,status FROM task_execution_rollback_events
		WHERE rollback_id=$1 ORDER BY event_sequence DESC LIMIT 1`, rollback.ID).Scan(&eventSequence, &previousStatus); err != nil {
		return domain.TaskExecutionRollbackEvent{}, mapNotFound(err)
	}
	if !validRollbackTransition(previousStatus, p.Status) {
		return domain.TaskExecutionRollbackEvent{}, fmt.Errorf("invalid rollback transition %q to %q", previousStatus, p.Status)
	}
	p.ID = evidenceID(p.ID)
	p.OccurredAt = evidenceTime(p.OccurredAt)
	eventSequence++
	_, err = tx.Exec(ctx, `INSERT INTO task_execution_rollback_events(
		id,rollback_id,attempt_id,project_id,plan_id,task_id,job_id,agent_run_id,
		event_sequence,status,message,details,occurred_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
		p.ID, rollback.ID, rollback.AttemptID, rollback.ProjectID, rollback.PlanID,
		rollback.TaskID, rollback.JobID, rollback.AgentRunID, eventSequence, p.Status,
		p.Message, evidenceJSON(p.Details), p.OccurredAt)
	if err != nil {
		return domain.TaskExecutionRollbackEvent{}, err
	}
	if _, err = insertEvent(ctx, tx, NewEvent{
		ProjectID:       &rollback.ProjectID,
		Type:            "task.rollback." + strings.TrimSpace(p.Status),
		AggregateType:   "task",
		AggregateID:     rollback.TaskID,
		ResourceVersion: eventSequence,
		Payload:         mustJSON(map[string]any{"attemptId": rollback.AttemptID, "rollbackId": rollback.ID, "message": p.Message}),
	}); err != nil {
		return domain.TaskExecutionRollbackEvent{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return domain.TaskExecutionRollbackEvent{}, err
	}
	return domain.TaskExecutionRollbackEvent{
		ID: p.ID, RollbackID: rollback.ID, AttemptID: rollback.AttemptID,
		ProjectID: rollback.ProjectID, PlanID: rollback.PlanID, TaskID: rollback.TaskID,
		JobID: rollback.JobID, AgentRunID: rollback.AgentRunID, EventSequence: eventSequence,
		Status: p.Status, Message: p.Message, Details: evidenceJSON(p.Details), OccurredAt: p.OccurredAt,
	}, nil
}
