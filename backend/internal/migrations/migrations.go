package migrations

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed sql/*.sql
var files embed.FS

const advisoryLockID int64 = 0x5350454352454c41

// Stage describes an observable phase of startup database migration.
type Stage string

const (
	StageWaitingForLock Stage = "waiting_for_lock"
	StageVerifying      Stage = "verifying"
	StageApplying       Stage = "applying"
	StageApplied        Stage = "applied"
	StageComplete       Stage = "complete"
)

// Event is emitted while the embedded migration catalog is checked and applied.
// It intentionally contains no connection details or SQL body, so callers can
// safely expose progress in desktop startup UI and structured logs.
type Event struct {
	Stage   Stage
	Version int64
	Name    string
}

// Observer receives migration progress. It must return quickly; migration work
// is performed synchronously by the caller.
type Observer func(Event)

type migration struct {
	version  int64
	name     string
	body     []byte
	checksum string
}

type appliedMigration struct {
	name     string
	checksum *string
}

// Run applies every embedded migration that has not yet been recorded. It is
// retained for callers that do not need startup progress notifications.
func Run(ctx context.Context, pool *pgxpool.Pool) error {
	return RunWithObserver(ctx, pool, nil)
}

// RunWithObserver validates the immutable migration catalog, serializes schema
// changes across all desktop instances sharing this PostgreSQL database, and
// applies each pending file in its own transaction.
//
// Applied files are protected by SHA-256 checksums. Databases created by older
// SpecRelay releases receive a one-time checksum backfill, after which editing
// a released migration is detected before any new schema change is attempted.
func RunWithObserver(ctx context.Context, pool *pgxpool.Pool, observe Observer) error {
	catalog, err := loadCatalog(files)
	if err != nil {
		return err
	}

	conn, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()

	emit(observe, Event{Stage: StageWaitingForLock})
	if _, err = conn.Exec(ctx, "SELECT pg_advisory_lock($1)", advisoryLockID); err != nil {
		return err
	}
	defer conn.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", advisoryLockID)

	if _, err = conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version BIGINT PRIMARY KEY, name TEXT NOT NULL, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	// Existing installations predate integrity tracking. Nullable is deliberate:
	// it allows the first upgraded binary to backfill checksums safely.
	if _, err = conn.Exec(ctx, `ALTER TABLE schema_migrations ADD COLUMN IF NOT EXISTS checksum TEXT`); err != nil {
		return fmt.Errorf("prepare schema migration integrity metadata: %w", err)
	}

	emit(observe, Event{Stage: StageVerifying})
	applied, err := readAppliedMigrations(ctx, conn)
	if err != nil {
		return err
	}
	if err := validateAppliedMigrations(catalog, applied); err != nil {
		return err
	}

	for _, item := range catalog {
		appliedItem, exists := applied[item.version]
		if !exists {
			emit(observe, Event{Stage: StageApplying, Version: item.version, Name: item.name})
			if err = applyMigration(ctx, conn, item); err != nil {
				return err
			}
			emit(observe, Event{Stage: StageApplied, Version: item.version, Name: item.name})
			continue
		}

		if appliedItem.checksum == nil || strings.TrimSpace(*appliedItem.checksum) == "" {
			if _, err = conn.Exec(ctx, `UPDATE schema_migrations SET checksum=$2 WHERE version=$1 AND (checksum IS NULL OR checksum='')`, item.version, item.checksum); err != nil {
				return fmt.Errorf("backfill checksum for migration %d: %w", item.version, err)
			}
		}
	}

	emit(observe, Event{Stage: StageComplete})
	return nil
}

func readAppliedMigrations(ctx context.Context, conn *pgxpool.Conn) (map[int64]appliedMigration, error) {
	rows, err := conn.Query(ctx, `SELECT version, name, checksum FROM schema_migrations ORDER BY version`)
	if err != nil {
		return nil, fmt.Errorf("read applied migrations: %w", err)
	}
	defer rows.Close()

	applied := make(map[int64]appliedMigration)
	for rows.Next() {
		var version int64
		var item appliedMigration
		if err := rows.Scan(&version, &item.name, &item.checksum); err != nil {
			return nil, fmt.Errorf("read applied migration: %w", err)
		}
		applied[version] = item
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read applied migrations: %w", err)
	}
	return applied, nil
}

func validateAppliedMigrations(catalog []migration, applied map[int64]appliedMigration) error {
	catalogByVersion := make(map[int64]migration, len(catalog))
	for _, item := range catalog {
		catalogByVersion[item.version] = item
	}

	versions := make([]int64, 0, len(applied))
	for version := range applied {
		versions = append(versions, version)
	}
	sort.Slice(versions, func(i, j int) bool { return versions[i] < versions[j] })
	for _, version := range versions {
		item, exists := catalogByVersion[version]
		if !exists {
			return fmt.Errorf("migration integrity check failed: database has migration version %d (%s) that is not present in this SpecRelay release; upgrade the application instead of running an older binary", version, applied[version].name)
		}

		appliedItem := applied[version]
		if appliedItem.name != item.name {
			return fmt.Errorf("migration integrity check failed for version %d: applied name %q does not match embedded name %q; restore the original release migration and do not edit applied files", item.version, appliedItem.name, item.name)
		}
		if appliedItem.checksum != nil && strings.TrimSpace(*appliedItem.checksum) != "" && *appliedItem.checksum != item.checksum {
			return fmt.Errorf("migration integrity check failed for version %d (%s): checksum differs from the applied release; restore the original migration and add a new migration instead of editing an applied file", item.version, item.name)
		}
	}

	return nil
}

func applyMigration(ctx context.Context, conn *pgxpool.Conn, item migration) error {
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin migration %s: %w", item.name, err)
	}
	if _, err = tx.Exec(ctx, string(item.body)); err == nil {
		_, err = tx.Exec(ctx, `INSERT INTO schema_migrations(version,name,checksum) VALUES($1,$2,$3)`, item.version, item.name, item.checksum)
	}
	if err != nil {
		_ = tx.Rollback(ctx)
		return fmt.Errorf("apply migration %s: %w", item.name, err)
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit migration %s: %w", item.name, err)
	}
	return nil
}

func loadCatalog(source fs.FS) ([]migration, error) {
	entries, err := fs.ReadDir(source, "sql")
	if err != nil {
		return nil, err
	}

	catalog := make([]migration, 0, len(entries))
	seenVersions := make(map[int64]struct{}, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".sql" {
			continue
		}
		version, err := parseVersion(entry.Name())
		if err != nil {
			return nil, err
		}
		if _, exists := seenVersions[version]; exists {
			return nil, fmt.Errorf("duplicate migration version %d", version)
		}
		seenVersions[version] = struct{}{}
		body, err := fs.ReadFile(source, "sql/"+entry.Name())
		if err != nil {
			return nil, err
		}
		digest := sha256.Sum256(body)
		catalog = append(catalog, migration{
			version:  version,
			name:     entry.Name(),
			body:     body,
			checksum: hex.EncodeToString(digest[:]),
		})
	}
	if len(catalog) == 0 {
		return nil, fmt.Errorf("no SQL migrations found")
	}
	sort.Slice(catalog, func(i, j int) bool { return catalog[i].version < catalog[j].version })
	return catalog, nil
}

func parseVersion(name string) (int64, error) {
	parts := strings.SplitN(name, "_", 2)
	if len(parts) != 2 || strings.TrimSpace(parts[1]) == ".sql" {
		return 0, fmt.Errorf("invalid migration filename %q: expected NNN_description.sql", name)
	}
	version, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || version <= 0 {
		return 0, fmt.Errorf("invalid migration %q: version must be a positive integer", name)
	}
	return version, nil
}

func emit(observer Observer, event Event) {
	if observer != nil {
		observer(event)
	}
}
