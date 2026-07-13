package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/lyming99/specrelay/backend/internal/agent"
	"github.com/lyming99/specrelay/backend/internal/domain"
	"github.com/lyming99/specrelay/backend/internal/planspec"
	"github.com/lyming99/specrelay/backend/internal/repository"
	"github.com/lyming99/specrelay/backend/internal/security"
)

type Service struct {
	Store         *repository.Store
	Runner        *agent.Runner
	DataDir       string
	LeaseDuration time.Duration
	Logger        *slog.Logger
}

func New(store *repository.Store, runner *agent.Runner, dataDir string, lease time.Duration, logger *slog.Logger) *Service {
	return &Service{Store: store, Runner: runner, DataDir: dataDir, LeaseDuration: lease, Logger: logger}
}
func (s *Service) CreateProject(ctx context.Context, name, description, workspace string) (domain.Project, error) {
	workspace = strings.TrimSpace(workspace)
	if workspace == "" {
		return domain.Project{}, errors.New("workspacePath is required")
	}
	absolute, err := filepath.Abs(workspace)
	if err != nil {
		return domain.Project{}, err
	}
	real, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return domain.Project{}, fmt.Errorf("workspace must exist: %w", err)
	}
	info, err := os.Stat(real)
	if err != nil || !info.IsDir() {
		return domain.Project{}, errors.New("workspace must be a directory")
	}
	return s.Store.CreateProject(ctx, repository.CreateProjectParams{Name: name, Description: description, WorkspacePath: absolute, NormalizedWorkspace: real})
}
func (s *Service) UpdateProject(ctx context.Context, id uuid.UUID, name, description, workspace string, version int64) (domain.Project, error) {
	absolute, err := filepath.Abs(workspace)
	if err != nil {
		return domain.Project{}, err
	}
	real, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return domain.Project{}, err
	}
	return s.Store.UpdateProject(ctx, id, repository.UpdateProjectParams{Name: name, Description: description, WorkspacePath: absolute, NormalizedWorkspace: real, Version: version})
}
func (s *Service) CreateIntake(ctx context.Context, p repository.CreateIntakeParams) (domain.Intake, *domain.Job, error) {
	return s.CreateIntakeWithProvider(ctx, p, "")
}

func (s *Service) CreateIntakeWithProvider(ctx context.Context, p repository.CreateIntakeParams, requestedProvider string) (domain.Intake, *domain.Job, error) {
	project, err := s.Store.GetProject(ctx, p.ProjectID)
	if err != nil {
		return domain.Intake{}, nil, err
	}
	settings, err := s.Store.GetProjectSettings(ctx, p.ProjectID)
	if err != nil {
		return domain.Intake{}, nil, err
	}
	if _, _, _, err = adapterFor(requestedProvider, settings); err != nil {
		return domain.Intake{}, nil, err
	}
	snapshot, _ := json.Marshal(settings)
	p.ConfigSnapshot = snapshot
	p.QueuePlan = project.AutomationEnabled
	intake, job, err := s.Store.CreateIntake(ctx, p)
	if err != nil || job == nil || strings.TrimSpace(requestedProvider) == "" {
		return intake, job, err
	}
	if err = s.setJobProvider(ctx, job, requestedProvider, false); err != nil {
		return domain.Intake{}, nil, err
	}
	return intake, job, nil
}

const planExecutionProviderKey = "executionAgentProvider"

func (s *Service) QueuePlanGeneration(ctx context.Context, intakeID uuid.UUID, version int64, requestedProvider string) (domain.Job, error) {
	intake, err := s.Store.GetIntake(ctx, intakeID)
	if err != nil {
		return domain.Job{}, err
	}
	if err = s.validateRequestedProvider(ctx, intake.ProjectID, requestedProvider); err != nil {
		return domain.Job{}, err
	}
	job, err := s.Store.QueuePlanGeneration(ctx, intakeID, version)
	if err != nil || strings.TrimSpace(requestedProvider) == "" {
		return job, err
	}
	if err = s.setJobProvider(ctx, &job, requestedProvider, false); err != nil {
		return domain.Job{}, err
	}
	return job, nil
}

func (s *Service) QueuePlan(ctx context.Context, planID uuid.UUID, version int64, requestedProvider string) (domain.Job, error) {
	plan, err := s.Store.GetPlan(ctx, planID)
	if err != nil {
		return domain.Job{}, err
	}
	if err = s.validateRequestedProvider(ctx, plan.ProjectID, requestedProvider); err != nil {
		return domain.Job{}, err
	}
	job, err := s.Store.QueuePlan(ctx, planID, version)
	if err != nil {
		return domain.Job{}, err
	}
	if err = s.setPlanAndJobProvider(ctx, plan, &job, requestedProvider); err != nil {
		return domain.Job{}, err
	}
	return job, nil
}

func (s *Service) QueueTask(ctx context.Context, taskID uuid.UUID, version int64, requestedProvider string) (domain.Job, error) {
	task, err := s.Store.GetTask(ctx, taskID)
	if err != nil {
		return domain.Job{}, err
	}
	if err = s.validateRequestedProvider(ctx, task.ProjectID, requestedProvider); err != nil {
		return domain.Job{}, err
	}
	job, err := s.Store.QueueTask(ctx, taskID, version)
	if err != nil {
		return domain.Job{}, err
	}
	if err = s.setJobProvider(ctx, &job, requestedProvider, true); err != nil {
		return domain.Job{}, err
	}
	return job, nil
}

func (s *Service) validateRequestedProvider(ctx context.Context, projectID uuid.UUID, requestedProvider string) error {
	settings, err := s.Store.GetProjectSettings(ctx, projectID)
	if err != nil {
		return err
	}
	_, _, _, err = adapterFor(requestedProvider, settings)
	return err
}

func jobPayloadWithProvider(raw json.RawMessage, requestedProvider string, requestScoped bool) (json.RawMessage, error) {
	payload := map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &payload); err != nil {
			return nil, fmt.Errorf("invalid job payload: %w", err)
		}
	}
	provider := strings.TrimSpace(requestedProvider)
	if provider == "" {
		delete(payload, "provider")
	} else {
		payload["provider"] = provider
	}
	if requestScoped {
		payload["providerRequested"] = true
	}
	return json.Marshal(payload)
}

func (s *Service) setJobProvider(ctx context.Context, job *domain.Job, requestedProvider string, requestScoped bool) error {
	payload, err := jobPayloadWithProvider(job.Payload, requestedProvider, requestScoped)
	if err != nil {
		return err
	}
	if _, err = s.Store.Pool.Exec(ctx, `UPDATE jobs SET payload=$2 WHERE id=$1`, job.ID, payload); err != nil {
		return err
	}
	job.Payload = payload
	return nil
}

func (s *Service) setPlanAndJobProvider(ctx context.Context, plan domain.Plan, job *domain.Job, requestedProvider string) error {
	snapshot := map[string]any{}
	if len(plan.ConfigSnapshot) > 0 {
		if err := json.Unmarshal(plan.ConfigSnapshot, &snapshot); err != nil {
			return fmt.Errorf("invalid plan config snapshot: %w", err)
		}
	}
	provider := strings.TrimSpace(requestedProvider)
	if provider == "" {
		delete(snapshot, planExecutionProviderKey)
	} else {
		snapshot[planExecutionProviderKey] = provider
	}
	snapshotJSON, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	payload, err := jobPayloadWithProvider(job.Payload, provider, true)
	if err != nil {
		return err
	}
	tx, err := s.Store.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `UPDATE plans SET config_snapshot=$2 WHERE id=$1`, plan.ID, snapshotJSON); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE jobs SET payload=$2 WHERE id=$1`, job.ID, payload); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	job.Payload = payload
	return nil
}

func (s *Service) SaveAttachment(ctx context.Context, intakeID uuid.UUID, header *multipart.FileHeader) (domain.Attachment, error) {
	intake, err := s.Store.GetIntake(ctx, intakeID)
	if err != nil {
		return domain.Attachment{}, err
	}
	src, err := header.Open()
	if err != nil {
		return domain.Attachment{}, err
	}
	defer src.Close()
	id := uuid.New()
	dir := filepath.Join(s.DataDir, "attachments", intake.ProjectID.String(), intake.ID.String())
	if err = os.MkdirAll(dir, 0o700); err != nil {
		return domain.Attachment{}, err
	}
	name := filepath.Base(header.Filename)
	path := filepath.Join(dir, id.String())
	policy, err := security.NewPathPolicy(filepath.Join(s.DataDir, "attachments"))
	if err != nil {
		return domain.Attachment{}, err
	}
	safePath, err := policy.Resolve(path)
	if err != nil {
		return domain.Attachment{}, err
	}
	dst, err := os.OpenFile(safePath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return domain.Attachment{}, err
	}
	hash := sha256.New()
	size, copyErr := io.Copy(io.MultiWriter(dst, hash), io.LimitReader(src, 50<<20+1))
	closeErr := dst.Close()
	if copyErr != nil || closeErr != nil {
		_ = os.Remove(safePath)
		return domain.Attachment{}, errors.Join(copyErr, closeErr)
	}
	if size > 50<<20 {
		_ = os.Remove(safePath)
		return domain.Attachment{}, errors.New("attachment exceeds 50 MiB")
	}
	a := domain.Attachment{ID: id, ProjectID: intake.ProjectID, IntakeID: intake.ID, OriginalName: name, MimeType: header.Header.Get("Content-Type"), SizeBytes: size, SHA256: hex.EncodeToString(hash.Sum(nil)), StoragePath: safePath}
	saved, err := s.Store.CreateAttachment(ctx, a)
	if err != nil {
		_ = os.Remove(safePath)
	}
	return saved, err
}

func (s *Service) ExecuteJob(ctx context.Context, workerID string, job domain.Job) error {
	project, err := s.Store.GetProject(ctx, job.ProjectID)
	if err != nil {
		return err
	}
	if !project.AutomationEnabled {
		return errors.New("project automation is disabled")
	}
	if err = s.Store.AcquireWorkspaceLease(ctx, project.ID, job.ID, project.WorkspacePath, workerID, s.LeaseDuration); err != nil {
		return Retryable(err)
	}
	defer s.Store.ReleaseWorkspaceLease(context.Background(), job.ID, workerID)
	switch job.Type {
	case "plan.generate":
		return s.generatePlan(ctx, job, project)
	case "task.execute":
		return s.executeTask(ctx, job, project)
	default:
		return fmt.Errorf("unsupported job type %q", job.Type)
	}
}
func (s *Service) generatePlan(ctx context.Context, job domain.Job, project domain.Project) error {
	intake, err := s.Store.GetIntake(ctx, job.AggregateID)
	if err != nil {
		return err
	}
	settings, err := s.Store.GetProjectSettings(ctx, project.ID)
	if err != nil {
		return err
	}
	adapter, command, args, err := adapterFor(providerFromJob(job.Payload), settings)
	if err != nil {
		return err
	}
	prompt := fmt.Sprintf(`You are planning implementation work for SpecRelay. First inspect the current workspace read-only to understand its architecture, existing behavior, and relevant files. Use local read-only shell commands such as find, grep, sed, and cat; do not use web search for workspace contents. Do not modify any files during planning.

Return ONLY a JSON object matching PlanSpec: {title, summary, tasks:[{title,scope,acceptance}], finalValidation}. Write title, summary, task titles, and acceptance criteria in Simplified Chinese. Scope entries must be real workspace-relative paths discovered from the project; do not invent paths.

Project: %s
Description: %s
Intake kind: %s
Title: %s
Body:
%s`, project.Name, project.Description, intake.Kind, intake.Title, intake.Body)
	logPath := filepath.Join(s.DataDir, "logs", job.ID.String()+".log")
	inv := adapter.GeneratePlan(command, args, project.WorkspacePath, prompt, 0, logPath)
	inv.Env = allowedEnv(settings.AllowedEnv)
	runID := uuid.New()
	_ = s.Store.StartAgentRun(ctx, repository.AgentRunStart{ID: runID, ProjectID: project.ID, JobID: &job.ID, Provider: adapter.Name(), CommandSummary: command, LogPath: logPath})
	finishOutput := s.instrumentInvocation(&inv, project.ID, &job.ID, runID, nil)
	result, runErr := s.Runner.Run(ctx, project.ID.String()+":"+job.ID.String(), inv)
	finishOutput()
	finishRun(s.Store, runID, result, runErr)
	if runErr != nil {
		return classifyRunError(result, runErr)
	}
	raw, err := agent.ExtractJSON(result.Output)
	if err != nil {
		return err
	}
	spec, err := planspec.Parse(raw)
	if err != nil {
		return err
	}
	plan, _, err := s.Store.SaveGeneratedPlan(ctx, intake, spec, planspec.Render(spec))
	if err != nil {
		return err
	}
	_, _, err = s.Store.QueuePlanAutomatically(ctx, plan.ID)
	return err
}
func (s *Service) executeTask(ctx context.Context, job domain.Job, project domain.Project) error {
	task, err := s.Store.GetTask(ctx, job.AggregateID)
	if err != nil {
		return err
	}
	settings, err := s.Store.GetProjectSettings(ctx, project.ID)
	if err != nil {
		return err
	}
	isValidation := task.Title == "Final validation"
	requestedProvider := ""
	if !isValidation {
		requestedProvider, err = s.requestedProviderForTask(ctx, job, task)
		if err != nil {
			return err
		}
		if _, _, _, err = adapterFor(requestedProvider, settings); err != nil {
			return err
		}
	}
	task, err = s.Store.StartTask(ctx, task.ID)
	if err != nil {
		return err
	}
	if isValidation && strings.TrimSpace(settings.ValidationCommand) == "" {
		return s.Store.FinishTask(ctx, task, "", true, "No validation command configured")
	}

	logPath := filepath.Join(s.DataDir, "logs", job.ID.String()+".log")
	prompt := ""
	if !isValidation {
		scope := []string{}
		acceptance := []string{}
		_ = json.Unmarshal(task.Scope, &scope)
		_ = json.Unmarshal(task.Acceptance, &acceptance)
		prompt = fmt.Sprintf("Implement exactly one SpecRelay task in the current workspace. Do not modify unrelated files.\nTask %s: %s\nScope:\n- %s\nAcceptance:\n- %s\nRun focused tests when useful, then summarize the changes.", task.TaskKey, task.Title, strings.Join(scope, "\n- "), strings.Join(acceptance, "\n- "))
	}
	inv, provider, commandSummary, err := taskInvocation(settings, requestedProvider, isValidation, project.WorkspacePath, prompt, task.ID.String(), task.SessionID, logPath)
	if err != nil {
		return err
	}
	if !isValidation {
		inv.Env = allowedEnv(settings.AllowedEnv)
	}

	runID := uuid.New()
	_ = s.Store.StartAgentRun(ctx, repository.AgentRunStart{ID: runID, ProjectID: project.ID, JobID: &job.ID, TaskID: &task.ID, Provider: provider, CommandSummary: commandSummary, LogPath: logPath})
	finishOutput := s.instrumentInvocation(&inv, project.ID, &job.ID, runID, &task.ID)
	result, runErr := s.Runner.Run(ctx, project.ID.String()+":"+job.ID.String(), inv)
	finishOutput()
	finishRun(s.Store, runID, result, runErr)
	message := "completed"
	if runErr != nil {
		message = runErr.Error()
	}
	if result.Cancelled {
		if err = s.Store.ReturnTaskPending(ctx, task, message); err != nil {
			return err
		}
		return Cancelled(runErr)
	}
	if runErr != nil {
		classified := runErr
		if !isValidation {
			classified = classifyRunError(result, runErr)
		}
		if IsRetryable(classified) && job.Attempt < job.MaxAttempts {
			if err = s.Store.ReturnTaskQueuedForRetry(ctx, task, result.SessionID, message); err != nil {
				return err
			}
			return classified
		}
		if err = s.Store.FinishTask(ctx, task, result.SessionID, false, message); err != nil {
			return err
		}
		return classified
	}
	return s.Store.FinishTask(ctx, task, result.SessionID, true, message)
}

func taskInvocation(settings domain.ProjectSettings, requestedProvider string, isValidation bool, workspace, prompt, taskID, sessionID, logPath string) (agent.Invocation, string, string, error) {
	if isValidation {
		return agent.Invocation{
			Provider: "validation",
			Command:  "/bin/sh",
			Args:     []string{"-lc", settings.ValidationCommand},
			Dir:      workspace,
			LogPath:  logPath,
		}, "validation", settings.ValidationCommand, nil
	}
	adapter, command, args, err := adapterFor(requestedProvider, settings)
	if err != nil {
		return agent.Invocation{}, "", "", err
	}
	var inv agent.Invocation
	if sessionID != "" {
		inv = adapter.ResumeTask(command, args, workspace, prompt, sessionID, 0, logPath)
	} else {
		inv = adapter.ExecuteTask(command, args, workspace, prompt, taskID, 0, logPath)
	}
	return inv, adapter.Name(), command, nil
}

type taskProviderPayload struct {
	Provider          string `json:"provider"`
	ProviderRequested bool   `json:"providerRequested"`
}

func (s *Service) requestedProviderForTask(ctx context.Context, job domain.Job, task domain.PlanTask) (string, error) {
	var payload taskProviderPayload
	if len(job.Payload) > 0 {
		if err := json.Unmarshal(job.Payload, &payload); err != nil {
			return "", fmt.Errorf("invalid task job payload: %w", err)
		}
	}
	if payload.ProviderRequested || strings.TrimSpace(payload.Provider) != "" {
		return payload.Provider, nil
	}
	plan, err := s.Store.GetPlan(ctx, task.PlanID)
	if err != nil {
		return "", err
	}
	var snapshot map[string]json.RawMessage
	if len(plan.ConfigSnapshot) > 0 {
		if err = json.Unmarshal(plan.ConfigSnapshot, &snapshot); err != nil {
			return "", fmt.Errorf("invalid plan config snapshot: %w", err)
		}
	}
	var provider string
	if raw := snapshot[planExecutionProviderKey]; len(raw) > 0 {
		if err = json.Unmarshal(raw, &provider); err != nil {
			return "", fmt.Errorf("invalid plan execution provider: %w", err)
		}
	}
	return provider, nil
}

type CLIProbeResult struct {
	Provider  string  `json:"provider"`
	Available bool    `json:"available"`
	Output    string  `json:"output"`
	ExitCode  *int    `json:"exitCode"`
	Error     *string `json:"error"`
}

type CLIProbeResponse struct {
	Results []CLIProbeResult `json:"results"`
}

func (s *Service) ProbeAgents(ctx context.Context, projectID uuid.UUID) (CLIProbeResponse, error) {
	project, err := s.Store.GetProject(ctx, projectID)
	if err != nil {
		return CLIProbeResponse{}, err
	}
	settings, err := s.Store.GetProjectSettings(ctx, projectID)
	if err != nil {
		return CLIProbeResponse{}, err
	}
	return probeConfiguredAgents(ctx, settings, project.WorkspacePath), nil
}

func probeConfiguredAgents(ctx context.Context, settings domain.ProjectSettings, workspace string) CLIProbeResponse {
	providers := []string{agent.ProviderCodex, agent.ProviderClaude}
	response := CLIProbeResponse{Results: make([]CLIProbeResult, 0, len(providers))}
	for _, provider := range providers {
		probe := CLIProbeResult{Provider: provider}
		adapter, command, args, resolveErr := adapterFor(provider, settings)
		if resolveErr != nil {
			message := resolveErr.Error()
			probe.Error = &message
			response.Results = append(response.Results, probe)
			continue
		}
		result, probeErr := adapter.Probe(ctx, command, args, workspace)
		probe.Output = string(result.Output)
		probe.Available = probeErr == nil
		if result.LogPath != "" {
			exitCode := result.ExitCode
			probe.ExitCode = &exitCode
		}
		if probeErr != nil {
			message := probeErr.Error()
			probe.Error = &message
		}
		response.Results = append(response.Results, probe)
	}
	return response
}

// ProbeAgent is retained for internal compatibility; diagnostics should prefer
// ProbeAgents so a broken CLI cannot hide the other provider's result.
func (s *Service) ProbeAgent(ctx context.Context, projectID uuid.UUID) (string, agent.Result, error) {
	project, err := s.Store.GetProject(ctx, projectID)
	if err != nil {
		return "", agent.Result{}, err
	}
	settings, err := s.Store.GetProjectSettings(ctx, projectID)
	if err != nil {
		return "", agent.Result{}, err
	}
	adapter, command, args, err := adapterFor("", settings)
	if err != nil {
		return "", agent.Result{}, err
	}
	result, err := adapter.Probe(ctx, command, args, project.WorkspacePath)
	return adapter.Name(), result, err
}

func (s *Service) StopTask(ctx context.Context, taskID uuid.UUID, version int64) (domain.PlanTask, []uuid.UUID, error) {
	task, jobs, err := s.Store.StopTask(ctx, taskID, version)
	if err != nil {
		return domain.PlanTask{}, nil, err
	}
	for _, jobID := range jobs {
		_ = s.Runner.Cancel(task.ProjectID.String() + ":" + jobID.String())
	}
	return task, jobs, nil
}

func (s *Service) StopPlan(ctx context.Context, planID uuid.UUID, version int64) (domain.Plan, []uuid.UUID, error) {
	plan, jobs, err := s.Store.StopPlan(ctx, planID, version)
	if err != nil {
		return domain.Plan{}, nil, err
	}
	for _, jobID := range jobs {
		_ = s.Runner.Cancel(plan.ProjectID.String() + ":" + jobID.String())
	}
	return plan, jobs, nil
}

func adapterFor(requested string, settings domain.ProjectSettings) (agent.Adapter, string, []string, error) {
	adapter, err := agent.ResolveProvider(requested, settings.AgentProvider)
	if err != nil {
		return nil, "", nil, err
	}
	var command string
	var rawArgs json.RawMessage
	switch adapter.Name() {
	case agent.ProviderCodex:
		command = settings.CodexCommand
		rawArgs = settings.CodexArgs
	case agent.ProviderClaude:
		command = settings.ClaudeCommand
		rawArgs = settings.ClaudeArgs
	}
	var args []string
	if len(rawArgs) > 0 {
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return nil, "", nil, fmt.Errorf("invalid %s arguments in project settings: %w", adapter.Name(), err)
		}
	}
	return adapter, command, append([]string(nil), args...), nil
}

func providerFromJob(raw json.RawMessage) string {
	var payload struct {
		Provider string `json:"provider"`
	}
	_ = json.Unmarshal(raw, &payload)
	return payload.Provider
}
func allowedEnv(raw json.RawMessage) []string {
	var names []string
	_ = json.Unmarshal(raw, &names)
	out := []string{}
	for _, name := range names {
		if value, ok := os.LookupEnv(name); ok {
			out = append(out, name+"="+value)
		}
	}
	return out
}
func finishRun(store *repository.Store, id uuid.UUID, result agent.Result, err error) {
	status := "succeeded"
	reason := ""
	if err != nil {
		status = "failed"
		reason = err.Error()
	}
	if result.TimedOut {
		status = "timed_out"
	}
	if result.Cancelled {
		status = "cancelled"
	}
	_ = store.FinishAgentRun(context.Background(), id, status, result.ExitCode, result.SessionID, reason, result.Duration)
}

func (s *Service) instrumentInvocation(inv *agent.Invocation, projectID uuid.UUID, jobID *uuid.UUID, runID uuid.UUID, taskID *uuid.UUID) func() {
	inv.OnStart = func(pid int) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = s.Store.SetAgentRunPID(ctx, runID, pid)
	}
	var mu sync.Mutex
	var bytesWritten int64
	var lastReported int64
	var lastEvent time.Time
	emit := func(count int64) {
		payload := map[string]any{"runId": runID, "logRef": "agent-run:" + runID.String(), "bytesWritten": count}
		if jobID != nil {
			payload["jobId"] = *jobID
		}
		if taskID != nil {
			payload["taskId"] = *taskID
		}
		payloadJSON, _ := json.Marshal(payload)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, _ = s.Store.AppendEvent(ctx, repository.NewEvent{ProjectID: &projectID, Type: "agent.output", AggregateType: "agent_run", AggregateID: runID, ResourceVersion: 1, Payload: payloadJSON})
	}
	inv.OnOutput = func(chunk []byte) {
		mu.Lock()
		bytesWritten += int64(len(chunk))
		if time.Since(lastEvent) < 500*time.Millisecond {
			mu.Unlock()
			return
		}
		lastEvent = time.Now()
		count := bytesWritten
		lastReported = count
		mu.Unlock()
		go emit(count)
	}
	return func() {
		mu.Lock()
		count := bytesWritten
		alreadyReported := lastReported == count
		mu.Unlock()
		if count > 0 && !alreadyReported {
			emit(count)
		}
	}
}

type cancelledError struct{ error }

func Cancelled(err error) error {
	if err == nil {
		err = errors.New("cancelled by user")
	}
	return cancelledError{error: err}
}
func IsCancelled(err error) bool { var e cancelledError; return errors.As(err, &e) }

type classifiedError struct {
	error
	retryable bool
}

func (e classifiedError) Retryable() bool { return e.retryable }
func Retryable(err error) error           { return classifiedError{error: err, retryable: true} }
func IsRetryable(err error) bool {
	var e interface{ Retryable() bool }
	return errors.As(err, &e) && e.Retryable()
}
func classifyRunError(result agent.Result, err error) error {
	if result.Cancelled {
		return err
	}
	if result.TimedOut {
		return err
	}
	if result.ExitCode == 126 || result.ExitCode == 127 {
		return err
	}
	return Retryable(err)
}
