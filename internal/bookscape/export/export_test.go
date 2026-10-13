package export_test

import (
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/adnoctem/pertry/internal/bookscape/config"
	"github.com/adnoctem/pertry/internal/bookscape/export"
	"github.com/adnoctem/pertry/internal/bookscape/netscape"
	"github.com/adnoctem/pertry/internal/bookscape/store"
)

func ptr(v int64) *int64 { return &v }

func TestBuildGroupsByCollectionInConfiguredOrder(t *testing.T) {
	t.Parallel()

	records := []store.ExportRecord{
		{ID: 1, URL: "https://a.test/", Collection: "Reference"},
		{ID: 2, URL: "https://b.test/", Collection: "Development"},
		{ID: 3, URL: "https://c.test/", Collection: "Reference"},
	}

	// The configured order, not the order the records happen to arrive in: the shape of the
	// exported file should match the taxonomy the user wrote.
	collections := []string{"Development", "Infrastructure", "Reference"}

	doc := export.BuildNetscape(records, config.Default().Export, collections)

	var names []string
	for _, f := range doc.Folders {
		names = append(names, f.Name)
	}

	// Infrastructure has no bookmarks, so it is omitted rather than creating an empty
	// collection in Linkwarden on import.
	if diff := cmp.Diff([]string{"Development", "Reference"}, names); diff != "" {
		t.Errorf("folder order mismatch (-want +got):\n%s", diff)
	}

	if got := len(doc.Folders[1].Bookmarks); got != 2 {
		t.Errorf("Reference has %d bookmarks, want 2", got)
	}
}

// TestBuildKeepsAnUnconfiguredCollection covers a collection removed from the config after it
// was assigned. Dropping those bookmarks silently would be worse than emitting them somewhere
// the user has to notice.
func TestBuildKeepsAnUnconfiguredCollection(t *testing.T) {
	t.Parallel()

	records := []store.ExportRecord{
		{ID: 1, URL: "https://a.test/", Collection: "Retired"},
		{ID: 2, URL: "https://b.test/", Collection: "Development"},
	}

	doc := export.BuildNetscape(records, config.Default().Export, []string{"Development"})

	var names []string
	for _, f := range doc.Folders {
		names = append(names, f.Name)
	}

	if diff := cmp.Diff([]string{"Development", "Retired"}, names); diff != "" {
		t.Errorf("folder order mismatch (-want +got):\n%s", diff)
	}
}

// TestBuildEmitsNoAnchorAttributes pins the invariant that replaced the old export.strip_icons
// config knob.
//
// That knob could never work: import discards anchor attributes, there is no column for them, and
// ExportRecords never populated ExportRecord.Attributes - so the strip branch was unreachable and
// the option did nothing in either position. The real invariant is stronger and is checked here
// against the emitted bytes rather than an in-memory struct: every attribute the downstream
// importer reads has a typed field, and nothing else is ever written. ICON payloads are most of a
// real export's bytes (1180 of them in a 1.18 MB sample), so this is load-bearing.
func TestBuildEmitsNoAnchorAttributes(t *testing.T) {
	t.Parallel()

	records := []store.ExportRecord{{
		ID: 1, URL: "https://a.test/", Collection: "Development",
		Title: "a title", Description: "a description",
		AddDate: ptr(1374257900), LastModified: ptr(1400000000),
		Tags: []string{"go", "repo"},
	}}

	encoded, err := netscape.Emit(export.BuildNetscape(records, config.Default().Export, []string{"Development"}))
	if err != nil {
		t.Fatalf("Emit() error = %v, want nil", err)
	}

	// Attribute names actually present on the emitted anchor.
	attrRe := regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_-]*)="`)

	var anchor string

	for line := range strings.SplitSeq(string(encoded), "\n") {
		if strings.Contains(line, "<A ") {
			anchor = line
		}
	}

	if anchor == "" {
		t.Fatalf("no anchor in the emitted document:\n%s", encoded)
	}

	var got []string
	for _, m := range attrRe.FindAllStringSubmatch(anchor, -1) {
		got = append(got, strings.ToUpper(m[1]))
	}

	slices.Sort(got)

	// Exactly the five channels the Netscape format models and Linkwarden reads. Anything
	// else appearing here means an attribute map found its way back into the emitter.
	want := []string{"ADD_DATE", "HREF", "LAST_MODIFIED", "PRIVATE", "TAGS"}

	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("emitted anchor attributes mismatch (-want +got):\n%s\nanchor: %s", diff, anchor)
	}

	// ICON is the specific payload the removed knob claimed to strip. Assert on the whole
	// document, not just the anchor, so a stray one anywhere fails.
	for _, banned := range []string{"ICON=", "ICON_URI="} {
		if strings.Contains(string(encoded), banned) {
			t.Errorf("emitted document contains %s", banned)
		}
	}
}

func TestBuildTruncatesDescriptions(t *testing.T) {
	t.Parallel()

	cfg := config.Default().Export
	cfg.MaxDescriptionLength = 20

	records := []store.ExportRecord{{
		ID: 1, URL: "https://a.test/", Collection: "Development",
		Description: "This description is comfortably longer than twenty bytes.",
	}}

	got := export.BuildNetscape(records, cfg, []string{"Development"}).Folders[0].Bookmarks[0].Description

	if len(got) > 20 {
		t.Errorf("Description = %q (%d bytes), want at most 20", got, len(got))
	}
}

// TestBuildTruncatesOnARuneBoundary keeps the output valid UTF-8, which matters because the
// encoder writes literal UTF-8 rather than entities.
func TestBuildTruncatesOnARuneBoundary(t *testing.T) {
	t.Parallel()

	cfg := config.Default().Export
	// Cuts mid-rune: "ü" is two bytes starting at offset 5.
	cfg.MaxDescriptionLength = 6

	records := []store.ExportRecord{{
		ID: 1, URL: "https://a.test/", Collection: "Development",
		Description: "Grüße aus München",
	}}

	got := export.BuildNetscape(records, cfg, []string{"Development"}).Folders[0].Bookmarks[0].Description

	for i, r := range got {
		if r == '�' {
			t.Errorf("Description = %q has an invalid rune at byte %d", got, i)
		}
	}
}

// TestBuildPreservesDates is the property the whole project turns on: without it the archive
// lands in Linkwarden dated today.
func TestBuildPreservesDates(t *testing.T) {
	t.Parallel()

	records := []store.ExportRecord{
		{ID: 1, URL: "https://dated.test/", Collection: "Development", AddDate: ptr(1374257900), LastModified: ptr(1400000000)},
		{ID: 2, URL: "https://undated.test/", Collection: "Development"},
	}

	marks := export.BuildNetscape(records, config.Default().Export, []string{"Development"}).Folders[0].Bookmarks

	if marks[0].AddDate == nil || *marks[0].AddDate != 1374257900 {
		t.Errorf("AddDate = %v, want 1374257900", marks[0].AddDate)
	}

	// Still nil, not zero: a nil date makes the encoder omit the attribute rather than
	// write a 1970 one.
	if marks[1].AddDate != nil {
		t.Errorf("AddDate = %d, want nil for an entry that never had one", *marks[1].AddDate)
	}
}

// TestEmitRoundTripsThroughTheLibrary is the end-to-end check on the projection: what the
// exporter builds has to parse back into the same values, through the same library the import
// side uses.
func TestEmitRoundTripsThroughTheLibrary(t *testing.T) {
	t.Parallel()

	records := []store.ExportRecord{{
		ID: 1, URL: "https://a.test/page?x=1", Collection: "Development",
		Title:       "A title with & and <angles>",
		Description: "Grüße aus München 🔖",
		AddDate:     ptr(1374257900),
		Tags:        []string{"go", "repo"},
	}}

	doc := export.BuildNetscape(records, config.Default().Export, []string{"Development"})

	encoded, err := netscape.Emit(doc)
	if err != nil {
		t.Fatalf("Emit() error = %v, want nil", err)
	}

	entries, report, err := netscape.Parse(encoded)
	if err != nil {
		t.Fatalf("Parse() of our own output error = %v, want nil", err)
	}

	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}

	got := entries[0]

	if got.URL != records[0].URL {
		t.Errorf("URL = %q, want %q", got.URL, records[0].URL)
	}

	if got.Title != records[0].Title {
		t.Errorf("Title = %q, want %q", got.Title, records[0].Title)
	}

	if got.Description != records[0].Description {
		t.Errorf("Description = %q, want %q", got.Description, records[0].Description)
	}

	if got.AddDate == nil || *got.AddDate != 1374257900 {
		t.Errorf("AddDate = %v, want 1374257900", got.AddDate)
	}

	if diff := cmp.Diff([]string{"go", "repo"}, got.Tags); diff != "" {
		t.Errorf("Tags mismatch (-want +got):\n%s", diff)
	}

	// The collection became a folder.
	if got.Path != "Development" {
		t.Errorf("Path = %q, want %q", got.Path, "Development")
	}

	// And the file the exporter wrote is one the count check accepts.
	if report.Dropped(len(entries)) != 0 {
		t.Errorf("our own output drops %d entries on re-parse", report.Dropped(len(entries)))
	}

	// Each attribute exactly once: writing a typed field through Attributes as well would
	// emit it twice.
	for _, attr := range []string{"ADD_DATE", "TAGS"} {
		if n := strings.Count(string(encoded), attr+`="`); n != 1 {
			t.Errorf("%s appears %d times, want exactly 1", attr, n)
		}
	}
}

func TestBuildWithNoRecords(t *testing.T) {
	t.Parallel()

	doc := export.BuildNetscape(nil, config.Default().Export, []string{"Development"})

	if len(doc.Folders) != 0 {
		t.Errorf("Folders = %d, want 0", len(doc.Folders))
	}

	// An empty document still has to be a valid one.
	if _, err := netscape.Emit(doc); err != nil {
		t.Errorf("Emit() of an empty document error = %v, want nil", err)
	}
}
