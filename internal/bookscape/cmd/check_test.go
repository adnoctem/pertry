package cmd_test

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// checkWorkspace imports an export whose links all point at a local test server, so that
// `check` has something real to probe without touching the network.
func checkWorkspace(t *testing.T) (dbPath string, cfgPath string) {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/alive":
			w.WriteHeader(http.StatusOK)
		case "/gone":
			w.WriteHeader(http.StatusGone)
		case "/forbidden":
			w.WriteHeader(http.StatusForbidden)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	export := filepath.Join(dir, "export.html")

	body := `<!DOCTYPE NETSCAPE-Bookmark-file-1>
<TITLE>Bookmarks</TITLE>
<H1>Bookmarks</H1>
<DL><p>
    <DT><A HREF="` + srv.URL + `/alive" ADD_DATE="1374257900">alive</A>
    <DT><A HREF="` + srv.URL + `/gone" ADD_DATE="1374257901">gone</A>
    <DT><A HREF="` + srv.URL + `/forbidden" ADD_DATE="1374257902">forbidden</A>
    <DT><A HREF="javascript:void(0)" ADD_DATE="1374257903">not fetchable</A>
</DL><p>`

	if err := os.WriteFile(export, []byte(body), 0o600); err != nil {
		t.Fatalf("write export: %v", err)
	}

	// Keep the check budget small: these are loopback requests, not a real corpus.
	cfgPath = filepath.Join(dir, "bookscape.yaml")
	if err := os.WriteFile(cfgPath, []byte(
		"check:\n  concurrency: 2\n  per_host_concurrency: 2\n  timeout: 2s\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	dbPath = filepath.Join(dir, "bookscape.db")

	if _, _, err := run(t, "import", "--config", cfgPath, "--database", dbPath, export); err != nil {
		t.Fatalf("import error = %v, want nil", err)
	}

	return dbPath, cfgPath
}

func queryString(t *testing.T, dbPath, query string, args ...any) string {
	t.Helper()

	handle, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer func() { _ = handle.Close() }()

	var s string
	if err := handle.QueryRow(query, args...).Scan(&s); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}

	return s
}

func TestCheck(t *testing.T) {
	t.Parallel()

	dbPath, cfgPath := checkWorkspace(t)

	out, _, err := run(t, "check", "--config", cfgPath, "--database", dbPath)
	if err != nil {
		t.Fatalf("check error = %v, want nil", err)
	}

	if !strings.Contains(out, "Checked 4 links") {
		t.Errorf("stdout = %q, want it to report four checked links", out)
	}

	counts := map[string]int{"alive": 1, "dead": 1, "uncertain": 1, "skipped": 1}
	for status, want := range counts {
		got := queryInt(t, dbPath, `SELECT count(*) FROM liveness WHERE status = ?`, status)
		if got != want {
			t.Errorf("liveness rows with status %q = %d, want %d", status, got, want)
		}
	}

	t.Run("a forbidden response records its HTTP status", func(t *testing.T) {
		got := queryInt(t, dbPath, `
			SELECT http_status FROM liveness
			WHERE bookmark_id = (SELECT id FROM bookmarks WHERE url LIKE '%/forbidden')`)

		if got != http.StatusForbidden {
			t.Errorf("http_status = %d, want %d", got, http.StatusForbidden)
		}
	})

	t.Run("an unfetchable URL records why", func(t *testing.T) {
		got := queryString(t, dbPath, `
			SELECT error FROM liveness
			WHERE bookmark_id = (SELECT id FROM bookmarks WHERE url LIKE 'javascript:%')`)

		if !strings.Contains(got, "javascript") {
			t.Errorf("error = %q, want it to name the unfetchable scheme", got)
		}
	})
}

// TestCheckIsResumable is the property section 5.4 claims: the default scope covers only links
// with no result, so re-running does not re-probe everything.
func TestCheckIsResumable(t *testing.T) {
	t.Parallel()

	dbPath, cfgPath := checkWorkspace(t)

	if _, _, err := run(t, "check", "--config", cfgPath, "--database", dbPath); err != nil {
		t.Fatalf("first check error = %v, want nil", err)
	}

	before := queryString(t, dbPath, `SELECT max(checked_at) FROM liveness`)

	out, _, err := run(t, "check", "--config", cfgPath, "--database", dbPath)
	if err != nil {
		t.Fatalf("second check error = %v, want nil", err)
	}

	if !strings.Contains(out, "Nothing to check") {
		t.Errorf("stdout = %q, want the second run to find nothing unchecked", out)
	}

	// The summary of the whole corpus is the useful thing to print when the scope is empty.
	if !strings.Contains(out, "Corpus state across 4 surviving links") {
		t.Errorf("stdout = %q, want a corpus summary", out)
	}

	if after := queryString(t, dbPath, `SELECT max(checked_at) FROM liveness`); after != before {
		t.Error("the second run rewrote results that were already recorded")
	}
}

// TestCheckOnlyUncertain is the re-run that matters later: a cleared rate limit or a recovered
// server shows up here, without re-probing the whole corpus.
func TestCheckOnlyUncertain(t *testing.T) {
	t.Parallel()

	dbPath, cfgPath := checkWorkspace(t)

	if _, _, err := run(t, "check", "--config", cfgPath, "--database", dbPath); err != nil {
		t.Fatalf("check error = %v, want nil", err)
	}

	out, _, err := run(t, "check", "--config", cfgPath, "--database", dbPath, "--only", "uncertain")
	if err != nil {
		t.Fatalf("check --only=uncertain error = %v, want nil", err)
	}

	// Exactly the one forbidden link, not the alive, dead or skipped ones.
	if !strings.Contains(out, "Checked 1 link") {
		t.Errorf("stdout = %q, want it to re-probe only the single uncertain link", out)
	}
}

func TestCheckRejectsAnUnknownScope(t *testing.T) {
	t.Parallel()

	dbPath, cfgPath := checkWorkspace(t)

	_, _, err := run(t, "check", "--config", cfgPath, "--database", dbPath, "--only", "maybe")
	if err == nil {
		t.Fatal("check error = nil, want a usage error")
	}

	for _, want := range []string{"maybe", "unchecked"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("check error = %q, want it to mention %q", err, want)
		}
	}
}

// TestCheckSkipsCollapsedRows is why the target query filters on canonical_id: a row dedupe
// collapsed will never be exported, so spending a request on it is waste.
func TestCheckSkipsCollapsedRows(t *testing.T) {
	t.Parallel()

	dbPath := imported(t)

	if _, _, err := run(t, "dedupe", "--database", dbPath); err != nil {
		t.Fatalf("dedupe error = %v, want nil", err)
	}

	// Mark everything checked so that the only question is how many targets the scope sees.
	handle, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}

	if _, err := handle.Exec(`
		INSERT INTO liveness (bookmark_id, status, checked_at)
		SELECT id, 'alive', 0 FROM bookmarks`); err != nil {
		t.Fatalf("seed liveness: %v", err)
	}

	if err := handle.Close(); err != nil {
		t.Fatalf("close database: %v", err)
	}

	out, _, err := run(t, "check", "--database", dbPath, "--only", "alive")
	if err != nil {
		t.Fatalf("check error = %v, want nil", err)
	}

	// 11 rows, 1 collapsed by dedupe, so 10 survivors.
	if !strings.Contains(out, "Checked 10 links") {
		t.Errorf("stdout = %q, want it to probe the 10 survivors and skip the collapsed row", out)
	}
}

func TestCheckOnAnEmptyDatabase(t *testing.T) {
	t.Parallel()

	_, dbPath := workspace(t)

	out, _, err := run(t, "check", "--database", dbPath)
	if err != nil {
		t.Fatalf("check error = %v, want nil", err)
	}

	if !strings.Contains(out, "no bookmarks yet") {
		t.Errorf("stdout = %q, want it to point at import", out)
	}
}
