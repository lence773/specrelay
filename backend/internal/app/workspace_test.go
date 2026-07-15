package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/lyming99/specrelay/backend/internal/domain"
)

func TestRequiresExclusiveWorkspace(t *testing.T) {
	tests := []struct {
		jobType string
		want    bool
	}{
		{jobType: "plan.generate", want: false},
		{jobType: "task.execute", want: true},
		{jobType: "unknown", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.jobType, func(t *testing.T) {
			if got := RequiresExclusiveWorkspace(domain.Job{Type: tt.jobType}); got != tt.want {
				t.Fatalf("RequiresExclusiveWorkspace(%q)=%t, want %t", tt.jobType, got, tt.want)
			}
		})
	}
}

func TestWorkspaceCollectorIsReadOnlyAndCapturesGitIdentity(t *testing.T) {
	repo := newGitRepository(t)
	tracked := filepath.Join(repo, "tracked.txt")
	writeFile(t, tracked, "changed")
	writeFile(t, filepath.Join(repo, "untracked.txt"), "untracked")

	beforeHead := gitOutput(t, repo, "rev-parse", "HEAD")
	beforeBranch := gitOutput(t, repo, "branch", "--show-current")
	beforeStatus := gitOutput(t, repo, "status", "--porcelain=v1", "--untracked-files=all")
	beforeIndex := fileSHA256(t, filepath.Join(repo, ".git", "index"))
	beforeTracked := fileSHA256(t, tracked)

	snapshot, err := CollectWorkspaceSnapshot(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.Readable || !snapshot.IsGitRepository {
		t.Fatalf("unexpected snapshot: %+v", snapshot)
	}
	if snapshot.ConfiguredPath != repo || snapshot.NormalizedPath != repo || snapshot.GitWorkTree != repo {
		t.Fatalf("paths not preserved and normalized: %+v", snapshot)
	}
	if snapshot.GitRepositoryIdentity != "https://example.invalid/specrelay.git" {
		t.Fatalf("repository identity=%q", snapshot.GitRepositoryIdentity)
	}
	if snapshot.GitBranch != "main" || snapshot.GitDetached || snapshot.GitHead != beforeHead {
		t.Fatalf("branch/head not captured: %+v", snapshot)
	}
	if len(snapshot.TrackedChanges) != 1 || snapshot.TrackedChanges[0].Path != "tracked.txt" {
		t.Fatalf("tracked changes=%+v", snapshot.TrackedChanges)
	}
	if len(snapshot.UntrackedFiles) != 1 || snapshot.UntrackedFiles[0].Path != "untracked.txt" {
		t.Fatalf("untracked files=%+v", snapshot.UntrackedFiles)
	}
	if len(snapshot.ConflictedFiles) != 0 || !isSHA256(snapshot.ContentDigest) || !isSHA256(snapshot.StatusDigest) {
		t.Fatalf("invalid status summary: %+v", snapshot)
	}

	if got := fileSHA256(t, filepath.Join(repo, ".git", "index")); got != beforeIndex {
		t.Fatalf("Git index changed: %s -> %s", beforeIndex, got)
	}
	if got := gitOutput(t, repo, "rev-parse", "HEAD"); got != beforeHead {
		t.Fatalf("HEAD changed: %s -> %s", beforeHead, got)
	}
	if got := gitOutput(t, repo, "branch", "--show-current"); got != beforeBranch {
		t.Fatalf("branch changed: %s -> %s", beforeBranch, got)
	}
	if got := gitOutput(t, repo, "status", "--porcelain=v1", "--untracked-files=all"); got != beforeStatus {
		t.Fatalf("status changed:\n%s\n->\n%s", beforeStatus, got)
	}
	if got := fileSHA256(t, tracked); got != beforeTracked {
		t.Fatalf("tracked file changed: %s -> %s", beforeTracked, got)
	}
}

func TestWorkspaceCollectorDetectsContentChangesWithSameGitStatus(t *testing.T) {
	t.Run("tracked", func(t *testing.T) {
		repo := newGitRepository(t)
		writeFile(t, filepath.Join(repo, "tracked.txt"), "first dirty content")
		first := mustCollectWorkspace(t, repo)
		writeFile(t, filepath.Join(repo, "tracked.txt"), "second dirty content")
		second := mustCollectWorkspace(t, repo)
		if first.StatusDigest != second.StatusDigest {
			t.Fatalf("porcelain status unexpectedly changed: %s != %s", first.StatusDigest, second.StatusDigest)
		}
		if first.ContentDigest == second.ContentDigest || first.TrackedChanges[0].ContentDigest == second.TrackedChanges[0].ContentDigest {
			t.Fatal("content-aware digest did not change for tracked file")
		}
	})

	t.Run("untracked", func(t *testing.T) {
		repo := newGitRepository(t)
		path := filepath.Join(repo, "new.txt")
		writeFile(t, path, "one")
		first := mustCollectWorkspace(t, repo)
		writeFile(t, path, "two")
		second := mustCollectWorkspace(t, repo)
		if first.StatusDigest != second.StatusDigest {
			t.Fatalf("porcelain status unexpectedly changed: %s != %s", first.StatusDigest, second.StatusDigest)
		}
		if first.ContentDigest == second.ContentDigest || first.UntrackedFiles[0].ContentDigest == second.UntrackedFiles[0].ContentDigest {
			t.Fatal("content-aware digest did not change for untracked file")
		}
	})
}

func TestWorkspaceCollectorCapturesBranchCommitAndDetachedHead(t *testing.T) {
	repo := newGitRepository(t)
	baseline := mustCollectWorkspace(t, repo)

	gitRun(t, repo, "checkout", "-b", "feature")
	branchSnapshot := mustCollectWorkspace(t, repo)
	if branchSnapshot.GitBranch != "feature" || branchSnapshot.GitHead != baseline.GitHead {
		t.Fatalf("branch snapshot=%+v", branchSnapshot)
	}
	branchReport := CompareWorkspaceSnapshots(&baseline, &branchSnapshot, nil)
	assertDifference(t, branchReport, "git.branch", DriftSeverityNeedsConfirmation)
	if branchReport.CLIAllowed {
		t.Fatal("branch change must require explicit disposition")
	}

	writeFile(t, filepath.Join(repo, "tracked.txt"), "feature commit")
	gitRun(t, repo, "add", "tracked.txt")
	gitRun(t, repo, "commit", "-m", "feature")
	commitSnapshot := mustCollectWorkspace(t, repo)
	if commitSnapshot.GitHead == baseline.GitHead {
		t.Fatal("new commit was not captured")
	}
	assertDifference(t, CompareWorkspaceSnapshots(&branchSnapshot, &commitSnapshot, nil), "git.head", DriftSeverityNeedsConfirmation)

	gitRun(t, repo, "checkout", "--detach", baseline.GitHead)
	detached := mustCollectWorkspace(t, repo)
	if !detached.GitDetached || detached.GitBranch != "" || detached.GitHead != baseline.GitHead {
		t.Fatalf("detached snapshot=%+v", detached)
	}
	assertDifference(t, CompareWorkspaceSnapshots(&commitSnapshot, &detached, nil), "git.detached_head", DriftSeverityNeedsConfirmation)
}

func TestWorkspaceComparatorClassifiesNewAndChangedDirtyFiles(t *testing.T) {
	repo := newGitRepository(t)
	baseline := mustCollectWorkspace(t, repo)
	writeFile(t, filepath.Join(repo, "new.txt"), "one")
	added := mustCollectWorkspace(t, repo)
	addedReport := CompareWorkspaceSnapshots(&baseline, &added, nil)
	item := assertDifference(t, addedReport, "git.untracked_files", DriftSeverityNeedsConfirmation)
	if !strings.Contains(item.Reason, "new untracked") || addedReport.CLIAllowed {
		t.Fatalf("new untracked file classification=%+v", addedReport)
	}

	writeFile(t, filepath.Join(repo, "new.txt"), "two")
	changed := mustCollectWorkspace(t, repo)
	changedReport := CompareWorkspaceSnapshots(&added, &changed, nil)
	item = assertDifference(t, changedReport, "git.untracked_files", DriftSeverityNeedsConfirmation)
	if !strings.Contains(item.Reason, "content changed") || changedReport.CLIAllowed {
		t.Fatalf("changed untracked file classification=%+v", changedReport)
	}
}

func TestWorkspaceCollectorIgnoresFileTimestampOnlyChanges(t *testing.T) {
	repo := newGitRepository(t)
	baseline := mustCollectWorkspace(t, repo)
	path := filepath.Join(repo, "tracked.txt")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	changedTime := info.ModTime().Add(2 * time.Hour)
	if err := os.Chtimes(path, changedTime, changedTime); err != nil {
		t.Fatal(err)
	}
	current := mustCollectWorkspace(t, repo)
	report := CompareWorkspaceSnapshots(&baseline, &current, nil)
	if report.Severity != DriftSeverityClean || !report.CLIAllowed {
		t.Fatalf("timestamp-only file change should be ignored: %+v", report)
	}
}

func TestWorkspaceComparatorBlocksRepositoryIdentityChange(t *testing.T) {
	repo := newGitRepository(t)
	baseline := mustCollectWorkspace(t, repo)
	gitRun(t, repo, "remote", "set-url", "origin", "https://example.invalid/other.git")
	current := mustCollectWorkspace(t, repo)
	report := CompareWorkspaceSnapshots(&baseline, &current, nil)
	assertDifference(t, report, "git.repository_identity", DriftSeverityMustBlock)
	if report.CLIAllowed {
		t.Fatal("repository identity change must block CLI")
	}
}

func TestWorkspaceComparatorPreservesKnownDirtyStateAndAcceptsCheckpoint(t *testing.T) {
	repo := newGitRepository(t)
	writeFile(t, filepath.Join(repo, "tracked.txt"), "pre-existing dirty")
	writeFile(t, filepath.Join(repo, "existing.txt"), "pre-existing untracked")
	baseline := mustCollectWorkspace(t, repo)
	unchanged := mustCollectWorkspace(t, repo)
	cleanReport := CompareWorkspaceSnapshots(&baseline, &unchanged, nil)
	if cleanReport.Severity != DriftSeverityClean || !cleanReport.CLIAllowed || len(cleanReport.Differences) != 0 {
		t.Fatalf("unchanged dirty workspace should be clean: %+v", cleanReport)
	}

	writeFile(t, filepath.Join(repo, "task-output.txt"), "created by successful task")
	checkpoint := mustCollectWorkspace(t, repo)
	current := mustCollectWorkspace(t, repo)
	report := CompareWorkspaceSnapshots(&baseline, &current, &checkpoint)
	if !report.CLIAllowed || report.Severity != DriftSeveritySafeIgnore {
		t.Fatalf("matching checkpoint should be safely ignored: %+v", report)
	}
	assertDifference(t, report, "git.untracked_files", DriftSeveritySafeIgnore)
}

func TestWorkspaceComparatorBlocksConflicts(t *testing.T) {
	repo := newGitRepository(t)
	baseline := mustCollectWorkspace(t, repo)

	gitRun(t, repo, "checkout", "-b", "feature")
	writeFile(t, filepath.Join(repo, "tracked.txt"), "feature\n")
	gitRun(t, repo, "add", "tracked.txt")
	gitRun(t, repo, "commit", "-m", "feature change")
	gitRun(t, repo, "checkout", "main")
	writeFile(t, filepath.Join(repo, "tracked.txt"), "main\n")
	gitRun(t, repo, "add", "tracked.txt")
	gitRun(t, repo, "commit", "-m", "main change")
	command := exec.Command("git", "-C", repo, "merge", "feature")
	if output, err := command.CombinedOutput(); err == nil {
		t.Fatalf("merge unexpectedly succeeded: %s", output)
	}

	conflicted := mustCollectWorkspace(t, repo)
	if len(conflicted.ConflictedFiles) != 1 || conflicted.ConflictedFiles[0].Path != "tracked.txt" {
		t.Fatalf("conflicts=%+v", conflicted.ConflictedFiles)
	}
	report := CompareWorkspaceSnapshots(&baseline, &conflicted, nil)
	assertDifference(t, report, "git.conflicted_files", DriftSeverityMustBlock)
	if report.CLIAllowed || report.Severity != DriftSeverityMustBlock {
		t.Fatalf("conflict must block CLI: %+v", report)
	}
}

func TestExecutionContextComparatorClassifiesMetadataAndProducesStableFingerprint(t *testing.T) {
	repo := newGitRepository(t)
	workspace := mustCollectWorkspace(t, repo)
	capturedAt := time.Date(2026, 7, 15, 1, 2, 3, 0, time.UTC)
	baseline := validExecutionContext(workspace, capturedAt)

	t.Run("safe server versions and timestamp", func(t *testing.T) {
		current := baseline
		current.CapturedAt = capturedAt.Add(time.Minute)
		current.Workspace.CapturedAt = current.CapturedAt
		current.ProjectVersion++
		current.ConfigVersion++
		current.PlanResourceVersion++
		report := CompareExecutionContexts(&baseline, current, nil)
		if report.Severity != DriftSeveritySafeIgnore || !report.CLIAllowed {
			t.Fatalf("safe changes should not gate CLI: %+v", report)
		}
		assertDifference(t, report, "snapshot.captured_at", DriftSeveritySafeIgnore)
		assertDifference(t, report, "project.version", DriftSeveritySafeIgnore)

		later := current
		later.CapturedAt = current.CapturedAt.Add(time.Minute)
		later.Workspace.CapturedAt = later.CapturedAt
		later.ProjectVersion++
		later.ConfigVersion++
		later.PlanResourceVersion++
		laterReport := CompareExecutionContexts(&baseline, later, nil)
		if report.Fingerprint != laterReport.Fingerprint {
			t.Fatalf("safe timestamp/version growth changed fingerprint: %s != %s", report.Fingerprint, laterReport.Fingerprint)
		}
	})

	t.Run("provider and settings require confirmation", func(t *testing.T) {
		current := baseline
		current.ExecutionProvider = "claude"
		current.ConfigVersion++
		current.KeyExecutionFields = json.RawMessage(`{"validationCommand":"go test ./...","maxRetries":3}`)
		report := CompareExecutionContexts(&baseline, current, nil)
		if report.Severity != DriftSeverityNeedsConfirmation || report.CLIAllowed {
			t.Fatalf("provider/settings change should require confirmation: %+v", report)
		}
		assertDifference(t, report, "provider.execution", DriftSeverityNeedsConfirmation)
		assertDifference(t, report, "settings.key_execution_fields", DriftSeverityNeedsConfirmation)
	})

	t.Run("requirement change blocks", func(t *testing.T) {
		current := baseline
		current.RequirementVersion++
		current.RequirementDigest = hashText("changed requirement")
		report := CompareExecutionContexts(&baseline, current, nil)
		if report.Severity != DriftSeverityMustBlock || report.CLIAllowed {
			t.Fatalf("requirement change should block: %+v", report)
		}
		assertDifference(t, report, "requirement.version", DriftSeverityMustBlock)
		assertDifference(t, report, "requirement.digest", DriftSeverityMustBlock)
	})

	t.Run("project path change blocks", func(t *testing.T) {
		current := baseline
		current.Workspace.ConfiguredPath = repo + "-other"
		report := CompareExecutionContexts(&baseline, current, nil)
		assertDifference(t, report, "workspace.configured_path", DriftSeverityMustBlock)
		if report.CLIAllowed {
			t.Fatal("project path change must block CLI")
		}
	})

	t.Run("missing and damaged snapshots block", func(t *testing.T) {
		missing := CompareExecutionContexts(nil, baseline, nil)
		assertDifference(t, missing, "snapshot.baseline", DriftSeverityMustBlock)
		if missing.CLIAllowed {
			t.Fatal("missing baseline must block CLI")
		}
		damaged := baseline
		damaged.Workspace.ContentDigest = "broken"
		report := CompareExecutionContexts(&baseline, damaged, nil)
		assertDifference(t, report, "snapshot.current_integrity", DriftSeverityMustBlock)
		if report.CLIAllowed {
			t.Fatal("damaged current snapshot must block CLI")
		}
	})

	t.Run("actionable environment changes alter fingerprint", func(t *testing.T) {
		first := baseline
		first.ExecutionProvider = "claude"
		firstReport := CompareExecutionContexts(&baseline, first, nil)
		second := first
		second.RequirementDigest = hashText("new body")
		secondReport := CompareExecutionContexts(&baseline, second, nil)
		if firstReport.Fingerprint == secondReport.Fingerprint {
			t.Fatal("actionable context change did not invalidate report fingerprint")
		}
	})
}

func TestCollectExecutionContextRejectsUnreadableOrIncompleteSnapshot(t *testing.T) {
	input := ExecutionContextInput{
		RequirementID:       uuid.New(),
		RequirementVersion:  1,
		RequirementDigest:   hashText("requirement"),
		PlanID:              uuid.New(),
		PlanResourceVersion: 1,
		PlanContentVersion:  1,
		PlanSpecDigest:      hashText("plan"),
		ProjectVersion:      1,
		ConfigVersion:       1,
		KeyExecutionFields:  json.RawMessage(`{"maxRetries":2}`),
		GenerationProvider:  "codex",
		ExecutionProvider:   "codex",
		WorkspacePath:       filepath.Join(t.TempDir(), "missing"),
	}
	snapshot, err := CollectExecutionContext(context.Background(), input)
	if err == nil || snapshot.IntegrityError == "" || snapshot.Workspace.CaptureError == "" {
		t.Fatalf("missing workspace should return a comparison-ready failure: snapshot=%+v err=%v", snapshot, err)
	}
	baseline := validExecutionContext(mustCollectWorkspace(t, newGitRepository(t)), time.Now())
	report := CompareExecutionContexts(&baseline, snapshot, nil)
	if report.Severity != DriftSeverityMustBlock || report.CLIAllowed {
		t.Fatalf("unreadable workspace must block: %+v", report)
	}
}

func TestReadOnlyGitQueryAllowlistRejectsMutationCommands(t *testing.T) {
	for _, args := range [][]string{
		{"clean", "-fd"},
		{"reset", "--hard"},
		{"checkout", "main"},
		{"restore", "."},
		{"symbolic-ref", "HEAD", "refs/heads/other"},
		{"config", "user.name", "mutated"},
	} {
		if err := validateReadOnlyGitQuery(args); err == nil {
			t.Fatalf("mutation command allowed: git %s", strings.Join(args, " "))
		}
	}
	for _, args := range [][]string{
		{"rev-parse", "HEAD"},
		{"symbolic-ref", "--quiet", "--short", "HEAD"},
		{"status", "--porcelain=v1"},
		{"ls-files", "--stage"},
		{"config", "--get", "remote.origin.url"},
	} {
		if err := validateReadOnlyGitQuery(args); err != nil {
			t.Fatalf("read-only query rejected: git %s: %v", strings.Join(args, " "), err)
		}
	}
}

func newGitRepository(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	gitRun(t, repo, "init", "-b", "main")
	gitRun(t, repo, "config", "user.name", "SpecRelay Test")
	gitRun(t, repo, "config", "user.email", "specrelay@example.invalid")
	gitRun(t, repo, "remote", "add", "origin", "https://example.invalid/specrelay.git")
	writeFile(t, filepath.Join(repo, "tracked.txt"), "initial\n")
	gitRun(t, repo, "add", "tracked.txt")
	gitRun(t, repo, "commit", "-m", "initial")
	return repo
}

func gitRun(t *testing.T, repo string, args ...string) {
	t.Helper()
	commandArgs := append([]string{"-C", repo}, args...)
	command := exec.Command("git", commandArgs...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
}

func gitOutput(t *testing.T, repo string, args ...string) string {
	t.Helper()
	commandArgs := append([]string{"-C", repo}, args...)
	output, err := exec.Command("git", commandArgs...).Output()
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(output))
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func fileSHA256(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

func mustCollectWorkspace(t *testing.T, path string) WorkspaceSnapshot {
	t.Helper()
	snapshot, err := CollectWorkspaceSnapshot(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func validExecutionContext(workspace WorkspaceSnapshot, capturedAt time.Time) ExecutionContextSnapshot {
	workspace.CapturedAt = capturedAt
	return ExecutionContextSnapshot{
		CapturedAt:          capturedAt,
		RequirementID:       uuid.MustParse("11111111-1111-4111-8111-111111111111"),
		RequirementVersion:  1,
		RequirementDigest:   hashText("requirement"),
		PlanID:              uuid.MustParse("22222222-2222-4222-8222-222222222222"),
		PlanResourceVersion: 1,
		PlanContentVersion:  1,
		PlanSpecDigest:      hashText("plan"),
		ProjectVersion:      1,
		ConfigVersion:       1,
		KeyExecutionFields:  json.RawMessage(`{"maxRetries":2,"validationCommand":"go test ./..."}`),
		GenerationProvider:  "codex",
		ExecutionProvider:   "codex",
		Workspace:           workspace,
	}
}

func hashText(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func assertDifference(t *testing.T, report DriftReport, field string, severity DriftSeverity) ContextDifference {
	t.Helper()
	for _, item := range report.Differences {
		if item.Field == field {
			if item.Severity != severity {
				t.Fatalf("difference %s severity=%s, want %s: %+v", field, item.Severity, severity, report)
			}
			if item.Reason == "" || item.RecommendedAction == "" {
				t.Fatalf("difference lacks reason/action: %+v", item)
			}
			return item
		}
	}
	t.Fatalf("difference %s not found: %+v", field, report)
	return ContextDifference{}
}

func TestWorkspaceCaptureFailureErrorIdentity(t *testing.T) {
	// Keep the typed error path covered so callers can use ordinary errors.Is
	// checks on the underlying filesystem error returned by collection.
	_, err := CollectWorkspaceSnapshot(context.Background(), filepath.Join(t.TempDir(), "missing"))
	if err == nil || errors.Is(err, context.Canceled) {
		t.Fatalf("unexpected error: %v", err)
	}
}
