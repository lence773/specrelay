package repository_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lyming99/specrelay/backend/internal/agent"
	"github.com/lyming99/specrelay/backend/internal/app"
	"github.com/lyming99/specrelay/backend/internal/domain"
	"github.com/lyming99/specrelay/backend/internal/httpapi"
	"github.com/lyming99/specrelay/backend/internal/mcpapi"
	"github.com/lyming99/specrelay/backend/internal/migrations"
	"github.com/lyming99/specrelay/backend/internal/planspec"
	"github.com/lyming99/specrelay/backend/internal/repository"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	testBrowserToken = "integration-browser-token"
	testMCPToken     = "integration-mcp-token"
)

type bearerTransport struct {
	base  http.RoundTripper
	token string
}

func (t bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.Header = req.Header.Clone()
	clone.Header.Set("Authorization", "Bearer "+t.token)
	return t.base.RoundTrip(clone)
}

func TestRESTAndMCPShareApplicationStateAndEvents(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err = migrations.Run(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `TRUNCATE runtime_instances,access_tokens,agent_runs,events,workspace_leases,jobs,plan_tasks,plans,attachments,intakes,project_settings,projects RESTART IDENTITY CASCADE`); err != nil {
		t.Fatal(err)
	}

	store := repository.New(pool)
	service := app.New(store, agent.NewRunner(), t.TempDir(), 30*time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))
	auth, _ := httpapi.NewAuth(testBrowserToken, testMCPToken)
	api := &httpapi.Server{
		Store:  store,
		App:    service,
		Auth:   auth,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		MCP:    mcpapi.Handler(service, store),
	}
	httpServer := httptest.NewServer(api.Handler())
	t.Cleanup(httpServer.Close)

	restWorkspace := filepath.Join(t.TempDir(), "rest-workspace")
	mcpWorkspace := filepath.Join(t.TempDir(), "mcp-workspace")
	if err = os.MkdirAll(restWorkspace, 0o755); err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(mcpWorkspace, 0o755); err != nil {
		t.Fatal(err)
	}

	restProject := createRESTProject(t, httpServer.URL, restWorkspace)
	assertProjectPersistence(t, store, restProject, "REST project")
	restProject, err = store.SetAutomation(ctx, restProject.ID, true, restProject.Version)
	if err != nil {
		t.Fatal(err)
	}
	restSettingsBefore, err := store.GetProjectSettings(ctx, restProject.ID)
	if err != nil {
		t.Fatal(err)
	}
	restIntake := createRESTIntake(t, httpServer.URL, restProject.ID.String(), "claude")
	if restIntake.Job == nil {
		t.Fatal("REST provider-selected intake did not queue planning")
	}
	assertPayloadProvider(t, restIntake.Job.Payload, "claude", false)
	restSettingsAfter, err := store.GetProjectSettings(ctx, restProject.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertProviderSettingsUnchanged(t, restSettingsBefore, restSettingsAfter)

	client := mcp.NewClient(&mcp.Implementation{Name: "specrelay-integration-test", Version: "1.0.0"}, nil)
	httpClient := &http.Client{Transport: bearerTransport{base: http.DefaultTransport, token: testMCPToken}}
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:             httpServer.URL + "/mcp",
		HTTPClient:           httpClient,
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })

	created := callToolData[domain.Project](t, ctx, session, "projects_create", map[string]any{
		"name":          "MCP project",
		"description":   "created through official SDK",
		"workspacePath": mcpWorkspace,
	})
	assertProjectPersistence(t, store, created, "MCP project")
	mcpSettingsBefore, err := store.GetProjectSettings(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}

	started := callToolData[domain.Project](t, ctx, session, "automation_start", map[string]any{
		"projectId": created.ID.String(),
		"version":   created.Version,
	})
	if !started.AutomationEnabled || started.Version != created.Version+1 {
		t.Fatalf("unexpected started project: enabled=%v version=%d", started.AutomationEnabled, started.Version)
	}

	var intakeResult struct {
		Intake domain.Intake `json:"intake"`
		Job    *domain.Job   `json:"job"`
	}
	callToolDataInto(t, ctx, session, "intakes_create", map[string]any{
		"projectId": created.ID.String(),
		"kind":      "requirement",
		"title":     "MCP requirement",
		"body":      "Create a persisted planning job and matching events.",
	}, &intakeResult)
	if intakeResult.Intake.ProjectID != created.ID || intakeResult.Intake.Status != "planning" || intakeResult.Job == nil {
		t.Fatalf("unexpected MCP intake result: %+v job=%+v", intakeResult.Intake, intakeResult.Job)
	}
	if intakeResult.Job.ProjectID != created.ID || intakeResult.Job.Type != "plan.generate" || intakeResult.Job.Status != "queued" {
		t.Fatalf("unexpected MCP job: %+v", intakeResult.Job)
	}
	assertPayloadProvider(t, intakeResult.Job.Payload, "", false)

	planIntake, generationJob, err := store.CreateIntake(ctx, repository.CreateIntakeParams{ProjectID: created.ID, Kind: "requirement", Title: "MCP plan run", Body: "Verify MCP uses the project default", ConfigSnapshot: json.RawMessage(`{"executionAgentProvider":"claude"}`), QueuePlan: true})
	if err != nil || generationJob == nil {
		t.Fatalf("create MCP plan fixture: job=%v err=%v", generationJob, err)
	}
	if _, err = pool.Exec(ctx, `UPDATE jobs SET status='cancelled' WHERE id=$1`, generationJob.ID); err != nil {
		t.Fatal(err)
	}
	spec := planspec.Spec{Title: "MCP default plan", Summary: "Default provider", Tasks: []planspec.Task{{Title: "Implement", Scope: []string{"backend"}, Acceptance: []string{"passes"}}}, FinalValidation: []string{"tests pass"}}
	plan, _, err := store.SaveGeneratedPlan(ctx, planIntake, spec, planspec.Render(spec))
	if err != nil {
		t.Fatal(err)
	}
	mcpPlanJob := callToolData[domain.Job](t, ctx, session, "plan_run", map[string]any{"planId": plan.ID.String(), "version": plan.Version})
	assertPayloadProvider(t, mcpPlanJob.Payload, "", true)
	persistedPlan, err := store.GetPlan(ctx, plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	var planSnapshot map[string]any
	if err = json.Unmarshal(persistedPlan.ConfigSnapshot, &planSnapshot); err != nil {
		t.Fatal(err)
	}
	if provider, ok := planSnapshot["executionAgentProvider"]; ok {
		t.Fatalf("MCP default run retained stale plan provider %v in %s", provider, persistedPlan.ConfigSnapshot)
	}
	mcpSettingsAfter, err := store.GetProjectSettings(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertProviderSettingsUnchanged(t, mcpSettingsBefore, mcpSettingsAfter)

	stopped := callToolData[domain.Project](t, ctx, session, "automation_stop", map[string]any{
		"projectId": created.ID.String(),
		"version":   started.Version,
	})
	if stopped.AutomationEnabled || stopped.Version != started.Version+1 {
		t.Fatalf("unexpected stopped project: enabled=%v version=%d", stopped.AutomationEnabled, stopped.Version)
	}

	persistedIntake, err := store.GetIntake(ctx, intakeResult.Intake.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persistedIntake.Status != "open" {
		t.Fatalf("automation stop should restore queued planning intake to open, got %q", persistedIntake.Status)
	}
	var jobStatus string
	if err = pool.QueryRow(ctx, `SELECT status FROM jobs WHERE id=$1`, intakeResult.Job.ID).Scan(&jobStatus); err != nil {
		t.Fatal(err)
	}
	if jobStatus != "cancelled" {
		t.Fatalf("automation stop should cancel queued job, got %q", jobStatus)
	}

	events, err := store.ListEvents(ctx, &created.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	assertEventTypes(t, events, "project.created", "project.automation_started", "intake.created", "project.automation_stopped", "intake.plan_cancelled")
}

func TestFinalValidationUsesValidationProviderAndCommand(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err = migrations.Run(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `TRUNCATE runtime_instances,access_tokens,agent_runs,events,workspace_leases,jobs,plan_tasks,plans,attachments,intakes,project_settings,projects RESTART IDENTITY CASCADE`); err != nil {
		t.Fatal(err)
	}

	store := repository.New(pool)
	dataDir := t.TempDir()
	service := app.New(store, agent.NewRunner(), dataDir, 30*time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))
	workspace := t.TempDir()
	project, err := store.CreateProject(ctx, repository.CreateProjectParams{Name: "Validation provider", WorkspacePath: workspace, NormalizedWorkspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	settings, err := store.GetProjectSettings(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	validationMarker := filepath.Join(dataDir, "validation-ran")
	codexMarker := filepath.Join(dataDir, "codex-ran")
	claudeMarker := filepath.Join(dataDir, "claude-ran")
	codexCommand := filepath.Join(dataDir, "codex-command")
	claudeCommand := filepath.Join(dataDir, "claude-command")
	if err = os.WriteFile(codexCommand, []byte("#!/bin/sh\nprintf called > \""+codexMarker+"\"\nexit 99\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(claudeCommand, []byte("#!/bin/sh\nprintf called > \""+claudeMarker+"\"\nexit 99\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	settings.AgentProvider = agent.ProviderClaude
	settings.CodexCommand = codexCommand
	settings.CodexArgs = json.RawMessage(`[]`)
	settings.ClaudeCommand = claudeCommand
	settings.ClaudeArgs = json.RawMessage(`[]`)
	settings.ValidationCommand = "printf validation > \"" + validationMarker + "\""
	settings, err = store.UpdateProjectSettings(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	project, err = store.SetAutomation(ctx, project.ID, true, project.Version)
	if err != nil {
		t.Fatal(err)
	}
	intake, generationJob, err := store.CreateIntake(ctx, repository.CreateIntakeParams{ProjectID: project.ID, Kind: "requirement", Title: "Validate", Body: "Run final validation", ConfigSnapshot: json.RawMessage(`{}`), QueuePlan: true})
	if err != nil || generationJob == nil {
		t.Fatalf("create validation fixture: job=%v err=%v", generationJob, err)
	}
	if _, err = pool.Exec(ctx, `UPDATE jobs SET status='cancelled' WHERE id=$1`, generationJob.ID); err != nil {
		t.Fatal(err)
	}
	spec := planspec.Spec{Title: "Validation plan", Summary: "Exercise final validation", Tasks: []planspec.Task{{Title: "Implement", Scope: []string{"backend"}, Acceptance: []string{"passes"}}}, FinalValidation: []string{"validation command passes"}}
	plan, tasks, err := store.SaveGeneratedPlan(ctx, intake, spec, planspec.Render(spec))
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 2 || tasks[1].Title != "Final validation" {
		t.Fatalf("tasks=%+v", tasks)
	}
	firstJob, err := store.QueuePlan(repository.WithExecutionProvider(ctx, agent.ProviderCodex), plan.ID, plan.Version)
	if err != nil {
		t.Fatal(err)
	}
	if firstJob.AggregateID != tasks[0].ID {
		t.Fatalf("first job=%+v", firstJob)
	}
	firstTask, err := store.StartTask(ctx, tasks[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.FinishTask(ctx, firstTask, "", true, "done"); err != nil {
		t.Fatal(err)
	}

	validationJob := domain.Job{}
	if err = pool.QueryRow(ctx, `SELECT id,project_id,job_type,aggregate_type,aggregate_id,payload,attempt,max_attempts FROM jobs WHERE aggregate_id=$1 AND status='queued'`, tasks[1].ID).Scan(&validationJob.ID, &validationJob.ProjectID, &validationJob.Type, &validationJob.AggregateType, &validationJob.AggregateID, &validationJob.Payload, &validationJob.Attempt, &validationJob.MaxAttempts); err != nil {
		t.Fatal(err)
	}
	if err = service.ExecuteJob(ctx, "validation-worker", validationJob); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(validationMarker)
	if err != nil || string(content) != "validation" {
		t.Fatalf("validation marker=%q err=%v", content, err)
	}
	for provider, markerPath := range map[string]string{agent.ProviderCodex: codexMarker, agent.ProviderClaude: claudeMarker} {
		if _, statErr := os.Stat(markerPath); !os.IsNotExist(statErr) {
			t.Fatalf("final validation invoked %s command: %v", provider, statErr)
		}
	}
	runs, err := store.ListAgentRuns(ctx, project.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	var validationRun *domain.AgentRun
	for index := range runs {
		if runs[index].TaskID != nil && *runs[index].TaskID == tasks[1].ID {
			validationRun = &runs[index]
			break
		}
	}
	if validationRun == nil || validationRun.Provider != "validation" || validationRun.CommandSummary != settings.ValidationCommand || validationRun.Status != "succeeded" {
		t.Fatalf("validation run=%+v", validationRun)
	}
}

type intakeJobResponse struct {
	Intake domain.Intake `json:"intake"`
	Job    *domain.Job   `json:"job"`
}

func createRESTIntake(t *testing.T, baseURL, projectID, provider string) intakeJobResponse {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"kind":     "requirement",
		"title":    "REST provider requirement",
		"body":     "Persist the selected provider before workers can claim the job.",
		"provider": provider,
	})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, baseURL+"/api/v1/projects/"+projectID+"/intakes", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testBrowserToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		payload, _ := io.ReadAll(resp.Body)
		t.Fatalf("REST create intake status=%d body=%s", resp.StatusCode, payload)
	}
	var result intakeJobResponse
	if err = json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	return result
}

func assertProviderSettingsUnchanged(t *testing.T, before, after domain.ProjectSettings) {
	t.Helper()
	if after.AgentProvider != before.AgentProvider || after.Version != before.Version {
		t.Fatalf("project provider settings changed: before provider=%q version=%d, after provider=%q version=%d", before.AgentProvider, before.Version, after.AgentProvider, after.Version)
	}
}

func assertPayloadProvider(t *testing.T, raw json.RawMessage, provider string, requested bool) {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("decode payload: %v (%s)", err, raw)
	}
	value, ok := payload["provider"]
	if provider == "" {
		if ok {
			t.Fatalf("unexpected provider %v in payload %s", value, raw)
		}
	} else if !ok || value != provider {
		t.Fatalf("provider=%v, want %q in payload %s", value, provider, raw)
	}
	requestedValue, hasRequested := payload["providerRequested"].(bool)
	if requested {
		if !hasRequested || !requestedValue {
			t.Fatalf("providerRequested=%v, want true in payload %s", payload["providerRequested"], raw)
		}
	} else if _, ok = payload["providerRequested"]; ok {
		t.Fatalf("unexpected providerRequested in payload %s", raw)
	}
}

func createRESTProject(t *testing.T, baseURL, workspace string) domain.Project {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"name":          "REST project",
		"description":   "created through REST",
		"workspacePath": workspace,
	})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, baseURL+"/api/v1/projects", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testBrowserToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		payload, _ := io.ReadAll(resp.Body)
		t.Fatalf("REST create project status=%d body=%s", resp.StatusCode, payload)
	}
	var project domain.Project
	if err = json.NewDecoder(resp.Body).Decode(&project); err != nil {
		t.Fatal(err)
	}
	return project
}

func assertProjectPersistence(t *testing.T, store *repository.Store, want domain.Project, name string) {
	t.Helper()
	got, err := store.GetProject(context.Background(), want.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != name || got.WorkspacePath != want.WorkspacePath || got.Version != want.Version {
		t.Fatalf("persisted project mismatch: got=%+v want=%+v", got, want)
	}
	settings, err := store.GetProjectSettings(context.Background(), want.ID)
	if err != nil {
		t.Fatal(err)
	}
	if settings.ProjectID != want.ID || settings.AgentProvider != "codex" {
		t.Fatalf("default settings mismatch: %+v", settings)
	}
	events, err := store.ListEvents(context.Background(), &want.ID, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	assertEventTypes(t, events, "project.created")
}

func callToolData[T any](t *testing.T, ctx context.Context, session *mcp.ClientSession, name string, arguments map[string]any) T {
	t.Helper()
	var value T
	callToolDataInto(t, ctx, session, name, arguments, &value)
	return value
}

func callToolDataInto(t *testing.T, ctx context.Context, session *mcp.ClientSession, name string, arguments map[string]any, target any) {
	t.Helper()
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: arguments})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		content, marshalErr := json.Marshal(result.Content)
		if marshalErr != nil {
			t.Fatalf("encode %s error response: %v", name, marshalErr)
		}
		t.Fatalf("tool %s returned an error: %s", name, content)
	}
	raw, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if err = json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("decode %s envelope: %v (%s)", name, err, raw)
	}
	if err = json.Unmarshal(envelope.Data, target); err != nil {
		t.Fatalf("decode %s data: %v (%s)", name, err, envelope.Data)
	}
}

func assertEventTypes(t *testing.T, events []domain.Event, expected ...string) {
	t.Helper()
	seen := make(map[string]bool, len(events))
	for _, event := range events {
		seen[event.Type] = true
	}
	for _, eventType := range expected {
		if !seen[eventType] {
			t.Fatalf("missing event type %q in %+v", eventType, seen)
		}
	}
}
