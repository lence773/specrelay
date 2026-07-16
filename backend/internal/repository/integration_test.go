package repository

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lyming99/specrelay/backend/internal/domain"
	"github.com/lyming99/specrelay/backend/internal/migrations"
	"github.com/lyming99/specrelay/backend/internal/planspec"
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
	_, err = pool.Exec(context.Background(), `TRUNCATE runtime_instances,access_tokens,agent_runs,events,workspace_leases,jobs,plan_tasks,plans,attachments,intakes,project_settings,projects RESTART IDENTITY CASCADE`)
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

func decodeLifecycleCheckpoint(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	checkpoint := map[string]any{}
	if err := json.Unmarshal(raw, &checkpoint); err != nil {
		t.Fatalf("decode lifecycle checkpoint: %v (%s)", err, raw)
	}
	return checkpoint
}

func requirePlanExecutionBlocked(t *testing.T, err error) *domain.PlanExecutionBlockedError {
	t.Helper()
	if !errors.Is(err, domain.ErrPlanExecutionBlocked) {
		t.Fatalf("expected plan execution blocker, got %v", err)
	}
	var blocked *domain.PlanExecutionBlockedError
	if !errors.As(err, &blocked) || blocked == nil || len(blocked.Blockers) == 0 {
		t.Fatalf("missing detailed plan execution blockers: %v", err)
	}
	return blocked
}

func assertLifecycleAudit(t *testing.T, store *Store, resource domain.LifecycleResource, resourceID uuid.UUID, fromStatus, toStatus string, source domain.LifecycleStatusSource, jobID, runID *uuid.UUID) domain.LifecycleTransition {
	t.Helper()
	transitions, err := store.ListLifecycleTransitions(context.Background(), resource, resourceID)
	if err != nil {
		t.Fatal(err)
	}
	var matched *domain.LifecycleTransition
	for index := range transitions {
		transition := &transitions[index]
		if transition.FromStatus == fromStatus && transition.ToStatus == toStatus {
			matched = transition
		}
	}
	if matched == nil {
		t.Fatalf("missing %s lifecycle transition %q -> %q: %+v", resource, fromStatus, toStatus, transitions)
	}
	if matched.ResourceType != resource || matched.ResourceID != resourceID || matched.ResourceVersion <= 0 || matched.StatusSource != source || matched.ReasonCode == "" || strings.TrimSpace(matched.Reason) == "" || matched.LastActivityAt.IsZero() || matched.OccurredAt.IsZero() {
		t.Fatalf("incomplete lifecycle audit: %+v", *matched)
	}
	checkpoint := decodeLifecycleCheckpoint(t, matched.ExecutionCheckpoint)
	if checkpoint["resourceId"] != resourceID.String() {
		t.Fatalf("resource checkpoint=%v, want resourceId=%s", checkpoint, resourceID)
	}
	if jobID != nil && checkpoint["jobId"] != jobID.String() {
		t.Fatalf("job checkpoint=%v, want jobId=%s", checkpoint, *jobID)
	}
	if runID != nil && checkpoint["agentRunId"] != runID.String() {
		t.Fatalf("run checkpoint=%v, want agentRunId=%s", checkpoint, *runID)
	}
	return *matched
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

func checkpointTestGit(t *testing.T, workspace string, args ...string) string {
	t.Helper()
	commandArgs := append([]string{"-C", workspace}, args...)
	output, err := exec.Command("git", commandArgs...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v (%s)", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

func checkpointTestWriteFile(t *testing.T, workspace, relative, content string) {
	t.Helper()
	absolute := filepath.Join(workspace, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(absolute), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(absolute, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func newRepositoryCheckpointWorkspace(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git is unavailable: %v", err)
	}
	workspace := t.TempDir()
	checkpointTestGit(t, workspace, "init", "-q")
	checkpointTestGit(t, workspace, "config", "user.email", "specrelay@example.test")
	checkpointTestGit(t, workspace, "config", "user.name", "SpecRelay Test")
	for relative, content := range map[string]string{
		"modified.txt":    "before\n",
		"deleted.txt":     "remove me\n",
		"rename-old.txt":  "rename content\n",
		"preexisting.txt": "committed\n",
	} {
		checkpointTestWriteFile(t, workspace, relative, content)
	}
	checkpointTestGit(t, workspace, "add", "--all")
	checkpointTestGit(t, workspace, "commit", "-q", "-m", "baseline")
	return workspace
}

func checkpointFilesByPath(files []domain.PlanExecutionSnapshotFile) map[string]domain.PlanExecutionSnapshotFile {
	indexed := make(map[string]domain.PlanExecutionSnapshotFile, len(files))
	for _, file := range files {
		indexed[file.Path] = file
	}
	return indexed
}

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

func TestFeedbackTraceabilityPersistsCheckpointDiffAndRevisionChain(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	project, err := store.CreateProject(ctx, CreateProjectParams{Name: "Feedback trace", WorkspacePath: "/tmp/feedback-trace", NormalizedWorkspace: "/tmp/feedback-trace"})
	if err != nil {
		t.Fatal(err)
	}
	otherProject, err := store.CreateProject(ctx, CreateProjectParams{Name: "Feedback trace other", WorkspacePath: "/tmp/feedback-trace-other", NormalizedWorkspace: "/tmp/feedback-trace-other"})
	if err != nil {
		t.Fatal(err)
	}
	createPlan := func(projectID uuid.UUID, title string) (domain.Intake, domain.Plan, domain.PlanTask) {
		t.Helper()
		intake, job, createErr := store.CreateIntake(ctx, CreateIntakeParams{ProjectID: projectID, Kind: "requirement", Title: title, Body: "body", QueuePlan: true})
		if createErr != nil || job == nil {
			t.Fatalf("create intake %q: job=%v err=%v", title, job, createErr)
		}
		if _, createErr = store.Pool.Exec(ctx, `UPDATE jobs SET status='cancelled' WHERE id=$1`, job.ID); createErr != nil {
			t.Fatal(createErr)
		}
		spec := planspec.Spec{Title: title + " plan", Summary: "summary", Tasks: []planspec.Task{{Title: "Implement", Scope: []string{"backend/internal/repository/store.go"}, Acceptance: []string{"passes"}}}, FinalValidation: []string{"tests pass"}}
		plan, tasks, createErr := store.SaveGeneratedPlan(ctx, intake, spec, planspec.Render(spec))
		if createErr != nil || len(tasks) == 0 {
			t.Fatalf("save plan %q: tasks=%d err=%v", title, len(tasks), createErr)
		}
		return intake, plan, tasks[0]
	}

	requirement, plan, task := createPlan(project.ID, "Original requirement")
	checkpoint, err := store.CapturePlanExecutionCheckpoint(ctx, PlanExecutionCheckpointParams{
		ProjectID:     project.ID,
		PlanID:        plan.ID,
		TaskID:        task.ID,
		ChangeSummary: json.RawMessage(`{"reason":"task completed","filesChanged":1}`),
		Files: []PlanExecutionCheckpointFile{{
			Path: "backend/internal/repository/store.go", Status: "modified", Additions: 4, Deletions: 2,
			Hunks: []PlanExecutionCheckpointHunk{{Header: "@@ -8,3 +10,5 @@", Patch: " context\n-old\n+new", OldStartLine: 8, OldLineCount: 3, NewStartLine: 10, NewLineCount: 5}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if checkpoint.Kind != domain.PlanSnapshotKindTaskCheckpoint || checkpoint.Additions != 4 || checkpoint.Deletions != 2 || len(checkpoint.Files) != 1 || len(checkpoint.Files[0].Hunks) != 1 {
		t.Fatalf("checkpoint evidence=%+v", checkpoint)
	}
	fileID := checkpoint.Files[0].ID
	hunkID := checkpoint.Files[0].Hunks[0].ID
	lineStart, lineEnd := 11, 13
	feedback, _, err := store.CreateIntake(ctx, CreateIntakeParams{
		ProjectID: project.ID, Kind: "feedback", ParentIntakeID: &requirement.ID, Title: "Review feedback", Body: "Please revisit these lines",
		Feedback: &FeedbackAssociationParams{PlanID: &plan.ID, TaskID: &task.ID, CheckpointID: &checkpoint.ID, FileID: &fileID, DiffHunkID: &hunkID, DiffLineSide: "new", DiffLineStart: &lineStart, DiffLineEnd: &lineEnd},
	})
	if err != nil {
		t.Fatal(err)
	}

	revisionIntake, revisionPlan, revisionTask := createPlan(project.ID, "Revised requirement")
	revision, err := store.RecordFeedbackRevision(ctx, RecordFeedbackRevisionParams{ProjectID: project.ID, FeedbackID: feedback.ID, RevisionIntakeID: revisionIntake.ID, RevisionPlanID: &revisionPlan.ID})
	if err != nil {
		t.Fatal(err)
	}
	if revision.RevisionPlan == nil || revision.RevisionPlan.ID != revisionPlan.ID || revision.RevisionPlan.Status != revisionPlan.Status {
		t.Fatalf("revision=%+v", revision)
	}

	trace, err := store.GetFeedbackTrace(ctx, project.ID, feedback.ID)
	if err != nil {
		t.Fatal(err)
	}
	if trace.Requirement.ID != requirement.ID || trace.Plan == nil || trace.Plan.ID != plan.ID || trace.Task == nil || trace.Task.ID != task.ID || trace.Checkpoint == nil || trace.Checkpoint.ID != checkpoint.ID || trace.File == nil || trace.File.ID != fileID || trace.DiffHunk == nil || trace.DiffHunk.ID != hunkID {
		t.Fatalf("feedback trace missing execution link: %+v", trace)
	}
	if len(trace.Revisions) != 1 || trace.Revisions[0].RevisionIntake.ID != revisionIntake.ID || trace.Revisions[0].RevisionPlan == nil || trace.Revisions[0].RevisionPlan.ID != revisionPlan.ID {
		t.Fatalf("feedback revisions=%+v", trace.Revisions)
	}
	for name, list := range map[string]func() ([]domain.FeedbackTrace, error){
		"requirement": func() ([]domain.FeedbackTrace, error) {
			return store.ListFeedbackForRequirement(ctx, project.ID, requirement.ID)
		},
		"plan": func() ([]domain.FeedbackTrace, error) { return store.ListFeedbackForPlan(ctx, project.ID, plan.ID) },
		"task": func() ([]domain.FeedbackTrace, error) { return store.ListFeedbackForTask(ctx, project.ID, task.ID) },
		"checkpoint": func() ([]domain.FeedbackTrace, error) {
			return store.ListFeedbackForCheckpoint(ctx, project.ID, checkpoint.ID)
		},
	} {
		items, listErr := list()
		if listErr != nil || len(items) != 1 || items[0].Feedback.ID != feedback.ID || len(items[0].Revisions) != 1 {
			t.Fatalf("reverse lookup %s: items=%+v err=%v", name, items, listErr)
		}
	}

	if _, err = store.Pool.Exec(ctx, `UPDATE plan_execution_snapshots SET additions=99 WHERE id=$1`, checkpoint.ID); err == nil {
		t.Fatal("expected checkpoint update to be rejected")
	}
	if _, err = store.Pool.Exec(ctx, `UPDATE plan_execution_snapshot_files SET additions=99 WHERE id=$1`, fileID); err == nil {
		t.Fatal("expected checkpoint file update to be rejected")
	}
	if _, err = store.Pool.Exec(ctx, `DELETE FROM feedback_revisions WHERE id=$1`, revision.ID); err == nil {
		t.Fatal("expected feedback revision deletion to be rejected")
	}

	var before int
	if err = store.Pool.QueryRow(ctx, `SELECT count(*) FROM intakes WHERE kind='feedback' AND project_id=$1`, project.ID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	invalidStart, invalidEnd := 99, 100
	if _, _, err = store.CreateIntake(ctx, CreateIntakeParams{
		ProjectID: project.ID, Kind: "feedback", ParentIntakeID: &requirement.ID, Title: "Invalid range", Body: "body",
		Feedback: &FeedbackAssociationParams{PlanID: &plan.ID, TaskID: &task.ID, CheckpointID: &checkpoint.ID, FileID: &fileID, DiffHunkID: &hunkID, DiffLineSide: "new", DiffLineStart: &invalidStart, DiffLineEnd: &invalidEnd},
	}); !errors.Is(err, domain.ErrInvalidDiffRange) {
		t.Fatalf("invalid range error=%v", err)
	}
	var after int
	if err = store.Pool.QueryRow(ctx, `SELECT count(*) FROM intakes WHERE kind='feedback' AND project_id=$1`, project.ID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("invalid feedback partially persisted: before=%d after=%d", before, after)
	}

	assertDatabaseRejectsLink := func(title string, linkSQL string, linkArgs ...any) {
		t.Helper()
		tx, txErr := store.Pool.Begin(ctx)
		if txErr != nil {
			t.Fatal(txErr)
		}
		defer tx.Rollback(ctx)
		dbFeedbackID := uuid.New()
		if _, txErr = tx.Exec(ctx, `INSERT INTO intakes(id,project_id,kind,parent_intake_id,title,body) VALUES($1,$2,'feedback',$3,$4,'body')`, dbFeedbackID, project.ID, requirement.ID, title); txErr != nil {
			t.Fatal(txErr)
		}
		args := []any{dbFeedbackID, project.ID, requirement.ID}
		args = append(args, linkArgs...)
		if _, txErr = tx.Exec(ctx, linkSQL, args...); txErr == nil {
			t.Fatalf("database accepted invalid feedback link %q", title)
		}
	}
	assertDatabaseRejectsLink("DB mismatched task", `INSERT INTO feedback_links(feedback_id,project_id,requirement_id,plan_id,task_id) VALUES($1,$2,$3,$4,$5)`, plan.ID, revisionTask.ID)
	assertDatabaseRejectsLink("DB invalid range", `INSERT INTO feedback_links(feedback_id,project_id,requirement_id,plan_id,task_id,checkpoint_id,file_id,diff_hunk_id,diff_line_side,diff_line_start,diff_line_end) VALUES($1,$2,$3,$4,$5,$6,$7,$8,'new',99,100)`, plan.ID, task.ID, checkpoint.ID, fileID, hunkID)

	missingPlanID := uuid.New()
	if _, _, err = store.CreateIntake(ctx, CreateIntakeParams{
		ProjectID: project.ID, Kind: "feedback", ParentIntakeID: &requirement.ID, Title: "Missing plan", Body: "body",
		Feedback: &FeedbackAssociationParams{PlanID: &missingPlanID},
	}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing association error=%v", err)
	}
	if err = store.Pool.QueryRow(ctx, `SELECT count(*) FROM intakes WHERE kind='feedback' AND project_id=$1`, project.ID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("missing association partially persisted: before=%d after=%d", before, after)
	}

	_, otherPlan, _ := createPlan(otherProject.ID, "Other project requirement")
	if _, _, err = store.CreateIntake(ctx, CreateIntakeParams{
		ProjectID: project.ID, Kind: "feedback", ParentIntakeID: &requirement.ID, Title: "Cross project", Body: "body",
		Feedback: &FeedbackAssociationParams{PlanID: &otherPlan.ID},
	}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("cross-project association error=%v", err)
	}
	if _, err = store.GetFeedbackTrace(ctx, otherProject.ID, feedback.ID); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("cross-project feedback read error=%v", err)
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

func TestReconcileInstanceShutdownKeepsPlansRecoverableAndTasksBlocked(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	const owner = "desktop-instance-a"

	project, plan, tasks := createReadyPlanFixture(t, store, "/tmp/shutdown-reconcile")
	taskJob, err := store.QueueTask(ctx, tasks[0].ID, tasks[0].Version)
	if err != nil {
		t.Fatal(err)
	}
	runningTask, err := store.StartTask(ctx, tasks[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Pool.Exec(ctx, `UPDATE jobs
		SET status='running',worker_id=$2,lease_expires_at=now()+interval '10 minutes',attempt=1
		WHERE id=$1`, taskJob.ID, owner+":worker-1"); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Pool.Exec(ctx, `INSERT INTO workspace_leases(id,project_id,workspace_path_normalized,worker_id,job_id,expires_at)
		VALUES($1,$2,$3,$4,$5,now()+interval '10 minutes')`, uuid.New(), project.ID, "/tmp/shutdown-reconcile", owner+":worker-1", taskJob.ID); err != nil {
		t.Fatal(err)
	}
	taskRunID := uuid.New()
	if err = store.StartAgentRun(ctx, AgentRunStart{ID: taskRunID, ProjectID: project.ID, JobID: &taskJob.ID, TaskID: &runningTask.ID, Provider: "codex", CommandSummary: "codex task", LogPath: "/tmp/task.log", OwnerInstanceID: owner}); err != nil {
		t.Fatal(err)
	}
	discussionRunID := uuid.New()
	if err = store.StartAgentRun(ctx, AgentRunStart{ID: discussionRunID, ProjectID: project.ID, Provider: "codex", CommandSummary: "codex discussion", LogPath: "/tmp/discussion.log", OwnerInstanceID: owner}); err != nil {
		t.Fatal(err)
	}
	otherRunID := uuid.New()
	if err = store.StartAgentRun(ctx, AgentRunStart{ID: otherRunID, ProjectID: project.ID, Provider: "codex", CommandSummary: "other desktop", LogPath: "/tmp/other.log", OwnerInstanceID: "desktop-instance-b"}); err != nil {
		t.Fatal(err)
	}

	intake, planJob, err := store.CreateIntake(ctx, CreateIntakeParams{ProjectID: project.ID, Kind: "requirement", Title: "Planning", Body: "Should resume", ConfigSnapshot: json.RawMessage(`{}`), QueuePlan: true})
	if err != nil || planJob == nil || intake.Status != "planning" {
		t.Fatalf("create planning intake: intake=%+v job=%+v err=%v", intake, planJob, err)
	}
	if _, err = store.Pool.Exec(ctx, `UPDATE jobs
		SET status='running',worker_id=$2,lease_expires_at=now()+interval '10 minutes',attempt=1
		WHERE id=$1`, planJob.ID, owner+":worker-2"); err != nil {
		t.Fatal(err)
	}
	planRunID := uuid.New()
	if err = store.StartAgentRun(ctx, AgentRunStart{ID: planRunID, ProjectID: project.ID, JobID: &planJob.ID, Provider: "codex", CommandSummary: "codex plan", LogPath: "/tmp/plan.log", OwnerInstanceID: owner}); err != nil {
		t.Fatal(err)
	}

	if err = store.ReconcileInstanceShutdown(ctx, owner); err != nil {
		t.Fatal(err)
	}

	var taskJobStatus string
	if err = store.Pool.QueryRow(ctx, `SELECT status FROM jobs WHERE id=$1`, taskJob.ID).Scan(&taskJobStatus); err != nil {
		t.Fatal(err)
	}
	if taskJobStatus != "cancelled" {
		t.Fatalf("task job status=%q, want cancelled", taskJobStatus)
	}
	stoppedTask, err := store.GetTask(ctx, runningTask.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stoppedTask.Status != "pending" || stoppedTask.StartedAt != nil || stoppedTask.FinishedAt != nil {
		t.Fatalf("task was not reset safely: %+v", stoppedTask)
	}
	stoppedPlan, err := store.GetPlan(ctx, plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stoppedPlan.Status != "blocked" {
		t.Fatalf("plan status=%q, want blocked", stoppedPlan.Status)
	}
	var leases int
	if err = store.Pool.QueryRow(ctx, `SELECT count(*) FROM workspace_leases WHERE job_id=$1`, taskJob.ID).Scan(&leases); err != nil {
		t.Fatal(err)
	}
	if leases != 0 {
		t.Fatalf("workspace lease retained after shutdown: %d", leases)
	}
	for _, runID := range []uuid.UUID{taskRunID, discussionRunID, planRunID} {
		var status string
		if err = store.Pool.QueryRow(ctx, `SELECT status FROM agent_runs WHERE id=$1`, runID).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status != "cancelled" {
			t.Fatalf("owned agent run %s status=%q, want cancelled", runID, status)
		}
	}
	var otherStatus string
	if err = store.Pool.QueryRow(ctx, `SELECT status FROM agent_runs WHERE id=$1`, otherRunID).Scan(&otherStatus); err != nil {
		t.Fatal(err)
	}
	if otherStatus != "running" {
		t.Fatalf("different instance run was changed to %q", otherStatus)
	}
	var planJobStatus string
	var planJobAttempt int
	var planJobWorker *string
	if err = store.Pool.QueryRow(ctx, `SELECT status,attempt,worker_id FROM jobs WHERE id=$1`, planJob.ID).Scan(&planJobStatus, &planJobAttempt, &planJobWorker); err != nil {
		t.Fatal(err)
	}
	if planJobStatus != "queued" || planJobAttempt != 0 || planJobWorker != nil {
		t.Fatalf("planning job was not safely requeued: status=%q attempt=%d worker=%v", planJobStatus, planJobAttempt, planJobWorker)
	}
}

func TestRecoverJobsUsesRuntimeHeartbeatWithoutInterruptingLiveInstances(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	const deadOwner = "dead-desktop"
	const liveOwner = "live-desktop"
	const slowOwner = "slow-heartbeat-desktop"
	if err := store.RegisterRuntimeInstance(ctx, deadOwner, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := store.RegisterRuntimeInstance(ctx, liveOwner, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	// A live instance may intentionally use a longer heartbeat. Recovery must
	// wait for its declared cadence instead of applying a global 30-second cut-off.
	if err := store.RegisterRuntimeInstance(ctx, slowOwner, 20*time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pool.Exec(ctx, `UPDATE runtime_instances SET heartbeat_at=now()-interval '31 seconds' WHERE instance_id=$1`, deadOwner); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pool.Exec(ctx, `UPDATE runtime_instances SET heartbeat_at=now()-interval '31 seconds' WHERE instance_id=$1`, slowOwner); err != nil {
		t.Fatal(err)
	}

	project, plan, tasks := createReadyPlanFixture(t, store, "/tmp/runtime-recovery")
	deadJob, err := store.QueueTask(ctx, tasks[0].ID, tasks[0].Version)
	if err != nil {
		t.Fatal(err)
	}
	deadTask, err := store.StartTask(ctx, tasks[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Pool.Exec(ctx, `UPDATE jobs SET status='running',worker_id=$2,lease_expires_at=now()+interval '10 minutes' WHERE id=$1`, deadJob.ID, deadOwner+":worker-1"); err != nil {
		t.Fatal(err)
	}
	deadRunID := uuid.New()
	if err = store.StartAgentRun(ctx, AgentRunStart{ID: deadRunID, ProjectID: project.ID, JobID: &deadJob.ID, TaskID: &deadTask.ID, Provider: "codex", CommandSummary: "dead task", LogPath: "/tmp/dead-task.log", OwnerInstanceID: deadOwner}); err != nil {
		t.Fatal(err)
	}
	deadDiscussionID := uuid.New()
	if err = store.StartAgentRun(ctx, AgentRunStart{ID: deadDiscussionID, ProjectID: project.ID, Provider: "codex", CommandSummary: "dead discussion", LogPath: "/tmp/dead-discussion.log", OwnerInstanceID: deadOwner}); err != nil {
		t.Fatal(err)
	}
	liveDiscussionID := uuid.New()
	if err = store.StartAgentRun(ctx, AgentRunStart{ID: liveDiscussionID, ProjectID: project.ID, Provider: "codex", CommandSummary: "live discussion", LogPath: "/tmp/live-discussion.log", OwnerInstanceID: liveOwner}); err != nil {
		t.Fatal(err)
	}
	slowIntake, slowJob, err := store.CreateIntake(ctx, CreateIntakeParams{ProjectID: project.ID, Kind: "requirement", Title: "Slow heartbeat planning", Body: "must remain live", ConfigSnapshot: json.RawMessage(`{}`), QueuePlan: true})
	if err != nil || slowJob == nil || slowIntake.Status != "planning" {
		t.Fatalf("create slow heartbeat plan job: intake=%+v job=%+v err=%v", slowIntake, slowJob, err)
	}
	if _, err = store.Pool.Exec(ctx, `UPDATE jobs SET status='running',worker_id=$2,lease_expires_at=now()+interval '10 minutes' WHERE id=$1`, slowJob.ID, slowOwner+":worker-1"); err != nil {
		t.Fatal(err)
	}
	slowRunID := uuid.New()
	if err = store.StartAgentRun(ctx, AgentRunStart{ID: slowRunID, ProjectID: project.ID, JobID: &slowJob.ID, Provider: "codex", CommandSummary: "slow live plan", LogPath: "/tmp/slow-plan.log", OwnerInstanceID: slowOwner}); err != nil {
		t.Fatal(err)
	}

	if err = store.RecoverJobs(ctx); err != nil {
		t.Fatal(err)
	}
	var deadJobStatus string
	if err = store.Pool.QueryRow(ctx, `SELECT status FROM jobs WHERE id=$1`, deadJob.ID).Scan(&deadJobStatus); err != nil {
		t.Fatal(err)
	}
	if deadJobStatus != "cancelled" {
		t.Fatalf("dead job status=%q, want cancelled", deadJobStatus)
	}
	recoveredTask, err := store.GetTask(ctx, deadTask.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recoveredTask.Status != "pending" {
		t.Fatalf("dead task status=%q, want pending", recoveredTask.Status)
	}
	recoveredPlan, err := store.GetPlan(ctx, plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recoveredPlan.Status != "blocked" {
		t.Fatalf("recovered plan status=%q, want blocked", recoveredPlan.Status)
	}
	for _, runID := range []uuid.UUID{deadRunID, deadDiscussionID} {
		var status string
		if err = store.Pool.QueryRow(ctx, `SELECT status FROM agent_runs WHERE id=$1`, runID).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status != "cancelled" {
			t.Fatalf("dead owner run %s status=%q, want cancelled", runID, status)
		}
	}
	var liveStatus string
	if err = store.Pool.QueryRow(ctx, `SELECT status FROM agent_runs WHERE id=$1`, liveDiscussionID).Scan(&liveStatus); err != nil {
		t.Fatal(err)
	}
	if liveStatus != "running" {
		t.Fatalf("live owner run status=%q, want running", liveStatus)
	}
	var slowJobStatus, slowRunStatus string
	if err = store.Pool.QueryRow(ctx, `SELECT status FROM jobs WHERE id=$1`, slowJob.ID).Scan(&slowJobStatus); err != nil {
		t.Fatal(err)
	}
	if err = store.Pool.QueryRow(ctx, `SELECT status FROM agent_runs WHERE id=$1`, slowRunID).Scan(&slowRunStatus); err != nil {
		t.Fatal(err)
	}
	if slowJobStatus != "running" || slowRunStatus != "running" {
		t.Fatalf("slow live instance was interrupted: job=%q run=%q", slowJobStatus, slowRunStatus)
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

func createModernSchedulingPlanFixture(t *testing.T, store *Store, workspace string, automated bool) (domain.Project, domain.Plan, []domain.PlanTask) {
	t.Helper()
	ctx := context.Background()
	project, err := store.CreateProject(ctx, CreateProjectParams{Name: "Strict scheduling", WorkspacePath: workspace, NormalizedWorkspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	if automated {
		project, err = store.SetAutomation(ctx, project.ID, true, project.Version)
		if err != nil {
			t.Fatal(err)
		}
	}
	intake, generationJob, err := store.CreateIntake(ctx, CreateIntakeParams{
		ProjectID: project.ID, Kind: "requirement", Title: "Strict scheduling", Body: "Exercise dependency gates",
		ConfigSnapshot: json.RawMessage(`{}`), QueuePlan: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if generationJob == nil {
		t.Fatal("expected plan generation job")
	}
	if _, err = store.Pool.Exec(ctx, `UPDATE jobs SET status='cancelled' WHERE id=$1`, generationJob.ID); err != nil {
		t.Fatal(err)
	}
	empty := []string{}
	spec := planspec.Spec{
		Version: planspec.CurrentVersion, Title: "Strict scheduling", Summary: "Run one dependency-ready task at a time",
		Tasks: []planspec.Task{
			{Key: "A", Title: "Dependent A", DependsOn: []string{"B"}, Scope: []string{"a"}, Inputs: empty, Outputs: empty, Risks: empty, ValidationCommands: empty, AcceptanceItems: []planspec.AcceptanceItem{{Key: "A-ACCEPT", Description: "A passes"}}},
			{Key: "B", Title: "Root B", DependsOn: empty, Scope: []string{"b"}, Inputs: empty, Outputs: empty, Risks: empty, ValidationCommands: empty, AcceptanceItems: []planspec.AcceptanceItem{{Key: "B-ACCEPT", Description: "B passes"}}},
			{Key: "C", Title: "Independent C", DependsOn: empty, Scope: []string{"c"}, Inputs: empty, Outputs: empty, Risks: empty, ValidationCommands: empty, AcceptanceItems: []planspec.AcceptanceItem{{Key: "C-ACCEPT", Description: "C passes"}}},
		},
		FinalValidationDefinition: planspec.ValidationDefinition{
			Acceptance: []planspec.AcceptanceItem{{Key: "FINAL-ACCEPT", Description: "All tests pass"}},
			Commands:   []string{"go test ./..."},
		},
	}
	plan, tasks, err := store.SaveGeneratedPlan(ctx, intake, spec, planspec.Render(spec))
	if err != nil {
		t.Fatal(err)
	}
	return project, plan, tasks
}

func TestUnifiedLifecycleAuditAcrossUserAutomationAndWorkerPaths(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	project, plan, tasks := createReadyPlanFixture(t, store, "/tmp/unified-lifecycle-audit")

	job, err := store.QueuePlan(ctx, plan.ID, plan.Version)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimJob(ctx, "audit-worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.ID != job.ID {
		t.Fatalf("claimed job=%s, want %s", claimed.ID, job.ID)
	}
	if _, err = store.MarkJobRunning(ctx, job.ID, "audit-worker"); err != nil {
		t.Fatal(err)
	}
	runningTask, err := store.StartTask(ctx, tasks[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	runID := uuid.New()
	if err = store.StartAgentRun(ctx, AgentRunStart{
		ID: runID, ProjectID: project.ID, PlanID: &plan.ID, JobID: &job.ID, TaskID: &runningTask.ID,
		Provider: "codex", CommandSummary: "codex task", LogPath: "/tmp/unified-lifecycle-audit.log",
	}); err != nil {
		t.Fatal(err)
	}
	if err = store.FinishAgentRun(ctx, runID, "succeeded", 0, "audit-session", "", time.Second); err != nil {
		t.Fatal(err)
	}
	if err = store.FinishTask(ctx, runningTask, "audit-session", true, "task completed"); err != nil {
		t.Fatal(err)
	}
	if err = store.CompleteJob(ctx, job.ID, "audit-worker"); err != nil {
		t.Fatal(err)
	}

	planAudit := assertLifecycleAudit(t, store, domain.LifecycleResourcePlan, plan.ID, "ready", "running", domain.LifecycleSourceUser, &job.ID, nil)
	queuedAudit := assertLifecycleAudit(t, store, domain.LifecycleResourceTask, tasks[0].ID, "pending", "queued", domain.LifecycleSourceUser, &job.ID, nil)
	completedTaskAudit := assertLifecycleAudit(t, store, domain.LifecycleResourceTask, tasks[0].ID, "running", "succeeded", domain.LifecycleSourceWorker, &job.ID, nil)
	jobAudit := assertLifecycleAudit(t, store, domain.LifecycleResourceJob, job.ID, "running", "succeeded", domain.LifecycleSourceWorker, &job.ID, nil)
	runAudit := assertLifecycleAudit(t, store, domain.LifecycleResourceAgentRun, runID, "running", "succeeded", domain.LifecycleSourceWorker, &job.ID, &runID)
	nextTaskAudit := assertLifecycleAudit(t, store, domain.LifecycleResourceTask, tasks[1].ID, "pending", "queued", domain.LifecycleSourceAutomation, nil, nil)

	for _, item := range []struct {
		resource domain.LifecycleResource
		id       uuid.UUID
		audit    domain.LifecycleTransition
	}{
		{domain.LifecycleResourcePlan, plan.ID, planAudit},
		{domain.LifecycleResourceTask, tasks[0].ID, completedTaskAudit},
		{domain.LifecycleResourceJob, job.ID, jobAudit},
		{domain.LifecycleResourceAgentRun, runID, runAudit},
	} {
		state, stateErr := store.GetLifecycleState(ctx, item.resource, item.id)
		if stateErr != nil {
			t.Fatal(stateErr)
		}
		if item.audit.ResourceVersion != state.Version || item.audit.ToStatus != state.Status || item.audit.StatusSource != state.StatusSource || item.audit.ReasonCode != state.ReasonCode || item.audit.Reason != state.Reason || !item.audit.LastActivityAt.Equal(state.LastActivityAt) || string(item.audit.ExecutionCheckpoint) != string(state.ExecutionCheckpoint) {
			t.Fatalf("audit and projection diverged for %s %s: audit=%+v state=%+v", item.resource, item.id, item.audit, state)
		}
	}
	if queuedAudit.ResourceVersion >= completedTaskAudit.ResourceVersion {
		t.Fatalf("task lifecycle versions are not ordered: queued=%d completed=%d", queuedAudit.ResourceVersion, completedTaskAudit.ResourceVersion)
	}
	if nextTaskAudit.StatusSource != domain.LifecycleSourceAutomation {
		t.Fatalf("next task source=%q", nextTaskAudit.StatusSource)
	}
}

func TestLifecycleGateReturnsConsistentResultsForRESTMCPAutomationAndWorkerSources(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	activity := time.Now().UTC().Truncate(time.Microsecond)
	type entryPoint struct {
		name   string
		source domain.LifecycleStatusSource
	}
	entryPoints := []entryPoint{
		{name: "rest", source: domain.LifecycleSourceUser},
		{name: "mcp", source: domain.LifecycleSourceUser},
		{name: "automation", source: domain.LifecycleSourceAutomation},
		{name: "worker", source: domain.LifecycleSourceWorker},
	}
	for _, entry := range entryPoints {
		t.Run(entry.name, func(t *testing.T) {
			_, _, tasks := createReadyPlanFixture(t, store, "/tmp/lifecycle-entry-"+entry.name)
			jobID := uuid.New()
			result, err := store.TransitionLifecycle(ctx, LifecycleTransitionParams{
				ResourceType:    domain.LifecycleResourceTask,
				ResourceID:      tasks[0].ID,
				ExpectedStatus:  "pending",
				ExpectedVersion: tasks[0].Version,
				Status:          "queued",
				StatusSource:    entry.source,
				ReasonCode:      domain.LifecycleReasonResumeRequested,
				Reason:          "same task queue action",
				LastActivityAt:  activity,
				RecoveryHint:    domain.LifecycleRecoveryNone,
				RelatedJobID:    &jobID,
			})
			if err != nil {
				t.Fatal(err)
			}
			if result.Idempotent || result.Transition == nil || result.State.Status != "queued" || result.State.Version != tasks[0].Version+1 || result.Transition.FromStatus != "pending" || result.Transition.ToStatus != "queued" || result.Transition.ResourceVersion != result.State.Version || result.Transition.StatusSource != entry.source || !result.Transition.LastActivityAt.Equal(activity) {
				t.Fatalf("inconsistent %s result: %+v", entry.name, result)
			}
			checkpoint := decodeLifecycleCheckpoint(t, result.Transition.ExecutionCheckpoint)
			if checkpoint["resourceId"] != tasks[0].ID.String() || checkpoint["jobId"] != jobID.String() {
				t.Fatalf("inconsistent %s checkpoint: %v", entry.name, checkpoint)
			}

			_, err = store.TransitionLifecycle(ctx, LifecycleTransitionParams{
				ResourceType:    domain.LifecycleResourceTask,
				ResourceID:      tasks[0].ID,
				ExpectedStatus:  "queued",
				ExpectedVersion: result.State.Version,
				Status:          "completed",
				StatusSource:    entry.source,
				ReasonCode:      domain.LifecycleReasonCompleted,
				Reason:          "same invalid completion action",
				LastActivityAt:  activity,
				RecoveryHint:    domain.LifecycleRecoveryNone,
				RelatedJobID:    &jobID,
			})
			if !errors.Is(err, domain.ErrInvalidTransition) {
				t.Fatalf("%s invalid action error=%v, want ErrInvalidTransition", entry.name, err)
			}
			after, err := store.GetLifecycleState(ctx, domain.LifecycleResourceTask, tasks[0].ID)
			if err != nil {
				t.Fatal(err)
			}
			transitions, err := store.ListLifecycleTransitions(ctx, domain.LifecycleResourceTask, tasks[0].ID)
			if err != nil {
				t.Fatal(err)
			}
			if after.Status != result.State.Status || after.Version != result.State.Version || len(transitions) != 1 {
				t.Fatalf("%s invalid action changed state/events: before=%+v after=%+v transitions=%+v", entry.name, result.State, after, transitions)
			}
		})
	}
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

func TestStrictSchedulerUsesTopologicalOrderAndOriginalPosition(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	_, plan, tasks := createModernSchedulingPlanFixture(t, store, "/tmp/strict-topological-order", true)
	if plan.CompatibilityMode || !plan.Executable || len(tasks) != 4 {
		t.Fatalf("plan=%+v tasks=%d", plan, len(tasks))
	}
	byKey := make(map[string]domain.PlanTask, len(tasks))
	for _, task := range tasks {
		byKey[task.TaskKey] = task
	}
	for key, want := range map[string]struct{ position, order int }{
		"B":     {position: 2, order: 1},
		"A":     {position: 1, order: 2},
		"C":     {position: 3, order: 3},
		"FINAL": {position: 4, order: 4},
	} {
		task, ok := byKey[key]
		if !ok || task.Position != want.position || task.ExecutionOrder != want.order {
			t.Fatalf("task %s=%+v, want position=%d executionOrder=%d", key, task, want.position, want.order)
		}
	}

	blocked := requirePlanExecutionBlocked(t, func() error {
		_, err := store.QueueTask(ctx, byKey["A"].ID, byKey["A"].Version)
		return err
	}())
	if blocker := blocked.Blockers[0]; blocker.Code != "earlier_task" || blocker.TaskKey != "B" {
		t.Fatalf("manual out-of-order blockers=%+v", blocked.Blockers)
	}

	job, err := store.QueuePlan(ctx, plan.ID, plan.Version)
	if err != nil {
		t.Fatal(err)
	}
	if job.AggregateID != byKey["B"].ID {
		t.Fatalf("queued task=%s, want B=%s", job.AggregateID, byKey["B"].ID)
	}
	var active int
	if err = store.Pool.QueryRow(ctx, `SELECT count(*) FROM plan_tasks WHERE plan_id=$1 AND status IN ('queued','running')`, plan.ID).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != 1 {
		t.Fatalf("active tasks=%d, want 1", active)
	}
	claimed, err := store.ClaimJob(ctx, "topology-worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err = store.MarkJobRunning(ctx, claimed.ID, "topology-worker")
	if err != nil {
		t.Fatal(err)
	}
	running, err := store.StartTask(ctx, byKey["B"].ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.FinishTask(ctx, running, "topology-session", true, "B passed"); err != nil {
		t.Fatal(err)
	}
	if err = store.CompleteJob(ctx, claimed.ID, "topology-worker"); err != nil {
		t.Fatal(err)
	}
	afterA, err := store.GetTask(ctx, byKey["A"].ID)
	if err != nil {
		t.Fatal(err)
	}
	afterC, err := store.GetTask(ctx, byKey["C"].ID)
	if err != nil {
		t.Fatal(err)
	}
	if afterA.Status != "queued" || afterC.Status != "pending" {
		t.Fatalf("after B: A=%s C=%s", afterA.Status, afterC.Status)
	}
}

func TestStartTaskRechecksRequiredAcceptanceItems(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	_, plan, tasks := createReadyPlanFixture(t, store, "/tmp/strict-acceptance-recheck")
	job, err := store.QueuePlan(ctx, plan.ID, plan.Version)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimJob(ctx, "acceptance-worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err = store.MarkJobRunning(ctx, claimed.ID, "acceptance-worker")
	if err != nil {
		t.Fatal(err)
	}
	running, err := store.StartTask(ctx, tasks[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.FinishTask(ctx, running, "acceptance-session", true, "implementation passed"); err != nil {
		t.Fatal(err)
	}
	if err = store.CompleteJob(ctx, job.ID, "acceptance-worker"); err != nil {
		t.Fatal(err)
	}

	var criteria []struct {
		Key string `json:"key"`
	}
	if err = json.Unmarshal(tasks[0].AcceptanceDefinition, &criteria); err != nil || len(criteria) != 1 {
		t.Fatalf("acceptance definition=%s err=%v", tasks[0].AcceptanceDefinition, err)
	}
	manualResult := mustJSON(map[string]any{
		"status": domain.AcceptanceStatusPassed,
		"items": []map[string]any{{
			"key": criteria[0].Key, "status": domain.AcceptanceStatusManualConfirmation,
			"reason": "security sign-off required", "evidence": []any{},
		}},
		"reason": "manual review required", "evidence": []any{},
	})
	if _, err = store.Pool.Exec(ctx, `UPDATE plan_tasks SET acceptance_status='passed',acceptance_result=$2 WHERE id=$1`, tasks[0].ID, manualResult); err != nil {
		t.Fatal(err)
	}

	nextJob, err := store.ClaimJob(ctx, "acceptance-worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	nextJob, err = store.MarkJobRunning(ctx, nextJob.ID, "acceptance-worker")
	if err != nil {
		t.Fatal(err)
	}
	if nextJob.AggregateID != tasks[1].ID {
		t.Fatalf("next job task=%s, want %s", nextJob.AggregateID, tasks[1].ID)
	}
	_, startErr := store.StartTask(ctx, tasks[1].ID)
	blocked := requirePlanExecutionBlocked(t, startErr)
	var foundItem bool
	for _, blocker := range blocked.Blockers {
		if blocker.TaskKey != tasks[0].TaskKey {
			continue
		}
		for _, item := range blocker.AcceptanceBlockers {
			if item.Key == criteria[0].Key && item.Status == domain.AcceptanceStatusManualConfirmation && strings.Contains(item.Reason, "security sign-off") {
				foundItem = true
			}
		}
	}
	if !foundItem || !strings.Contains(startErr.Error(), "manual review required") {
		t.Fatalf("acceptance blockers=%+v err=%v", blocked.Blockers, startErr)
	}
	currentPlan, err := store.GetPlan(ctx, plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	currentNext, err := store.GetTask(ctx, tasks[1].ID)
	if err != nil {
		t.Fatal(err)
	}
	if currentPlan.Status != "blocked" || currentNext.Status != "pending" {
		t.Fatalf("plan=%s next=%s", currentPlan.Status, currentNext.Status)
	}
	var active int
	if err = store.Pool.QueryRow(ctx, `SELECT count(*) FROM plan_tasks WHERE plan_id=$1 AND status IN ('queued','running')`, plan.ID).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != 0 {
		t.Fatalf("active tasks after gate failure=%d", active)
	}
}

func TestFailedTaskBlocksSuccessorAndCanBeRetriedExplicitly(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	_, plan, tasks := createReadyPlanFixture(t, store, "/tmp/strict-failed-retry")
	if _, err := store.QueuePlan(ctx, plan.ID, plan.Version); err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimJob(ctx, "failure-worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err = store.MarkJobRunning(ctx, claimed.ID, "failure-worker")
	if err != nil {
		t.Fatal(err)
	}
	running, err := store.StartTask(ctx, tasks[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.FinishTask(ctx, running, "failure-session", false, "focused tests failed"); err != nil {
		t.Fatal(err)
	}
	if err = store.FailJob(ctx, claimed, "failure-worker", "focused tests failed", false); err != nil {
		t.Fatal(err)
	}

	failed, err := store.GetTask(ctx, tasks[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	successor, err := store.GetTask(ctx, tasks[1].ID)
	if err != nil {
		t.Fatal(err)
	}
	blocked := requirePlanExecutionBlocked(t, func() error {
		_, queueErr := store.QueueTask(ctx, successor.ID, successor.Version)
		return queueErr
	}())
	if blocker := blocked.Blockers[0]; blocker.TaskID == nil || *blocker.TaskID != failed.ID || blocker.TaskStatus != "failed" {
		t.Fatalf("successor blockers=%+v", blocked.Blockers)
	}
	_, automaticallyQueued, automaticErr := store.QueuePlanAutomatically(ctx, plan.ID)
	automaticBlocked := requirePlanExecutionBlocked(t, automaticErr)
	if automaticallyQueued || automaticBlocked.Blockers[0].Code != "manual_retry_required" || automaticBlocked.Blockers[0].TaskStatus != "failed" {
		t.Fatalf("automatic queued=%t blockers=%+v", automaticallyQueued, automaticBlocked.Blockers)
	}

	retryJob, err := store.QueueTask(ctx, failed.ID, failed.Version)
	if err != nil {
		t.Fatal(err)
	}
	if retryJob.AggregateID != failed.ID {
		t.Fatalf("retry queued task=%s, want failed task=%s", retryJob.AggregateID, failed.ID)
	}
	currentPlan, err := store.GetPlan(ctx, plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if currentPlan.Status != "running" {
		t.Fatalf("retried plan status=%s", currentPlan.Status)
	}
	var active int
	if err = store.Pool.QueryRow(ctx, `SELECT count(*) FROM plan_tasks WHERE plan_id=$1 AND status IN ('queued','running')`, plan.ID).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != 1 {
		t.Fatalf("active tasks after retry=%d, want 1", active)
	}
}

func TestInvalidPlanIsRejectedByManualAndAutomaticEntrypoints(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	project, err := store.CreateProject(ctx, CreateProjectParams{Name: "Invalid scheduling", WorkspacePath: "/tmp/strict-invalid", NormalizedWorkspace: "/tmp/strict-invalid"})
	if err != nil {
		t.Fatal(err)
	}
	project, err = store.SetAutomation(ctx, project.ID, true, project.Version)
	if err != nil {
		t.Fatal(err)
	}
	intake, generationJob, err := store.CreateIntake(ctx, CreateIntakeParams{ProjectID: project.ID, Kind: "requirement", Title: "Invalid scheduling", Body: "Reject invalid graph", ConfigSnapshot: json.RawMessage(`{}`), QueuePlan: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Pool.Exec(ctx, `UPDATE jobs SET status='cancelled' WHERE id=$1`, generationJob.ID); err != nil {
		t.Fatal(err)
	}
	invalidSpec := planspec.Spec{Version: planspec.CurrentVersion, Title: "Invalid", Summary: "Missing tasks"}
	plan, tasks, err := store.SaveGeneratedPlan(ctx, intake, invalidSpec, planspec.Render(invalidSpec))
	if err != nil {
		t.Fatal(err)
	}
	if plan.Executable || plan.Status != "blocked" || len(tasks) != 0 {
		t.Fatalf("invalid plan=%+v tasks=%d", plan, len(tasks))
	}

	manual := requirePlanExecutionBlocked(t, func() error {
		_, queueErr := store.QueuePlan(ctx, plan.ID, plan.Version)
		return queueErr
	}())
	if manual.Blockers[0].Code != "plan_invalid" || len(manual.Blockers[0].ValidationProblems) == 0 || !strings.Contains(manual.Blockers[0].Reason, "tasks") {
		t.Fatalf("manual invalid blockers=%+v", manual.Blockers)
	}
	_, queued, automaticErr := store.QueuePlanAutomatically(ctx, plan.ID)
	automatic := requirePlanExecutionBlocked(t, automaticErr)
	if queued || automatic.Blockers[0].Code != "plan_invalid" || len(automatic.Blockers[0].ValidationProblems) == 0 {
		t.Fatalf("automatic queued=%t blockers=%+v", queued, automatic.Blockers)
	}
	var taskJobs int
	if err = store.Pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE project_id=$1 AND job_type='task.execute'`, project.ID).Scan(&taskJobs); err != nil {
		t.Fatal(err)
	}
	if taskJobs != 0 {
		t.Fatalf("invalid plan created %d task jobs", taskJobs)
	}
}

func TestCompatibilityPlanKeepsPositionSerialOrder(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	_, plan, tasks := createReadyPlanFixture(t, store, "/tmp/strict-compatibility-order")
	if !plan.CompatibilityMode || len(tasks) != 2 {
		t.Fatalf("plan compatibility=%t tasks=%d", plan.CompatibilityMode, len(tasks))
	}
	if _, err := store.Pool.Exec(ctx, `UPDATE plan_tasks SET execution_order=execution_order+100 WHERE plan_id=$1`, plan.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pool.Exec(ctx, `UPDATE plan_tasks SET execution_order=CASE id WHEN $2 THEN 2 WHEN $3 THEN 1 END WHERE plan_id=$1`, plan.ID, tasks[0].ID, tasks[1].ID); err != nil {
		t.Fatal(err)
	}
	job, err := store.QueuePlan(ctx, plan.ID, plan.Version)
	if err != nil {
		t.Fatal(err)
	}
	if job.AggregateID != tasks[0].ID {
		t.Fatalf("compatibility plan queued task=%s, want position-first task=%s", job.AggregateID, tasks[0].ID)
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
	var automaticRetryJobs int
	if err = store.Pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE aggregate_type='task' AND aggregate_id=$1`, running.ID).Scan(&automaticRetryJobs); err != nil {
		t.Fatal(err)
	}
	if automaticRetryJobs != 1 {
		t.Fatalf("automatic retry created %d jobs, want exactly the original job", automaticRetryJobs)
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
	if requeued.ID == claimed.ID {
		t.Fatalf("explicit retry reused terminal job %s", requeued.ID)
	}
	var explicitRetryJobs int
	if err = store.Pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE aggregate_type='task' AND aggregate_id=$1`, running.ID).Scan(&explicitRetryJobs); err != nil {
		t.Fatal(err)
	}
	if explicitRetryJobs != 2 {
		t.Fatalf("explicit retry left %d jobs, want one original and one retry", explicitRetryJobs)
	}
	assertJobProvider(t, requeued, "codex", boolPointer(true))
	retryAudit := assertLifecycleAudit(t, store, domain.LifecycleResourceTask, running.ID, "running", "queued", domain.LifecycleSourceWorker, &claimed.ID, nil)
	if retryAudit.ReasonCode != domain.LifecycleReasonAutomaticRetry {
		t.Fatalf("automatic retry reason=%q", retryAudit.ReasonCode)
	}
	explicitAudit := assertLifecycleAudit(t, store, domain.LifecycleResourceTask, running.ID, "pending", "queued", domain.LifecycleSourceUser, &requeued.ID, nil)
	if explicitAudit.ReasonCode != domain.LifecycleReasonResumeRequested {
		t.Fatalf("explicit retry reason=%q", explicitAudit.ReasonCode)
	}
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

func TestBlockTaskJobForDriftRestoresRecoverableStateAndEmitsEvents(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	project, plan, tasks := createReadyPlanFixture(t, store, "/tmp/task-drift-block")

	queued, err := store.QueuePlan(ctx, plan.ID, plan.Version)
	if err != nil {
		t.Fatal(err)
	}
	const workerID = "drift-worker"
	claimed, err := store.ClaimJob(ctx, workerID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.ID != queued.ID {
		t.Fatalf("claimed job=%s, want queued task job=%s", claimed.ID, queued.ID)
	}
	running, err := store.MarkJobRunning(ctx, claimed.ID, workerID)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.BlockTaskJobForDrift(ctx, running, workerID, json.RawMessage(`{"severity":"requires_confirmation","fingerprint":"changed"}`)); err != nil {
		t.Fatal(err)
	}

	var jobStatus string
	var retryAttempts int
	if err = store.Pool.QueryRow(ctx, `SELECT status,attempt FROM jobs WHERE id=$1`, running.ID).Scan(&jobStatus, &retryAttempts); err != nil {
		t.Fatal(err)
	}
	if jobStatus != "failed" || retryAttempts != 1 {
		t.Fatalf("drift-blocked job status=%q attempts=%d, want failed without retry", jobStatus, retryAttempts)
	}
	task, err := store.GetTask(ctx, tasks[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != "pending" || task.StartedAt != nil || task.FinishedAt != nil {
		t.Fatalf("drift-blocked task=%+v, want pending recoverable task without execution timestamps", task)
	}
	blockedPlan, err := store.GetPlan(ctx, plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if blockedPlan.Status != "blocked" {
		t.Fatalf("drift-blocked plan status=%q, want blocked", blockedPlan.Status)
	}
	var taskEvents, planEvents, agentRuns int
	if err = store.Pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE aggregate_type='task' AND aggregate_id=$1 AND event_type='task.drift_detected'`, task.ID).Scan(&taskEvents); err != nil {
		t.Fatal(err)
	}
	if err = store.Pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE aggregate_type='plan' AND aggregate_id=$1 AND event_type='plan.drift_detected'`, plan.ID).Scan(&planEvents); err != nil {
		t.Fatal(err)
	}
	if err = store.Pool.QueryRow(ctx, `SELECT count(*) FROM agent_runs WHERE project_id=$1 AND job_id=$2`, project.ID, running.ID).Scan(&agentRuns); err != nil {
		t.Fatal(err)
	}
	if taskEvents != 1 || planEvents != 1 || agentRuns != 0 {
		t.Fatalf("drift events/runs: task=%d plan=%d agentRuns=%d", taskEvents, planEvents, agentRuns)
	}
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

func TestClaimJobDoesNotInterleavePlansWhileTaskIsInFlight(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	project, err := store.CreateProject(ctx, CreateProjectParams{Name: "In-flight plan ownership", WorkspacePath: "/tmp/in-flight-plan-ownership", NormalizedWorkspace: "/tmp/in-flight-plan-ownership"})
	if err != nil {
		t.Fatal(err)
	}
	project, err = store.SetAutomation(ctx, project.ID, true, project.Version)
	if err != nil {
		t.Fatal(err)
	}

	createReadyPlan := func(title string) (domain.Plan, []domain.PlanTask) {
		t.Helper()
		intake, generationJob, createErr := store.CreateIntake(ctx, CreateIntakeParams{ProjectID: project.ID, Kind: "requirement", Title: title, Body: "Keep task execution single-flight per project", ConfigSnapshot: json.RawMessage(`{}`), QueuePlan: true})
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
			Summary: "Verify a project never leases tasks from two plans at once",
			Tasks: []planspec.Task{
				{Title: "Implementation", Scope: []string{"implementation"}, Acceptance: []string{"done"}},
			},
			FinalValidation: []string{"validate"},
		}, title)
		if saveErr != nil {
			t.Fatal(saveErr)
		}
		return plan, tasks
	}

	planA, tasksA := createReadyPlan("Plan A")
	planB, tasksB := createReadyPlan("Plan B")
	firstA, err := store.QueuePlan(ctx, planA.ID, planA.Version)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.QueuePlan(ctx, planB.ID, planB.Version); err != nil {
		t.Fatal(err)
	}

	claimed, err := store.ClaimJob(ctx, "first-worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.ID != firstA.ID || claimed.AggregateID != tasksA[0].ID {
		t.Fatalf("first claimed job=%s task=%s, want Plan A job=%s task=%s", claimed.ID, claimed.AggregateID, firstA.ID, tasksA[0].ID)
	}

	// A second worker must not lease Plan B while Plan A's task is merely
	// leased (the small interval before it becomes running) nor after it is
	// running. This is the boundary that previously allowed plans to appear to
	// alternate when multiple workers woke up together.
	if _, err = store.ClaimJob(ctx, "second-worker", time.Minute); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("claim while Plan A task is leased error=%v, want no job; Plan B task=%s", err, tasksB[0].ID)
	}
	if _, err = store.MarkJobRunning(ctx, claimed.ID, "first-worker"); err != nil {
		t.Fatal(err)
	}
	if _, err = store.ClaimJob(ctx, "second-worker", time.Minute); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("claim while Plan A task is running error=%v, want no job; Plan B task=%s", err, tasksB[0].ID)
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
	if run.JobID != nil || run.PID == nil || *run.PID != 1234 || run.Status != "succeeded" || run.DurationMS == nil || *run.DurationMS != 1500 || run.LogPath != logPath {
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

func TestAgentRunPersistsStructuredObservabilityWithoutInventingUnknowns(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	project, plan, tasks := createReadyPlanFixture(t, store, "/tmp/run-observability")
	job, err := store.QueuePlan(ctx, plan.ID, plan.Version)
	if err != nil {
		t.Fatal(err)
	}

	logicalOperationID := uuid.New()
	jobAttempt, retryCount := 2, 1
	queueWaitMS := int64(275)
	runID := uuid.New()
	if err = store.StartAgentRun(ctx, AgentRunStart{
		ID:                        runID,
		ProjectID:                 project.ID,
		IntakeID:                  &plan.IntakeID,
		PlanID:                    &plan.ID,
		JobID:                     &job.ID,
		TaskID:                    &tasks[0].ID,
		LogicalOperationID:        &logicalOperationID,
		JobAttempt:                &jobAttempt,
		RetryCount:                &retryCount,
		QueueWaitMS:               &queueWaitMS,
		Provider:                  "codex",
		OperationType:             domain.AgentRunOperationTaskExecution,
		CommandSummary:            "codex task",
		SessionMode:               domain.AgentRunSessionModeSnapshotRestored,
		SessionInvalidationReason: domain.AgentRunSessionInvalidationSessionNotFound,
		LogPath:                   "/tmp/run-observability.log",
	}); err != nil {
		t.Fatal(err)
	}

	exitCode := 17
	durationMS := int64(1250)
	outputBytes, outputLines, eventCount := int64(4096), int64(38), int64(12)
	outputTruncated := true
	inputTokens, outputTokens, totalTokens := int64(1200), int64(340), int64(1540)
	if err = store.FinishAgentRunWithDetails(ctx, runID, AgentRunFinish{
		Status:            "failed",
		ExitCode:          &exitCode,
		SessionID:         "cli-session-1",
		FailureCategory:   "cli_exit",
		TerminationReason: "provider returned a non-zero exit code",
		DurationMS:        &durationMS,
		OutputBytes:       &outputBytes,
		OutputLines:       &outputLines,
		EventCount:        &eventCount,
		OutputTruncated:   &outputTruncated,
		InputTokens:       &inputTokens,
		OutputTokens:      &outputTokens,
		TotalTokens:       &totalTokens,
		CostAmount:        "0.01234567",
		CostCurrency:      "EUR",
	}); err != nil {
		t.Fatal(err)
	}

	run, err := store.GetAgentRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.IntakeID == nil || *run.IntakeID != plan.IntakeID || run.PlanID == nil || *run.PlanID != plan.ID ||
		run.JobID == nil || *run.JobID != job.ID || run.TaskID == nil || *run.TaskID != tasks[0].ID ||
		run.LogicalOperationID == nil || *run.LogicalOperationID != logicalOperationID {
		t.Fatalf("run associations were not persisted: %+v", run)
	}
	if run.OperationType == nil || *run.OperationType != domain.AgentRunOperationTaskExecution ||
		run.JobAttempt == nil || *run.JobAttempt != jobAttempt || run.RetryCount == nil || *run.RetryCount != retryCount ||
		run.SessionMode == nil || *run.SessionMode != domain.AgentRunSessionModeSnapshotRestored ||
		run.SessionInvalidationReason == nil || *run.SessionInvalidationReason != domain.AgentRunSessionInvalidationSessionNotFound {
		t.Fatalf("run classification was not persisted: %+v", run)
	}
	if run.QueueWaitMS == nil || *run.QueueWaitMS != queueWaitMS || run.DurationMS == nil || *run.DurationMS != durationMS ||
		run.ExitCode == nil || *run.ExitCode != exitCode || run.FailureCategory == nil || *run.FailureCategory != "cli_exit" ||
		run.OutputBytes == nil || *run.OutputBytes != outputBytes || run.OutputLines == nil || *run.OutputLines != outputLines ||
		run.EventCount == nil || *run.EventCount != eventCount || run.OutputTruncated == nil || !*run.OutputTruncated {
		t.Fatalf("run timing/output statistics were not persisted: %+v", run)
	}
	if run.InputTokens == nil || *run.InputTokens != inputTokens || run.OutputTokens == nil || *run.OutputTokens != outputTokens ||
		run.TotalTokens == nil || *run.TotalTokens != totalTokens || run.CostAmount == nil || *run.CostAmount != "0.01234567" ||
		run.CostCurrency == nil || *run.CostCurrency != "EUR" {
		t.Fatalf("run usage/cost was not persisted verbatim: %+v", run)
	}

	unknownID := uuid.New()
	if err = store.StartAgentRun(ctx, AgentRunStart{
		ID:             unknownID,
		ProjectID:      project.ID,
		Provider:       "claude",
		CommandSummary: "legacy-compatible invocation",
		LogPath:        "/tmp/run-unknown.log",
	}); err != nil {
		t.Fatal(err)
	}
	if err = store.FinishAgentRunWithDetails(ctx, unknownID, AgentRunFinish{
		Status:     "succeeded",
		CostAmount: "4.25", // No explicit currency: preserve neither field.
	}); err != nil {
		t.Fatal(err)
	}
	unknown, err := store.GetAgentRun(ctx, unknownID)
	if err != nil {
		t.Fatal(err)
	}
	if unknown.OperationType != nil || unknown.JobAttempt != nil || unknown.RetryCount != nil ||
		unknown.SessionMode != nil || unknown.SessionInvalidationReason != nil || unknown.QueueWaitMS != nil ||
		unknown.DurationMS != nil || unknown.ExitCode != nil || unknown.FailureCategory != nil ||
		unknown.OutputBytes != nil || unknown.OutputLines != nil || unknown.EventCount != nil || unknown.OutputTruncated != nil ||
		unknown.InputTokens != nil || unknown.OutputTokens != nil || unknown.TotalTokens != nil ||
		unknown.CostAmount != nil || unknown.CostCurrency != nil {
		t.Fatalf("unknown observability values were fabricated: %+v", unknown)
	}

	expectedIndexes := []string{
		"agent_runs_project_created_idx",
		"agent_runs_started_idx",
		"agent_runs_provider_started_idx",
		"agent_runs_intake_started_idx",
		"agent_runs_plan_started_idx",
		"agent_runs_task_started_idx",
	}
	for _, indexName := range expectedIndexes {
		var exists bool
		if err = store.Pool.QueryRow(ctx, `SELECT EXISTS (
			SELECT 1 FROM pg_indexes WHERE schemaname=current_schema() AND tablename='agent_runs' AND indexname=$1
		)`, indexName).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if !exists {
			t.Errorf("missing agent_runs query index %q", indexName)
		}
	}

	forbiddenColumns := map[string]bool{
		"source_code": true, "raw_log": true, "command_args": true,
		"environment": true, "environment_variables": true, "auth_token": true,
	}
	rows, err := store.Pool.Query(ctx, `SELECT column_name FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='agent_runs'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var column string
		if err = rows.Scan(&column); err != nil {
			t.Fatal(err)
		}
		if forbiddenColumns[column] {
			t.Errorf("agent_runs must not persist sensitive or bulky field %q", column)
		}
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
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
	if stoppedRun.Status != "cancelled" || stoppedRun.FinishedAt == nil || stoppedRun.TerminationReason == nil || *stoppedRun.TerminationReason != "project automation stopped" {
		t.Fatalf("agent run was not reconciled: %+v", stoppedRun)
	}
	jobBefore, err := store.GetLifecycleState(ctx, domain.LifecycleResourceJob, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	taskBefore, err := store.GetLifecycleState(ctx, domain.LifecycleResourceTask, running.ID)
	if err != nil {
		t.Fatal(err)
	}
	runBefore, err := store.GetLifecycleState(ctx, domain.LifecycleResourceAgentRun, runID)
	if err != nil {
		t.Fatal(err)
	}
	jobTransitionsBefore, err := store.ListLifecycleTransitions(ctx, domain.LifecycleResourceJob, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	taskTransitionsBefore, err := store.ListLifecycleTransitions(ctx, domain.LifecycleResourceTask, running.ID)
	if err != nil {
		t.Fatal(err)
	}
	runTransitionsBefore, err := store.ListLifecycleTransitions(ctx, domain.LifecycleResourceAgentRun, runID)
	if err != nil {
		t.Fatal(err)
	}

	// The worker can observe SIGTERM after the transactional reset. Late run
	// completion, task cleanup, duplicate cancellation, and recovery inspection
	// must all be harmless and must not append replacement lifecycle events.
	if err = store.ReturnTaskPending(ctx, running, "cancelled by automation stop"); err != nil {
		t.Fatalf("late task cleanup: %v", err)
	}
	if err = store.FinishAgentRun(ctx, runID, "succeeded", 0, "", "", time.Second); err != nil {
		t.Fatalf("late run cleanup: %v", err)
	}
	if err = store.CancelJob(ctx, job.ID, "stop-worker"); err != nil {
		t.Fatalf("duplicate job cancellation: %v", err)
	}
	if err = store.RecoverJobs(ctx); err != nil {
		t.Fatalf("recovery inspection: %v", err)
	}
	stoppedRun, err = store.GetAgentRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if stoppedRun.Status != "cancelled" || stoppedRun.TerminationReason == nil || *stoppedRun.TerminationReason != "project automation stopped" {
		t.Fatalf("late cleanup overwrote cancellation: %+v", stoppedRun)
	}
	for _, item := range []struct {
		resource        domain.LifecycleResource
		id              uuid.UUID
		before          domain.LifecycleState
		transitionCount int
	}{
		{domain.LifecycleResourceJob, job.ID, jobBefore, len(jobTransitionsBefore)},
		{domain.LifecycleResourceTask, running.ID, taskBefore, len(taskTransitionsBefore)},
		{domain.LifecycleResourceAgentRun, runID, runBefore, len(runTransitionsBefore)},
	} {
		after, stateErr := store.GetLifecycleState(ctx, item.resource, item.id)
		if stateErr != nil {
			t.Fatal(stateErr)
		}
		afterTransitions, transitionErr := store.ListLifecycleTransitions(ctx, item.resource, item.id)
		if transitionErr != nil {
			t.Fatal(transitionErr)
		}
		if after.Status != item.before.Status || after.Version != item.before.Version || len(afterTransitions) != item.transitionCount {
			t.Fatalf("terminal %s changed after late callbacks: before=%+v after=%+v transitions=%d->%d", item.resource, item.before, after, item.transitionCount, len(afterTransitions))
		}
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

func TestAutomationStopResetsStaleQueuedTaskWithoutReopeningTerminalPlan(t *testing.T) {
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

	preservedPlan, err := store.GetPlan(ctx, plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if preservedPlan.Status != "completed" {
		t.Fatalf("automation stop reopened terminal plan: %+v", preservedPlan)
	}
	terminalTransitions, err := store.ListLifecycleTransitions(ctx, domain.LifecycleResourcePlan, plan.ID)
	if err != nil {
		t.Fatal(err)
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
	if first.Status != "pending" || second.Status != "pending" {
		t.Fatalf("terminal plan tasks changed during automation resume: first=%+v second=%+v", first, second)
	}
	var queuedJobs int
	if err = store.Pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE project_id=$1 AND job_type='task.execute' AND status='queued'`, project.ID).Scan(&queuedJobs); err != nil {
		t.Fatal(err)
	}
	if queuedJobs != 0 {
		t.Fatalf("automation resume queued %d jobs for a terminal plan", queuedJobs)
	}
	afterResume, err := store.GetPlan(ctx, plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	afterTransitions, err := store.ListLifecycleTransitions(ctx, domain.LifecycleResourcePlan, plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if afterResume.Status != "completed" || afterResume.Version != preservedPlan.Version || len(afterTransitions) != len(terminalTransitions) {
		t.Fatalf("terminal plan changed on resume: before=%+v after=%+v transitions=%d->%d", preservedPlan, afterResume, len(terminalTransitions), len(afterTransitions))
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

func TestAgentSessionsPersistRequirementAndPlanExecutionChains(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	project, err := store.CreateProject(ctx, CreateProjectParams{Name: "Session chain", WorkspacePath: "/tmp/session-chain", NormalizedWorkspace: "/tmp/session-chain"})
	if err != nil {
		t.Fatal(err)
	}
	intake, generationJob, err := store.CreateIntake(ctx, CreateIntakeParams{
		ProjectID:                  project.ID,
		Kind:                       "requirement",
		Title:                      "复用讨论会话",
		Body:                       "计划和任务要续接同一个 CLI 会话。",
		ConfigSnapshot:             json.RawMessage(`{}`),
		QueuePlan:                  true,
		RequirementSessionID:       "discussion-session-001",
		RequirementSessionProvider: "codex",
	})
	if err != nil || generationJob == nil {
		t.Fatalf("create intake: job=%+v err=%v", generationJob, err)
	}
	requirement, err := store.GetRequirementSession(ctx, intake.ID)
	if err != nil {
		t.Fatal(err)
	}
	if requirement.Provider != "codex" || requirement.CLISessionID != "discussion-session-001" || requirement.Purpose != "requirement" {
		t.Fatalf("requirement session=%+v", requirement)
	}
	if _, err = store.UpsertRequirementSession(ctx, project.ID, intake.ID, "codex", "planning-session-002", "planning snapshot"); err != nil {
		t.Fatal(err)
	}
	requirement, err = store.GetRequirementSession(ctx, intake.ID)
	if err != nil || requirement.CLISessionID != "planning-session-002" || requirement.ContextSummary != "planning snapshot" {
		t.Fatalf("upserted requirement session=%+v err=%v", requirement, err)
	}

	if _, err = store.Pool.Exec(ctx, `UPDATE jobs SET status='cancelled' WHERE id=$1`, generationJob.ID); err != nil {
		t.Fatal(err)
	}
	spec := planspec.Spec{Title: "会话计划", Summary: "续接计划上下文", Tasks: []planspec.Task{{Title: "实现会话复用", Scope: []string{"backend"}, Acceptance: []string{"会话持续"}}}, FinalValidation: []string{"tests pass"}}
	plan, tasks, err := store.SaveGeneratedPlan(ctx, intake, spec, planspec.Render(spec))
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) == 0 {
		t.Fatal("generated plan has no implementation task")
	}
	if _, err = store.UpsertExecutionSession(ctx, project.ID, plan.ID, "codex", "execution-session-003", "execution snapshot", &tasks[0].ID); err != nil {
		t.Fatal(err)
	}
	execution, err := store.GetExecutionSession(ctx, plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if execution.Provider != "codex" || execution.CLISessionID != "execution-session-003" || execution.LastTaskID == nil || *execution.LastTaskID != tasks[0].ID {
		t.Fatalf("execution session=%+v", execution)
	}
	if err = store.MarkAgentSessionStale(ctx, execution.ID); err != nil {
		t.Fatal(err)
	}
	stale, err := store.GetExecutionSession(ctx, plan.ID)
	if err != nil || stale.Status != "stale" {
		t.Fatalf("stale session=%+v err=%v", stale, err)
	}
	if _, err = store.UpsertExecutionSession(ctx, project.ID, plan.ID, "claude", "execution-session-004", "recovered snapshot", &tasks[0].ID); err != nil {
		t.Fatal(err)
	}
	recovered, err := store.GetExecutionSession(ctx, plan.ID)
	if err != nil || recovered.Status != "active" || recovered.Provider != "claude" || recovered.CLISessionID != "execution-session-004" {
		t.Fatalf("recovered session=%+v err=%v", recovered, err)
	}
}

func TestRepositoryGitCheckpointCapturesBoundedTaskDiffWithoutMutatingWorkspace(t *testing.T) {
	workspace := newRepositoryCheckpointWorkspace(t)
	ctx := context.Background()
	checkpointTestWriteFile(t, workspace, "preexisting.txt", "dirty before task\n")
	before := captureRepositoryGitCheckpoint(ctx, workspace, uuid.New())
	if !before.Available {
		t.Fatalf("baseline checkpoint unavailable: %+v", before)
	}

	checkpointTestWriteFile(t, workspace, "modified.txt", "after\n")
	if err := os.Remove(filepath.Join(workspace, "deleted.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(workspace, "rename-old.txt"), filepath.Join(workspace, "rename-new.txt")); err != nil {
		t.Fatal(err)
	}
	checkpointTestWriteFile(t, workspace, "added.txt", "added\n")
	if err := os.WriteFile(filepath.Join(workspace, "binary.bin"), []byte{0, 1, 2, 3, 0xff}, 0o600); err != nil {
		t.Fatal(err)
	}
	largePatch := strings.Repeat("bounded diff line\n", 60000)
	checkpointTestWriteFile(t, workspace, "large-diff.txt", largePatch)
	oversizedPath := filepath.Join(workspace, "oversized.dat")
	oversized, err := os.Create(oversizedPath)
	if err != nil {
		t.Fatal(err)
	}
	if err = oversized.Truncate(maxTaskCheckpointFileBytes + 1); err != nil {
		_ = oversized.Close()
		t.Fatal(err)
	}
	if err = oversized.Close(); err != nil {
		t.Fatal(err)
	}

	statusBefore := checkpointTestGit(t, workspace, "status", "--porcelain=v1", "--untracked-files=all")
	headBefore := checkpointTestGit(t, workspace, "rev-parse", "HEAD")
	branchBefore := checkpointTestGit(t, workspace, "branch", "--show-current")
	indexBefore, err := os.ReadFile(filepath.Join(workspace, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	modifiedBefore, err := os.ReadFile(filepath.Join(workspace, "modified.txt"))
	if err != nil {
		t.Fatal(err)
	}

	after := captureRepositoryGitCheckpoint(ctx, workspace, uuid.New())
	if !after.Available {
		t.Fatalf("task checkpoint unavailable: %+v", after)
	}
	if got := checkpointTestGit(t, workspace, "status", "--porcelain=v1", "--untracked-files=all"); got != statusBefore {
		t.Fatalf("checkpoint changed Git status:\n%s\n->\n%s", statusBefore, got)
	}
	if got := checkpointTestGit(t, workspace, "rev-parse", "HEAD"); got != headBefore {
		t.Fatalf("checkpoint changed HEAD: %s -> %s", headBefore, got)
	}
	if got := checkpointTestGit(t, workspace, "branch", "--show-current"); got != branchBefore {
		t.Fatalf("checkpoint changed branch: %s -> %s", branchBefore, got)
	}
	indexAfter, err := os.ReadFile(filepath.Join(workspace, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(indexBefore, indexAfter) {
		t.Fatal("checkpoint changed the user Git index")
	}
	modifiedAfter, err := os.ReadFile(filepath.Join(workspace, "modified.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(modifiedBefore, modifiedAfter) {
		t.Fatal("checkpoint changed a worktree file")
	}

	files, summary := collectRepositoryTaskDiff(ctx, before, after)
	if available, _ := summary["diffAvailable"].(bool); !available {
		t.Fatalf("task diff unavailable: %+v", summary)
	}
	metadata, ok := summary["gitCheckpoint"].(map[string]any)
	if !ok || metadata["available"] != true || metadata["worktreeTree"] != after.WorktreeTree {
		t.Fatalf("missing current checkpoint metadata: %+v", summary)
	}
	indexed := make(map[string]PlanExecutionCheckpointFile, len(files))
	totalPatchBytes := 0
	for _, file := range files {
		if filepath.IsAbs(file.Path) || strings.Contains(file.Path, "..") || strings.Contains(file.Path, `\`) {
			t.Fatalf("unsafe checkpoint path: %+v", file)
		}
		indexed[file.Path] = file
		for _, hunk := range file.Hunks {
			totalPatchBytes += len(hunk.Patch)
		}
	}
	if _, exists := indexed["preexisting.txt"]; exists {
		t.Fatalf("pre-existing dirty file was attributed to task: %+v", indexed["preexisting.txt"])
	}
	for path, status := range map[string]string{
		"modified.txt": "modified", "deleted.txt": "deleted", "rename-new.txt": "renamed",
		"added.txt": "added", "binary.bin": "added", "large-diff.txt": "added", "oversized.dat": "added",
	} {
		if file, exists := indexed[path]; !exists || file.Status != status {
			t.Fatalf("file %s=%+v exists=%t, want status %s", path, file, exists, status)
		}
	}
	if indexed["rename-new.txt"].PreviousPath != "rename-old.txt" {
		t.Fatalf("rename evidence=%+v", indexed["rename-new.txt"])
	}
	if !indexed["binary.bin"].Binary || len(indexed["binary.bin"].Hunks) != 0 {
		t.Fatalf("binary evidence=%+v", indexed["binary.bin"])
	}
	if len(indexed["oversized.dat"].Hunks) != 0 {
		t.Fatalf("oversized file unexpectedly retained a patch: %+v", indexed["oversized.dat"])
	}
	if totalPatchBytes > maxTaskCheckpointPatchBytes {
		t.Fatalf("stored patch bytes=%d, limit=%d", totalPatchBytes, maxTaskCheckpointPatchBytes)
	}
	if truncated, _ := summary["diffTruncated"].(bool); !truncated {
		t.Fatalf("large diff was not marked truncated: %+v", summary)
	}
	if count, _ := summary["oversizedFiles"].(int); count != 1 {
		t.Fatalf("oversized statistics=%+v", summary)
	}
	blobSize := checkpointTestGit(t, workspace, "cat-file", "-s", after.WorktreeTree+":oversized.dat")
	if blobSize == fmt.Sprint(maxTaskCheckpointFileBytes+1) {
		t.Fatalf("oversized worktree content was copied into checkpoint: blob size=%s", blobSize)
	}
}

func TestPreparePlanExecutionChangesRejectsUnsafeWorkspacePaths(t *testing.T) {
	workspace := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(workspace, "escape")); err != nil {
		t.Skipf("symlinks are unavailable: %v", err)
	}
	for _, relative := range []string{"/absolute", "../traversal", `C:\\escape`, `nested\\..\\escape`, "escape/file.txt"} {
		t.Run(strings.NewReplacer("/", "_", `\\`, "_").Replace(relative), func(t *testing.T) {
			_, err := preparePlanExecutionChanges(workspace, PlanExecutionCheckpointParams{Files: []PlanExecutionCheckpointFile{{Path: relative, Status: "modified"}}})
			if err == nil {
				t.Fatalf("unsafe checkpoint path %q was accepted", relative)
			}
		})
	}
	safe, err := preparePlanExecutionChanges(workspace, PlanExecutionCheckpointParams{Files: []PlanExecutionCheckpointFile{{Path: "missing/deleted.txt", Status: "deleted"}}})
	if err != nil || len(safe.Files) != 1 || safe.Files[0].Path != "missing/deleted.txt" {
		t.Fatalf("safe missing deletion path was rejected: changes=%+v err=%v", safe, err)
	}
}

func TestSuccessfulTaskPersistsRepositoryCheckpointFilesAndDiff(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	workspace := newRepositoryCheckpointWorkspace(t)
	checkpointTestWriteFile(t, workspace, "preexisting.txt", "dirty before plan baseline\n")
	_, plan, tasks := createReadyPlanFixture(t, store, workspace)
	if _, err := store.QueuePlan(ctx, plan.ID, plan.Version); err != nil {
		t.Fatal(err)
	}
	started, err := store.StartTask(ctx, tasks[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	checkpointTestWriteFile(t, workspace, "modified.txt", "task modification\n")
	if err = os.Remove(filepath.Join(workspace, "deleted.txt")); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(filepath.Join(workspace, "rename-old.txt"), filepath.Join(workspace, "rename-new.txt")); err != nil {
		t.Fatal(err)
	}
	checkpointTestWriteFile(t, workspace, "added.txt", "task addition\n")
	if err = os.WriteFile(filepath.Join(workspace, "binary.bin"), []byte{0, 1, 2, 3}, 0o600); err != nil {
		t.Fatal(err)
	}
	statusBefore := checkpointTestGit(t, workspace, "status", "--porcelain=v1", "--untracked-files=all")
	headBefore := checkpointTestGit(t, workspace, "rev-parse", "HEAD")
	indexBefore, err := os.ReadFile(filepath.Join(workspace, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	if err = store.FinishTask(ctx, started, "session-checkpoint", true, "done"); err != nil {
		t.Fatal(err)
	}
	if got := checkpointTestGit(t, workspace, "status", "--porcelain=v1", "--untracked-files=all"); got != statusBefore {
		t.Fatalf("task completion changed Git status:\n%s\n->\n%s", statusBefore, got)
	}
	if got := checkpointTestGit(t, workspace, "rev-parse", "HEAD"); got != headBefore {
		t.Fatalf("task completion changed HEAD: %s -> %s", headBefore, got)
	}
	indexAfter, err := os.ReadFile(filepath.Join(workspace, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(indexBefore, indexAfter) {
		t.Fatal("task completion changed the user Git index")
	}

	checkpoint, err := store.GetLatestPlanExecutionSnapshot(ctx, plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if checkpoint.Kind != domain.PlanSnapshotKindTaskCheckpoint || checkpoint.TaskID == nil || *checkpoint.TaskID != started.ID {
		t.Fatalf("task checkpoint=%+v", checkpoint)
	}
	indexed := checkpointFilesByPath(checkpoint.Files)
	if _, exists := indexed["preexisting.txt"]; exists {
		t.Fatalf("checkpoint included pre-existing dirty file: %+v", indexed["preexisting.txt"])
	}
	for path, status := range map[string]string{
		"modified.txt": "modified", "deleted.txt": "deleted", "rename-new.txt": "renamed", "added.txt": "added", "binary.bin": "added",
	} {
		if file, exists := indexed[path]; !exists || file.Status != status {
			t.Fatalf("persisted file %s=%+v exists=%t, want %s", path, file, exists, status)
		}
	}
	if indexed["rename-new.txt"].PreviousPath != "rename-old.txt" || !indexed["binary.bin"].Binary || len(indexed["binary.bin"].Hunks) != 0 {
		t.Fatalf("rename/binary checkpoint evidence: rename=%+v binary=%+v", indexed["rename-new.txt"], indexed["binary.bin"])
	}
	if len(indexed["modified.txt"].Hunks) == 0 || !strings.Contains(indexed["modified.txt"].Hunks[0].Patch, "+task modification") {
		t.Fatalf("modified file diff was not persisted: %+v", indexed["modified.txt"])
	}
	var summary map[string]any
	if err = json.Unmarshal(checkpoint.ChangeSummary, &summary); err != nil {
		t.Fatal(err)
	}
	metadata, _ := summary["gitCheckpoint"].(map[string]any)
	if summary["diffAvailable"] != true || metadata["available"] != true || strings.TrimSpace(fmt.Sprint(metadata["worktreeTree"])) == "" {
		t.Fatalf("checkpoint summary=%+v", summary)
	}
}

func TestGeneratedPlanPersistsImmutableExecutionSnapshotHistory(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	workspace := t.TempDir()
	if _, err := exec.Command("git", "init", workspace).CombinedOutput(); err != nil {
		t.Skipf("git is unavailable: %v", err)
	}
	for _, args := range [][]string{
		{"-C", workspace, "config", "user.email", "specrelay@example.test"},
		{"-C", workspace, "config", "user.name", "SpecRelay Test"},
	} {
		if output, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, output)
		}
	}
	if err := os.WriteFile(filepath.Join(workspace, "README.md"), []byte("baseline\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"-C", workspace, "add", "README.md"}, {"-C", workspace, "commit", "-m", "baseline"}} {
		if output, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, output)
		}
	}

	project, err := store.CreateProject(ctx, CreateProjectParams{Name: "Snapshot", WorkspacePath: workspace, NormalizedWorkspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	intake, generationJob, err := store.CreateIntake(WithExecutionProvider(ctx, "claude"), CreateIntakeParams{
		ProjectID: project.ID, Kind: "requirement", Title: "Immutable snapshot", Body: "Preserve every execution input", ConfigSnapshot: json.RawMessage(`{}`), QueuePlan: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Pool.Exec(ctx, `UPDATE jobs SET status='cancelled' WHERE id=$1`, generationJob.ID); err != nil {
		t.Fatal(err)
	}
	spec := planspec.Spec{Title: "Snapshot plan", Summary: "Keep a baseline", Tasks: []planspec.Task{{Title: "Implement", Scope: []string{"backend"}, Acceptance: []string{"passes"}}}, FinalValidation: []string{"tests pass"}}
	plan, _, err := store.SaveGeneratedPlan(ctx, intake, spec, planspec.Render(spec))
	if err != nil {
		t.Fatal(err)
	}
	if plan.Version != 2 || plan.ContentVersion != 1 || plan.DriftStatus != domain.PlanDriftStatusClean || plan.ExecutionSnapshotID == nil {
		t.Fatalf("generated plan versions/state: %+v", plan)
	}
	baseline, err := store.GetLatestPlanExecutionSnapshot(ctx, plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if baseline.Sequence != 1 || baseline.Kind != domain.PlanSnapshotKindGenerationBaseline || baseline.RequirementID != intake.ID || baseline.RequirementVersion != 2 || baseline.PlanResourceVersion != 2 || baseline.PlanContentVersion != 1 {
		t.Fatalf("baseline identity/version fields: %+v", baseline)
	}
	if baseline.GenerationProvider != "claude" || baseline.ExecutionProvider != "codex" || baseline.WorkspacePathNormalized != workspace || baseline.GitRoot == "" || baseline.GitHead == "" || len(baseline.GitWorkspaceDigest) != 64 {
		t.Fatalf("baseline provider/workspace/git fields: %+v", baseline)
	}
	if len(baseline.RequirementDigest) != 64 || len(baseline.PlanSpecDigest) != 64 || baseline.ProjectVersion != project.Version || baseline.ConfigVersion < 1 || !json.Valid(baseline.KeyExecutionFields) {
		t.Fatalf("baseline summaries/config fields: %+v", baseline)
	}

	if _, err = store.Pool.Exec(ctx, `UPDATE plans SET status='blocked',version=version+1,updated_at=now() WHERE id=$1`, plan.ID); err != nil {
		t.Fatal(err)
	}
	stateOnly, err := store.GetPlan(ctx, plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stateOnly.Version != 3 || stateOnly.ContentVersion != 1 {
		t.Fatalf("state transition changed content version: %+v", stateOnly)
	}
	if err = os.WriteFile(filepath.Join(workspace, "README.md"), []byte("workspace drift\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	drift, err := store.GetPlanDrift(ctx, plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if drift.Status != domain.PlanDriftStatusDetected || !drift.RequiresExplicitDisposition || string(drift.Differences) == "[]" {
		t.Fatalf("workspace drift was not detected: %+v", drift)
	}

	updatedSpec := planspec.Spec{Title: "Accepted plan", Summary: "Accept reviewed drift", Tasks: spec.Tasks, FinalValidation: spec.FinalValidation}
	updatedSpecJSON, err := json.Marshal(updatedSpec)
	if err != nil {
		t.Fatal(err)
	}
	updated, accepted, audit, err := store.UpdatePlanExecutionSnapshot(ctx, UpdatePlanExecutionSnapshotParams{
		PlanID: plan.ID, Version: stateOnly.Version, OriginalSnapshotID: baseline.ID,
		Title: updatedSpec.Title, Spec: updatedSpecJSON, Markdown: planspec.Render(updatedSpec), ConfigSnapshot: stateOnly.ConfigSnapshot,
		RawDiff: json.RawMessage(`{"title":{"before":"Snapshot plan","after":"Accepted plan"}}`), Channel: "desktop", Reason: "reviewed and accepted plan changes",
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Version != 4 || updated.ContentVersion != 2 || accepted.Sequence != 2 || accepted.PreviousSnapshotID == nil || *accepted.PreviousSnapshotID != baseline.ID || accepted.Kind != domain.PlanSnapshotKindUserAccepted {
		t.Fatalf("accepted snapshot/version: plan=%+v snapshot=%+v", updated, accepted)
	}
	if audit.Action != domain.PlanDriftAuditSnapshotUpdated || audit.OriginalSnapshotID == nil || *audit.OriginalSnapshotID != baseline.ID || audit.NewSnapshotID == nil || *audit.NewSnapshotID != accepted.ID || audit.Channel != "desktop" || audit.Reason == "" {
		t.Fatalf("snapshot audit: %+v", audit)
	}
	history, err := store.ListPlanExecutionSnapshots(ctx, plan.ID)
	if err != nil || len(history) != 2 || history[0].ID != baseline.ID || history[1].ID != accepted.ID {
		t.Fatalf("snapshot history=%+v err=%v", history, err)
	}
	if _, err = store.Pool.Exec(ctx, `UPDATE plan_execution_snapshots SET requirement_digest=$2 WHERE id=$1`, baseline.ID, strings.Repeat("0", 64)); err == nil {
		t.Fatal("immutable baseline unexpectedly allowed an update")
	}
}

func TestSuccessfulTaskCreatesOrderedExecutionCheckpoint(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	_, plan, tasks := createReadyPlanFixture(t, store, "/tmp/execution-checkpoint")
	if _, err := store.QueuePlan(ctx, plan.ID, plan.Version); err != nil {
		t.Fatal(err)
	}
	started, err := store.StartTask(ctx, tasks[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.FinishTask(ctx, started, "session-checkpoint", true, "done"); err != nil {
		t.Fatal(err)
	}
	history, err := store.ListPlanExecutionSnapshots(ctx, plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 2 || history[0].Kind != domain.PlanSnapshotKindGenerationBaseline || history[1].Kind != domain.PlanSnapshotKindTaskCheckpoint || history[1].Sequence != 2 || history[1].TaskID == nil || *history[1].TaskID != tasks[0].ID || history[1].PreviousSnapshotID == nil || *history[1].PreviousSnapshotID != history[0].ID {
		t.Fatalf("checkpoint history: %+v", history)
	}
	var summary map[string]any
	if err = json.Unmarshal(history[1].ChangeSummary, &summary); err != nil {
		t.Fatal(err)
	}
	metadata, _ := summary["gitCheckpoint"].(map[string]any)
	if metadata["available"] != false || strings.TrimSpace(fmt.Sprint(metadata["reason"])) == "" || summary["diffAvailable"] != false || len(history[1].Files) != 0 {
		t.Fatalf("non-Git checkpoint summary=%+v files=%+v", summary, history[1].Files)
	}
}

func TestFinalValidationFailurePersistsBoundedTaskSummaryAndAgentRunExitEvidence(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	project, plan, tasks := createReadyPlanFixture(t, store, "/tmp/bounded-validation-failure")
	if _, err := store.QueuePlan(ctx, plan.ID, plan.Version); err != nil {
		t.Fatal(err)
	}
	implementation, err := store.StartTask(ctx, tasks[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.FinishTask(ctx, implementation, "implementation-session", true, "implementation complete"); err != nil {
		t.Fatal(err)
	}
	taskList, err := store.ListTasks(ctx, plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	var validation domain.PlanTask
	for _, task := range taskList {
		if task.TaskType == domain.PlanTaskTypeFinalValidation {
			validation = task
			break
		}
	}
	if validation.ID == uuid.Nil || validation.Status != "queued" {
		t.Fatalf("final validation task=%+v", validation)
	}
	validation, err = store.StartTask(ctx, validation.ID)
	if err != nil {
		t.Fatal(err)
	}
	var jobID uuid.UUID
	if err = store.Pool.QueryRow(ctx, `SELECT id FROM jobs WHERE aggregate_type='task' AND aggregate_id=$1 ORDER BY created_at DESC LIMIT 1`, validation.ID).Scan(&jobID); err != nil {
		t.Fatal(err)
	}
	runID := uuid.New()
	if err = store.StartAgentRun(ctx, AgentRunStart{
		ID: runID, ProjectID: project.ID, PlanID: &plan.ID, JobID: &jobID, TaskID: &validation.ID,
		Provider: "codex", OperationType: domain.AgentRunOperationValidation, CommandSummary: "go test ./...", LogPath: "/tmp/bounded-validation-failure.log", OwnerInstanceID: "test-worker",
	}); err != nil {
		t.Fatal(err)
	}
	exitCode := 2
	durationMS, outputBytes, outputLines, eventCount := int64(1234), int64(2_000_000), int64(50_000), int64(250)
	outputTruncated := true
	if err = store.FinishAgentRunWithDetails(ctx, runID, AgentRunFinish{
		Status: "failed", ExitCode: &exitCode, TerminationReason: "validation command exited with status 2", FailureCategory: "validation_failed",
		DurationMS: &durationMS, OutputBytes: &outputBytes, OutputLines: &outputLines, EventCount: &eventCount, OutputTruncated: &outputTruncated,
	}); err != nil {
		t.Fatal(err)
	}
	longMessage := strings.Repeat(strings.Repeat("validation failure detail ", 8)+"\n", 400)
	if err = store.FinishTask(ctx, validation, "validation-session", false, longMessage); err != nil {
		t.Fatal(err)
	}

	failedTask, err := store.GetTask(ctx, validation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if failedTask.Status != "failed" || failedTask.AcceptanceStatus != domain.AcceptanceStatusFailed {
		t.Fatalf("failed validation task=%+v", failedTask)
	}
	var acceptance map[string]any
	if err = json.Unmarshal(failedTask.AcceptanceResult, &acceptance); err != nil {
		t.Fatal(err)
	}
	reason, _ := acceptance["reason"].(string)
	if reason == "" || len(reason) > maxTaskResultSummaryBytes || strings.Count(reason, "\n") > maxTaskResultSummaryLines || reason == longMessage {
		t.Fatalf("acceptance failure summary was not bounded: bytes=%d lines=%d", len(reason), strings.Count(reason, "\n"))
	}

	events, err := store.ListEvents(ctx, &project.ID, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	var failurePayload map[string]any
	for _, event := range events {
		if event.Type == "task.failed" && event.AggregateID == validation.ID {
			if err = json.Unmarshal(event.Payload, &failurePayload); err != nil {
				t.Fatal(err)
			}
		}
	}
	message, _ := failurePayload["message"].(string)
	if failurePayload["summaryTruncated"] != true || failurePayload["finalValidation"] != true || failurePayload["success"] != false || message != reason {
		t.Fatalf("task failure event payload=%+v acceptance reason bytes=%d", failurePayload, len(reason))
	}
	transitions, err := store.ListLifecycleTransitions(ctx, domain.LifecycleResourceTask, validation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(transitions) == 0 || transitions[len(transitions)-1].ToStatus != "failed" || transitions[len(transitions)-1].Reason != reason {
		t.Fatalf("task failure lifecycle transitions=%+v", transitions)
	}
	run, err := store.GetAgentRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != "failed" || run.ExitCode == nil || *run.ExitCode != exitCode || run.FailureCategory == nil || *run.FailureCategory != "validation_failed" || run.OutputBytes == nil || *run.OutputBytes != outputBytes || run.OutputLines == nil || *run.OutputLines != outputLines || run.OutputTruncated == nil || !*run.OutputTruncated || run.TerminationReason == nil || strings.TrimSpace(*run.TerminationReason) == "" {
		t.Fatalf("AgentRun failure evidence=%+v", run)
	}
}

func TestLegacyPlanWithoutBaselineReportsMissingDriftAndCannotQueue(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	project, err := store.CreateProject(ctx, CreateProjectParams{Name: "Legacy", WorkspacePath: "/tmp/legacy-plan", NormalizedWorkspace: "/tmp/legacy-plan"})
	if err != nil {
		t.Fatal(err)
	}
	intake, job, err := store.CreateIntake(ctx, CreateIntakeParams{ProjectID: project.ID, Kind: "requirement", Title: "Legacy", Body: "Created before snapshots", ConfigSnapshot: json.RawMessage(`{}`), QueuePlan: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Pool.Exec(ctx, `UPDATE jobs SET status='cancelled' WHERE id=$1`, job.ID); err != nil {
		t.Fatal(err)
	}
	planID, taskID := uuid.New(), uuid.New()
	if _, err = store.Pool.Exec(ctx, `INSERT INTO plans(id,project_id,intake_id,title,spec,markdown,status,config_snapshot) VALUES($1,$2,$3,'Legacy plan','{}','legacy','ready','{}')`, planID, project.ID, intake.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Pool.Exec(ctx, `INSERT INTO plan_tasks(id,project_id,plan_id,task_key,position,title) VALUES($1,$2,$3,'P001',1,'Legacy task')`, taskID, project.ID, planID); err != nil {
		t.Fatal(err)
	}
	legacy, err := store.GetPlan(ctx, planID)
	if err != nil {
		t.Fatal(err)
	}
	if legacy.DriftStatus != domain.PlanDriftStatusMissingBaseline || !legacy.DriftResolutionRequired || legacy.ExecutionSnapshotID != nil || legacy.ContentVersion != 1 {
		t.Fatalf("legacy drift state: %+v", legacy)
	}
	if _, err = store.QueuePlan(ctx, planID, legacy.Version); !errors.Is(err, domain.ErrPlanExecutionBaselineMissing) || !errors.Is(err, domain.ErrPlanDriftResolutionRequired) {
		t.Fatalf("legacy queue error=%v", err)
	}
	after, err := store.GetPlan(ctx, planID)
	if err != nil || after.Status != "ready" || after.Version != legacy.Version {
		t.Fatalf("legacy plan mutated: %+v err=%v", after, err)
	}
}

func TestStoppingPlanCreatesImmutableAbandonmentAudit(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	_, plan, _ := createReadyPlanFixture(t, store, "/tmp/abandon-audit")
	if _, _, err := store.StopPlan(ctx, plan.ID, plan.Version); err != nil {
		t.Fatal(err)
	}
	audits, err := store.ListPlanDriftAudits(ctx, plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(audits) != 1 || audits[0].Action != domain.PlanDriftAuditExecutionAbandoned || audits[0].TargetPlanID == nil || *audits[0].TargetPlanID != plan.ID || audits[0].Channel == "" || audits[0].Reason == "" || !json.Valid(audits[0].RawDiff) {
		t.Fatalf("abandonment audits: %+v", audits)
	}
	if _, err = store.Pool.Exec(ctx, `UPDATE plan_drift_audits SET reason='changed' WHERE id=$1`, audits[0].ID); err == nil {
		t.Fatal("immutable audit unexpectedly allowed an update")
	}
}

func TestRegeneratedPlanAuditsOriginalSnapshotAndTargetPlan(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	_, original, _ := createReadyPlanFixture(t, store, "/tmp/regeneration-audit")
	intake, err := store.GetIntake(ctx, original.IntakeID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Pool.Exec(ctx, `UPDATE intakes SET status='planning',version=version+1,updated_at=now() WHERE id=$1`, intake.ID); err != nil {
		t.Fatal(err)
	}
	intake, err = store.GetIntake(ctx, intake.ID)
	if err != nil {
		t.Fatal(err)
	}
	regeneratedSpec := planspec.Spec{Title: "Regenerated", Summary: "Replace original plan", Tasks: []planspec.Task{{Title: "Reimplement", Scope: []string{"backend"}, Acceptance: []string{"passes"}}}, FinalValidation: []string{"tests pass"}}
	regenerated, _, err := store.SaveGeneratedPlan(ctx, intake, regeneratedSpec, planspec.Render(regeneratedSpec))
	if err != nil {
		t.Fatal(err)
	}
	audits, err := store.ListPlanDriftAudits(ctx, original.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(audits) != 1 || audits[0].Action != domain.PlanDriftAuditPlanRegenerated || audits[0].OriginalSnapshotID == nil || audits[0].TargetPlanID == nil || *audits[0].TargetPlanID != regenerated.ID || audits[0].Channel == "" || audits[0].Reason == "" || !json.Valid(audits[0].RawDiff) {
		t.Fatalf("regeneration audits: %+v", audits)
	}
}

func TestAgentRunObservabilityCoversSessionsRetriesRecoveryAggregationAndPrivacy(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	project, plan, tasks := createReadyPlanFixture(t, store, "/private/workspaces/acme-secret")
	if _, err := store.Pool.Exec(ctx, `UPDATE intakes SET title='Safe requirement label' WHERE id=$1`, plan.IntakeID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pool.Exec(ctx, `UPDATE plans SET title='Safe plan label' WHERE id=$1`, plan.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pool.Exec(ctx, `UPDATE plan_tasks SET title='Safe task label' WHERE id=$1`, tasks[0].ID); err != nil {
		t.Fatal(err)
	}

	type runFixture struct {
		id, logicalID                 uuid.UUID
		provider, operation, session  string
		invalidation, status, failure string
		attempt, retry                int
		started                       time.Time
		duration, queue               *int64
		input, output, total          *int64
		cost, currency                string
		taskID                        *uuid.UUID
	}
	millis := func(value int64) *int64 { return &value }
	tokens := func(value int64) *int64 { return &value }
	base := time.Date(2026, 7, 15, 1, 0, 0, 0, time.UTC)
	planOperation, taskRetryOperation := uuid.New(), uuid.New()
	cancelOperation, crashOperation := uuid.New(), uuid.New()
	fixtures := []runFixture{
		{id: uuid.New(), logicalID: planOperation, provider: "codex", operation: domain.AgentRunOperationPlanGeneration, session: domain.AgentRunSessionModeNew, status: "failed", failure: "provider_error", attempt: 1, retry: 0, started: base, duration: nil, queue: millis(25)},
		{id: uuid.New(), logicalID: planOperation, provider: "codex", operation: domain.AgentRunOperationPlanGeneration, session: domain.AgentRunSessionModeReused, status: "succeeded", attempt: 2, retry: 1, started: base.Add(time.Minute), duration: millis(1200), queue: millis(50), input: tokens(0), output: tokens(0), total: tokens(0), cost: "0", currency: "USD"},
		{id: uuid.New(), logicalID: taskRetryOperation, provider: "codex", operation: domain.AgentRunOperationTaskExecution, session: domain.AgentRunSessionModeNew, status: "failed", failure: "non_zero_exit", attempt: 1, retry: 0, started: base.Add(2 * time.Minute), duration: millis(800), queue: millis(75), input: tokens(10), output: tokens(5), total: tokens(15), cost: "1", currency: "USD", taskID: &tasks[0].ID},
		{id: uuid.New(), logicalID: taskRetryOperation, provider: "claude", operation: domain.AgentRunOperationTaskExecution, session: domain.AgentRunSessionModeSnapshotRestored, invalidation: domain.AgentRunSessionInvalidationProviderSwitched, status: "succeeded", attempt: 2, retry: 1, started: base.Add(3 * time.Minute), duration: millis(900), queue: millis(100), input: tokens(20), output: tokens(10), total: tokens(30), cost: "2", currency: "CNY", taskID: &tasks[0].ID},
		{id: uuid.New(), logicalID: cancelOperation, provider: "claude", operation: domain.AgentRunOperationTaskExecution, session: domain.AgentRunSessionModeReused, status: "cancelled", failure: "cancellation", attempt: 1, retry: 0, started: base.Add(4 * time.Minute), duration: millis(400), queue: nil, taskID: &tasks[0].ID},
		{id: uuid.New(), logicalID: crashOperation, provider: "codex", operation: domain.AgentRunOperationTaskExecution, session: domain.AgentRunSessionModeReused, invalidation: domain.AgentRunSessionInvalidationSessionNotFound, status: "interrupted", failure: "interrupted", attempt: 1, retry: 0, started: base.Add(5 * time.Minute), duration: millis(500), queue: millis(125), taskID: &tasks[0].ID},
	}

	for index, fixture := range fixtures {
		command := "codex exec --model secret --token sk-proj-0123456789abcdef /private/workspaces/acme-secret"
		logPath := fmt.Sprintf("/private/logs/run-%d.log", index)
		sessionID := fmt.Sprintf("session-full-secret-%02d-abcdef0123456789", index)
		termination := "log body: BEGIN PRIVATE LOG; source: password := hardcodedSecret"
		_, err := store.Pool.Exec(ctx, `INSERT INTO agent_runs(
			id,project_id,intake_id,plan_id,task_id,logical_operation_id,operation_type,
			job_attempt,retry_count,provider,command_summary,session_id,session_mode,
			session_invalidation_reason,status,queue_wait_ms,duration_ms,failure_category,
			input_tokens,output_tokens,total_tokens,cost_amount,cost_currency,log_path,
			termination_reason,started_at,finished_at
		) VALUES(
			$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,nullif($14,''),$15,$16,$17,
			nullif($18,''),$19,$20,$21,nullif($22,'')::numeric,nullif($23,''),$24,$25,$26::timestamptz,$26::timestamptz+interval '1 second'
		)`, fixture.id, project.ID, plan.IntakeID, plan.ID, fixture.taskID, fixture.logicalID,
			fixture.operation, fixture.attempt, fixture.retry, fixture.provider, command, sessionID,
			fixture.session, fixture.invalidation, fixture.status, fixture.queue, fixture.duration,
			fixture.failure, fixture.input, fixture.output, fixture.total, fixture.cost, fixture.currency,
			logPath, termination, fixture.started)
		if err != nil {
			t.Fatalf("insert fixture %d: %v", index, err)
		}
	}

	other, err := store.CreateProject(ctx, CreateProjectParams{Name: "Other", WorkspacePath: "/other", NormalizedWorkspace: "/other"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Pool.Exec(ctx, `INSERT INTO agent_runs(id,project_id,provider,command_summary,status,log_path,started_at,finished_at) VALUES($1,$2,'codex','other project','succeeded','/other.log',$3,$3)`, uuid.New(), other.ID, base.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}

	result, err := store.QueryAgentRunObservability(ctx, AgentRunObservabilityFilter{ProjectID: project.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Runs) != len(fixtures) || len(result.Requirements) != 1 || len(result.Plans) != 1 || len(result.Tasks) != 1 {
		t.Fatalf("associations runs=%d requirements=%d plans=%d tasks=%d", len(result.Runs), len(result.Requirements), len(result.Plans), len(result.Tasks))
	}
	byID := make(map[uuid.UUID]ObservabilityAgentRun, len(result.Runs))
	for _, run := range result.Runs {
		byID[run.ID] = run
	}
	for _, fixture := range fixtures {
		run, ok := byID[fixture.id]
		if !ok {
			t.Fatalf("missing run %s", fixture.id)
		}
		if run.RequirementID == nil || *run.RequirementID != plan.IntakeID || run.PlanID == nil || *run.PlanID != plan.ID ||
			run.LogicalOperationID == nil || *run.LogicalOperationID != fixture.logicalID || run.OperationType == nil || *run.OperationType != fixture.operation ||
			run.SessionMode == nil || *run.SessionMode != fixture.session || run.JobAttempt == nil || *run.JobAttempt != fixture.attempt ||
			run.RetryCount == nil || *run.RetryCount != fixture.retry || run.Provider != fixture.provider || run.Status != fixture.status {
			t.Fatalf("run classification/relationship mismatch: %+v fixture=%+v", run, fixture)
		}
		if fixture.taskID != nil && (run.TaskID == nil || *run.TaskID != *fixture.taskID) {
			t.Fatalf("task relationship mismatch: %+v", run)
		}
		if fixture.duration == nil && run.DurationMS != nil || fixture.duration != nil && (run.DurationMS == nil || *run.DurationMS != *fixture.duration) {
			t.Fatalf("duration mismatch: %+v fixture=%+v", run, fixture)
		}
		if fixture.failure == "" && run.FailureCategory != nil || fixture.failure != "" && (run.FailureCategory == nil || *run.FailureCategory != fixture.failure) {
			t.Fatalf("failure mismatch: %+v fixture=%+v", run, fixture)
		}
	}

	switched, err := store.GetAgentRun(ctx, fixtures[3].id)
	if err != nil {
		t.Fatal(err)
	}
	if switched.SessionMode == nil || *switched.SessionMode != domain.AgentRunSessionModeSnapshotRestored || switched.SessionInvalidationReason == nil || *switched.SessionInvalidationReason != domain.AgentRunSessionInvalidationProviderSwitched || switched.Provider != "claude" {
		t.Fatalf("provider-switch snapshot restoration was not persisted: %+v", switched)
	}
	crashed, err := store.GetAgentRun(ctx, fixtures[5].id)
	if err != nil {
		t.Fatal(err)
	}
	if crashed.Status != "interrupted" || crashed.FailureCategory == nil || *crashed.FailureCategory != "interrupted" || crashed.SessionInvalidationReason == nil || *crashed.SessionInvalidationReason != domain.AgentRunSessionInvalidationSessionNotFound {
		t.Fatalf("crash recovery classification was not persisted: %+v", crashed)
	}

	aggregates := result.Aggregates
	assertRate := func(name string, rate ObservabilityRate, numerator, denominator int) {
		t.Helper()
		if !rate.Available || rate.Numerator != numerator || rate.Denominator != denominator || rate.Value == nil || *rate.Value != float64(numerator)/float64(denominator) {
			t.Fatalf("%s=%+v, want %d/%d", name, rate, numerator, denominator)
		}
	}
	assertRate("session reuse", aggregates.SessionReuseRate, 3, 6)
	assertRate("snapshot restore", aggregates.SnapshotRestoreRate, 1, 6)
	assertRate("plan generation success", aggregates.PlanGenerationSuccessRate, 1, 1)
	assertRate("task execution success", aggregates.TaskExecutionSuccessRate, 1, 3)
	if fmt.Sprint(aggregates.FailureCategories) != "[{cancellation 1} {interrupted 1}]" {
		t.Fatalf("retry failures were not deduplicated by logical operation: %+v", aggregates.FailureCategories)
	}
	usage := aggregates.Usage.Overall
	if !usage.Tokens.Available || usage.Tokens.CoverageCount != 3 || usage.Tokens.TotalRunCount != 6 || usage.Tokens.TotalTokens == nil || *usage.Tokens.TotalTokens != 45 {
		t.Fatalf("token aggregation=%+v", usage.Tokens)
	}
	if !usage.Costs.Available || usage.Costs.CoverageCount != 3 || usage.Costs.TotalRunCount != 6 || len(usage.Costs.Currencies) != 2 ||
		usage.Costs.Currencies[0].Currency != "CNY" || usage.Costs.Currencies[0].Amount != "2" || usage.Costs.Currencies[0].CoverageCount != 1 ||
		usage.Costs.Currencies[1].Currency != "USD" || usage.Costs.Currencies[1].Amount != "1" || usage.Costs.Currencies[1].CoverageCount != 2 {
		t.Fatalf("currency aggregation=%+v", usage.Costs)
	}
	if len(aggregates.DurationTrend) != 1 || aggregates.DurationTrend[0].RunCount != 6 || aggregates.DurationTrend[0].RunDuration.CoverageCount != 5 || aggregates.DurationTrend[0].RunDuration.TotalMS != 3800 || aggregates.DurationTrend[0].QueueWait.CoverageCount != 5 {
		t.Fatalf("duration trend=%+v", aggregates.DurationTrend)
	}

	from, to := fixtures[1].started, fixtures[4].started
	bounded, err := store.QueryAgentRunObservability(ctx, AgentRunObservabilityFilter{ProjectID: project.ID, From: &from, To: &to})
	if err != nil || len(bounded.Runs) != 4 || bounded.Runs[0].ID != fixtures[4].id || bounded.Runs[3].ID != fixtures[1].id {
		t.Fatalf("inclusive boundary filter runs=%+v err=%v", bounded.Runs, err)
	}
	claude, err := store.QueryAgentRunObservability(ctx, AgentRunObservabilityFilter{ProjectID: project.ID, Provider: "claude", PlanID: &plan.ID})
	if err != nil || len(claude.Runs) != 2 {
		t.Fatalf("provider/plan filter runs=%+v err=%v", claude.Runs, err)
	}
	future := base.Add(24 * time.Hour)
	empty, err := store.QueryAgentRunObservability(ctx, AgentRunObservabilityFilter{ProjectID: project.ID, From: &future})
	if err != nil {
		t.Fatal(err)
	}
	if len(empty.Runs) != 0 || len(empty.Requirements) != 0 || len(empty.Plans) != 0 || len(empty.Tasks) != 0 ||
		empty.Aggregates.SessionReuseRate.Available || empty.Aggregates.PlanGenerationSuccessRate.Available ||
		empty.Aggregates.Usage.Overall.Tokens.Available || empty.Aggregates.Usage.Overall.Tokens.TotalTokens != nil ||
		empty.Aggregates.Usage.Overall.Costs.Available || len(empty.Aggregates.Usage.Overall.Costs.Currencies) != 0 {
		t.Fatalf("empty range fabricated data: %+v", empty)
	}

	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	payload := string(encoded)
	for _, forbidden := range []string{
		"/private/workspaces/acme-secret", "session-full-secret", "--model secret", "--token", "BEGIN PRIVATE LOG",
		"password := hardcodedSecret", "sk-proj-0123456789abcdef", "/private/logs/", "commandSummary", "sessionId", "terminationReason", "logPath",
	} {
		if strings.Contains(payload, forbidden) {
			t.Fatalf("observability query leaked %q: %s", forbidden, payload)
		}
	}
}
