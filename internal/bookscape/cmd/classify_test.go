package cmd_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/adnoctem/pertry/internal/bookscape/classify"
	"github.com/adnoctem/pertry/internal/bookscape/cmd"
	"github.com/adnoctem/pertry/pkg/infer"
)

// runWithBackend is run() with a model-pass backend injected. The real backend is never
// constructed here: a test that could spend tokens by accident is a trap.
func runWithBackend(t *testing.T, backend infer.Completer, args ...string) (string, string, error) {
	t.Helper()

	var out, errOut bytes.Buffer

	app := &cmd.App{Out: &out, ClassifyBackend: backend}
	root := cmd.NewRootCommand(app)
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs(args)

	err := root.ExecuteContext(context.Background())

	return out.String(), errOut.String(), err
}

// answerWith builds an infer.Completer that answers in terms of classify.Result, going through
// the real wire format: the items are decoded out of the prompt and the results are returned as
// the {"results": …} envelope the schema describes.
func answerWith(answer func(items []classify.Item) ([]classify.Result, error)) infer.Completer {
	return &infer.Fake{CompleteFunc: func(req infer.Request) (infer.Response, error) {
		_, encoded, ok := strings.Cut(req.Prompt, "\n\n")
		if !ok {
			return infer.Response{}, fmt.Errorf("prompt carries no JSON payload: %.80s", req.Prompt)
		}

		var items []classify.Item
		if err := json.Unmarshal([]byte(encoded), &items); err != nil {
			return infer.Response{}, fmt.Errorf("decode items from the prompt: %w", err)
		}

		results, err := answer(items)
		if err != nil {
			return infer.Response{}, err
		}

		body, err := json.Marshal(map[string]any{"results": results})
		if err != nil {
			return infer.Response{}, err
		}

		return infer.Response{JSON: body, Model: req.Model}, nil
	}}
}

// refuseAll is the backend every test that must not reach the model uses. Nothing in this suite
// may touch a paid API, so the backend that would be used instead fails the test if called.
func refuseAll(t *testing.T) infer.Completer {
	t.Helper()

	return &infer.Fake{CompleteFunc: func(infer.Request) (infer.Response, error) {
		t.Error("the model pass was reached when it should not have been")

		return infer.Response{}, errors.New("the model pass must not be reached in this test")
	}}
}

// classifiable imports an export whose links exercise both passes: two that the default rules
// resolve by domain, one that a folder rule resolves, and one that nothing matches.
func classifiable(t *testing.T) string {
	t.Helper()

	export := writeExport(t, `<!DOCTYPE NETSCAPE-Bookmark-file-1>
<TITLE>Bookmarks</TITLE>
<H1>Bookmarks</H1>
<DL><p>
    <DT><A HREF="https://github.com/golang/go" ADD_DATE="1385000100">golang/go</A>
    <DT><A HREF="https://stackoverflow.com/questions/1" ADD_DATE="1385000200">a question</A>
    <DT><H3 ADD_DATE="1385000000">Dev</H3>
    <DL><p>
        <DT><A HREF="https://example.com/some-tool" ADD_DATE="1385000300">a tool</A>
    </DL><p>
    <DT><A HREF="https://example.org/mystery" ADD_DATE="1385000400">Home</A>
</DL><p>`)

	dbPath := filepath.Join(filepath.Dir(export), "bookscape.db")

	if _, _, err := run(t, "import", "--database", dbPath, export); err != nil {
		t.Fatalf("import error = %v, want nil", err)
	}

	return dbPath
}

func TestClassifyRulesPass(t *testing.T) {
	t.Parallel()

	dbPath := classifiable(t)

	out, _, err := runWithBackend(t, refuseAll(t), "classify", "--pass", "rules", "--database", dbPath)
	if err != nil {
		t.Fatalf("classify --pass=rules error = %v, want nil", err)
	}

	// github.com and stackoverflow.com by domain, example.com by its Dev folder.
	if !strings.Contains(out, "Rules pass: classified 3 of 4 bookmarks considered") {
		t.Errorf("stdout = %q, want three of four classified by rules", out)
	}

	if got := queryInt(t, dbPath, `SELECT count(*) FROM classifications WHERE source = 'rule'`); got != 3 {
		t.Errorf("rule classifications = %d, want 3", got)
	}

	t.Run("collections come from the configured set", func(t *testing.T) {
		got := queryString(t, dbPath, `
			SELECT collection FROM classifications
			WHERE bookmark_id = (SELECT id FROM bookmarks WHERE url LIKE '%github.com%')`)

		if got != "Development" {
			t.Errorf("collection = %q, want %q", got, "Development")
		}
	})

	t.Run("rule tags are recorded with their provenance", func(t *testing.T) {
		got := queryString(t, dbPath, `
			SELECT t.name FROM bookmark_tags bt
			JOIN tags t ON t.id = bt.tag_id
			WHERE bt.source = 'rule'
			  AND bt.bookmark_id = (SELECT id FROM bookmarks WHERE url LIKE '%github.com%')`)

		if got != "repo" {
			t.Errorf("tag = %q, want %q", got, "repo")
		}
	})

	t.Run("a rule result is fully confident", func(t *testing.T) {
		// A rule either matched or it did not; there is nothing to be partially sure of.
		handle, err := sql.Open("sqlite", dbPath)
		if err != nil {
			t.Fatalf("open database: %v", err)
		}
		defer func() { _ = handle.Close() }()

		var confidence float64
		if err := handle.QueryRow(
			`SELECT confidence FROM classifications WHERE source = 'rule' LIMIT 1`).Scan(&confidence); err != nil {
			t.Fatalf("read confidence: %v", err)
		}

		if confidence != 1 {
			t.Errorf("confidence = %v, want 1", confidence)
		}
	})
}

// TestClassifyRulesPassPicksUpEditedRules is why the rules scope includes rows a rule already
// classified: editing bookscape.yaml and re-running has to take effect.
func TestClassifyRulesPassPicksUpEditedRules(t *testing.T) {
	t.Parallel()

	dbPath := classifiable(t)

	if _, _, err := runWithBackend(t, refuseAll(t), "classify", "--pass", "rules", "--database", dbPath); err != nil {
		t.Fatalf("first rules pass error = %v, want nil", err)
	}

	cfgPath := filepath.Join(filepath.Dir(dbPath), "bookscape.yaml")
	if err := os.WriteFile(cfgPath, []byte(`
classify:
  collections: [Development, Infrastructure, Archive]
  rules:
    - match_domain: [github.com]
      collection: Archive
      tags: [rewritten]
`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	if _, _, err := runWithBackend(t, refuseAll(t),
		"classify", "--pass", "rules", "--config", cfgPath, "--database", dbPath); err != nil {
		t.Fatalf("second rules pass error = %v, want nil", err)
	}

	got := queryString(t, dbPath, `
		SELECT collection FROM classifications
		WHERE bookmark_id = (SELECT id FROM bookmarks WHERE url LIKE '%github.com%')`)

	if got != "Archive" {
		t.Errorf("collection = %q, want %q after the rules were edited", got, "Archive")
	}
}

// TestClassifyRulesPassLeavesManualAlone is the precedence rule: a human decided that, and a
// later pass has no business undoing it.
func TestClassifyRulesPassLeavesManualAlone(t *testing.T) {
	t.Parallel()

	dbPath := classifiable(t)

	handle, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}

	if _, err := handle.Exec(`
		INSERT INTO classifications (bookmark_id, collection, source, confidence, created_at)
		SELECT id, 'Finance', 'manual', 1, 0 FROM bookmarks WHERE url LIKE '%github.com%'`); err != nil {
		t.Fatalf("seed manual classification: %v", err)
	}

	if err := handle.Close(); err != nil {
		t.Fatalf("close database: %v", err)
	}

	if _, _, err := runWithBackend(t, refuseAll(t), "classify", "--pass", "rules", "--database", dbPath); err != nil {
		t.Fatalf("rules pass error = %v, want nil", err)
	}

	got := queryString(t, dbPath, `
		SELECT collection FROM classifications
		WHERE bookmark_id = (SELECT id FROM bookmarks WHERE url LIKE '%github.com%')`)

	if got != "Finance" {
		t.Errorf("collection = %q, want the manual %q to survive the rules pass", got, "Finance")
	}
}

func TestClassifyModelPass(t *testing.T) {
	t.Parallel()

	dbPath := classifiable(t)

	backend := answerWith(func(items []classify.Item) ([]classify.Result, error) {
		results := make([]classify.Result, 0, len(items))
		for _, item := range items {
			results = append(results, classify.Result{
				ID:          item.ID,
				Collection:  "Reference",
				Description: "a generated description",
				Tags:        []string{"generated"},
				Confidence:  0.9,
			})
		}

		return results, nil
	})

	out, _, err := runWithBackend(t, backend, "classify", "--database", dbPath)
	if err != nil {
		t.Fatalf("classify error = %v, want nil", err)
	}

	// The rules resolved three, so exactly one reaches the model.
	if !strings.Contains(out, "Model pass: classified 1 bookmark") {
		t.Errorf("stdout = %q, want one bookmark classified by the model", out)
	}

	if got := queryInt(t, dbPath, `SELECT count(*) FROM classifications WHERE source = 'model'`); got != 1 {
		t.Errorf("model classifications = %d, want 1", got)
	}

	t.Run("provenance is persisted", func(t *testing.T) {
		handle, err := sql.Open("sqlite", dbPath)
		if err != nil {
			t.Fatalf("open database: %v", err)
		}
		defer func() { _ = handle.Close() }()

		var (
			model      string
			runID      int64
			promptHash string
		)

		if err := handle.QueryRow(
			`SELECT model, run_id, prompt_hash FROM classifications WHERE source = 'model'`).
			Scan(&model, &runID, &promptHash); err != nil {
			t.Fatalf("read provenance: %v", err)
		}

		if model == "" || runID == 0 || promptHash == "" {
			t.Errorf("provenance = (%q, %d, %q), want all three populated", model, runID, promptHash)
		}
	})

	t.Run("the generated description is stored", func(t *testing.T) {
		got := queryString(t, dbPath,
			`SELECT description FROM classifications WHERE source = 'model'`)

		if got != "a generated description" {
			t.Errorf("description = %q, want the generated one", got)
		}
	})
}

// TestClassifyDryRunSendsNothing is the guard on a pass that costs money.
func TestClassifyDryRunSendsNothing(t *testing.T) {
	t.Parallel()

	dbPath := classifiable(t)

	out, _, err := runWithBackend(t, refuseAll(t), "classify", "--dry-run", "--database", dbPath)
	if err != nil {
		t.Fatalf("classify --dry-run error = %v, want nil", err)
	}

	if !strings.Contains(out, "Nothing was sent and nothing was written") {
		t.Errorf("stdout = %q, want it to say nothing was sent", out)
	}

	if got := queryInt(t, dbPath, `SELECT count(*) FROM classifications`); got != 0 {
		t.Errorf("classifications = %d, want 0 after --dry-run", got)
	}
}

func TestClassifyModelPassRespectsLimit(t *testing.T) {
	t.Parallel()

	dbPath := classifiable(t)

	// Distinct ids rather than a running total: a backend that answers nothing gets its
	// items re-queued once, so the same two links are legitimately sent twice.
	seen := map[int64]struct{}{}

	backend := answerWith(func(items []classify.Item) ([]classify.Result, error) {
		for _, item := range items {
			seen[item.ID] = struct{}{}
		}

		return nil, nil
	})

	// Skip the rules pass so that every one of the four links is a model candidate.
	if _, _, err := runWithBackend(t, backend,
		"classify", "--pass", "model", "--limit", "2", "--database", dbPath); err != nil {
		t.Fatalf("classify error = %v, want nil", err)
	}

	if len(seen) != 2 {
		t.Errorf("distinct bookmarks sent = %d, want 2", len(seen))
	}
}

// TestClassifyModelPassRejectsAnInventedCollection is section 5.5's validation step, end to
// end: nothing outside the configured set reaches the database.
func TestClassifyModelPassRejectsAnInventedCollection(t *testing.T) {
	t.Parallel()

	dbPath := classifiable(t)

	backend := answerWith(func(items []classify.Item) ([]classify.Result, error) {
		results := make([]classify.Result, 0, len(items))
		for _, item := range items {
			results = append(results, classify.Result{
				ID: item.ID, Collection: "Something New", Confidence: 0.99,
			})
		}

		return results, nil
	})

	out, _, err := runWithBackend(t, backend, "classify", "--pass", "model", "--database", dbPath)
	if err != nil {
		t.Fatalf("classify error = %v, want nil", err)
	}

	if !strings.Contains(out, "outside the configured set") {
		t.Errorf("stdout = %q, want it to report the rejection", out)
	}

	if got := queryInt(t, dbPath, `SELECT count(*) FROM classifications`); got != 0 {
		t.Errorf("classifications = %d, want 0; an invented collection must not be stored", got)
	}
}

// TestClassifyModelPassSurfacesLowConfidence is what tells the user review is needed.
func TestClassifyModelPassSurfacesLowConfidence(t *testing.T) {
	t.Parallel()

	dbPath := classifiable(t)

	backend := answerWith(func(items []classify.Item) ([]classify.Result, error) {
		results := make([]classify.Result, 0, len(items))
		for _, item := range items {
			results = append(results, classify.Result{
				ID: item.ID, Collection: "Archive", Confidence: 0.2,
			})
		}

		return results, nil
	})

	out, _, err := runWithBackend(t, backend, "classify", "--pass", "model", "--database", dbPath)
	if err != nil {
		t.Fatalf("classify error = %v, want nil", err)
	}

	if !strings.Contains(out, "need review before export") {
		t.Errorf("stdout = %q, want it to flag the low-confidence results", out)
	}
}

func TestClassifyRejectsAnUnknownPass(t *testing.T) {
	t.Parallel()

	dbPath := classifiable(t)

	_, _, err := runWithBackend(t, refuseAll(t), "classify", "--pass", "vibes", "--database", dbPath)
	if err == nil {
		t.Fatal("classify error = nil, want a usage error")
	}

	if !strings.Contains(err.Error(), "vibes") {
		t.Errorf("classify error = %q, want it to name the offending value", err)
	}
}
