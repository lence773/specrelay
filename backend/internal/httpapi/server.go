package httpapi

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/lyming99/specrelay/backend/internal/agent"
	"github.com/lyming99/specrelay/backend/internal/app"
	"github.com/lyming99/specrelay/backend/internal/domain"
	"github.com/lyming99/specrelay/backend/internal/events"
	"github.com/lyming99/specrelay/backend/internal/repository"
)

type Server struct {
	Store           *repository.Store
	App             *app.Service
	Auth            *Auth
	Broker          *events.Broker
	Logger          *slog.Logger
	PublicDir       string
	DataDir         string
	MCP             http.Handler
	ShutdownToken   string
	RequestShutdown func()
	Draining        *atomic.Bool
}
type apiError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Details   any    `json:"details,omitempty"`
	RequestID string `json:"requestId"`
}
type asyncResponse struct {
	JobID           uuid.UUID `json:"jobId"`
	State           string    `json:"state"`
	ResourceVersion int64     `json:"resourceVersion"`
}

type feedbackAssociationInput struct {
	PlanID        *uuid.UUID `json:"planId,omitempty"`
	TaskID        *uuid.UUID `json:"taskId,omitempty"`
	CheckpointID  *uuid.UUID `json:"checkpointId,omitempty"`
	FileID        *uuid.UUID `json:"fileId,omitempty"`
	DiffHunkID    *uuid.UUID `json:"diffHunkId,omitempty"`
	DiffLineSide  string     `json:"diffLineSide,omitempty"`
	DiffLineStart *int       `json:"diffLineStart,omitempty"`
	DiffLineEnd   *int       `json:"diffLineEnd,omitempty"`
}

func (in feedbackAssociationInput) repositoryParams() *repository.FeedbackAssociationParams {
	return &repository.FeedbackAssociationParams{
		PlanID:        in.PlanID,
		TaskID:        in.TaskID,
		CheckpointID:  in.CheckpointID,
		FileID:        in.FileID,
		DiffHunkID:    in.DiffHunkID,
		DiffLineSide:  in.DiffLineSide,
		DiffLineStart: in.DiffLineStart,
		DiffLineEnd:   in.DiffLineEnd,
	}
}

type intakeCreateInput struct {
	Kind                       string     `json:"kind"`
	ParentIntakeID             *uuid.UUID `json:"parentIntakeId"`
	Title                      string     `json:"title"`
	Body                       string     `json:"body"`
	Provider                   string     `json:"provider,omitempty"`
	RequirementSessionID       string     `json:"requirementSessionId,omitempty"`
	RequirementSessionProvider string     `json:"requirementSessionProvider,omitempty"`
	feedbackAssociationInput
}

type feedbackCreateInput struct {
	RequirementID uuid.UUID `json:"requirementId"`
	Title         string    `json:"title"`
	Body          string    `json:"body"`
	Provider      string    `json:"provider,omitempty"`
	feedbackAssociationInput
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /readyz", s.ready)
	mux.HandleFunc("POST /internal/shutdown", s.shutdown)
	mux.HandleFunc("POST /api/v1/auth/exchange", s.exchange)
	mux.HandleFunc("GET /api/v1/filesystem/directories", s.directories)
	mux.HandleFunc("GET /api/v1/projects", s.projects)
	mux.HandleFunc("POST /api/v1/projects", s.projects)
	mux.HandleFunc("GET /api/v1/projects/{id}", s.project)
	mux.HandleFunc("PUT /api/v1/projects/{id}", s.project)
	mux.HandleFunc("DELETE /api/v1/projects/{id}", s.project)
	mux.HandleFunc("GET /api/v1/projects/{id}/settings", s.projectSettings)
	mux.HandleFunc("PUT /api/v1/projects/{id}/settings", s.projectSettings)
	mux.HandleFunc("POST /api/v1/projects/{id}/automation/start", s.startAutomation)
	mux.HandleFunc("POST /api/v1/projects/{id}/automation/stop", s.stopAutomation)
	mux.HandleFunc("GET /api/v1/projects/{id}/intakes", s.intakes)
	mux.HandleFunc("POST /api/v1/projects/{id}/intakes", s.intakes)
	mux.HandleFunc("POST /api/v1/projects/{id}/intakes/discuss", s.discussRequirement)
	mux.HandleFunc("POST /api/v1/projects/{id}/feedback", s.createFeedback)
	mux.HandleFunc("GET /api/v1/projects/{id}/feedback/{feedbackId}", s.feedbackContext)
	mux.HandleFunc("GET /api/v1/intakes/{id}", s.intake)
	mux.HandleFunc("PUT /api/v1/intakes/{id}", s.intake)
	mux.HandleFunc("POST /api/v1/intakes/{id}/generate", s.generatePlan)
	mux.HandleFunc("POST /api/v1/intakes/{id}/attachments", s.upload)
	mux.HandleFunc("GET /api/v1/projects/{id}/plans", s.plans)
	mux.HandleFunc("GET /api/v1/plans/{id}", s.plan)
	mux.HandleFunc("DELETE /api/v1/plans/{id}", s.plan)
	mux.HandleFunc("GET /api/v1/plans/{id}/execution-context", s.planExecutionContext)
	mux.HandleFunc("POST /api/v1/plans/{id}/execution-context/accept", s.acceptPlanExecutionContext)
	mux.HandleFunc("POST /api/v1/plans/{id}/run", s.runPlan)
	mux.HandleFunc("POST /api/v1/plans/{id}/stop", s.stopPlan)
	mux.HandleFunc("GET /api/v1/plans/{id}/tasks", s.tasks)
	mux.HandleFunc("GET /api/v1/tasks/{id}", s.task)
	mux.HandleFunc("GET /api/v1/checkpoints/{id}", s.checkpoint)
	mux.HandleFunc("POST /api/v1/tasks/{id}/run", s.runTask)
	mux.HandleFunc("POST /api/v1/tasks/{id}/retry", s.runTask)
	mux.HandleFunc("POST /api/v1/tasks/{id}/stop", s.stopTask)
	mux.HandleFunc("GET /api/v1/projects/{id}/agent-runs", s.agentRuns)
	mux.HandleFunc("GET /api/v1/projects/{id}/observability", s.agentRunObservability)
	mux.HandleFunc("GET /api/v1/projects/{id}/observability/export", s.exportAgentRunObservability)
	mux.HandleFunc("GET /api/v1/agent-runs/{id}/log", s.agentRunLog)
	mux.HandleFunc("POST /api/v1/agents/probe", s.probeAgent)
	mux.HandleFunc("POST /api/v1/settings/mcp-token/rotate", s.rotateMCPToken)
	mux.HandleFunc("GET /api/v1/events", s.eventHistory)
	mux.HandleFunc("GET /api/v1/events/stream", s.eventStream)
	if s.MCP != nil {
		mux.Handle("/mcp", s.MCP)
	}
	if s.PublicDir != "" {
		mux.Handle("/", spaHandler(s.PublicDir))
	}
	return s.middleware(mux)
}
func (s *Server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := r.Header.Get("X-Request-ID")
		if requestID == "" {
			requestID = uuid.NewString()
		}
		w.Header().Set("X-Request-ID", requestID)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; connect-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'")
		if !LocalRequest(r) {
			writeError(w, r, http.StatusForbidden, "request_not_allowed", "Host or Origin is not allowed", nil)
			return
		}
		isShutdownEndpoint := r.URL.Path == "/internal/shutdown"
		if s.Draining != nil && s.Draining.Load() && !isShutdownEndpoint && r.URL.Path != "/healthz" {
			writeError(w, r, http.StatusServiceUnavailable, "shutting_down", "Backend is shutting down", nil)
			return
		}
		if !isShutdownEndpoint && r.URL.Path != "/healthz" && r.URL.Path != "/readyz" && r.URL.Path != "/api/v1/auth/exchange" && !strings.HasPrefix(r.URL.Path, "/assets/") && r.URL.Path != "/" && !s.Auth.Allowed(r) {
			writeError(w, r, http.StatusUnauthorized, "unauthorized", "Authentication required", nil)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/v1/") && r.URL.Path != "/api/v1/auth/exchange" {
			pingCtx, cancel := context.WithTimeout(r.Context(), time.Second)
			err := s.Store.Ping(pingCtx)
			cancel()
			if err != nil {
				writeError(w, r, http.StatusServiceUnavailable, "database_unavailable", "Database is unavailable", nil)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
func (s *Server) shutdown(w http.ResponseWriter, r *http.Request) {
	token := r.Header.Get("X-SpecRelay-Shutdown-Token")
	if s.ShutdownToken == "" || subtle.ConstantTimeCompare([]byte(token), []byte(s.ShutdownToken)) != 1 {
		writeError(w, r, http.StatusUnauthorized, "unauthorized", "Shutdown token is invalid", nil)
		return
	}
	if s.Draining != nil {
		s.Draining.Store(true)
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"state": "shutting_down"})
	if s.RequestShutdown != nil {
		s.RequestShutdown()
	}
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.Store.Ping(ctx); err != nil {
		writeError(w, r, http.StatusServiceUnavailable, "database_unavailable", "Database is unavailable", nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}
func (s *Server) exchange(w http.ResponseWriter, r *http.Request) {
	if !s.Auth.Exchange(w, r) {
		writeError(w, r, http.StatusUnauthorized, "invalid_token", "The access token is invalid", nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"state": "authenticated"})
}

type directoryEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
}
type directoryListing struct {
	Path        string           `json:"path"`
	ParentPath  string           `json:"parentPath,omitempty"`
	Roots       []directoryEntry `json:"roots"`
	Directories []directoryEntry `json:"directories"`
}

// directoryRoots exposes every selectable filesystem root. On Windows this
// means all mounted drive letters rather than only the drive containing the
// user's home directory, so the project picker can reach D:, E:, and other
// local volumes directly.
func directoryRoots() []directoryEntry {
	if runtime.GOOS == "windows" {
		roots := make([]directoryEntry, 0, 4)
		for letter := 'A'; letter <= 'Z'; letter++ {
			path := fmt.Sprintf("%c:\\", letter)
			if info, err := os.Stat(path); err == nil && info.IsDir() {
				roots = append(roots, directoryEntry{Name: fmt.Sprintf("%c:", letter), Path: path})
			}
		}
		return roots
	}
	root := string(filepath.Separator)
	return []directoryEntry{{Name: root, Path: root}}
}

func (s *Server) directories(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimSpace(r.URL.Query().Get("path"))
	if path == "" {
		var err error
		path, err = os.UserHomeDir()
		if err != nil {
			writeError(w, r, http.StatusBadRequest, "directory_unavailable", "Unable to determine the home directory", nil)
			return
		}
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_directory", err.Error(), nil)
		return
	}
	real, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_directory", "Directory does not exist or cannot be accessed", nil)
		return
	}
	info, err := os.Stat(real)
	if err != nil || !info.IsDir() {
		writeError(w, r, http.StatusBadRequest, "invalid_directory", "Path must be an existing directory", nil)
		return
	}
	entries, err := os.ReadDir(real)
	if err != nil {
		writeError(w, r, http.StatusForbidden, "directory_unreadable", "Directory cannot be read", nil)
		return
	}
	directories := make([]directoryEntry, 0, len(entries))
	for _, entry := range entries {
		child := filepath.Join(real, entry.Name())
		childInfo, statErr := os.Stat(child)
		if statErr != nil || !childInfo.IsDir() {
			continue
		}
		resolved, resolveErr := filepath.EvalSymlinks(child)
		if resolveErr != nil {
			continue
		}
		directories = append(directories, directoryEntry{Name: entry.Name(), Path: resolved})
	}
	sort.Slice(directories, func(i, j int) bool {
		return strings.ToLower(directories[i].Name) < strings.ToLower(directories[j].Name)
	})
	parent := filepath.Dir(real)
	if parent == real {
		parent = ""
	}
	writeJSON(w, http.StatusOK, directoryListing{Path: real, ParentPath: parent, Roots: directoryRoots(), Directories: directories})
}

func (s *Server) projects(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		items, err := s.Store.ListProjects(r.Context())
		respond(w, r, items, err)
		return
	}
	var in struct{ Name, Description, WorkspacePath string }
	if err := decodeJSON(r, &in); err != nil {
		badJSON(w, r, err)
		return
	}
	p, err := s.App.CreateProject(r.Context(), in.Name, in.Description, in.WorkspacePath)
	respondStatus(w, r, http.StatusCreated, p, err)
}
func (s *Server) project(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		p, err := s.Store.GetProject(r.Context(), id)
		respond(w, r, p, err)
	case http.MethodPut:
		var in struct {
			Name, Description, WorkspacePath string
			Version                          int64
		}
		if err := decodeJSON(r, &in); err != nil {
			badJSON(w, r, err)
			return
		}
		p, err := s.App.UpdateProject(r.Context(), id, in.Name, in.Description, in.WorkspacePath, in.Version)
		respond(w, r, p, err)
	case http.MethodDelete:
		version, _ := strconv.ParseInt(r.URL.Query().Get("version"), 10, 64)
		err := s.Store.DeleteProject(r.Context(), id, version)
		if err != nil {
			respond(w, r, nil, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
func (s *Server) projectSettings(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	if r.Method == http.MethodGet {
		p, err := s.Store.GetProjectSettings(r.Context(), id)
		respond(w, r, p, err)
		return
	}
	var in domain.ProjectSettings
	if err := decodeJSON(r, &in); err != nil {
		badJSON(w, r, err)
		return
	}
	in.ProjectID = id
	p, err := s.Store.UpdateProjectSettings(r.Context(), in)
	respond(w, r, p, err)
}
func (s *Server) startAutomation(w http.ResponseWriter, r *http.Request) { s.automation(w, r, true) }
func (s *Server) stopAutomation(w http.ResponseWriter, r *http.Request)  { s.automation(w, r, false) }
func (s *Server) automation(w http.ResponseWriter, r *http.Request, enabled bool) {
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	var in struct {
		Version int64 `json:"version"`
	}
	if err := decodeJSON(r, &in); err != nil {
		badJSON(w, r, err)
		return
	}
	p, err := s.Store.SetAutomation(r.Context(), id, enabled, in.Version)
	if err == nil && !enabled {
		s.App.Runner.CancelPrefix(id.String() + ":")
	}
	respond(w, r, p, err)
}
func (s *Server) intakes(w http.ResponseWriter, r *http.Request) {
	projectID, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	if r.Method == http.MethodGet {
		items, err := s.Store.ListIntakes(r.Context(), projectID)
		respond(w, r, items, err)
		return
	}
	var in intakeCreateInput
	if err := decodeJSON(r, &in); err != nil {
		badJSON(w, r, err)
		return
	}
	params := repository.CreateIntakeParams{ProjectID: projectID, Kind: in.Kind, ParentIntakeID: in.ParentIntakeID, Title: in.Title, Body: in.Body, RequirementSessionID: in.RequirementSessionID, RequirementSessionProvider: in.RequirementSessionProvider}
	if in.Kind == "feedback" {
		params.Feedback = in.feedbackAssociationInput.repositoryParams()
	}
	item, job, err := s.App.CreateIntakeWithProvider(repository.WithExecutionProvider(r.Context(), in.Provider), params, in.Provider)
	if err != nil {
		respond(w, r, nil, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"intake": item, "job": job})
}
func (s *Server) createFeedback(w http.ResponseWriter, r *http.Request) {
	projectID, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	var in feedbackCreateInput
	if err := decodeJSON(r, &in); err != nil {
		badJSON(w, r, err)
		return
	}
	params := repository.CreateIntakeParams{
		ProjectID:      projectID,
		Kind:           "feedback",
		ParentIntakeID: &in.RequirementID,
		Title:          in.Title,
		Body:           in.Body,
		Feedback:       in.feedbackAssociationInput.repositoryParams(),
	}
	item, job, err := s.App.CreateIntakeWithProvider(repository.WithExecutionProvider(r.Context(), in.Provider), params, in.Provider)
	if err != nil {
		respond(w, r, nil, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"feedback": item, "job": job})
}

func (s *Server) feedbackContext(w http.ResponseWriter, r *http.Request) {
	projectID, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	feedbackID, ok := pathUUID(w, r, "feedbackId")
	if !ok {
		return
	}
	context, err := s.App.GetFeedbackContext(r.Context(), projectID, feedbackID)
	respond(w, r, context, err)
}

func (s *Server) discussRequirement(w http.ResponseWriter, r *http.Request) {
	projectID, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	var in app.RequirementDiscussionRequest
	if err := decodeJSON(r, &in); err != nil {
		badJSON(w, r, err)
		return
	}
	result, err := s.App.DiscussRequirement(r.Context(), projectID, in)
	respond(w, r, result, err)
}

func (s *Server) intake(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	if r.Method == http.MethodGet {
		i, err := s.Store.GetIntake(r.Context(), id)
		respond(w, r, i, err)
		return
	}
	var in repository.UpdateIntakeParams
	if err := decodeJSON(r, &in); err != nil {
		badJSON(w, r, err)
		return
	}
	i, err := s.Store.UpdateIntake(r.Context(), id, in)
	respond(w, r, i, err)
}
func (s *Server) generatePlan(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	var in struct {
		Version  int64  `json:"version"`
		Provider string `json:"provider,omitempty"`
	}
	if err := decodeJSON(r, &in); err != nil {
		badJSON(w, r, err)
		return
	}
	job, err := s.App.QueuePlanGeneration(repository.WithExecutionProvider(r.Context(), in.Provider), id, in.Version, in.Provider)
	if err != nil {
		respond(w, r, nil, err)
		return
	}
	writeJSON(w, http.StatusAccepted, asyncResponse{JobID: job.ID, State: job.Status, ResourceVersion: job.Version})
}
func (s *Server) upload(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 51<<20)
	if err := r.ParseMultipartForm(51 << 20); err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_multipart", err.Error(), nil)
		return
	}
	fileHeaders := r.MultipartForm.File["file"]
	if len(fileHeaders) != 1 {
		writeError(w, r, http.StatusBadRequest, "file_required", "Exactly one file is required", nil)
		return
	}
	a, err := s.App.SaveAttachment(r.Context(), id, fileHeaders[0])
	respondStatus(w, r, http.StatusCreated, a, err)
}
func (s *Server) plans(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	items, err := s.Store.ListPlans(r.Context(), id)
	respond(w, r, items, err)
}
func (s *Server) plan(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	if r.Method == http.MethodDelete {
		version, err := strconv.ParseInt(r.URL.Query().Get("version"), 10, 64)
		if err != nil || version < 1 {
			writeError(w, r, http.StatusBadRequest, "invalid_version", "version must be a positive integer", nil)
			return
		}
		err = s.Store.DeletePlan(r.Context(), id, version)
		if err != nil {
			respond(w, r, nil, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	p, err := s.Store.GetPlan(r.Context(), id)
	if err != nil {
		respond(w, r, nil, err)
		return
	}
	tasks, err := s.Store.ListTasks(r.Context(), id)
	if err != nil {
		respond(w, r, nil, err)
		return
	}
	feedback, err := s.App.ListFeedbackForPlan(r.Context(), p.ProjectID, p.ID)
	respond(w, r, map[string]any{"plan": p, "tasks": tasks, "feedback": feedback}, err)
}

func (s *Server) planExecutionContext(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	context, err := s.App.GetPlanExecutionContext(r.Context(), id, r.URL.Query().Get("provider"))
	respond(w, r, context, err)
}

func (s *Server) acceptPlanExecutionContext(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	var in struct {
		BaselineSnapshotID uuid.UUID `json:"baselineSnapshotId"`
		Fingerprint        string    `json:"fingerprint"`
		Reason             string    `json:"reason"`
		Provider           string    `json:"provider,omitempty"`
	}
	if err := decodeJSON(r, &in); err != nil {
		badJSON(w, r, err)
		return
	}
	checkpoint, audit, err := s.App.AcceptPlanExecutionContext(r.Context(), id, in.BaselineSnapshotID, in.Fingerprint, in.Reason, in.Provider)
	respond(w, r, map[string]any{"checkpoint": checkpoint, "audit": audit}, err)
}

func (s *Server) runPlan(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	var in struct {
		Version  int64  `json:"version"`
		Provider string `json:"provider,omitempty"`
	}
	if err := decodeJSON(r, &in); err != nil {
		badJSON(w, r, err)
		return
	}
	job, err := s.App.QueuePlan(repository.WithExecutionProvider(r.Context(), in.Provider), id, in.Version, in.Provider)
	if err != nil {
		respond(w, r, nil, err)
		return
	}
	writeJSON(w, http.StatusAccepted, asyncResponse{JobID: job.ID, State: job.Status, ResourceVersion: job.Version})
}
func (s *Server) stopPlan(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	var in struct {
		Version int64 `json:"version"`
	}
	if err := decodeJSON(r, &in); err != nil {
		badJSON(w, r, err)
		return
	}
	plan, jobs, err := s.App.StopPlan(r.Context(), id, in.Version)
	if err != nil {
		respond(w, r, nil, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"plan": plan, "jobIds": jobs})
}

func (s *Server) tasks(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	items, err := s.Store.ListTasks(r.Context(), id)
	respond(w, r, items, err)
}
func (s *Server) task(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	task, err := s.Store.GetTask(r.Context(), id)
	if err != nil {
		respond(w, r, nil, err)
		return
	}
	feedback, err := s.App.ListFeedbackForTask(r.Context(), task.ProjectID, task.ID)
	respond(w, r, map[string]any{"task": task, "feedback": feedback}, err)
}

func (s *Server) checkpoint(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	checkpoint, err := s.Store.GetPlanExecutionSnapshot(r.Context(), id)
	if err != nil {
		respond(w, r, nil, err)
		return
	}
	feedback, err := s.App.ListFeedbackForCheckpoint(r.Context(), checkpoint.ProjectID, checkpoint.ID)
	respond(w, r, map[string]any{"checkpoint": checkpoint, "feedback": feedback}, err)
}

func (s *Server) runTask(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	var in struct {
		Version  int64  `json:"version"`
		Provider string `json:"provider,omitempty"`
	}
	if err := decodeJSON(r, &in); err != nil {
		badJSON(w, r, err)
		return
	}
	job, err := s.App.QueueTask(repository.WithExecutionProvider(r.Context(), in.Provider), id, in.Version, in.Provider)
	if err != nil {
		respond(w, r, nil, err)
		return
	}
	writeJSON(w, http.StatusAccepted, asyncResponse{JobID: job.ID, State: job.Status, ResourceVersion: job.Version})
}
func (s *Server) stopTask(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	var in struct {
		Version int64 `json:"version"`
	}
	if err := decodeJSON(r, &in); err != nil {
		badJSON(w, r, err)
		return
	}
	task, jobs, err := s.App.StopTask(r.Context(), id, in.Version)
	if err != nil {
		respond(w, r, nil, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"task": task, "jobIds": jobs})
}

func (s *Server) agentRuns(w http.ResponseWriter, r *http.Request) {
	projectID, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	limit := 100
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 200 {
			writeError(w, r, http.StatusBadRequest, "invalid_limit", "limit must be between 1 and 200", nil)
			return
		}
		limit = parsed
	}
	items, err := s.Store.ListAgentRuns(r.Context(), projectID, limit)
	respond(w, r, items, err)
}

type observabilityRequest struct {
	Filter   repository.AgentRunObservabilityFilter
	Page     int
	PageSize int
}

type observabilityFilterResponse struct {
	From     *time.Time `json:"from,omitempty"`
	To       *time.Time `json:"to,omitempty"`
	Provider string     `json:"provider,omitempty"`
	PlanID   *uuid.UUID `json:"planId,omitempty"`
}

type observabilityPagination struct {
	Page       int  `json:"page"`
	PageSize   int  `json:"pageSize"`
	TotalItems int  `json:"totalItems"`
	HasMore    bool `json:"hasMore"`
}

type agentRunObservabilityResponse struct {
	ProjectID  uuid.UUID                   `json:"projectId"`
	Filter     observabilityFilterResponse `json:"filter"`
	Pagination observabilityPagination     `json:"pagination"`
	repository.AgentRunObservability
}

func parseObservabilityRequest(w http.ResponseWriter, r *http.Request, projectID uuid.UUID) (observabilityRequest, bool) {
	request := observabilityRequest{Filter: repository.AgentRunObservabilityFilter{ProjectID: projectID}, Page: 1, PageSize: 50}
	query := r.URL.Query()
	parseTime := func(name string) (*time.Time, bool) {
		raw := strings.TrimSpace(query.Get(name))
		if raw == "" {
			return nil, true
		}
		value, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			writeError(w, r, http.StatusBadRequest, "invalid_time", name+" must be an RFC3339 timestamp", nil)
			return nil, false
		}
		return &value, true
	}
	var ok bool
	if request.Filter.From, ok = parseTime("from"); !ok {
		return observabilityRequest{}, false
	}
	if request.Filter.To, ok = parseTime("to"); !ok {
		return observabilityRequest{}, false
	}
	if request.Filter.From != nil && request.Filter.To != nil && request.Filter.From.After(*request.Filter.To) {
		writeError(w, r, http.StatusBadRequest, "invalid_time_range", "from must be earlier than or equal to to", nil)
		return observabilityRequest{}, false
	}
	request.Filter.Provider = strings.TrimSpace(query.Get("provider"))
	switch request.Filter.Provider {
	case "", "codex", "claude", "validation":
	default:
		writeError(w, r, http.StatusBadRequest, "invalid_provider", "provider must be codex, claude, or validation", nil)
		return observabilityRequest{}, false
	}
	if raw := strings.TrimSpace(query.Get("planId")); raw != "" {
		planID, err := uuid.Parse(raw)
		if err != nil {
			writeError(w, r, http.StatusBadRequest, "invalid_plan", "planId must be a UUID belonging to the current project", nil)
			return observabilityRequest{}, false
		}
		request.Filter.PlanID = &planID
	}
	if raw := strings.TrimSpace(query.Get("page")); raw != "" {
		page, err := strconv.Atoi(raw)
		if err != nil || page < 1 {
			writeError(w, r, http.StatusBadRequest, "invalid_page", "page must be a positive integer", nil)
			return observabilityRequest{}, false
		}
		request.Page = page
	}
	if raw := strings.TrimSpace(query.Get("pageSize")); raw != "" {
		pageSize, err := strconv.Atoi(raw)
		if err != nil || pageSize < 1 || pageSize > 200 {
			writeError(w, r, http.StatusBadRequest, "invalid_page_size", "pageSize must be between 1 and 200", nil)
			return observabilityRequest{}, false
		}
		request.PageSize = pageSize
	}
	return request, true
}

func (s *Server) loadAgentRunObservability(w http.ResponseWriter, r *http.Request, projectID uuid.UUID) (domain.Project, observabilityRequest, repository.AgentRunObservability, bool) {
	request, ok := parseObservabilityRequest(w, r, projectID)
	if !ok {
		return domain.Project{}, observabilityRequest{}, repository.AgentRunObservability{}, false
	}
	project, err := s.Store.GetProject(r.Context(), projectID)
	if err != nil {
		respond(w, r, nil, err)
		return domain.Project{}, observabilityRequest{}, repository.AgentRunObservability{}, false
	}
	if request.Filter.PlanID != nil {
		plan, planErr := s.Store.GetPlan(r.Context(), *request.Filter.PlanID)
		if errors.Is(planErr, domain.ErrNotFound) || planErr == nil && plan.ProjectID != projectID {
			writeError(w, r, http.StatusBadRequest, "invalid_plan", "planId must belong to the current project", nil)
			return domain.Project{}, observabilityRequest{}, repository.AgentRunObservability{}, false
		}
		if planErr != nil {
			respond(w, r, nil, planErr)
			return domain.Project{}, observabilityRequest{}, repository.AgentRunObservability{}, false
		}
	}
	result, err := s.Store.QueryAgentRunObservability(r.Context(), request.Filter)
	if err != nil {
		respond(w, r, nil, err)
		return domain.Project{}, observabilityRequest{}, repository.AgentRunObservability{}, false
	}
	return project, request, result, true
}

func (s *Server) agentRunObservability(w http.ResponseWriter, r *http.Request) {
	projectID, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	_, request, result, ok := s.loadAgentRunObservability(w, r, projectID)
	if !ok {
		return
	}
	total := len(result.Runs)
	start := total
	pageOffset := request.Page - 1
	if pageOffset <= total/request.PageSize {
		start = pageOffset * request.PageSize
		if start > total {
			start = total
		}
	}
	end := total
	if request.PageSize <= total-start {
		end = start + request.PageSize
	}
	result.Runs = result.Runs[start:end]
	writeJSON(w, http.StatusOK, agentRunObservabilityResponse{
		ProjectID:             projectID,
		Filter:                observabilityFilterResponse{From: request.Filter.From, To: request.Filter.To, Provider: request.Filter.Provider, PlanID: request.Filter.PlanID},
		Pagination:            observabilityPagination{Page: request.Page, PageSize: request.PageSize, TotalItems: total, HasMore: end < total},
		AgentRunObservability: result,
	})
}

type observabilityExportOptions struct {
	IncludeProjectName    bool `json:"includeProjectName"`
	IncludeWorkspacePath  bool `json:"includeWorkspacePath"`
	IncludeBusinessTitles bool `json:"includeBusinessTitles"`
}

type observabilityExportDocument struct {
	GeneratedAt   time.Time                        `json:"generatedAt"`
	ProjectID     uuid.UUID                        `json:"projectId"`
	ProjectName   *string                          `json:"projectName,omitempty"`
	WorkspacePath *string                          `json:"workspacePath,omitempty"`
	Filter        observabilityFilterResponse      `json:"filter"`
	Options       observabilityExportOptions       `json:"options"`
	Summary       repository.AgentRunObservability `json:"summary"`
}

func parseQueryBool(w http.ResponseWriter, r *http.Request, name string) (bool, bool) {
	switch strings.TrimSpace(r.URL.Query().Get(name)) {
	case "":
		return false, true
	case "true":
		return true, true
	case "false":
		return false, true
	default:
		writeError(w, r, http.StatusBadRequest, "invalid_export_option", name+" must be true or false", nil)
		return false, false
	}
}

func (s *Server) exportAgentRunObservability(w http.ResponseWriter, r *http.Request) {
	projectID, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	format := strings.TrimSpace(r.URL.Query().Get("format"))
	if format == "" {
		format = "json"
	}
	if format != "json" && format != "csv" {
		writeError(w, r, http.StatusBadRequest, "invalid_export_format", "format must be json or csv", nil)
		return
	}
	options := observabilityExportOptions{}
	if options.IncludeProjectName, ok = parseQueryBool(w, r, "includeProjectName"); !ok {
		return
	}
	if options.IncludeWorkspacePath, ok = parseQueryBool(w, r, "includeWorkspacePath"); !ok {
		return
	}
	if options.IncludeBusinessTitles, ok = parseQueryBool(w, r, "includeBusinessTitles"); !ok {
		return
	}
	project, request, result, ok := s.loadAgentRunObservability(w, r, projectID)
	if !ok {
		return
	}
	if !options.IncludeBusinessTitles {
		redactObservabilityTitles(&result)
	}
	document := observabilityExportDocument{
		GeneratedAt: time.Now().UTC(), ProjectID: projectID,
		Filter:  observabilityFilterResponse{From: request.Filter.From, To: request.Filter.To, Provider: request.Filter.Provider, PlanID: request.Filter.PlanID},
		Options: options, Summary: result,
	}
	if options.IncludeProjectName {
		document.ProjectName = &project.Name
	}
	if options.IncludeWorkspacePath {
		document.WorkspacePath = &project.WorkspacePath
	}
	filename := "specrelay-observability-" + document.GeneratedAt.Format("20060102T150405Z") + "." + format
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	if format == "json" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(document)
		return
	}
	body, err := marshalObservabilityCSV(document)
	if err != nil {
		respond(w, r, nil, err)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func redactObservabilityTitles(result *repository.AgentRunObservability) {
	for index := range result.Requirements {
		result.Requirements[index].Title = ""
	}
	for index := range result.Plans {
		result.Plans[index].Title = ""
	}
	for index := range result.Tasks {
		result.Tasks[index].Title = ""
	}
	for index := range result.Aggregates.Usage.ByRequirement {
		result.Aggregates.Usage.ByRequirement[index].Title = ""
	}
	for index := range result.Aggregates.Usage.ByPlan {
		result.Aggregates.Usage.ByPlan[index].Title = ""
	}
}

func marshalObservabilityCSV(document observabilityExportDocument) ([]byte, error) {
	var buffer bytes.Buffer
	writer := csv.NewWriter(&buffer)
	header := []string{"section", "key", "projectId", "requirementId", "planId", "taskId", "runId", "title", "provider", "operationType", "sessionMode", "retryCount", "queueWaitMs", "durationMs", "status", "failureCategory", "outputBytes", "outputLines", "eventCount", "outputTruncated", "inputTokens", "outputTokens", "totalTokens", "costAmount", "costCurrency", "value", "numerator", "denominator", "coverageCount", "totalCount", "available", "bucket"}
	if err := writer.Write(header); err != nil {
		return nil, err
	}
	row := func(values map[string]string) error {
		entry := make([]string, len(header))
		for index, column := range header {
			entry[index] = values[column]
		}
		return writer.Write(entry)
	}
	if document.ProjectName != nil {
		if err := row(map[string]string{"section": "metadata", "key": "projectName", "value": *document.ProjectName}); err != nil {
			return nil, err
		}
	}
	if document.WorkspacePath != nil {
		if err := row(map[string]string{"section": "metadata", "key": "workspacePath", "value": *document.WorkspacePath}); err != nil {
			return nil, err
		}
	}
	for _, item := range document.Summary.Requirements {
		if err := row(map[string]string{"section": "requirement", "projectId": document.ProjectID.String(), "requirementId": item.ID.String(), "title": item.Title, "status": item.Status}); err != nil {
			return nil, err
		}
	}
	for _, item := range document.Summary.Plans {
		if err := row(map[string]string{"section": "plan", "projectId": document.ProjectID.String(), "requirementId": item.RequirementID.String(), "planId": item.ID.String(), "title": item.Title, "status": item.Status}); err != nil {
			return nil, err
		}
	}
	for _, item := range document.Summary.Tasks {
		if err := row(map[string]string{"section": "task", "projectId": document.ProjectID.String(), "planId": item.PlanID.String(), "taskId": item.ID.String(), "key": item.TaskKey, "title": item.Title, "status": item.Status}); err != nil {
			return nil, err
		}
	}
	for _, item := range document.Summary.Runs {
		values := map[string]string{"section": "run", "projectId": document.ProjectID.String(), "runId": item.ID.String(), "provider": item.Provider, "status": item.Status}
		setUUIDString(values, "requirementId", item.RequirementID)
		setUUIDString(values, "planId", item.PlanID)
		setUUIDString(values, "taskId", item.TaskID)
		setString(values, "operationType", item.OperationType)
		setString(values, "sessionMode", item.SessionMode)
		setInt(values, "retryCount", item.RetryCount)
		setInt64(values, "queueWaitMs", item.QueueWaitMS)
		setInt64(values, "durationMs", item.DurationMS)
		setString(values, "failureCategory", item.FailureCategory)
		setInt64(values, "outputBytes", item.OutputBytes)
		setInt64(values, "outputLines", item.OutputLines)
		setInt64(values, "eventCount", item.EventCount)
		if item.OutputTruncated != nil {
			values["outputTruncated"] = strconv.FormatBool(*item.OutputTruncated)
		}
		setInt64(values, "inputTokens", item.InputTokens)
		setInt64(values, "outputTokens", item.OutputTokens)
		setInt64(values, "totalTokens", item.TotalTokens)
		setString(values, "costAmount", item.CostAmount)
		setString(values, "costCurrency", item.CostCurrency)
		if err := row(values); err != nil {
			return nil, err
		}
	}
	rates := []struct {
		name  string
		value repository.ObservabilityRate
	}{
		{"sessionReuseRate", document.Summary.Aggregates.SessionReuseRate},
		{"snapshotRestoreRate", document.Summary.Aggregates.SnapshotRestoreRate},
		{"planGenerationSuccessRate", document.Summary.Aggregates.PlanGenerationSuccessRate},
		{"taskExecutionSuccessRate", document.Summary.Aggregates.TaskExecutionSuccessRate},
	}
	for _, rate := range rates {
		values := map[string]string{"section": "rate", "key": rate.name, "numerator": strconv.Itoa(rate.value.Numerator), "denominator": strconv.Itoa(rate.value.Denominator)}
		if rate.value.Value != nil {
			values["value"] = strconv.FormatFloat(*rate.value.Value, 'f', -1, 64)
		}
		if err := row(values); err != nil {
			return nil, err
		}
	}
	for _, item := range document.Summary.Aggregates.FailureCategories {
		if err := row(map[string]string{"section": "failureCategory", "key": item.Category, "value": strconv.Itoa(item.Count)}); err != nil {
			return nil, err
		}
	}
	for _, item := range document.Summary.Aggregates.DurationTrend {
		if err := row(map[string]string{"section": "queueWaitTrend", "bucket": item.Bucket, "totalCount": strconv.Itoa(item.RunCount), "queueWaitMs": strconv.FormatInt(item.QueueWait.AverageMS, 10), "coverageCount": strconv.Itoa(item.QueueWait.CoverageCount), "available": strconv.FormatBool(item.QueueWait.Available)}); err != nil {
			return nil, err
		}
		if err := row(map[string]string{"section": "runDurationTrend", "bucket": item.Bucket, "totalCount": strconv.Itoa(item.RunCount), "durationMs": strconv.FormatInt(item.RunDuration.AverageMS, 10), "coverageCount": strconv.Itoa(item.RunDuration.CoverageCount), "available": strconv.FormatBool(item.RunDuration.Available)}); err != nil {
			return nil, err
		}
	}
	writeUsage := func(section, key, title string, usage repository.ObservabilityUsageSummary) error {
		values := map[string]string{"section": section + "Tokens", "key": key, "title": title, "coverageCount": strconv.Itoa(usage.Tokens.CoverageCount), "totalCount": strconv.Itoa(usage.Tokens.TotalRunCount), "available": strconv.FormatBool(usage.Tokens.Available)}
		setInt64(values, "inputTokens", usage.Tokens.InputTokens)
		setInt64(values, "outputTokens", usage.Tokens.OutputTokens)
		setInt64(values, "totalTokens", usage.Tokens.TotalTokens)
		if err := row(values); err != nil {
			return err
		}
		if err := row(map[string]string{"section": section + "Costs", "key": key, "title": title, "coverageCount": strconv.Itoa(usage.Costs.CoverageCount), "totalCount": strconv.Itoa(usage.Costs.TotalRunCount), "available": strconv.FormatBool(usage.Costs.Available)}); err != nil {
			return err
		}
		for _, cost := range usage.Costs.Currencies {
			if err := row(map[string]string{"section": section + "CostCurrency", "key": key, "title": title, "costAmount": cost.Amount, "costCurrency": cost.Currency, "coverageCount": strconv.Itoa(cost.CoverageCount), "totalCount": strconv.Itoa(usage.Costs.TotalRunCount), "available": "true"}); err != nil {
				return err
			}
		}
		return nil
	}
	if err := writeUsage("usageOverall", "overall", "", document.Summary.Aggregates.Usage.Overall); err != nil {
		return nil, err
	}
	usageRows := []struct {
		section string
		groups  []repository.ObservabilityUsageGroup
	}{
		{"usageProvider", document.Summary.Aggregates.Usage.ByProvider},
		{"usageRequirement", document.Summary.Aggregates.Usage.ByRequirement},
		{"usagePlan", document.Summary.Aggregates.Usage.ByPlan},
	}
	for _, grouped := range usageRows {
		for _, item := range grouped.groups {
			if err := writeUsage(grouped.section, item.Key, item.Title, item.ObservabilityUsageSummary); err != nil {
				return nil, err
			}
		}
	}
	writer.Flush()
	if err := writer.Error(); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func setUUIDString(values map[string]string, key string, value *uuid.UUID) {
	if value != nil {
		values[key] = value.String()
	}
}
func setString(values map[string]string, key string, value *string) {
	if value != nil {
		values[key] = *value
	}
}
func setInt(values map[string]string, key string, value *int) {
	if value != nil {
		values[key] = strconv.Itoa(*value)
	}
}
func setInt64(values map[string]string, key string, value *int64) {
	if value != nil {
		values[key] = strconv.FormatInt(*value, 10)
	}
}

const defaultAgentRunLogLines = 50
const maxAgentRunLogLines = 200

type agentRunLogResponse struct {
	RunID      uuid.UUID `json:"runId"`
	Status     string    `json:"status"`
	Provider   string    `json:"provider"`
	Lines      []string  `json:"lines"`
	SizeBytes  int64     `json:"sizeBytes"`
	HasMore    bool      `json:"hasMore"`
	NextBefore *int64    `json:"nextBefore,omitempty"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

func (s *Server) agentRunLog(w http.ResponseWriter, r *http.Request) {
	runID, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	limit := defaultAgentRunLogLines
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > maxAgentRunLogLines {
			writeError(w, r, http.StatusBadRequest, "invalid_limit", "limit must be between 1 and 200", nil)
			return
		}
		limit = parsed
	}
	var before *int64
	if raw := r.URL.Query().Get("before"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed < 0 {
			writeError(w, r, http.StatusBadRequest, "invalid_before", "before must be a non-negative byte cursor", nil)
			return
		}
		before = &parsed
	}
	run, err := s.Store.GetAgentRun(r.Context(), runID)
	if err != nil {
		respond(w, r, nil, err)
		return
	}
	lines, size, hasMore, nextBefore, updatedAt, err := readAgentRunLog(s.DataDir, run.LogPath, before, limit)
	if err != nil {
		respond(w, r, nil, err)
		return
	}
	writeJSON(w, http.StatusOK, agentRunLogResponse{RunID: run.ID, Status: run.Status, Provider: run.Provider, Lines: lines, SizeBytes: size, HasMore: hasMore, NextBefore: nextBefore, UpdatedAt: updatedAt})
}

// readAgentRunLog returns complete physical log lines in chronological order.  before is
// an exclusive byte cursor: nil reads the newest lines, while a returned NextBefore can
// be supplied to retrieve the preceding page. No entry is cut by an arbitrary byte size.
func readAgentRunLog(dataDir, logPath string, before *int64, limit int) ([]string, int64, bool, *int64, time.Time, error) {
	if strings.TrimSpace(dataDir) == "" {
		return nil, 0, false, nil, time.Time{}, errors.New("data directory is not configured")
	}
	target, err := filepath.Abs(logPath)
	if err != nil {
		return nil, 0, false, nil, time.Time{}, err
	}
	roots, err := agentRunLogRoots(dataDir)
	if err != nil {
		return nil, 0, false, nil, time.Time{}, err
	}
	if !pathWithinAnyRoot(roots, target) {
		return nil, 0, false, nil, time.Time{}, errors.New("agent log path is outside the application log directories")
	}
	body, err := os.ReadFile(target)
	if os.IsNotExist(err) {
		return []string{}, 0, false, nil, time.Now(), nil
	}
	if err != nil {
		return nil, 0, false, nil, time.Time{}, err
	}
	info, err := os.Stat(target)
	if err != nil {
		return nil, 0, false, nil, time.Time{}, err
	}
	if limit <= 0 {
		limit = defaultAgentRunLogLines
	}
	size := int64(len(body))
	end := size
	if before != nil && *before < end {
		end = *before
	}
	segment := body[:end]
	if len(segment) > 0 && segment[len(segment)-1] == '\n' {
		segment = segment[:len(segment)-1]
	}
	lines := make([]string, 0, limit)
	cut := len(segment)
	for len(lines) < limit && cut > 0 {
		separator := bytes.LastIndexByte(segment[:cut], '\n')
		lineStart := separator + 1
		lines = append(lines, string(segment[lineStart:cut]))
		cut = separator
	}
	for left, right := 0, len(lines)-1; left < right; left, right = left+1, right-1 {
		lines[left], lines[right] = lines[right], lines[left]
	}
	hasMore := cut > 0
	var nextBefore *int64
	if hasMore {
		cursor := int64(cut)
		nextBefore = &cursor
	}
	return lines, size, hasMore, nextBefore, info.ModTime(), nil
}

const desktopAppIdentifier = "com.lyming99.specrelay"

// agentRunLogRoots returns the current backend's log directory plus the two
// historical locations used by SpecRelay on the same local user account:
// direct host-backend configurations and the packaged Tauri desktop app.
//
// Agent-run paths live in the shared database. A desktop app can therefore
// display a run that was produced earlier by a host backend (and vice versa).
// Restricting reads to only the *current* DATA_DIR made those legitimate logs
// look like path traversal attempts. The list remains deliberately fixed to
// SpecRelay-owned data locations; arbitrary paths from the database are never
// accepted.
func agentRunLogRoots(dataDir string) ([]string, error) {
	candidates := []string{filepath.Join(dataDir, "logs")}
	if configDir, err := os.UserConfigDir(); err == nil && strings.TrimSpace(configDir) != "" {
		candidates = append(candidates,
			filepath.Join(configDir, "specrelay", "logs"),
			filepath.Join(configDir, "specrelay-production", "logs"),
			filepath.Join(configDir, desktopAppIdentifier, "data", "logs"),
		)
	}
	if dataHome := strings.TrimSpace(os.Getenv("XDG_DATA_HOME")); dataHome != "" {
		candidates = append(candidates, filepath.Join(dataHome, desktopAppIdentifier, "data", "logs"))
	} else if home, err := os.UserHomeDir(); err == nil && strings.TrimSpace(home) != "" {
		candidates = append(candidates,
			filepath.Join(home, ".local", "share", desktopAppIdentifier, "data", "logs"),
			filepath.Join(home, "Library", "Application Support", desktopAppIdentifier, "data", "logs"),
		)
	}

	roots := make([]string, 0, len(candidates))
	seen := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		if strings.TrimSpace(candidate) == "" {
			continue
		}
		root, err := filepath.Abs(candidate)
		if err != nil {
			return nil, err
		}
		if _, ok := seen[root]; ok {
			continue
		}
		seen[root] = struct{}{}
		roots = append(roots, root)
	}
	return roots, nil
}

func pathWithinAnyRoot(roots []string, target string) bool {
	for _, root := range roots {
		rel, err := filepath.Rel(root, target)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) && !filepath.IsAbs(rel) {
			return true
		}
	}
	return false
}

func (s *Server) probeAgent(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ProjectID uuid.UUID `json:"projectId"`
	}
	if err := decodeJSON(r, &in); err != nil {
		badJSON(w, r, err)
		return
	}
	if in.ProjectID == uuid.Nil {
		writeError(w, r, http.StatusBadRequest, "invalid_project_id", "projectId is required", nil)
		return
	}
	result, err := s.App.ProbeAgents(r.Context(), in.ProjectID)
	respond(w, r, result, err)
}

func (s *Server) rotateMCPToken(w http.ResponseWriter, r *http.Request) {
	token := randomToken()
	if err := s.Store.SaveAccessTokenHash(r.Context(), "mcp", "mcp", TokenHash(token)); err != nil {
		respond(w, r, nil, err)
		return
	}
	s.Auth.SetMCPToken(token)
	writeJSON(w, http.StatusCreated, map[string]string{"token": token})
}

const (
	defaultEventPageLimit = 10
	maxEventPageLimit     = 1000
)

type eventPageResponse struct {
	Items      []domain.Event `json:"items"`
	HasMore    bool           `json:"hasMore"`
	NextBefore *int64         `json:"nextBefore"`
}

func (s *Server) eventHistory(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	rawProjectID := query.Get("projectId")
	projectID, err := uuid.Parse(rawProjectID)
	if err != nil {
		message := "projectId must be a UUID"
		if rawProjectID == "" {
			message = "projectId is required"
		}
		writeError(w, r, http.StatusBadRequest, "invalid_project_id", message, nil)
		return
	}

	var before *int64
	if query.Has("before") {
		value, parseErr := strconv.ParseInt(query.Get("before"), 10, 64)
		if parseErr != nil || value < 1 {
			writeError(w, r, http.StatusBadRequest, "invalid_before_cursor", "before must be a positive event ID", nil)
			return
		}
		before = &value
	}

	limit := defaultEventPageLimit
	if query.Has("limit") {
		value, parseErr := strconv.Atoi(query.Get("limit"))
		if parseErr != nil || value < 1 || value > maxEventPageLimit {
			writeError(w, r, http.StatusBadRequest, "invalid_limit", "limit must be between 1 and 1000", nil)
			return
		}
		limit = value
	}

	page, err := s.Store.ListEventPage(r.Context(), projectID, before, limit)
	if err != nil {
		respond(w, r, nil, err)
		return
	}
	writeJSON(w, http.StatusOK, eventPageResponse{Items: page.Items, HasMore: page.HasMore, NextBefore: page.NextBefore})
}
func (s *Server) eventStream(w http.ResponseWriter, r *http.Request) {
	after := int64(0)
	if raw := r.Header.Get("Last-Event-ID"); raw != "" {
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || value < 0 {
			writeError(w, r, http.StatusBadRequest, "invalid_last_event_id", "Last-Event-ID must be a non-negative event ID", nil)
			return
		}
		after = value
	}
	if query := r.URL.Query(); query.Has("after") {
		value, err := strconv.ParseInt(query.Get("after"), 10, 64)
		if err != nil || value < 0 {
			writeError(w, r, http.StatusBadRequest, "invalid_after_cursor", "after must be a non-negative event ID", nil)
			return
		}
		if value > after {
			after = value
		}
	}
	var projectID *uuid.UUID
	if raw := r.URL.Query().Get("projectId"); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			writeError(w, r, http.StatusBadRequest, "invalid_project_id", "projectId must be a UUID", nil)
			return
		}
		projectID = &id
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, r, http.StatusInternalServerError, "streaming_unsupported", "Streaming is unavailable", nil)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	_, wake, unsubscribe := s.Broker.Subscribe()
	defer unsubscribe()
	keepalive := time.NewTicker(15 * time.Second)
	fallback := time.NewTicker(2 * time.Second)
	defer keepalive.Stop()
	defer fallback.Stop()
	for {
		events, err := s.Store.ListEvents(r.Context(), projectID, after, 500)
		if err != nil {
			return
		}
		for _, event := range events {
			data, _ := json.Marshal(event)
			fmt.Fprintf(w, "id: %d\ndata: %s\n\n", event.ID, data)
			after = event.ID
		}
		if len(events) > 0 {
			flusher.Flush()
			continue
		}
		select {
		case <-r.Context().Done():
			return
		case <-wake:
		case <-fallback.C:
		case <-keepalive.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}
func decodeJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	const maxBody = int64(2 << 20)
	limited := &io.LimitedReader{R: r.Body, N: maxBody + 1}
	decoder := json.NewDecoder(limited)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(v); err != nil {
		return err
	}
	if limited.N <= 0 {
		return errors.New("request body exceeds 2 MiB")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("request body must contain one JSON value")
		}
		return err
	}
	return nil
}
func pathUUID(w http.ResponseWriter, r *http.Request, name string) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue(name))
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_id", name+" must be a UUID", nil)
		return uuid.Nil, false
	}
	return id, true
}
func respond(w http.ResponseWriter, r *http.Request, v any, err error) {
	respondStatus(w, r, http.StatusOK, v, err)
}
func respondStatus(w http.ResponseWriter, r *http.Request, status int, v any, err error) {
	if err == nil {
		writeJSON(w, status, v)
		return
	}
	switch {
	case app.IsDriftBlocked(err):
		blocked, _ := app.DriftBlock(err)
		writeError(w, r, http.StatusConflict, "execution_context_drift", "Execution context changed and requires review", blocked.Report)
	case errors.Is(err, domain.ErrPlanDriftResolutionRequired):
		writeError(w, r, http.StatusConflict, "execution_context_not_confirmable", "Execution context must be repaired or the plan regenerated", nil)
	case errors.Is(err, domain.ErrNotFound):
		writeError(w, r, http.StatusNotFound, "resource_not_found", "Resource not found", nil)
	case errors.Is(err, domain.ErrForbidden):
		writeError(w, r, http.StatusForbidden, "resource_forbidden", "Resource is not accessible in this project", nil)
	case errors.Is(err, domain.ErrInvalidDiffRange):
		writeError(w, r, http.StatusBadRequest, "invalid_diff_range", "Diff line range must be complete and contained in the selected hunk", nil)
	case errors.Is(err, domain.ErrInvalidFeedbackLink):
		writeError(w, r, http.StatusBadRequest, "invalid_feedback_relation", "Feedback associations must form one consistent requirement, plan, task, checkpoint, file, and diff chain", nil)
	case errors.Is(err, domain.ErrVersionConflict):
		writeError(w, r, http.StatusConflict, "resource_version_conflict", "Resource version does not match", nil)
	case errors.Is(err, domain.ErrInvalidTransition):
		writeError(w, r, http.StatusConflict, "invalid_state_transition", err.Error(), nil)
	case agent.IsInvalidProvider(err):
		writeError(w, r, http.StatusBadRequest, "invalid_provider", err.Error(), nil)
	case pgconn.SafeToRetry(err):
		writeError(w, r, http.StatusServiceUnavailable, "database_unavailable", "Database is unavailable", nil)
	default:
		slog.Error("api request failed", "requestId", w.Header().Get("X-Request-ID"), "error", err)
		writeError(w, r, http.StatusBadRequest, "request_failed", err.Error(), nil)
	}
}
func badJSON(w http.ResponseWriter, r *http.Request, err error) {
	writeError(w, r, http.StatusBadRequest, "invalid_json", err.Error(), nil)
}
func writeError(w http.ResponseWriter, r *http.Request, status int, code, message string, details any) {
	writeJSON(w, status, apiError{Code: code, Message: message, Details: details, RequestID: w.Header().Get("X-Request-ID")})
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func spaHandler(dir string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := filepath.Join(dir, filepath.Clean(r.URL.Path))
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			http.ServeFile(w, r, path)
			return
		}
		http.ServeFile(w, r, filepath.Join(dir, "index.html"))
	})
}

var _ *multipart.FileHeader
