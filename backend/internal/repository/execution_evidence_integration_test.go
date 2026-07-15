package repository

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/lyming99/specrelay/backend/internal/domain"
)

func createTaskEvidenceJob(t *testing.T, store *Store, projectID, taskID uuid.UUID, key string) domain.Job {
	t.Helper()
	ctx := context.Background()
	tx, err := store.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	job, err := insertJob(ctx, tx, NewJob{
		ID:             uuid.New(),
		ProjectID:      projectID,
		Type:           "task.execute",
		AggregateType:  "task",
		AggregateID:    taskID,
		Payload:        json.RawMessage(`{}`),
		Priority:       100,
		MaxAttempts:    3,
		RunAfter:       time.Now(),
		IdempotencyKey: key,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return job
}

func createTaskEvidenceRun(t *testing.T, store *Store, project domain.Project, plan domain.Plan, task domain.PlanTask, job domain.Job, queueAttempt int) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	runID := uuid.New()
	retryCount := queueAttempt - 1
	if err := store.StartAgentRun(ctx, AgentRunStart{
		ID:              runID,
		ProjectID:       project.ID,
		IntakeID:        &plan.IntakeID,
		PlanID:          &plan.ID,
		JobID:           &job.ID,
		TaskID:          &task.ID,
		JobAttempt:      &queueAttempt,
		RetryCount:      &retryCount,
		Provider:        "codex",
		OperationType:   domain.AgentRunOperationTaskExecution,
		CommandSummary:  "codex task evidence",
		SessionMode:     domain.AgentRunSessionModeNew,
		LogPath:         "/tmp/task-evidence.log",
		OwnerInstanceID: "evidence-test",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishAgentRun(ctx, runID, domain.TaskExecutionOutcomeSucceeded, 0, "evidence-session", "", time.Second); err != nil {
		t.Fatal(err)
	}
	return runID
}

func checkpointEvidenceInput(id uuid.UUID, refState, branch, head, suffix string) TaskExecutionCheckpointInput {
	return TaskExecutionCheckpointInput{
		ID:                    id,
		GitReferenceState:     refState,
		CurrentBranch:         branch,
		HeadOID:               head,
		IndexTreeOID:          strings.Repeat(suffix, 40),
		WorkspaceTreeOID:      strings.Repeat(suffix, 40),
		GitStatusSummary:      json.RawMessage(`{"tracked":1,"untracked":0}`),
		GitStatusFingerprint:  strings.Repeat(suffix, 64),
		ProjectConfigSnapshot: json.RawMessage(`{"validationCommand":"go test ./...","maxRetries":2}`),
	}
}

func saveEvidenceClosure(t *testing.T, store *Store, project domain.Project, plan domain.Plan, task domain.PlanTask, job domain.Job, runID uuid.UUID, origin string, queueAttempt int, refState, branch, head, suffix string) domain.TaskExecutionClosure {
	t.Helper()
	beforeID, afterID := uuid.New(), uuid.New()
	startedAt := time.Now().Add(-2 * time.Second).UTC()
	finishedAt := startedAt.Add(time.Second)
	additions, deletions, exitCode := 7, 2, 0
	validationStarted := startedAt.Add(500 * time.Millisecond)
	validationFinished := validationStarted.Add(250 * time.Millisecond)
	closure, err := store.SaveTaskExecutionClosure(context.Background(), SaveTaskExecutionClosureParams{
		ProjectID:        project.ID,
		PlanID:           plan.ID,
		TaskID:           task.ID,
		JobID:            job.ID,
		AgentRunID:       runID,
		AttemptOrigin:    origin,
		QueueAttempt:     queueAttempt,
		Outcome:          domain.TaskExecutionOutcomeSucceeded,
		StartedAt:        startedAt,
		FinishedAt:       finishedAt,
		BeforeCheckpoint: checkpointEvidenceInput(beforeID, refState, branch, head, suffix),
		AfterCheckpoint:  checkpointEvidenceInput(afterID, domain.GitReferenceStateBranch, "main", strings.Repeat("a", 40), suffix),
		FileChanges: []TaskExecutionFileChangeInput{{
			Path:             "backend/internal/domain/models.go",
			ChangeKind:       domain.TaskExecutionFileChangeModified,
			Additions:        &additions,
			Deletions:        &deletions,
			BeforeBlobOID:    strings.Repeat("b", 40),
			AfterBlobOID:     strings.Repeat("c", 40),
			PatchFingerprint: strings.Repeat(suffix, 64),
			Summary:          json.RawMessage(`{"hunks":2}`),
		}},
		ValidationEvidence: []TaskExecutionValidationInput{{
			Command:           "go test ./internal/repository",
			WorkingDirectory:  "/workspace/backend",
			Status:            domain.TaskExecutionValidationPassed,
			ExitCode:          &exitCode,
			StartedAt:         &validationStarted,
			FinishedAt:        &validationFinished,
			StdoutFingerprint: strings.Repeat(suffix, 64),
			OutputSummary:     json.RawMessage(`{"tests":12}`),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return closure
}

func TestTaskExecutionEvidenceRetainsRetriesAndRollbackAudit(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	project, plan, tasks := createReadyPlanFixture(t, store, "/tmp/task-execution-evidence")
	task := tasks[0]

	queueJob := createTaskEvidenceJob(t, store, project.ID, task.ID, "evidence-queue-job")
	initialRun := createTaskEvidenceRun(t, store, project, plan, task, queueJob, 1)
	initial := saveEvidenceClosure(t, store, project, plan, task, queueJob, initialRun,
		domain.TaskExecutionAttemptOriginInitial, 1, domain.GitReferenceStateBranch, "main", strings.Repeat("1", 40), "1")

	queueRetryRun := createTaskEvidenceRun(t, store, project, plan, task, queueJob, 2)
	queueRetry := saveEvidenceClosure(t, store, project, plan, task, queueJob, queueRetryRun,
		domain.TaskExecutionAttemptOriginQueueRetry, 2, domain.GitReferenceStateDetached, "", strings.Repeat("2", 40), "2")

	manualJob := createTaskEvidenceJob(t, store, project.ID, task.ID, "evidence-manual-job")
	manualRun := createTaskEvidenceRun(t, store, project, plan, task, manualJob, 1)
	manualRetry := saveEvidenceClosure(t, store, project, plan, task, manualJob, manualRun,
		domain.TaskExecutionAttemptOriginManualRetry, 1, domain.GitReferenceStateUnborn, "main", "", "3")

	history, err := store.ListTaskExecutionClosures(ctx, plan.ID, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 3 {
		t.Fatalf("execution history length=%d, want 3", len(history))
	}
	if history[0].Attempt.ID != initial.Attempt.ID || history[1].Attempt.ID != queueRetry.Attempt.ID || history[2].Attempt.ID != manualRetry.Attempt.ID {
		t.Fatalf("unexpected attempt order: %+v", history)
	}
	if history[1].Attempt.SupersedesAttemptID == nil || *history[1].Attempt.SupersedesAttemptID != initial.Attempt.ID ||
		history[2].Attempt.SupersedesAttemptID == nil || *history[2].Attempt.SupersedesAttemptID != queueRetry.Attempt.ID {
		t.Fatalf("supersession chain was not retained: %+v", history)
	}
	for _, closure := range history {
		if len(closure.Checkpoints) != 2 || len(closure.FileChanges) != 1 || len(closure.ValidationEvidence) != 1 {
			t.Fatalf("incomplete execution closure: %+v", closure)
		}
		if closure.Checkpoints[0].ProjectID != project.ID || closure.Checkpoints[0].PlanID != plan.ID || closure.Checkpoints[0].TaskID != task.ID ||
			closure.Checkpoints[0].JobID != closure.Attempt.JobID || closure.Checkpoints[0].AgentRunID != closure.Attempt.AgentRunID {
			t.Fatalf("checkpoint linkage differs from attempt: %+v", closure)
		}
	}

	mismatchedRun := createTaskEvidenceRun(t, store, project, plan, task, queueJob, 3)
	startedAt := time.Now().Add(-time.Second).UTC()
	_, err = store.SaveTaskExecutionClosure(ctx, SaveTaskExecutionClosureParams{
		ProjectID: project.ID, PlanID: plan.ID, TaskID: task.ID,
		JobID: manualJob.ID, AgentRunID: mismatchedRun,
		AttemptOrigin: domain.TaskExecutionAttemptOriginQueueRetry, QueueAttempt: 3,
		Outcome: domain.TaskExecutionOutcomeSucceeded, StartedAt: startedAt, FinishedAt: startedAt.Add(time.Second),
		BeforeCheckpoint: checkpointEvidenceInput(uuid.New(), domain.GitReferenceStateBranch, "main", strings.Repeat("4", 40), "4"),
		AfterCheckpoint:  checkpointEvidenceInput(uuid.New(), domain.GitReferenceStateBranch, "main", strings.Repeat("4", 40), "4"),
	})
	if err == nil {
		t.Fatal("inconsistent job/agent-run linkage unexpectedly produced execution evidence")
	}

	rollback, err := store.RecordTaskExecutionRollback(ctx, RecordTaskExecutionRollbackParams{
		AttemptID:          queueRetry.Attempt.ID,
		SourceCheckpointID: queueRetry.Checkpoints[1].ID,
		TargetCheckpointID: queueRetry.Checkpoints[0].ID,
		RollbackKind:       domain.TaskExecutionRollbackKindManual,
		CommandSummary:     "git restore checkpoint",
		Reason:             "revert failed retry",
		RequestedBy:        "operator@example.test",
		Details:            json.RawMessage(`{"channel":"ui"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if rollback.Status != domain.TaskExecutionRollbackRequested || len(rollback.Events) != 1 {
		t.Fatalf("requested rollback=%+v", rollback)
	}
	if _, err = store.AppendTaskExecutionRollbackEvent(ctx, AppendTaskExecutionRollbackEventParams{
		RollbackID: rollback.Operation.ID, Status: domain.TaskExecutionRollbackRunning, Message: "restoring",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = store.AppendTaskExecutionRollbackEvent(ctx, AppendTaskExecutionRollbackEventParams{
		RollbackID: rollback.Operation.ID, Status: domain.TaskExecutionRollbackSucceeded, Message: "restored",
	}); err != nil {
		t.Fatal(err)
	}

	latest, err := store.GetLatestTaskExecutionClosure(ctx, plan.ID, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if latest.Attempt.ID != manualRetry.Attempt.ID || len(latest.Rollbacks) != 0 {
		t.Fatalf("latest closure=%+v", latest)
	}
	rolledBack, err := store.GetTaskExecutionClosure(ctx, queueRetry.Attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rolledBack.Rollbacks) != 1 || rolledBack.Rollbacks[0].Status != domain.TaskExecutionRollbackSucceeded || len(rolledBack.Rollbacks[0].Events) != 3 {
		t.Fatalf("rollback history=%+v", rolledBack.Rollbacks)
	}
	if len(rolledBack.Checkpoints) != 2 || len(rolledBack.FileChanges) != 1 || len(rolledBack.ValidationEvidence) != 1 {
		t.Fatalf("rollback removed original evidence: %+v", rolledBack)
	}

	if _, err = store.Pool.Exec(ctx, `UPDATE task_execution_checkpoints SET current_branch='changed' WHERE id=$1`, initial.Checkpoints[0].ID); err == nil {
		t.Fatal("immutable checkpoint unexpectedly allowed an update")
	}

	if err = store.DeleteProject(ctx, project.ID, project.Version); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{
		"task_execution_attempts", "task_execution_checkpoints", "task_execution_file_changes",
		"task_execution_validations", "task_execution_rollbacks", "task_execution_rollback_events",
	} {
		var count int
		if err = store.Pool.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("%s retained %d rows after project deletion", table, count)
		}
	}
}
