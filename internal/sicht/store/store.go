// Package store owns sicht's SQLite database: connection setup, the embedded migrations, and the
// queries the pipeline runs.
//
// Everything is keyed by content hash rather than by path, so a result survives the file moving -
// which it will, because `apply` moves files (docs/ARCHITECTURE.md#workspace-and-identity).
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"

	// Pure-Go SQLite, registered as "sqlite" rather than the cgo binding's "sqlite3". A
	// build-system decision: it keeps the windows/amd64 cross-compile free of a C toolchain,
	// which docs/CONTRIBUTING.md makes a requirement.
	_ "modernc.org/sqlite"
)

// DriverName is the driver registered by modernc.org/sqlite.
const DriverName = "sqlite"

//go:embed migrations/*.sql
var migrationFS embed.FS

// DB is a handle on a workspace's database.
type DB struct {
	sql *sql.DB
}

// Open opens the database at path, creating it if absent, and brings it up to the current schema.
func Open(ctx context.Context, dbPath string) (*DB, error) {
	// WAL and foreign keys go through the DSN rather than a PRAGMA statement so they apply to
	// every pooled connection rather than whichever one ran the PRAGMA.
	dsn := dbPath + "?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"

	handle, err := sql.Open(DriverName, dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s database %q: %w", DriverName, dbPath, err)
	}

	// Pure-Go SQLite tolerates concurrent readers but writers must be serialised, and `scan`
	// writes from a worker pool.
	handle.SetMaxOpenConns(1)

	if err := handle.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("connect to %q: %w\nhint: check that .sicht/ is writable",
			dbPath, errors.Join(err, handle.Close()))
	}

	db := &DB{sql: handle}

	if err := db.migrate(ctx); err != nil {
		return nil, errors.Join(err, handle.Close())
	}

	return db, nil
}

// Close releases the handle.
func (db *DB) Close() error {
	if err := db.sql.Close(); err != nil {
		return fmt.Errorf("close database: %w", err)
	}

	return nil
}

// SQL exposes the underlying handle for the read-only aggregate queries `stats` and `doctor` run.
func (db *DB) SQL() *sql.DB { return db.sql }

// PendingMigrations reports which embedded migrations have not been applied. `doctor` uses it to
// answer "are migrations current" without applying anything.
func (db *DB) PendingMigrations(ctx context.Context) ([]string, error) {
	applied, err := db.appliedMigrations(ctx)
	if err != nil {
		return nil, err
	}

	names, err := migrationNames()
	if err != nil {
		return nil, err
	}

	var pending []string

	for _, name := range names {
		if _, ok := applied[name]; !ok {
			pending = append(pending, name)
		}
	}

	return pending, nil
}

// migrate applies every embedded migration not yet applied, in filename order, each in its own
// transaction.
func (db *DB) migrate(ctx context.Context) error {
	if _, err := db.sql.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
		    name       TEXT PRIMARY KEY,
		    applied_at TEXT NOT NULL
		)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	pending, err := db.PendingMigrations(ctx)
	if err != nil {
		return err
	}

	for _, name := range pending {
		body, err := migrationFS.ReadFile(path.Join("migrations", name))
		if err != nil {
			return fmt.Errorf("read migration %q: %w", name, err)
		}

		if err := db.applyMigration(ctx, name, string(body)); err != nil {
			return err
		}
	}

	return nil
}

func (db *DB) appliedMigrations(ctx context.Context) (map[string]struct{}, error) {
	rows, err := db.sql.QueryContext(ctx, `SELECT name FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("read applied migrations: %w", err)
	}
	defer func() { _ = rows.Close() }()

	applied := make(map[string]struct{})

	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan applied migration: %w", err)
		}

		applied[name] = struct{}{}
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate applied migrations: %w", err)
	}

	return applied, nil
}

func (db *DB) applyMigration(ctx context.Context, name, body string) error {
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration %q: %w", name, err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, body); err != nil {
		return fmt.Errorf("apply migration %q: %w", name, err)
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations (name, applied_at) VALUES (?, datetime('now'))`, name); err != nil {
		return fmt.Errorf("record migration %q: %w", name, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration %q: %w", name, err)
	}

	return nil
}

// migrationNames lists the embedded migrations in application order, which is lexical order of
// the NNNN_ prefix.
func migrationNames() ([]string, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("list embedded migrations: %w", err)
	}

	names := make([]string, 0, len(entries))

	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}

	sort.Strings(names)

	return names, nil
}
