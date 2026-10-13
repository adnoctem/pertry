// Package netscape_test characterises github.com/virtualtam/netscape-go/v2.
//
// These are not tests of bookscape. They are the executable record of the spike that
// docs/TODO.md items 1 and 2 demanded before any pipeline code was
// written, and every assertion here backs a claim made in docs/ARCHITECTURE.md#parsing of that document.
// They exist so that a library upgrade which changes one of those behaviours fails the build
// instead of quietly changing what bookscape exports.
//
// The fixture in testdata is synthetic but mirrors Chrome's export shape: interleaved
// bookmarks and subfolders, three levels of nesting, an ICON payload, an entry with no
// ADD_DATE, an empty title, HTML-escaped title text, a <DD> description, and an empty folder.
// Section 10 item 3 - the count check against a real corpus - is deliberately not covered
// here; it needs the owner's actual export, not a fixture.
package netscape_test

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	netscape "github.com/virtualtam/netscape-go/v2"
)

const fixture = "chrome_export.html"

func readFixture(t *testing.T) []byte {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join("testdata", fixture))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	return raw
}

func parseFixture(t *testing.T) *netscape.Document {
	t.Helper()

	doc, err := netscape.Unmarshal(readFixture(t))
	if err != nil {
		t.Fatalf("Unmarshal(%s) error = %v, want nil", fixture, err)
	}

	return doc
}

// flatBookmark is a bookmark plus the folder path it was found under, which is the shape
// bookscape's importer needs.
type flatBookmark struct {
	Path string
	URL  string
}

func flatten(f *netscape.Folder, path []string) []flatBookmark {
	var out []flatBookmark

	for i := range f.Bookmarks {
		out = append(out, flatBookmark{Path: strings.Join(path, "/"), URL: f.Bookmarks[i].URL})
	}

	for i := range f.Subfolders {
		sub := &f.Subfolders[i]
		out = append(out, flatten(sub, append(path, sub.Name))...)
	}

	return out
}

func findBookmark(t *testing.T, f *netscape.Folder, url string) netscape.Bookmark {
	t.Helper()

	for i := range f.Bookmarks {
		if f.Bookmarks[i].URL == url {
			return f.Bookmarks[i]
		}
	}

	for i := range f.Subfolders {
		if b := findBookmarkOrZero(&f.Subfolders[i], url); b.URL != "" {
			return b
		}
	}

	t.Fatalf("bookmark %q not found in document", url)

	return netscape.Bookmark{}
}

func findBookmarkOrZero(f *netscape.Folder, url string) netscape.Bookmark {
	for i := range f.Bookmarks {
		if f.Bookmarks[i].URL == url {
			return f.Bookmarks[i]
		}
	}

	for i := range f.Subfolders {
		if b := findBookmarkOrZero(&f.Subfolders[i], url); b.URL != "" {
			return b
		}
	}

	return netscape.Bookmark{}
}

// TestUnmarshalPreservesTreeStructure is the guard against the failure mode docs/ARCHITECTURE.md#parsing
// names: a parser that flattens or drops a subtree without saying so. Every bookmark in the
// fixture must come back under the exact folder path it was filed in.
func TestUnmarshalPreservesTreeStructure(t *testing.T) {
	t.Parallel()

	doc := parseFixture(t)

	want := []flatBookmark{
		{Path: "Bookmarks bar", URL: "https://go.dev/doc/"},
		{Path: "Bookmarks bar", URL: "https://news.ycombinator.com/"},
		{Path: "Bookmarks bar", URL: "https://www.linkedin.com/feed/"},
		{Path: "Bookmarks bar", URL: "https://example.com/untitled"},
		{Path: "Bookmarks bar/Dev", URL: "https://github.com/golang/go"},
		{Path: "Bookmarks bar/Dev", URL: "https://stackoverflow.com/questions/1"},
		{Path: "Bookmarks bar/Dev/Go", URL: "https://pkg.go.dev/database/sql"},
		{Path: "Bookmarks bar/Dev/Go", URL: "https://pkg.go.dev/net/url"},
		{Path: "Bookmarks bar/Dev/Go", URL: "https://example.org/no-date"},
		{Path: "Reading List", URL: "https://github.com/golang/go"},
		{Path: "Reading List", URL: "https://example.net/article?utm_source=newsletter&utm_medium=email&id=42"},
	}

	if diff := cmp.Diff(want, flatten(&doc.Root, nil)); diff != "" {
		t.Errorf("flattened document mismatch (-want +got):\n%s", diff)
	}

	if doc.Title != "Bookmarks" {
		t.Errorf("Document.Title = %q, want %q", doc.Title, "Bookmarks")
	}

	if doc.Root.Name != "Bookmarks" {
		t.Errorf("Document.Root.Name = %q, want %q", doc.Root.Name, "Bookmarks")
	}
}

// TestUnmarshalDecodesADDDATEIntoCreatedAt is the answer to the blocking question in docs/TODO.md
// item 1, read from the decoder's side: ADD_DATE arrives as a time.Time, not as an Attributes
// entry.
func TestUnmarshalDecodesADDDATEIntoCreatedAt(t *testing.T) {
	t.Parallel()

	doc := parseFixture(t)

	tests := map[string]struct {
		url     string
		addDate int64 // 0 means "ADD_DATE absent, CreatedAt must be the zero time"
	}{
		"top-level entry":  {url: "https://go.dev/doc/", addDate: 1374257900},
		"nested entry":     {url: "https://pkg.go.dev/database/sql", addDate: 1385000300},
		"deeply nested":    {url: "https://pkg.go.dev/net/url", addDate: 1385000400},
		"no ADD_DATE":      {url: "https://example.org/no-date", addDate: 0},
		"sibling folder":   {url: "https://example.net/article?utm_source=newsletter&utm_medium=email&id=42", addDate: 1600000200},
		"duplicated entry": {url: "https://github.com/golang/go", addDate: 1385000100},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			b := findBookmark(t, &doc.Root, tc.url)

			if tc.addDate == 0 {
				if !b.CreatedAt.IsZero() {
					t.Errorf("CreatedAt = %v, want the zero time for an entry with no ADD_DATE", b.CreatedAt)
				}

				return
			}

			if got := b.CreatedAt.Unix(); got != tc.addDate {
				t.Errorf("CreatedAt.Unix() = %d, want %d", got, tc.addDate)
			}
		})
	}

	// Folders carry dates on the same typed fields.
	bar := doc.Root.Subfolders[0]
	if got := bar.CreatedAt.Unix(); got != 1374257895 {
		t.Errorf("folder %q CreatedAt.Unix() = %d, want 1374257895", bar.Name, got)
	}

	if got := bar.UpdatedAt.Unix(); got != 1759500000 {
		t.Errorf("folder %q UpdatedAt.Unix() = %d, want 1759500000", bar.Name, got)
	}
}

var addDateRe = regexp.MustCompile(`ADD_DATE="(\d+)"`)

func addDates(b []byte) []string {
	var out []string
	for _, m := range addDateRe.FindAllStringSubmatch(string(b), -1) {
		out = append(out, m[1])
	}

	slices.Sort(out)

	return out
}

// TestMarshalRoundTripsEveryADDDATE is docs/ARCHITECTURE.md#parsing proper: the timestamps that went in
// come back out, for folders as well as bookmarks. If this ever fails, bookscape's entire
// archive lands in Linkwarden dated today.
func TestMarshalRoundTripsEveryADDDATE(t *testing.T) {
	t.Parallel()

	raw := readFixture(t)

	doc, err := netscape.Unmarshal(raw)
	if err != nil {
		t.Fatalf("Unmarshal error = %v, want nil", err)
	}

	out, err := netscape.Marshal(doc)
	if err != nil {
		t.Fatalf("Marshal error = %v, want nil", err)
	}

	if diff := cmp.Diff(addDates(raw), addDates(out)); diff != "" {
		t.Errorf("ADD_DATE multiset changed across the round-trip (-input +output):\n%s", diff)
	}
}

// TestMarshalIsAFixedPoint records that the encoder's own output re-decodes and re-encodes
// identically. The first round-trip is not byte-identical to the input - see
// TestRoundTripIsAttributeAdditive - but every round-trip after it is.
func TestMarshalIsAFixedPoint(t *testing.T) {
	t.Parallel()

	doc := parseFixture(t)

	first, err := netscape.Marshal(doc)
	if err != nil {
		t.Fatalf("Marshal error = %v, want nil", err)
	}

	reparsed, err := netscape.Unmarshal(first)
	if err != nil {
		t.Fatalf("Unmarshal of encoder output error = %v, want nil", err)
	}

	second, err := netscape.Marshal(reparsed)
	if err != nil {
		t.Fatalf("second Marshal error = %v, want nil", err)
	}

	if diff := cmp.Diff(string(first), string(second)); diff != "" {
		t.Errorf("encode(decode(encode(x))) != encode(x) (-first +second):\n%s", diff)
	}

	if got, want := len(flatten(&reparsed.Root, nil)), len(flatten(&doc.Root, nil)); got != want {
		t.Errorf("bookmark count across a re-parse = %d, want %d", got, want)
	}
}

// TestRoundTripIsAttributeAdditive pins the two attributes the encoder adds that the input did
// not have. This is why the exporter's output is never byte-comparable to the input, and why
// the bookmarks.last_modified column equals add_date rather than being NULL for entries the
// browser never modified.
func TestRoundTripIsAttributeAdditive(t *testing.T) {
	t.Parallel()

	raw := readFixture(t)

	doc, err := netscape.Unmarshal(raw)
	if err != nil {
		t.Fatalf("Unmarshal error = %v, want nil", err)
	}

	out, err := netscape.Marshal(doc)
	if err != nil {
		t.Fatalf("Marshal error = %v, want nil", err)
	}

	// The decoder back-fills UpdatedAt from CreatedAt when LAST_MODIFIED is absent, so the
	// encoder then writes one for every dated entry and folder.
	if in, got := strings.Count(string(raw), "LAST_MODIFIED="), strings.Count(string(out), "LAST_MODIFIED="); got <= in {
		t.Errorf("LAST_MODIFIED count went %d -> %d, want the output to gain back-filled dates", in, got)
	}

	// PRIVATE has no omitempty on the encoder's side.
	if strings.Contains(string(raw), "PRIVATE=") {
		t.Fatal("fixture should not carry PRIVATE, so that the encoder's unconditional output is observable")
	}

	if !strings.Contains(string(out), `PRIVATE="0"`) {
		t.Error(`output is missing PRIVATE="0", which the encoder writes unconditionally`)
	}

	// A bookmark with no ADD_DATE must not acquire one.
	noDate := findBookmark(t, &doc.Root, "https://example.org/no-date")
	if !noDate.CreatedAt.IsZero() {
		t.Fatal("fixture entry https://example.org/no-date unexpectedly carries a date")
	}

	// Match on `ADD_DATE="` rather than the bare name: this entry's own title contains the
	// words "ADD_DATE", which a substring check would mistake for an attribute.
	for line := range strings.SplitSeq(string(out), "\n") {
		if strings.Contains(line, "https://example.org/no-date") && strings.Contains(line, `ADD_DATE="`) {
			t.Errorf("undated entry gained an ADD_DATE on encode: %s", strings.TrimSpace(line))
		}
	}
}

// TestAttributesDuplicateTypedFields is the trap the spike found, and the reason docs/ARCHITECTURE.md#export-and-linkwarden's
// exporter uses CreatedAt and Tags rather than an Attributes map. The encoder writes the typed
// field and the Attributes entry, producing the same attribute twice on one element.
func TestAttributesDuplicateTypedFields(t *testing.T) {
	t.Parallel()

	doc := &netscape.Document{
		Title: "probe",
		Root: netscape.Folder{
			Name: "probe",
			Bookmarks: []netscape.Bookmark{{
				URL:        "https://example.com/",
				Title:      "probe",
				CreatedAt:  time.Unix(1374257900, 0).UTC(),
				Tags:       []string{"a", "b"},
				Attributes: map[string]string{"ADD_DATE": "999", "TAGS": "zzz"},
			}},
		},
	}

	out, err := netscape.Marshal(doc)
	if err != nil {
		t.Fatalf("Marshal error = %v, want nil", err)
	}

	if got := strings.Count(string(out), "ADD_DATE="); got != 2 {
		t.Errorf("ADD_DATE occurrences = %d, want 2 "+
			"(the library no longer duplicates typed fields set in Attributes; "+
			"re-check the caution in docs/ARCHITECTURE.md#parsing)", got)
	}

	if got := strings.Count(string(out), "TAGS="); got != 2 {
		t.Errorf("TAGS occurrences = %d, want 2 (see ADD_DATE above)", got)
	}
}

// TestUnmarshalDropsUnrecognisedElementCase is the single most dangerous behaviour the spike
// found: a bookmark whose element name does not match case-sensitively vanishes, and
// Unmarshal returns no error at all. bookscape's import must reconcile its entry count against
// a count taken independently of the parser rather than trusting a successful parse.
func TestUnmarshalDropsUnrecognisedElementCase(t *testing.T) {
	t.Parallel()

	const lowercase = `<!DOCTYPE NETSCAPE-Bookmark-file-1>
<TITLE>Bookmarks</TITLE>
<H1>Bookmarks</H1>
<DL><p>
    <DT><a href="https://example.com/lower" add_date="1374257900">lowercase</a>
</DL><p>`

	doc, err := netscape.Unmarshal([]byte(lowercase))
	if err != nil {
		t.Fatalf("Unmarshal error = %v, want nil (the library accepts the document silently)", err)
	}

	if got := len(flatten(&doc.Root, nil)); got != 0 {
		t.Errorf("bookmarks = %d, want 0; the library now handles lowercase element names, "+
			"so the warning in docs/ARCHITECTURE.md#parsing can be relaxed", got)
	}
}

// TestUnmarshalRescalesOutOfRangeDates records that an implausible timestamp is silently
// reinterpreted rather than rejected. The value here is microseconds since 1601 (the WebKit
// epoch Chrome uses in its internal Bookmarks JSON, not in its HTML export) and should mean
// 2020; the library reads it as nanoseconds since 1970 and lands in June 1970. import must
// range-check decoded dates itself.
func TestUnmarshalRescalesOutOfRangeDates(t *testing.T) {
	t.Parallel()

	const webkitEpoch = `<!DOCTYPE NETSCAPE-Bookmark-file-1>
<TITLE>Bookmarks</TITLE>
<H1>Bookmarks</H1>
<DL><p>
    <DT><A HREF="https://example.com/webkit" ADD_DATE="13244000000000000">webkit epoch</A>
</DL><p>`

	doc, err := netscape.Unmarshal([]byte(webkitEpoch))
	if err != nil {
		t.Fatalf("Unmarshal error = %v, want nil", err)
	}

	b := findBookmark(t, &doc.Root, "https://example.com/webkit")
	if got, want := b.CreatedAt.Unix(), int64(13244000); got != want {
		t.Errorf("CreatedAt.Unix() = %d, want %d; the library's date rescaling changed, "+
			"re-check the range-check requirement in docs/ARCHITECTURE.md#parsing", got, want)
	}

	if b.CreatedAt.Year() != 1970 {
		t.Errorf("CreatedAt.Year() = %d, want 1970", b.CreatedAt.Year())
	}
}

// TestUnmarshalDoesNotTrimTitles justifies the strings.TrimSpace on every imported title. An
// unclosed <A> - which old exports do contain - absorbs the following whitespace into the
// title text.
func TestUnmarshalDoesNotTrimTitles(t *testing.T) {
	t.Parallel()

	const unclosed = `<!DOCTYPE NETSCAPE-Bookmark-file-1>
<TITLE>Bookmarks</TITLE>
<H1>Bookmarks</H1>
<DL><p>
    <DT><A HREF="https://example.com/unclosed" ADD_DATE="1374257900">unclosed
    <DT><A HREF="https://example.com/second" ADD_DATE="1374257901">second</A>
</DL><p>`

	doc, err := netscape.Unmarshal([]byte(unclosed))
	if err != nil {
		t.Fatalf("Unmarshal error = %v, want nil", err)
	}

	b := findBookmark(t, &doc.Root, "https://example.com/unclosed")
	if b.Title == strings.TrimSpace(b.Title) {
		t.Errorf("Title = %q is already trimmed; the library now trims titles, so bookscape's "+
			"own TrimSpace is redundant rather than required", b.Title)
	}
}

// TestUnmarshalRejectsMissingDoctype records the one malformed input the library does refuse,
// so that import can map it to a user-facing hint rather than a bare error string.
func TestUnmarshalRejectsMissingDoctype(t *testing.T) {
	t.Parallel()

	const noDoctype = `<TITLE>Bookmarks</TITLE>
<H1>Bookmarks</H1>
<DL><p>
    <DT><A HREF="https://example.com/" ADD_DATE="1">x</A>
</DL><p>`

	_, err := netscape.Unmarshal([]byte(noDoctype))
	if err == nil {
		t.Fatal("Unmarshal error = nil, want an error for a document with no DOCTYPE")
	}

	if !strings.Contains(err.Error(), netscape.ErrDoctypeMissing.Error()) {
		t.Errorf("Unmarshal error = %v, want it to mention %v", err, netscape.ErrDoctypeMissing)
	}
}

// TestRoundTripPreservesContent covers the things that must survive untouched: ICON payloads
// (which the exporter strips deliberately, not accidentally), <DD> descriptions, HTML-escaped
// title text, empty titles, empty folders, and literal UTF-8.
func TestRoundTripPreservesContent(t *testing.T) {
	t.Parallel()

	doc := parseFixture(t)

	t.Run("ICON stays in Attributes", func(t *testing.T) {
		t.Parallel()

		b := findBookmark(t, &doc.Root, "https://go.dev/doc/")
		if got := b.Attributes["ICON"]; !strings.HasPrefix(got, "data:image/png;base64,") {
			t.Errorf("Attributes[ICON] = %q, want a data URI", got)
		}
	})

	t.Run("description from DD", func(t *testing.T) {
		t.Parallel()

		b := findBookmark(t, &doc.Root, "https://stackoverflow.com/questions/1")
		if !strings.HasPrefix(b.Description, "A description Chrome itself never writes") {
			t.Errorf("Description = %q, want the <DD> text", b.Description)
		}
	})

	t.Run("title is HTML-unescaped", func(t *testing.T) {
		t.Parallel()

		b := findBookmark(t, &doc.Root, "https://stackoverflow.com/questions/1")
		if want := "Why is this & that <broken>?"; b.Title != want {
			t.Errorf("Title = %q, want %q", b.Title, want)
		}
	})

	t.Run("empty title survives", func(t *testing.T) {
		t.Parallel()

		b := findBookmark(t, &doc.Root, "https://example.com/untitled")
		if b.Title != "" {
			t.Errorf("Title = %q, want the empty string", b.Title)
		}
	})

	t.Run("empty folder survives", func(t *testing.T) {
		t.Parallel()

		var names []string
		for _, f := range doc.Root.Subfolders {
			names = append(names, f.Name)
		}

		if !slices.Contains(names, "Empty folder") {
			t.Errorf("root subfolders = %v, want one named %q", names, "Empty folder")
		}
	})

	t.Run("tags decode from the TAGS attribute", func(t *testing.T) {
		t.Parallel()

		b := findBookmark(t, &doc.Root, "https://github.com/golang/go")
		if diff := cmp.Diff([]string{"go", "repo"}, b.Tags); diff != "" {
			t.Errorf("Tags mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("literal UTF-8 survives encoding", func(t *testing.T) {
		t.Parallel()

		const utf8Doc = `<!DOCTYPE NETSCAPE-Bookmark-file-1>
<TITLE>Bookmarks</TITLE>
<H1>Bookmarks</H1>
<DL><p>
    <DT><A HREF="https://x.test/" ADD_DATE="1">Grüße aus München 🔖</A>
</DL><p>`

		parsed, err := netscape.Unmarshal([]byte(utf8Doc))
		if err != nil {
			t.Fatalf("Unmarshal error = %v, want nil", err)
		}

		out, err := netscape.Marshal(parsed)
		if err != nil {
			t.Fatalf("Marshal error = %v, want nil", err)
		}

		if !strings.Contains(string(out), "Grüße aus München 🔖") {
			t.Error("output does not contain the original UTF-8 title")
		}

		// Chrome's own export carries a <META> charset declaration; this encoder does not.
		// Confirm Linkwarden copes during the docs/ARCHITECTURE.md#export-and-linkwarden trial import.
		if strings.Contains(string(out), "META") {
			t.Error("encoder now emits a META tag, so the charset note in docs/ARCHITECTURE.md#parsing is stale")
		}
	})
}
