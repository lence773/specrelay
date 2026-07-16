package migrations

import (
	"strings"
	"testing"
	"testing/fstest"
)

func TestLoadCatalogSortsVersionsAndCalculatesChecksums(t *testing.T) {
	source := fstest.MapFS{
		"sql/010_later.sql":   {Data: []byte("SELECT 10;")},
		"sql/002_earlier.sql": {Data: []byte("SELECT 2;")},
		"sql/README.txt":      {Data: []byte("ignored")},
	}

	catalog, err := loadCatalog(source)
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}
	if len(catalog) != 2 {
		t.Fatalf("catalog length = %d, want 2", len(catalog))
	}
	if catalog[0].version != 2 || catalog[0].name != "002_earlier.sql" {
		t.Fatalf("first migration = %+v, want version 2", catalog[0])
	}
	if catalog[1].version != 10 || catalog[1].name != "010_later.sql" {
		t.Fatalf("second migration = %+v, want version 10", catalog[1])
	}
	for _, item := range catalog {
		if len(item.checksum) != 64 {
			t.Fatalf("checksum for %s has length %d, want SHA-256 hex", item.name, len(item.checksum))
		}
	}
	if catalog[0].checksum == catalog[1].checksum {
		t.Fatal("different migration content unexpectedly has the same checksum")
	}
}

func TestLoadCatalogRejectsDuplicateMigrationVersions(t *testing.T) {
	_, err := loadCatalog(fstest.MapFS{
		"sql/001_initial.sql": {Data: []byte("SELECT 1;")},
		"sql/001_other.sql":   {Data: []byte("SELECT 2;")},
	})
	if err == nil || !strings.Contains(err.Error(), "duplicate migration version 1") {
		t.Fatalf("duplicate version error = %v", err)
	}
}

func TestParseVersionRejectsInvalidMigrationNames(t *testing.T) {
	for _, name := range []string{"initial.sql", "000_invalid.sql", "001_.sql", "abc_change.sql"} {
		if _, err := parseVersion(name); err == nil {
			t.Fatalf("parseVersion(%q) unexpectedly succeeded", name)
		}
	}

	version, err := parseVersion("015_add_priority.sql")
	if err != nil || version != 15 {
		t.Fatalf("parseVersion valid name = (%d, %v), want (15, nil)", version, err)
	}
}

func TestValidateAppliedMigrationsRejectsUnknownDatabaseVersion(t *testing.T) {
	catalog := []migration{{version: 1, name: "001_initial.sql", checksum: "expected"}}
	applied := map[int64]appliedMigration{
		2: {name: "002_future.sql", checksum: stringPointer("future")},
	}

	err := validateAppliedMigrations(catalog, applied)
	if err == nil || !strings.Contains(err.Error(), "migration version 2 (002_future.sql) that is not present") {
		t.Fatalf("unknown migration version error = %v", err)
	}
}

func TestValidateAppliedMigrationsRejectsChangedMetadata(t *testing.T) {
	catalog := []migration{{version: 1, name: "001_initial.sql", checksum: "expected"}}

	for _, applied := range []map[int64]appliedMigration{
		{1: {name: "001_renamed.sql", checksum: stringPointer("expected")}},
		{1: {name: "001_initial.sql", checksum: stringPointer("changed")}},
	} {
		if err := validateAppliedMigrations(catalog, applied); err == nil {
			t.Fatal("changed applied migration metadata unexpectedly passed validation")
		}
	}
}

func stringPointer(value string) *string {
	return &value
}
