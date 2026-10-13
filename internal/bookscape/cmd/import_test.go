package cmd_test

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/adnoctem/pertry/internal/bookscape/cmd"
)

// run executes the root command with args and returns stdout, stderr and the error.
func run(t *testing.T, args ...string) (string, string, error) {
	t.Helper()

	var out, errOut bytes.Buffer

	app := &cmd.App{Out: &out}
	root := cmd.NewRootCommand(app)
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs(args)

	err := root.ExecuteContext(context.Background())

	return out.String(), errOut.String(), err
}

// workspace returns a temp directory holding a copy of the fixture export and a database path.
func workspace(t *testing.T) (export, db string) {
	t.Helper()

	dir := t.TempDir()

	raw, err := os.ReadFile(filepath.Join("testdata", "chrome_export.html"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	export = filepath.Join(dir, "bookmarks.html")
	if err := os.WriteFile(export, raw, 0o600); err != nil {
		t.Fatalf("write export: %v", err)
	}

	return export, filepath.Join(dir, "bookscape.db")
}

// writeExport writes a bookmark export into a fresh temp directory and returns its path.
func writeExport(t *testing.T, body string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "export.html")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write export: %v", err)
	}

	return path
}

func countRows(t *testing.T, dbPath, table string) int {
	t.Helper()

	handle, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer func() { _ = handle.Close() }()

	var n int
	if err := handle.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}

	return n
}

func TestImport(t *testing.T) {
	t.Parallel()

	export, dbPath := workspace(t)

	out, _, err := run(t, "import", "--database", dbPath, export)
	if err != nil {
		t.Fatalf("import error = %v, want nil", err)
	}

	if !strings.Contains(out, "Imported 11 bookmarks") {
		t.Errorf("stdout = %q, want it to report 11 imported bookmarks", out)
	}

	// The one fixture entry with no ADD_DATE is surfaced, in the singular.
	if !strings.Contains(out, "1 entry carried no ADD_DATE") {
		t.Errorf("stdout = %q, want it to report the single undated entry", out)
	}

	if got := countRows(t, dbPath, "bookmarks"); got != 11 {
		t.Errorf("bookmarks rows = %d, want 11", got)
	}

	if got := countRows(t, dbPath, "source_paths"); got != 11 {
		t.Errorf("source_paths rows = %d, want 11", got)
	}
}

// TestImportIsIdempotent is the property section 9 claims for this subcommand.
func TestImportIsIdempotent(t *testing.T) {
	t.Parallel()

	export, dbPath := workspace(t)

	if _, _, err := run(t, "import", "--database", dbPath, export); err != nil {
		t.Fatalf("first import error = %v, want nil", err)
	}

	out, _, err := run(t, "import", "--database", dbPath, export)
	if err != nil {
		t.Fatalf("second import error = %v, want nil", err)
	}

	if !strings.Contains(out, "Already imported") {
		t.Errorf("stdout = %q, want it to report the file as already imported", out)
	}

	if got := countRows(t, dbPath, "bookmarks"); got != 11 {
		t.Errorf("bookmarks rows = %d, want 11; the second import duplicated rows", got)
	}

	if got := countRows(t, dbPath, "import_batches"); got != 1 {
		t.Errorf("import_batches rows = %d, want 1", got)
	}
}

// TestImportRefusesDroppedEntries is the check that exists because the parser drops
// case-mismatched anchors silently. Importing 1 of 2 entries without complaint would lose data
// with no trace.
func TestImportRefusesDroppedEntries(t *testing.T) {
	t.Parallel()

	export := writeExport(t, `<!DOCTYPE NETSCAPE-Bookmark-file-1>
<TITLE>Bookmarks</TITLE>
<H1>Bookmarks</H1>
<DL><p>
    <DT><A HREF="https://example.com/upper" ADD_DATE="1374257900">kept</A>
    <DT><a href="https://example.com/lower" add_date="1374257901">dropped</a>
</DL><p>`)

	_, _, err := run(t, "import", "--database", filepath.Join(t.TempDir(), "b.db"), export)
	if err == nil {
		t.Fatal("import error = nil, want a refusal")
	}

	for _, want := range []string{"2 anchor tags", "1 were dropped", "case-sensitively"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("import error = %q, want it to mention %q", err, want)
		}
	}
}

// TestImportRefusesImplausibleDates is the other silent failure: a WebKit-epoch timestamp is
// rescaled into 1970 rather than rejected by the library.
func TestImportRefusesImplausibleDates(t *testing.T) {
	t.Parallel()

	export := writeExport(t, `<!DOCTYPE NETSCAPE-Bookmark-file-1>
<TITLE>Bookmarks</TITLE>
<H1>Bookmarks</H1>
<DL><p>
    <DT><A HREF="https://example.com/x" ADD_DATE="13244000000000000">x</A>
</DL><p>`)

	_, _, err := run(t, "import", "--database", filepath.Join(t.TempDir(), "b.db"), export)
	if err == nil {
		t.Fatal("import error = nil, want a refusal")
	}

	if !strings.Contains(err.Error(), "implausible dates") {
		t.Errorf("import error = %q, want it to mention implausible dates", err)
	}
}

func TestImportReportsAMissingFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	_, _, err := run(t, "import", "--database", filepath.Join(dir, "b.db"), filepath.Join(dir, "absent.html"))
	if err == nil {
		t.Fatal("import error = nil, want an error")
	}

	// The hint is the useful half for a user who has not exported anything yet.
	if !strings.Contains(err.Error(), "Export bookmarks to HTML") {
		t.Errorf("import error = %q, want it to say how to produce an export", err)
	}
}

func TestImportRejectsBadLogLevel(t *testing.T) {
	t.Parallel()

	export, dbPath := workspace(t)

	_, _, err := run(t, "import", "--log-level", "chatty", "--database", dbPath, export)
	if err == nil {
		t.Fatal("import error = nil, want a usage error")
	}

	if !strings.Contains(err.Error(), "--log-level") {
		t.Errorf("import error = %q, want it to name the offending flag", err)
	}
}

func TestImportRejectsAMissingNamedConfig(t *testing.T) {
	t.Parallel()

	export, dbPath := workspace(t)

	_, _, err := run(t, "import", "--config", filepath.Join(t.TempDir(), "absent.yaml"),
		"--database", dbPath, export)
	if err == nil {
		t.Fatal("import error = nil, want an error for a config file the user named but does not exist")
	}
}

// TestImportUsesTheConfiguredNormalisePolicy proves the policy is actually wired through to
// url_norm rather than the defaults being hardcoded.
func TestImportUsesTheConfiguredNormalisePolicy(t *testing.T) {
	t.Parallel()

	export, dbPath := workspace(t)

	cfgPath := filepath.Join(filepath.Dir(dbPath), "bookscape.yaml")
	if err := os.WriteFile(cfgPath, []byte("normalize:\n  strip_www: false\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	if _, _, err := run(t, "import", "--config", cfgPath, "--database", dbPath, export); err != nil {
		t.Fatalf("import error = %v, want nil", err)
	}

	handle, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer func() { _ = handle.Close() }()

	var norm string
	if err := handle.QueryRow(
		`SELECT url_norm FROM bookmarks WHERE url = ?`, "https://www.linkedin.com/feed/").Scan(&norm); err != nil {
		t.Fatalf("read url_norm: %v", err)
	}

	if want := "https://www.linkedin.com/feed"; norm != want {
		t.Errorf("url_norm = %q, want %q (strip_www: false was not honoured)", norm, want)
	}
}
