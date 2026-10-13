package netscape_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/adnoctem/pertry/internal/bookscape/netscape"
)

func ptr(v int64) *int64 { return &v }

func TestParseFile(t *testing.T) {
	t.Parallel()

	entries, report, err := netscape.ParseFile(filepath.Join("testdata", "chrome_export.html"))
	if err != nil {
		t.Fatalf("ParseFile() error = %v, want nil", err)
	}

	t.Run("entries", func(t *testing.T) {
		t.Parallel()

		// Compared on the fields the importer stores. Attributes is checked separately
		// because only one fixture entry has any.
		type stored struct {
			URL     string
			Title   string
			Path    string
			AddDate *int64
		}

		got := make([]stored, 0, len(entries))
		for _, e := range entries {
			got = append(got, stored{URL: e.URL, Title: e.Title, Path: e.Path, AddDate: e.AddDate})
		}

		want := []stored{
			{URL: "https://go.dev/doc/", Title: "Documentation - The Go Programming Language", Path: "Bookmarks bar", AddDate: ptr(1374257900)},
			{URL: "https://news.ycombinator.com/", Title: "Hacker News", Path: "Bookmarks bar", AddDate: ptr(1402938421)},
			{URL: "https://www.linkedin.com/feed/", Title: "(1) LinkedIn", Path: "Bookmarks bar", AddDate: ptr(1500000000)},
			{URL: "https://example.com/untitled", Title: "", Path: "Bookmarks bar", AddDate: ptr(1500000001)},
			{URL: "https://github.com/golang/go", Title: "golang/go: The Go programming language", Path: "Bookmarks bar/Dev", AddDate: ptr(1385000100)},
			{URL: "https://stackoverflow.com/questions/1", Title: "Why is this & that <broken>?", Path: "Bookmarks bar/Dev", AddDate: ptr(1385000500)},
			{URL: "https://pkg.go.dev/database/sql", Title: "database/sql", Path: "Bookmarks bar/Dev/Go", AddDate: ptr(1385000300)},
			{URL: "https://pkg.go.dev/net/url", Title: "net/url", Path: "Bookmarks bar/Dev/Go", AddDate: ptr(1385000400)},
			{URL: "https://example.org/no-date", Title: "Bookmark with no ADD_DATE at all", Path: "Bookmarks bar/Dev/Go", AddDate: nil},
			{URL: "https://github.com/golang/go", Title: "golang/go", Path: "Reading List", AddDate: ptr(1600000100)},
			{URL: "https://example.net/article?utm_source=newsletter&utm_medium=email&id=42", Title: "An article with tracking parameters", Path: "Reading List", AddDate: ptr(1600000200)},
		}

		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("entries mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("report", func(t *testing.T) {
		t.Parallel()

		if report.Title != "Bookmarks" {
			t.Errorf("Report.Title = %q, want %q", report.Title, "Bookmarks")
		}

		if report.RootName != "Bookmarks" {
			t.Errorf("Report.RootName = %q, want %q", report.RootName, "Bookmarks")
		}

		// 5 <H3> folders below the root.
		if report.Folders != 5 {
			t.Errorf("Report.Folders = %d, want 5", report.Folders)
		}

		// The independent count must agree with the parser on a well-formed document; the
		// point of the check is the case where it does not.
		if report.AnchorTags != len(entries) {
			t.Errorf("Report.AnchorTags = %d, want %d (the parser and the raw count disagree)",
				report.AnchorTags, len(entries))
		}

		if got := report.Dropped(len(entries)); got != 0 {
			t.Errorf("Report.Dropped() = %d, want 0", got)
		}

		if report.UndatedEntries != 1 {
			t.Errorf("Report.UndatedEntries = %d, want 1", report.UndatedEntries)
		}

		if len(report.SuspectDates) != 0 {
			t.Errorf("Report.SuspectDates = %v, want none", report.SuspectDates)
		}
	})

	t.Run("attributes exclude typed fields", func(t *testing.T) {
		t.Parallel()

		var icon netscape.Entry
		for _, e := range entries {
			if e.URL == "https://go.dev/doc/" {
				icon = e
			}
		}

		if _, ok := icon.Attributes["ICON"]; !ok {
			t.Errorf("Attributes = %v, want an ICON key", icon.Attributes)
		}

		// Carrying these in Attributes as well as in the typed fields is what makes the
		// encoder emit each attribute twice.
		for _, key := range []string{"ADD_DATE", "LAST_MODIFIED", "PRIVATE", "TAGS"} {
			if _, ok := icon.Attributes[key]; ok {
				t.Errorf("Attributes contains %q, which belongs in a typed field only", key)
			}
		}
	})

	t.Run("tags decode into the typed field", func(t *testing.T) {
		t.Parallel()

		for _, e := range entries {
			if e.URL == "https://github.com/golang/go" && e.Path == "Bookmarks bar/Dev" {
				if diff := cmp.Diff([]string{"go", "repo"}, e.Tags); diff != "" {
					t.Errorf("Tags mismatch (-want +got):\n%s", diff)
				}

				return
			}
		}

		t.Fatal("fixture entry under Bookmarks bar/Dev not found")
	})
}

// TestParseDetectsDroppedEntries is the guard the spike's worst finding demands: the parser
// drops a case-mismatched anchor without an error, so Parse must notice that the raw document
// contained more anchors than came back.
func TestParseDetectsDroppedEntries(t *testing.T) {
	t.Parallel()

	const mixedCase = `<!DOCTYPE NETSCAPE-Bookmark-file-1>
<TITLE>Bookmarks</TITLE>
<H1>Bookmarks</H1>
<DL><p>
    <DT><A HREF="https://example.com/upper" ADD_DATE="1374257900">kept</A>
    <DT><a href="https://example.com/lower" add_date="1374257901">silently dropped</a>
</DL><p>`

	entries, report, err := netscape.Parse([]byte(mixedCase))
	if err != nil {
		t.Fatalf("Parse() error = %v, want nil (the library accepts this document)", err)
	}

	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}

	if report.AnchorTags != 2 {
		t.Errorf("Report.AnchorTags = %d, want 2; the raw count must see the anchor the parser dropped", report.AnchorTags)
	}

	if got := report.Dropped(len(entries)); got != 1 {
		t.Errorf("Report.Dropped() = %d, want 1", got)
	}
}

// TestParseFlagsImplausibleDates covers the other silent failure: an out-of-range timestamp is
// rescaled into a plausible-looking but wrong date rather than rejected.
func TestParseFlagsImplausibleDates(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		addDate string
		suspect bool
	}{
		"ordinary date":       {addDate: "1374257900", suspect: false},
		"webkit epoch micros": {addDate: "13244000000000000", suspect: true},
		"unix epoch zero":     {addDate: "0", suspect: true},
		"far future":          {addDate: "99999999999", suspect: true},
		"earliest plausible":  {addDate: "631152000", suspect: false}, // 1990-01-01
		"just before that":    {addDate: "631151999", suspect: true},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			doc := `<!DOCTYPE NETSCAPE-Bookmark-file-1>
<TITLE>Bookmarks</TITLE>
<H1>Bookmarks</H1>
<DL><p>
    <DT><A HREF="https://example.com/x" ADD_DATE="` + tc.addDate + `">x</A>
</DL><p>`

			_, report, err := netscape.Parse([]byte(doc))
			if err != nil {
				t.Fatalf("Parse() error = %v, want nil", err)
			}

			got := len(report.SuspectDates) > 0
			if got != tc.suspect {
				t.Errorf("suspect = %v, want %v (decoded dates: %v)", got, tc.suspect, report.SuspectDates)
			}
		})
	}
}

func TestParseTrimsTitles(t *testing.T) {
	t.Parallel()

	const unclosed = `<!DOCTYPE NETSCAPE-Bookmark-file-1>
<TITLE>Bookmarks</TITLE>
<H1>Bookmarks</H1>
<DL><p>
    <DT><A HREF="https://example.com/unclosed" ADD_DATE="1374257900">needs trimming
    <DT><A HREF="https://example.com/second" ADD_DATE="1374257901">second</A>
</DL><p>`

	entries, _, err := netscape.Parse([]byte(unclosed))
	if err != nil {
		t.Fatalf("Parse() error = %v, want nil", err)
	}

	if entries[0].Title != "needs trimming" {
		t.Errorf("Title = %q, want %q", entries[0].Title, "needs trimming")
	}
}

func TestParseRejectsMissingDoctype(t *testing.T) {
	t.Parallel()

	_, _, err := netscape.Parse([]byte("<TITLE>x</TITLE><H1>x</H1><DL><p>\n</DL><p>"))
	if err == nil {
		t.Fatal("Parse() error = nil, want an error")
	}

	// The error is what a user sees when they point import at the wrong file, so it has to
	// say what the right one looks like.
	for _, want := range []string{"DOCTYPE", "export bookmarks"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Parse() error = %q, want it to mention %q", err, want)
		}
	}
}

func TestParseFileReportsAMissingFile(t *testing.T) {
	t.Parallel()

	_, _, err := netscape.ParseFile(filepath.Join(t.TempDir(), "absent.html"))
	if err == nil {
		t.Fatal("ParseFile() error = nil, want an error")
	}

	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("ParseFile() error = %v, want it to wrap os.ErrNotExist", err)
	}
}

func TestCountAnchorsIgnoresFolderAndPlainTags(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		in   string
		want int
	}{
		"uppercase anchor":      {in: `<DT><A HREF="https://x.test/">x</A>`, want: 1},
		"lowercase anchor":      {in: `<dt><a href="https://x.test/">x</a>`, want: 1},
		"mixed case attribute":  {in: `<DT><a HrEf="https://x.test/">x</a>`, want: 1},
		"folder header":         {in: `<DT><H3 ADD_DATE="1">Dev</H3>`, want: 0},
		"anchor without href":   {in: `<DT><A NAME="anchor">x</A>`, want: 0},
		"href in folder header": {in: `<DT><H3 TITLE="see <a href=x>">Dev</H3>`, want: 0},
		"two anchors":           {in: `<A HREF="a">a</A><A HREF="b">b</A>`, want: 2},
		"single-quoted href":    {in: `<A HREF='https://x.test/'>x</A>`, want: 1},
		"unquoted href":         {in: `<A HREF=https://x.test/>x</A>`, want: 1},
		"href after other attr": {in: `<A ADD_DATE="1" HREF="https://x.test/">x</A>`, want: 1},
		"abbr is not an anchor": {in: `<ABBR HREF="x">x</ABBR>`, want: 0},
		"closing anchor tag":    {in: `</A>`, want: 0},
		"unterminated tag":      {in: `<A HREF="https://x.test/"`, want: 1},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if got := netscape.CountAnchors([]byte(tc.in)); got != tc.want {
				t.Errorf("CountAnchors(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}
