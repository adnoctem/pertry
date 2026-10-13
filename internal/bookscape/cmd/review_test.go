package cmd_test

import (
	"database/sql"
	"strconv"
	"strings"
	"testing"
)

// reviewable imports three links, lets the rules classify one, and seeds one low-confidence and
// one confident model classification directly - so the gate has something to block on without
// the model pass being involved.
func reviewable(t *testing.T) string {
	t.Helper()

	export := writeExport(t, `<!DOCTYPE NETSCAPE-Bookmark-file-1>
<TITLE>Bookmarks</TITLE>
<H1>Bookmarks</H1>
<DL><p>
    <DT><A HREF="https://github.com/golang/go" ADD_DATE="1385000100">golang/go</A>
    <DT><A HREF="https://example.org/mystery" ADD_DATE="1385000400">Home</A>
    <DT><A HREF="https://example.net/other" ADD_DATE="1385000500">untitled</A>
</DL><p>`)

	dbPath := strings.TrimSuffix(export, "export.html") + "bookscape.db"

	if _, _, err := run(t, "import", "--database", dbPath, export); err != nil {
		t.Fatalf("import error = %v, want nil", err)
	}

	if _, _, err := runWithBackend(t, refuseAll(t), "classify", "--pass", "rules", "--database", dbPath); err != nil {
		t.Fatalf("rules pass error = %v, want nil", err)
	}

	handle, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}

	if _, err := handle.Exec(`
		INSERT INTO classifications
		    (bookmark_id, collection, description, confidence, source, model, run_id, prompt_hash, created_at)
		SELECT id, 'Archive', 'An unclear page.', 0.31, 'model', 'test-model', 1, 'hash', 0
		FROM bookmarks WHERE url LIKE '%mystery%'
		UNION ALL
		SELECT id, 'Reference', 'A reference page.', 0.88, 'model', 'test-model', 1, 'hash', 0
		FROM bookmarks WHERE url LIKE '%other%'`); err != nil {
		t.Fatalf("seed classifications: %v", err)
	}

	if err := handle.Close(); err != nil {
		t.Fatalf("close database: %v", err)
	}

	return dbPath
}

// TestReviewShowsOnlyWhatBlocksTheExport is the default scope: the point of the gate is the
// low-confidence rows, not a dump of the whole corpus.
func TestReviewShowsOnlyWhatBlocksTheExport(t *testing.T) {
	t.Parallel()

	dbPath := reviewable(t)

	out, _, err := run(t, "review", "--database", dbPath)
	if err != nil {
		t.Fatalf("review error = %v, want nil", err)
	}

	if !strings.Contains(out, "example.org/mystery") {
		t.Errorf("stdout = %q, want the low-confidence link", out)
	}

	for _, unwanted := range []string{"example.net/other", "github.com/golang/go"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("stdout = %q, want it to omit %q, which is not blocking the export", out, unwanted)
		}
	}

	if !strings.Contains(out, "will not be exported until confirmed") {
		t.Errorf("stdout = %q, want it to say the export is blocked", out)
	}
}

func TestReviewOnlyAll(t *testing.T) {
	t.Parallel()

	dbPath := reviewable(t)

	out, _, err := run(t, "review", "--only", "all", "--database", dbPath)
	if err != nil {
		t.Fatalf("review error = %v, want nil", err)
	}

	for _, want := range []string{"example.org/mystery", "example.net/other", "github.com/golang/go"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout = %q, want it to include %q", out, want)
		}
	}
}

func TestReviewSampleLimitsTheTable(t *testing.T) {
	t.Parallel()

	dbPath := reviewable(t)

	out, _, err := run(t, "review", "--only", "all", "--sample", "1", "--database", dbPath)
	if err != nil {
		t.Fatalf("review error = %v, want nil", err)
	}

	// Ordered by confidence ascending, so a sample of one is the worst classification.
	if !strings.Contains(out, "example.org/mystery") {
		t.Errorf("stdout = %q, want the lowest-confidence link", out)
	}

	if strings.Contains(out, "example.net/other") {
		t.Errorf("stdout = %q, want only one row", out)
	}
}

func TestReviewRejectsAnUnknownFilter(t *testing.T) {
	t.Parallel()

	dbPath := reviewable(t)

	_, _, err := run(t, "review", "--only", "whatever", "--database", dbPath)
	if err == nil {
		t.Fatal("review error = nil, want a usage error")
	}

	if !strings.Contains(err.Error(), "whatever") {
		t.Errorf("review error = %q, want it to name the offending value", err)
	}
}

func TestReviewSet(t *testing.T) {
	t.Parallel()

	dbPath := reviewable(t)

	id := queryInt(t, dbPath, `SELECT id FROM bookmarks WHERE url LIKE '%mystery%'`)

	out, _, err := run(t, "review", "set", strconv.Itoa(id), "collection=Reference", "tags=unclear,todo", "--database", dbPath)
	if err != nil {
		t.Fatalf("review set error = %v, want nil", err)
	}

	if !strings.Contains(out, "Nothing is blocking the export") {
		t.Errorf("stdout = %q, want the gate to have cleared", out)
	}

	t.Run("the decision is authoritative", func(t *testing.T) {
		if got := queryString(t, dbPath,
			`SELECT source FROM classifications WHERE bookmark_id = ?`, id); got != "manual" {
			t.Errorf("source = %q, want %q", got, "manual")
		}

		if got := queryString(t, dbPath,
			`SELECT collection FROM classifications WHERE bookmark_id = ?`, id); got != "Reference" {
			t.Errorf("collection = %q, want %q", got, "Reference")
		}
	})

	t.Run("an unmentioned field is left alone", func(t *testing.T) {
		// Correcting the collection must not wipe a generated description that was fine.
		got := queryString(t, dbPath, `SELECT description FROM classifications WHERE bookmark_id = ?`, id)
		if got != "An unclear page." {
			t.Errorf("description = %q, want the original to survive", got)
		}
	})

	t.Run("manual tags are recorded with their provenance", func(t *testing.T) {
		got := queryInt(t, dbPath,
			`SELECT count(*) FROM bookmark_tags WHERE bookmark_id = ? AND source = 'manual'`, id)

		if got != 2 {
			t.Errorf("manual tags = %d, want 2", got)
		}
	})
}

// TestReviewSetRefusesAnUnconfiguredCollection is the one mistake worth refusing: Linkwarden
// creates collections as a side effect of importing, so a typo here becomes a stray collection
// over there.
func TestReviewSetRefusesAnUnconfiguredCollection(t *testing.T) {
	t.Parallel()

	dbPath := reviewable(t)

	id := queryInt(t, dbPath, `SELECT id FROM bookmarks WHERE url LIKE '%mystery%'`)

	_, _, err := run(t, "review", "set", strconv.Itoa(id), "collection=Nonsense", "--database", dbPath)
	if err == nil {
		t.Fatal("review set error = nil, want a refusal")
	}

	if !strings.Contains(err.Error(), "classify.collections") {
		t.Errorf("review set error = %q, want it to point at the configured set", err)
	}

	if got := queryString(t, dbPath,
		`SELECT collection FROM classifications WHERE bookmark_id = ?`, id); got != "Archive" {
		t.Errorf("collection = %q, want the original to be untouched", got)
	}
}

func TestReviewSetRejectsBadArguments(t *testing.T) {
	t.Parallel()

	dbPath := reviewable(t)

	tests := map[string][]string{
		"not an id":         {"review", "set", "banana", "collection=Reference"},
		"not an assignment": {"review", "set", "1", "collection"},
		"unknown key":       {"review", "set", "1", "colour=blue"},
	}

	for name, args := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if _, _, err := run(t, append(args, "--database", dbPath)...); err == nil {
				t.Error("error = nil, want a usage error")
			}
		})
	}
}

// TestReviewAccept is what clears the gate when the classification was right all along.
func TestReviewAccept(t *testing.T) {
	t.Parallel()

	dbPath := reviewable(t)

	id := queryInt(t, dbPath, `SELECT id FROM bookmarks WHERE url LIKE '%mystery%'`)

	out, _, err := run(t, "review", "accept", strconv.Itoa(id), "--database", dbPath)
	if err != nil {
		t.Fatalf("review accept error = %v, want nil", err)
	}

	if !strings.Contains(out, "Accepted 1 classification") {
		t.Errorf("stdout = %q, want it to report one acceptance", out)
	}

	if got := queryString(t, dbPath,
		`SELECT source FROM classifications WHERE bookmark_id = ?`, id); got != "manual" {
		t.Errorf("source = %q, want %q", got, "manual")
	}

	// Accepting confirms; it does not rewrite.
	if got := queryString(t, dbPath,
		`SELECT collection FROM classifications WHERE bookmark_id = ?`, id); got != "Archive" {
		t.Errorf("collection = %q, want the model's value to be preserved", got)
	}
}

func TestReviewAcceptAll(t *testing.T) {
	t.Parallel()

	dbPath := reviewable(t)

	out, _, err := run(t, "review", "accept", "--all", "--database", dbPath)
	if err != nil {
		t.Fatalf("review accept --all error = %v, want nil", err)
	}

	if !strings.Contains(out, "Nothing is blocking the export") {
		t.Errorf("stdout = %q, want the gate to have cleared", out)
	}

	// Only the below-threshold row is promoted; the confident one stays a model result.
	if got := queryInt(t, dbPath, `SELECT count(*) FROM classifications WHERE source = 'manual'`); got != 1 {
		t.Errorf("manual classifications = %d, want 1", got)
	}

	if got := queryInt(t, dbPath, `SELECT count(*) FROM classifications WHERE source = 'model'`); got != 1 {
		t.Errorf("model classifications = %d, want 1; a confident result needs no acceptance", got)
	}
}

func TestReviewAcceptRejectsBadArguments(t *testing.T) {
	t.Parallel()

	dbPath := reviewable(t)

	tests := map[string][]string{
		"nothing to accept": {"review", "accept"},
		"both all and ids":  {"review", "accept", "--all", "1"},
		"not an id":         {"review", "accept", "banana"},
	}

	for name, args := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if _, _, err := run(t, append(args, "--database", dbPath)...); err == nil {
				t.Error("error = nil, want a usage error")
			}
		})
	}
}
