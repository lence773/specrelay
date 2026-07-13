package security

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

var ErrPathOutsideRoots = errors.New("path is outside allowed roots")

type PathPolicy struct{ roots []string }

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
		if candidate == root || strings.HasPrefix(candidate, root+string(os.PathSeparator)) {
			return candidate, nil
		}
	}
	return "", ErrPathOutsideRoots
}
func normalizeExisting(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(absolute)
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
