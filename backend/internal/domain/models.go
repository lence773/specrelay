package domain

import (
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"time"
)

type Project struct {
	ID                uuid.UUID `json:"id"`
	Name              string    `json:"name"`
	Description       string    `json:"description"`
	WorkspacePath     string    `json:"workspacePath"`
	AutomationEnabled bool      `json:"automationEnabled"`
	CreatedAt         time.Time `json:"createdAt"`
	UpdatedAt         time.Time `json:"updatedAt"`
	Version           int64     `json:"version"`
}
type ProjectSettings struct {
	ProjectID                 uuid.UUID       `json:"projectId"`
	ValidationCommand         string          `json:"validationCommand"`
	AgentProvider             string          `json:"agentProvider"`
	CodexCommand              string          `json:"codexCommand"`
	CodexArgs                 json.RawMessage `json:"codexArgs"`
	ClaudeCommand             string          `json:"claudeCommand"`
	ClaudeArgs                json.RawMessage `json:"claudeArgs"`
	PlanGenerationTimeoutSecs int             `json:"planGenerationTimeoutSeconds"`
	TaskExecutionTimeoutSecs  int             `json:"taskExecutionTimeoutSeconds"`
	MaxRetries                int             `json:"maxRetries"`
	AllowedEnv                json.RawMessage `json:"allowedEnv"`
	CreatedAt                 time.Time       `json:"createdAt"`
	UpdatedAt                 time.Time       `json:"updatedAt"`
	Version                   int64           `json:"version"`
}
type Intake struct {
	ID             uuid.UUID       `json:"id"`
	ProjectID      uuid.UUID       `json:"projectId"`
	Kind           string          `json:"kind"`
	ParentIntakeID *uuid.UUID      `json:"parentIntakeId,omitempty"`
	Title          string          `json:"title"`
	Body           string          `json:"body"`
	Status         string          `json:"status"`
	ConfigSnapshot json.RawMessage `json:"configSnapshot"`
	CreatedAt      time.Time       `json:"createdAt"`
	UpdatedAt      time.Time       `json:"updatedAt"`
	Version        int64           `json:"version"`
}
type Plan struct {
	ID             uuid.UUID       `json:"id"`
	ProjectID      uuid.UUID       `json:"projectId"`
	IntakeID       uuid.UUID       `json:"intakeId"`
	Title          string          `json:"title"`
	Spec           json.RawMessage `json:"spec"`
	Markdown       string          `json:"markdown"`
	Status         string          `json:"status"`
	ConfigSnapshot json.RawMessage `json:"configSnapshot"`
	CreatedAt      time.Time       `json:"createdAt"`
	UpdatedAt      time.Time       `json:"updatedAt"`
	Version        int64           `json:"version"`
}
type PlanTask struct {
	ID         uuid.UUID       `json:"id"`
	ProjectID  uuid.UUID       `json:"projectId"`
	PlanID     uuid.UUID       `json:"planId"`
	TaskKey    string          `json:"taskKey"`
	Position   int             `json:"position"`
	Title      string          `json:"title"`
	Scope      json.RawMessage `json:"scope"`
	Acceptance json.RawMessage `json:"acceptance"`
	Status     string          `json:"status"`
	SessionID  string          `json:"sessionId,omitempty"`
	StartedAt  *time.Time      `json:"startedAt,omitempty"`
	FinishedAt *time.Time      `json:"finishedAt,omitempty"`
	CreatedAt  time.Time       `json:"createdAt"`
	UpdatedAt  time.Time       `json:"updatedAt"`
	Version    int64           `json:"version"`
}
type Job struct {
	ID             uuid.UUID       `json:"id"`
	ProjectID      uuid.UUID       `json:"projectId"`
	Type           string          `json:"type"`
	AggregateType  string          `json:"aggregateType"`
	AggregateID    uuid.UUID       `json:"aggregateId"`
	Payload        json.RawMessage `json:"payload"`
	Priority       int             `json:"priority"`
	Status         string          `json:"status"`
	RunAfter       time.Time       `json:"runAfter"`
	WorkerID       string          `json:"workerId,omitempty"`
	LeaseExpiresAt *time.Time      `json:"leaseExpiresAt,omitempty"`
	Attempt        int             `json:"attempt"`
	MaxAttempts    int             `json:"maxAttempts"`
	LastError      string          `json:"lastError,omitempty"`
	IdempotencyKey string          `json:"idempotencyKey"`
	CreatedAt      time.Time       `json:"createdAt"`
	UpdatedAt      time.Time       `json:"updatedAt"`
	Version        int64           `json:"version"`
}
type Event struct {
	ID              int64           `json:"id"`
	ProjectID       *uuid.UUID      `json:"projectId,omitempty"`
	Type            string          `json:"type"`
	AggregateType   string          `json:"aggregateType"`
	AggregateID     uuid.UUID       `json:"aggregateId"`
	ResourceVersion int64           `json:"resourceVersion"`
	Payload         json.RawMessage `json:"payload"`
	OccurredAt      time.Time       `json:"occurredAt"`
}
type AgentRun struct {
	ID                uuid.UUID  `json:"id"`
	ProjectID         uuid.UUID  `json:"projectId"`
	JobID             *uuid.UUID `json:"jobId,omitempty"`
	TaskID            *uuid.UUID `json:"taskId,omitempty"`
	Provider          string     `json:"provider"`
	CommandSummary    string     `json:"commandSummary"`
	PID               *int       `json:"pid,omitempty"`
	SessionID         string     `json:"sessionId,omitempty"`
	Status            string     `json:"status"`
	ExitCode          *int       `json:"exitCode,omitempty"`
	DurationMS        int64      `json:"durationMs"`
	LogPath           string     `json:"-"`
	TerminationReason string     `json:"terminationReason,omitempty"`
	StartedAt         time.Time  `json:"startedAt"`
	FinishedAt        *time.Time `json:"finishedAt,omitempty"`
	CreatedAt         time.Time  `json:"createdAt"`
	UpdatedAt         time.Time  `json:"updatedAt"`
	Version           int64      `json:"version"`
}
type Attachment struct {
	ID           uuid.UUID `json:"id"`
	ProjectID    uuid.UUID `json:"projectId"`
	IntakeID     uuid.UUID `json:"intakeId"`
	OriginalName string    `json:"originalName"`
	MimeType     string    `json:"mimeType"`
	SizeBytes    int64     `json:"sizeBytes"`
	SHA256       string    `json:"sha256"`
	StoragePath  string    `json:"-"`
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
	Version      int64     `json:"version"`
}

var ErrVersionConflict = errors.New("resource version conflict")
var ErrNotFound = errors.New("resource not found")
var ErrInvalidTransition = errors.New("invalid state transition")
var transitions = map[string]map[string]map[string]bool{
	"intake": {"open": {"planning": true, "closed": true}, "planning": {"planned": true, "plan_failed": true, "open": true}, "planned": {"closed": true}, "plan_failed": {"planning": true, "closed": true}},
	"plan":   {"generating": {"ready": true, "blocked": true, "cancelled": true}, "ready": {"running": true, "cancelled": true}, "running": {"validating": true, "blocked": true, "cancelled": true}, "validating": {"completed": true, "blocked": true, "cancelled": true}, "blocked": {"ready": true, "running": true, "cancelled": true}},
	"task":   {"pending": {"queued": true, "cancelled": true}, "queued": {"running": true, "pending": true, "cancelled": true}, "running": {"succeeded": true, "failed": true, "cancelled": true, "pending": true}, "failed": {"queued": true, "cancelled": true}, "cancelled": {"queued": true, "pending": true}},
	"job":    {"queued": {"leased": true, "cancelled": true}, "retry_wait": {"queued": true, "cancelled": true}, "leased": {"running": true, "queued": true, "cancelled": true}, "running": {"succeeded": true, "retry_wait": true, "failed": true, "cancelled": true}},
}

func ValidateTransition(resource, from, to string) error {
	if from == to {
		return nil
	}
	if transitions[resource] != nil && transitions[resource][from][to] {
		return nil
	}
	return ErrInvalidTransition
}
