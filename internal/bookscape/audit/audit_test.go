package audit_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/adnoctem/pertry/internal/bookscape/audit"
	"github.com/adnoctem/pertry/internal/bookscape/config"
	"github.com/adnoctem/pertry/internal/bookscape/netscape"
)

// document wraps anchors in a minimal Netscape file inside one folder.
func document(folder string, anchors ...string) []byte {
	var b strings.Builder

	b.WriteString("<!DOCTYPE NETSCAPE-Bookmark-file-1>\n<TITLE>Bookmarks</TITLE>\n<H1>Bookmarks</H1>\n<DL><p>\n")
	b.WriteString("    <DT><H3>" + folder + "</H3>\n    <DL><p>\n")

	for _, a := range anchors {
		b.WriteString("        " + a + "\n")
	}

	b.WriteString("    </DL><p>\n</DL><p>\n")

	return []byte(b.String())
}

func link(url, title string) string {
	return `<DT><A HREF="` + url + `" ADD_DATE="1374257900">` + title + `</A>`
}

func analyze(t *testing.T, doc []byte, cfg *config.Config, opts audit.Options) audit.Report {
	t.Helper()

	entries, parsed, err := netscape.Parse(doc)
	if err != nil {
		t.Fatalf("Parse() error = %v, want nil", err)
	}

	return audit.Analyze(entries, parsed, cfg, opts)
}

func defaults() *config.Config {
	cfg := config.Default()

	return &cfg
}

// TestAnalyzeMatchesTheNormalisedHost is the bug this test suite was written for.
//
// Rules match against the host of url_norm, which the default policy has lowercased and stripped
// "www." from. An audit that tallied the raw host would report "www.youtube.com" as uncovered
// while the rule matching "youtube.com" resolves it - which is exactly backwards for someone
// using this report to write rules.
func TestAnalyzeMatchesTheNormalisedHost(t *testing.T) {
	t.Parallel()

	r := analyze(t, document("Watch",
		link("https://www.youtube.com/watch?v=1", "one"),
		link("http://WWW.YouTube.com/watch?v=2", "two"),
	), defaults(), audit.Options{Top: 10})

	if r.Resolved != 2 {
		t.Errorf("Resolved = %d, want 2; the www-stripped host must match the bare-domain rule", r.Resolved)
	}

	if r.Unresolved != 0 {
		t.Errorf("Unresolved = %d, want 0", r.Unresolved)
	}

	// And the breakdown lists the matchable form, not the spelling in the file.
	if len(r.Domains) != 1 || r.Domains[0].Label != "youtube.com" {
		t.Errorf("Domains = %+v, want a single entry labelled youtube.com", r.Domains)
	}
}

// TestAnalyzeCountsSurvivorsNotRows matters because the headline number is a cost estimate: what
// the model pass will be asked to classify is the deduplicated set.
func TestAnalyzeCountsSurvivorsNotRows(t *testing.T) {
	t.Parallel()

	// Four rows, two distinct comparison keys.
	r := analyze(t, document("Dev",
		link("https://example.com/a", "a"),
		link("http://www.example.com/a/", "a again"),
		link("https://example.com/b?utm_source=x", "b"),
		link("https://example.com/b", "b again"),
	), defaults(), audit.Options{Top: 10})

	if r.Entries != 4 {
		t.Errorf("Entries = %d, want 4", r.Entries)
	}

	if r.Survivors != 2 {
		t.Errorf("Survivors = %d, want 2", r.Survivors)
	}

	if r.DuplicateGroups != 2 || r.DuplicateRows != 2 {
		t.Errorf("DuplicateGroups = %d, DuplicateRows = %d, want 2 and 2",
			r.DuplicateGroups, r.DuplicateRows)
	}
}

// TestAnalyzeUnionsPathsAcrossADuplicateGroup mirrors what dedupe does, so a link filed in a
// folder a rule covers is resolved even when its duplicate sits somewhere uncovered.
func TestAnalyzeUnionsPathsAcrossADuplicateGroup(t *testing.T) {
	t.Parallel()

	cfg := config.Default()
	cfg.Classify.Rules = []config.Rule{
		{MatchSourcePath: []string{"Keep/*"}, Collection: "Archive"},
	}

	doc := []byte(`<!DOCTYPE NETSCAPE-Bookmark-file-1>
<TITLE>Bookmarks</TITLE>
<H1>Bookmarks</H1>
<DL><p>
    <DT><H3>Random</H3>
    <DL><p>
        <DT><A HREF="https://example.com/x" ADD_DATE="1374257900">x</A>
    </DL><p>
    <DT><H3>Keep</H3>
    <DL><p>
        <DT><A HREF="https://example.com/x" ADD_DATE="1374257901">x again</A>
    </DL><p>
</DL><p>`)

	r := analyze(t, doc, &cfg, audit.Options{Top: 10})

	if r.Survivors != 1 {
		t.Fatalf("Survivors = %d, want 1", r.Survivors)
	}

	if r.Resolved != 1 {
		t.Errorf("Resolved = %d, want 1; the survivor should inherit the Keep path from its duplicate", r.Resolved)
	}
}

// TestAnalyzeFlagsARuleThatNeverMatches is the single most useful line in the report: a rule
// written for someone else's corpus.
func TestAnalyzeFlagsARuleThatNeverMatches(t *testing.T) {
	t.Parallel()

	cfg := config.Default()
	cfg.Classify.Rules = []config.Rule{
		{MatchDomain: []string{"example.com"}, Collection: "Archive"},
		{MatchDomain: []string{"never.test"}, Collection: "Archive"},
	}

	r := analyze(t, document("Dev", link("https://example.com/a", "a")), &cfg, audit.Options{Top: 10})

	if len(r.RuleHits) != 2 {
		t.Fatalf("RuleHits = %d, want 2", len(r.RuleHits))
	}

	if r.RuleHits[0].N != 1 || r.RuleHits[0].Decisive != 1 {
		t.Errorf("rule 0 = %+v, want 1 match and 1 decision", r.RuleHits[0])
	}

	if r.RuleHits[1].N != 0 {
		t.Errorf("rule 1 matched %d, want 0", r.RuleHits[1].N)
	}

	var out bytes.Buffer
	if err := audit.Render(&out, r, audit.Options{Top: 10}); err != nil {
		t.Fatalf("Render() error = %v, want nil", err)
	}

	if !strings.Contains(out.String(), "never matches") {
		t.Errorf("report does not flag the dead rule:\n%s", out.String())
	}
}

// TestAnalyzeFlagsAShadowedRule catches a rule-ordering mistake: it matches, but an earlier rule
// always decides first, so it can never assign its collection.
func TestAnalyzeFlagsAShadowedRule(t *testing.T) {
	t.Parallel()

	cfg := config.Default()
	cfg.Classify.Rules = []config.Rule{
		{MatchDomain: []string{"example.com"}, Collection: "Development"},
		{MatchDomain: []string{"example.com"}, Collection: "Archive"},
	}

	r := analyze(t, document("Dev", link("https://example.com/a", "a")), &cfg, audit.Options{Top: 10})

	if r.RuleHits[1].N != 1 {
		t.Errorf("rule 1 matched %d, want 1", r.RuleHits[1].N)
	}

	if r.RuleHits[1].Decisive != 0 {
		t.Errorf("rule 1 decided %d, want 0", r.RuleHits[1].Decisive)
	}

	var out bytes.Buffer
	if err := audit.Render(&out, r, audit.Options{Top: 10}); err != nil {
		t.Fatalf("Render() error = %v, want nil", err)
	}

	if !strings.Contains(out.String(), "shadowed") {
		t.Errorf("report does not flag the shadowed rule:\n%s", out.String())
	}
}

func TestSuggestions(t *testing.T) {
	t.Parallel()

	cfg := config.Default()
	cfg.Classify.Rules = []config.Rule{
		{MatchDomain: []string{"covered.test"}, Collection: "Archive"},
	}

	anchors := []string{
		link("https://covered.test/1", "covered"),
		link("https://covered.test/2", "covered"),
	}
	// three links on heavy.test, two on medium.test, one on lonely.test
	for i := range 3 {
		anchors = append(anchors, link("https://heavy.test/"+string(rune('a'+i)), "h"))
	}

	for i := range 2 {
		anchors = append(anchors, link("https://medium.test/"+string(rune('a'+i)), "m"))
	}

	anchors = append(anchors, link("https://lonely.test/a", "l"))

	r := analyze(t, document("Dev", anchors...), &cfg,
		audit.Options{Top: 10, Suggest: 10, MinSuggest: 2})

	var domains []string
	for _, s := range r.Suggestions {
		if s.MatchDomain != "" {
			domains = append(domains, s.MatchDomain)
		}
	}

	// Ordered by weight; lonely.test is below MinSuggest and covered.test is already
	// resolved, so neither is proposed.
	if diff := cmp.Diff([]string{"heavy.test", "medium.test"}, domains); diff != "" {
		t.Errorf("suggested domains mismatch (-want +got):\n%s", diff)
	}

	// Two already resolved, plus the five the suggestions would cover.
	if r.Resolved != 2 {
		t.Errorf("Resolved = %d, want 2", r.Resolved)
	}

	if r.Projected != 7 {
		t.Errorf("Projected = %d, want 7 (2 resolved + 3 heavy + 2 medium)", r.Projected)
	}
}

func TestSuggestionsCanBeDisabled(t *testing.T) {
	t.Parallel()

	r := analyze(t, document("Dev", link("https://nothing.test/a", "a")), defaults(),
		audit.Options{Top: 10, Suggest: 0})

	if len(r.Suggestions) != 0 {
		t.Errorf("Suggestions = %+v, want none when Suggest is 0", r.Suggestions)
	}

	if r.Projected != r.Resolved {
		t.Errorf("Projected = %d, want it to equal Resolved (%d) with no suggestions",
			r.Projected, r.Resolved)
	}
}

// TestRenderWarnsAboutAnUnimportableFile is why audit is worth running before import: the two
// conditions that make import refuse are reported here without writing anything.
func TestRenderWarnsAboutAnUnimportableFile(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		doc  string
		want string
	}{
		"a dropped anchor": {
			doc: `<!DOCTYPE NETSCAPE-Bookmark-file-1>
<TITLE>Bookmarks</TITLE>
<H1>Bookmarks</H1>
<DL><p>
    <DT><A HREF="https://example.com/kept" ADD_DATE="1374257900">kept</A>
    <DT><a href="https://example.com/lost" add_date="1374257901">lost</a>
</DL><p>`,
			want: "will refuse this file",
		},
		"an implausible date": {
			doc: `<!DOCTYPE NETSCAPE-Bookmark-file-1>
<TITLE>Bookmarks</TITLE>
<H1>Bookmarks</H1>
<DL><p>
    <DT><A HREF="https://example.com/x" ADD_DATE="13244000000000000">x</A>
</DL><p>`,
			want: "implausible dates",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			r := analyze(t, []byte(tc.doc), defaults(), audit.Options{Top: 10})

			var out bytes.Buffer
			if err := audit.Render(&out, r, audit.Options{Top: 10}); err != nil {
				t.Fatalf("Render() error = %v, want nil", err)
			}

			if !strings.Contains(out.String(), tc.want) {
				t.Errorf("report does not mention %q:\n%s", tc.want, out.String())
			}
		})
	}
}

func TestRenderOnAnEmptyDocument(t *testing.T) {
	t.Parallel()

	doc := []byte("<!DOCTYPE NETSCAPE-Bookmark-file-1>\n<TITLE>Bookmarks</TITLE>\n<H1>Bookmarks</H1>\n<DL><p>\n</DL><p>")

	r := analyze(t, doc, defaults(), audit.Options{Top: 10, Suggest: 5})

	var out bytes.Buffer
	if err := audit.Render(&out, r, audit.Options{Top: 10, Suggest: 5}); err != nil {
		t.Fatalf("Render() error = %v, want nil", err)
	}

	// No division by zero, no suggestions, no panic.
	if !strings.Contains(out.String(), "bookmarks        0") {
		t.Errorf("report does not handle an empty document:\n%s", out.String())
	}
}
