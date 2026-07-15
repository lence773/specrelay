package security

import (
	"errors"
	"os"
	"path"
	"path/filepath"
	"strings"
)

var ErrPathOutsideRoots = errors.New("path is outside allowed roots")
var ErrInvalidRelativePath = errors.New("path must be a safe relative path")

type PathPolicy struct{ roots []string }

// PathIdentity preserves the configured spelling of a path while also
// recording its absolute and symlink-resolved identities. Keeping both values
// is important when a project path is later compared with an execution
// snapshot: changing a symlink target must not look like an unchanged project.
type PathIdentity struct {
	Configured string `json:"configured"`
	Absolute   string `json:"absolute"`
	Real       string `json:"real"`
}

func NewPathPolicy(roots ...string) (*PathPolicy, error) {
	p := &PathPolicy{}
	for _, root := range roots {
		normalized, err := normalizeExisting(root)
		if err != nil {
			return nil, err
		}
		p.roots = append(p.roots, normalized)
	}
	return p, nil
}

func (p *PathPolicy) Resolve(path string) (string, error) {
	candidate, err := normalizeCandidate(path)
	if err != nil {
		return "", err
	}
	for _, root := range p.roots {
		if pathWithinRoot(root, candidate) {
			return candidate, nil
		}
	}
	return "", ErrPathOutsideRoots
}

// InspectExistingPath returns both the absolute configured path and its real,
// symlink-resolved identity. The target must already exist.
func InspectExistingPath(path string) (PathIdentity, error) {
	configured := strings.TrimSpace(path)
	if configured == "" {
		return PathIdentity{}, errors.New("path is required")
	}
	absolute, err := filepath.Abs(configured)
	if err != nil {
		return PathIdentity{}, err
	}
	absolute = filepath.Clean(absolute)
	real, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return PathIdentity{}, err
	}
	real, err = filepath.Abs(real)
	if err != nil {
		return PathIdentity{}, err
	}
	return PathIdentity{Configured: configured, Absolute: absolute, Real: filepath.Clean(real)}, nil
}

// NormalizeExistingPath returns the canonical real path of an existing file or
// directory.
func NormalizeExistingPath(path string) (string, error) {
	identity, err := InspectExistingPath(path)
	if err != nil {
		return "", err
	}
	return identity.Real, nil
}

// ResolveRelativePath resolves a repository-relative path beneath root. It
// rejects absolute paths, traversal, and symlink escapes in parent components.
// The final component itself is deliberately not followed so callers can safely
// inspect or hash a symlink without reading its target outside the repository.
func ResolveRelativePath(root, relative string) (string, error) {
	if relative == "" || filepath.IsAbs(relative) || filepath.VolumeName(relative) != "" {
		return "", ErrInvalidRelativePath
	}
	clean := filepath.Clean(filepath.FromSlash(relative))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
		return "", ErrInvalidRelativePath
	}
	rootReal, err := normalizeExisting(root)
	if err != nil {
		return "", err
	}
	parentReal, err := normalizeExisting(filepath.Join(rootReal, filepath.Dir(clean)))
	if err != nil {
		return "", err
	}
	if !pathWithinRoot(rootReal, parentReal) {
		return "", ErrPathOutsideRoots
	}
	candidate := filepath.Join(parentReal, filepath.Base(clean))
	if !pathWithinRoot(rootReal, candidate) {
		return "", ErrPathOutsideRoots
	}
	return filepath.Clean(candidate), nil
}

func normalizeExisting(path string) (string, error) {
	identity, err := InspectExistingPath(path)
	if err != nil {
		return "", err
	}
	return identity.Real, nil
}

func normalizeCandidate(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	current := absolute
	suffix := []string{}
	for {
		resolved, evalErr := filepath.EvalSymlinks(current)
		if evalErr == nil {
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return filepath.Clean(resolved), nil
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", evalErr
		}
		suffix = append(suffix, filepath.Base(current))
		current = parent
	}
}

func pathWithinRoot(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	if err != nil {
		return false
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(os.PathSeparator)))
}

// ResolveRelativePathForWrite resolves a repository-relative path for a future
// write. Unlike ResolveRelativePath, parent directories may be absent. Every
// existing parent is symlink-resolved and must remain beneath root; the final
// component is never followed so an existing symlink can be safely replaced.
func ResolveRelativePathForWrite(root, relative string) (string, error) {
	if err := ValidateRelativePath(relative); err != nil {
		return "", err
	}
	rootReal, err := normalizeExisting(root)
	if err != nil {
		return "", err
	}
	parts := strings.Split(filepath.Clean(filepath.FromSlash(relative)), string(os.PathSeparator))
	current := rootReal
	for index, part := range parts {
		candidate := filepath.Join(current, part)
		if index == len(parts)-1 {
			if !pathWithinRoot(rootReal, candidate) {
				return "", ErrPathOutsideRoots
			}
			return filepath.Clean(candidate), nil
		}
		info, statErr := os.Lstat(candidate)
		if errors.Is(statErr, os.ErrNotExist) {
			for _, remaining := range parts[index+1:] {
				candidate = filepath.Join(candidate, remaining)
			}
			if !pathWithinRoot(rootReal, candidate) {
				return "", ErrPathOutsideRoots
			}
			return filepath.Clean(candidate), nil
		}
		if statErr != nil {
			return "", statErr
		}
		if info.Mode()&os.ModeSymlink != 0 {
			resolved, resolveErr := filepath.EvalSymlinks(candidate)
			if resolveErr != nil {
				return "", resolveErr
			}
			resolved, resolveErr = filepath.Abs(resolved)
			if resolveErr != nil {
				return "", resolveErr
			}
			resolved = filepath.Clean(resolved)
			if !pathWithinRoot(rootReal, resolved) {
				return "", ErrPathOutsideRoots
			}
			current = resolved
			continue
		}
		if !info.IsDir() {
			return "", errors.New("path parent is not a directory")
		}
		current = candidate
	}
	return "", ErrInvalidRelativePath
}

// ValidateRelativePath rejects absolute, empty, dot, traversal, and
// platform-volume paths. Git paths use slash separators, which are normalized
// before validation.
func ValidateRelativePath(relative string) error {
	if relative == "" || strings.IndexByte(relative, 0) >= 0 || strings.Contains(relative, `\`) ||
		filepath.IsAbs(relative) || filepath.VolumeName(relative) != "" || looksLikePortableAbsolutePath(relative) {
		return ErrInvalidRelativePath
	}
	clean := path.Clean(relative)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "/") {
		return ErrInvalidRelativePath
	}
	return nil
}

// NormalizeWorkspaceRelativePath converts a Git/workspace path to its canonical
// slash-separated spelling and verifies that every existing parent component
// remains beneath root. Missing leaf components are allowed so deleted files
// can still be represented in a checkpoint. The final component is not
// followed: a symlink is checkpointed as a symlink rather than as the file it
// points to.
func NormalizeWorkspaceRelativePath(root, relative string) (string, error) {
	if err := ValidateRelativePath(relative); err != nil {
		return "", err
	}
	normalized := path.Clean(relative)
	if _, err := ResolveRelativePathForWrite(root, normalized); err != nil {
		return "", err
	}
	return normalized, nil
}

func looksLikePortableAbsolutePath(value string) bool {
	if len(value) < 2 || value[1] != ':' {
		return false
	}
	first := value[0]
	return (first >= 'A' && first <= 'Z') || (first >= 'a' && first <= 'z')
}

// PathWithinRoot reports whether candidate resolves beneath an existing root.
// Existing symlinks are followed, including in the nearest existing ancestor
// when candidate itself does not yet exist.
func PathWithinRoot(root, candidate string) (bool, error) {
	rootReal, err := normalizeExisting(root)
	if err != nil {
		return false, err
	}
	candidateReal, err := normalizeCandidate(candidate)
	if err != nil {
		return false, err
	}
	return pathWithinRoot(rootReal, candidateReal), nil
}
