package store_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/adnoctem/pertry/internal/sicht/store"
)

func open(t *testing.T) *store.DB {
	t.Helper()

	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "sicht.db"))
	if err != nil {
		t.Fatalf("Open() error = %v, want nil", err)
	}

	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})

	return db
}

func TestOpenAppliesMigrations(t *testing.T) {
	t.Parallel()

	db := open(t)

	pending, err := db.PendingMigrations(context.Background())
	if err != nil {
		t.Fatalf("PendingMigrations() error = %v, want nil", err)
	}

	if len(pending) != 0 {
		t.Errorf("PendingMigrations() = %v, want none immediately after Open", pending)
	}

	// Every table and view docs/ARCHITECTURE.md#sicht-data-model specifies has to exist, because doctor reports the
	// schema as current on the strength of the migration bookkeeping alone.
	for _, name := range []string{"files", "locations", "metadata", "results", "batches", "runs", "decisions"} {
		var found string

		err := db.SQL().QueryRowContext(context.Background(),
			`SELECT name FROM sqlite_master WHERE name = ?`, name).Scan(&found)
		if err != nil {
			t.Errorf("object %q missing from the schema: %v", name, err)
		}
	}
}

func TestOpenIsIdempotent(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "sicht.db")

	for i := range 3 {
		db, err := store.Open(context.Background(), path)
		if err != nil {
			t.Fatalf("Open() #%d error = %v, want nil", i+1, err)
		}

		if err := db.Close(); err != nil {
			t.Fatalf("Close() #%d error = %v", i+1, err)
		}
	}
}

// TestForeignKeysAreEnforced proves the DSN pragma took effect. SQLite defaults foreign keys OFF
// and silently ignores a malformed pragma, so without this the cascade below is decoration.
func TestForeignKeysAreEnforced(t *testing.T) {
	t.Parallel()

	db := open(t)

	_, err := db.SQL().ExecContext(context.Background(),
		`INSERT INTO locations (path, hash, mtime, seen_at) VALUES (?, ?, ?, ?)`,
		"photos/orphan.jpg", "no-such-hash", "2026-10-12T00:00:00Z", "2026-10-12T00:00:00Z")
	if err == nil {
		t.Error("inserting a location for an unknown hash succeeded, want a foreign key violation")
	}
}

// TestDeletingAFileCascades is what keeps `scan` from leaving dangling rows behind.
func TestDeletingAFileCascades(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := open(t)

	insertFile(t, db, "aaa")
	exec(t, db, `INSERT INTO locations (path, hash, mtime, seen_at) VALUES (?, ?, ?, ?)`,
		"photos/a.jpg", "aaa", "2026-10-12T00:00:00Z", "2026-10-12T00:00:00Z")
	exec(t, db, `INSERT INTO metadata (hash, exif_json) VALUES (?, ?)`, "aaa", `{}`)
	insertResult(t, db, "aaa", "kind", "photo", "model:qwen3-vl:8b", "2026-10-12T00:00:00Z")

	exec(t, db, `DELETE FROM files WHERE hash = ?`, "aaa")

	for _, table := range []string{"locations", "metadata", "results"} {
		var n int

		if err := db.SQL().QueryRowContext(ctx,
			`SELECT count(*) FROM `+table+` WHERE hash = ?`, "aaa").Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}

		if n != 0 {
			t.Errorf("%s still holds %d rows for the deleted hash, want 0", table, n)
		}
	}
}

// TestOneHashManyLocations is the deduplication premise: the same image filed twice is one file in
// two places, not two files (docs/ARCHITECTURE.md#sicht-data-model).
func TestOneHashManyLocations(t *testing.T) {
	t.Parallel()

	db := open(t)

	insertFile(t, db, "aaa")
	exec(t, db, `INSERT INTO locations (path, hash, mtime, seen_at) VALUES (?, ?, ?, ?)`,
		"photos/a.jpg", "aaa", "2026-10-12T00:00:00Z", "2026-10-12T00:00:00Z")
	exec(t, db, `INSERT INTO locations (path, hash, mtime, seen_at) VALUES (?, ?, ?, ?)`,
		"backup/a copy.jpg", "aaa", "2026-10-12T00:00:00Z", "2026-10-12T00:00:00Z")

	var n int
	if err := db.SQL().QueryRowContext(context.Background(),
		`SELECT count(*) FROM locations WHERE hash = ?`, "aaa").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}

	if n != 2 {
		t.Errorf("locations for one hash = %d, want 2", n)
	}
}

// TestResultsFromDifferentSourcesCoexist is the reason source is in the primary key: a human
// decision must not overwrite the model's opinion, because the review UI needs to show what the
// model said and `stats` needs to measure agreement (docs/ARCHITECTURE.md#review).
func TestResultsFromDifferentSourcesCoexist(t *testing.T) {
	t.Parallel()

	db := open(t)

	insertFile(t, db, "aaa")
	insertResult(t, db, "aaa", "kind", "photo", "model:qwen3-vl:8b", "2026-10-12T10:00:00Z")
	insertResult(t, db, "aaa", "kind", "screenshot", "human", "2026-10-12T11:00:00Z")
	insertResult(t, db, "aaa", "kind", "photo", "rule:0", "2026-10-12T09:00:00Z")

	var n int
	if err := db.SQL().QueryRowContext(context.Background(),
		`SELECT count(*) FROM results WHERE hash = ? AND facet = ?`, "aaa", "kind").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}

	if n != 3 {
		t.Errorf("results rows = %d, want all 3 sources to coexist", n)
	}
}

// TestDecisionsPrecedence is the view docs/ARCHITECTURE.md#review turns on: human beats rule beats model, and
// within one source class the most recent wins. Every ordering is exercised with the rows inserted
// in an order that would give the wrong answer under a naive "last write wins".
func TestDecisionsPrecedence(t *testing.T) {
	t.Parallel()

	type row struct {
		facet, value, source, createdAt string
	}

	tests := []struct {
		name  string
		rows  []row
		want  string
		which string
	}{
		{
			name: "human beats a later model result",
			rows: []row{
				{"kind", "screenshot", "human", "2026-10-12T09:00:00Z"},
				{"kind", "photo", "model:qwen3-vl:8b", "2026-10-12T23:00:00Z"},
			},
			want:  "screenshot",
			which: "human",
		},
		{
			name: "human beats a later rule result",
			rows: []row{
				{"kind", "screenshot", "human", "2026-10-12T09:00:00Z"},
				{"kind", "photo", "rule:2", "2026-10-12T23:00:00Z"},
			},
			want:  "screenshot",
			which: "human",
		},
		{
			name: "a rule beats a later model result",
			rows: []row{
				{"kind", "photo", "rule:2", "2026-10-12T09:00:00Z"},
				{"kind", "screenshot", "model:qwen3-vl:8b", "2026-10-12T23:00:00Z"},
			},
			want:  "photo",
			which: "rule:2",
		},
		{
			// Re-running `run` after tuning a prompt must take effect without anyone
			// deleting the old row.
			name: "the newest model result wins",
			rows: []row{
				{"kind", "photo", "model:qwen3-vl:8b", "2026-10-12T09:00:00Z"},
				{"kind", "screenshot", "model:llama4-vision:16b", "2026-10-12T23:00:00Z"},
			},
			want:  "screenshot",
			which: "model:llama4-vision:16b",
		},
		{
			name: "the newest rule result wins",
			rows: []row{
				{"kind", "photo", "rule:0", "2026-10-12T23:00:00Z"},
				{"kind", "screenshot", "rule:9", "2026-10-12T09:00:00Z"},
			},
			want:  "photo",
			which: "rule:0",
		},
		{
			name: "all three classes at once",
			rows: []row{
				{"kind", "a", "model:m", "2026-10-12T23:00:00Z"},
				{"kind", "b", "rule:0", "2026-10-12T22:00:00Z"},
				{"kind", "c", "human", "2026-10-12T01:00:00Z"},
			},
			want:  "c",
			which: "human",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			db := open(t)
			insertFile(t, db, "aaa")

			for _, r := range tt.rows {
				insertResult(t, db, "aaa", r.facet, r.value, r.source, r.createdAt)
			}

			var value, source string

			err := db.SQL().QueryRowContext(context.Background(),
				`SELECT value, source FROM decisions WHERE hash = ? AND facet = ?`,
				"aaa", "kind").Scan(&value, &source)
			if err != nil {
				t.Fatalf("query decisions: %v", err)
			}

			if value != tt.want || source != tt.which {
				t.Errorf("decisions = (%q, %q), want (%q, %q)", value, source, tt.want, tt.which)
			}
		})
	}
}

// TestAHumanCorrectionIsAnUpsert records a consequence of the primary key that is easy to get
// wrong when writing the review handler in M3.
//
// `PRIMARY KEY (hash, facet, source)` means a second 'human' row for the same facet cannot exist,
// so correcting an earlier human decision is an upsert and not an append. That is the right
// design - nobody wants a per-facet audit log of every time they changed their mind - but it means
// the view's `created_at DESC` tiebreaker only ever discriminates between *different* source
// strings within one class, such as two models or two rule indices. A writer that inserts a
// correction gets a constraint violation rather than a silently ignored edit, which is why this is
// worth pinning down.
func TestAHumanCorrectionIsAnUpsert(t *testing.T) {
	t.Parallel()

	db := open(t)
	insertFile(t, db, "aaa")
	insertResult(t, db, "aaa", "kind", "photo", "human", "2026-10-12T09:00:00Z")

	// A plain insert is refused, which is the behaviour a caller has to be written against.
	_, err := db.SQL().ExecContext(context.Background(),
		`INSERT INTO results (hash, facet, value, source, created_at) VALUES (?, ?, ?, ?, ?)`,
		"aaa", "kind", "screenshot", "human", "2026-10-12T23:00:00Z")
	if err == nil {
		t.Fatal("a second human row for one facet was accepted, want a primary key violation")
	}

	exec(t, db, `INSERT INTO results (hash, facet, value, source, created_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (hash, facet, source) DO UPDATE SET value = excluded.value,
		                                                created_at = excluded.created_at`,
		"aaa", "kind", "screenshot", "human", "2026-10-12T23:00:00Z")

	var value string
	if err := db.SQL().QueryRowContext(context.Background(),
		`SELECT value FROM decisions WHERE hash = ? AND facet = ?`, "aaa", "kind").Scan(&value); err != nil {
		t.Fatalf("query decisions: %v", err)
	}

	if value != "screenshot" {
		t.Errorf("decisions value = %q, want the corrected %q", value, "screenshot")
	}

	var n int
	if err := db.SQL().QueryRowContext(context.Background(),
		`SELECT count(*) FROM results WHERE hash = ? AND facet = ? AND source = ?`,
		"aaa", "kind", "human").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}

	if n != 1 {
		t.Errorf("human rows = %d, want the correction to replace rather than append", n)
	}
}

// TestDecisionsIsOnePerFacet guards the partition: a file with several facets decided yields one
// row each, not a cross product.
func TestDecisionsIsOnePerFacet(t *testing.T) {
	t.Parallel()

	db := open(t)

	insertFile(t, db, "aaa")
	insertFile(t, db, "bbb")

	insertResult(t, db, "aaa", "kind", "screenshot", "human", "2026-10-12T09:00:00Z")
	insertResult(t, db, "aaa", "kind", "photo", "model:m", "2026-10-12T10:00:00Z")
	insertResult(t, db, "aaa", "platform", "ios", "model:m", "2026-10-12T10:00:00Z")
	insertResult(t, db, "bbb", "kind", "photo", "rule:0", "2026-10-12T10:00:00Z")

	var n int
	if err := db.SQL().QueryRowContext(context.Background(),
		`SELECT count(*) FROM decisions`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}

	if n != 3 {
		t.Errorf("decisions rows = %d, want 3 (two facets for aaa, one for bbb)", n)
	}
}

// TestDecisionsCarriesProvenance matters because the review UI and `run`'s staleness check read
// these columns through the view, not from results directly.
func TestDecisionsCarriesProvenance(t *testing.T) {
	t.Parallel()

	db := open(t)
	insertFile(t, db, "aaa")

	exec(t, db, `INSERT INTO results
		(hash, facet, value, confidence, source, stage, model, model_digest,
		 prompt_hash, schema_hash, payload_json, alternatives_json, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"aaa", "kind", "photo", 0.83, "model:qwen3-vl:8b", "kind", "qwen3-vl:8b", "sha256:dead",
		"prompt-hash", "schema-hash", `{"visible_ui":"none"}`, `[{"value":"screenshot","p":0.1}]`,
		"2026-10-12T10:00:00Z")

	var (
		confidence                                    sql.NullFloat64
		stage, model, digest, prompt, schema, payload sql.NullString
		alternatives                                  sql.NullString
	)

	err := db.SQL().QueryRowContext(context.Background(),
		`SELECT confidence, stage, model, model_digest, prompt_hash, schema_hash,
		        payload_json, alternatives_json
		 FROM decisions WHERE hash = ?`, "aaa").
		Scan(&confidence, &stage, &model, &digest, &prompt, &schema, &payload, &alternatives)
	if err != nil {
		t.Fatalf("query: %v", err)
	}

	if confidence.Float64 != 0.83 {
		t.Errorf("confidence = %v, want 0.83", confidence.Float64)
	}

	if digest.String != "sha256:dead" {
		t.Errorf("model_digest = %q, want it preserved through the view", digest.String)
	}

	if payload.String != `{"visible_ui":"none"}` {
		t.Errorf("payload_json = %q, want it preserved through the view", payload.String)
	}

	if alternatives.String == "" {
		t.Error("alternatives_json is empty, want it preserved through the view")
	}
}

func TestPendingMigrationsOnAnEmptyDatabase(t *testing.T) {
	t.Parallel()

	// A database file that exists but has no schema_migrations table is what doctor sees
	// before `init` has run to completion.
	path := filepath.Join(t.TempDir(), "sicht.db")

	db, err := sql.Open(store.DriverName, path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}

	if err := db.PingContext(context.Background()); err != nil {
		t.Fatalf("ping: %v", err)
	}

	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	opened, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open() error = %v, want nil on an existing but empty file", err)
	}

	defer func() { _ = opened.Close() }()

	pending, err := opened.PendingMigrations(context.Background())
	if err != nil {
		t.Fatalf("PendingMigrations() error = %v", err)
	}

	if len(pending) != 0 {
		t.Errorf("PendingMigrations() = %v, want none after Open migrated it", pending)
	}
}

func TestOpenRejectsAnUnwritableDirectory(t *testing.T) {
	t.Parallel()

	_, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "absent", "sicht.db"))
	if err == nil {
		t.Error("Open() error = nil, want an error for a path whose directory does not exist")
	}
}

// TestTimestampsAreComparableAsText relies on RFC 3339 in UTC, which is what makes the view's
// created_at DESC ordering correct without a date function.
func TestTimestampsAreComparableAsText(t *testing.T) {
	t.Parallel()

	earlier := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC).Format(time.RFC3339)
	later := time.Date(2026, 10, 12, 23, 0, 0, 0, time.UTC).Format(time.RFC3339)

	if earlier >= later {
		t.Errorf("%q < %q is false; the decisions view's text ordering would be wrong", earlier, later)
	}
}

func insertFile(t *testing.T, db *store.DB, hash string) {
	t.Helper()

	exec(t, db, `INSERT INTO files (hash, size, ext, first_seen) VALUES (?, ?, ?, ?)`,
		hash, 1024, "jpg", "2026-10-12T00:00:00Z")
}

func insertResult(t *testing.T, db *store.DB, hash, facet, value, source, createdAt string) {
	t.Helper()

	exec(t, db, `INSERT INTO results (hash, facet, value, source, created_at) VALUES (?, ?, ?, ?, ?)`,
		hash, facet, value, source, createdAt)
}

func exec(t *testing.T, db *store.DB, query string, args ...any) {
	t.Helper()

	if _, err := db.SQL().ExecContext(context.Background(), query, args...); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}
