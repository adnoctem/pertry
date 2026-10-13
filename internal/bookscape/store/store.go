// Package store owns bookscape's SQLite database: connection setup, the embedded migrations,
// and every query the pipeline runs.
//
// The HTML file is an interchange format, not a working store. All mutation happens here, and
// the exporter is a pure projection back out (docs/ARCHITECTURE.md#process-model).
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"

	// Pure-Go SQLite. Registers the driver under the name "sqlite" - not "sqlite3", which is
	// what the cgo binding most SQLite documentation assumes registers. This is a
	// build-system decision, not a performance one: it keeps the windows/amd64
	// cross-compile free of a C toolchain (docs/ARCHITECTURE.md#storage).
	_ "modernc.org/sqlite"
)

// DriverName is the driver registered by modernc.org/sqlite.
const DriverName = "sqlite"

//go:embed migrations/*.sql
var migrationFS embed.FS

// DB is a handle on the bookscape database.
type DB struct {
	sql *sql.DB
}

// Open opens (creating if absent) the database at path and brings it up to the current schema
// version.
func Open(ctx context.Context, path string) (*DB, error) {
	// WAL mode and foreign keys are set through the DSN rather than with PRAGMA statements,
	// so they apply to every connection in the pool rather than only the one that happened
	// to run the PRAGMA.
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"

	handle, err := sql.Open(DriverName, dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s database %q: %w", DriverName, path, err)
	}

	// Pure-Go SQLite tolerates concurrent readers, but writers must be serialised.
	handle.SetMaxOpenConns(1)

	if err := handle.PingContext(ctx); err != nil {
		closeErr := handle.Close()

		return nil, fmt.Errorf("connect to database %q: %w\nhint: check the directory exists and is writable",
			path, errors.Join(err, closeErr))
	}

	db := &DB{sql: handle}
	if err := db.migrate(ctx); err != nil {
		closeErr := handle.Close()

		return nil, errors.Join(err, closeErr)
	}

	return db, nil
}

// Close releases the database handle.
func (db *DB) Close() error {
	if err := db.sql.Close(); err != nil {
		return fmt.Errorf("close database: %w", err)
	}

	return nil
}

// SQL exposes the underlying handle. It exists for the stats and review passes, which run
// read-only aggregate queries that do not justify a method each.
func (db *DB) SQL() *sql.DB {
	return db.sql
}

// migrate applies every embedded migration that has not been applied yet, in filename order,
// each inside its own transaction.
func (db *DB) migrate(ctx context.Context) error {
	if _, err := db.sql.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
		    name       TEXT PRIMARY KEY,
		    applied_at INTEGER NOT NULL
		)`); err != nil {
		return fmt.Errorf("create schema_migrations table: %w", err)
	}

	applied, err := db.appliedMigrations(ctx)
	if err != nil {
		return err
	}

	names, err := migrationNames()
	if err != nil {
		return err
	}

	for _, name := range names {
		if _, ok := applied[name]; ok {
			continue
		}

		body, err := migrationFS.ReadFile(filepath.ToSlash(filepath.Join("migrations", name)))
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
		`INSERT INTO schema_migrations (name, applied_at) VALUES (?, unixepoch())`, name); err != nil {
		return fmt.Errorf("record migration %q: %w", name, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration %q: %w", name, err)
	}

	return nil
}

// migrationNames lists the embedded migrations in the order they must be applied, which is
// lexical order of the NNNN_ prefix.
func migrationNames() ([]string, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("list embedded migrations: %w", err)
	}

	names := make([]string, 0, len(entries))

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}

		names = append(names, e.Name())
	}

	sort.Strings(names)

	return names, nil
}
