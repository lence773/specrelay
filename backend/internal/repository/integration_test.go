package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lyming99/specrelay/backend/internal/domain"
	"github.com/lyming99/specrelay/backend/internal/migrations"
	"github.com/lyming99/specrelay/backend/internal/planspec"
	"os"
	"strings"
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

func assertProviderSettingsUnchanged(t *testing.T, before, after domain.ProjectSettings) {
	t.Helper()
	if after.AgentProvider != before.AgentProvider || after.Version != before.Version {
		t.Fatalf("project provider settings changed: before provider=%q version=%d, after provider=%q version=%d", before.AgentProvider, before.Version, after.AgentProvider, after.Version)
	}
}

func TestPlanningJobsPersistExplicitProviderAndDefaultFallback(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	project, err := store.CreateProject(ctx, CreateProjectParams{Name: "Provider payloads", WorkspacePath: "/tmp/provider-payloads", NormalizedWorkspace: "/tmp/provider-payloads"})
	if err != nil {
		t.Fatal(err)
	}
	originalSettings, err := store.GetProjectSettings(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}

	selectedIntake, selectedJob, err := store.CreateIntake(WithExecutionProvider(ctx, "claude"), CreateIntakeParams{ProjectID: project.ID, Kind: "requirement", Title: "Selected", Body: "Use Claude", ConfigSnapshot: json.RawMessage(`{}`), QueuePlan: true})
	if err != nil || selectedJob == nil {
		t.Fatalf("create selected intake: job=%v err=%v", selectedJob, err)
	}
	assertJobProvider(t, *selectedJob, "claude", nil)
	persistedSelectedJob, err := scanJob(store.Pool.QueryRow(ctx, `SELECT id,project_id,job_type,aggregate_type,aggregate_id,payload,priority,status,run_after,coalesce(worker_id,''),lease_expires_at,attempt,max_attempts,coalesce(last_error,''),idempotency_key,created_at,updated_at,version FROM jobs WHERE id=$1`, selectedJob.ID))
	if err != nil {
		t.Fatal(err)
	}
	assertJobProvider(t, persistedSelectedJob, "claude", nil)

	_, defaultJob, err := store.CreateIntake(ctx, CreateIntakeParams{ProjectID: project.ID, Kind: "requirement", Title: "Default", Body: "Use project default", ConfigSnapshot: json.RawMessage(`{}`), QueuePlan: true})
	if err != nil || defaultJob == nil {
		t.Fatalf("create default intake: job=%v err=%v", defaultJob, err)
	}
	assertJobProvider(t, *defaultJob, "", nil)
	persistedDefaultJob, err := scanJob(store.Pool.QueryRow(ctx, `SELECT id,project_id,job_type,aggregate_type,aggregate_id,payload,priority,status,run_after,coalesce(worker_id,''),lease_expires_at,attempt,max_attempts,coalesce(last_error,''),idempotency_key,created_at,updated_at,version FROM jobs WHERE id=$1`, defaultJob.ID))
	if err != nil {
		t.Fatal(err)
	}
	assertJobProvider(t, persistedDefaultJob, "", nil)

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
	persistedManualJob, err := scanJob(store.Pool.QueryRow(ctx, `SELECT id,project_id,job_type,aggregate_type,aggregate_id,payload,priority,status,run_after,coalesce(worker_id,''),lease_expires_at,attempt,max_attempts,coalesce(last_error,''),idempotency_key,created_at,updated_at,version FROM jobs WHERE id=$1`, manualJob.ID))
	if err != nil {
		t.Fatal(err)
	}
	assertJobProvider(t, persistedManualJob, "codex", nil)
	currentSettings, err := store.GetProjectSettings(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertProviderSettingsUnchanged(t, originalSettings, currentSettings)
}

func TestQueuePlanGenerationValidatesIntakeStateAtomically(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	project, err := store.CreateProject(ctx, CreateProjectParams{Name: "Plan state", WorkspacePath: "/tmp/plan-state", NormalizedWorkspace: "/tmp/plan-state"})
	if err != nil {
		t.Fatal(err)
	}

	countSideEffects := func(t *testing.T, intakeID uuid.UUID) (int, int) {
		t.Helper()
		var jobs, events int
		if err := store.Pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE aggregate_type='intake' AND aggregate_id=$1 AND job_type='plan.generate'`, intakeID).Scan(&jobs); err != nil {
			t.Fatal(err)
		}
		if err := store.Pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE aggregate_type='intake' AND aggregate_id=$1 AND event_type='intake.planning'`, intakeID).Scan(&events); err != nil {
			t.Fatal(err)
		}
		return jobs, events
	}

	for _, status := range []string{"open", "plan_failed"} {
		t.Run("queues_"+status, func(t *testing.T) {
			intake, _, createErr := store.CreateIntake(ctx, CreateIntakeParams{ProjectID: project.ID, Kind: "requirement", Title: status, Body: "Body", ConfigSnapshot: json.RawMessage(`{}`)})
			if createErr != nil {
				t.Fatal(createErr)
			}
			if status == "plan_failed" {
				if _, createErr = store.Pool.Exec(ctx, `UPDATE intakes SET status='plan_failed',updated_at=now(),version=version+1 WHERE id=$1`, intake.ID); createErr != nil {
					t.Fatal(createErr)
				}
				intake, createErr = store.GetIntake(ctx, intake.ID)
				if createErr != nil {
					t.Fatal(createErr)
				}
			}

			job, queueErr := store.QueuePlanGeneration(ctx, intake.ID, intake.Version)
			if queueErr != nil {
				t.Fatalf("queue %s intake: %v", status, queueErr)
			}
			if job.ProjectID != project.ID || job.Type != "plan.generate" || job.AggregateType != "intake" || job.AggregateID != intake.ID || job.Status != "queued" {
				t.Fatalf("unexpected job: %+v", job)
			}
			updated, getErr := store.GetIntake(ctx, intake.ID)
			if getErr != nil {
				t.Fatal(getErr)
			}
			if updated.Status != "planning" || updated.Version != intake.Version+1 {
				t.Fatalf("updated intake status=%s version=%d, want planning version=%d", updated.Status, updated.Version, intake.Version+1)
			}
			jobs, events := countSideEffects(t, intake.ID)
			if jobs != 1 || events != 1 {
				t.Fatalf("jobs=%d events=%d, want one of each", jobs, events)
			}
			var resourceVersion int64
			var payload json.RawMessage
			if getErr = store.Pool.QueryRow(ctx, `SELECT resource_version,payload FROM events WHERE aggregate_type='intake' AND aggregate_id=$1 AND event_type='intake.planning'`, intake.ID).Scan(&resourceVersion, &payload); getErr != nil {
				t.Fatal(getErr)
			}
			if resourceVersion != updated.Version || decodeJobPayload(t, payload)["jobId"] != job.ID.String() {
				t.Fatalf("planning event version=%d payload=%s, want version=%d jobId=%s", resourceVersion, payload, updated.Version, job.ID)
			}
		})
	}

	for _, status := range []string{"planning", "planned", "closed"} {
		t.Run("rejects_"+status, func(t *testing.T) {
			intake, _, createErr := store.CreateIntake(ctx, CreateIntakeParams{ProjectID: project.ID, Kind: "requirement", Title: status, Body: "Body", ConfigSnapshot: json.RawMessage(`{}`)})
			if createErr != nil {
				t.Fatal(createErr)
			}
			if _, createErr = store.Pool.Exec(ctx, `UPDATE intakes SET status=$2,updated_at=now(),version=version+1 WHERE id=$1`, intake.ID, status); createErr != nil {
				t.Fatal(createErr)
			}
			before, getErr := store.GetIntake(ctx, intake.ID)
			if getErr != nil {
				t.Fatal(getErr)
			}
			jobsBefore, eventsBefore := countSideEffects(t, intake.ID)

			_, queueErr := store.QueuePlanGeneration(ctx, intake.ID, before.Version)
			if !errors.Is(queueErr, domain.ErrInvalidTransition) {
				t.Fatalf("queue %s intake: got %v, want invalid transition", status, queueErr)
			}
			after, getErr := store.GetIntake(ctx, intake.ID)
			if getErr != nil {
				t.Fatal(getErr)
			}
			if after.Status != before.Status || after.Version != before.Version || !after.UpdatedAt.Equal(before.UpdatedAt) {
				t.Fatalf("intake changed: before status=%s version=%d updated=%s, after status=%s version=%d updated=%s", before.Status, before.Version, before.UpdatedAt, after.Status, after.Version, after.UpdatedAt)
			}
			jobsAfter, eventsAfter := countSideEffects(t, intake.ID)
			if jobsAfter != jobsBefore || eventsAfter != eventsBefore {
				t.Fatalf("side effects changed: jobs %d->%d events %d->%d", jobsBefore, jobsAfter, eventsBefore, eventsAfter)
			}
		})
	}

	t.Run("preserves_not_found_and_version_conflict", func(t *testing.T) {
		if _, queueErr := store.QueuePlanGeneration(ctx, uuid.New(), 1); !errors.Is(queueErr, domain.ErrNotFound) {
			t.Fatalf("missing intake: got %v, want not found", queueErr)
		}
		intake, _, createErr := store.CreateIntake(ctx, CreateIntakeParams{ProjectID: project.ID, Kind: "requirement", Title: "stale", Body: "Body", ConfigSnapshot: json.RawMessage(`{}`)})
		if createErr != nil {
			t.Fatal(createErr)
		}
		jobsBefore, eventsBefore := countSideEffects(t, intake.ID)
		if _, queueErr := store.QueuePlanGeneration(ctx, intake.ID, intake.Version-1); !errors.Is(queueErr, domain.ErrVersionConflict) {
			t.Fatalf("stale intake: got %v, want version conflict", queueErr)
		}
		after, getErr := store.GetIntake(ctx, intake.ID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if after.Status != intake.Status || after.Version != intake.Version || !after.UpdatedAt.Equal(intake.UpdatedAt) {
			t.Fatalf("stale request changed intake: before=%+v after=%+v", intake, after)
		}
		jobsAfter, eventsAfter := countSideEffects(t, intake.ID)
		if jobsAfter != jobsBefore || eventsAfter != eventsBefore {
			t.Fatalf("stale request side effects changed: jobs %d->%d events %d->%d", jobsBefore, jobsAfter, eventsBefore, eventsAfter)
		}
	})
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

func TestCreateFeedbackRequiresParentRequirementInSameProject(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	project, err := store.CreateProject(ctx, CreateProjectParams{Name: "Feedback parent", WorkspacePath: "/tmp/feedback-parent", NormalizedWorkspace: "/tmp/feedback-parent"})
	if err != nil {
		t.Fatal(err)
	}
	otherProject, err := store.CreateProject(ctx, CreateProjectParams{Name: "Other project", WorkspacePath: "/tmp/feedback-other", NormalizedWorkspace: "/tmp/feedback-other"})
	if err != nil {
		t.Fatal(err)
	}
	requirement, _, err := store.CreateIntake(ctx, CreateIntakeParams{ProjectID: project.ID, Kind: "requirement", Title: "Original requirement", Body: "Original body"})
	if err != nil {
		t.Fatal(err)
	}
	feedback, _, err := store.CreateIntake(ctx, CreateIntakeParams{ProjectID: project.ID, Kind: "feedback", ParentIntakeID: &requirement.ID, Title: "Linked feedback", Body: "Feedback body"})
	if err != nil {
		t.Fatal(err)
	}
	if feedback.ParentIntakeID == nil || *feedback.ParentIntakeID != requirement.ID {
		t.Fatalf("feedback parent=%v, want %s", feedback.ParentIntakeID, requirement.ID)
	}

	if _, _, err = store.CreateIntake(ctx, CreateIntakeParams{ProjectID: project.ID, Kind: "feedback", Title: "Missing parent", Body: "body"}); err == nil || !strings.Contains(err.Error(), "linked to a requirement") {
		t.Fatalf("missing parent error=%v", err)
	}
	if _, _, err = store.CreateIntake(ctx, CreateIntakeParams{ProjectID: project.ID, Kind: "requirement", ParentIntakeID: &requirement.ID, Title: "Nested requirement", Body: "body"}); err == nil || !strings.Contains(err.Error(), "cannot have a parent") {
		t.Fatalf("nested requirement error=%v", err)
	}
	if _, _, err = store.CreateIntake(ctx, CreateIntakeParams{ProjectID: project.ID, Kind: "feedback", ParentIntakeID: &feedback.ID, Title: "Nested feedback", Body: "body"}); err == nil || !strings.Contains(err.Error(), "linked directly") {
		t.Fatalf("nested feedback error=%v", err)
	}
	otherRequirement, _, err := store.CreateIntake(ctx, CreateIntakeParams{ProjectID: otherProject.ID, Kind: "requirement", Title: "Other requirement", Body: "body"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = store.CreateIntake(ctx, CreateIntakeParams{ProjectID: project.ID, Kind: "feedback", ParentIntakeID: &otherRequirement.ID, Title: "Cross project feedback", Body: "body"}); err == nil || !strings.Contains(err.Error(), "another project") {
		t.Fatalf("cross project parent error=%v", err)
	}
}

func TestListPlansForIntakeOnlyReturnsItsPlans(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	project, err := store.CreateProject(ctx, CreateProjectParams{Name: "Plan lookup", WorkspacePath: "/tmp/plan-lookup", NormalizedWorkspace: "/tmp/plan-lookup"})
	if err != nil {
		t.Fatal(err)
	}
	createPlan := func(title string) (domain.Intake, domain.Plan) {
		t.Helper()
		intake, job, createErr := store.CreateIntake(ctx, CreateIntakeParams{ProjectID: project.ID, Kind: "requirement", Title: title, Body: "body", QueuePlan: true})
		if createErr != nil || job == nil {
			t.Fatalf("create intake: job=%v err=%v", job, createErr)
		}
		if _, createErr = store.Pool.Exec(ctx, `UPDATE jobs SET status='cancelled' WHERE id=$1`, job.ID); createErr != nil {
			t.Fatal(createErr)
		}
		spec := planspec.Spec{Title: title + " plan", Summary: "summary", Tasks: []planspec.Task{{Title: "Implement", Scope: []string{"backend"}, Acceptance: []string{"passes"}}}, FinalValidation: []string{"tests pass"}}
		plan, _, createErr := store.SaveGeneratedPlan(ctx, intake, spec, planspec.Render(spec))
		if createErr != nil {
			t.Fatal(createErr)
		}
		return intake, plan
	}
	firstIntake, firstPlan := createPlan("First")
	_, secondPlan := createPlan("Second")

	plans, err := store.ListPlansForIntake(ctx, firstIntake.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || plans[0].ID != firstPlan.ID {
		t.Fatalf("plans=%+v, want only %s and not %s", plans, firstPlan.ID, secondPlan.ID)
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
	if _, err = store.Pool.Exec(ctx, `UPDATE plans SET config_snapshot=$2 WHERE id=$1`, plan.ID, json.RawMessage(`{"executionAgentProvider":"claude"}`)); err != nil {
		t.Fatal(err)
	}
	originalSettings, err := store.GetProjectSettings(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
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
	if provider, providerErr := providerFromPlanConfigSnapshot(started.ConfigSnapshot); providerErr != nil || provider != "" {
		t.Fatalf("automatic backfill retained provider %q: %v", provider, providerErr)
	}
	currentSettings, err := store.GetProjectSettings(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertProviderSettingsUnchanged(t, originalSettings, currentSettings)
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
	job, err := store.QueuePlan(WithExecutionProvider(ctx, "codex"), plan.ID, plan.Version)
	if err != nil {
		t.Fatal(err)
	}
	if job.MaxAttempts != 5 {
		t.Fatalf("max attempts=%d", job.MaxAttempts)
	}
	assertJobProvider(t, job, "codex", boolPointer(true))
	originalPayload := string(job.Payload)
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
	settings, err := store.GetProjectSettings(ctx, claimed.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	settings.AgentProvider = "claude"
	if _, err = store.UpdateProjectSettings(ctx, settings); err != nil {
		t.Fatal(err)
	}
	retrying, err := scanJob(store.Pool.QueryRow(ctx, `SELECT id,project_id,job_type,aggregate_type,aggregate_id,payload,priority,status,run_after,coalesce(worker_id,''),lease_expires_at,attempt,max_attempts,coalesce(last_error,''),idempotency_key,created_at,updated_at,version FROM jobs WHERE id=$1`, claimed.ID))
	if err != nil {
		t.Fatal(err)
	}
	if retrying.Status != "retry_wait" {
		t.Fatalf("retrying job status=%s", retrying.Status)
	}
	assertJobProvider(t, retrying, "codex", boolPointer(true))
	if string(retrying.Payload) != originalPayload {
		t.Fatalf("automatic retry changed payload: before=%s after=%s", originalPayload, retrying.Payload)
	}
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

func TestTaskProviderOverrideDoesNotReplacePlanProvider(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	_, plan, tasks := createReadyPlanFixture(t, store, "/tmp/provider-task-override")

	planJob, err := store.QueuePlan(WithExecutionProvider(ctx, "claude"), plan.ID, plan.Version)
	if err != nil {
		t.Fatal(err)
	}
	assertJobProvider(t, planJob, "claude", boolPointer(true))
	queued, err := store.GetTask(ctx, tasks[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	stopped, _, err := store.StopTask(ctx, queued.ID, queued.Version)
	if err != nil {
		t.Fatal(err)
	}
	overrideJob, err := store.QueueTask(WithExecutionProvider(ctx, "codex"), stopped.ID, stopped.Version)
	if err != nil {
		t.Fatal(err)
	}
	assertJobProvider(t, overrideJob, "codex", boolPointer(true))

	running, err := store.StartTask(ctx, stopped.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.FinishTask(ctx, running, "", true, "done"); err != nil {
		t.Fatal(err)
	}
	nextJob, err := scanJob(store.Pool.QueryRow(ctx, `SELECT id,project_id,job_type,aggregate_type,aggregate_id,payload,priority,status,run_after,coalesce(worker_id,''),lease_expires_at,attempt,max_attempts,coalesce(last_error,''),idempotency_key,created_at,updated_at,version FROM jobs WHERE aggregate_id=$1 AND status='queued'`, tasks[1].ID))
	if err != nil {
		t.Fatal(err)
	}
	assertJobProvider(t, nextJob, "claude", nil)
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

func TestClaimJobKeepsStartedPlanTasksContiguous(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	project, err := store.CreateProject(ctx, CreateProjectParams{Name: "Plan execution ownership", WorkspacePath: "/tmp/plan-execution-ownership", NormalizedWorkspace: "/tmp/plan-execution-ownership"})
	if err != nil {
		t.Fatal(err)
	}
	project, err = store.SetAutomation(ctx, project.ID, true, project.Version)
	if err != nil {
		t.Fatal(err)
	}

	createReadyPlan := func(title string) (domain.Plan, []domain.PlanTask) {
		t.Helper()
		intake, generationJob, createErr := store.CreateIntake(ctx, CreateIntakeParams{ProjectID: project.ID, Kind: "requirement", Title: title, Body: "Keep this plan contiguous", ConfigSnapshot: json.RawMessage(`{}`), QueuePlan: true})
		if createErr != nil {
			t.Fatal(createErr)
		}
		if generationJob == nil {
			t.Fatal("expected plan generation job")
		}
		if _, execErr := store.Pool.Exec(ctx, `UPDATE jobs SET status='cancelled' WHERE id=$1`, generationJob.ID); execErr != nil {
			t.Fatal(execErr)
		}
		plan, tasks, saveErr := store.SaveGeneratedPlan(ctx, intake, planspec.Spec{
			Title:   title,
			Summary: "Run all tasks before the next plan starts",
			Tasks: []planspec.Task{
				{Title: "First implementation", Scope: []string{"first"}, Acceptance: []string{"first complete"}},
				{Title: "Second implementation", Scope: []string{"second"}, Acceptance: []string{"second complete"}},
			},
			FinalValidation: []string{"validate"},
		}, title)
		if saveErr != nil {
			t.Fatal(saveErr)
		}
		return plan, tasks
	}
	completeClaimedTask := func(job domain.Job, workerID string) {
		t.Helper()
		if _, runErr := store.MarkJobRunning(ctx, job.ID, workerID); runErr != nil {
			t.Fatal(runErr)
		}
		task, runErr := store.StartTask(ctx, job.AggregateID)
		if runErr != nil {
			t.Fatal(runErr)
		}
		if runErr = store.FinishTask(ctx, task, "", true, "done"); runErr != nil {
			t.Fatal(runErr)
		}
		if runErr = store.CompleteJob(ctx, job.ID, workerID); runErr != nil {
			t.Fatal(runErr)
		}
	}

	planA, tasksA := createReadyPlan("Plan A")
	planB, tasksB := createReadyPlan("Plan B")
	firstA, err := store.QueuePlan(ctx, planA.ID, planA.Version)
	if err != nil {
		t.Fatal(err)
	}
	firstB, err := store.QueuePlan(ctx, planB.ID, planB.Version)
	if err != nil {
		t.Fatal(err)
	}

	claimed, err := store.ClaimJob(ctx, "plan-owner-worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.ID != firstA.ID {
		t.Fatalf("first claimed job=%s, want Plan A job %s", claimed.ID, firstA.ID)
	}
	completeClaimedTask(claimed, "plan-owner-worker")

	// Plan B's first job was queued earlier than Plan A's second job. The
	// started plan must nevertheless keep ownership until its final validation.
	claimed, err = store.ClaimJob(ctx, "plan-owner-worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.AggregateID != tasksA[1].ID {
		t.Fatalf("second claimed task=%s, want Plan A second task %s; Plan B first task is %s", claimed.AggregateID, tasksA[1].ID, tasksB[0].ID)
	}
	completeClaimedTask(claimed, "plan-owner-worker")

	claimed, err = store.ClaimJob(ctx, "plan-owner-worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.AggregateID != tasksA[2].ID {
		t.Fatalf("third claimed task=%s, want Plan A final validation %s", claimed.AggregateID, tasksA[2].ID)
	}
	completeClaimedTask(claimed, "plan-owner-worker")

	completedA, err := store.GetPlan(ctx, planA.ID)
	if err != nil {
		t.Fatal(err)
	}
	if completedA.Status != "completed" {
		t.Fatalf("Plan A status=%q, want completed", completedA.Status)
	}
	claimed, err = store.ClaimJob(ctx, "plan-owner-worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.ID != firstB.ID || claimed.AggregateID != tasksB[0].ID {
		t.Fatalf("claimed job after Plan A completed=%s task=%s, want Plan B job=%s task=%s", claimed.ID, claimed.AggregateID, firstB.ID, tasksB[0].ID)
	}
}

func TestFinishTaskDoesNotSkipEarlierUnfinishedTask(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	project, err := store.CreateProject(ctx, CreateProjectParams{Name: "Finish ordering", WorkspacePath: "/tmp/finish-ordering", NormalizedWorkspace: "/tmp/finish-ordering"})
	if err != nil {
		t.Fatal(err)
	}
	project, err = store.SetAutomation(ctx, project.ID, true, project.Version)
	if err != nil {
		t.Fatal(err)
	}
	intake, generationJob, err := store.CreateIntake(ctx, CreateIntakeParams{ProjectID: project.ID, Kind: "requirement", Title: "Ordering", Body: "Do not skip earlier work", ConfigSnapshot: json.RawMessage(`{}`), QueuePlan: true})
	if err != nil {
		t.Fatal(err)
	}
	if generationJob == nil {
		t.Fatal("expected planning job")
	}
	if _, err = store.Pool.Exec(ctx, `UPDATE jobs SET status='cancelled' WHERE id=$1`, generationJob.ID); err != nil {
		t.Fatal(err)
	}
	plan, tasks, err := store.SaveGeneratedPlan(ctx, intake, planspec.Spec{
		Title:   "Ordering plan",
		Summary: "Ensure execution remains sequential",
		Tasks: []planspec.Task{
			{Title: "First", Scope: []string{"first"}, Acceptance: []string{"first complete"}},
			{Title: "Second", Scope: []string{"second"}, Acceptance: []string{"second complete"}},
			{Title: "Third", Scope: []string{"third"}, Acceptance: []string{"third complete"}},
		},
		FinalValidation: []string{"validate"},
	}, "ordering")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Pool.Exec(ctx, `UPDATE plans SET status='running' WHERE id=$1`, plan.ID); err != nil {
		t.Fatal(err)
	}
	for _, index := range []int{0, 1, 2} {
		if _, err = store.Pool.Exec(ctx, `UPDATE plan_tasks SET status='running',started_at=now() WHERE id=$1`, tasks[index].ID); err != nil {
			t.Fatal(err)
		}
	}
	runningThird, err := store.GetTask(ctx, tasks[2].ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.FinishTask(ctx, runningThird, "", true, "done"); err != nil {
		t.Fatal(err)
	}
	finalTask, err := store.GetTask(ctx, tasks[3].ID)
	if err != nil {
		t.Fatal(err)
	}
	if finalTask.Status != "pending" {
		t.Fatalf("final validation was queued before earlier work finished: %+v", finalTask)
	}
	var queued int
	if err = store.Pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE aggregate_id=$1 AND job_type='task.execute' AND status='queued'`, finalTask.ID).Scan(&queued); err != nil {
		t.Fatal(err)
	}
	if queued != 0 {
		t.Fatalf("final validation queued jobs=%d", queued)
	}
}

func TestFinishTaskCompletesRecoveredRunningPlanWhenAllTasksSucceeded(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	project, err := store.CreateProject(ctx, CreateProjectParams{Name: "Recovered completion", WorkspacePath: "/tmp/recovered-completion", NormalizedWorkspace: "/tmp/recovered-completion"})
	if err != nil {
		t.Fatal(err)
	}
	project, err = store.SetAutomation(ctx, project.ID, true, project.Version)
	if err != nil {
		t.Fatal(err)
	}
	intake, generationJob, err := store.CreateIntake(ctx, CreateIntakeParams{ProjectID: project.ID, Kind: "requirement", Title: "Recovered", Body: "Recover a successful task", ConfigSnapshot: json.RawMessage(`{}`), QueuePlan: true})
	if err != nil {
		t.Fatal(err)
	}
	if generationJob == nil {
		t.Fatal("expected planning job")
	}
	if _, err = store.Pool.Exec(ctx, `UPDATE jobs SET status='cancelled' WHERE id=$1`, generationJob.ID); err != nil {
		t.Fatal(err)
	}
	plan, tasks, err := store.SaveGeneratedPlan(ctx, intake, planspec.Spec{
		Title:           "Recovered plan",
		Summary:         "Complete a recovered plan",
		Tasks:           []planspec.Task{{Title: "Implementation", Scope: []string{"backend"}, Acceptance: []string{"complete"}}},
		FinalValidation: []string{"validate"},
	}, "recovered")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Pool.Exec(ctx, `UPDATE plans SET status='running' WHERE id=$1`, plan.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Pool.Exec(ctx, `UPDATE plan_tasks SET status='running',started_at=now() WHERE id=$1`, tasks[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Pool.Exec(ctx, `UPDATE plan_tasks SET status='succeeded',started_at=now(),finished_at=now() WHERE id=$1`, tasks[1].ID); err != nil {
		t.Fatal(err)
	}
	running, err := store.GetTask(ctx, tasks[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.FinishTask(ctx, running, "", true, "completed after recovery"); err != nil {
		t.Fatal(err)
	}
	completedTask, err := store.GetTask(ctx, tasks[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if completedTask.Status != "succeeded" || completedTask.FinishedAt == nil {
		t.Fatalf("task did not complete: %+v", completedTask)
	}
	completedPlan, err := store.GetPlan(ctx, plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if completedPlan.Status != "completed" {
		t.Fatalf("plan status=%q, want completed", completedPlan.Status)
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

func TestRenewJobLeaseDoesNotRequireWorkspaceLease(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	project, err := store.CreateProject(ctx, CreateProjectParams{Name: "Read-only plan", WorkspacePath: "/tmp/read-only-plan", NormalizedWorkspace: "/tmp/read-only-plan"})
	if err != nil {
		t.Fatal(err)
	}
	jobID := uuid.New()
	if _, err = store.Pool.Exec(ctx, `INSERT INTO jobs(id,project_id,job_type,aggregate_type,aggregate_id,status,worker_id,lease_expires_at,idempotency_key) VALUES($1,$2,'plan.generate','intake',$1,'running','plan-worker',now()-interval '1 second',$3)`, jobID, project.ID, "read-only-plan"); err != nil {
		t.Fatal(err)
	}
	if err = store.RenewJobLease(ctx, jobID, "plan-worker", time.Minute); err != nil {
		t.Fatal(err)
	}
	var expiresAt time.Time
	if err = store.Pool.QueryRow(ctx, `SELECT lease_expires_at FROM jobs WHERE id=$1`, jobID).Scan(&expiresAt); err != nil {
		t.Fatal(err)
	}
	if !expiresAt.After(time.Now()) {
		t.Fatalf("job lease was not renewed: %s", expiresAt)
	}
	var workspaceLeases int
	if err = store.Pool.QueryRow(ctx, `SELECT count(*) FROM workspace_leases WHERE job_id=$1`, jobID).Scan(&workspaceLeases); err != nil {
		t.Fatal(err)
	}
	if workspaceLeases != 0 {
		t.Fatalf("read-only plan unexpectedly owns %d workspace leases", workspaceLeases)
	}
}

func TestDeferWorkspaceBusyJobDoesNotConsumeAttempt(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	_, plan, _ := createReadyPlanFixture(t, store, "/tmp/workspace-busy-defer")

	job, err := store.QueuePlan(ctx, plan.ID, plan.Version)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimJob(ctx, "waiting-worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.ID != job.ID || claimed.Attempt != 1 {
		t.Fatalf("claimed=%+v", claimed)
	}
	if _, err = store.MarkJobRunning(ctx, job.ID, "waiting-worker"); err != nil {
		t.Fatal(err)
	}
	before := time.Now()
	if err = store.DeferJobForWorkspace(ctx, job.ID, "waiting-worker", time.Second); err != nil {
		t.Fatal(err)
	}

	deferred, err := scanJob(store.Pool.QueryRow(ctx, `SELECT id,project_id,job_type,aggregate_type,aggregate_id,payload,priority,status,run_after,coalesce(worker_id,''),lease_expires_at,attempt,max_attempts,coalesce(last_error,''),idempotency_key,created_at,updated_at,version FROM jobs WHERE id=$1`, job.ID))
	if err != nil {
		t.Fatal(err)
	}
	if deferred.Status != "queued" || deferred.WorkerID != "" || deferred.LeaseExpiresAt != nil || deferred.Attempt != 0 {
		t.Fatalf("workspace-busy job was not returned cleanly to the queue: %+v", deferred)
	}
	if !deferred.RunAfter.After(before) {
		t.Fatalf("workspace-busy job was not delayed: run_after=%s before=%s", deferred.RunAfter, before)
	}
}

func TestAutomationStopResetsStaleQueuedTaskBeforeResume(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	project, plan, tasks := createReadyPlanFixture(t, store, "/tmp/automation-stale-queued")
	if len(tasks) != 2 {
		t.Fatalf("tasks=%d", len(tasks))
	}

	job, err := store.QueuePlan(ctx, plan.ID, plan.Version)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a job that failed before StartTask could run, such as a failed
	// workspace-lease acquisition. The task remains queued in the legacy state.
	if _, err = store.Pool.Exec(ctx, `UPDATE jobs SET status='failed',worker_id=NULL,lease_expires_at=NULL WHERE id=$1`, job.ID); err != nil {
		t.Fatal(err)
	}
	stale, err := store.GetTask(ctx, tasks[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if stale.Status != "queued" {
		t.Fatalf("stale task status=%q, want queued", stale.Status)
	}
	// Older releases could mark later tasks as succeeded and the whole plan as
	// completed even though an earlier task was left queued after its job failed.
	// Stopping automation must still repair that inconsistent completed state.
	if _, err = store.Pool.Exec(ctx, `UPDATE plans SET status='completed' WHERE id=$1`, plan.ID); err != nil {
		t.Fatal(err)
	}

	project, err = store.SetAutomation(ctx, project.ID, false, project.Version)
	if err != nil {
		t.Fatal(err)
	}
	reset, err := store.GetTask(ctx, tasks[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if reset.Status != "pending" {
		t.Fatalf("stale first task was not reset: %+v", reset)
	}

	project, err = store.SetAutomation(ctx, project.ID, true, project.Version)
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.GetTask(ctx, tasks[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.GetTask(ctx, tasks[1].ID)
	if err != nil {
		t.Fatal(err)
	}
	if first.Status != "queued" || second.Status != "pending" {
		t.Fatalf("automation resume skipped task order: first=%+v second=%+v", first, second)
	}
	var nextJobTaskID uuid.UUID
	if err = store.Pool.QueryRow(ctx, `SELECT aggregate_id FROM jobs WHERE project_id=$1 AND job_type='task.execute' AND status='queued' ORDER BY created_at DESC LIMIT 1`, project.ID).Scan(&nextJobTaskID); err != nil {
		t.Fatal(err)
	}
	if nextJobTaskID != first.ID {
		t.Fatalf("resumed job targets %s, want first unfinished task %s", nextJobTaskID, first.ID)
	}
}

func TestAgentOutputEventIsNotStoredOrNotified(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	projectID := uuid.New()
	if _, err := store.Pool.Exec(ctx, `INSERT INTO projects(id,name,workspace_path,workspace_path_normalized) VALUES($1,'Event filter',$2,$2)`, projectID, "/tmp/event-filter-"+projectID.String()); err != nil {
		t.Fatal(err)
	}

	listener, err := store.Pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Release()
	if _, err = listener.Exec(ctx, `LISTEN specrelay_events`); err != nil {
		t.Fatal(err)
	}

	id, err := store.AppendEvent(ctx, NewEvent{ProjectID: &projectID, Type: "agent.output", AggregateType: "agent_run", AggregateID: uuid.New(), ResourceVersion: 1, Payload: json.RawMessage(`{"line":"ignored"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if id != 0 {
		t.Fatalf("agent.output id=%d, want 0", id)
	}
	var count int
	if err = store.Pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE project_id=$1`, projectID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("stored agent.output events=%d", count)
	}

	waitCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if notification, waitErr := listener.Conn().WaitForNotification(waitCtx); !errors.Is(waitErr, context.DeadlineExceeded) {
		t.Fatalf("agent.output notification=%v err=%v", notification, waitErr)
	}

	visibleID, err := store.AppendEvent(ctx, NewEvent{ProjectID: &projectID, Type: "test.visible", AggregateType: "project", AggregateID: projectID, ResourceVersion: 1})
	if err != nil {
		t.Fatal(err)
	}
	notifyCtx, notifyCancel := context.WithTimeout(ctx, time.Second)
	defer notifyCancel()
	notification, err := listener.Conn().WaitForNotification(notifyCtx)
	if err != nil {
		t.Fatal(err)
	}
	if notification.Channel != "specrelay_events" || notification.Payload != fmt.Sprint(visibleID) {
		t.Fatalf("notification=%+v, want event id %d", notification, visibleID)
	}
}

func TestEventQueriesFilterAgentOutputAndPageWithoutGaps(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	projectID := uuid.New()
	otherProjectID := uuid.New()
	for _, project := range []struct {
		id   uuid.UUID
		name string
		path string
	}{
		{id: projectID, name: "Event page", path: "/tmp/event-page-" + projectID.String()},
		{id: otherProjectID, name: "Other event page", path: "/tmp/event-page-" + otherProjectID.String()},
	} {
		if _, err := store.Pool.Exec(ctx, `INSERT INTO projects(id,name,workspace_path,workspace_path_normalized) VALUES($1,$2,$3,$3)`, project.id, project.name, project.path); err != nil {
			t.Fatal(err)
		}
	}

	visibleIDs := make([]int64, 0, 13)
	historicalAgentIDs := make(map[int64]struct{}, 13)
	for i := 0; i < 13; i++ {
		id, err := store.AppendEvent(ctx, NewEvent{ProjectID: &projectID, Type: fmt.Sprintf("test.visible.%02d", i), AggregateType: "project", AggregateID: projectID, ResourceVersion: int64(i + 1)})
		if err != nil {
			t.Fatal(err)
		}
		visibleIDs = append(visibleIDs, id)

		var agentID int64
		if err = store.Pool.QueryRow(ctx, `INSERT INTO events(project_id,event_type,aggregate_type,aggregate_id,resource_version,payload) VALUES($1,'agent.output','agent_run',$2,1,'{}') RETURNING id`, projectID, uuid.New()).Scan(&agentID); err != nil {
			t.Fatal(err)
		}
		historicalAgentIDs[agentID] = struct{}{}
	}
	if _, err := store.AppendEvent(ctx, NewEvent{ProjectID: &otherProjectID, Type: "test.other-project", AggregateType: "project", AggregateID: otherProjectID, ResourceVersion: 1}); err != nil {
		t.Fatal(err)
	}

	defaultPage, err := store.ListEventPage(ctx, projectID, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(defaultPage.Items) != 10 || !defaultPage.HasMore || defaultPage.NextBefore == nil {
		t.Fatalf("default page len=%d hasMore=%v nextBefore=%v", len(defaultPage.Items), defaultPage.HasMore, defaultPage.NextBefore)
	}
	for i, event := range defaultPage.Items {
		wantID := visibleIDs[len(visibleIDs)-1-i]
		if event.ID != wantID {
			t.Fatalf("default page item %d id=%d, want %d", i, event.ID, wantID)
		}
	}
	if *defaultPage.NextBefore != defaultPage.Items[len(defaultPage.Items)-1].ID {
		t.Fatalf("nextBefore=%d, want oldest returned id %d", *defaultPage.NextBefore, defaultPage.Items[len(defaultPage.Items)-1].ID)
	}

	var pagedIDs []int64
	seen := map[int64]struct{}{}
	var before *int64
	for {
		page, pageErr := store.ListEventPage(ctx, projectID, before, 4)
		if pageErr != nil {
			t.Fatal(pageErr)
		}
		for i, event := range page.Items {
			if event.Type == "agent.output" {
				t.Fatalf("page returned filtered event id=%d", event.ID)
			}
			if event.ProjectID == nil || *event.ProjectID != projectID {
				t.Fatalf("page returned project %v, want %s", event.ProjectID, projectID)
			}
			if i > 0 && page.Items[i-1].ID <= event.ID {
				t.Fatalf("page is not descending: %d before %d", page.Items[i-1].ID, event.ID)
			}
			if before != nil && event.ID >= *before {
				t.Fatalf("before cursor is not exclusive: event=%d before=%d", event.ID, *before)
			}
			if _, duplicate := seen[event.ID]; duplicate {
				t.Fatalf("duplicate event id=%d", event.ID)
			}
			seen[event.ID] = struct{}{}
			pagedIDs = append(pagedIDs, event.ID)
		}
		if !page.HasMore {
			if page.NextBefore != nil {
				t.Fatalf("final page nextBefore=%d", *page.NextBefore)
			}
			break
		}
		if page.NextBefore == nil || len(page.Items) == 0 || *page.NextBefore != page.Items[len(page.Items)-1].ID {
			t.Fatalf("page hasMore=%v items=%d nextBefore=%v", page.HasMore, len(page.Items), page.NextBefore)
		}
		cursor := *page.NextBefore
		before = &cursor
	}
	if len(pagedIDs) != len(visibleIDs) {
		t.Fatalf("paged ids=%v, want %d visible events", pagedIDs, len(visibleIDs))
	}
	for i, id := range pagedIDs {
		wantID := visibleIDs[len(visibleIDs)-1-i]
		if id != wantID {
			t.Fatalf("paged id %d=%d, want %d", i, id, wantID)
		}
	}

	after := visibleIDs[3]
	incremental, err := store.ListEvents(ctx, &projectID, after, 100)
	if err != nil {
		t.Fatal(err)
	}
	wantIncremental := visibleIDs[4:]
	if len(incremental) != len(wantIncremental) {
		t.Fatalf("incremental len=%d, want %d: %+v", len(incremental), len(wantIncremental), incremental)
	}
	for i, event := range incremental {
		if event.ID != wantIncremental[i] {
			t.Fatalf("incremental item %d id=%d, want %d", i, event.ID, wantIncremental[i])
		}
		if event.Type == "agent.output" {
			t.Fatalf("incremental query returned filtered event id=%d", event.ID)
		}
		if _, historical := historicalAgentIDs[event.ID]; historical {
			t.Fatalf("incremental query returned historical agent output id=%d", event.ID)
		}
	}
}
