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

func TestInspectExistingPathPreservesConfiguredAndRealPaths(t *testing.T) {
	parent := t.TempDir()
	real := filepath.Join(parent, "real")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	configured := filepath.Join(parent, "workspace-link")
	if err := os.Symlink(real, configured); err != nil {
		t.Fatal(err)
	}
	identity, err := InspectExistingPath(configured)
	if err != nil {
		t.Fatal(err)
	}
	if identity.Configured != configured {
		t.Fatalf("configured=%q, want %q", identity.Configured, configured)
	}
	if identity.Absolute != configured {
		t.Fatalf("absolute=%q, want %q", identity.Absolute, configured)
	}
	if identity.Real != real {
		t.Fatalf("real=%q, want %q", identity.Real, real)
	}
}

func TestResolveRelativePathRejectsTraversalAndSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "safe"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "safe", "file.txt"), []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "target"), filepath.Join(root, "link-file")); err != nil {
		t.Fatal(err)
	}

	resolved, err := ResolveRelativePath(root, "safe/file.txt")
	if err != nil || resolved != filepath.Join(root, "safe", "file.txt") {
		t.Fatalf("resolved=%q err=%v", resolved, err)
	}
	for _, unsafe := range []string{"../outside", filepath.Join(root, "safe", "file.txt"), "escape/file.txt"} {
		if _, err := ResolveRelativePath(root, unsafe); !errors.Is(err, ErrInvalidRelativePath) && !errors.Is(err, ErrPathOutsideRoots) {
			t.Fatalf("ResolveRelativePath(%q) error=%v", unsafe, err)
		}
	}
	// The final symlink is returned without following it. This lets callers hash
	// the link target text without reading a file outside the workspace.
	resolved, err = ResolveRelativePath(root, "link-file")
	if err != nil || resolved != filepath.Join(root, "link-file") {
		t.Fatalf("final symlink resolved=%q err=%v", resolved, err)
	}
}
