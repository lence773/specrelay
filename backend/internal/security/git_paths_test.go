package security

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestResolveRelativePathForWriteChecksExistingParentsAndMissingSuffix(t *testing.T) {
	root := t.TempDir()
	path, err := ResolveRelativePathForWrite(root, "new/nested/file.txt")
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(root, "new", "nested", "file.txt") {
		t.Fatalf("resolved path=%q", path)
	}
	outside := t.TempDir()
	if err = os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if _, err = ResolveRelativePathForWrite(root, "escape/file.txt"); !errors.Is(err, ErrPathOutsideRoots) {
		t.Fatalf("symlink escape error=%v", err)
	}
	for _, invalid := range []string{"", ".", "../outside", "/absolute"} {
		if _, err = ResolveRelativePathForWrite(root, invalid); !errors.Is(err, ErrInvalidRelativePath) {
			t.Fatalf("%q error=%v", invalid, err)
		}
	}
}
