package store_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/adnoctem/pertry/internal/bookscape/store"
)

func open(t *testing.T) *store.DB {
	t.Helper()

	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "bookscape.db"))
	if err != nil {
		t.Fatalf("Open() error = %v, want nil", err)
	}

	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("Close() error = %v, want nil", err)
		}
	})

	return db
}

func ptr(v int64) *int64 { return &v }

func TestOpenAppliesMigrations(t *testing.T) {
	t.Parallel()

	db := open(t)
	ctx := context.Background()

	// Every table the pipeline writes to has to exist after Open.
	for _, table := range []string{
		"schema_migrations", "import_batches", "bookmarks", "source_paths",
		"liveness", "classifications", "tags", "bookmark_tags", "merge_log",
	} {
		var name string
		if err := db.SQL().QueryRowContext(ctx,
			`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&name); err != nil {
			t.Errorf("table %q missing after Open: %v", table, err)
		}
	}
}

// TestOpenIsIdempotent covers reopening an existing database: migrations already applied must
// not be applied twice.
func TestOpenIsIdempotent(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "bookscape.db")

	for i := range 3 {
		db, err := store.Open(ctx, path)
		if err != nil {
			t.Fatalf("Open() #%d error = %v, want nil", i+1, err)
		}

		if err := db.Close(); err != nil {
			t.Fatalf("Close() #%d error = %v, want nil", i+1, err)
		}
	}

	db, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("Open() error = %v, want nil", err)
	}
	defer func() { _ = db.Close() }()

	var applied int
	if err := db.SQL().QueryRowContext(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&applied); err != nil {
		t.Fatalf("count migrations: %v", err)
	}

	if applied != 1 {
		t.Errorf("schema_migrations rows = %d, want 1", applied)
	}
}

// TestOpenEnablesForeignKeys matters because the schema relies on ON DELETE CASCADE, and
// SQLite enforces foreign keys per connection rather than per database.
func TestOpenEnablesForeignKeys(t *testing.T) {
	t.Parallel()

	db := open(t)

	var on int
	if err := db.SQL().QueryRowContext(context.Background(), `PRAGMA foreign_keys`).Scan(&on); err != nil {
		t.Fatalf("read foreign_keys pragma: %v", err)
	}

	if on != 1 {
		t.Errorf("PRAGMA foreign_keys = %d, want 1", on)
	}
}

func TestImport(t *testing.T) {
	t.Parallel()

	db := open(t)
	ctx := context.Background()

	bookmarks := []store.NewBookmark{
		{
			URL:          "https://github.com/golang/go",
			URLNorm:      "https://github.com/golang/go",
			Title:        "golang/go",
			AddDate:      ptr(1385000100),
			LastModified: ptr(1385000100),
			SourcePath:   "Bookmarks bar/Dev",
			Tags:         []string{"go", "repo"},
		},
		{
			// The same normalised URL under a different folder: two rows now, one
			// survivor after dedupe.
			URL:        "http://www.github.com/golang/go/",
			URLNorm:    "https://github.com/golang/go",
			Title:      "",
			AddDate:    ptr(1600000100),
			SourcePath: "Reading List",
			Tags:       []string{"go"},
		},
		{
			URL:         "https://example.org/no-date",
			URLNorm:     "https://example.org/no-date",
			Title:       "No date",
			Description: "A <DD> the export carried",
			SourcePath:  "",
		},
	}

	batch, err := db.Import(ctx, "export.html", "abc123", bookmarks)
	if err != nil {
		t.Fatalf("Import() error = %v, want nil", err)
	}

	if batch.EntryCount != 3 {
		t.Errorf("Batch.EntryCount = %d, want 3", batch.EntryCount)
	}

	t.Run("row counts", func(t *testing.T) {
		counts := map[string]int{
			"bookmarks":    3,
			"source_paths": 2, // the third bookmark has no path
			"tags":         2, // "go" interned once across two bookmarks
			// 3 links: (go, repo) on the first, (go) on the second.
			"bookmark_tags": 3,
		}

		for table, want := range counts {
			var got int
			if err := db.SQL().QueryRowContext(ctx, `SELECT count(*) FROM `+table).Scan(&got); err != nil {
				t.Fatalf("count %s: %v", table, err)
			}

			if got != want {
				t.Errorf("%s rows = %d, want %d", table, got, want)
			}
		}
	})

	t.Run("a missing date stays NULL", func(t *testing.T) {
		// The distinction between "no date" and "epoch" has to survive to export, where a
		// NULL makes the encoder omit ADD_DATE rather than write a 1970 date.
		var addDate *int64
		if err := db.SQL().QueryRowContext(ctx,
			`SELECT add_date FROM bookmarks WHERE url = ?`, "https://example.org/no-date").Scan(&addDate); err != nil {
			t.Fatalf("read add_date: %v", err)
		}

		if addDate != nil {
			t.Errorf("add_date = %d, want NULL", *addDate)
		}
	})

	t.Run("an empty title stays NULL", func(t *testing.T) {
		var title *string
		if err := db.SQL().QueryRowContext(ctx,
			`SELECT title FROM bookmarks WHERE url = ?`, "http://www.github.com/golang/go/").Scan(&title); err != nil {
			t.Fatalf("read title: %v", err)
		}

		if title != nil {
			t.Errorf("title = %q, want NULL", *title)
		}
	})

	t.Run("the original URL is stored verbatim", func(t *testing.T) {
		var raw string
		if err := db.SQL().QueryRowContext(ctx,
			`SELECT url FROM bookmarks WHERE url_norm = ? ORDER BY id DESC LIMIT 1`,
			"https://github.com/golang/go").Scan(&raw); err != nil {
			t.Fatalf("read url: %v", err)
		}

		if want := "http://www.github.com/golang/go/"; raw != want {
			t.Errorf("url = %q, want %q; normalisation must never overwrite the original", raw, want)
		}
	})

	t.Run("imported tags record their provenance", func(t *testing.T) {
		rows, err := db.SQL().QueryContext(ctx,
			`SELECT DISTINCT source FROM bookmark_tags ORDER BY source`)
		if err != nil {
			t.Fatalf("read tag sources: %v", err)
		}
		defer func() { _ = rows.Close() }()

		var sources []string

		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatalf("scan source: %v", err)
			}

			sources = append(sources, s)
		}

		if diff := cmp.Diff([]string{string(store.TagSourceImport)}, sources); diff != "" {
			t.Errorf("tag sources mismatch (-want +got):\n%s", diff)
		}
	})
}

// TestFindBatch is what makes import idempotent.
func TestFindBatch(t *testing.T) {
	t.Parallel()

	db := open(t)
	ctx := context.Background()

	got, err := db.FindBatch(ctx, "nothing-imported-yet")
	if err != nil {
		t.Fatalf("FindBatch() error = %v, want nil", err)
	}

	if got != nil {
		t.Fatalf("FindBatch() = %+v, want nil for an unknown digest", got)
	}

	want, err := db.Import(ctx, "export.html", "digest", []store.NewBookmark{{
		URL: "https://example.com/", URLNorm: "https://example.com/",
	}})
	if err != nil {
		t.Fatalf("Import() error = %v, want nil", err)
	}

	got, err = db.FindBatch(ctx, "digest")
	if err != nil {
		t.Fatalf("FindBatch() error = %v, want nil", err)
	}

	if got == nil {
		t.Fatal("FindBatch() = nil, want the batch just imported")
	}

	if got.ID != want.ID || got.Filename != want.Filename || got.EntryCount != want.EntryCount {
		t.Errorf("FindBatch() = %+v, want %+v", got, want)
	}

	if got.ImportedAt.IsZero() {
		t.Error("Batch.ImportedAt is the zero time, want the recorded timestamp")
	}
}

// TestImportRejectsADuplicateDigest records that the UNIQUE constraint on the digest is what
// enforces idempotency, rather than it being only a convention in the import command.
func TestImportRejectsADuplicateDigest(t *testing.T) {
	t.Parallel()

	db := open(t)
	ctx := context.Background()

	b := []store.NewBookmark{{URL: "https://example.com/", URLNorm: "https://example.com/"}}

	if _, err := db.Import(ctx, "export.html", "same", b); err != nil {
		t.Fatalf("first Import() error = %v, want nil", err)
	}

	if _, err := db.Import(ctx, "export.html", "same", b); err == nil {
		t.Error("second Import() with the same digest error = nil, want a constraint error")
	}

	// The failed transaction must not have left its bookmarks behind.
	var count int
	if err := db.SQL().QueryRowContext(ctx, `SELECT count(*) FROM bookmarks`).Scan(&count); err != nil {
		t.Fatalf("count bookmarks: %v", err)
	}

	if count != 1 {
		t.Errorf("bookmarks rows = %d, want 1; the rolled-back import left rows behind", count)
	}
}
