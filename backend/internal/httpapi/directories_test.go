package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestDirectoriesListsOnlyDirectories(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "beta"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "Alpha"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte("ignored"), 0o644); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/filesystem/directories?path="+root, nil)
	recorder := httptest.NewRecorder()
	new(Server).directories(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var listing directoryListing
	if err := json.NewDecoder(recorder.Body).Decode(&listing); err != nil {
		t.Fatal(err)
	}
	if listing.Path != root {
		t.Fatalf("path = %q, want %q", listing.Path, root)
	}
	if len(listing.Directories) != 2 || listing.Directories[0].Name != "Alpha" || listing.Directories[1].Name != "beta" {
		t.Fatalf("directories = %#v", listing.Directories)
	}
	if listing.ParentPath != filepath.Dir(root) {
		t.Fatalf("parentPath = %q, want %q", listing.ParentPath, filepath.Dir(root))
	}
	if len(listing.Roots) == 0 {
		t.Fatal("roots must include at least one selectable filesystem root")
	}
}

func TestDirectoryRootsAreAccessibleDirectories(t *testing.T) {
	roots := directoryRoots()
	if len(roots) == 0 {
		t.Fatal("expected at least one filesystem root")
	}
	for _, root := range roots {
		info, err := os.Stat(root.Path)
		if err != nil || !info.IsDir() {
			t.Fatalf("root %#v is not an accessible directory: %v", root, err)
		}
	}
}

func TestDirectoriesRejectsMissingPath(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/filesystem/directories?path=/path/that/does/not/exist", nil)
	recorder := httptest.NewRecorder()
	new(Server).directories(recorder, req)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}
