package security

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestPathPolicy(t *testing.T) {
	root := t.TempDir()
	p, err := NewPathPolicy(root)
	if err != nil {
		t.Fatal(err)
	}
	inside, err := p.Resolve(filepath.Join(root, "a", "b"))
	if err != nil || inside == "" {
		t.Fatal(err)
	}
	_, err = p.Resolve(filepath.Join(root, "..", "secret"))
	if !errors.Is(err, ErrPathOutsideRoots) {
		t.Fatalf("got %v", err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	_, err = p.Resolve(filepath.Join(root, "link", "x"))
	if !errors.Is(err, ErrPathOutsideRoots) {
		t.Fatalf("got %v", err)
	}
}
