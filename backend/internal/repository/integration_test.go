package repository

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lyming99/specrelay/backend/internal/domain"
	"github.com/lyming99/specrelay/backend/internal/migrations"
	"github.com/lyming99/specrelay/backend/internal/planspec"
	"os"
	"sync"
	"testing"
	"time"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err = migrations.Run(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
	if err = migrations.Run(context.Background(), pool); err != nil {
		t.Fatalf("migration not idempotent: %v", err)
	}
	_, err = pool.Exec(context.Background(), `TRUNCATE access_tokens,agent_runs,events,workspace_leases,jobs,plan_tasks,plans,attachments,intakes,project_settings,projects RESTART IDENTITY CASCADE`)
	if err != nil {
		t.Fatal(err)
	}
	return New(pool)
}

func decodeJobPayload(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	payload := map[string]any{}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("decode job payload: %v (%s)", err, raw)
	}
	return payload
}

func assertJobProvider(t *testing.T, job domain.Job, provider string, requested *bool) {
	t.Helper()
	payload := decodeJobPayload(t, job.Payload)
	value, hasProvider := payload["provider"]
	if provider == "" {
		if hasProvider {
			t.Fatalf("job %s unexpectedly has provider %v in payload %s", job.ID, value, job.Payload)
		}
	} else if !hasProvider || value != provider {
		t.Fatalf("job %s provider=%v, want %q (payload %s)", job.ID, value, provider, job.Payload)
	}
	if requested == nil {
		if _, ok := payload["providerRequested"]; ok {
			t.Fatalf("job %s unexpectedly marks providerRequested in payload %s", job.ID, job.Payload)
		}
		return
	}
	value, ok := payload["providerRequested"].(bool)
	if !ok || value != *requested {
		t.Fatalf("job %s providerRequested=%v, want %t (payload %s)", job.ID, payload["providerRequested"], *requested, job.Payload)
	}
}

func boolPointer(value bool) *bool { return &value }
func TestPlanningJobsPersistExplicitProviderAndDefaultFallback(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	project, err := store.CreateProject(ctx, CreateProjectParams{Name: "Provider payloads", WorkspacePath: "/tmp/provider-payloads", NormalizedWorkspace: "/tmp/provider-payloads"})
	if err != nil {
		t.Fatal(err)
	}

	selectedIntake, selectedJob, err := store.CreateIntake(WithExecutionProvider(ctx, "claude"), CreateIntakeParams{ProjectID: project.ID, Kind: "requirement", Title: "Selected", Body: "Use Claude", ConfigSnapshot: json.RawMessage(`{}`), QueuePlan: true})
	if err != nil || selectedJob == nil {
		t.Fatalf("create selected intake: job=%v err=%v", selectedJob, err)
	}
	assertJobProvider(t, *selectedJob, "claude", nil)

	_, defaultJob, err := store.CreateIntake(ctx, CreateIntakeParams{ProjectID: project.ID, Kind: "requirement", Title: "Default", Body: "Use project default", ConfigSnapshot: json.RawMessage(`{}`), QueuePlan: true})
	if err != nil || defaultJob == nil {
		t.Fatalf("create default intake: job=%v err=%v", defaultJob, err)
	}
	assertJobProvider(t, *defaultJob, "", nil)

	if _, err = store.Pool.Exec(ctx, `UPDATE jobs SET status='cancelled' WHERE id=$1`, selectedJob.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Pool.Exec(ctx, `UPDATE intakes SET status='open' WHERE id=$1`, selectedIntake.ID); err != nil {
		t.Fatal(err)
	}
	selectedIntake, err = store.GetIntake(ctx, selectedIntake.ID)
	if err != nil {
		t.Fatal(err)
	}
	manualJob, err := store.QueuePlanGeneration(WithExecutionProvider(ctx, "codex"), selectedIntake.ID, selectedIntake.Version)
	if err != nil {
		t.Fatal(err)
	}
	assertJobProvider(t, manualJob, "codex", nil)
}

func TestTransactionalCreateAndOptimisticLock(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	project, err := store.CreateProject(ctx, CreateProjectParams{Name: "Demo", WorkspacePath: "/tmp/demo", NormalizedWorkspace: "/tmp/demo"})
	if err != nil {
		t.Fatal(err)
	}
	var eventCount, settingsCount int
	if err = store.Pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE aggregate_id=$1`, project.ID).Scan(&eventCount); err != nil {
		t.Fatal(err)
	}
	if err = store.Pool.QueryRow(ctx, `SELECT count(*) FROM project_settings WHERE project_id=$1`, project.ID).Scan(&settingsCount); err != nil {
		t.Fatal(err)
	}
	if eventCount != 1 || settingsCount != 1 {
		t.Fatalf("event=%d settings=%d", eventCount, settingsCount)
	}
	_, err = store.UpdateProject(ctx, project.ID, UpdateProjectParams{Name: "Wrong", WorkspacePath: "/tmp/demo", NormalizedWorkspace: "/tmp/demo", Version: 99})
	if !errors.Is(err, domain.ErrVersionConflict) {
		t.Fatalf("expected version conflict, got %v", err)
	}
}
func TestSkipLockedClaimAndWorkspaceLease(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	project, err := store.CreateProject(ctx, CreateProjectParams{Name: "Queue", WorkspacePath: "/tmp/queue", NormalizedWorkspace: "/tmp/queue"})
	if err != nil {
		t.Fatal(err)
	}
	project, err = store.SetAutomation(ctx, project.ID, true, project.Version)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = store.CreateIntake(ctx, CreateIntakeParams{ProjectID: project.ID, Kind: "requirement", Title: "One", Body: "Body", ConfigSnapshot: json.RawMessage(`{}`), QueuePlan: true})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	results := make(chan error, 2)
	for _, worker := range []string{"a", "b"} {
		go func(worker string) {
			defer wg.Done()
			_, err := store.ClaimJob(ctx, worker, 30*time.Second)
			results <- err
		}(worker)
	}
	wg.Wait()
	close(results)
	claimed, empty := 0, 0
	for err := range results {
		if err == nil {
			claimed++
		} else if errors.Is(err, pgx.ErrNoRows) {
			empty++
		} else {
			t.Fatal(err)
		}
	}
	if claimed != 1 || empty != 1 {
		t.Fatalf("claimed=%d empty=%d", claimed, empty)
	}
	job1 := uuid.New()
	job2 := uuid.New()
	_, err = store.Pool.Exec(ctx, `INSERT INTO jobs(id,project_id,job_type,aggregate_type,aggregate_id,idempotency_key) VALUES($1,$3,'task.execute','task',$1,$4),($2,$3,'task.execute','task',$2,$5)`, job1, job2, project.ID, "lease-1", "lease-2")
	if err != nil {
		t.Fatal(err)
	}
	if err = store.AcquireWorkspaceLease(ctx, project.ID, job1, "/tmp/queue", "a", 30*time.Second); err != nil {
		t.Fatal(err)
	}
	if err = store.AcquireWorkspaceLease(ctx, project.ID, job2, "/tmp/queue", "b", 30*time.Second); err == nil {
		t.Fatal("expected competing lease to fail")
	}
}

func TestAutomationStartQueuesExistingIntakes(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	project, err := store.CreateProject(ctx, CreateProjectParams{Name: "Start", WorkspacePath: "/tmp/start", NormalizedWorkspace: "/tmp/start"})
	if err != nil {
		t.Fatal(err)
	}
	openIntake, _, err := store.CreateIntake(ctx, CreateIntakeParams{ProjectID: project.ID, Kind: "requirement", Title: "Open", Body: "Body", ConfigSnapshot: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	failedIntake, _, err := store.CreateIntake(ctx, CreateIntakeParams{ProjectID: project.ID, Kind: "requirement", Title: "Failed", Body: "Body", ConfigSnapshot: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Pool.Exec(ctx, `UPDATE intakes SET status='plan_failed' WHERE id=$1`, failedIntake.ID); err != nil {
		t.Fatal(err)
	}
	project, err = store.SetAutomation(ctx, project.ID, true, project.Version)
	if err != nil {
		t.Fatal(err)
	}
	for _, intakeID := range []uuid.UUID{openIntake.ID, failedIntake.ID} {
		intake, getErr := store.GetIntake(ctx, intakeID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if intake.Status != "planning" {
			t.Fatalf("intake %s status=%s", intakeID, intake.Status)
		}
		var count int
		if err = store.Pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE aggregate_id=$1 AND job_type='plan.generate' AND status='queued'`, intakeID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("intake %s queued jobs=%d", intakeID, count)
		}
		job, scanErr := scanJob(store.Pool.QueryRow(ctx, `SELECT id,project_id,job_type,aggregate_type,aggregate_id,payload,priority,status,run_after,coalesce(worker_id,''),lease_expires_at,attempt,max_attempts,coalesce(last_error,''),idempotency_key,created_at,updated_at,version FROM jobs WHERE aggregate_id=$1 AND job_type='plan.generate' AND status='queued'`, intakeID))
		if scanErr != nil {
			t.Fatal(scanErr)
		}
		assertJobProvider(t, job, "", nil)
	}
}

func TestAutomationStopRestoresQueuedPlanningIntake(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	project, err := store.CreateProject(ctx, CreateProjectParams{Name: "Stop", WorkspacePath: "/tmp/stop", NormalizedWorkspace: "/tmp/stop"})
	if err != nil {
		t.Fatal(err)
	}
	project, err = store.SetAutomation(ctx, project.ID, true, project.Version)
	if err != nil {
		t.Fatal(err)
	}
	intake, job, err := store.CreateIntake(ctx, CreateIntakeParams{ProjectID: project.ID, Kind: "requirement", Title: "Queued", Body: "Body", ConfigSnapshot: json.RawMessage(`{}`), QueuePlan: true})
	if err != nil || job == nil {
		t.Fatalf("create intake: job=%v err=%v", job, err)
	}
	project, err = store.SetAutomation(ctx, project.ID, false, project.Version)
	if err != nil {
		t.Fatal(err)
	}
	intake, err = store.GetIntake(ctx, intake.ID)
	if err != nil {
		t.Fatal(err)
	}
	if intake.Status != "open" {
		t.Fatalf("intake status=%s", intake.Status)
	}
	var status string
	if err = store.Pool.QueryRow(ctx, `SELECT status FROM jobs WHERE id=$1`, job.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "cancelled" {
		t.Fatalf("job status=%s", status)
	}
}

func TestRecoverExpiredRunningJobAndWorkspaceLease(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	project, err := store.CreateProject(ctx, CreateProjectParams{Name: "Recover", WorkspacePath: "/tmp/recover", NormalizedWorkspace: "/tmp/recover"})
	if err != nil {
		t.Fatal(err)
	}
	jobID := uuid.New()
	_, err = store.Pool.Exec(ctx, `INSERT INTO jobs(id,project_id,job_type,aggregate_type,aggregate_id,status,worker_id,lease_expires_at,idempotency_key) VALUES($1,$2,'task.execute','task',$1,'running','dead-worker',now()-interval '1 second',$3)`, jobID, project.ID, "recover-job")
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Pool.Exec(ctx, `INSERT INTO workspace_leases(id,project_id,workspace_path_normalized,worker_id,job_id,expires_at) VALUES($1,$2,$3,'dead-worker',$4,now()-interval '1 second')`, uuid.New(), project.ID, "/tmp/recover", jobID)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.RecoverJobs(ctx); err != nil {
		t.Fatal(err)
	}
	var status string
	var workerID *string
	if err = store.Pool.QueryRow(ctx, `SELECT status,worker_id FROM jobs WHERE id=$1`, jobID).Scan(&status, &workerID); err != nil {
		t.Fatal(err)
	}
	if status != "queued" || workerID != nil {
		t.Fatalf("status=%s worker=%v", status, workerID)
	}
	var leases int
	if err = store.Pool.QueryRow(ctx, `SELECT count(*) FROM workspace_leases WHERE job_id=$1`, jobID).Scan(&leases); err != nil {
		t.Fatal(err)
	}
	if leases != 0 {
		t.Fatalf("expired lease count=%d", leases)
	}
}

func TestInsertJobIdempotencyKeyReturnsExistingJob(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	project, err := store.CreateProject(ctx, CreateProjectParams{Name: "Idempotent", WorkspacePath: "/tmp/idempotent", NormalizedWorkspace: "/tmp/idempotent"})
	if err != nil {
		t.Fatal(err)
	}
	tx, err := store.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	first, err := insertJob(ctx, tx, NewJob{ID: uuid.New(), ProjectID: project.ID, Type: "plan.generate", AggregateType: "intake", AggregateID: uuid.New(), IdempotencyKey: "same-key"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := insertJob(ctx, tx, NewJob{ID: uuid.New(), ProjectID: project.ID, Type: "plan.generate", AggregateType: "intake", AggregateID: uuid.New(), IdempotencyKey: "same-key"})
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID {
		t.Fatalf("idempotent jobs differ: %s != %s", first.ID, second.ID)
	}
}

func createReadyPlanFixture(t *testing.T, store *Store, workspace string) (domain.Project, domain.Plan, []domain.PlanTask) {
	t.Helper()
	ctx := context.Background()
	project, err := store.CreateProject(ctx, CreateProjectParams{Name: "Lifecycle", WorkspacePath: workspace, NormalizedWorkspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	settings, err := store.GetProjectSettings(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	settings.MaxRetries = 4
	if _, err = store.UpdateProjectSettings(ctx, settings); err != nil {
		t.Fatal(err)
	}
	project, err = store.SetAutomation(ctx, project.ID, true, project.Version)
	if err != nil {
		t.Fatal(err)
	}
	intake, generationJob, err := store.CreateIntake(ctx, CreateIntakeParams{ProjectID: project.ID, Kind: "requirement", Title: "Lifecycle", Body: "Exercise the queue", ConfigSnapshot: json.RawMessage(`{}`), QueuePlan: true})
	if err != nil {
		t.Fatal(err)
	}
	if generationJob == nil || generationJob.MaxAttempts != 5 {
		t.Fatalf("generation job=%+v", generationJob)
	}
	if _, err = store.Pool.Exec(ctx, `UPDATE jobs SET status='cancelled' WHERE id=$1`, generationJob.ID); err != nil {
		t.Fatal(err)
	}
	spec := planspec.Spec{Title: "Lifecycle plan", Summary: "Exercise task transitions", Tasks: []planspec.Task{{Title: "Implement", Scope: []string{"backend"}, Acceptance: []string{"implementation passes"}}}, FinalValidation: []string{"all tests pass"}}
	plan, tasks, err := store.SaveGeneratedPlan(ctx, intake, spec, planspec.Render(spec))
	if err != nil {
		t.Fatal(err)
	}
	return project, plan, tasks
}

func TestAutomationStartQueuesReadyPlans(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	project, err := store.CreateProject(ctx, CreateProjectParams{Name: "Automation ready", WorkspacePath: "/tmp/automation-ready", NormalizedWorkspace: "/tmp/automation-ready"})
	if err != nil {
		t.Fatal(err)
	}
	intake, generationJob, err := store.CreateIntake(ctx, CreateIntakeParams{ProjectID: project.ID, Kind: "requirement", Title: "Ready before automation", Body: "Queue the first task on start", ConfigSnapshot: json.RawMessage(`{}`), QueuePlan: true})
	if err != nil {
		t.Fatal(err)
	}
	if generationJob == nil {
		t.Fatal("expected plan generation job")
	}
	if _, err = store.Pool.Exec(ctx, `UPDATE jobs SET status='cancelled' WHERE id=$1`, generationJob.ID); err != nil {
		t.Fatal(err)
	}
	spec := planspec.Spec{Title: "Ready plan", Summary: "Verify automation start", Tasks: []planspec.Task{{Title: "Implement", Scope: []string{"backend"}, Acceptance: []string{"passes"}}}, FinalValidation: []string{"tests pass"}}
	plan, tasks, err := store.SaveGeneratedPlan(ctx, intake, spec, planspec.Render(spec))
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != "ready" || tasks[0].Status != "pending" {
		t.Fatalf("plan=%+v task=%+v", plan, tasks[0])
	}

	project, err = store.SetAutomation(ctx, project.ID, true, project.Version)
	if err != nil {
		t.Fatal(err)
	}
	started, err := store.GetPlan(ctx, plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	firstTask, err := store.GetTask(ctx, tasks[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if !project.AutomationEnabled || started.Status != "running" || firstTask.Status != "queued" {
		t.Fatalf("project=%+v plan=%+v task=%+v", project, started, firstTask)
	}
	var jobCount int
	if err = store.Pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE project_id=$1 AND aggregate_id=$2 AND job_type='task.execute' AND status='queued'`, project.ID, firstTask.ID).Scan(&jobCount); err != nil {
		t.Fatal(err)
	}
	if jobCount != 1 {
		t.Fatalf("queued task jobs=%d", jobCount)
	}
	job, err := scanJob(store.Pool.QueryRow(ctx, `SELECT id,project_id,job_type,aggregate_type,aggregate_id,payload,priority,status,run_after,coalesce(worker_id,''),lease_expires_at,attempt,max_attempts,coalesce(last_error,''),idempotency_key,created_at,updated_at,version FROM jobs WHERE project_id=$1 AND aggregate_id=$2 AND job_type='task.execute' AND status='queued'`, project.ID, firstTask.ID))
	if err != nil {
		t.Fatal(err)
	}
	assertJobProvider(t, job, "", nil)
}

func TestQueuePlanAutomaticallyRequiresReadyAutomatedPlan(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	project, err := store.CreateProject(ctx, CreateProjectParams{Name: "Automatic queue", WorkspacePath: "/tmp/automatic-queue", NormalizedWorkspace: "/tmp/automatic-queue"})
	if err != nil {
		t.Fatal(err)
	}
	project, err = store.SetAutomation(ctx, project.ID, true, project.Version)
	if err != nil {
		t.Fatal(err)
	}
	intake, generationJob, err := store.CreateIntake(ctx, CreateIntakeParams{ProjectID: project.ID, Kind: "requirement", Title: "Generated while automated", Body: "Queue after plan generation", ConfigSnapshot: json.RawMessage(`{}`), QueuePlan: true})
	if err != nil {
		t.Fatal(err)
	}
	if generationJob == nil {
		t.Fatal("expected plan generation job")
	}
	if _, err = store.Pool.Exec(ctx, `UPDATE jobs SET status='cancelled' WHERE id=$1`, generationJob.ID); err != nil {
		t.Fatal(err)
	}
	spec := planspec.Spec{Title: "Automatic plan", Summary: "Verify plan completion queues a task", Tasks: []planspec.Task{{Title: "Implement", Scope: []string{"backend"}, Acceptance: []string{"passes"}}}, FinalValidation: []string{"tests pass"}}
	plan, tasks, err := store.SaveGeneratedPlan(ctx, intake, spec, planspec.Render(spec))
	if err != nil {
		t.Fatal(err)
	}
	job, queued, err := store.QueuePlanAutomatically(ctx, plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !queued || job.Type != "task.execute" || job.AggregateID != tasks[0].ID {
		t.Fatalf("queued=%t job=%+v", queued, job)
	}
	assertJobProvider(t, job, "", nil)
	if _, queued, err = store.QueuePlanAutomatically(ctx, plan.ID); err != nil || queued {
		t.Fatalf("second queue queued=%t err=%v", queued, err)
	}
}

func TestQueueTaskRetryStopAndConfiguredAttempts(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	_, plan, tasks := createReadyPlanFixture(t, store, "/tmp/task-lifecycle")
	if len(tasks) != 2 {
		t.Fatalf("tasks=%d", len(tasks))
	}
	if _, err := store.QueueTask(ctx, tasks[1].ID, tasks[1].Version); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("expected ordered execution rejection, got %v", err)
	}
	job, err := store.QueuePlan(WithExecutionProvider(ctx, "claude"), plan.ID, plan.Version)
	if err != nil {
		t.Fatal(err)
	}
	if job.MaxAttempts != 5 {
		t.Fatalf("max attempts=%d", job.MaxAttempts)
	}
	assertJobProvider(t, job, "claude", boolPointer(true))
	claimed, err := store.ClaimJob(ctx, "retry-worker", 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err = store.MarkJobRunning(ctx, claimed.ID, "retry-worker")
	if err != nil {
		t.Fatal(err)
	}
	running, err := store.StartTask(ctx, tasks[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.ReturnTaskQueuedForRetry(ctx, running, "session-retry", "temporary failure"); err != nil {
		t.Fatal(err)
	}
	if err = store.FailJob(ctx, claimed, "retry-worker", "temporary failure", true); err != nil {
		t.Fatal(err)
	}
	retrying, err := scanJob(store.Pool.QueryRow(ctx, `SELECT id,project_id,job_type,aggregate_type,aggregate_id,payload,priority,status,run_after,coalesce(worker_id,''),lease_expires_at,attempt,max_attempts,coalesce(last_error,''),idempotency_key,created_at,updated_at,version FROM jobs WHERE id=$1`, claimed.ID))
	if err != nil {
		t.Fatal(err)
	}
	if retrying.Status != "retry_wait" {
		t.Fatalf("retrying job status=%s", retrying.Status)
	}
	assertJobProvider(t, retrying, "claude", boolPointer(true))
	queued, err := store.GetTask(ctx, running.ID)
	if err != nil {
		t.Fatal(err)
	}
	if queued.Status != "queued" || queued.SessionID != "session-retry" {
		t.Fatalf("task=%+v", queued)
	}
	stopped, active, err := store.StopTask(ctx, queued.ID, queued.Version)
	if err != nil {
		t.Fatal(err)
	}
	if stopped.Status != "pending" || len(active) != 0 {
		t.Fatalf("stopped=%+v active=%v", stopped, active)
	}
	var jobStatus string
	if err = store.Pool.QueryRow(ctx, `SELECT status FROM jobs WHERE id=$1`, claimed.ID).Scan(&jobStatus); err != nil {
		t.Fatal(err)
	}
	if jobStatus != "cancelled" {
		t.Fatalf("job status=%s", jobStatus)
	}
	requeued, err := store.QueueTask(WithExecutionProvider(ctx, "codex"), stopped.ID, stopped.Version)
	if err != nil {
		t.Fatal(err)
	}
	if requeued.MaxAttempts != 5 {
		t.Fatalf("requeued max attempts=%d", requeued.MaxAttempts)
	}
	assertJobProvider(t, requeued, "codex", boolPointer(true))
}

func TestPlanProviderPropagationCanBeCleared(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	_, plan, tasks := createReadyPlanFixture(t, store, "/tmp/provider-clear")
	if _, err := store.Pool.Exec(ctx, `UPDATE plans SET config_snapshot=$2 WHERE id=$1`, plan.ID, json.RawMessage(`{"executionAgentProvider":"claude"}`)); err != nil {
		t.Fatal(err)
	}
	job, err := store.QueuePlan(WithExecutionProvider(ctx, ""), plan.ID, plan.Version)
	if err != nil {
		t.Fatal(err)
	}
	assertJobProvider(t, job, "", boolPointer(true))
	first, err := store.StartTask(ctx, tasks[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.FinishTask(ctx, first, "", true, "done"); err != nil {
		t.Fatal(err)
	}
	validationJob, err := scanJob(store.Pool.QueryRow(ctx, `SELECT id,project_id,job_type,aggregate_type,aggregate_id,payload,priority,status,run_after,coalesce(worker_id,''),lease_expires_at,attempt,max_attempts,coalesce(last_error,''),idempotency_key,created_at,updated_at,version FROM jobs WHERE aggregate_id=$1 AND status='queued'`, tasks[1].ID))
	if err != nil {
		t.Fatal(err)
	}
	assertJobProvider(t, validationJob, "", nil)
}

func TestSuccessfulTasksQueueValidationAndCompletePlan(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	_, plan, tasks := createReadyPlanFixture(t, store, "/tmp/task-success")
	firstJob, err := store.QueuePlan(WithExecutionProvider(ctx, "claude"), plan.ID, plan.Version)
	if err != nil {
		t.Fatal(err)
	}
	assertJobProvider(t, firstJob, "claude", boolPointer(true))
	first, err := store.StartTask(ctx, tasks[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.FinishTask(ctx, first, "session-success", true, "done"); err != nil {
		t.Fatal(err)
	}
	var validationJob domain.Job
	validationJob, err = scanJob(store.Pool.QueryRow(ctx, `SELECT id,project_id,job_type,aggregate_type,aggregate_id,payload,priority,status,run_after,coalesce(worker_id,''),lease_expires_at,attempt,max_attempts,coalesce(last_error,''),idempotency_key,created_at,updated_at,version FROM jobs WHERE aggregate_id=$1 AND status='queued'`, tasks[1].ID))
	if err != nil {
		t.Fatal(err)
	}
	if validationJob.MaxAttempts != 5 || validationJob.ID == firstJob.ID {
		t.Fatalf("validation job=%+v", validationJob)
	}
	assertJobProvider(t, validationJob, "claude", nil)
	validation, err := store.StartTask(ctx, tasks[1].ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.FinishTask(ctx, validation, "", true, "validated"); err != nil {
		t.Fatal(err)
	}
	completed, err := store.GetPlan(ctx, plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if completed.Status != "completed" {
		t.Fatalf("plan status=%s", completed.Status)
	}
	intake, err := store.GetIntake(ctx, completed.IntakeID)
	if err != nil {
		t.Fatal(err)
	}
	if intake.Status != "closed" {
		t.Fatalf("intake status=%s", intake.Status)
	}
}

func TestAgentRunListAndGet(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	project, err := store.CreateProject(ctx, CreateProjectParams{Name: "Runs", WorkspacePath: "/tmp/runs", NormalizedWorkspace: "/tmp/runs"})
	if err != nil {
		t.Fatal(err)
	}
	runID := uuid.New()
	logPath := "/tmp/runs.log"
	if err = store.StartAgentRun(ctx, AgentRunStart{ID: runID, ProjectID: project.ID, Provider: "codex", CommandSummary: "codex（需求讨论）", LogPath: logPath}); err != nil {
		t.Fatal(err)
	}
	if err = store.SetAgentRunPID(ctx, runID, 1234); err != nil {
		t.Fatal(err)
	}
	if err = store.FinishAgentRun(ctx, runID, "succeeded", 0, "session-1", "", 1500*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	run, err := store.GetAgentRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.JobID != nil || run.PID == nil || *run.PID != 1234 || run.Status != "succeeded" || run.DurationMS != 1500 || run.LogPath != logPath {
		t.Fatalf("unexpected run: %+v", run)
	}
	items, err := store.ListAgentRuns(ctx, project.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].ID != runID {
		t.Fatalf("unexpected runs: %+v", items)
	}
}

func TestAutomationStopReconcilesRunningTaskAndAgentRun(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	project, plan, tasks := createReadyPlanFixture(t, store, "/tmp/automation-stop-running")

	job, err := store.QueuePlan(WithExecutionProvider(ctx, ""), plan.ID, plan.Version)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimJob(ctx, "stop-worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.ID != job.ID {
		t.Fatalf("claimed job=%s, want %s", claimed.ID, job.ID)
	}
	if _, err = store.MarkJobRunning(ctx, job.ID, "stop-worker"); err != nil {
		t.Fatal(err)
	}
	running, err := store.StartTask(ctx, tasks[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	runID := uuid.New()
	if err = store.StartAgentRun(ctx, AgentRunStart{
		ID:             runID,
		ProjectID:      project.ID,
		JobID:          &job.ID,
		TaskID:         &running.ID,
		Provider:       "codex",
		CommandSummary: "codex",
		LogPath:        "/tmp/automation-stop-running.log",
	}); err != nil {
		t.Fatal(err)
	}
	if err = store.SetAgentRunPID(ctx, runID, 1234); err != nil {
		t.Fatal(err)
	}

	project, err = store.SetAutomation(ctx, project.ID, false, project.Version)
	if err != nil {
		t.Fatal(err)
	}
	if project.AutomationEnabled {
		t.Fatal("automation remains enabled")
	}

	var jobStatus string
	if err = store.Pool.QueryRow(ctx, `SELECT status FROM jobs WHERE id=$1`, job.ID).Scan(&jobStatus); err != nil {
		t.Fatal(err)
	}
	if jobStatus != "cancelled" {
		t.Fatalf("job status=%q, want cancelled", jobStatus)
	}
	stoppedTask, err := store.GetTask(ctx, running.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stoppedTask.Status != "pending" || stoppedTask.StartedAt != nil || stoppedTask.FinishedAt != nil {
		t.Fatalf("task was not reset after automation stop: %+v", stoppedTask)
	}
	stoppedPlan, err := store.GetPlan(ctx, plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stoppedPlan.Status != "ready" {
		t.Fatalf("plan status=%q, want ready", stoppedPlan.Status)
	}
	stoppedRun, err := store.GetAgentRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if stoppedRun.Status != "cancelled" || stoppedRun.FinishedAt == nil || stoppedRun.TerminationReason != "project automation stopped" {
		t.Fatalf("agent run was not reconciled: %+v", stoppedRun)
	}

	// The worker can observe SIGTERM after the transactional reset. Both late
	// cleanup calls must be harmless and must not resurrect the run state.
	if err = store.ReturnTaskPending(ctx, running, "cancelled by automation stop"); err != nil {
		t.Fatalf("late task cleanup: %v", err)
	}
	if err = store.FinishAgentRun(ctx, runID, "succeeded", 0, "", "", time.Second); err != nil {
		t.Fatalf("late run cleanup: %v", err)
	}
	stoppedRun, err = store.GetAgentRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if stoppedRun.Status != "cancelled" || stoppedRun.TerminationReason != "project automation stopped" {
		t.Fatalf("late cleanup overwrote cancellation: %+v", stoppedRun)
	}
}
