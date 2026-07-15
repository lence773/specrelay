package domain

import (
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
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

const (
	PlanSnapshotKindGenerationBaseline = "generation_baseline"
	PlanSnapshotKindTaskCheckpoint     = "task_checkpoint"
	PlanSnapshotKindUserAccepted       = "user_accepted"

	PlanDriftStatusClean           = "clean"
	PlanDriftStatusDetected        = "drifted"
	PlanDriftStatusMissingBaseline = "missing_baseline"

	PlanDriftAuditSnapshotUpdated    = "snapshot_updated"
	PlanDriftAuditPlanRegenerated    = "plan_regenerated"
	PlanDriftAuditExecutionAbandoned = "execution_abandoned"
)

type Plan struct {
	ID                        uuid.UUID       `json:"id"`
	ProjectID                 uuid.UUID       `json:"projectId"`
	IntakeID                  uuid.UUID       `json:"intakeId"`
	Title                     string          `json:"title"`
	Spec                      json.RawMessage `json:"spec"`
	SpecVersion               int             `json:"specVersion"`
	CompatibilityMode         bool            `json:"compatibilityMode"`
	Executable                bool            `json:"executable"`
	ValidationProblems        json.RawMessage `json:"validationProblems"`
	Markdown                  string          `json:"markdown"`
	Status                    string          `json:"status"`
	DeliveryStatus            string          `json:"deliveryStatus"`
	AcceptanceSummary         json.RawMessage `json:"acceptanceSummary"`
	ConfigSnapshot            json.RawMessage `json:"configSnapshot"`
	CreatedAt                 time.Time       `json:"createdAt"`
	UpdatedAt                 time.Time       `json:"updatedAt"`
	Version                   int64           `json:"version"`
	ContentVersion            int64           `json:"contentVersion"`
	ExecutionSnapshotID       *uuid.UUID      `json:"executionSnapshotId,omitempty"`
	ExecutionSnapshotSequence int64           `json:"executionSnapshotSequence"`
	DriftStatus               string          `json:"driftStatus"`
	DriftResolutionRequired   bool            `json:"driftResolutionRequired"`
}

type PlanExecutionSnapshot struct {
	ID                      uuid.UUID                   `json:"id"`
	ProjectID               uuid.UUID                   `json:"projectId"`
	PlanID                  uuid.UUID                   `json:"planId"`
	IntakeID                uuid.UUID                   `json:"intakeId"`
	PreviousSnapshotID      *uuid.UUID                  `json:"previousSnapshotId,omitempty"`
	TaskID                  *uuid.UUID                  `json:"taskId,omitempty"`
	Sequence                int64                       `json:"sequence"`
	Kind                    string                      `json:"kind"`
	RequirementID           uuid.UUID                   `json:"requirementId"`
	RequirementVersion      int64                       `json:"requirementVersion"`
	RequirementDigest       string                      `json:"requirementDigest"`
	PlanResourceVersion     int64                       `json:"planResourceVersion"`
	PlanContentVersion      int64                       `json:"planContentVersion"`
	PlanSpecDigest          string                      `json:"planSpecDigest"`
	ProjectVersion          int64                       `json:"projectVersion"`
	ConfigVersion           int64                       `json:"configVersion"`
	KeyExecutionFields      json.RawMessage             `json:"keyExecutionFields"`
	GenerationProvider      string                      `json:"generationProvider"`
	ExecutionProvider       string                      `json:"executionProvider"`
	WorkspacePathNormalized string                      `json:"workspacePathNormalized"`
	GitRoot                 string                      `json:"gitRoot"`
	GitRepositoryIdentity   string                      `json:"gitRepositoryIdentity"`
	GitBranch               string                      `json:"gitBranch"`
	GitHead                 string                      `json:"gitHead"`
	GitWorkspaceDigest      string                      `json:"gitWorkspaceDigest"`
	ChangeSummary           json.RawMessage             `json:"changeSummary"`
	Additions               int                         `json:"additions"`
	Deletions               int                         `json:"deletions"`
	Files                   []PlanExecutionSnapshotFile `json:"files"`
	CreatedAt               time.Time                   `json:"createdAt"`
}

type PlanExecutionSnapshotFile struct {
	ID           uuid.UUID                   `json:"id"`
	SnapshotID   uuid.UUID                   `json:"snapshotId"`
	Sequence     int                         `json:"sequence"`
	Path         string                      `json:"path"`
	PreviousPath string                      `json:"previousPath,omitempty"`
	Status       string                      `json:"status"`
	Staged       bool                        `json:"staged"`
	Binary       bool                        `json:"binary"`
	Additions    int                         `json:"additions"`
	Deletions    int                         `json:"deletions"`
	Hunks        []PlanExecutionSnapshotHunk `json:"hunks"`
	CreatedAt    time.Time                   `json:"createdAt"`
}

type PlanExecutionSnapshotHunk struct {
	ID           uuid.UUID `json:"id"`
	FileID       uuid.UUID `json:"fileId"`
	Sequence     int       `json:"sequence"`
	Header       string    `json:"header"`
	Patch        string    `json:"patch"`
	OldStartLine int       `json:"oldStartLine"`
	OldLineCount int       `json:"oldLineCount"`
	NewStartLine int       `json:"newStartLine"`
	NewLineCount int       `json:"newLineCount"`
	CreatedAt    time.Time `json:"createdAt"`
}

type FeedbackLink struct {
	FeedbackID    uuid.UUID  `json:"feedbackId"`
	ProjectID     uuid.UUID  `json:"projectId"`
	RequirementID uuid.UUID  `json:"requirementId"`
	PlanID        *uuid.UUID `json:"planId,omitempty"`
	TaskID        *uuid.UUID `json:"taskId,omitempty"`
	CheckpointID  *uuid.UUID `json:"checkpointId,omitempty"`
	FileID        *uuid.UUID `json:"fileId,omitempty"`
	DiffHunkID    *uuid.UUID `json:"diffHunkId,omitempty"`
	DiffLineSide  string     `json:"diffLineSide,omitempty"`
	DiffLineStart *int       `json:"diffLineStart,omitempty"`
	DiffLineEnd   *int       `json:"diffLineEnd,omitempty"`
	CreatedAt     time.Time  `json:"createdAt"`
}

type FeedbackRevision struct {
	ID             uuid.UUID `json:"id"`
	FeedbackID     uuid.UUID `json:"feedbackId"`
	ProjectID      uuid.UUID `json:"projectId"`
	RequirementID  uuid.UUID `json:"requirementId"`
	RevisionIntake Intake    `json:"revisionIntake"`
	RevisionPlan   *Plan     `json:"revisionPlan,omitempty"`
	CreatedAt      time.Time `json:"createdAt"`
}

type FeedbackTrace struct {
	Feedback    Intake                     `json:"feedback"`
	Requirement Intake                     `json:"requirement"`
	Link        FeedbackLink               `json:"link"`
	Plan        *Plan                      `json:"plan,omitempty"`
	Task        *PlanTask                  `json:"task,omitempty"`
	Checkpoint  *PlanExecutionSnapshot     `json:"checkpoint,omitempty"`
	File        *PlanExecutionSnapshotFile `json:"file,omitempty"`
	DiffHunk    *PlanExecutionSnapshotHunk `json:"diffHunk,omitempty"`
	Revisions   []FeedbackRevision         `json:"revisions"`
}

type PlanDriftAudit struct {
	ID                 uuid.UUID       `json:"id"`
	ProjectID          uuid.UUID       `json:"projectId"`
	PlanID             uuid.UUID       `json:"planId"`
	Sequence           int64           `json:"sequence"`
	Action             string          `json:"action"`
	OriginalSnapshotID *uuid.UUID      `json:"originalSnapshotId,omitempty"`
	NewSnapshotID      *uuid.UUID      `json:"newSnapshotId,omitempty"`
	TargetPlanID       *uuid.UUID      `json:"targetPlanId,omitempty"`
	RawDiff            json.RawMessage `json:"rawDiff"`
	Channel            string          `json:"channel"`
	Reason             string          `json:"reason"`
	OccurredAt         time.Time       `json:"occurredAt"`
}

type PlanDrift struct {
	PlanID                      uuid.UUID       `json:"planId"`
	Status                      string          `json:"status"`
	BaselineSnapshotID          *uuid.UUID      `json:"baselineSnapshotId,omitempty"`
	BaselineSnapshotSequence    int64           `json:"baselineSnapshotSequence"`
	Differences                 json.RawMessage `json:"differences"`
	RequiresExplicitDisposition bool            `json:"requiresExplicitDisposition"`
}

type AgentSession struct {
	ID             uuid.UUID  `json:"id"`
	ProjectID      uuid.UUID  `json:"projectId"`
	IntakeID       *uuid.UUID `json:"intakeId,omitempty"`
	PlanID         *uuid.UUID `json:"planId,omitempty"`
	Provider       string     `json:"provider"`
	Purpose        string     `json:"purpose"`
	CLISessionID   string     `json:"cliSessionId"`
	ContextSummary string     `json:"contextSummary,omitempty"`
	LastTaskID     *uuid.UUID `json:"lastTaskId,omitempty"`
	Status         string     `json:"status"`
	CreatedAt      time.Time  `json:"createdAt"`
	UpdatedAt      time.Time  `json:"updatedAt"`
	Version        int64      `json:"version"`
}

const (
	PlanTaskTypeImplementation  = "implementation"
	PlanTaskTypeFinalValidation = "final_validation"

	AcceptanceStatusPending            = "pending"
	AcceptanceStatusPassed             = "passed"
	AcceptanceStatusFailed             = "failed"
	AcceptanceStatusSkipped            = "skipped"
	AcceptanceStatusManualConfirmation = "manual_confirmation"

	PlanDeliveryStatusPending            = "pending"
	PlanDeliveryStatusDelivered          = "delivered"
	PlanDeliveryStatusFailed             = "failed"
	PlanDeliveryStatusCancelled          = "cancelled"
	PlanDeliveryStatusManualConfirmation = "manual_confirmation"
)

type PlanTask struct {
	ID                   uuid.UUID       `json:"id"`
	ProjectID            uuid.UUID       `json:"projectId"`
	PlanID               uuid.UUID       `json:"planId"`
	TaskKey              string          `json:"taskKey"`
	TaskType             string          `json:"taskType"`
	Position             int             `json:"position"`
	ExecutionOrder       int             `json:"executionOrder"`
	DependencyKeys       json.RawMessage `json:"dependencyKeys"`
	Title                string          `json:"title"`
	Scope                json.RawMessage `json:"scope"`
	Inputs               json.RawMessage `json:"inputs"`
	Outputs              json.RawMessage `json:"outputs"`
	Risks                json.RawMessage `json:"risks"`
	ValidationCommands   json.RawMessage `json:"validationCommands"`
	Acceptance           json.RawMessage `json:"acceptance"`
	AcceptanceDefinition json.RawMessage `json:"acceptanceDefinition"`
	AcceptanceStatus     string          `json:"acceptanceStatus"`
	AcceptanceResult     json.RawMessage `json:"acceptanceResult"`
	Status               string          `json:"status"`
	SessionID            string          `json:"sessionId,omitempty"`
	StartedAt            *time.Time      `json:"startedAt,omitempty"`
	FinishedAt           *time.Time      `json:"finishedAt,omitempty"`
	CreatedAt            time.Time       `json:"createdAt"`
	UpdatedAt            time.Time       `json:"updatedAt"`
	Version              int64           `json:"version"`
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

const (
	AgentRunOperationRequirementDiscussion = "requirement_discussion"
	AgentRunOperationPlanGeneration        = "plan_generation"
	AgentRunOperationTaskExecution         = "task_execution"
	AgentRunOperationValidation            = "validation"

	AgentRunSessionModeNew              = "new"
	AgentRunSessionModeReused           = "reused"
	AgentRunSessionModeSnapshotRestored = "snapshot_restored"
	AgentRunSessionModeNotApplicable    = "not_applicable"

	AgentRunSessionInvalidationProviderSwitched = "provider_switched"
	AgentRunSessionInvalidationSessionNotFound  = "session_not_found"
	AgentRunSessionInvalidationRestoreFailed    = "restore_failed"
)

type AgentRun struct {
	ID                        uuid.UUID  `json:"id"`
	ProjectID                 uuid.UUID  `json:"projectId"`
	IntakeID                  *uuid.UUID `json:"intakeId,omitempty"`
	PlanID                    *uuid.UUID `json:"planId,omitempty"`
	JobID                     *uuid.UUID `json:"jobId,omitempty"`
	TaskID                    *uuid.UUID `json:"taskId,omitempty"`
	LogicalOperationID        *uuid.UUID `json:"logicalOperationId,omitempty"`
	OperationType             *string    `json:"operationType,omitempty"`
	JobAttempt                *int       `json:"jobAttempt,omitempty"`
	RetryCount                *int       `json:"retryCount,omitempty"`
	Provider                  string     `json:"provider"`
	CommandSummary            string     `json:"commandSummary"`
	PID                       *int       `json:"pid,omitempty"`
	SessionID                 *string    `json:"sessionId,omitempty"`
	SessionMode               *string    `json:"sessionMode,omitempty"`
	SessionInvalidationReason *string    `json:"sessionInvalidationReason,omitempty"`
	Status                    string     `json:"status"`
	ExitCode                  *int       `json:"exitCode,omitempty"`
	QueueWaitMS               *int64     `json:"queueWaitMs,omitempty"`
	DurationMS                *int64     `json:"durationMs,omitempty"`
	FailureCategory           *string    `json:"failureCategory,omitempty"`
	OutputBytes               *int64     `json:"outputBytes,omitempty"`
	OutputLines               *int64     `json:"outputLines,omitempty"`
	EventCount                *int64     `json:"eventCount,omitempty"`
	OutputTruncated           *bool      `json:"outputTruncated,omitempty"`
	InputTokens               *int64     `json:"inputTokens,omitempty"`
	OutputTokens              *int64     `json:"outputTokens,omitempty"`
	TotalTokens               *int64     `json:"totalTokens,omitempty"`
	CostAmount                *string    `json:"costAmount,omitempty"`
	CostCurrency              *string    `json:"costCurrency,omitempty"`
	LogPath                   string     `json:"-"`
	TerminationReason         *string    `json:"terminationReason,omitempty"`
	StartedAt                 time.Time  `json:"startedAt"`
	FinishedAt                *time.Time `json:"finishedAt,omitempty"`
	CreatedAt                 time.Time  `json:"createdAt"`
	UpdatedAt                 time.Time  `json:"updatedAt"`
	Version                   int64      `json:"version"`
}

const (
	TaskExecutionAttemptOriginInitial     = "initial"
	TaskExecutionAttemptOriginManualRetry = "manual_retry"
	TaskExecutionAttemptOriginQueueRetry  = "queue_retry"
	TaskExecutionAttemptOriginRecovery    = "recovery"

	TaskExecutionOutcomeSucceeded   = "succeeded"
	TaskExecutionOutcomeFailed      = "failed"
	TaskExecutionOutcomeInterrupted = "interrupted"
	TaskExecutionOutcomeCancelled   = "cancelled"

	TaskExecutionCheckpointBeforeExecution = "before_execution"
	TaskExecutionCheckpointAfterExecution  = "after_execution"
	TaskExecutionCheckpointBeforeRollback  = "before_rollback"
	TaskExecutionCheckpointAfterRollback   = "after_rollback"

	GitReferenceStateBranch   = "branch"
	GitReferenceStateDetached = "detached"
	GitReferenceStateUnborn   = "unborn"

	TaskExecutionFileChangeAdded       = "added"
	TaskExecutionFileChangeModified    = "modified"
	TaskExecutionFileChangeDeleted     = "deleted"
	TaskExecutionFileChangeRenamed     = "renamed"
	TaskExecutionFileChangeCopied      = "copied"
	TaskExecutionFileChangeTypeChanged = "type_changed"
	TaskExecutionFileChangeUnmerged    = "unmerged"
	TaskExecutionFileChangeUntracked   = "untracked"

	TaskExecutionValidationPassed   = "passed"
	TaskExecutionValidationFailed   = "failed"
	TaskExecutionValidationTimedOut = "timed_out"
	TaskExecutionValidationSkipped  = "skipped"
	TaskExecutionValidationError    = "error"

	TaskExecutionRollbackKindManual    = "manual"
	TaskExecutionRollbackKindAutomatic = "automatic"
	TaskExecutionRollbackKindRecovery  = "recovery"

	TaskExecutionRollbackRequested = "requested"
	TaskExecutionRollbackRunning   = "running"
	TaskExecutionRollbackSucceeded = "succeeded"
	TaskExecutionRollbackFailed    = "failed"
	TaskExecutionRollbackCancelled = "cancelled"
)

// TaskExecutionAttempt is an immutable record of one concrete worker attempt.
// AttemptSequence is scoped to a task; QueueAttempt preserves the queue's own
// retry number so manual retries and queue retries remain distinguishable.
type TaskExecutionAttempt struct {
	ID                  uuid.UUID  `json:"id"`
	ProjectID           uuid.UUID  `json:"projectId"`
	PlanID              uuid.UUID  `json:"planId"`
	TaskID              uuid.UUID  `json:"taskId"`
	JobID               uuid.UUID  `json:"jobId"`
	AgentRunID          uuid.UUID  `json:"agentRunId"`
	AttemptSequence     int64      `json:"attemptSequence"`
	AttemptOrigin       string     `json:"attemptOrigin"`
	QueueAttempt        int        `json:"queueAttempt"`
	SupersedesAttemptID *uuid.UUID `json:"supersedesAttemptId,omitempty"`
	Outcome             string     `json:"outcome"`
	StartedAt           time.Time  `json:"startedAt"`
	FinishedAt          time.Time  `json:"finishedAt"`
	CreatedAt           time.Time  `json:"createdAt"`
}

// TaskExecutionCheckpoint captures a complete immutable Git/workspace state.
// Phase differentiates the state before and after an attempt without making a
// task ID a mutable checkpoint key.
type TaskExecutionCheckpoint struct {
	ID                    uuid.UUID       `json:"id"`
	AttemptID             uuid.UUID       `json:"attemptId"`
	ProjectID             uuid.UUID       `json:"projectId"`
	PlanID                uuid.UUID       `json:"planId"`
	TaskID                uuid.UUID       `json:"taskId"`
	JobID                 uuid.UUID       `json:"jobId"`
	AgentRunID            uuid.UUID       `json:"agentRunId"`
	CheckpointSequence    int64           `json:"checkpointSequence"`
	Phase                 string          `json:"phase"`
	GitReferenceState     string          `json:"gitReferenceState"`
	CurrentBranch         string          `json:"currentBranch"`
	HeadOID               string          `json:"headOid"`
	IndexTreeOID          string          `json:"indexTreeOid"`
	WorkspaceTreeOID      string          `json:"workspaceTreeOid"`
	GitStatusSummary      json.RawMessage `json:"gitStatusSummary"`
	GitStatusFingerprint  string          `json:"gitStatusFingerprint"`
	TaskKey               string          `json:"taskKey"`
	ProjectConfigSnapshot json.RawMessage `json:"projectConfigSnapshot"`
	CreatedAt             time.Time       `json:"createdAt"`
}

type TaskExecutionFileChange struct {
	ID               uuid.UUID       `json:"id"`
	AttemptID        uuid.UUID       `json:"attemptId"`
	ProjectID        uuid.UUID       `json:"projectId"`
	PlanID           uuid.UUID       `json:"planId"`
	TaskID           uuid.UUID       `json:"taskId"`
	JobID            uuid.UUID       `json:"jobId"`
	AgentRunID       uuid.UUID       `json:"agentRunId"`
	ChangeSequence   int64           `json:"changeSequence"`
	Path             string          `json:"path"`
	PreviousPath     string          `json:"previousPath,omitempty"`
	ChangeKind       string          `json:"changeKind"`
	Staged           bool            `json:"staged"`
	Binary           bool            `json:"binary"`
	Additions        *int            `json:"additions,omitempty"`
	Deletions        *int            `json:"deletions,omitempty"`
	BeforeBlobOID    string          `json:"beforeBlobOid,omitempty"`
	AfterBlobOID     string          `json:"afterBlobOid,omitempty"`
	PatchFingerprint string          `json:"patchFingerprint,omitempty"`
	Summary          json.RawMessage `json:"summary"`
	CreatedAt        time.Time       `json:"createdAt"`
}

type TaskExecutionValidation struct {
	ID                 uuid.UUID       `json:"id"`
	AttemptID          uuid.UUID       `json:"attemptId"`
	ProjectID          uuid.UUID       `json:"projectId"`
	PlanID             uuid.UUID       `json:"planId"`
	TaskID             uuid.UUID       `json:"taskId"`
	JobID              uuid.UUID       `json:"jobId"`
	AgentRunID         uuid.UUID       `json:"agentRunId"`
	ValidationSequence int64           `json:"validationSequence"`
	Command            string          `json:"command"`
	WorkingDirectory   string          `json:"workingDirectory"`
	Status             string          `json:"status"`
	ExitCode           *int            `json:"exitCode,omitempty"`
	StartedAt          *time.Time      `json:"startedAt,omitempty"`
	FinishedAt         *time.Time      `json:"finishedAt,omitempty"`
	StdoutFingerprint  string          `json:"stdoutFingerprint,omitempty"`
	StderrFingerprint  string          `json:"stderrFingerprint,omitempty"`
	OutputSummary      json.RawMessage `json:"outputSummary"`
	CreatedAt          time.Time       `json:"createdAt"`
}

type TaskExecutionRollback struct {
	ID                 uuid.UUID `json:"id"`
	AttemptID          uuid.UUID `json:"attemptId"`
	ProjectID          uuid.UUID `json:"projectId"`
	PlanID             uuid.UUID `json:"planId"`
	TaskID             uuid.UUID `json:"taskId"`
	JobID              uuid.UUID `json:"jobId"`
	AgentRunID         uuid.UUID `json:"agentRunId"`
	RollbackSequence   int64     `json:"rollbackSequence"`
	SourceCheckpointID uuid.UUID `json:"sourceCheckpointId"`
	TargetCheckpointID uuid.UUID `json:"targetCheckpointId"`
	RollbackKind       string    `json:"rollbackKind"`
	CommandSummary     string    `json:"commandSummary"`
	Reason             string    `json:"reason"`
	RequestedBy        string    `json:"requestedBy"`
	CreatedAt          time.Time `json:"createdAt"`
}

type TaskExecutionRollbackEvent struct {
	ID            uuid.UUID       `json:"id"`
	RollbackID    uuid.UUID       `json:"rollbackId"`
	AttemptID     uuid.UUID       `json:"attemptId"`
	ProjectID     uuid.UUID       `json:"projectId"`
	PlanID        uuid.UUID       `json:"planId"`
	TaskID        uuid.UUID       `json:"taskId"`
	JobID         uuid.UUID       `json:"jobId"`
	AgentRunID    uuid.UUID       `json:"agentRunId"`
	EventSequence int64           `json:"eventSequence"`
	Status        string          `json:"status"`
	Message       string          `json:"message"`
	Details       json.RawMessage `json:"details"`
	OccurredAt    time.Time       `json:"occurredAt"`
}

type TaskExecutionRollbackAudit struct {
	Operation TaskExecutionRollback        `json:"operation"`
	Status    string                       `json:"status"`
	Events    []TaskExecutionRollbackEvent `json:"events"`
}

// TaskExecutionClosure is the query aggregate for one attempt. Historical
// closures remain queryable after rollback or after a later retry supersedes it.
type TaskExecutionClosure struct {
	Attempt            TaskExecutionAttempt         `json:"attempt"`
	Checkpoints        []TaskExecutionCheckpoint    `json:"checkpoints"`
	FileChanges        []TaskExecutionFileChange    `json:"fileChanges"`
	ValidationEvidence []TaskExecutionValidation    `json:"validationEvidence"`
	Rollbacks          []TaskExecutionRollbackAudit `json:"rollbacks"`
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

type LifecycleResource string

type PlanStatus string
type TaskStatus string
type JobStatus string
type AgentRunStatus string

type LifecycleStatusSource string
type LifecycleReasonCode string
type LifecycleRecoveryHint string
type LifecycleTransitionDisposition string

const (
	LifecycleResourcePlan     LifecycleResource = "plan"
	LifecycleResourceTask     LifecycleResource = "task"
	LifecycleResourceJob      LifecycleResource = "job"
	LifecycleResourceAgentRun LifecycleResource = "agent_run"

	PlanStatusGenerating  PlanStatus = "generating"
	PlanStatusReady       PlanStatus = "ready"
	PlanStatusRunning     PlanStatus = "running"
	PlanStatusValidating  PlanStatus = "validating"
	PlanStatusBlocked     PlanStatus = "blocked"
	PlanStatusCancelling  PlanStatus = "cancelling"
	PlanStatusCompleted   PlanStatus = "completed"
	PlanStatusFailed      PlanStatus = "failed"
	PlanStatusInterrupted PlanStatus = "interrupted"
	PlanStatusCancelled   PlanStatus = "cancelled"

	TaskStatusPending     TaskStatus = "pending"
	TaskStatusQueued      TaskStatus = "queued"
	TaskStatusRunning     TaskStatus = "running"
	TaskStatusCancelling  TaskStatus = "cancelling"
	TaskStatusSucceeded   TaskStatus = "succeeded"
	TaskStatusFailed      TaskStatus = "failed"
	TaskStatusInterrupted TaskStatus = "interrupted"
	TaskStatusCancelled   TaskStatus = "cancelled"

	JobStatusQueued      JobStatus = "queued"
	JobStatusLeased      JobStatus = "leased"
	JobStatusRunning     JobStatus = "running"
	JobStatusRetryWait   JobStatus = "retry_wait"
	JobStatusCancelling  JobStatus = "cancelling"
	JobStatusSucceeded   JobStatus = "succeeded"
	JobStatusFailed      JobStatus = "failed"
	JobStatusInterrupted JobStatus = "interrupted"
	JobStatusCancelled   JobStatus = "cancelled"

	AgentRunStatusStarting    AgentRunStatus = "starting"
	AgentRunStatusRunning     AgentRunStatus = "running"
	AgentRunStatusCancelling  AgentRunStatus = "cancelling"
	AgentRunStatusSucceeded   AgentRunStatus = "succeeded"
	AgentRunStatusFailed      AgentRunStatus = "failed"
	AgentRunStatusInterrupted AgentRunStatus = "interrupted"
	AgentRunStatusCancelled   AgentRunStatus = "cancelled"
	// AgentRunStatusTimedOut is retained only so historical rows remain readable.
	// New plan generation, task execution, and validation runs must not enter it.
	AgentRunStatusTimedOut AgentRunStatus = "timed_out"

	LifecycleSourceUser       LifecycleStatusSource = "user"
	LifecycleSourceAutomation LifecycleStatusSource = "automation"
	LifecycleSourceWorker     LifecycleStatusSource = "worker"
	LifecycleSourceBackend    LifecycleStatusSource = "backend"
	LifecycleSourceRecovery   LifecycleStatusSource = "recovery"
	LifecycleSourceSystem     LifecycleStatusSource = "system"
	LifecycleSourceLegacy     LifecycleStatusSource = "legacy"

	LifecycleReasonCreated               LifecycleReasonCode = "created"
	LifecycleReasonCompleted             LifecycleReasonCode = "completed"
	LifecycleReasonAutomaticRetry        LifecycleReasonCode = "automatic_retry"
	LifecycleReasonCancellationRequested LifecycleReasonCode = "cancellation_requested"
	LifecycleReasonUserCancelled         LifecycleReasonCode = "user_cancelled"
	LifecycleReasonAutomationDisabled    LifecycleReasonCode = "automation_disabled"
	LifecycleReasonBackendShutdown       LifecycleReasonCode = "backend_shutdown"
	LifecycleReasonProcessLost           LifecycleReasonCode = "process_lost"
	LifecycleReasonExecutionFailed       LifecycleReasonCode = "execution_failed"
	LifecycleReasonValidationFailed      LifecycleReasonCode = "validation_failed"
	LifecycleReasonResumeRequested       LifecycleReasonCode = "resume_requested"
	LifecycleReasonHistoricalTimeout     LifecycleReasonCode = "historical_timeout"
	LifecycleReasonLegacyState           LifecycleReasonCode = "legacy_state"

	LifecycleRecoveryNone                 LifecycleRecoveryHint = "none"
	LifecycleRecoveryAutomaticRetry       LifecycleRecoveryHint = "automatic_retry"
	LifecycleRecoveryResumeFromCheckpoint LifecycleRecoveryHint = "resume_from_checkpoint"
	LifecycleRecoveryRetryFromStart       LifecycleRecoveryHint = "retry_from_start"
	LifecycleRecoveryManualReview         LifecycleRecoveryHint = "manual_review"

	LifecycleTransitionApplied    LifecycleTransitionDisposition = "applied"
	LifecycleTransitionIdempotent LifecycleTransitionDisposition = "idempotent"
)

// LifecycleMetadata is the durable explanation attached to a resource's
// current state. ExecutionCheckpoint must be a JSON object when present.
type LifecycleMetadata struct {
	StatusSource        LifecycleStatusSource `json:"statusSource"`
	ReasonCode          LifecycleReasonCode   `json:"reasonCode"`
	Reason              string                `json:"reason"`
	LastActivityAt      time.Time             `json:"lastActivityAt"`
	RecoveryHint        LifecycleRecoveryHint `json:"recoveryHint"`
	ExecutionCheckpoint json.RawMessage       `json:"executionCheckpoint"`
}

// LifecycleState is the common persisted projection for plans, tasks, jobs,
// and agent runs.
type LifecycleState struct {
	ResourceType LifecycleResource `json:"resourceType"`
	ResourceID   uuid.UUID         `json:"resourceId"`
	ProjectID    uuid.UUID         `json:"projectId"`
	Status       string            `json:"status"`
	Version      int64             `json:"version"`
	LifecycleMetadata
}

// LifecycleTransition is append-only audit data. FromStatus is empty only for
// the migration baseline that makes pre-contract records readable.
type LifecycleTransition struct {
	ID              int64             `json:"id"`
	ProjectID       uuid.UUID         `json:"projectId"`
	ResourceType    LifecycleResource `json:"resourceType"`
	ResourceID      uuid.UUID         `json:"resourceId"`
	ResourceVersion int64             `json:"resourceVersion"`
	FromStatus      string            `json:"fromStatus,omitempty"`
	ToStatus        string            `json:"toStatus"`
	LifecycleMetadata
	OccurredAt time.Time `json:"occurredAt"`
}

// PlanExecutionAcceptanceBlocker identifies an acceptance item that prevents a
// dependent task from being scheduled.
type PlanExecutionAcceptanceBlocker struct {
	Key    string `json:"key,omitempty"`
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

// PlanExecutionBlocker is a stable, presentation-ready explanation of one
// scheduling gate that was not satisfied. TaskID is nil for plan-level
// validation failures.
type PlanExecutionBlocker struct {
	Code               string                           `json:"code"`
	TaskID             *uuid.UUID                       `json:"taskId,omitempty"`
	TaskKey            string                           `json:"taskKey,omitempty"`
	TaskTitle          string                           `json:"taskTitle,omitempty"`
	TaskStatus         string                           `json:"taskStatus,omitempty"`
	AcceptanceStatus   string                           `json:"acceptanceStatus,omitempty"`
	Reason             string                           `json:"reason"`
	AcceptanceBlockers []PlanExecutionAcceptanceBlocker `json:"acceptanceBlockers,omitempty"`
	ValidationProblems json.RawMessage                  `json:"validationProblems,omitempty"`
}

// PlanExecutionBlockedError is returned by every scheduling entry point when
// the persisted plan graph cannot safely advance. It unwraps to both the
// specific scheduling sentinel and ErrInvalidTransition for compatibility with
// existing callers.
type PlanExecutionBlockedError struct {
	PlanID   uuid.UUID              `json:"planId"`
	TaskID   *uuid.UUID             `json:"taskId,omitempty"`
	Blockers []PlanExecutionBlocker `json:"blockers"`
}

func (e *PlanExecutionBlockedError) Error() string {
	if e == nil || len(e.Blockers) == 0 {
		return ErrPlanExecutionBlocked.Error()
	}
	reasons := make([]string, 0, len(e.Blockers))
	for _, blocker := range e.Blockers {
		reason := strings.TrimSpace(blocker.Reason)
		if reason != "" {
			reasons = append(reasons, reason)
		}
	}
	if len(reasons) == 0 {
		return ErrPlanExecutionBlocked.Error()
	}
	return ErrPlanExecutionBlocked.Error() + ": " + strings.Join(reasons, "; ")
}

func (e *PlanExecutionBlockedError) Unwrap() []error {
	return []error{ErrPlanExecutionBlocked, ErrInvalidTransition}
}

var ErrVersionConflict = errors.New("resource version conflict")
var ErrNotFound = errors.New("resource not found")
var ErrForbidden = errors.New("resource belongs to another project")
var ErrInvalidFeedbackLink = errors.New("invalid feedback association")
var ErrInvalidDiffRange = errors.New("invalid feedback diff line range")
var ErrInvalidTransition = errors.New("invalid state transition")
var ErrPlanExecutionBlocked = errors.New("plan execution is blocked")
var ErrInvalidLifecycleMetadata = errors.New("invalid lifecycle metadata")
var ErrPlanExecutionBaselineMissing = errors.New("plan execution baseline is missing")
var ErrPlanDriftResolutionRequired = errors.New("plan drift requires explicit disposition")
var ErrPlanDriftAuditReasonRequired = errors.New("plan drift audit reason is required")
var ErrInvalidPlanDriftAudit = errors.New("invalid plan drift audit")

func ValidatePlanDriftAudit(action, channel, reason string, rawDiff json.RawMessage) error {
	if strings.TrimSpace(reason) == "" {
		return ErrPlanDriftAuditReasonRequired
	}
	if strings.TrimSpace(channel) == "" || !json.Valid(rawDiff) {
		return ErrInvalidPlanDriftAudit
	}
	switch action {
	case PlanDriftAuditSnapshotUpdated, PlanDriftAuditPlanRegenerated, PlanDriftAuditExecutionAbandoned:
		return nil
	default:
		return ErrInvalidPlanDriftAudit
	}
}

type lifecycleContract struct {
	known       map[string]struct{}
	transitions map[string]map[string]struct{}
	terminal    map[string]struct{}
	recoverable map[string]struct{}
	cancelling  map[string]struct{}
}

func statusSet(values ...string) map[string]struct{} {
	out := make(map[string]struct{}, len(values))
	for _, value := range values {
		out[value] = struct{}{}
	}
	return out
}

func transitionSet(entries map[string][]string) map[string]map[string]struct{} {
	out := make(map[string]map[string]struct{}, len(entries))
	for from, destinations := range entries {
		out[from] = statusSet(destinations...)
	}
	return out
}

var lifecycleContracts = map[LifecycleResource]lifecycleContract{
	LifecycleResourcePlan: {
		known: statusSet("generating", "ready", "running", "validating", "blocked", "cancelling", "completed", "failed", "interrupted", "cancelled"),
		transitions: transitionSet(map[string][]string{
			"generating":  {"ready", "failed", "cancelling", "interrupted"},
			"ready":       {"running", "cancelled"},
			"running":     {"validating", "completed", "failed", "blocked", "cancelling", "interrupted"},
			"validating":  {"completed", "failed", "blocked", "cancelling", "interrupted"},
			"blocked":     {"ready", "running", "cancelled"},
			"cancelling":  {"cancelled", "interrupted"},
			"interrupted": {"generating", "ready", "running", "validating", "cancelled"},
		}),
		terminal:    statusSet("completed", "failed", "interrupted", "cancelled"),
		recoverable: statusSet("interrupted"),
		cancelling:  statusSet("cancelling"),
	},
	LifecycleResourceTask: {
		known: statusSet("pending", "queued", "running", "cancelling", "succeeded", "failed", "interrupted", "cancelled"),
		transitions: transitionSet(map[string][]string{
			"pending":     {"queued", "cancelled"},
			"queued":      {"running", "pending", "cancelled", "interrupted"},
			"running":     {"succeeded", "failed", "cancelling", "interrupted"},
			"cancelling":  {"cancelled", "interrupted"},
			"interrupted": {"pending", "queued", "cancelled"},
		}),
		terminal:    statusSet("succeeded", "failed", "interrupted", "cancelled"),
		recoverable: statusSet("interrupted"),
		cancelling:  statusSet("cancelling"),
	},
	LifecycleResourceJob: {
		known: statusSet("queued", "leased", "running", "retry_wait", "cancelling", "succeeded", "failed", "interrupted", "cancelled"),
		transitions: transitionSet(map[string][]string{
			"queued":      {"leased", "cancelled"},
			"retry_wait":  {"queued", "leased", "cancelled"},
			"leased":      {"running", "queued", "cancelled", "interrupted"},
			"running":     {"succeeded", "retry_wait", "failed", "cancelling", "interrupted"},
			"cancelling":  {"cancelled", "interrupted"},
			"interrupted": {"queued", "cancelled"},
		}),
		terminal:    statusSet("succeeded", "failed", "interrupted", "cancelled"),
		recoverable: statusSet("interrupted"),
		cancelling:  statusSet("cancelling"),
	},
	LifecycleResourceAgentRun: {
		known: statusSet("starting", "running", "cancelling", "succeeded", "failed", "interrupted", "cancelled", "timed_out"),
		transitions: transitionSet(map[string][]string{
			"starting":    {"running", "failed", "cancelling", "interrupted"},
			"running":     {"succeeded", "failed", "cancelling", "interrupted"},
			"cancelling":  {"cancelled", "interrupted"},
			"interrupted": {"starting", "cancelled"},
		}),
		terminal:    statusSet("succeeded", "failed", "interrupted", "cancelled", "timed_out"),
		recoverable: statusSet("interrupted"),
		cancelling:  statusSet("cancelling"),
	},
}

var intakeTransitions = transitionSet(map[string][]string{
	"open":        {"planning", "closed"},
	"planning":    {"planned", "plan_failed", "open"},
	"planned":     {"closed"},
	"plan_failed": {"planning", "closed"},
})
var intakeStatuses = statusSet("open", "planning", "planned", "plan_failed", "closed")

func ParseLifecycleResource(resource string) (LifecycleResource, bool) {
	switch strings.ToLower(strings.TrimSpace(resource)) {
	case "plan":
		return LifecycleResourcePlan, true
	case "task":
		return LifecycleResourceTask, true
	case "job":
		return LifecycleResourceJob, true
	case "agent_run", "agent-run", "agentrun":
		return LifecycleResourceAgentRun, true
	default:
		return "", false
	}
}

// EvaluateTransition applies the shared idempotency rule: replaying a known
// state to itself succeeds without changing metadata, version, or audit data.
func EvaluateTransition(resource, from, to string) (LifecycleTransitionDisposition, error) {
	if resource == "intake" {
		if _, ok := intakeStatuses[from]; !ok {
			return "", ErrInvalidTransition
		}
		if _, ok := intakeStatuses[to]; !ok {
			return "", ErrInvalidTransition
		}
		if from == to {
			return LifecycleTransitionIdempotent, nil
		}
		if _, ok := intakeTransitions[from][to]; ok {
			return LifecycleTransitionApplied, nil
		}
		return "", ErrInvalidTransition
	}
	resourceType, ok := ParseLifecycleResource(resource)
	if !ok {
		return "", ErrInvalidTransition
	}
	contract := lifecycleContracts[resourceType]
	if _, ok = contract.known[from]; !ok {
		return "", ErrInvalidTransition
	}
	if _, ok = contract.known[to]; !ok {
		return "", ErrInvalidTransition
	}
	if from == to {
		return LifecycleTransitionIdempotent, nil
	}
	if _, ok = contract.transitions[from][to]; ok {
		return LifecycleTransitionApplied, nil
	}
	return "", ErrInvalidTransition
}

func ValidateTransition(resource, from, to string) error {
	_, err := EvaluateTransition(resource, from, to)
	return err
}

func ValidatePlanTransition(from, to PlanStatus) error {
	return ValidateTransition(string(LifecycleResourcePlan), string(from), string(to))
}

func ValidateTaskTransition(from, to TaskStatus) error {
	return ValidateTransition(string(LifecycleResourceTask), string(from), string(to))
}

func ValidateJobTransition(from, to JobStatus) error {
	return ValidateTransition(string(LifecycleResourceJob), string(from), string(to))
}

func ValidateAgentRunTransition(from, to AgentRunStatus) error {
	return ValidateTransition(string(LifecycleResourceAgentRun), string(from), string(to))
}

func IsTerminalStatus(resource LifecycleResource, status string) bool {
	_, ok := lifecycleContracts[resource].terminal[status]
	return ok
}

func IsRecoverableTerminalStatus(resource LifecycleResource, status string) bool {
	_, ok := lifecycleContracts[resource].recoverable[status]
	return ok
}

func IsCancellingStatus(resource LifecycleResource, status string) bool {
	_, ok := lifecycleContracts[resource].cancelling[status]
	return ok
}

func (s PlanStatus) IsTerminal() bool { return IsTerminalStatus(LifecycleResourcePlan, string(s)) }
func (s PlanStatus) IsRecoverableTerminal() bool {
	return IsRecoverableTerminalStatus(LifecycleResourcePlan, string(s))
}
func (s PlanStatus) IsCancelling() bool { return IsCancellingStatus(LifecycleResourcePlan, string(s)) }
func (s TaskStatus) IsTerminal() bool   { return IsTerminalStatus(LifecycleResourceTask, string(s)) }
func (s TaskStatus) IsRecoverableTerminal() bool {
	return IsRecoverableTerminalStatus(LifecycleResourceTask, string(s))
}
func (s TaskStatus) IsCancelling() bool { return IsCancellingStatus(LifecycleResourceTask, string(s)) }
func (s JobStatus) IsTerminal() bool    { return IsTerminalStatus(LifecycleResourceJob, string(s)) }
func (s JobStatus) IsRecoverableTerminal() bool {
	return IsRecoverableTerminalStatus(LifecycleResourceJob, string(s))
}
func (s JobStatus) IsCancelling() bool { return IsCancellingStatus(LifecycleResourceJob, string(s)) }
func (s AgentRunStatus) IsTerminal() bool {
	return IsTerminalStatus(LifecycleResourceAgentRun, string(s))
}
func (s AgentRunStatus) IsRecoverableTerminal() bool {
	return IsRecoverableTerminalStatus(LifecycleResourceAgentRun, string(s))
}
func (s AgentRunStatus) IsCancelling() bool {
	return IsCancellingStatus(LifecycleResourceAgentRun, string(s))
}

func ValidateLifecycleMetadata(metadata LifecycleMetadata) error {
	if _, ok := statusSet(
		string(LifecycleSourceUser), string(LifecycleSourceAutomation), string(LifecycleSourceWorker),
		string(LifecycleSourceBackend), string(LifecycleSourceRecovery), string(LifecycleSourceSystem),
		string(LifecycleSourceLegacy),
	)[string(metadata.StatusSource)]; !ok {
		return ErrInvalidLifecycleMetadata
	}
	if strings.TrimSpace(string(metadata.ReasonCode)) == "" || strings.TrimSpace(metadata.Reason) == "" {
		return ErrInvalidLifecycleMetadata
	}
	if _, ok := statusSet(
		string(LifecycleRecoveryNone), string(LifecycleRecoveryAutomaticRetry),
		string(LifecycleRecoveryResumeFromCheckpoint), string(LifecycleRecoveryRetryFromStart),
		string(LifecycleRecoveryManualReview),
	)[string(metadata.RecoveryHint)]; !ok {
		return ErrInvalidLifecycleMetadata
	}
	if len(metadata.ExecutionCheckpoint) > 0 {
		var checkpoint map[string]any
		if !json.Valid(metadata.ExecutionCheckpoint) || json.Unmarshal(metadata.ExecutionCheckpoint, &checkpoint) != nil || checkpoint == nil {
			return ErrInvalidLifecycleMetadata
		}
	}
	return nil
}
