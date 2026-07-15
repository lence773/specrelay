package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGitCheckpointRejectsUnsafeWorkspaceBeforeGit(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "git-was-started")
	fakeGit := filepath.Join(t.TempDir(), "git")
	writeFile(t, fakeGit, "#!/bin/sh\ntouch "+marker+"\nexit 99\n")
	if err := os.Chmod(fakeGit, 0o755); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	engine, err := NewGitCheckpointEngine(root)
	if err != nil {
		t.Fatal(err)
	}
	engine.GitBinary = fakeGit
	if _, err = engine.CreateCheckpoint(context.Background(), root); !errors.Is(err, ErrNotGitWorkspace) {
		t.Fatalf("non-Git workspace error=%v", err)
	}
	if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("Git started before non-repository validation: %v", statErr)
	}

	repo := newGitRepository(t)
	subdirectory := filepath.Join(repo, "subdir")
	if err = os.Mkdir(subdirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	engine, err = NewGitCheckpointEngine(repo)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = engine.CreateCheckpoint(context.Background(), subdirectory); !errors.Is(err, ErrNotGitWorkspace) && !errors.Is(err, ErrUnsafeGitWorkspace) {
		t.Fatalf("nested workspace error=%v", err)
	}
}

func TestGitCheckpointRejectsDamagedIndexWithoutModification(t *testing.T) {
	repo := newGitRepository(t)
	indexPath := filepath.Join(repo, ".git", "index")
	if err := os.WriteFile(indexPath, []byte("damaged-index"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := fileSHA256(t, indexPath)
	trackedBefore := string(mustReadFile(t, filepath.Join(repo, "tracked.txt")))
	if _, err := CreateGitCheckpoint(context.Background(), repo); !errors.Is(err, ErrGitRepository) {
		t.Fatalf("damaged repository error=%v", err)
	}
	if got := fileSHA256(t, indexPath); got != before {
		t.Fatalf("damaged index was modified: %s != %s", got, before)
	}
	if got := string(mustReadFile(t, filepath.Join(repo, "tracked.txt"))); got != trackedBefore {
		t.Fatalf("work tree was modified: %q != %q", got, trackedBefore)
	}
}

func TestGitCheckpointUsesTemporaryIndexAndCapturesDirtySummary(t *testing.T) {
	repo := newGitRepository(t)
	writeFile(t, filepath.Join(repo, ".gitignore"), "ignored.txt\n")
	writeFile(t, filepath.Join(repo, "delete.txt"), "delete me\n")
	writeFile(t, filepath.Join(repo, "rename.txt"), "rename me\n")
	if err := os.Symlink("tracked.txt", filepath.Join(repo, "link")); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "add", ".gitignore", "delete.txt", "rename.txt", "link")
	gitRun(t, repo, "commit", "-m", "fixtures")

	writeFile(t, filepath.Join(repo, "tracked.txt"), "staged\n")
	gitRun(t, repo, "add", "tracked.txt")
	writeFile(t, filepath.Join(repo, "tracked.txt"), "worktree\n")
	gitRun(t, repo, "mv", "rename.txt", "renamed.txt")
	if err := os.Remove(filepath.Join(repo, "delete.txt")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(repo, "untracked.txt"), "untracked\n")
	writeFile(t, filepath.Join(repo, "ignored.txt"), "ignored\n")

	indexBefore := fileSHA256(t, filepath.Join(repo, ".git", "index"))
	headBefore := gitOutput(t, repo, "rev-parse", "HEAD")
	statusBefore := gitOutput(t, repo, "status", "--porcelain=v1", "--untracked-files=all")
	checkpoint, err := CreateGitCheckpoint(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if got := fileSHA256(t, filepath.Join(repo, ".git", "index")); got != indexBefore {
		t.Fatalf("user index changed: %s != %s", got, indexBefore)
	}
	if got := gitOutput(t, repo, "rev-parse", "HEAD"); got != headBefore {
		t.Fatalf("HEAD changed: %s != %s", got, headBefore)
	}
	if got := gitOutput(t, repo, "status", "--porcelain=v1", "--untracked-files=all"); got != statusBefore {
		t.Fatalf("status changed:\n%s\nwant:\n%s", got, statusBefore)
	}
	if !checkpoint.Status.Dirty || !checkpoint.Status.PreExistingChanges {
		t.Fatalf("dirty baseline not marked: %+v", checkpoint.Status)
	}
	assertContains(t, checkpoint.Status.Staged, "tracked.txt")
	assertContains(t, checkpoint.Status.Unstaged, "tracked.txt")
	assertContains(t, checkpoint.Status.Untracked, "untracked.txt")
	assertContains(t, checkpoint.Status.Deleted, "delete.txt")
	if len(checkpoint.Status.Renamed) != 1 || !strings.Contains(checkpoint.Status.Renamed[0], "rename.txt") || !strings.Contains(checkpoint.Status.Renamed[0], "renamed.txt") {
		t.Fatalf("rename summary=%v", checkpoint.Status.Renamed)
	}
	if got := gitOutput(t, repo, "show", checkpoint.WorktreeTree+":tracked.txt"); got != "worktree" {
		t.Fatalf("worktree tree has %q", got)
	}
	if got := gitOutput(t, repo, "show", checkpoint.IndexTree+":tracked.txt"); got != "staged" {
		t.Fatalf("index tree has %q", got)
	}
	if got := gitOutput(t, repo, "show", checkpoint.WorktreeTree+":untracked.txt"); got != "untracked" {
		t.Fatalf("untracked file absent from worktree tree: %q", got)
	}
	if output, runErr := exec.Command("git", "-C", repo, "cat-file", "-e", checkpoint.WorktreeTree+":ignored.txt").CombinedOutput(); runErr == nil {
		t.Fatalf("ignored file entered checkpoint tree: %s", output)
	}
	for _, ref := range []string{checkpoint.Refs.Worktree, checkpoint.Refs.IndexTree, checkpoint.Refs.Index, checkpoint.Refs.Metadata, checkpoint.Refs.Head} {
		if got := gitOutput(t, repo, "rev-parse", "--verify", ref); got == "" {
			t.Fatalf("checkpoint ref %s missing", ref)
		}
	}
	writeFile(t, filepath.Join(repo, "task-new.txt"), "new task change\n")
	after, err := CreateGitCheckpoint(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	diff, err := CompareGitCheckpoints(context.Background(), checkpoint, after)
	if err != nil {
		t.Fatal(err)
	}
	if len(diff.Files) != 1 || diff.Files[0].Path != "task-new.txt" || diff.Files[0].Status != GitChangeAdded {
		t.Fatalf("pre-existing dirty state was misreported as task changes: %+v", diff.Files)
	}
}

func TestGitCheckpointDiffIsStructuredAndUnifiedDiffIsBounded(t *testing.T) {
	repo := newGitRepository(t)
	writeFile(t, filepath.Join(repo, "delete.txt"), "one\ntwo\n")
	writeFile(t, filepath.Join(repo, "rename.txt"), "same\n")
	gitRun(t, repo, "add", "delete.txt", "rename.txt")
	gitRun(t, repo, "commit", "-m", "diff fixtures")
	engine, err := NewGitCheckpointEngine(repo)
	if err != nil {
		t.Fatal(err)
	}
	engine.MaxDiffFileSize = 100
	engine.MaxDiffBytes = 180
	engine.MaxDiffLines = 8
	engine.MaxDiffContext = 2
	before, err := engine.CreateCheckpoint(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}

	writeFile(t, filepath.Join(repo, "tracked.txt"), "initial\nchanged\n")
	writeFile(t, filepath.Join(repo, "added.txt"), "added\n")
	if err = os.Remove(filepath.Join(repo, "delete.txt")); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "mv", "rename.txt", "renamed.txt")
	if err = os.WriteFile(filepath.Join(repo, "binary.bin"), []byte{0, 1, 2, 3}, 0o600); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(repo, "large.txt"), strings.Repeat("large line\n", 80))
	marker := filepath.Join(t.TempDir(), "external-diff-ran")
	external := filepath.Join(t.TempDir(), "external-diff")
	writeFile(t, external, "#!/bin/sh\ntouch "+marker+"\nexit 0\n")
	if err = os.Chmod(external, 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "config", "diff.external", external)
	after, err := engine.CreateCheckpoint(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	diff, err := engine.Compare(context.Background(), before, after)
	if err != nil {
		t.Fatal(err)
	}
	assertDiffStatus(t, diff, "added.txt", GitChangeAdded)
	modified := assertDiffStatus(t, diff, "tracked.txt", GitChangeModified)
	assertDiffStatus(t, diff, "delete.txt", GitChangeDeleted)
	renamed := assertDiffStatus(t, diff, "renamed.txt", GitChangeRenamed)
	if renamed.PreviousPath != "rename.txt" {
		t.Fatalf("rename=%+v", renamed)
	}
	binary := assertDiffStatus(t, diff, "binary.bin", GitChangeAdded)
	if !binary.Binary {
		t.Fatalf("binary file not flagged: %+v", binary)
	}
	large := assertDiffStatus(t, diff, "large.txt", GitChangeAdded)
	if !large.Oversized {
		t.Fatalf("large file not flagged: %+v", large)
	}
	if diff.Additions == 0 || diff.Deletions == 0 {
		t.Fatalf("line totals not captured: %+v", diff)
	}
	patch, err := engine.UnifiedDiff(context.Background(), before, after, modified, UnifiedDiffOptions{MaxBytes: 80, MaxLines: 4, ContextLines: 99})
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(patch.Patch)) > 80 || patch.Lines > 4 || !patch.Truncated {
		t.Fatalf("unified diff limits not enforced: %+v", patch)
	}
	if _, err = engine.UnifiedDiff(context.Background(), before, after, binary, UnifiedDiffOptions{}); !errors.Is(err, ErrDiffUnavailable) {
		t.Fatalf("binary unified diff error=%v", err)
	}
	if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("external diff executed: %v", statErr)
	}
}

func TestGitCheckpointRestoreReturnsExactStateAndPreservesIgnoredFiles(t *testing.T) {
	repo := newGitRepository(t)
	writeFile(t, filepath.Join(repo, ".gitignore"), "ignored.txt\ncache/ignored.log\n")
	gitRun(t, repo, "add", ".gitignore")
	gitRun(t, repo, "commit", "-m", "ignore fixture")
	writeFile(t, filepath.Join(repo, "tracked.txt"), "staged baseline\n")
	gitRun(t, repo, "add", "tracked.txt")
	writeFile(t, filepath.Join(repo, "tracked.txt"), "unstaged baseline\n")
	writeFile(t, filepath.Join(repo, "baseline-untracked.txt"), "baseline untracked\n")
	writeFile(t, filepath.Join(repo, "ignored.txt"), "ignored at checkpoint\n")
	if err := os.Symlink("tracked.txt", filepath.Join(repo, "baseline-link")); err != nil {
		t.Fatal(err)
	}
	checkpoint, err := CreateGitCheckpoint(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	indexAtCheckpoint := fileSHA256(t, filepath.Join(repo, ".git", "index"))

	gitRun(t, repo, "checkout", "-b", "other")
	writeFile(t, filepath.Join(repo, "tracked.txt"), "other branch\n")
	writeFile(t, filepath.Join(repo, "extra.txt"), "remove me\n")
	if err = os.Mkdir(filepath.Join(repo, "cache"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(repo, "cache", "extra.txt"), "remove nested controlled file\n")
	writeFile(t, filepath.Join(repo, "cache", "ignored.log"), "preserve nested ignored file\n")
	writeFile(t, filepath.Join(repo, "ignored.txt"), "must survive restore\n")
	if err = os.Remove(filepath.Join(repo, "baseline-untracked.txt")); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(filepath.Join(repo, "baseline-link")); err != nil {
		t.Fatal(err)
	}
	engine, err := NewGitCheckpointEngine(repo)
	if err != nil {
		t.Fatal(err)
	}
	current, err := engine.Inspect(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if err = engine.Restore(context.Background(), checkpoint, RestoreGitCheckpointOptions{ExpectedCurrentFingerprint: current.Fingerprint}); err != nil {
		t.Fatal(err)
	}
	restored, err := engine.Inspect(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if restored.Fingerprint != checkpoint.Fingerprint {
		t.Fatalf("restored fingerprint=%s want %s", restored.Fingerprint, checkpoint.Fingerprint)
	}
	if got := gitOutput(t, repo, "branch", "--show-current"); got != "main" {
		t.Fatalf("branch=%q", got)
	}
	if got := fileSHA256(t, filepath.Join(repo, ".git", "index")); got != indexAtCheckpoint {
		t.Fatalf("index fingerprint=%s want %s", got, indexAtCheckpoint)
	}
	if got := string(mustReadFile(t, filepath.Join(repo, "tracked.txt"))); got != "unstaged baseline\n" {
		t.Fatalf("tracked content=%q", got)
	}
	if got := string(mustReadFile(t, filepath.Join(repo, "baseline-untracked.txt"))); got != "baseline untracked\n" {
		t.Fatalf("untracked content=%q", got)
	}
	link, err := os.Readlink(filepath.Join(repo, "baseline-link"))
	if err != nil || link != "tracked.txt" {
		t.Fatalf("symlink=%q err=%v", link, err)
	}
	if _, err = os.Stat(filepath.Join(repo, "extra.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("extra controlled file survived: %v", err)
	}
	if got := string(mustReadFile(t, filepath.Join(repo, "ignored.txt"))); got != "must survive restore\n" {
		t.Fatalf("ignored file changed: %q", got)
	}
	if got := string(mustReadFile(t, filepath.Join(repo, "cache", "ignored.log"))); got != "preserve nested ignored file\n" {
		t.Fatalf("nested ignored file changed: %q", got)
	}
	if _, err = os.Stat(filepath.Join(repo, "cache", "extra.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("nested extra controlled file survived: %v", err)
	}

	writeFile(t, filepath.Join(repo, "raced.txt"), "change after approval\n")
	if err = engine.Restore(context.Background(), checkpoint, RestoreGitCheckpointOptions{ExpectedCurrentFingerprint: restored.Fingerprint}); !errors.Is(err, ErrWorkspaceChanged) {
		t.Fatalf("restore race validation error=%v", err)
	}
	if got := string(mustReadFile(t, filepath.Join(repo, "raced.txt"))); got != "change after approval\n" {
		t.Fatalf("failed validation modified files: %q", got)
	}
}

func TestGitCheckpointRestoresUnbornBranch(t *testing.T) {
	repo := t.TempDir()
	gitRun(t, repo, "init", "-b", "main")
	gitRun(t, repo, "config", "user.name", "SpecRelay Test")
	gitRun(t, repo, "config", "user.email", "specrelay@example.invalid")
	writeFile(t, filepath.Join(repo, "first.txt"), "unborn content\n")
	checkpoint, err := CreateGitCheckpoint(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if !checkpoint.Unborn || checkpoint.Head != "" || checkpoint.IndexPresent {
		t.Fatalf("unborn checkpoint=%+v", checkpoint)
	}
	gitRun(t, repo, "add", "first.txt")
	gitRun(t, repo, "commit", "-m", "first")
	engine, err := NewGitCheckpointEngine(repo)
	if err != nil {
		t.Fatal(err)
	}
	current, err := engine.Inspect(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if err = engine.Restore(context.Background(), checkpoint, RestoreGitCheckpointOptions{ExpectedCurrentFingerprint: current.Fingerprint}); err != nil {
		t.Fatal(err)
	}
	if command := exec.Command("git", "-C", repo, "rev-parse", "--verify", "HEAD"); command.Run() == nil {
		t.Fatal("HEAD still resolves after restoring unborn branch")
	}
	if got := gitOutput(t, repo, "status", "--porcelain=v1", "--untracked-files=all"); got != "?? first.txt" {
		t.Fatalf("unborn status=%q", got)
	}
}

func TestGitCheckpointMarksDirtySubmodule(t *testing.T) {
	submodule := newGitRepository(t)
	repo := newGitRepository(t)
	command := exec.Command("git", "-c", "protocol.file.allow=always", "-C", repo, "submodule", "add", submodule, "module")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("add submodule: %v\n%s", err, output)
	}
	gitRun(t, repo, "commit", "-am", "submodule")
	writeFile(t, filepath.Join(repo, "module", "tracked.txt"), "dirty submodule\n")
	checkpoint, err := CreateGitCheckpoint(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	assertContains(t, checkpoint.Status.Submodules, "module")
	assertContains(t, checkpoint.Status.DirtySubmodules, "module")
}

func assertContains(t *testing.T, values []string, expected string) {
	t.Helper()
	for _, value := range values {
		if value == expected {
			return
		}
	}
	t.Fatalf("%q not found in %v", expected, values)
}

func assertDiffStatus(t *testing.T, diff GitTreeDiff, path string, status GitChangeStatus) GitFileDiff {
	t.Helper()
	for _, file := range diff.Files {
		if file.Path == path {
			if file.Status != status {
				t.Fatalf("%s status=%s want %s: %+v", path, file.Status, status, file)
			}
			return file
		}
	}
	t.Fatalf("diff path %q missing: %+v", path, diff.Files)
	return GitFileDiff{}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Clone(content)
}
