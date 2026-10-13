package cmd_test

import (
	"database/sql"
	"strings"
	"testing"
)

// imported sets up a workspace and runs import, returning the database path.
func imported(t *testing.T) string {
	t.Helper()

	export, dbPath := workspace(t)

	if _, _, err := run(t, "import", "--database", dbPath, export); err != nil {
		t.Fatalf("import error = %v, want nil", err)
	}

	return dbPath
}

func queryInt(t *testing.T, dbPath, query string, args ...any) int {
	t.Helper()

	handle, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer func() { _ = handle.Close() }()

	var n int
	if err := handle.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}

	return n
}

// TestDedupeDryRunWritesNothing is the property --dry-run exists for.
func TestDedupeDryRunWritesNothing(t *testing.T) {
	t.Parallel()

	dbPath := imported(t)

	out, _, err := run(t, "dedupe", "--database", dbPath, "--dry-run")
	if err != nil {
		t.Fatalf("dedupe --dry-run error = %v, want nil", err)
	}

	if !strings.Contains(out, "Nothing was written") {
		t.Errorf("stdout = %q, want it to say nothing was written", out)
	}

	// Both original URLs have to be visible for the proposal to be auditable.
	for _, want := range []string{"https://github.com/golang/go", "keep  [", "merge ["} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout = %q, want it to contain %q", out, want)
		}
	}

	if got := queryInt(t, dbPath, `SELECT count(*) FROM bookmarks WHERE canonical_id IS NOT NULL`); got != 0 {
		t.Errorf("rows with canonical_id = %d, want 0 after --dry-run", got)
	}

	if got := queryInt(t, dbPath, `SELECT count(*) FROM merge_log`); got != 0 {
		t.Errorf("merge_log rows = %d, want 0 after --dry-run", got)
	}
}

func TestDedupe(t *testing.T) {
	t.Parallel()

	dbPath := imported(t)

	out, _, err := run(t, "dedupe", "--database", dbPath)
	if err != nil {
		t.Fatalf("dedupe error = %v, want nil", err)
	}

	if !strings.Contains(out, "Deduplicated 1 group") {
		t.Errorf("stdout = %q, want it to report one deduplicated group", out)
	}

	// The fixture has github.com/golang/go twice, under Bookmarks bar/Dev and Reading List.
	if got := queryInt(t, dbPath, `SELECT count(*) FROM bookmarks WHERE canonical_id IS NOT NULL`); got != 1 {
		t.Errorf("collapsed rows = %d, want 1", got)
	}

	// Nothing is ever deleted.
	if got := queryInt(t, dbPath, `SELECT count(*) FROM bookmarks`); got != 11 {
		t.Errorf("bookmarks rows = %d, want 11; dedupe deleted rows", got)
	}

	if got := queryInt(t, dbPath, `SELECT count(*) FROM merge_log`); got != 1 {
		t.Errorf("merge_log rows = %d, want 1", got)
	}

	t.Run("survivor takes the earliest date", func(t *testing.T) {
		got := queryInt(t, dbPath,
			`SELECT add_date FROM bookmarks WHERE url_norm = ? AND canonical_id IS NULL`,
			"https://github.com/golang/go")

		if got != 1385000100 {
			t.Errorf("survivor add_date = %d, want 1385000100 (the earlier of the two)", got)
		}
	})

	t.Run("survivor takes the union of folder paths", func(t *testing.T) {
		got := queryInt(t, dbPath, `
			SELECT count(*) FROM source_paths
			WHERE bookmark_id = (SELECT id FROM bookmarks WHERE url_norm = ? AND canonical_id IS NULL)`,
			"https://github.com/golang/go")

		if got != 2 {
			t.Errorf("survivor source paths = %d, want 2 (Bookmarks bar/Dev and Reading List)", got)
		}
	})

	t.Run("survivor keeps the longest title", func(t *testing.T) {
		handle, err := sql.Open("sqlite", dbPath)
		if err != nil {
			t.Fatalf("open database: %v", err)
		}
		defer func() { _ = handle.Close() }()

		var title string
		if err := handle.QueryRow(
			`SELECT title FROM bookmarks WHERE url_norm = ? AND canonical_id IS NULL`,
			"https://github.com/golang/go").Scan(&title); err != nil {
			t.Fatalf("read title: %v", err)
		}

		if want := "golang/go: The Go programming language"; title != want {
			t.Errorf("survivor title = %q, want %q", title, want)
		}
	})

	t.Run("a collapsed row keeps its own URL", func(t *testing.T) {
		handle, err := sql.Open("sqlite", dbPath)
		if err != nil {
			t.Fatalf("open database: %v", err)
		}
		defer func() { _ = handle.Close() }()

		var url string
		if err := handle.QueryRow(
			`SELECT url FROM bookmarks WHERE canonical_id IS NOT NULL`).Scan(&url); err != nil {
			t.Fatalf("read url: %v", err)
		}

		if url == "" {
			t.Error("collapsed row has no URL; dedupe must not rewrite it")
		}
	})
}

// TestDedupeIsRerunnable is the section 2 principle: every stage can be re-run without
// repeating or undoing work.
func TestDedupeIsRerunnable(t *testing.T) {
	t.Parallel()

	dbPath := imported(t)

	if _, _, err := run(t, "dedupe", "--database", dbPath); err != nil {
		t.Fatalf("first dedupe error = %v, want nil", err)
	}

	out, _, err := run(t, "dedupe", "--database", dbPath)
	if err != nil {
		t.Fatalf("second dedupe error = %v, want nil", err)
	}

	if !strings.Contains(out, "nothing changed") {
		t.Errorf("stdout = %q, want the second run to report no changes", out)
	}

	if got := queryInt(t, dbPath, `SELECT count(*) FROM bookmarks WHERE canonical_id IS NOT NULL`); got != 1 {
		t.Errorf("collapsed rows = %d, want 1 after a second run", got)
	}

	if got := queryInt(t, dbPath, `SELECT count(*) FROM merge_log`); got != 1 {
		t.Errorf("merge_log rows = %d, want 1 after a second run", got)
	}
}

// TestDedupeFlattensChainsAfterALaterImport is the case canonical_id chains would break: a
// second import carrying an earlier date displaces the previous survivor, and the rows that
// pointed at it must be re-pointed at the new survivor rather than at a row that is now itself
// a loser.
func TestDedupeFlattensChainsAfterALaterImport(t *testing.T) {
	t.Parallel()

	export, dbPath := workspace(t)

	if _, _, err := run(t, "import", "--database", dbPath, export); err != nil {
		t.Fatalf("import error = %v, want nil", err)
	}

	if _, _, err := run(t, "dedupe", "--database", dbPath); err != nil {
		t.Fatalf("dedupe error = %v, want nil", err)
	}

	// A second export of the same links, dated earlier than anything in the first.
	earlier := writeExport(t, `<!DOCTYPE NETSCAPE-Bookmark-file-1>
<TITLE>Bookmarks</TITLE>
<H1>Bookmarks</H1>
<DL><p>
    <DT><H3 ADD_DATE="700000000">Older</H3>
    <DL><p>
        <DT><A HREF="https://github.com/golang/go" ADD_DATE="700000001">golang/go, saved earlier</A>
    </DL><p>
</DL><p>`)

	if _, _, err := run(t, "import", "--database", dbPath, earlier); err != nil {
		t.Fatalf("second import error = %v, want nil", err)
	}

	if _, _, err := run(t, "dedupe", "--database", dbPath); err != nil {
		t.Fatalf("second dedupe error = %v, want nil", err)
	}

	// Exactly one survivor for the key, and it is the earliest-dated row.
	if got := queryInt(t, dbPath,
		`SELECT count(*) FROM bookmarks WHERE url_norm = ? AND canonical_id IS NULL`,
		"https://github.com/golang/go"); got != 1 {
		t.Errorf("survivors for the key = %d, want exactly 1", got)
	}

	if got := queryInt(t, dbPath,
		`SELECT add_date FROM bookmarks WHERE url_norm = ? AND canonical_id IS NULL`,
		"https://github.com/golang/go"); got != 700000001 {
		t.Errorf("survivor add_date = %d, want 700000001 (the newly imported earlier date)", got)
	}

	// No row points at a row that is itself collapsed.
	if got := queryInt(t, dbPath, `
		SELECT count(*) FROM bookmarks loser
		JOIN bookmarks target ON target.id = loser.canonical_id
		WHERE target.canonical_id IS NOT NULL`); got != 0 {
		t.Errorf("rows pointing at a collapsed row = %d, want 0; canonical_id chains were built", got)
	}

	// The union of folder paths spans all three originals.
	if got := queryInt(t, dbPath, `
		SELECT count(*) FROM source_paths
		WHERE bookmark_id = (SELECT id FROM bookmarks WHERE url_norm = ? AND canonical_id IS NULL)`,
		"https://github.com/golang/go"); got != 3 {
		t.Errorf("survivor source paths = %d, want 3 (Bookmarks bar/Dev, Reading List, Older)", got)
	}
}

func TestDedupeOnAnEmptyDatabase(t *testing.T) {
	t.Parallel()

	_, dbPath := workspace(t)

	out, _, err := run(t, "dedupe", "--database", dbPath)
	if err != nil {
		t.Fatalf("dedupe error = %v, want nil", err)
	}

	if !strings.Contains(out, "No duplicates found") {
		t.Errorf("stdout = %q, want it to report no duplicates", out)
	}
}
