package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/lyming99/specrelay/backend/internal/domain"
	"github.com/lyming99/specrelay/backend/internal/security"
)

const (
	DriftSeverityClean             DriftSeverity = "clean"
	DriftSeveritySafeIgnore        DriftSeverity = "safe_ignore"
	DriftSeverityNeedsConfirmation DriftSeverity = "needs_confirmation"
	DriftSeverityMustBlock         DriftSeverity = "must_block"

	DriftActionContinue         = "continue"
	DriftActionReviewAndAccept  = "review_and_accept_or_regenerate"
	DriftActionRegenerate       = "regenerate_plan"
	DriftActionResolveConflicts = "resolve_conflicts"
	DriftActionRepairWorkspace  = "repair_workspace_and_recapture"
)

// Compatibility aliases keep call sites concise while preserving explicit JSON
// values for REST and MCP contracts added by later tasks.
const (
	DriftSeverityConfirm = DriftSeverityNeedsConfirmation
	DriftSeverityBlock   = DriftSeverityMustBlock
)

type DriftSeverity string

type WorkspaceFileState struct {
	Path           string   `json:"path"`
	OriginalPath   string   `json:"originalPath,omitempty"`
	IndexStatus    string   `json:"indexStatus"`
	WorktreeStatus string   `json:"worktreeStatus"`
	ContentKind    string   `json:"contentKind"`
	Mode           string   `json:"mode"`
	ContentDigest  string   `json:"contentDigest"`
	IndexDigest    string   `json:"indexDigest"`
	Size           int64    `json:"size"`
	IndexEntries   []string `json:"indexEntries,omitempty"`
}

type WorkspaceSnapshot struct {
	CapturedAt             time.Time            `json:"capturedAt"`
	ConfiguredPath         string               `json:"configuredPath"`
	AbsoluteConfiguredPath string               `json:"absoluteConfiguredPath"`
	NormalizedPath         string               `json:"normalizedPath"`
	Readable               bool                 `json:"readable"`
	CaptureError           string               `json:"captureError,omitempty"`
	IsGitRepository        bool                 `json:"isGitRepository"`
	GitWorkTree            string               `json:"gitWorkTree"`
	GitDirectory           string               `json:"gitDirectory"`
	GitRepositoryIdentity  string               `json:"gitRepositoryIdentity"`
	GitBranch              string               `json:"gitBranch"`
	GitDetached            bool                 `json:"gitDetached"`
	GitHead                string               `json:"gitHead"`
	TrackedChanges         []WorkspaceFileState `json:"trackedChanges"`
	UntrackedFiles         []WorkspaceFileState `json:"untrackedFiles"`
	ConflictedFiles        []WorkspaceFileState `json:"conflictedFiles"`
	StatusDigest           string               `json:"statusDigest"`
	ContentDigest          string               `json:"contentDigest"`
}

// ExecutionContextSnapshot is the application-level, comparison-ready form of
// the durable plan snapshot. It mirrors the execution-relevant database fields
// and augments them with a content-aware workspace snapshot.
type ExecutionContextSnapshot struct {
	SnapshotID          uuid.UUID         `json:"snapshotId,omitempty"`
	SnapshotKind        string            `json:"snapshotKind,omitempty"`
	SnapshotSequence    int64             `json:"snapshotSequence,omitempty"`
	CapturedAt          time.Time         `json:"capturedAt"`
	RequirementID       uuid.UUID         `json:"requirementId"`
	RequirementVersion  int64             `json:"requirementVersion"`
	RequirementDigest   string            `json:"requirementDigest"`
	PlanID              uuid.UUID         `json:"planId"`
	PlanResourceVersion int64             `json:"planResourceVersion"`
	PlanContentVersion  int64             `json:"planContentVersion"`
	PlanSpecDigest      string            `json:"planSpecDigest"`
	ProjectVersion      int64             `json:"projectVersion"`
	ConfigVersion       int64             `json:"configVersion"`
	KeyExecutionFields  json.RawMessage   `json:"keyExecutionFields"`
	GenerationProvider  string            `json:"generationProvider"`
	ExecutionProvider   string            `json:"executionProvider"`
	Workspace           WorkspaceSnapshot `json:"workspace"`
	IntegrityError      string            `json:"integrityError,omitempty"`
}

type ExecutionContextInput struct {
	SnapshotID          uuid.UUID
	SnapshotKind        string
	SnapshotSequence    int64
	RequirementID       uuid.UUID
	RequirementVersion  int64
	RequirementDigest   string
	PlanID              uuid.UUID
	PlanResourceVersion int64
	PlanContentVersion  int64
	PlanSpecDigest      string
	ProjectVersion      int64
	ConfigVersion       int64
	KeyExecutionFields  json.RawMessage
	GenerationProvider  string
	ExecutionProvider   string
	WorkspacePath       string
}

type ContextDifference struct {
	Field             string        `json:"field"`
	BaselineValue     string        `json:"baselineValue"`
	CurrentValue      string        `json:"currentValue"`
	Severity          DriftSeverity `json:"severity"`
	Reason            string        `json:"reason"`
	RecommendedAction string        `json:"recommendedAction"`
}

type DriftReport struct {
	Severity    DriftSeverity       `json:"severity"`
	Differences []ContextDifference `json:"differences"`
	Fingerprint string              `json:"fingerprint"`
	CLIAllowed  bool                `json:"cliAllowed"`
}

type WorkspaceCollector struct {
	GitBinary string
	Timeout   time.Duration
}

func NewWorkspaceCollector() WorkspaceCollector {
	return WorkspaceCollector{GitBinary: "git", Timeout: 3 * time.Second}
}

func CollectWorkspaceSnapshot(ctx context.Context, configuredPath string) (WorkspaceSnapshot, error) {
	return NewWorkspaceCollector().Collect(ctx, configuredPath)
}

// CaptureWorkspaceSnapshot is an alias used by lifecycle code that describes
// snapshots as captures rather than collections.
func CaptureWorkspaceSnapshot(ctx context.Context, configuredPath string) (WorkspaceSnapshot, error) {
	return CollectWorkspaceSnapshot(ctx, configuredPath)
}

func (collector WorkspaceCollector) Collect(ctx context.Context, configuredPath string) (WorkspaceSnapshot, error) {
	snapshot := WorkspaceSnapshot{CapturedAt: time.Now().UTC(), ConfiguredPath: configuredPath}
	identity, err := security.InspectExistingPath(configuredPath)
	if err != nil {
		return workspaceCaptureFailure(snapshot, fmt.Errorf("inspect workspace path: %w", err))
	}
	snapshot.AbsoluteConfiguredPath = identity.Absolute
	snapshot.NormalizedPath = identity.Real
	info, err := os.Stat(identity.Real)
	if err != nil {
		return workspaceCaptureFailure(snapshot, fmt.Errorf("stat workspace: %w", err))
	}
	if !info.IsDir() {
		return workspaceCaptureFailure(snapshot, errors.New("workspace is not a directory"))
	}
	directory, err := os.Open(identity.Real)
	if err != nil {
		return workspaceCaptureFailure(snapshot, fmt.Errorf("open workspace: %w", err))
	}
	_, readErr := directory.Readdirnames(1)
	closeErr := directory.Close()
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return workspaceCaptureFailure(snapshot, fmt.Errorf("read workspace: %w", readErr))
	}
	if closeErr != nil {
		return workspaceCaptureFailure(snapshot, fmt.Errorf("close workspace: %w", closeErr))
	}
	snapshot.Readable = true

	rootOutput, err := collector.gitQuery(ctx, identity.Real, "rev-parse", "--show-toplevel")
	if err != nil {
		if findGitMarker(identity.Real) != "" {
			return workspaceCaptureFailure(snapshot, fmt.Errorf("read Git work tree: %w", err))
		}
		snapshot.StatusDigest = digestBytes(nil)
		snapshot.ContentDigest = digestJSON(workspaceDigestInput{})
		return snapshot, nil
	}
	root := strings.TrimSpace(string(rootOutput))
	rootIdentity, err := security.InspectExistingPath(root)
	if err != nil {
		return workspaceCaptureFailure(snapshot, fmt.Errorf("normalize Git work tree: %w", err))
	}
	snapshot.IsGitRepository = true
	snapshot.GitWorkTree = rootIdentity.Real

	gitDirOutput, err := collector.gitQuery(ctx, identity.Real, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return workspaceCaptureFailure(snapshot, fmt.Errorf("read Git directory: %w", err))
	}
	gitDirectory, err := security.NormalizeExistingPath(strings.TrimSpace(string(gitDirOutput)))
	if err != nil {
		return workspaceCaptureFailure(snapshot, fmt.Errorf("normalize Git directory: %w", err))
	}
	snapshot.GitDirectory = gitDirectory

	origin, originErr := collector.gitQuery(ctx, identity.Real, "config", "--get", "remote.origin.url")
	if originErr == nil && strings.TrimSpace(string(origin)) != "" {
		snapshot.GitRepositoryIdentity = strings.TrimSpace(string(origin))
	} else {
		snapshot.GitRepositoryIdentity = gitDirectory
	}

	head, headErr := collector.gitQuery(ctx, identity.Real, "rev-parse", "--verify", "HEAD")
	if headErr == nil {
		snapshot.GitHead = strings.TrimSpace(string(head))
	}
	branch, branchErr := collector.gitQuery(ctx, identity.Real, "symbolic-ref", "--quiet", "--short", "HEAD")
	if branchErr == nil {
		snapshot.GitBranch = strings.TrimSpace(string(branch))
	} else if snapshot.GitHead != "" {
		snapshot.GitDetached = true
	}

	statusText, err := collector.gitQuery(ctx, identity.Real, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return workspaceCaptureFailure(snapshot, fmt.Errorf("read Git status: %w", err))
	}
	// This digest is intentionally compatible with the P001 snapshot format.
	// ContentDigest below is stronger and detects bytes changing under the same
	// porcelain status.
	snapshot.StatusDigest = digestBytes(bytes.TrimSpace(statusText))

	statusRaw, err := collector.gitQuery(ctx, identity.Real, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return workspaceCaptureFailure(snapshot, fmt.Errorf("read machine Git status: %w", err))
	}
	indexRaw, err := collector.gitQuery(ctx, identity.Real, "ls-files", "--stage", "-z")
	if err != nil {
		return workspaceCaptureFailure(snapshot, fmt.Errorf("read Git index: %w", err))
	}
	indexEntries, err := parseIndexEntries(indexRaw)
	if err != nil {
		return workspaceCaptureFailure(snapshot, err)
	}
	records, err := parsePorcelainV1Z(statusRaw)
	if err != nil {
		return workspaceCaptureFailure(snapshot, err)
	}
	for _, record := range records {
		state, stateErr := collectFileState(snapshot.GitWorkTree, record, indexEntries[record.Path])
		if stateErr != nil {
			return workspaceCaptureFailure(snapshot, stateErr)
		}
		switch {
		case record.Status == "??":
			snapshot.UntrackedFiles = append(snapshot.UntrackedFiles, state)
		case isConflictStatus(record.Status):
			snapshot.ConflictedFiles = append(snapshot.ConflictedFiles, state)
		default:
			snapshot.TrackedChanges = append(snapshot.TrackedChanges, state)
		}
	}
	sortWorkspaceFiles(snapshot.TrackedChanges)
	sortWorkspaceFiles(snapshot.UntrackedFiles)
	sortWorkspaceFiles(snapshot.ConflictedFiles)
	snapshot.ContentDigest = digestJSON(workspaceDigestInput{
		Tracked:   snapshot.TrackedChanges,
		Untracked: snapshot.UntrackedFiles,
		Conflicts: snapshot.ConflictedFiles,
	})
	return snapshot, nil
}

func CollectExecutionContext(ctx context.Context, input ExecutionContextInput) (ExecutionContextSnapshot, error) {
	workspace, err := CollectWorkspaceSnapshot(ctx, input.WorkspacePath)
	snapshot := ExecutionContextSnapshot{
		SnapshotID:          input.SnapshotID,
		SnapshotKind:        input.SnapshotKind,
		SnapshotSequence:    input.SnapshotSequence,
		CapturedAt:          workspace.CapturedAt,
		RequirementID:       input.RequirementID,
		RequirementVersion:  input.RequirementVersion,
		RequirementDigest:   strings.TrimSpace(input.RequirementDigest),
		PlanID:              input.PlanID,
		PlanResourceVersion: input.PlanResourceVersion,
		PlanContentVersion:  input.PlanContentVersion,
		PlanSpecDigest:      strings.TrimSpace(input.PlanSpecDigest),
		ProjectVersion:      input.ProjectVersion,
		ConfigVersion:       input.ConfigVersion,
		KeyExecutionFields:  canonicalJSON(input.KeyExecutionFields),
		GenerationProvider:  strings.TrimSpace(input.GenerationProvider),
		ExecutionProvider:   strings.TrimSpace(input.ExecutionProvider),
		Workspace:           workspace,
	}
	if err != nil {
		snapshot.IntegrityError = err.Error()
		return snapshot, err
	}
	if integrityErr := validateExecutionContextSnapshot(snapshot); integrityErr != nil {
		snapshot.IntegrityError = integrityErr.Error()
		return snapshot, integrityErr
	}
	return snapshot, nil
}

func CaptureExecutionContext(ctx context.Context, input ExecutionContextInput) (ExecutionContextSnapshot, error) {
	return CollectExecutionContext(ctx, input)
}

// ExecutionContextFromPlanSnapshot converts the P001 durable snapshot into the
// comparison model. Rich per-file data is unavailable in legacy rows, but the
// stable Git workspace digest remains usable until an accepted/checkpoint
// snapshot is captured by the application layer.
func ExecutionContextFromPlanSnapshot(snapshot domain.PlanExecutionSnapshot) ExecutionContextSnapshot {
	workspace := WorkspaceSnapshot{
		CapturedAt:             snapshot.CreatedAt,
		ConfiguredPath:         snapshot.WorkspacePathNormalized,
		AbsoluteConfiguredPath: snapshot.WorkspacePathNormalized,
		NormalizedPath:         snapshot.WorkspacePathNormalized,
		Readable:               true,
		IsGitRepository:        snapshot.GitRoot != "" || snapshot.GitRepositoryIdentity != "",
		GitWorkTree:            snapshot.GitRoot,
		GitRepositoryIdentity:  snapshot.GitRepositoryIdentity,
		GitBranch:              snapshot.GitBranch,
		GitDetached:            snapshot.GitBranch == "" && snapshot.GitHead != "",
		GitHead:                snapshot.GitHead,
		StatusDigest:           snapshot.GitWorkspaceDigest,
		ContentDigest:          snapshot.GitWorkspaceDigest,
	}
	return ExecutionContextSnapshot{
		SnapshotID:          snapshot.ID,
		SnapshotKind:        snapshot.Kind,
		SnapshotSequence:    snapshot.Sequence,
		CapturedAt:          snapshot.CreatedAt,
		RequirementID:       snapshot.RequirementID,
		RequirementVersion:  snapshot.RequirementVersion,
		RequirementDigest:   snapshot.RequirementDigest,
		PlanID:              snapshot.PlanID,
		PlanResourceVersion: snapshot.PlanResourceVersion,
		PlanContentVersion:  snapshot.PlanContentVersion,
		PlanSpecDigest:      snapshot.PlanSpecDigest,
		ProjectVersion:      snapshot.ProjectVersion,
		ConfigVersion:       snapshot.ConfigVersion,
		KeyExecutionFields:  canonicalJSON(snapshot.KeyExecutionFields),
		GenerationProvider:  snapshot.GenerationProvider,
		ExecutionProvider:   snapshot.ExecutionProvider,
		Workspace:           workspace,
	}
}

func CompareExecutionContexts(baseline *ExecutionContextSnapshot, current ExecutionContextSnapshot, checkpoint *ExecutionContextSnapshot) DriftReport {
	differences := make([]ContextDifference, 0)
	if baseline == nil {
		differences = append(differences, difference("snapshot.baseline", "missing", "present", DriftSeverityMustBlock,
			"the plan has no execution baseline", DriftActionRepairWorkspace))
		return finishDriftReport(differences, current)
	}
	if err := validateExecutionContextSnapshot(*baseline); err != nil {
		differences = append(differences, difference("snapshot.baseline_integrity", "valid", err.Error(), DriftSeverityMustBlock,
			"the baseline snapshot is incomplete or damaged", DriftActionRepairWorkspace))
		return finishDriftReport(differences, current)
	}
	if err := validateExecutionContextSnapshot(current); err != nil {
		differences = append(differences, difference("snapshot.current_integrity", "valid", err.Error(), DriftSeverityMustBlock,
			"the current execution context could not be read safely", DriftActionRepairWorkspace))
		return finishDriftReport(differences, current)
	}

	if !baseline.CapturedAt.Equal(current.CapturedAt) {
		differences = append(differences, difference("snapshot.captured_at", formatTime(baseline.CapturedAt), formatTime(current.CapturedAt), DriftSeveritySafeIgnore,
			"capture timestamps do not change execution semantics", DriftActionContinue))
	}
	compareIdentityFields(&differences, *baseline, current)
	compareVersionAndContentFields(&differences, *baseline, current)
	compareProviderAndSettingsFields(&differences, *baseline, current)

	var workspaceCheckpoint *WorkspaceSnapshot
	if checkpoint != nil && validateWorkspaceSnapshot(checkpoint.Workspace) == nil {
		workspaceCheckpoint = &checkpoint.Workspace
	}
	differences = append(differences, compareWorkspaceSnapshots(&baseline.Workspace, &current.Workspace, workspaceCheckpoint)...)
	return finishDriftReport(differences, current)
}

func CalculateExecutionContextDiff(baseline *ExecutionContextSnapshot, current ExecutionContextSnapshot, checkpoint *ExecutionContextSnapshot) DriftReport {
	return CompareExecutionContexts(baseline, current, checkpoint)
}

func CompareWorkspaceSnapshots(baseline, current, checkpoint *WorkspaceSnapshot) DriftReport {
	differences := compareWorkspaceSnapshots(baseline, current, checkpoint)
	context := ExecutionContextSnapshot{}
	if current != nil {
		context.CapturedAt = current.CapturedAt
		context.Workspace = *current
	}
	return finishDriftReport(differences, context)
}

func (report DriftReport) AllowsCLI() bool { return report.CLIAllowed }

func compareIdentityFields(differences *[]ContextDifference, baseline, current ExecutionContextSnapshot) {
	compareString(differences, "workspace.configured_path", baseline.Workspace.ConfiguredPath, current.Workspace.ConfiguredPath,
		DriftSeverityMustBlock, "the configured project path changed", DriftActionRegenerate)
	compareString(differences, "workspace.normalized_path", baseline.Workspace.NormalizedPath, current.Workspace.NormalizedPath,
		DriftSeverityMustBlock, "the real project path changed", DriftActionRegenerate)
	if baseline.RequirementID != current.RequirementID {
		*differences = append(*differences, difference("requirement.id", baseline.RequirementID.String(), current.RequirementID.String(), DriftSeverityMustBlock,
			"the plan now points to a different requirement", DriftActionRegenerate))
	}
	if baseline.PlanID != current.PlanID {
		*differences = append(*differences, difference("plan.id", baseline.PlanID.String(), current.PlanID.String(), DriftSeverityMustBlock,
			"the execution context points to a different plan", DriftActionRegenerate))
	}
}

func compareVersionAndContentFields(differences *[]ContextDifference, baseline, current ExecutionContextSnapshot) {
	if baseline.RequirementVersion != current.RequirementVersion {
		*differences = append(*differences, difference("requirement.version", formatInt(baseline.RequirementVersion), formatInt(current.RequirementVersion), DriftSeverityMustBlock,
			"the requirement version changed after planning", DriftActionRegenerate))
	}
	compareString(differences, "requirement.digest", baseline.RequirementDigest, current.RequirementDigest,
		DriftSeverityMustBlock, "the requirement content changed after planning", DriftActionRegenerate)
	if baseline.PlanContentVersion != current.PlanContentVersion {
		*differences = append(*differences, difference("plan.content_version", formatInt(baseline.PlanContentVersion), formatInt(current.PlanContentVersion), DriftSeverityMustBlock,
			"the executable plan content version changed", DriftActionRegenerate))
	}
	compareString(differences, "plan.spec_digest", baseline.PlanSpecDigest, current.PlanSpecDigest,
		DriftSeverityMustBlock, "the executable plan specification changed", DriftActionRegenerate)
	compareServerVersion(differences, "plan.resource_version", baseline.PlanResourceVersion, current.PlanResourceVersion)
	compareServerVersion(differences, "project.version", baseline.ProjectVersion, current.ProjectVersion)
	compareServerVersion(differences, "config.version", baseline.ConfigVersion, current.ConfigVersion)
}

func compareProviderAndSettingsFields(differences *[]ContextDifference, baseline, current ExecutionContextSnapshot) {
	compareString(differences, "provider.generation", baseline.GenerationProvider, current.GenerationProvider,
		DriftSeverityNeedsConfirmation, "the plan generation provider changed", DriftActionReviewAndAccept)
	compareString(differences, "provider.execution", baseline.ExecutionProvider, current.ExecutionProvider,
		DriftSeverityNeedsConfirmation, "the task execution provider changed", DriftActionReviewAndAccept)
	baselineSettings := string(canonicalJSON(baseline.KeyExecutionFields))
	currentSettings := string(canonicalJSON(current.KeyExecutionFields))
	compareString(differences, "settings.key_execution_fields", baselineSettings, currentSettings,
		DriftSeverityNeedsConfirmation, "execution-critical project settings changed", DriftActionReviewAndAccept)
}

func compareWorkspaceSnapshots(baseline, current, checkpoint *WorkspaceSnapshot) []ContextDifference {
	differences := make([]ContextDifference, 0)
	if baseline == nil {
		return append(differences, difference("workspace.baseline", "missing", "present", DriftSeverityMustBlock,
			"the workspace baseline is missing", DriftActionRepairWorkspace))
	}
	if current == nil {
		return append(differences, difference("workspace.current", "present", "missing", DriftSeverityMustBlock,
			"the current workspace snapshot is missing", DriftActionRepairWorkspace))
	}
	if err := validateWorkspaceSnapshot(*baseline); err != nil {
		return append(differences, difference("workspace.baseline_integrity", "valid", err.Error(), DriftSeverityMustBlock,
			"the saved workspace snapshot is incomplete or damaged", DriftActionRepairWorkspace))
	}
	if err := validateWorkspaceSnapshot(*current); err != nil {
		return append(differences, difference("workspace.current_integrity", "valid", err.Error(), DriftSeverityMustBlock,
			"the workspace cannot be read safely", DriftActionRepairWorkspace))
	}

	matchesCheckpoint := checkpoint != nil && workspaceStateEqual(*current, *checkpoint)
	checkpointSeverity := DriftSeverityNeedsConfirmation
	checkpointReasonSuffix := ""
	checkpointAction := DriftActionReviewAndAccept
	if matchesCheckpoint {
		checkpointSeverity = DriftSeveritySafeIgnore
		checkpointReasonSuffix = "; the current state exactly matches the latest successful task checkpoint"
		checkpointAction = DriftActionContinue
	}

	compareString(&differences, "git.work_tree", baseline.GitWorkTree, current.GitWorkTree,
		DriftSeverityMustBlock, "the Git work tree changed", DriftActionRegenerate)
	compareString(&differences, "git.repository_identity", baseline.GitRepositoryIdentity, current.GitRepositoryIdentity,
		DriftSeverityMustBlock, "the Git repository identity changed", DriftActionRegenerate)
	if baseline.IsGitRepository != current.IsGitRepository {
		differences = append(differences, difference("git.is_repository", formatBool(baseline.IsGitRepository), formatBool(current.IsGitRepository), DriftSeverityMustBlock,
			"the workspace Git repository status changed", DriftActionRegenerate))
	}
	if baseline.GitBranch != current.GitBranch {
		differences = append(differences, difference("git.branch", baseline.GitBranch, current.GitBranch, checkpointSeverity,
			"the current branch changed"+checkpointReasonSuffix, checkpointAction))
	}
	if baseline.GitDetached != current.GitDetached {
		differences = append(differences, difference("git.detached_head", formatBool(baseline.GitDetached), formatBool(current.GitDetached), checkpointSeverity,
			"the workspace changed between a branch and detached HEAD"+checkpointReasonSuffix, checkpointAction))
	}
	if baseline.GitHead != current.GitHead {
		differences = append(differences, difference("git.head", baseline.GitHead, current.GitHead, checkpointSeverity,
			"the HEAD commit changed"+checkpointReasonSuffix, checkpointAction))
	}

	if len(current.ConflictedFiles) > 0 {
		differences = append(differences, difference("git.conflicted_files", stableJSON(baseline.ConflictedFiles), stableJSON(current.ConflictedFiles), DriftSeverityMustBlock,
			"the workspace contains unresolved merge conflicts", DriftActionResolveConflicts))
	} else if !workspaceFilesEqual(baseline.ConflictedFiles, current.ConflictedFiles) {
		differences = append(differences, difference("git.conflicted_files", stableJSON(baseline.ConflictedFiles), stableJSON(current.ConflictedFiles), checkpointSeverity,
			"the conflict set changed"+checkpointReasonSuffix, checkpointAction))
	}
	if !workspaceFilesEqual(baseline.TrackedChanges, current.TrackedChanges) {
		reason := dirtyChangeReason("tracked", baseline.TrackedChanges, current.TrackedChanges) + checkpointReasonSuffix
		differences = append(differences, difference("git.tracked_changes", stableJSON(baseline.TrackedChanges), stableJSON(current.TrackedChanges), checkpointSeverity, reason, checkpointAction))
	}
	if !workspaceFilesEqual(baseline.UntrackedFiles, current.UntrackedFiles) {
		reason := dirtyChangeReason("untracked", baseline.UntrackedFiles, current.UntrackedFiles) + checkpointReasonSuffix
		differences = append(differences, difference("git.untracked_files", stableJSON(baseline.UntrackedFiles), stableJSON(current.UntrackedFiles), checkpointSeverity, reason, checkpointAction))
	}
	if baseline.ContentDigest != current.ContentDigest {
		differences = append(differences, difference("git.workspace_content_digest", baseline.ContentDigest, current.ContentDigest, checkpointSeverity,
			"dirty file status or content changed"+checkpointReasonSuffix, checkpointAction))
	}
	return differences
}

func compareServerVersion(differences *[]ContextDifference, field string, baseline, current int64) {
	if baseline == current {
		return
	}
	if current > baseline {
		*differences = append(*differences, difference(field, formatInt(baseline), formatInt(current), DriftSeveritySafeIgnore,
			"the server resource version increased without changing compared execution content", DriftActionContinue))
		return
	}
	*differences = append(*differences, difference(field, formatInt(baseline), formatInt(current), DriftSeverityMustBlock,
		"the server resource version moved backwards", DriftActionRepairWorkspace))
}

func compareString(differences *[]ContextDifference, field, baseline, current string, severity DriftSeverity, reason, action string) {
	if baseline == current {
		return
	}
	*differences = append(*differences, difference(field, baseline, current, severity, reason, action))
}

func difference(field, baseline, current string, severity DriftSeverity, reason, action string) ContextDifference {
	return ContextDifference{
		Field: field, BaselineValue: baseline, CurrentValue: current, Severity: severity,
		Reason: reason, RecommendedAction: action,
	}
}

func finishDriftReport(differences []ContextDifference, current ExecutionContextSnapshot) DriftReport {
	sort.Slice(differences, func(i, j int) bool {
		if differences[i].Field != differences[j].Field {
			return differences[i].Field < differences[j].Field
		}
		if differences[i].Severity != differences[j].Severity {
			return severityRank(differences[i].Severity) > severityRank(differences[j].Severity)
		}
		return differences[i].Reason < differences[j].Reason
	})
	severity := DriftSeverityClean
	for _, item := range differences {
		if severityRank(item.Severity) > severityRank(severity) {
			severity = item.Severity
		}
	}
	fingerprintItems := make([]ContextDifference, 0, len(differences))
	for _, item := range differences {
		if item.Severity == DriftSeverityNeedsConfirmation || item.Severity == DriftSeverityMustBlock {
			fingerprintItems = append(fingerprintItems, item)
		}
	}
	fingerprint := digestJSON(struct {
		Differences []ContextDifference `json:"differences"`
		Current     stableContext       `json:"current"`
	}{Differences: fingerprintItems, Current: stableExecutionContext(current)})
	return DriftReport{
		Severity: severity, Differences: differences, Fingerprint: fingerprint,
		CLIAllowed: severity != DriftSeverityNeedsConfirmation && severity != DriftSeverityMustBlock,
	}
}

type stableContext struct {
	RequirementID      uuid.UUID            `json:"requirementId"`
	RequirementVersion int64                `json:"requirementVersion"`
	RequirementDigest  string               `json:"requirementDigest"`
	PlanID             uuid.UUID            `json:"planId"`
	PlanContentVersion int64                `json:"planContentVersion"`
	PlanSpecDigest     string               `json:"planSpecDigest"`
	KeyExecutionFields json.RawMessage      `json:"keyExecutionFields"`
	GenerationProvider string               `json:"generationProvider"`
	ExecutionProvider  string               `json:"executionProvider"`
	ConfiguredPath     string               `json:"configuredPath"`
	NormalizedPath     string               `json:"normalizedPath"`
	GitWorkTree        string               `json:"gitWorkTree"`
	RepositoryIdentity string               `json:"repositoryIdentity"`
	GitBranch          string               `json:"gitBranch"`
	GitDetached        bool                 `json:"gitDetached"`
	GitHead            string               `json:"gitHead"`
	WorkspaceDigest    string               `json:"workspaceDigest"`
	ConflictedFiles    []WorkspaceFileState `json:"conflictedFiles"`
}

func stableExecutionContext(snapshot ExecutionContextSnapshot) stableContext {
	return stableContext{
		RequirementID: snapshot.RequirementID, RequirementVersion: snapshot.RequirementVersion,
		RequirementDigest: snapshot.RequirementDigest, PlanID: snapshot.PlanID,
		PlanContentVersion: snapshot.PlanContentVersion, PlanSpecDigest: snapshot.PlanSpecDigest,
		KeyExecutionFields: canonicalJSON(snapshot.KeyExecutionFields),
		GenerationProvider: snapshot.GenerationProvider, ExecutionProvider: snapshot.ExecutionProvider,
		ConfiguredPath: snapshot.Workspace.ConfiguredPath, NormalizedPath: snapshot.Workspace.NormalizedPath,
		GitWorkTree: snapshot.Workspace.GitWorkTree, RepositoryIdentity: snapshot.Workspace.GitRepositoryIdentity,
		GitBranch: snapshot.Workspace.GitBranch, GitDetached: snapshot.Workspace.GitDetached,
		GitHead: snapshot.Workspace.GitHead, WorkspaceDigest: snapshot.Workspace.ContentDigest,
		ConflictedFiles: snapshot.Workspace.ConflictedFiles,
	}
}

func validateExecutionContextSnapshot(snapshot ExecutionContextSnapshot) error {
	if strings.TrimSpace(snapshot.IntegrityError) != "" {
		return errors.New(snapshot.IntegrityError)
	}
	if snapshot.RequirementID == uuid.Nil || snapshot.PlanID == uuid.Nil {
		return errors.New("requirement and plan identities are required")
	}
	if snapshot.RequirementVersion < 1 || snapshot.PlanResourceVersion < 1 || snapshot.PlanContentVersion < 1 || snapshot.ProjectVersion < 1 || snapshot.ConfigVersion < 1 {
		return errors.New("snapshot versions must be positive")
	}
	if !isSHA256(snapshot.RequirementDigest) || !isSHA256(snapshot.PlanSpecDigest) {
		return errors.New("requirement and plan digests must be SHA-256 values")
	}
	if strings.TrimSpace(snapshot.GenerationProvider) == "" || strings.TrimSpace(snapshot.ExecutionProvider) == "" {
		return errors.New("generation and execution providers are required")
	}
	var settings map[string]any
	if len(snapshot.KeyExecutionFields) == 0 || json.Unmarshal(snapshot.KeyExecutionFields, &settings) != nil || settings == nil {
		return errors.New("key execution fields must be a JSON object")
	}
	return validateWorkspaceSnapshot(snapshot.Workspace)
}

func validateWorkspaceSnapshot(snapshot WorkspaceSnapshot) error {
	if strings.TrimSpace(snapshot.CaptureError) != "" {
		return errors.New(snapshot.CaptureError)
	}
	if !snapshot.Readable {
		return errors.New("workspace is not readable")
	}
	if strings.TrimSpace(snapshot.ConfiguredPath) == "" || strings.TrimSpace(snapshot.NormalizedPath) == "" {
		return errors.New("workspace configured and normalized paths are required")
	}
	if !isSHA256(snapshot.ContentDigest) {
		return errors.New("workspace content digest is missing or invalid")
	}
	if snapshot.IsGitRepository {
		if strings.TrimSpace(snapshot.GitWorkTree) == "" || strings.TrimSpace(snapshot.GitRepositoryIdentity) == "" {
			return errors.New("Git work tree and repository identity are required")
		}
		if snapshot.GitBranch == "" && !snapshot.GitDetached && snapshot.GitHead != "" {
			return errors.New("Git branch or detached HEAD state is required")
		}
	}
	return nil
}

func workspaceCaptureFailure(snapshot WorkspaceSnapshot, err error) (WorkspaceSnapshot, error) {
	snapshot.Readable = false
	snapshot.CaptureError = err.Error()
	if snapshot.ContentDigest == "" {
		snapshot.ContentDigest = digestJSON(workspaceDigestInput{Error: snapshot.CaptureError})
	}
	return snapshot, err
}

type porcelainRecord struct {
	Status       string
	Path         string
	OriginalPath string
}

func parsePorcelainV1Z(raw []byte) ([]porcelainRecord, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	parts := bytes.Split(raw, []byte{0})
	records := make([]porcelainRecord, 0, len(parts))
	for index := 0; index < len(parts); index++ {
		part := parts[index]
		if len(part) == 0 {
			continue
		}
		if len(part) < 4 || part[2] != ' ' {
			return nil, fmt.Errorf("invalid Git status record %q", part)
		}
		record := porcelainRecord{Status: string(part[:2]), Path: string(part[3:])}
		if statusHasOriginalPath(record.Status) {
			index++
			if index >= len(parts) || len(parts[index]) == 0 {
				return nil, fmt.Errorf("missing original path for Git status record %q", part)
			}
			record.OriginalPath = string(parts[index])
		}
		if !safeGitRelativePath(record.Path) || (record.OriginalPath != "" && !safeGitRelativePath(record.OriginalPath)) {
			return nil, fmt.Errorf("unsafe path in Git status: %q", record.Path)
		}
		records = append(records, record)
	}
	return records, nil
}

func parseIndexEntries(raw []byte) (map[string][]string, error) {
	entries := map[string][]string{}
	for _, part := range bytes.Split(raw, []byte{0}) {
		if len(part) == 0 {
			continue
		}
		tab := bytes.IndexByte(part, '\t')
		if tab < 0 || tab == len(part)-1 {
			return nil, fmt.Errorf("invalid Git index record %q", part)
		}
		metadata := string(part[:tab])
		path := string(part[tab+1:])
		if !safeGitRelativePath(path) {
			return nil, fmt.Errorf("unsafe path in Git index: %q", path)
		}
		entries[path] = append(entries[path], metadata)
	}
	for path := range entries {
		sort.Strings(entries[path])
	}
	return entries, nil
}

func collectFileState(root string, record porcelainRecord, indexEntries []string) (WorkspaceFileState, error) {
	state := WorkspaceFileState{
		Path: record.Path, OriginalPath: record.OriginalPath,
		IndexStatus: string(record.Status[0]), WorktreeStatus: string(record.Status[1]),
		IndexEntries: append([]string(nil), indexEntries...),
		IndexDigest:  digestBytes([]byte(strings.Join(indexEntries, "\n"))),
	}
	if record.Status[1] == 'D' || (record.Status[0] == 'D' && record.Status[1] == ' ') {
		state.ContentKind = "missing"
		state.ContentDigest = digestBytes([]byte("missing"))
		return state, nil
	}
	path, err := security.ResolveRelativePath(root, record.Path)
	if err != nil {
		return state, fmt.Errorf("resolve dirty file %q: %w", record.Path, err)
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		state.ContentKind = "missing"
		state.ContentDigest = digestBytes([]byte("missing"))
		return state, nil
	}
	if err != nil {
		return state, fmt.Errorf("inspect dirty file %q: %w", record.Path, err)
	}
	state.Size = info.Size()
	state.Mode = info.Mode().String()
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		target, readErr := os.Readlink(path)
		if readErr != nil {
			return state, fmt.Errorf("read dirty symlink %q: %w", record.Path, readErr)
		}
		state.ContentKind = "symlink"
		state.ContentDigest = digestBytes([]byte(target))
	case info.Mode().IsRegular():
		file, openErr := os.Open(path)
		if openErr != nil {
			return state, fmt.Errorf("open dirty file %q: %w", record.Path, openErr)
		}
		hash := sha256.New()
		_, copyErr := io.Copy(hash, file)
		closeErr := file.Close()
		if copyErr != nil {
			return state, fmt.Errorf("hash dirty file %q: %w", record.Path, copyErr)
		}
		if closeErr != nil {
			return state, fmt.Errorf("close dirty file %q: %w", record.Path, closeErr)
		}
		state.ContentKind = "regular"
		state.ContentDigest = hex.EncodeToString(hash.Sum(nil))
	case info.IsDir():
		state.ContentKind = "directory"
		state.ContentDigest = digestBytes([]byte("directory"))
	default:
		state.ContentKind = "special"
		state.ContentDigest = digestBytes([]byte(info.Mode().String()))
	}
	return state, nil
}

type workspaceDigestInput struct {
	Tracked   []WorkspaceFileState `json:"tracked,omitempty"`
	Untracked []WorkspaceFileState `json:"untracked,omitempty"`
	Conflicts []WorkspaceFileState `json:"conflicts,omitempty"`
	Error     string               `json:"error,omitempty"`
}

func (collector WorkspaceCollector) gitQuery(ctx context.Context, workspace string, args ...string) ([]byte, error) {
	if err := validateReadOnlyGitQuery(args); err != nil {
		return nil, err
	}
	binary := strings.TrimSpace(collector.GitBinary)
	if binary == "" {
		binary = "git"
	}
	timeout := collector.Timeout
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	commandContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	commandArgs := append([]string{"-C", workspace}, args...)
	command := exec.CommandContext(commandContext, binary, commandArgs...)
	command.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0")
	output, err := command.Output()
	if err == nil {
		return output, nil
	}
	if commandContext.Err() != nil {
		return nil, commandContext.Err()
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		message := strings.TrimSpace(string(exitErr.Stderr))
		if message == "" {
			message = exitErr.Error()
		}
		return nil, errors.New(message)
	}
	return nil, err
}

func validateReadOnlyGitQuery(args []string) error {
	if len(args) == 0 {
		return errors.New("Git query is required")
	}
	switch args[0] {
	case "rev-parse":
		return nil
	case "symbolic-ref":
		if len(args) == 4 && args[1] == "--quiet" && args[2] == "--short" && args[3] == "HEAD" {
			return nil
		}
	case "status":
		return nil
	case "ls-files":
		return nil
	case "config":
		if len(args) >= 3 && args[1] == "--get" {
			return nil
		}
	}
	return fmt.Errorf("Git command %q is not an approved read-only query", strings.Join(args, " "))
}

func findGitMarker(start string) string {
	current := start
	for {
		candidate := filepath.Join(current, ".git")
		if _, err := os.Lstat(candidate); err == nil {
			return candidate
		}
		parent := filepath.Dir(current)
		if parent == current {
			return ""
		}
		current = parent
	}
}

func statusHasOriginalPath(status string) bool {
	return len(status) == 2 && (status[0] == 'R' || status[0] == 'C' || status[1] == 'R' || status[1] == 'C')
}

func isConflictStatus(status string) bool {
	switch status {
	case "DD", "AU", "UD", "UA", "DU", "AA", "UU":
		return true
	default:
		return false
	}
}

func safeGitRelativePath(path string) bool {
	if path == "" || filepath.IsAbs(path) || filepath.VolumeName(path) != "" {
		return false
	}
	clean := filepath.Clean(filepath.FromSlash(path))
	return clean != "." && clean != ".." && !strings.HasPrefix(clean, ".."+string(os.PathSeparator))
}

func sortWorkspaceFiles(files []WorkspaceFileState) {
	sort.Slice(files, func(i, j int) bool {
		if files[i].Path != files[j].Path {
			return files[i].Path < files[j].Path
		}
		return files[i].OriginalPath < files[j].OriginalPath
	})
}

func workspaceFilesEqual(left, right []WorkspaceFileState) bool {
	return stableJSON(left) == stableJSON(right)
}

func workspaceStateEqual(left, right WorkspaceSnapshot) bool {
	return left.NormalizedPath == right.NormalizedPath &&
		left.IsGitRepository == right.IsGitRepository &&
		left.GitWorkTree == right.GitWorkTree &&
		left.GitRepositoryIdentity == right.GitRepositoryIdentity &&
		left.GitBranch == right.GitBranch &&
		left.GitDetached == right.GitDetached &&
		left.GitHead == right.GitHead &&
		left.ContentDigest == right.ContentDigest &&
		workspaceFilesEqual(left.ConflictedFiles, right.ConflictedFiles)
}

func dirtyChangeReason(kind string, baseline, current []WorkspaceFileState) string {
	baselineByPath := make(map[string]WorkspaceFileState, len(baseline))
	for _, file := range baseline {
		baselineByPath[file.Path] = file
	}
	for _, file := range current {
		if _, ok := baselineByPath[file.Path]; !ok {
			return "new " + kind + " dirty files appeared"
		}
	}
	return kind + " dirty file status or content changed"
}

func canonicalJSON(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return append(json.RawMessage(nil), raw...)
	}
	normalized, err := json.Marshal(value)
	if err != nil {
		return append(json.RawMessage(nil), raw...)
	}
	return normalized
}

func digestJSON(value any) string {
	raw, err := json.Marshal(value)
	if err != nil {
		return digestBytes([]byte(err.Error()))
	}
	return digestBytes(raw)
}

func digestBytes(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

func stableJSON(value any) string {
	raw, err := json.Marshal(value)
	if err != nil {
		return "null"
	}
	return string(raw)
}

func isSHA256(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func severityRank(severity DriftSeverity) int {
	switch severity {
	case DriftSeverityMustBlock:
		return 3
	case DriftSeverityNeedsConfirmation:
		return 2
	case DriftSeveritySafeIgnore:
		return 1
	default:
		return 0
	}
}

func formatInt(value int64) string { return strconv.FormatInt(value, 10) }
func formatBool(value bool) string { return strconv.FormatBool(value) }
func formatTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}
