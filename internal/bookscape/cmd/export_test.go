package cmd_test

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/adnoctem/pertry/internal/bookscape/cmd"
)

// exportable imports four links, classifies two by rule, and seeds the rest so that each
// export exclusion has something to act on: one dead, one uncertain, one below the confidence
// threshold, and one collapsed duplicate.
func exportable(t *testing.T) (dbPath, outPath string) {
	t.Helper()

	export := writeExport(t, `<!DOCTYPE NETSCAPE-Bookmark-file-1>
<TITLE>Bookmarks</TITLE>
<H1>Bookmarks</H1>
<DL><p>
    <DT><A HREF="https://github.com/golang/go" ADD_DATE="1385000100" TAGS="go">golang/go</A>
    <DT><A HREF="http://www.github.com/golang/go/" ADD_DATE="1600000100">golang/go again</A>
    <DT><A HREF="https://stackoverflow.com/questions/1" ADD_DATE="1385000200">a question</A>
    <DT><A HREF="https://dead.test/gone" ADD_DATE="1385000300">gone</A>
    <DT><A HREF="https://maybe.test/blocked" ADD_DATE="1385000400">blocked</A>
    <DT><A HREF="https://unsure.test/what" ADD_DATE="1385000500">what is this</A>
</DL><p>`)

	dir := filepath.Dir(export)
	dbPath = filepath.Join(dir, "bookscape.db")
	outPath = filepath.Join(dir, "enriched.json")

	if _, _, err := run(t, "import", "--database", dbPath, export); err != nil {
		t.Fatalf("import error = %v, want nil", err)
	}

	if _, _, err := run(t, "dedupe", "--database", dbPath); err != nil {
		t.Fatalf("dedupe error = %v, want nil", err)
	}

	if _, _, err := runWithBackend(t, refuseAll(t), "classify", "--pass", "rules", "--database", dbPath); err != nil {
		t.Fatalf("rules pass error = %v, want nil", err)
	}

	handle, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}

	for _, stmt := range []string{
		`INSERT INTO liveness (bookmark_id, status, checked_at)
		 SELECT id, 'dead', 0 FROM bookmarks WHERE url LIKE '%dead.test%'`,
		`INSERT INTO liveness (bookmark_id, status, checked_at)
		 SELECT id, 'uncertain', 0 FROM bookmarks WHERE url LIKE '%maybe.test%'`,
		`INSERT INTO classifications
		     (bookmark_id, collection, description, confidence, source, model, run_id, prompt_hash, created_at)
		 SELECT id, 'Archive', 'An unclear page.', 0.2, 'model', 'test-model', 1, 'hash', 0
		 FROM bookmarks WHERE url LIKE '%unsure.test%'`,
		`INSERT INTO classifications
		     (bookmark_id, collection, description, confidence, source, model, run_id, prompt_hash, created_at)
		 SELECT id, 'Reading', 'A page behind bot protection.', 0.9, 'model', 'test-model', 1, 'hash', 0
		 FROM bookmarks WHERE url LIKE '%maybe.test%'`,
		`INSERT INTO classifications
		     (bookmark_id, collection, description, confidence, source, model, run_id, prompt_hash, created_at)
		 SELECT id, 'Archive', 'A page that is gone.', 0.9, 'model', 'test-model', 1, 'hash', 0
		 FROM bookmarks WHERE url LIKE '%dead.test%'`,
	} {
		if _, err := handle.Exec(stmt); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	if err := handle.Close(); err != nil {
		t.Fatalf("close database: %v", err)
	}

	return dbPath, outPath
}

// TestExportHTML covers the Netscape projection, which is no longer the default - `--type html`
// selects it. Every assertion here is about the document's own syntax, which is why it did not
// move to the JSON test below.
func TestExportHTML(t *testing.T) {
	t.Parallel()

	dbPath, jsonPath := exportable(t)
	outPath := filepath.Join(filepath.Dir(jsonPath), "enriched.html")

	out, _, err := run(t, "export", "--type", "html", "--database", dbPath, "-o", outPath)
	if err != nil {
		t.Fatalf("export error = %v, want nil", err)
	}

	// 6 rows: 1 collapsed, 1 dead, 1 pending review. The uncertain one ships, because
	// export.include_uncertain defaults to true.
	if !strings.Contains(out, "Exported 3 bookmarks") {
		t.Errorf("stdout = %q, want three exported bookmarks", out)
	}

	body, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read export: %v", err)
	}

	html := string(body)

	t.Run("original dates survive", func(t *testing.T) {
		if !strings.Contains(html, `ADD_DATE="1385000100"`) {
			t.Errorf("output does not carry the original ADD_DATE:\n%s", html)
		}
	})

	t.Run("collections became folders", func(t *testing.T) {
		for _, want := range []string{"<H3>Development</H3>", "<H3>Reference</H3>"} {
			if !strings.Contains(html, want) {
				t.Errorf("output does not contain %q", want)
			}
		}
	})

	t.Run("tags became the TAGS attribute", func(t *testing.T) {
		if !strings.Contains(html, `TAGS="go,repo"`) {
			t.Errorf("output does not carry merged tags:\n%s", html)
		}
	})

	t.Run("no attribute is emitted twice", func(t *testing.T) {
		// Writing a typed field through Attributes as well would duplicate it, which is
		// invalid HTML and would silently discard one of the two values downstream.
		for _, line := range strings.Split(html, "\n") {
			for _, attr := range []string{"ADD_DATE", "TAGS", "PRIVATE"} {
				if n := strings.Count(line, attr+`="`); n > 1 {
					t.Errorf("%s appears %d times on one element: %s", attr, n, strings.TrimSpace(line))
				}
			}
		}
	})

	t.Run("exclusions are accounted for", func(t *testing.T) {
		for _, want := range []string{
			"duplicates collapsed by dedupe",
			"dead links",
			"below min_confidence and not yet reviewed",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("stdout = %q, want it to report %q", out, want)
			}
		}
	})

	t.Run("the collapsed duplicate is not exported", func(t *testing.T) {
		if strings.Contains(html, "http://www.github.com/golang/go/") {
			t.Error("output contains a row that dedupe collapsed")
		}
	})

	t.Run("the dead link is not exported", func(t *testing.T) {
		if strings.Contains(html, "dead.test") {
			t.Error("output contains a dead link")
		}
	})

	t.Run("the description loss is stated", func(t *testing.T) {
		// The <DD> is in the file; Linkwarden's HTML importer is what ignores it. A user
		// who picked this format has to be told, or they will conclude the model pass
		// did nothing.
		if !strings.Contains(out, "descriptions") {
			t.Errorf("stdout = %q, want it to warn that descriptions do not survive", out)
		}
	})
}

// TestExportJSON covers the default format, Linkwarden's own backup document.
//
// The assertions are shaped by what importFromLinkwarden.ts (v2.16.3) actually does, because
// three of its dereferences have no nil guard and the transaction's error is swallowed by a
// `.catch` that still answers 200 "Success." - so a document that gets any of them wrong imports
// nothing and reports success. Go marshals a nil slice as null, which is precisely how that
// happens by accident.
func TestExportJSON(t *testing.T) {
	t.Parallel()

	dbPath, outPath := exportable(t)

	out, _, err := run(t, "export", "--database", dbPath, "-o", outPath)
	if err != nil {
		t.Fatalf("export error = %v, want nil", err)
	}

	if !strings.Contains(out, "Exported 3 bookmarks") {
		t.Errorf("stdout = %q, want three exported bookmarks", out)
	}

	body, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read export: %v", err)
	}

	var doc struct {
		Collections []struct {
			Name  string `json:"name"`
			Links []struct {
				URL         string `json:"url"`
				Name        string `json:"name"`
				Description string `json:"description"`
				ImportDate  string `json:"importDate"`
				CreatedAt   string `json:"createdAt"`
				Tags        []struct {
					Name string `json:"name"`
				} `json:"tags"`
			} `json:"links"`
		} `json:"collections"`
		PinnedLinks []any `json:"pinnedLinks"`
	}

	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, body)
	}

	links := map[string]int{}
	for _, c := range doc.Collections {
		for _, l := range c.Links {
			links[l.URL]++
		}
	}

	t.Run("collections became collections", func(t *testing.T) {
		names := map[string]bool{}
		for _, c := range doc.Collections {
			names[c.Name] = true
		}

		for _, want := range []string{"Development", "Reference"} {
			if !names[want] {
				t.Errorf("output has no %q collection, only %v", want, names)
			}
		}
	})

	t.Run("original dates survive as RFC 3339", func(t *testing.T) {
		// 1385000100 is the ADD_DATE on golang/go in the fixture.
		want := time.Unix(1385000100, 0).UTC().Format(time.RFC3339)

		var found bool

		for _, c := range doc.Collections {
			for _, l := range c.Links {
				if l.ImportDate == want {
					found = true

					// Both fields carry it: the importer reads
					// `importDate || createdAt` and a future version may
					// read only one.
					if l.CreatedAt != want {
						t.Errorf("createdAt = %q, want %q", l.CreatedAt, want)
					}
				}
			}
		}

		if !found {
			t.Errorf("no link carries importDate %q:\n%s", want, body)
		}
	})

	t.Run("tags are objects carrying a name", func(t *testing.T) {
		var found []string

		for _, c := range doc.Collections {
			for _, l := range c.Links {
				if strings.Contains(l.URL, "github.com/golang/go") {
					for _, tag := range l.Tags {
						found = append(found, tag.Name)
					}
				}
			}
		}

		slices.Sort(found)

		if want := []string{"go", "repo"}; !slices.Equal(found, want) {
			t.Errorf("tags = %v, want %v", found, want)
		}
	})

	t.Run("the generated description lands", func(t *testing.T) {
		// The whole reason this format is the default. The fixture seeds this
		// description on the uncertain link, which ships because include_uncertain
		// defaults to true.
		var found bool

		for _, c := range doc.Collections {
			for _, l := range c.Links {
				if l.Description == "A page behind bot protection." {
					found = true
				}
			}
		}

		if !found {
			t.Errorf("no link carries the generated description:\n%s", body)
		}
	})

	t.Run("no array is encoded as null", func(t *testing.T) {
		// Each of these is dereferenced without a guard by the importer, so a null
		// makes it throw inside a transaction whose error is logged and discarded -
		// and the endpoint then reports success having imported nothing.
		raw := string(body)

		for _, forbidden := range []string{
			`"pinnedLinks": null`,
			`"collections": null`,
			`"links": null`,
			`"tags": null`,
		} {
			if strings.Contains(raw, forbidden) {
				t.Errorf("output contains %s, which makes Linkwarden import nothing and report success", forbidden)
			}
		}

		if doc.PinnedLinks == nil {
			t.Error("pinnedLinks decoded as nil; it must be present as an array")
		}
	})

	t.Run("the collapsed duplicate and the dead link are not exported", func(t *testing.T) {
		for url := range links {
			if strings.Contains(url, "dead.test") {
				t.Error("output contains a dead link")
			}

			if url == "http://www.github.com/golang/go/" {
				t.Error("output contains a row that dedupe collapsed")
			}
		}
	})

	t.Run("every link appears once", func(t *testing.T) {
		for url, n := range links {
			if n != 1 {
				t.Errorf("%s appears %d times", url, n)
			}
		}
	})
}

// TestExportUnknownType is a usage error rather than a run that failed: the document was never
// going to be produced, so it must not look like a failed export.
func TestExportUnknownType(t *testing.T) {
	t.Parallel()

	dbPath, outPath := exportable(t)

	_, _, err := run(t, "export", "--type", "yaml", "--database", dbPath, "-o", outPath)
	if err == nil {
		t.Fatal("export --type yaml succeeded, want a usage error")
	}

	if got := cmd.ExitCodeFor(err); got != cmd.ExitUsage {
		t.Errorf("exit code = %d, want %d", got, cmd.ExitUsage)
	}
}

// TestExportDefaultOutputFollowsTheFormat is why export.output defaults to empty: a fixed
// default would write JSON into a file named .html the moment the default format changed.
func TestExportDefaultOutputFollowsTheFormat(t *testing.T) {
	// Not parallel: t.Chdir panics in a parallel test, and the default output path is
	// relative, so the working directory is the thing under test.
	dbPath, outPath := exportable(t)
	dir := filepath.Dir(outPath)

	t.Chdir(dir)

	if _, _, err := run(t, "export", "--database", dbPath); err != nil {
		t.Fatalf("export error = %v, want nil", err)
	}

	if _, err := os.Stat(filepath.Join(dir, "enriched-bookmarks.json")); err != nil {
		t.Errorf("the default json export is not at enriched-bookmarks.json: %v", err)
	}

	if _, _, err := run(t, "export", "--type", "html", "--database", dbPath); err != nil {
		t.Fatalf("html export error = %v, want nil", err)
	}

	if _, err := os.Stat(filepath.Join(dir, "enriched-bookmarks.html")); err != nil {
		t.Errorf("the html export is not at enriched-bookmarks.html: %v", err)
	}
}

// TestExportIsGatedOnReview is the mechanism by which `review` gates `export`.
func TestExportIsGatedOnReview(t *testing.T) {
	t.Parallel()

	dbPath, outPath := exportable(t)

	if _, _, err := run(t, "export", "--database", dbPath, "-o", outPath); err != nil {
		t.Fatalf("export error = %v, want nil", err)
	}

	before, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read export: %v", err)
	}

	if strings.Contains(string(before), "unsure.test") {
		t.Fatal("the low-confidence link was exported before review")
	}

	if _, _, err := run(t, "review", "accept", "--all", "--database", dbPath); err != nil {
		t.Fatalf("review accept error = %v, want nil", err)
	}

	if _, _, err := run(t, "export", "--force", "--database", dbPath, "-o", outPath); err != nil {
		t.Fatalf("second export error = %v, want nil", err)
	}

	after, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read export: %v", err)
	}

	if !strings.Contains(string(after), "unsure.test") {
		t.Error("the link is still held back after being accepted in review")
	}
}

func TestExportIncludeDead(t *testing.T) {
	t.Parallel()

	dbPath, outPath := exportable(t)

	cfgPath := filepath.Join(filepath.Dir(dbPath), "bookscape.yaml")
	if err := os.WriteFile(cfgPath, []byte("export:\n  include_dead: true\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	if _, _, err := run(t, "export", "--config", cfgPath, "--database", dbPath, "-o", outPath); err != nil {
		t.Fatalf("export error = %v, want nil", err)
	}

	body, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read export: %v", err)
	}

	if !strings.Contains(string(body), "dead.test") {
		t.Error("output omits the dead link even though include_dead is set")
	}
}

func TestExportExcludeUncertain(t *testing.T) {
	t.Parallel()

	dbPath, outPath := exportable(t)

	cfgPath := filepath.Join(filepath.Dir(dbPath), "bookscape.yaml")
	if err := os.WriteFile(cfgPath, []byte("export:\n  include_uncertain: false\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	out, _, err := run(t, "export", "--config", cfgPath, "--database", dbPath, "-o", outPath)
	if err != nil {
		t.Fatalf("export error = %v, want nil", err)
	}

	body, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read export: %v", err)
	}

	if strings.Contains(string(body), "maybe.test") {
		t.Error("output contains an uncertain link even though include_uncertain is off")
	}

	if !strings.Contains(out, "inconclusive liveness check") {
		t.Errorf("stdout = %q, want it to account for the uncertain link", out)
	}
}

// TestExportRefusesToClobber matters because a previous export may already have been imported
// somewhere, and replacing it silently loses the record of what was sent.
func TestExportRefusesToClobber(t *testing.T) {
	t.Parallel()

	dbPath, outPath := exportable(t)

	if _, _, err := run(t, "export", "--database", dbPath, "-o", outPath); err != nil {
		t.Fatalf("first export error = %v, want nil", err)
	}

	_, _, err := run(t, "export", "--database", dbPath, "-o", outPath)
	if err == nil {
		t.Fatal("second export error = nil, want a refusal")
	}

	if !strings.Contains(err.Error(), "--force") {
		t.Errorf("export error = %q, want it to mention --force", err)
	}

	if _, _, err := run(t, "export", "--force", "--database", dbPath, "-o", outPath); err != nil {
		t.Fatalf("export --force error = %v, want nil", err)
	}
}

func TestExportWithNothingToExport(t *testing.T) {
	t.Parallel()

	_, dbPath := workspace(t)
	outPath := filepath.Join(filepath.Dir(dbPath), "enriched.html")

	out, _, err := run(t, "export", "--database", dbPath, "-o", outPath)
	if err != nil {
		t.Fatalf("export error = %v, want nil", err)
	}

	if !strings.Contains(out, "Nothing to export") {
		t.Errorf("stdout = %q, want it to say there is nothing to export", out)
	}

	// No file is written, so a stale one from a previous run is not mistaken for current.
	if _, err := os.Stat(outPath); err == nil {
		t.Error("an output file was written even though there was nothing to export")
	}
}

func TestStats(t *testing.T) {
	t.Parallel()

	dbPath, _ := exportable(t)

	out, _, err := run(t, "stats", "--database", dbPath)
	if err != nil {
		t.Fatalf("stats error = %v, want nil", err)
	}

	for _, want := range []string{
		"Corpus", "bookmarks", "survivors", "collapsed",
		"Liveness", "Classification", "Collections", "Top domains", "Saved per year",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout = %q, want a %q section", out, want)
		}
	}

	// The date histogram is the view that makes a decade-old archive legible.
	if !strings.Contains(out, "2013") {
		t.Errorf("stdout = %q, want the histogram to show the fixture's year", out)
	}
}

func TestStatsOnAnEmptyDatabase(t *testing.T) {
	t.Parallel()

	_, dbPath := workspace(t)

	out, _, err := run(t, "stats", "--database", dbPath)
	if err != nil {
		t.Fatalf("stats error = %v, want nil", err)
	}

	if !strings.Contains(out, "no bookmarks yet") {
		t.Errorf("stdout = %q, want it to point at import", out)
	}
}

func TestStatsRedirectTargets(t *testing.T) {
	t.Parallel()

	dbPath, _ := exportable(t)

	handle, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}

	// Two links resolving to the same place, which is the signal the view exists for. The
	// fixture seeds liveness for two bookmarks, so update those rather than picking ids
	// that may have no liveness row at all.
	res, err := handle.Exec(
		`UPDATE liveness SET final_url = 'https://parked.test/for-sale'`)
	if err != nil {
		t.Fatalf("seed redirect targets: %v", err)
	}

	if n, err := res.RowsAffected(); err != nil || n < 2 {
		t.Fatalf("seeded %d redirect targets (err %v), want at least 2", n, err)
	}

	if err := handle.Close(); err != nil {
		t.Fatalf("close database: %v", err)
	}

	out, _, err := run(t, "stats", "--redirect-targets", "--database", dbPath)
	if err != nil {
		t.Fatalf("stats --redirect-targets error = %v, want nil", err)
	}

	if !strings.Contains(out, "parked.test/for-sale") {
		t.Errorf("stdout = %q, want the shared destination", out)
	}
}
