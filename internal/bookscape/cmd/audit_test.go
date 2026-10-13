package cmd_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// TestAuditTouchesNothing is the property that makes audit safe to re-run while editing rules:
// it reads the file and the config and writes nothing at all.
func TestAuditTouchesNothing(t *testing.T) {
	t.Parallel()

	export, dbPath := workspace(t)

	out, _, err := run(t, "audit", "--database", dbPath, export)
	if err != nil {
		t.Fatalf("audit error = %v, want nil", err)
	}

	if !strings.Contains(out, "bookmarks        11") {
		t.Errorf("stdout = %q, want the fixture's eleven bookmarks", out)
	}

	// No database is created, even though --database was supplied.
	if _, err := os.Stat(dbPath); err == nil {
		t.Error("audit created a database; it must not open one at all")
	}
}

// TestAuditReportsRuleCoverage is what the subcommand exists for: a free, deterministic answer to
// "how much would the model pass cost me".
func TestAuditReportsRuleCoverage(t *testing.T) {
	t.Parallel()

	export, _ := workspace(t)

	out, _, err := run(t, "audit", export)
	if err != nil {
		t.Fatalf("audit error = %v, want nil", err)
	}

	for _, want := range []string{
		"Rule coverage",
		"resolved by rules",
		"left for the model",
		"Top domains",
		"Saved per year",
		"Suggested rules",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout = %q, want a %q section", out, want)
		}
	}
}

// TestAuditAgreesWithTheRulesPass is the correctness check on the whole feature. If audit's
// number does not match what `classify --pass rules` actually does, the report is worse than
// useless - it is confidently wrong, and someone will tune against it.
func TestAuditAgreesWithTheRulesPass(t *testing.T) {
	t.Parallel()

	export, dbPath := workspace(t)

	auditOut, _, err := run(t, "audit", export)
	if err != nil {
		t.Fatalf("audit error = %v, want nil", err)
	}

	// The real pipeline, including the deduplication audit predicts.
	if _, _, err := run(t, "import", "--database", dbPath, export); err != nil {
		t.Fatalf("import error = %v, want nil", err)
	}

	if _, _, err := run(t, "dedupe", "--database", dbPath); err != nil {
		t.Fatalf("dedupe error = %v, want nil", err)
	}

	classifyOut, _, err := runWithBackend(t, refuseAll(t), "classify", "--pass", "rules", "--database", dbPath)
	if err != nil {
		t.Fatalf("classify error = %v, want nil", err)
	}

	resolved := queryInt(t, dbPath, `SELECT count(*) FROM classifications WHERE source = 'rule'`)
	survivors := queryInt(t, dbPath, `SELECT count(*) FROM bookmarks WHERE canonical_id IS NULL`)

	// Parsed rather than matched as a substring: the report's column widths are presentation
	// and should be free to change without breaking a correctness test.
	if got := reportInt(t, auditOut, `resolved by rules\s+(\d+)`); got != resolved {
		t.Errorf("audit predicted %d links resolved by rules, the pipeline produced %d.\n"+
			"audit said:\n%s\nclassify said:\n%s", got, resolved, auditOut, classifyOut)
	}

	if got := reportInt(t, auditOut, `survivors\s+(\d+)`); got != survivors {
		t.Errorf("audit predicted %d survivors, dedupe produced %d:\n%s", got, survivors, auditOut)
	}
}

// reportInt pulls the first capture group of pattern out of the report as an integer.
func reportInt(t *testing.T, report, pattern string) int {
	t.Helper()

	m := regexp.MustCompile(pattern).FindStringSubmatch(report)
	if m == nil {
		t.Fatalf("pattern %q not found in the report:\n%s", pattern, report)
	}

	n, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("parse %q from the report: %v", m[1], err)
	}

	return n
}

// TestAuditSuggestionsAreValidYAML checks the pasteable block actually parses as the config
// fragment it claims to be, once a collection is filled in.
func TestAuditSuggestionsAreValidYAML(t *testing.T) {
	t.Parallel()

	export, _ := workspace(t)

	out, _, err := run(t, "audit", "--suggest", "3", "--min-links", "1", export)
	if err != nil {
		t.Fatalf("audit error = %v, want nil", err)
	}

	start := strings.Index(out, "    - match_")
	if start < 0 {
		t.Fatalf("no suggestion block in the report:\n%s", out)
	}

	block := out[start:]
	if end := strings.Index(block, "\n\n"); end >= 0 {
		block = block[:end]
	}

	// Fill in the collections the report leaves for the user, then check it parses as a
	// config and validates.
	filled := strings.ReplaceAll(block, "collection: #", "collection: Archive #")

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "bookscape.yaml")
	body := "classify:\n  collections: [Archive]\n  rules:\n" + filled + "\n"

	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	if _, _, err := run(t, "audit", "--config", cfgPath, export); err != nil {
		t.Fatalf("the suggested rules do not form a valid config: %v\n%s", err, body)
	}
}

func TestAuditReportsAMissingFile(t *testing.T) {
	t.Parallel()

	_, _, err := run(t, "audit", filepath.Join(t.TempDir(), "absent.html"))
	if err == nil {
		t.Fatal("audit error = nil, want an error")
	}
}
