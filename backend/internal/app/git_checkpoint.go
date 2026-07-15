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
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/lyming99/specrelay/backend/internal/security"
)

const (
	defaultGitOperationTimeout = 20 * time.Second
	defaultMaxDiffBytes        = int64(512 * 1024)
	defaultMaxDiffLines        = 12000
	defaultMaxDiffContext      = 20
	defaultMaxDiffFileBytes    = int64(4 * 1024 * 1024)
)

var (
	ErrNotGitWorkspace    = errors.New("workspace is not a Git work tree")
	ErrUnsafeGitWorkspace = errors.New("Git work tree is outside the normalized project workspace")
	ErrGitRepository      = errors.New("Git repository is damaged or cannot be read")
	ErrCheckpointMismatch = errors.New("checkpoint does not belong to the current Git workspace")
	ErrWorkspaceChanged   = errors.New("workspace state changed since it was approved for restore")
	ErrDiffUnavailable    = errors.New("unified diff is unavailable for this file")
)

// GitCheckpointEngine creates immutable Git object snapshots without touching
// the user's index or current branch. AllowedRoots is optional; when empty the
// configured project directory itself is the only allowed work-tree root.
type GitCheckpointEngine struct {
	GitBinary       string
	Timeout         time.Duration
	AllowedRoots    []string
	MaxDiffBytes    int64
	MaxDiffLines    int
	MaxDiffContext  int
	MaxDiffFileSize int64
}

type GitCheckpointRefs struct {
	Worktree  string `json:"worktree"`
	IndexTree string `json:"indexTree"`
	Index     string `json:"index"`
	Metadata  string `json:"metadata"`
	Head      string `json:"head,omitempty"`
}

type GitStatusSummary struct {
	Dirty              bool     `json:"dirty"`
	PreExistingChanges bool     `json:"preExistingChanges"`
	Staged             []string `json:"staged,omitempty"`
	Unstaged           []string `json:"unstaged,omitempty"`
	Untracked          []string `json:"untracked,omitempty"`
	Deleted            []string `json:"deleted,omitempty"`
	Renamed            []string `json:"renamed,omitempty"`
	Submodules         []string `json:"submodules,omitempty"`
	DirtySubmodules    []string `json:"dirtySubmodules,omitempty"`
	Conflicted         []string `json:"conflicted,omitempty"`
}

type GitWorkspaceState struct {
	WorkspaceRoot      string           `json:"workspaceRoot"`
	GitDirectory       string           `json:"gitDirectory"`
	CommonDirectory    string           `json:"commonDirectory"`
	RepositoryIdentity string           `json:"repositoryIdentity"`
	HeadRef            string           `json:"headRef,omitempty"`
	Branch             string           `json:"branch,omitempty"`
	Head               string           `json:"head,omitempty"`
	Detached           bool             `json:"detached"`
	Unborn             bool             `json:"unborn"`
	IndexPresent       bool             `json:"indexPresent"`
	IndexTree          string           `json:"indexTree"`
	IndexBlob          string           `json:"indexBlob"`
	WorktreeTree       string           `json:"worktreeTree"`
	Status             GitStatusSummary `json:"status"`
	Fingerprint        string           `json:"fingerprint"`
}

type GitCheckpoint struct {
	ID                 string            `json:"id"`
	CapturedAt         time.Time         `json:"capturedAt"`
	WorkspaceRoot      string            `json:"workspaceRoot"`
	GitDirectory       string            `json:"gitDirectory"`
	CommonDirectory    string            `json:"commonDirectory"`
	RepositoryIdentity string            `json:"repositoryIdentity"`
	HeadRef            string            `json:"headRef,omitempty"`
	Branch             string            `json:"branch,omitempty"`
	Head               string            `json:"head,omitempty"`
	Detached           bool              `json:"detached"`
	Unborn             bool              `json:"unborn"`
	IndexPresent       bool              `json:"indexPresent"`
	IndexTree          string            `json:"indexTree"`
	IndexBlob          string            `json:"indexBlob"`
	WorktreeTree       string            `json:"worktreeTree"`
	Status             GitStatusSummary  `json:"status"`
	Fingerprint        string            `json:"fingerprint"`
	Refs               GitCheckpointRefs `json:"refs"`
	MetadataBlob       string            `json:"metadataBlob"`
}

type GitChangeStatus string

const (
	GitChangeAdded    GitChangeStatus = "added"
	GitChangeModified GitChangeStatus = "modified"
	GitChangeDeleted  GitChangeStatus = "deleted"
	GitChangeRenamed  GitChangeStatus = "renamed"
	GitChangeCopied   GitChangeStatus = "copied"
	GitChangeType     GitChangeStatus = "type_changed"
	GitChangeUnknown  GitChangeStatus = "unknown"
)

type GitFileDiff struct {
	Path         string          `json:"path"`
	PreviousPath string          `json:"previousPath,omitempty"`
	Status       GitChangeStatus `json:"status"`
	Similarity   int             `json:"similarity,omitempty"`
	Additions    int64           `json:"additions"`
	Deletions    int64           `json:"deletions"`
	OldMode      string          `json:"oldMode,omitempty"`
	NewMode      string          `json:"newMode,omitempty"`
	OldObject    string          `json:"oldObject,omitempty"`
	NewObject    string          `json:"newObject,omitempty"`
	Binary       bool            `json:"binary"`
	Oversized    bool            `json:"oversized"`
	Unreadable   bool            `json:"unreadable"`
}

type GitTreeDiff struct {
	BeforeTree string        `json:"beforeTree"`
	AfterTree  string        `json:"afterTree"`
	Files      []GitFileDiff `json:"files"`
	Additions  int64         `json:"additions"`
	Deletions  int64         `json:"deletions"`
}

type UnifiedDiffOptions struct {
	MaxBytes     int64
	MaxLines     int
	ContextLines int
}

type UnifiedFileDiff struct {
	Path          string `json:"path"`
	PreviousPath  string `json:"previousPath,omitempty"`
	Patch         string `json:"patch,omitempty"`
	Truncated     bool   `json:"truncated"`
	Binary        bool   `json:"binary"`
	Oversized     bool   `json:"oversized"`
	Unreadable    bool   `json:"unreadable"`
	OmittedReason string `json:"omittedReason,omitempty"`
	Bytes         int64  `json:"bytes"`
	Lines         int    `json:"lines"`
}

type RestoreGitCheckpointOptions struct {
	// ExpectedCurrentFingerprint is the state explicitly approved by the
	// caller. When set, restore aborts before any file or ref mutation if the
	// live workspace differs.
	ExpectedCurrentFingerprint string
}

type gitRepository struct {
	root      string
	gitDir    string
	commonDir string
	identity  string
	indexPath string
}

type gitTreeEntry struct {
	Mode string
	Type string
	OID  string
	Path string
}

func NewGitCheckpointEngine(allowedRoots ...string) (*GitCheckpointEngine, error) {
	engine := &GitCheckpointEngine{
		GitBinary:       "git",
		Timeout:         defaultGitOperationTimeout,
		MaxDiffBytes:    defaultMaxDiffBytes,
		MaxDiffLines:    defaultMaxDiffLines,
		MaxDiffContext:  defaultMaxDiffContext,
		MaxDiffFileSize: defaultMaxDiffFileBytes,
	}
	for _, root := range allowedRoots {
		identity, err := security.InspectExistingPath(root)
		if err != nil {
			return nil, fmt.Errorf("normalize allowed Git workspace root %q: %w", root, err)
		}
		engine.AllowedRoots = append(engine.AllowedRoots, identity.Real)
	}
	return engine, nil
}

func CreateGitCheckpoint(ctx context.Context, workspace string) (GitCheckpoint, error) {
	engine, err := NewGitCheckpointEngine(workspace)
	if err != nil {
		return GitCheckpoint{}, err
	}
	return engine.CreateCheckpoint(ctx, workspace)
}

func InspectGitWorkspace(ctx context.Context, workspace string) (GitWorkspaceState, error) {
	engine, err := NewGitCheckpointEngine(workspace)
	if err != nil {
		return GitWorkspaceState{}, err
	}
	return engine.Inspect(ctx, workspace)
}

func CompareGitCheckpoints(ctx context.Context, before, after GitCheckpoint) (GitTreeDiff, error) {
	engine, err := NewGitCheckpointEngine(before.WorkspaceRoot)
	if err != nil {
		return GitTreeDiff{}, err
	}
	return engine.Compare(ctx, before, after)
}

func RestoreGitCheckpoint(ctx context.Context, checkpoint GitCheckpoint, options ...RestoreGitCheckpointOptions) error {
	engine, err := NewGitCheckpointEngine(checkpoint.WorkspaceRoot)
	if err != nil {
		return err
	}
	var option RestoreGitCheckpointOptions
	if len(options) > 0 {
		option = options[0]
	}
	return engine.Restore(ctx, checkpoint, option)
}

func (engine *GitCheckpointEngine) CreateCheckpoint(ctx context.Context, workspace string) (GitCheckpoint, error) {
	repo, err := engine.openRepository(ctx, workspace)
	if err != nil {
		return GitCheckpoint{}, err
	}
	state, err := engine.captureState(ctx, repo, true)
	if err != nil {
		return GitCheckpoint{}, err
	}
	id := uuid.NewString()
	baseRef := "refs/specrelay/checkpoints/" + id
	checkpoint := GitCheckpoint{
		ID: id, CapturedAt: time.Now().UTC(), WorkspaceRoot: state.WorkspaceRoot,
		GitDirectory: state.GitDirectory, CommonDirectory: state.CommonDirectory,
		RepositoryIdentity: state.RepositoryIdentity, HeadRef: state.HeadRef,
		Branch: state.Branch, Head: state.Head, Detached: state.Detached,
		Unborn: state.Unborn, IndexPresent: state.IndexPresent,
		IndexTree: state.IndexTree, IndexBlob: state.IndexBlob,
		WorktreeTree: state.WorktreeTree, Status: state.Status,
		Fingerprint: state.Fingerprint,
		Refs: GitCheckpointRefs{
			Worktree:  baseRef + "/worktree",
			IndexTree: baseRef + "/index-tree",
			Index:     baseRef + "/index",
			Metadata:  baseRef + "/metadata",
		},
	}
	if checkpoint.Head != "" {
		checkpoint.Refs.Head = baseRef + "/head"
	}
	metadata, err := json.Marshal(struct {
		ID                 string           `json:"id"`
		CapturedAt         time.Time        `json:"capturedAt"`
		WorkspaceRoot      string           `json:"workspaceRoot"`
		RepositoryIdentity string           `json:"repositoryIdentity"`
		HeadRef            string           `json:"headRef,omitempty"`
		Head               string           `json:"head,omitempty"`
		Detached           bool             `json:"detached"`
		Unborn             bool             `json:"unborn"`
		IndexPresent       bool             `json:"indexPresent"`
		IndexTree          string           `json:"indexTree"`
		IndexBlob          string           `json:"indexBlob"`
		WorktreeTree       string           `json:"worktreeTree"`
		Status             GitStatusSummary `json:"status"`
		Fingerprint        string           `json:"fingerprint"`
	}{
		ID: checkpoint.ID, CapturedAt: checkpoint.CapturedAt,
		WorkspaceRoot: checkpoint.WorkspaceRoot, RepositoryIdentity: checkpoint.RepositoryIdentity,
		HeadRef: checkpoint.HeadRef, Head: checkpoint.Head, Detached: checkpoint.Detached,
		Unborn: checkpoint.Unborn, IndexPresent: checkpoint.IndexPresent,
		IndexTree: checkpoint.IndexTree, IndexBlob: checkpoint.IndexBlob,
		WorktreeTree: checkpoint.WorktreeTree, Status: checkpoint.Status,
		Fingerprint: checkpoint.Fingerprint,
	})
	if err != nil {
		return GitCheckpoint{}, fmt.Errorf("encode Git checkpoint metadata: %w", err)
	}
	metadataOID, err := engine.gitInput(ctx, repo, metadata, nil, "hash-object", "-w", "--stdin")
	if err != nil {
		return GitCheckpoint{}, fmt.Errorf("store Git checkpoint metadata: %w", err)
	}
	checkpoint.MetadataBlob = strings.TrimSpace(string(metadataOID))

	// The raw index blob was already written while state was captured. Creating
	// all refs in one ref transaction means a partial checkpoint is never
	// published. "create" prevents replacement of an existing immutable ref.
	var transaction strings.Builder
	transaction.WriteString("start\n")
	transaction.WriteString("create " + checkpoint.Refs.Worktree + " " + checkpoint.WorktreeTree + "\n")
	transaction.WriteString("create " + checkpoint.Refs.IndexTree + " " + checkpoint.IndexTree + "\n")
	transaction.WriteString("create " + checkpoint.Refs.Index + " " + checkpoint.IndexBlob + "\n")
	transaction.WriteString("create " + checkpoint.Refs.Metadata + " " + checkpoint.MetadataBlob + "\n")
	if checkpoint.Refs.Head != "" {
		transaction.WriteString("create " + checkpoint.Refs.Head + " " + checkpoint.Head + "\n")
	}
	transaction.WriteString("prepare\ncommit\n")
	if _, err = engine.gitInput(ctx, repo, []byte(transaction.String()), nil, "update-ref", "--stdin"); err != nil {
		return GitCheckpoint{}, fmt.Errorf("protect Git checkpoint objects: %w", err)
	}
	return checkpoint, nil
}

func (engine *GitCheckpointEngine) Inspect(ctx context.Context, workspace string) (GitWorkspaceState, error) {
	repo, err := engine.openRepository(ctx, workspace)
	if err != nil {
		return GitWorkspaceState{}, err
	}
	state, err := engine.captureState(ctx, repo, false)
	return state, err
}

func (engine *GitCheckpointEngine) captureState(ctx context.Context, repo gitRepository, writeIndexBlob bool) (GitWorkspaceState, error) {
	indexBytes, indexPresent, err := readOptionalFile(repo.indexPath)
	if err != nil {
		return GitWorkspaceState{}, fmt.Errorf("read user Git index: %w", err)
	}
	status, err := engine.readStatus(ctx, repo)
	if err != nil {
		return GitWorkspaceState{}, err
	}
	headRef, branch, head, detached, unborn, err := engine.readHead(ctx, repo)
	if err != nil {
		return GitWorkspaceState{}, err
	}
	hashArgs := []string{"hash-object", "--stdin"}
	if writeIndexBlob {
		hashArgs = []string{"hash-object", "-w", "--stdin"}
	}
	output, hashErr := engine.gitInput(ctx, repo, indexBytes, nil, hashArgs...)
	if hashErr != nil {
		return GitWorkspaceState{}, fmt.Errorf("hash exact Git index object: %w", hashErr)
	}
	indexBlob := strings.TrimSpace(string(output))

	indexTree, err := engine.treeFromIndex(ctx, repo, indexBytes, indexPresent, false)
	if err != nil {
		return GitWorkspaceState{}, fmt.Errorf("snapshot Git index: %w", err)
	}
	worktreeTree, err := engine.treeFromIndex(ctx, repo, indexBytes, indexPresent, true)
	if err != nil {
		return GitWorkspaceState{}, fmt.Errorf("snapshot Git work tree: %w", err)
	}
	state := GitWorkspaceState{
		WorkspaceRoot: repo.root, GitDirectory: repo.gitDir, CommonDirectory: repo.commonDir,
		RepositoryIdentity: repo.identity, HeadRef: headRef, Branch: branch, Head: head,
		Detached: detached, Unborn: unborn, IndexPresent: indexPresent,
		IndexTree: indexTree, IndexBlob: indexBlob, WorktreeTree: worktreeTree,
		Status: status,
	}
	state.Fingerprint = gitStateFingerprint(state)
	return state, nil
}

func (engine *GitCheckpointEngine) treeFromIndex(ctx context.Context, repo gitRepository, indexBytes []byte, indexPresent, includeWorktree bool) (string, error) {
	temporary, err := os.CreateTemp("", "specrelay-git-index-*")
	if err != nil {
		return "", err
	}
	temporaryPath := temporary.Name()
	if closeErr := temporary.Close(); closeErr != nil {
		_ = os.Remove(temporaryPath)
		return "", closeErr
	}
	defer os.Remove(temporaryPath)
	if indexPresent {
		if err = os.WriteFile(temporaryPath, indexBytes, 0o600); err != nil {
			return "", err
		}
	} else {
		_ = os.Remove(temporaryPath)
		if _, err = engine.git(ctx, repo, []string{"GIT_INDEX_FILE=" + temporaryPath}, "read-tree", "--empty"); err != nil {
			return "", err
		}
	}
	if includeWorktree {
		// -A updates tracked files and deletions and adds every non-ignored
		// untracked file. The temporary index isolates all index mutations.
		if _, err = engine.git(ctx, repo, []string{"GIT_INDEX_FILE=" + temporaryPath}, "add", "-A", "--", "."); err != nil {
			return "", err
		}
	}
	output, err := engine.git(ctx, repo, []string{"GIT_INDEX_FILE=" + temporaryPath}, "write-tree")
	if err != nil {
		return "", err
	}
	tree := strings.TrimSpace(string(output))
	if !isGitObjectID(tree) {
		return "", fmt.Errorf("Git returned invalid tree object %q", tree)
	}
	listed, listErr := engine.git(ctx, repo, nil, "ls-tree", tree)
	if listErr != nil {
		return "", fmt.Errorf("verify Git tree %s: %w", tree, listErr)
	}
	if len(listed) == 0 {
		// Git treats the canonical empty tree as a virtual object: cat-file can
		// resolve it even when no loose/packed object exists. Store it explicitly
		// before a protection ref is created so fsck and GC remain satisfied.
		stored, storeErr := engine.gitInput(ctx, repo, []byte{}, nil, "mktree")
		if storeErr != nil || strings.TrimSpace(string(stored)) != tree {
			if storeErr == nil {
				storeErr = fmt.Errorf("mktree returned %q", strings.TrimSpace(string(stored)))
			}
			return "", fmt.Errorf("materialize Git tree %s: %w", tree, storeErr)
		}
	}
	return tree, nil
}

func (engine *GitCheckpointEngine) openRepository(ctx context.Context, configured string) (gitRepository, error) {
	identity, err := security.InspectExistingPath(configured)
	if err != nil {
		return gitRepository{}, fmt.Errorf("inspect project workspace before starting Git: %w", err)
	}
	info, err := os.Stat(identity.Real)
	if err != nil {
		return gitRepository{}, fmt.Errorf("inspect project workspace before starting Git: %w", err)
	}
	if !info.IsDir() {
		return gitRepository{}, errors.New("project workspace must be a directory")
	}
	if err = engine.validateAllowedRoot(identity.Real); err != nil {
		return gitRepository{}, err
	}
	// Reject obvious non-repositories without starting Git. A .git directory,
	// file, or a bare-repository HEAD/object layout is required first.
	gitMarker := filepath.Join(identity.Real, ".git")
	if _, markerErr := os.Lstat(gitMarker); markerErr != nil {
		if !errors.Is(markerErr, os.ErrNotExist) || !looksLikeBareRepository(identity.Real) {
			return gitRepository{}, fmt.Errorf("%w: %s has no .git metadata", ErrNotGitWorkspace, identity.Real)
		}
	}

	repo := gitRepository{root: identity.Real}
	inside, err := engine.git(ctx, repo, nil, "rev-parse", "--is-inside-work-tree")
	if err != nil || strings.TrimSpace(string(inside)) != "true" {
		if err == nil {
			err = errors.New("Git did not identify a work tree")
		}
		return gitRepository{}, fmt.Errorf("%w: %v", ErrNotGitWorkspace, err)
	}
	top, err := engine.git(ctx, repo, nil, "rev-parse", "--show-toplevel")
	if err != nil {
		return gitRepository{}, fmt.Errorf("%w: resolve Git work tree: %v", ErrGitRepository, err)
	}
	topIdentity, err := security.InspectExistingPath(strings.TrimSpace(string(top)))
	if err != nil {
		return gitRepository{}, fmt.Errorf("%w: normalize Git work tree: %v", ErrGitRepository, err)
	}
	if topIdentity.Real != identity.Real {
		return gitRepository{}, fmt.Errorf("%w: configured project %s resolves to repository root %s", ErrUnsafeGitWorkspace, identity.Real, topIdentity.Real)
	}
	if err = engine.validateAllowedRoot(topIdentity.Real); err != nil {
		return gitRepository{}, err
	}

	gitDir, err := engine.git(ctx, repo, nil, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return gitRepository{}, fmt.Errorf("%w: resolve Git directory: %v", ErrGitRepository, err)
	}
	gitDirIdentity, err := security.InspectExistingPath(strings.TrimSpace(string(gitDir)))
	if err != nil {
		return gitRepository{}, fmt.Errorf("%w: normalize Git directory: %v", ErrGitRepository, err)
	}
	repo.gitDir = gitDirIdentity.Real
	commonDir, err := engine.git(ctx, repo, nil, "rev-parse", "--git-common-dir")
	if err != nil {
		return gitRepository{}, fmt.Errorf("%w: resolve Git common directory: %v", ErrGitRepository, err)
	}
	commonPath := strings.TrimSpace(string(commonDir))
	if !filepath.IsAbs(commonPath) {
		commonPath = filepath.Join(repo.root, commonPath)
	}
	commonIdentity, err := security.InspectExistingPath(commonPath)
	if err != nil {
		return gitRepository{}, fmt.Errorf("%w: normalize Git common directory: %v", ErrGitRepository, err)
	}
	repo.commonDir = commonIdentity.Real
	indexOutput, err := engine.git(ctx, repo, nil, "rev-parse", "--git-path", "index")
	if err != nil {
		return gitRepository{}, fmt.Errorf("%w: resolve Git index: %v", ErrGitRepository, err)
	}
	indexPath := strings.TrimSpace(string(indexOutput))
	if !filepath.IsAbs(indexPath) {
		indexPath = filepath.Join(repo.root, indexPath)
	}
	indexParent, err := security.InspectExistingPath(filepath.Dir(indexPath))
	if err != nil {
		return gitRepository{}, fmt.Errorf("%w: normalize Git index parent: %v", ErrGitRepository, err)
	}
	repo.indexPath = filepath.Join(indexParent.Real, filepath.Base(indexPath))
	origin, originErr := engine.git(ctx, repo, nil, "config", "--get", "remote.origin.url")
	if originErr == nil && strings.TrimSpace(string(origin)) != "" {
		repo.identity = strings.TrimSpace(string(origin))
	} else {
		repo.identity = repo.commonDir
	}

	// Verify refs, objects reachable from refs, index readability, and work-tree
	// parsing before any checkpoint command is allowed to write an object/ref.
	if _, err = engine.git(ctx, repo, nil, "fsck", "--connectivity-only", "--no-dangling"); err != nil {
		return gitRepository{}, fmt.Errorf("%w: Git connectivity check failed: %v", ErrGitRepository, err)
	}
	if _, err = engine.git(ctx, repo, nil, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--ignore-submodules=none"); err != nil {
		return gitRepository{}, fmt.Errorf("%w: Git index or work tree cannot be parsed: %v", ErrGitRepository, err)
	}
	return repo, nil
}

func (engine *GitCheckpointEngine) validateAllowedRoot(root string) error {
	if len(engine.AllowedRoots) == 0 {
		return nil
	}
	for _, allowed := range engine.AllowedRoots {
		inside, err := security.PathWithinRoot(allowed, root)
		if err == nil && inside {
			return nil
		}
	}
	return fmt.Errorf("%w: %s is outside configured roots", ErrUnsafeGitWorkspace, root)
}

func looksLikeBareRepository(path string) bool {
	for _, name := range []string{"HEAD", "objects", "refs"} {
		if _, err := os.Lstat(filepath.Join(path, name)); err != nil {
			return false
		}
	}
	return true
}

func (engine *GitCheckpointEngine) readHead(ctx context.Context, repo gitRepository) (headRef, branch, head string, detached, unborn bool, err error) {
	refOutput, refErr := engine.git(ctx, repo, nil, "symbolic-ref", "-q", "HEAD")
	if refErr == nil {
		headRef = strings.TrimSpace(string(refOutput))
		branch = strings.TrimPrefix(headRef, "refs/heads/")
	}
	headOutput, headErr := engine.git(ctx, repo, nil, "rev-parse", "--verify", "HEAD")
	if headErr == nil {
		head = strings.TrimSpace(string(headOutput))
		if !isGitObjectID(head) {
			return "", "", "", false, false, fmt.Errorf("%w: invalid HEAD object %q", ErrGitRepository, head)
		}
	} else if headRef != "" {
		unborn = true
	} else {
		return "", "", "", false, false, fmt.Errorf("%w: HEAD is neither symbolic, detached, nor unborn: %v", ErrGitRepository, headErr)
	}
	detached = headRef == "" && head != ""
	return
}

func (engine *GitCheckpointEngine) readStatus(ctx context.Context, repo gitRepository) (GitStatusSummary, error) {
	raw, err := engine.git(ctx, repo, nil, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--ignore-submodules=none")
	if err != nil {
		return GitStatusSummary{}, fmt.Errorf("read Git status: %w", err)
	}
	records, err := parsePorcelainV1Z(raw)
	if err != nil {
		return GitStatusSummary{}, fmt.Errorf("parse Git status: %w", err)
	}
	indexRaw, err := engine.git(ctx, repo, nil, "ls-files", "--stage", "-z")
	if err != nil {
		return GitStatusSummary{}, fmt.Errorf("read Git index entries: %w", err)
	}
	submoduleSet := map[string]struct{}{}
	for _, part := range bytes.Split(indexRaw, []byte{0}) {
		if len(part) == 0 {
			continue
		}
		tab := bytes.IndexByte(part, '\t')
		if tab < 0 {
			return GitStatusSummary{}, fmt.Errorf("invalid Git index entry %q", part)
		}
		fields := strings.Fields(string(part[:tab]))
		path := string(part[tab+1:])
		if len(fields) >= 1 && fields[0] == "160000" {
			if !safeGitRelativePath(path) {
				return GitStatusSummary{}, fmt.Errorf("unsafe submodule path %q", path)
			}
			submoduleSet[path] = struct{}{}
		}
	}
	summary := GitStatusSummary{}
	for path := range submoduleSet {
		summary.Submodules = append(summary.Submodules, path)
	}
	for _, record := range records {
		status := record.Status
		if status == "??" {
			summary.Untracked = append(summary.Untracked, record.Path)
			continue
		}
		if status == "!!" {
			continue
		}
		if status[0] != ' ' && status[0] != '?' {
			summary.Staged = append(summary.Staged, record.Path)
		}
		if status[1] != ' ' && status[1] != '?' {
			summary.Unstaged = append(summary.Unstaged, record.Path)
		}
		if status[0] == 'D' || status[1] == 'D' {
			summary.Deleted = append(summary.Deleted, record.Path)
		}
		if status[0] == 'R' || status[1] == 'R' {
			summary.Renamed = append(summary.Renamed, record.OriginalPath+" -> "+record.Path)
		}
		if isConflictStatus(status) {
			summary.Conflicted = append(summary.Conflicted, record.Path)
		}
		if _, ok := submoduleSet[record.Path]; ok {
			summary.DirtySubmodules = append(summary.DirtySubmodules, record.Path)
		}
	}
	for _, values := range [][]string{summary.Staged, summary.Unstaged, summary.Untracked, summary.Deleted, summary.Renamed, summary.Submodules, summary.DirtySubmodules, summary.Conflicted} {
		sort.Strings(values)
	}
	summary.Dirty = len(records) > 0
	summary.PreExistingChanges = summary.Dirty
	return summary, nil
}

func (engine *GitCheckpointEngine) Compare(ctx context.Context, before, after GitCheckpoint) (GitTreeDiff, error) {
	if before.WorkspaceRoot == "" || before.WorkspaceRoot != after.WorkspaceRoot || before.RepositoryIdentity != after.RepositoryIdentity {
		return GitTreeDiff{}, ErrCheckpointMismatch
	}
	repo, err := engine.openRepository(ctx, before.WorkspaceRoot)
	if err != nil {
		return GitTreeDiff{}, err
	}
	if err = engine.verifyCheckpoint(ctx, repo, before); err != nil {
		return GitTreeDiff{}, err
	}
	if err = engine.verifyCheckpoint(ctx, repo, after); err != nil {
		return GitTreeDiff{}, err
	}
	return engine.compareTrees(ctx, repo, before.WorktreeTree, after.WorktreeTree)
}

func (engine *GitCheckpointEngine) compareTrees(ctx context.Context, repo gitRepository, beforeTree, afterTree string) (GitTreeDiff, error) {
	if !isGitObjectID(beforeTree) || !isGitObjectID(afterTree) {
		return GitTreeDiff{}, errors.New("checkpoint contains an invalid Git tree object")
	}
	oldEntries, err := engine.listTree(ctx, repo, beforeTree)
	if err != nil {
		return GitTreeDiff{}, fmt.Errorf("read before checkpoint tree: %w", err)
	}
	newEntries, err := engine.listTree(ctx, repo, afterTree)
	if err != nil {
		return GitTreeDiff{}, fmt.Errorf("read after checkpoint tree: %w", err)
	}
	raw, err := engine.git(ctx, repo, nil, "diff-tree", "--no-commit-id", "-r", "-M", "-C", "--name-status", "-z", "--no-ext-diff", beforeTree, afterTree)
	if err != nil {
		return GitTreeDiff{}, fmt.Errorf("compare checkpoint trees: %w", err)
	}
	changes, err := parseNameStatusZ(raw)
	if err != nil {
		return GitTreeDiff{}, err
	}
	result := GitTreeDiff{BeforeTree: beforeTree, AfterTree: afterTree}
	for _, change := range changes {
		file := GitFileDiff{
			Path: change.path, PreviousPath: change.previousPath,
			Status: change.status, Similarity: change.similarity,
		}
		if old, ok := oldEntries[change.oldLookupPath()]; ok {
			file.OldMode, file.OldObject = old.Mode, old.OID
		}
		if next, ok := newEntries[change.path]; ok {
			file.NewMode, file.NewObject = next.Mode, next.OID
		}
		file.Additions, file.Deletions, file.Binary = engine.fileNumstat(ctx, repo, beforeTree, afterTree, file)
		file.Unreadable, file.Oversized = engine.inspectDiffObjects(ctx, repo, file)
		result.Additions += file.Additions
		result.Deletions += file.Deletions
		result.Files = append(result.Files, file)
	}
	return result, nil
}

type nameStatusChange struct {
	status       GitChangeStatus
	path         string
	previousPath string
	similarity   int
}

func (change nameStatusChange) oldLookupPath() string {
	if change.previousPath != "" {
		return change.previousPath
	}
	return change.path
}

func parseNameStatusZ(raw []byte) ([]nameStatusChange, error) {
	parts := bytes.Split(raw, []byte{0})
	changes := make([]nameStatusChange, 0)
	for index := 0; index < len(parts); {
		if len(parts[index]) == 0 {
			index++
			continue
		}
		code := string(parts[index])
		index++
		if index >= len(parts) || len(parts[index]) == 0 {
			return nil, fmt.Errorf("missing path after Git diff status %q", code)
		}
		change := nameStatusChange{}
		switch code[0] {
		case 'A':
			change.status = GitChangeAdded
		case 'M':
			change.status = GitChangeModified
		case 'D':
			change.status = GitChangeDeleted
		case 'T':
			change.status = GitChangeType
		case 'R', 'C':
			if code[0] == 'R' {
				change.status = GitChangeRenamed
			} else {
				change.status = GitChangeCopied
			}
			if len(code) > 1 {
				change.similarity, _ = strconv.Atoi(code[1:])
			}
			change.previousPath = string(parts[index])
			index++
			if index >= len(parts) || len(parts[index]) == 0 {
				return nil, fmt.Errorf("missing destination path after Git diff status %q", code)
			}
			change.path = string(parts[index])
			index++
		default:
			change.status = GitChangeUnknown
		}
		if change.path == "" {
			change.path = string(parts[index])
			index++
		}
		if !safeGitRelativePath(change.path) || (change.previousPath != "" && !safeGitRelativePath(change.previousPath)) {
			return nil, fmt.Errorf("unsafe path returned by Git tree diff: %q", change.path)
		}
		changes = append(changes, change)
	}
	return changes, nil
}

func (engine *GitCheckpointEngine) fileNumstat(ctx context.Context, repo gitRepository, beforeTree, afterTree string, file GitFileDiff) (additions, deletions int64, binary bool) {
	paths := []string{file.Path}
	if file.PreviousPath != "" && file.PreviousPath != file.Path {
		paths = append(paths, file.PreviousPath)
	}
	args := []string{"diff", "--numstat", "--no-renames", "--no-ext-diff", "--no-textconv", beforeTree, afterTree, "--"}
	args = append(args, paths...)
	output, err := engine.git(ctx, repo, nil, args...)
	if err != nil {
		return 0, 0, false
	}
	for _, line := range bytes.Split(bytes.TrimSpace(output), []byte{'\n'}) {
		fields := bytes.SplitN(line, []byte{'\t'}, 3)
		if len(fields) < 2 {
			continue
		}
		if string(fields[0]) == "-" || string(fields[1]) == "-" {
			binary = true
			continue
		}
		added, addErr := strconv.ParseInt(string(fields[0]), 10, 64)
		removed, deleteErr := strconv.ParseInt(string(fields[1]), 10, 64)
		if addErr == nil {
			additions += added
		}
		if deleteErr == nil {
			deletions += removed
		}
	}
	return
}

func (engine *GitCheckpointEngine) inspectDiffObjects(ctx context.Context, repo gitRepository, file GitFileDiff) (unreadable, oversized bool) {
	limit := engine.maxDiffFileSize()
	for _, object := range []struct {
		oid  string
		mode string
	}{{file.OldObject, file.OldMode}, {file.NewObject, file.NewMode}} {
		if object.oid == "" || object.mode == "160000" {
			continue
		}
		output, err := engine.git(ctx, repo, nil, "cat-file", "-s", object.oid)
		if err != nil {
			unreadable = true
			continue
		}
		size, err := strconv.ParseInt(strings.TrimSpace(string(output)), 10, 64)
		if err != nil {
			unreadable = true
			continue
		}
		if size > limit {
			oversized = true
		}
	}
	return
}

func (engine *GitCheckpointEngine) UnifiedDiff(ctx context.Context, before, after GitCheckpoint, file GitFileDiff, options UnifiedDiffOptions) (UnifiedFileDiff, error) {
	result := UnifiedFileDiff{
		Path: file.Path, PreviousPath: file.PreviousPath, Binary: file.Binary,
		Oversized: file.Oversized, Unreadable: file.Unreadable,
	}
	if file.Binary || file.Oversized || file.Unreadable {
		switch {
		case file.Binary:
			result.OmittedReason = "binary file"
		case file.Oversized:
			result.OmittedReason = "file exceeds configured diff size limit"
		default:
			result.OmittedReason = "Git object cannot be read"
		}
		return result, ErrDiffUnavailable
	}
	if before.WorkspaceRoot != after.WorkspaceRoot || before.RepositoryIdentity != after.RepositoryIdentity {
		return result, ErrCheckpointMismatch
	}
	if !safeGitRelativePath(file.Path) || (file.PreviousPath != "" && !safeGitRelativePath(file.PreviousPath)) {
		return result, errors.New("diff path is unsafe")
	}
	repo, err := engine.openRepository(ctx, before.WorkspaceRoot)
	if err != nil {
		return result, err
	}
	if err = engine.verifyCheckpoint(ctx, repo, before); err != nil {
		return result, err
	}
	if err = engine.verifyCheckpoint(ctx, repo, after); err != nil {
		return result, err
	}
	maxBytes, maxLines, contextLines := engine.diffLimits(options)
	paths := []string{file.Path}
	if file.PreviousPath != "" && file.PreviousPath != file.Path {
		paths = append(paths, file.PreviousPath)
	}
	args := []string{"diff", "--no-ext-diff", "--no-textconv", "--find-renames", "--unified=" + strconv.Itoa(contextLines), before.WorktreeTree, after.WorktreeTree, "--"}
	args = append(args, paths...)
	output, truncated, err := engine.gitLimited(ctx, repo, maxBytes, maxLines, args...)
	if err != nil {
		return result, fmt.Errorf("generate bounded unified diff for %q: %w", file.Path, err)
	}
	result.Patch = string(output)
	result.Truncated = truncated
	result.Bytes = int64(len(output))
	result.Lines = bytes.Count(output, []byte{'\n'})
	if truncated {
		result.OmittedReason = "diff truncated at configured byte or line limit"
	}
	return result, nil
}

func (engine *GitCheckpointEngine) diffLimits(options UnifiedDiffOptions) (int64, int, int) {
	maxBytes := options.MaxBytes
	if maxBytes <= 0 || maxBytes > engine.maxDiffBytes() {
		maxBytes = engine.maxDiffBytes()
	}
	maxLines := options.MaxLines
	if maxLines <= 0 || maxLines > engine.maxDiffLines() {
		maxLines = engine.maxDiffLines()
	}
	contextLines := options.ContextLines
	if contextLines < 0 {
		contextLines = 0
	}
	if contextLines > engine.maxDiffContext() {
		contextLines = engine.maxDiffContext()
	}
	return maxBytes, maxLines, contextLines
}

func (engine *GitCheckpointEngine) Restore(ctx context.Context, checkpoint GitCheckpoint, options RestoreGitCheckpointOptions) error {
	repo, err := engine.openRepository(ctx, checkpoint.WorkspaceRoot)
	if err != nil {
		return err
	}
	if err = engine.verifyCheckpoint(ctx, repo, checkpoint); err != nil {
		return err
	}
	current, err := engine.captureState(ctx, repo, false)
	if err != nil {
		return fmt.Errorf("capture current state before restore: %w", err)
	}
	if expected := strings.TrimSpace(options.ExpectedCurrentFingerprint); expected != "" && current.Fingerprint != expected {
		return fmt.Errorf("%w: expected %s, found %s", ErrWorkspaceChanged, expected, current.Fingerprint)
	}
	targetEntries, err := engine.listTree(ctx, repo, checkpoint.WorktreeTree)
	if err != nil {
		return fmt.Errorf("read restore tree: %w", err)
	}
	currentEntries, err := engine.listTree(ctx, repo, current.WorktreeTree)
	if err != nil {
		return fmt.Errorf("read current controlled tree: %w", err)
	}
	indexBytes, err := engine.readBlob(ctx, repo, checkpoint.IndexBlob, -1)
	if err != nil {
		return fmt.Errorf("read checkpoint index: %w", err)
	}
	if err = engine.preflightRestore(ctx, repo, targetEntries, currentEntries); err != nil {
		return err
	}
	if err = engine.applyWorktreeRestore(ctx, repo, targetEntries, currentEntries); err != nil {
		return err
	}
	if err = restoreIndexAtomically(repo.indexPath, indexBytes, checkpoint.IndexPresent); err != nil {
		return fmt.Errorf("restore exact Git index: %w", err)
	}
	if err = engine.restoreHead(ctx, repo, checkpoint); err != nil {
		return err
	}
	verified, err := engine.captureState(ctx, repo, false)
	if err != nil {
		return fmt.Errorf("verify restored Git state: %w", err)
	}
	if verified.Fingerprint != checkpoint.Fingerprint {
		return fmt.Errorf("restored Git state fingerprint mismatch: got %s, want %s", verified.Fingerprint, checkpoint.Fingerprint)
	}
	return nil
}

func (engine *GitCheckpointEngine) verifyCheckpoint(ctx context.Context, repo gitRepository, checkpoint GitCheckpoint) error {
	if checkpoint.ID == "" || checkpoint.WorkspaceRoot != repo.root || checkpoint.RepositoryIdentity != repo.identity {
		return ErrCheckpointMismatch
	}
	if checkpoint.Refs.Worktree == "" || checkpoint.Refs.IndexTree == "" || checkpoint.Refs.Index == "" || checkpoint.Refs.Metadata == "" {
		return errors.New("checkpoint protection refs are incomplete")
	}
	checks := []struct {
		ref string
		oid string
		typ string
	}{
		{checkpoint.Refs.Worktree, checkpoint.WorktreeTree, "tree"},
		{checkpoint.Refs.IndexTree, checkpoint.IndexTree, "tree"},
		{checkpoint.Refs.Index, checkpoint.IndexBlob, "blob"},
		{checkpoint.Refs.Metadata, checkpoint.MetadataBlob, "blob"},
	}
	if checkpoint.Head != "" {
		if checkpoint.Refs.Head == "" {
			return errors.New("checkpoint HEAD protection ref is missing")
		}
		checks = append(checks, struct {
			ref string
			oid string
			typ string
		}{checkpoint.Refs.Head, checkpoint.Head, "commit"})
	}
	for _, check := range checks {
		if !strings.HasPrefix(check.ref, "refs/specrelay/checkpoints/"+checkpoint.ID+"/") {
			return fmt.Errorf("checkpoint ref %q is outside the controlled namespace", check.ref)
		}
		output, err := engine.git(ctx, repo, nil, "rev-parse", "--verify", check.ref)
		if err != nil || strings.TrimSpace(string(output)) != check.oid {
			return fmt.Errorf("checkpoint object protection ref %s is missing or changed", check.ref)
		}
		typeOutput, err := engine.git(ctx, repo, nil, "cat-file", "-t", check.oid)
		if err != nil || strings.TrimSpace(string(typeOutput)) != check.typ {
			return fmt.Errorf("checkpoint object %s is unreadable or has the wrong type", check.oid)
		}
	}
	return nil
}

func (engine *GitCheckpointEngine) preflightRestore(ctx context.Context, repo gitRepository, target, current map[string]gitTreeEntry) error {
	extra := make(map[string]gitTreeEntry)
	for path, entry := range current {
		if _, exists := target[path]; !exists {
			extra[path] = entry
		}
	}
	for path, entry := range target {
		if !safeGitRelativePath(path) {
			return fmt.Errorf("unsafe checkpoint path %q", path)
		}
		if entry.Mode != "100644" && entry.Mode != "100755" && entry.Mode != "120000" && entry.Mode != "160000" {
			return fmt.Errorf("unsupported Git mode %s for %q", entry.Mode, path)
		}
		if entry.Mode != "160000" {
			if _, err := engine.git(ctx, repo, nil, "cat-file", "-e", entry.OID+"^{blob}"); err != nil {
				return fmt.Errorf("checkpoint blob for %q is unreadable: %w", path, err)
			}
		}
		absolute := filepath.Join(repo.root, filepath.FromSlash(path))
		if err := preflightRestoreAncestors(repo.root, absolute, extra); err != nil {
			return fmt.Errorf("checkpoint path %q cannot be restored safely: %w", path, err)
		}
		info, statErr := os.Lstat(absolute)
		if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
			return fmt.Errorf("inspect restore target %q: %w", path, statErr)
		}
		if entry.Mode == "160000" {
			if errors.Is(statErr, os.ErrNotExist) || (statErr == nil && !info.IsDir()) {
				return fmt.Errorf("cannot restore submodule %q without an existing submodule work tree", path)
			}
			continue
		}
		if statErr == nil && info.IsDir() {
			if err := directoryContainsOnlyControlledExtras(repo.root, absolute, extra); err != nil {
				return fmt.Errorf("cannot replace directory at %q while preserving ignored files: %w", path, err)
			}
		}
	}
	for path := range current {
		if !safeGitRelativePath(path) {
			return fmt.Errorf("unsafe current controlled path %q", path)
		}
	}
	return nil
}

func preflightRestoreAncestors(root, path string, extra map[string]gitTreeEntry) error {
	relative, err := filepath.Rel(root, filepath.Dir(path))
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
		return security.ErrPathOutsideRoots
	}
	if relative == "." {
		return nil
	}
	current := root
	for _, part := range strings.Split(relative, string(os.PathSeparator)) {
		current = filepath.Join(current, part)
		info, statErr := os.Lstat(current)
		if errors.Is(statErr, os.ErrNotExist) {
			return nil
		}
		if statErr != nil {
			return statErr
		}
		if info.IsDir() {
			continue
		}
		rel, relErr := filepath.Rel(root, current)
		if relErr != nil {
			return relErr
		}
		if _, controlled := extra[filepath.ToSlash(rel)]; controlled {
			// A controlled file/symlink-to-directory type change will remove this
			// ancestor before target directories are created.
			return nil
		}
		return errors.New("an uncontrolled file or symlink blocks a target parent directory")
	}
	return nil
}

func directoryContainsOnlyControlledExtras(root, directory string, extra map[string]gitTreeEntry) error {
	return filepath.WalkDir(directory, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == directory || entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if _, controlled := extra[filepath.ToSlash(relative)]; !controlled {
			return fmt.Errorf("uncontrolled path %s would be removed", filepath.ToSlash(relative))
		}
		return nil
	})
}

func (engine *GitCheckpointEngine) applyWorktreeRestore(ctx context.Context, repo gitRepository, target, current map[string]gitTreeEntry) error {
	extra := make([]string, 0)
	for path := range current {
		if _, exists := target[path]; !exists {
			extra = append(extra, path)
		}
	}
	sort.Slice(extra, func(i, j int) bool {
		leftDepth := strings.Count(extra[i], "/")
		rightDepth := strings.Count(extra[j], "/")
		if leftDepth != rightDepth {
			return leftDepth > rightDepth
		}
		return extra[i] > extra[j]
	})
	for _, path := range extra {
		entry := current[path]
		absolute, err := security.ResolveRelativePathForWrite(repo.root, path)
		if err != nil {
			return fmt.Errorf("resolve extra controlled path %q: %w", path, err)
		}
		info, statErr := os.Lstat(absolute)
		if errors.Is(statErr, os.ErrNotExist) {
			continue
		}
		if statErr != nil {
			return fmt.Errorf("inspect extra controlled path %q: %w", path, statErr)
		}
		if entry.Mode == "160000" || info.IsDir() {
			// Never recursively remove a directory: it may contain ignored data.
			entries, readErr := os.ReadDir(absolute)
			if readErr != nil {
				return fmt.Errorf("inspect extra controlled directory %q: %w", path, readErr)
			}
			if len(entries) != 0 {
				return fmt.Errorf("cannot remove controlled directory %q because preserving its contents is required", path)
			}
			if err = os.Remove(absolute); err != nil {
				return fmt.Errorf("remove empty controlled directory %q: %w", path, err)
			}
			continue
		}
		if err = os.Remove(absolute); err != nil {
			return fmt.Errorf("remove extra controlled path %q: %w", path, err)
		}
		removeEmptyParents(repo.root, filepath.Dir(absolute))
	}

	paths := make([]string, 0, len(target))
	for path := range target {
		paths = append(paths, path)
	}
	sort.Slice(paths, func(i, j int) bool {
		leftDepth := strings.Count(paths[i], "/")
		rightDepth := strings.Count(paths[j], "/")
		if leftDepth != rightDepth {
			return leftDepth < rightDepth
		}
		return paths[i] < paths[j]
	})
	for _, path := range paths {
		entry := target[path]
		if entry.Mode == "160000" {
			continue
		}
		absolute, err := security.ResolveRelativePathForWrite(repo.root, path)
		if err != nil {
			return fmt.Errorf("resolve restore path %q: %w", path, err)
		}
		if err = ensureSafeParentDirectories(repo.root, filepath.Dir(absolute)); err != nil {
			return fmt.Errorf("prepare restore path %q: %w", path, err)
		}
		content, err := engine.readBlob(ctx, repo, entry.OID, -1)
		if err != nil {
			return fmt.Errorf("read restore content for %q: %w", path, err)
		}
		if entry.Mode == "120000" {
			if err = replaceWithSymlink(absolute, string(content)); err != nil {
				return fmt.Errorf("restore symlink %q: %w", path, err)
			}
			continue
		}
		mode := os.FileMode(0o644)
		if entry.Mode == "100755" {
			mode = 0o755
		}
		if err = replaceWithRegularFile(absolute, content, mode); err != nil {
			return fmt.Errorf("restore file %q: %w", path, err)
		}
	}
	return nil
}

func (engine *GitCheckpointEngine) restoreHead(ctx context.Context, repo gitRepository, checkpoint GitCheckpoint) error {
	if checkpoint.HeadRef != "" {
		if !strings.HasPrefix(checkpoint.HeadRef, "refs/heads/") {
			return fmt.Errorf("checkpoint symbolic HEAD %q is not a local branch", checkpoint.HeadRef)
		}
		if checkpoint.Unborn {
			// A branch created after an unborn checkpoint must disappear again.
			_, _ = engine.git(ctx, repo, nil, "update-ref", "-d", checkpoint.HeadRef)
		} else {
			if _, err := engine.git(ctx, repo, nil, "update-ref", checkpoint.HeadRef, checkpoint.Head); err != nil {
				return fmt.Errorf("restore target branch %s: %w", checkpoint.HeadRef, err)
			}
		}
		if _, err := engine.git(ctx, repo, nil, "symbolic-ref", "HEAD", checkpoint.HeadRef); err != nil {
			return fmt.Errorf("restore symbolic HEAD: %w", err)
		}
		return nil
	}
	if !checkpoint.Detached || checkpoint.Head == "" {
		return errors.New("checkpoint has no restorable branch or detached HEAD")
	}
	if _, err := engine.git(ctx, repo, nil, "update-ref", "--no-deref", "HEAD", checkpoint.Head); err != nil {
		return fmt.Errorf("restore detached HEAD: %w", err)
	}
	return nil
}

func (engine *GitCheckpointEngine) listTree(ctx context.Context, repo gitRepository, tree string) (map[string]gitTreeEntry, error) {
	output, err := engine.git(ctx, repo, nil, "ls-tree", "-r", "-z", "--full-tree", tree)
	if err != nil {
		return nil, err
	}
	entries := make(map[string]gitTreeEntry)
	for _, record := range bytes.Split(output, []byte{0}) {
		if len(record) == 0 {
			continue
		}
		tab := bytes.IndexByte(record, '\t')
		if tab < 0 || tab == len(record)-1 {
			return nil, fmt.Errorf("invalid Git tree record %q", record)
		}
		fields := strings.Fields(string(record[:tab]))
		if len(fields) != 3 {
			return nil, fmt.Errorf("invalid Git tree metadata %q", record[:tab])
		}
		path := string(record[tab+1:])
		if !safeGitRelativePath(path) {
			return nil, fmt.Errorf("unsafe path in Git tree: %q", path)
		}
		entries[path] = gitTreeEntry{Mode: fields[0], Type: fields[1], OID: fields[2], Path: path}
	}
	return entries, nil
}

func (engine *GitCheckpointEngine) readBlob(ctx context.Context, repo gitRepository, oid string, limit int64) ([]byte, error) {
	if !isGitObjectID(oid) {
		return nil, errors.New("invalid Git blob object ID")
	}
	output, err := engine.git(ctx, repo, nil, "cat-file", "blob", oid)
	if err != nil {
		return nil, err
	}
	if limit >= 0 && int64(len(output)) > limit {
		return nil, fmt.Errorf("Git blob exceeds %d bytes", limit)
	}
	return output, nil
}

func restoreIndexAtomically(indexPath string, content []byte, present bool) error {
	lockPath := indexPath + ".lock"
	lock, err := os.OpenFile(lockPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("acquire Git index lock: %w", err)
	}
	committed := false
	defer func() {
		_ = lock.Close()
		if !committed {
			_ = os.Remove(lockPath)
		}
	}()
	if present {
		if _, err = lock.Write(content); err != nil {
			return err
		}
		if err = lock.Sync(); err != nil {
			return err
		}
		if err = lock.Close(); err != nil {
			return err
		}
		if err = os.Rename(lockPath, indexPath); err != nil {
			return err
		}
		committed = true
		return nil
	}
	if err = lock.Close(); err != nil {
		return err
	}
	if err = os.Remove(indexPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err = os.Remove(lockPath); err != nil {
		return err
	}
	committed = true
	return nil
}

func ensureSafeParentDirectories(root, parent string) error {
	rootIdentity, err := security.InspectExistingPath(root)
	if err != nil {
		return err
	}
	relative, err := filepath.Rel(rootIdentity.Real, parent)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
		return security.ErrPathOutsideRoots
	}
	current := rootIdentity.Real
	if relative == "." {
		return nil
	}
	for _, part := range strings.Split(relative, string(os.PathSeparator)) {
		current = filepath.Join(current, part)
		info, statErr := os.Lstat(current)
		if errors.Is(statErr, os.ErrNotExist) {
			if err = os.Mkdir(current, 0o755); err != nil {
				return err
			}
			continue
		}
		if statErr != nil {
			return statErr
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("restore parent is a symlink")
		}
		if !info.IsDir() {
			return errors.New("restore parent is not a directory")
		}
	}
	return nil
}

func replaceWithRegularFile(path string, content []byte, mode os.FileMode) error {
	if err := removeReplaceablePath(path); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".specrelay-restore-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	committed := false
	defer func() {
		_ = temporary.Close()
		if !committed {
			_ = os.Remove(temporaryPath)
		}
	}()
	if _, err = temporary.Write(content); err != nil {
		return err
	}
	if err = temporary.Chmod(mode); err != nil {
		return err
	}
	if err = temporary.Sync(); err != nil {
		return err
	}
	if err = temporary.Close(); err != nil {
		return err
	}
	if err = os.Rename(temporaryPath, path); err != nil {
		return err
	}
	committed = true
	return nil
}

func replaceWithSymlink(path, target string) error {
	if strings.IndexByte(target, 0) >= 0 {
		return errors.New("symlink target contains NUL")
	}
	if err := removeReplaceablePath(path); err != nil {
		return err
	}
	temporary := filepath.Join(filepath.Dir(path), ".specrelay-link-"+uuid.NewString())
	if err := os.Symlink(target, temporary); err != nil {
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	return nil
}

func removeReplaceablePath(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return os.Remove(path)
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return errors.New("refusing to replace a non-empty directory because it may contain ignored files")
	}
	return os.Remove(path)
}

func removeEmptyParents(root, start string) {
	root = filepath.Clean(root)
	current := filepath.Clean(start)
	for current != root && current != filepath.Dir(current) {
		if err := os.Remove(current); err != nil {
			return
		}
		current = filepath.Dir(current)
	}
}

func readOptionalFile(path string) ([]byte, bool, error) {
	content, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return content, true, nil
}

func gitStateFingerprint(state GitWorkspaceState) string {
	value := struct {
		WorkspaceRoot      string           `json:"workspaceRoot"`
		RepositoryIdentity string           `json:"repositoryIdentity"`
		HeadRef            string           `json:"headRef"`
		Head               string           `json:"head"`
		Detached           bool             `json:"detached"`
		Unborn             bool             `json:"unborn"`
		IndexPresent       bool             `json:"indexPresent"`
		IndexTree          string           `json:"indexTree"`
		IndexBlob          string           `json:"indexBlob"`
		WorktreeTree       string           `json:"worktreeTree"`
		Status             GitStatusSummary `json:"status"`
	}{
		WorkspaceRoot: state.WorkspaceRoot, RepositoryIdentity: state.RepositoryIdentity,
		HeadRef: state.HeadRef, Head: state.Head, Detached: state.Detached, Unborn: state.Unborn,
		IndexPresent: state.IndexPresent, IndexTree: state.IndexTree, IndexBlob: state.IndexBlob,
		WorktreeTree: state.WorktreeTree, Status: state.Status,
	}
	raw, _ := json.Marshal(value)
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}

func isGitObjectID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func (engine *GitCheckpointEngine) git(ctx context.Context, repo gitRepository, extraEnv []string, args ...string) ([]byte, error) {
	return engine.gitInput(ctx, repo, nil, extraEnv, args...)
}

func (engine *GitCheckpointEngine) gitInput(ctx context.Context, repo gitRepository, input []byte, extraEnv []string, args ...string) ([]byte, error) {
	binary := strings.TrimSpace(engine.GitBinary)
	if binary == "" {
		binary = "git"
	}
	timeout := engine.Timeout
	if timeout <= 0 {
		timeout = defaultGitOperationTimeout
	}
	commandContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	commandArgs := []string{
		"--literal-pathspecs",
		"-c", "core.quotePath=false",
		"-c", "diff.external=",
		"-c", "core.hooksPath=/dev/null",
		"-C", repo.root,
	}
	commandArgs = append(commandArgs, args...)
	command := exec.CommandContext(commandContext, binary, commandArgs...)
	command.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "GIT_EXTERNAL_DIFF=")
	command.Env = append(command.Env, extraEnv...)
	if input != nil {
		command.Stdin = bytes.NewReader(input)
	}
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		if commandContext.Err() != nil {
			return nil, commandContext.Err()
		}
		message := strings.TrimSpace(stderr.String())
		if stdoutMessage := strings.TrimSpace(stdout.String()); stdoutMessage != "" {
			if message != "" {
				message += "\n"
			}
			message += stdoutMessage
		}
		if message == "" {
			message = err.Error()
		}
		return nil, errors.New(message)
	}
	return stdout.Bytes(), nil
}

func (engine *GitCheckpointEngine) gitLimited(ctx context.Context, repo gitRepository, maxBytes int64, maxLines int, args ...string) ([]byte, bool, error) {
	binary := strings.TrimSpace(engine.GitBinary)
	if binary == "" {
		binary = "git"
	}
	timeout := engine.Timeout
	if timeout <= 0 {
		timeout = defaultGitOperationTimeout
	}
	commandContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	commandArgs := []string{
		"--literal-pathspecs",
		"-c", "core.quotePath=false",
		"-c", "diff.external=",
		"-c", "core.hooksPath=/dev/null",
		"-C", repo.root,
	}
	commandArgs = append(commandArgs, args...)
	command := exec.CommandContext(commandContext, binary, commandArgs...)
	command.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "GIT_EXTERNAL_DIFF=")
	writer := &boundedGitWriter{maxBytes: maxBytes, maxLines: maxLines}
	var stderr bytes.Buffer
	command.Stdout = writer
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		if commandContext.Err() != nil {
			return nil, writer.truncated, commandContext.Err()
		}
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		return nil, writer.truncated, errors.New(message)
	}
	return writer.Bytes(), writer.truncated, nil
}

type boundedGitWriter struct {
	mu        sync.Mutex
	buffer    bytes.Buffer
	maxBytes  int64
	maxLines  int
	lines     int
	truncated bool
}

func (writer *boundedGitWriter) Write(value []byte) (int, error) {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	originalLength := len(value)
	for _, character := range value {
		if int64(writer.buffer.Len()) >= writer.maxBytes || (writer.maxLines > 0 && writer.lines >= writer.maxLines) {
			writer.truncated = true
			continue
		}
		_ = writer.buffer.WriteByte(character)
		if character == '\n' {
			writer.lines++
		}
	}
	return originalLength, nil
}

func (writer *boundedGitWriter) Bytes() []byte {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	return append([]byte(nil), writer.buffer.Bytes()...)
}

func (engine *GitCheckpointEngine) maxDiffBytes() int64 {
	if engine.MaxDiffBytes <= 0 {
		return defaultMaxDiffBytes
	}
	return engine.MaxDiffBytes
}

func (engine *GitCheckpointEngine) maxDiffLines() int {
	if engine.MaxDiffLines <= 0 {
		return defaultMaxDiffLines
	}
	return engine.MaxDiffLines
}

func (engine *GitCheckpointEngine) maxDiffContext() int {
	if engine.MaxDiffContext <= 0 {
		return defaultMaxDiffContext
	}
	return engine.MaxDiffContext
}

func (engine *GitCheckpointEngine) maxDiffFileSize() int64 {
	if engine.MaxDiffFileSize <= 0 {
		return defaultMaxDiffFileBytes
	}
	return engine.MaxDiffFileSize
}

// Compile-time assertion that the bounded writer obeys io.Writer. Keeping this
// explicit also documents that command output is bounded at the writer edge.
var _ io.Writer = (*boundedGitWriter)(nil)

// Workspace-oriented aliases keep lifecycle call sites readable while the Git
// prefix makes the underlying persistence mechanism explicit at API edges.
type WorkspaceCheckpointEngine = GitCheckpointEngine
type WorkspaceCheckpoint = GitCheckpoint
type WorkspaceCheckpointDiff = GitTreeDiff
type WorkspaceCheckpointFileDiff = GitFileDiff
type WorkspaceRestoreOptions = RestoreGitCheckpointOptions

func NewWorkspaceCheckpointEngine(allowedRoots ...string) (*GitCheckpointEngine, error) {
	return NewGitCheckpointEngine(allowedRoots...)
}

func (engine *GitCheckpointEngine) CaptureCheckpoint(ctx context.Context, workspace string) (GitCheckpoint, error) {
	return engine.CreateCheckpoint(ctx, workspace)
}

func (engine *GitCheckpointEngine) DiffCheckpoints(ctx context.Context, before, after GitCheckpoint) (GitTreeDiff, error) {
	return engine.Compare(ctx, before, after)
}

func (engine *GitCheckpointEngine) GenerateUnifiedDiff(ctx context.Context, before, after GitCheckpoint, file GitFileDiff, options UnifiedDiffOptions) (UnifiedFileDiff, error) {
	return engine.UnifiedDiff(ctx, before, after, file, options)
}

func (engine *GitCheckpointEngine) RestoreCheckpoint(ctx context.Context, checkpoint GitCheckpoint, options RestoreGitCheckpointOptions) error {
	return engine.Restore(ctx, checkpoint, options)
}
